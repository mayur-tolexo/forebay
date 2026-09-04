/* SPDX-License-Identifier: Apache-2.0 */
/*
 * The namespace an export presents, and the object key it resolves to.
 *
 * RFC-0021 fixes the mapping: a reference is rendered as a path for the file
 * view and as a key for the object view, and both resolve to the same bytes.
 * This is the file view's half in C, and `internal/dataset` is the same half
 * in Go. They agree by construction — join the components with a slash — and
 * the check binary proves it against cases the Go tests also carry.
 *
 * Kept apart from the FSAL module so that it compiles and is tested without
 * NFS-Ganesha's headers, which are not in this repository.
 */
#ifndef FOREBAY_PATH_H
#define FOREBAY_PATH_H

#include <stddef.h>

/* Why a path is not one this export can serve. */
enum forebay_path_result {
	FOREBAY_PATH_OK = 0,
	/* Empty, or every component was empty. */
	FOREBAY_PATH_EMPTY,
	/* A component is "." or "..". Refused rather than resolved: a path that
	 * climbs out of the export is a caller asking for something the export
	 * does not hold, and resolving it would answer for something else.
	 */
	FOREBAY_PATH_TRAVERSAL,
	/* Longer than an object key may be, or too many components deep. */
	FOREBAY_PATH_TOO_LONG,
};

/* The longest object key this will build, which bounds what a lookup can ask
 * the agent for. A key longer than this is a caller that has lost its way
 * rather than an object anybody stored.
 */
#define FOREBAY_KEY_MAX 1024

/* forebay_path_key turns a path under the export root into an object key.
 *
 * Components are joined with a single slash, so "/a//b/" and "/a/b" are one
 * key: two spellings of one path must not become two cache keys for one set
 * of bytes.
 *
 * out holds at least FOREBAY_KEY_MAX + 1 bytes.
 */
enum forebay_path_result forebay_path_key(const char *path, char *out,
					  size_t out_len);

/* forebay_path_reason names a result, for an error a person reads. */
const char *forebay_path_reason(enum forebay_path_result r);

#endif
