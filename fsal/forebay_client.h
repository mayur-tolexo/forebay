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

/* forebay_size asks how large an object is, writing it to *size.
 *
 * An NFS server has to answer getattrs before a client will read anything,
 * and it cannot invent a size: too small truncates the file and too large
 * sends the client reading past the end.
 */
enum forebay_status forebay_size(struct forebay_conn *c, const char *tenant,
				 const char *object, int64_t *size);

/* The fixed part of one entry in a listing: a name length, a directory flag
 * and a size.
 */
#define FOREBAY_ENTRY_HEADER (2 + 1 + 8)

/* The longest name a listing carries, which is what the protocol bounds a name
 * at. A caller reads into this rather than allocating per entry.
 */
#define FOREBAY_ENTRY_NAME_MAX 1024

/* One name under a prefix. A directory here is a prefix that has objects
 * beneath it, since a store has no directories of its own.
 */
struct forebay_entry {
	char name[FOREBAY_ENTRY_NAME_MAX + 1];
	int dir;
	int64_t bytes;
};

/* forebay_list asks what names are under a prefix, writing the reply into buf
 * and setting *got to how many bytes it holds.
 *
 * The reply is walked with forebay_entry_next rather than returned as an
 * array: a caller hands each name onward as it reads it, and a second copy
 * would be a second thing to free on every error path.
 */
enum forebay_status forebay_list(struct forebay_conn *c, const char *tenant,
				 const char *prefix, const char *after,
				 int limit, void *buf, int64_t cap,
				 int64_t *got);

/* forebay_entry_next reads the record at *at and advances it.
 *
 * Returns 1 for an entry, 0 at the end, and -1 for a reply that ends inside
 * one, which is a far side that does not speak this rather than an empty
 * directory.
 */
int forebay_entry_next(const void *body, int64_t len, int64_t *at,
		       struct forebay_entry *out);

/* forebay_broken reports whether the conversation is over. */
int forebay_broken(const struct forebay_conn *c);

#endif
