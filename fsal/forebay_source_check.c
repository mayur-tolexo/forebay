/* SPDX-License-Identifier: Apache-2.0 */
/*
 * Checks the connection holder without an agent.
 *
 * What matters here is what it does when there is nothing to talk to, since
 * that is the state a node is in while the agent is starting and the state it
 * returns to whenever the agent restarts.
 */
#include "forebay_source.h"

#include <stdio.h>
#include <string.h>

static int failures;

static void expect(const char *what, int ok)
{
	printf("%-52s %s\n", what, ok ? "ok" : "FAILED");
	if (!ok)
		failures++;
}

int main(void)
{
	char buf[64];
	int64_t got = 0, size = 0;
	struct forebay_source *s;

	/* A source with nowhere to dial is a configuration mistake, and is
	 * refused rather than becoming a source that always fails.
	 */
	expect("no socket is not a source", forebay_source_new(NULL, 1 << 20, 1000, 1000) == NULL);
	expect("nor is an empty one", forebay_source_new("", 1 << 20, 1000, 1000) == NULL);

	/* A long window, so a second call inside it must not dial again. */
	s = forebay_source_new("/nonexistent/forebay.sock", 1 << 20, 200, 60000);
	expect("a source is made without connecting", s != NULL);
	expect("and it has not dialled yet", forebay_source_dials(s) == 0);
	expect("nor is it connected", !forebay_source_connected(s));

	expect("a read with no agent fails",
	       forebay_source_read(s, "t1", "o", 0, sizeof(buf), buf, &got) == FOREBAY_FAILED);
	expect("which took one dial", forebay_source_dials(s) == 1);

	/* The window is what keeps an agent that is down from becoming a
	 * connect storm: every read would otherwise be a connect syscall.
	 */
	for (int i = 0; i < 20; i++) {
		forebay_source_read(s, "t1", "o", 0, sizeof(buf), buf, &got);
		forebay_source_size(s, "t1", "o", &size);
	}
	expect("and forty more calls inside the window dial no further",
	       forebay_source_dials(s) == 1);

	forebay_source_free(s);
	expect("freeing a source that never connected is safe", 1);
	forebay_source_free(NULL);

	/* A window of zero is what a caller asks for when it wants every
	 * attempt to try, and it must actually try.
	 */
	s = forebay_source_new("/nonexistent/forebay.sock", 1 << 20, 200, 0);
	forebay_source_read(s, "t1", "o", 0, sizeof(buf), buf, &got);
	forebay_source_read(s, "t1", "o", 0, sizeof(buf), buf, &got);
	expect("no window means every call dials", forebay_source_dials(s) == 2);
	forebay_source_free(s);

	printf("%d failed\n", failures);
	return failures == 0 ? 0 : 1;
}
