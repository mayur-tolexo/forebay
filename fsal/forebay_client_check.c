/* SPDX-License-Identifier: Apache-2.0 */
/*
 * Checks this client against a running agent, without Ganesha in the way.
 *
 * The protocol has two implementations and this is the second, so the thing
 * worth proving is that they agree: the same bytes, and the same three
 * answers for a bad range, a request that will never be valid, and a backend
 * that could not answer.
 */

#include "forebay_client.h"

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* fnv1a is a checksum that notices order.
 *
 * A byte sum does not: two blocks returned the wrong way round, or a range
 * served from the wrong offset, add up the same. That is the failure this
 * path is most exposed to, since a read is assembled from fixed-size blocks
 * at one end and reassembled from chunks at the other.
 */
static uint64_t fnv1a(uint64_t h, const unsigned char *p, size_t n)
{
	for (size_t i = 0; i < n; i++) {
		h ^= p[i];
		h *= 1099511628211ULL;
	}
	return h;
}

static int failures;

static void expect(const char *what, int ok)
{
	printf("%-52s %s\n", what, ok ? "ok" : "FAILED");
	if (!ok)
		failures++;
}

/* sum_file prints the checksum of a local file, so a test can compute what it
 * expects without a second implementation of this function.
 */
static int sum_file(const char *path)
{
	unsigned char chunk[1 << 16];
	uint64_t h = 1469598103934665603ULL;
	FILE *f = fopen(path, "rb");
	size_t n;

	if (f == NULL) {
		perror(path);
		return 1;
	}
	while ((n = fread(chunk, 1, sizeof(chunk), f)) > 0)
		h = fnv1a(h, chunk, n);
	fclose(f);
	printf("%016llx\n", (unsigned long long)h);
	return 0;
}

int main(int argc, char **argv)
{
	const char *sock, *object;
	int64_t size, got = 0;
	struct forebay_conn *c;
	unsigned char *buf;
	uint64_t sum = 1469598103934665603ULL;
	enum forebay_status st;

	if (argc == 3 && strcmp(argv[1], "--sum") == 0)
		return sum_file(argv[2]);
	if (argc != 4 && argc != 5) {
		fprintf(stderr, "usage: %s <socket> <object> <size> [expected-checksum]\n",
			argv[0]);
		return 2;
	}
	sock = argv[1];
	object = argv[2];
	size = strtoll(argv[3], NULL, 10);

	c = forebay_dial(sock, 8 << 20, 30000);
	if (c == NULL) {
		perror("dial");
		return 1;
	}

	buf = malloc(8 << 20);
	if (buf == NULL)
		return 1;

	/* The whole object, in chunks, summed so a misplaced byte shows. */
	for (int64_t off = 0; off < size;) {
		int64_t want = size - off > (8 << 20) ? (8 << 20) : size - off;

		st = forebay_read(c, "t1", object, off, want, buf, &got);
		if (st != FOREBAY_OK || got != want) {
			printf("read at %lld: status %d, %lld of %lld bytes\n",
			       (long long)off, st, (long long)got,
			       (long long)want);
			failures++;
			break;
		}
		sum = fnv1a(sum, buf, (size_t)got);
		off += got;
	}
	printf("%-52s %016llx\n", "checksum of the whole object",
	       (unsigned long long)sum);

	st = forebay_read(c, "t1", object, size + (1 << 20), 4096, buf, &got);
	expect("a read past the end is a range error", st == FOREBAY_RANGE);

	st = forebay_read(c, "", object, 0, 16, buf, &got);
	expect("a request with no tenant is refused", st == FOREBAY_REFUSED);

	st = forebay_read(c, "t1", "no-such-object", 0, 16, buf, &got);
	expect("an object that is not there is a failure", st == FOREBAY_FAILED);

	/* None of those three is a reason to end the conversation, and the only
	 * way to show it is to keep using it.
	 */
	expect("none of that marked the conversation over", !forebay_broken(c));
	st = forebay_read(c, "t1", object, 0, 4096, buf, &got);
	expect("and it still answers afterwards",
	       st == FOREBAY_OK && got == 4096);

	if (argc == 5) {
		char want[32];

		snprintf(want, sizeof(want), "%016llx",
			 (unsigned long long)sum);
		expect("the checksum is the one expected",
		       strcmp(want, argv[4]) == 0);
	}

	free(buf);
	forebay_close(c);
	printf("%d failed\n", failures);
	return failures == 0 ? 0 : 1;
}
