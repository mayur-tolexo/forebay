/* SPDX-License-Identifier: Apache-2.0 */
/*
 * The read side of Forebay's data server, for an NFS server to call.
 *
 * The protocol is the one internal/dataserver speaks: one request, one reply,
 * fixed header, no negotiation. This is its second implementation, which is
 * why there is so little of it.
 */
#ifndef FOREBAY_CLIENT_H
#define FOREBAY_CLIENT_H

#include <stdint.h>
#include <sys/types.h>

/* What the far side made of a request. A bad range, a request that will never
 * be valid and a backend that could not answer need different NFS statuses,
 * so they are three answers rather than one failure.
 */
enum forebay_status {
	FOREBAY_OK = 0,
	FOREBAY_RANGE = 1,
	FOREBAY_REFUSED = 2,
	FOREBAY_FAILED = 3,
};

/* A connection to one agent. Not safe for concurrent use: one connection
 * carries a request and then its reply, so two callers sharing one would read
 * each other's.
 */
struct forebay_conn;

/* forebay_dial connects to an agent's socket. Returns NULL and sets errno. */
struct forebay_conn *forebay_dial(const char *path, int64_t max_reply,
				  int timeout_ms);

/* forebay_close ends the conversation. */
void forebay_close(struct forebay_conn *c);

/* forebay_read asks for length bytes of object from offset, writing them into
 * buf. Returns the status; on FOREBAY_OK, *got holds how many bytes arrived.
 *
 * A conversation that loses its place in the stream cannot be resynchronised,
 * so it is marked done and every later call fails without touching the
 * socket. The caller reconnects.
 */
enum forebay_status forebay_read(struct forebay_conn *c, const char *tenant,
				 const char *object, int64_t offset,
				 int64_t length, void *buf, int64_t *got);

/* forebay_broken reports whether the conversation is over. */
int forebay_broken(const struct forebay_conn *c);

#endif
