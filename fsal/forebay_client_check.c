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

	/* The size the far side reports is what an NFS server puts in getattrs,
	 * so it has to be the object's and not a number this side worked out.
	 */
	{
		int64_t reported = -1;

		st = forebay_size(c, "t1", object, &reported);
		expect("the object's size comes back", st == FOREBAY_OK);
		expect("and it is the size the caller was told",
		       reported == size);

		st = forebay_size(c, "", object, &reported);
		expect("a size with no tenant is refused", st == FOREBAY_REFUSED);

		st = forebay_size(c, "t1", "no-such-object", &reported);
		expect("an object that is not there has no size",
		       st == FOREBAY_FAILED);
	}

	/* A listing is what an NFS server answers readdir from, and this side
	 * has to read the records the Go side wrote.
	 */
	{
		struct forebay_entry e;
		int64_t at = 0, n = 0;
		int names = 0, rc;

		st = forebay_list(c, "t1", "", "", 100, buf, 1 << 20, &n);
		expect("a listing comes back", st == FOREBAY_OK);
		while ((rc = forebay_entry_next(buf, n, &at, &e)) == 1) {
			names++;
			if (e.name[0] == 0) {
				expect("a listing carried an empty name", 0);
				break;
			}
		}
		expect("and every record read whole", rc == 0);
		expect("with at least the object in it", names >= 1);

		st = forebay_list(c, "", "", "", 100, buf, 1 << 20, &n);
		expect("a listing with no tenant is refused", st == FOREBAY_REFUSED);

		st = forebay_list(c, "t1", "", "", 0, buf, 1 << 20, &n);
		expect("a listing of no names is refused", st == FOREBAY_REFUSED);
	}

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
