/* SPDX-License-Identifier: Apache-2.0 */
/*
 * A connection to one agent, dialled when it is needed and again when it
 * breaks.
 *
 * An agent that starts after the NFS server is the ordinary race on a node, so
 * a first failure cannot be final. Dialling on every read instead would turn
 * an agent that is down into a connect storm, which is why there is a window.
 *
 * Kept apart from the module so it compiles and is tested without
 * NFS-Ganesha's headers.
 */
#ifndef FOREBAY_SOURCE_H
#define FOREBAY_SOURCE_H

#include "forebay_client.h"

#include <stdint.h>

struct forebay_source;

/* forebay_source_new holds what is needed to dial, without dialling.
 *
 * Nothing is connected here: an export configured before the agent is running
 * is ordinary, and failing to load for it would make the order they start in
 * matter.
 */
struct forebay_source *forebay_source_new(const char *socket_path,
					  int64_t max_reply, int timeout_ms,
					  int64_t retry_ms);

void forebay_source_free(struct forebay_source *s);

/* forebay_source_read and forebay_source_size ask the agent, dialling first if
 * there is no connection and the retry window has passed.
 *
 * One connection carries a request and then its reply, so these serialise:
 * two callers sharing one would read each other's answers.
 */
enum forebay_status forebay_source_read(struct forebay_source *s,
					const char *tenant, const char *object,
					int64_t offset, int64_t length,
					void *buf, int64_t *got);

enum forebay_status forebay_source_size(struct forebay_source *s,
					const char *tenant, const char *object,
					int64_t *size);

/* forebay_source_dials reports how many times this has tried to connect, which
 * is how a test sees that a window stopped a storm rather than inferring it
 * from a clock.
 */
int forebay_source_dials(const struct forebay_source *s);

/* forebay_source_connected reports whether there is a live conversation. */
int forebay_source_connected(const struct forebay_source *s);

#endif
