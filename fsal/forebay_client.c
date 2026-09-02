/* SPDX-License-Identifier: Apache-2.0 */

#include "forebay_client.h"

#include <errno.h>
#include <poll.h>
#include <time.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#define FOREBAY_MAGIC 0x46425259u /* "FBRY" */
#define FOREBAY_VERSION 1
#define FOREBAY_OP_READ 1
#define REQUEST_HEADER 26
#define REPLY_HEADER 14
#define MAX_NAME 1024

struct forebay_conn {
	int fd;
	int64_t max_reply;
	int timeout_ms;
	int broken;
};

static void put_u32(uint8_t *p, uint32_t v)
{
	p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v;
}

static void put_u16(uint8_t *p, uint16_t v)
{
	p[0] = v >> 8; p[1] = v;
}

static void put_u64(uint8_t *p, uint64_t v)
{
	for (int i = 0; i < 8; i++)
		p[i] = (uint8_t)(v >> (56 - 8 * i));
}

static uint32_t get_u32(const uint8_t *p)
{
	return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
	       ((uint32_t)p[2] << 8) | p[3];
}

static uint64_t get_u64(const uint8_t *p)
{
	uint64_t v = 0;
	for (int i = 0; i < 8; i++)
		v = (v << 8) | p[i];
	return v;
}

/* now_ms is a clock that does not jump, for measuring a deadline against. */
static int64_t now_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

/* wait_for blocks until the socket is ready or the deadline passes.
 *
 * The deadline is for the whole exchange, not for one poll. A timeout that
 * restarted each time a byte arrived would bound nothing: a peer sending one
 * byte just inside it holds this thread for as long as it likes, and the
 * thread belongs to an NFS server with a client of its own waiting.
 */
static int wait_for(int fd, short events, int64_t deadline)
{
	struct pollfd p = { .fd = fd, .events = events };
	int n;

	for (;;) {
		int64_t left = deadline - now_ms();

		if (left <= 0) {
			errno = ETIMEDOUT;
			return -1;
		}
		n = poll(&p, 1, (int)left);
		if (n > 0)
			return 0;
		if (n == 0) {
			errno = ETIMEDOUT;
			return -1;
		}
		if (errno != EINTR)
			return -1;
	}
}

static int write_all(struct forebay_conn *c, const void *buf, size_t n,
		     int64_t deadline)
{
	const uint8_t *p = buf;

	while (n > 0) {
		ssize_t w;

		if (wait_for(c->fd, POLLOUT, deadline) < 0)
			return -1;
		w = write(c->fd, p, n);
		if (w < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		p += w;
		n -= (size_t)w;
	}
	return 0;
}

static int read_all(struct forebay_conn *c, void *buf, size_t n,
		    int64_t deadline)
{
	uint8_t *p = buf;

	while (n > 0) {
		ssize_t r;

		if (wait_for(c->fd, POLLIN, deadline) < 0)
			return -1;
		r = read(c->fd, p, n);
		if (r == 0) {
			errno = ECONNRESET;
			return -1;
		}
		if (r < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		p += r;
		n -= (size_t)r;
	}
	return 0;
}

struct forebay_conn *forebay_dial(const char *path, int64_t max_reply,
				  int timeout_ms)
{
	struct sockaddr_un addr;
	struct forebay_conn *c;
	int fd;

	if (path == NULL || max_reply <= 0 || timeout_ms <= 0 ||
	    strlen(path) >= sizeof(addr.sun_path)) {
		errno = EINVAL;
		return NULL;
	}
	fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (fd < 0)
		return NULL;

	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
		int saved = errno;

		close(fd);
		errno = saved;
		return NULL;
	}
	c = calloc(1, sizeof(*c));
	if (c == NULL) {
		close(fd);
		errno = ENOMEM;
		return NULL;
	}
	c->fd = fd;
	c->max_reply = max_reply;
	c->timeout_ms = timeout_ms;
	return c;
}

void forebay_close(struct forebay_conn *c)
{
	if (c == NULL)
		return;
	if (c->fd >= 0)
		close(c->fd);
	free(c);
}

int forebay_broken(const struct forebay_conn *c)
{
	return c == NULL || c->broken;
}

/* fail marks the conversation over. A request half sent or a reply half read
 * leaves the stream in the same place, which is nowhere.
 */
static enum forebay_status fail(struct forebay_conn *c)
{
	c->broken = 1;
	return FOREBAY_FAILED;
}

enum forebay_status forebay_read(struct forebay_conn *c, const char *tenant,
				 const char *object, int64_t offset,
				 int64_t length, void *buf, int64_t *got)
{
	uint8_t head[REQUEST_HEADER];
	uint8_t reply[REPLY_HEADER];
	size_t tlen, olen;
	uint64_t declared;
	uint8_t *frame;
	int64_t deadline;

	if (c == NULL || c->broken)
		return FOREBAY_FAILED;
	tlen = strlen(tenant);
	olen = strlen(object);
	if (tlen > MAX_NAME || olen > MAX_NAME || length <= 0 ||
	    length > c->max_reply)
		return FOREBAY_REFUSED;

	deadline = now_ms() + c->timeout_ms;

	put_u32(head, FOREBAY_MAGIC);
	head[4] = FOREBAY_VERSION;
	head[5] = FOREBAY_OP_READ;
	put_u16(head + 6, (uint16_t)tlen);
	put_u16(head + 8, (uint16_t)olen);
	put_u64(head + 10, (uint64_t)offset);
	put_u64(head + 18, (uint64_t)length);

	/* One write, so a request cannot be left half sent by a short one. */
	frame = malloc(sizeof(head) + tlen + olen);
	if (frame == NULL)
		return FOREBAY_FAILED;
	memcpy(frame, head, sizeof(head));
	memcpy(frame + sizeof(head), tenant, tlen);
	memcpy(frame + sizeof(head) + tlen, object, olen);
	if (write_all(c, frame, sizeof(head) + tlen + olen, deadline) < 0) {
		free(frame);
		return fail(c);
	}
	free(frame);

	if (read_all(c, reply, sizeof(reply), deadline) < 0)
		return fail(c);
	if (get_u32(reply) != FOREBAY_MAGIC || reply[4] != FOREBAY_VERSION)
		return fail(c);

	declared = get_u64(reply + 6);
	/* The length is a number the far side chose, so it is checked before
	 * anything is read into a buffer sized by the caller.
	 */
	if (declared > (uint64_t)length)
		return fail(c);
	if (declared > 0 && read_all(c, buf, (size_t)declared, deadline) < 0)
		return fail(c);

	*got = (int64_t)declared;
	switch (reply[5]) {
	case FOREBAY_OK:
		return FOREBAY_OK;
	case FOREBAY_RANGE:
		return FOREBAY_RANGE;
	case FOREBAY_REFUSED:
		return FOREBAY_REFUSED;
	default:
		return FOREBAY_FAILED;
	}
}
