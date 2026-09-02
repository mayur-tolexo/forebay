/* SPDX-License-Identifier: Apache-2.0 */
#ifndef MEM_FOREBAY_H
#define MEM_FOREBAY_H

#include <stddef.h>
#include <stdint.h>

/* What a read through Forebay came to.
 *
 * ABSENT and FAILED are separate because the caller does different things
 * with them: one falls back to what the FSAL holds, the other must not, since
 * that padding would reach a client as the file's contents.
 */
enum mem_forebay_result {
	MEM_FOREBAY_SERVED,
	MEM_FOREBAY_ABSENT,
	MEM_FOREBAY_FAILED,
};

enum mem_forebay_result mem_forebay_read(const char *object, uint64_t offset,
					 void *buf, size_t len, size_t *got);

#endif
