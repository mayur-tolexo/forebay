/* SPDX-License-Identifier: Apache-2.0 */
#include "forebay_path.h"

#include <string.h>

/* "." names the directory it is in, so it is dropped: it cannot escape, and
 * refusing it would disagree with the Go side, which normalises the path
 * before reading it. Two views that resolve one path differently are the thing
 * RFC-0021 exists to prevent.
 */
static int here(const char *start, size_t len)
{
	return len == 1 && start[0] == '.';
}

/* ".." is refused rather than resolved.
 *
 * Resolving it here would mean this file deciding what an export holds, which
 * is the export's question, and getting it wrong answers for another tenant's
 * bytes. Refusing is the answer that cannot be subtly wrong, and it matches
 * the Go side, which resolves first and then refuses what left the export.
 */
static int traversal(const char *start, size_t len)
{
	return len == 2 && start[0] == '.' && start[1] == '.';
}

enum forebay_path_result forebay_path_key(const char *path, char *out,
					  size_t out_len)
{
	size_t written = 0;
	const char *p;

	if (path == NULL || out == NULL || out_len == 0)
		return FOREBAY_PATH_EMPTY;
	out[0] = '\0';

	for (p = path; *p != '\0';) {
		const char *start;
		size_t len;

		/* Runs of slashes collapse, so two spellings of one path do not
		 * become two keys for one set of bytes.
		 */
		while (*p == '/')
			p++;
		if (*p == '\0')
			break;
		start = p;
		while (*p != '\0' && *p != '/')
			p++;
		len = (size_t)(p - start);

		if (here(start, len))
			continue;
		if (traversal(start, len))
			return FOREBAY_PATH_TRAVERSAL;
		/* One for the separator, one for the terminator. */
		if (written + (written > 0 ? 1 : 0) + len >= out_len)
			return FOREBAY_PATH_TOO_LONG;
		if (written > 0)
			out[written++] = '/';
		memcpy(out + written, start, len);
		written += len;
	}

	if (written == 0)
		return FOREBAY_PATH_EMPTY;
	out[written] = '\0';
	return FOREBAY_PATH_OK;
}

const char *forebay_path_reason(enum forebay_path_result r)
{
	switch (r) {
	case FOREBAY_PATH_OK:
		return "ok";
	case FOREBAY_PATH_EMPTY:
		return "names nothing";
	case FOREBAY_PATH_TRAVERSAL:
		return "climbs out of the export";
	case FOREBAY_PATH_TOO_LONG:
		return "longer than an object key may be";
	default:
		return "unknown";
	}
}
