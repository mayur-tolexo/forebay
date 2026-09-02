/* SPDX-License-Identifier: Apache-2.0 */
/*
 * Makes an NFS server read through Forebay.
 *
 * A spike, and honest about which half is which: the namespace is the memory
 * FSAL's, and the bytes are Forebay's. A file is created to give the name a
 * size, and reading it fetches from the agent rather than from memory.
 *
 * The socket comes from FOREBAY_SOCKET. One connection per export would be
 * better; one per process is enough to answer whether a client can read
 * through this at all.
 */

#include "mem_forebay.h"
#include "config.h"
#include "fsal.h"
#include "../../fsal/forebay_client.h"

#include <pthread.h>
#include <time.h>
#include <stdlib.h>

/* retry_after bounds how often a failed dial is repeated, so an agent that is
 * not up yet is picked up when it is, without dialling on every read.
 */
#define RETRY_AFTER_MS 2000

static pthread_mutex_t forebay_lock = PTHREAD_MUTEX_INITIALIZER;
static struct forebay_conn *forebay;
static int64_t next_dial;
static int said_missing;

static int64_t clock_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

/* forebay_source returns the connection, dialling again when there is none.
 *
 * An agent that starts after this process is the ordinary race on a node, so
 * a first failure cannot be final: dialling once and giving up would mean an
 * agent that arrives a second later is never used.
 */
static struct forebay_conn *forebay_source(void)
{
	const char *path;
	int64_t now;

	if (forebay != NULL && !forebay_broken(forebay))
		return forebay;
	if (forebay != NULL) {
		/* A conversation that lost its place cannot be resumed. */
		forebay_close(forebay);
		forebay = NULL;
	}
	now = clock_ms();
	if (now < next_dial)
		return NULL;
	next_dial = now + RETRY_AFTER_MS;

	path = getenv("FOREBAY_SOCKET");
	if (path == NULL)
		return NULL;
	forebay = forebay_dial(path, 8 << 20, 30000);
	if (forebay == NULL) {
		if (!said_missing) {
			LogCrit(COMPONENT_FSAL, "forebay: could not reach %s",
				path);
			said_missing = 1;
		}
		return NULL;
	}
	LogEvent(COMPONENT_FSAL, "forebay: reading through %s", path);
	said_missing = 0;
	return forebay;
}

/* mem_forebay_read fills buf from the agent.
 *
 * Three answers, not two. Falling back to what the FSAL holds is right when
 * the object is not Forebay's, and wrong when Forebay simply could not answer:
 * the FSAL fills a buffer with padding, so an agent that is down would hand a
 * client fabricated bytes as the file's contents. That is worse than an error,
 * because nothing about it looks like a failure.
 */
enum mem_forebay_result mem_forebay_read(const char *object, uint64_t offset,
					 void *buf, size_t len, size_t *got)
{
	struct forebay_conn *c;
	enum forebay_status st;
	enum mem_forebay_result result;
	int64_t n = 0;

	if (len == 0)
		return MEM_FOREBAY_ABSENT;

	PTHREAD_MUTEX_lock(&forebay_lock);
	c = forebay_source();
	if (c == NULL) {
		PTHREAD_MUTEX_unlock(&forebay_lock);
		/* No agent configured is not a failure; no agent reachable is,
		 * and the two are told apart by whether a socket was named.
		 */
		return getenv("FOREBAY_SOCKET") == NULL ? MEM_FOREBAY_ABSENT
						       : MEM_FOREBAY_FAILED;
	}
	st = forebay_read(c, "t1", object, (int64_t)offset, (int64_t)len, buf,
			  &n);
	PTHREAD_MUTEX_unlock(&forebay_lock);

	switch (st) {
	case FOREBAY_OK:
		*got = (size_t)n;
		return MEM_FOREBAY_SERVED;
	case FOREBAY_RANGE:
		/* The object is shorter than this file claims to be, which is
		 * the FSAL's business rather than an error.
		 */
		result = MEM_FOREBAY_ABSENT;
		break;
	default:
		result = MEM_FOREBAY_FAILED;
		break;
	}
	LogDebug(COMPONENT_FSAL, "forebay: %s at %llu returned status %d",
		 object, (unsigned long long)offset, st);
	return result;
}
