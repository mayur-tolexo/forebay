/* SPDX-License-Identifier: Apache-2.0 */
#include "forebay_source.h"

#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

struct forebay_source {
	pthread_mutex_t lock;
	struct forebay_conn *conn;
	char *path;
	int64_t max_reply;
	int timeout_ms;
	int64_t retry_ms;
	int64_t next_dial;
	int dials;
};

static int64_t clock_ms(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

struct forebay_source *forebay_source_new(const char *socket_path,
					  int64_t max_reply, int timeout_ms,
					  int64_t retry_ms)
{
	struct forebay_source *s;

	if (socket_path == NULL || socket_path[0] == '\0')
		return NULL;
	s = calloc(1, sizeof(*s));
	if (s == NULL)
		return NULL;
	s->path = strdup(socket_path);
	if (s->path == NULL) {
		free(s);
		return NULL;
	}
	pthread_mutex_init(&s->lock, NULL);
	s->max_reply = max_reply;
	s->timeout_ms = timeout_ms;
	s->retry_ms = retry_ms;
	return s;
}

void forebay_source_free(struct forebay_source *s)
{
	if (s == NULL)
		return;
	if (s->conn != NULL)
		forebay_close(s->conn);
	pthread_mutex_destroy(&s->lock);
	free(s->path);
	free(s);
}

/* connect_locked returns a live conversation, dialling if there is none and
 * the window has passed. The caller holds the lock.
 */
static struct forebay_conn *connect_locked(struct forebay_source *s)
{
	int64_t now;

	if (s->conn != NULL && !forebay_broken(s->conn))
		return s->conn;
	if (s->conn != NULL) {
		/* A conversation that lost its place in the stream cannot be
		 * resynchronised, so it is replaced rather than reused.
		 */
		forebay_close(s->conn);
		s->conn = NULL;
	}
	now = clock_ms();
	if (now < s->next_dial)
		return NULL;
	s->next_dial = now + s->retry_ms;
	s->dials++;
	s->conn = forebay_dial(s->path, s->max_reply, s->timeout_ms);
	return s->conn;
}

enum forebay_status forebay_source_read(struct forebay_source *s,
					const char *tenant, const char *object,
					int64_t offset, int64_t length,
					void *buf, int64_t *got)
{
	struct forebay_conn *c;
	enum forebay_status st;

	if (s == NULL)
		return FOREBAY_FAILED;
	pthread_mutex_lock(&s->lock);
	c = connect_locked(s);
	if (c == NULL) {
		pthread_mutex_unlock(&s->lock);
		return FOREBAY_FAILED;
	}
	st = forebay_read(c, tenant, object, offset, length, buf, got);
	pthread_mutex_unlock(&s->lock);
	return st;
}

enum forebay_status forebay_source_size(struct forebay_source *s,
					const char *tenant, const char *object,
					int64_t *size)
{
	struct forebay_conn *c;
	enum forebay_status st;

	if (s == NULL)
		return FOREBAY_FAILED;
	pthread_mutex_lock(&s->lock);
	c = connect_locked(s);
	if (c == NULL) {
		pthread_mutex_unlock(&s->lock);
		return FOREBAY_FAILED;
	}
	st = forebay_size(c, tenant, object, size);
	pthread_mutex_unlock(&s->lock);
	return st;
}

enum forebay_status forebay_source_list(struct forebay_source *s,
					const char *tenant, const char *prefix,
					const char *after, int limit,
					void *buf, int64_t cap, int64_t *got)
{
	struct forebay_conn *c;
	enum forebay_status st;

	if (s == NULL)
		return FOREBAY_FAILED;
	pthread_mutex_lock(&s->lock);
	c = connect_locked(s);
	if (c == NULL) {
		pthread_mutex_unlock(&s->lock);
		return FOREBAY_FAILED;
	}
	st = forebay_list(c, tenant, prefix, after, limit, buf, cap, got);
	pthread_mutex_unlock(&s->lock);
	return st;
}

int forebay_source_dials(const struct forebay_source *s)
{
	return s == NULL ? 0 : s->dials;
}

int forebay_source_connected(const struct forebay_source *s)
{
	return s != NULL && s->conn != NULL && !forebay_broken(s->conn);
}
