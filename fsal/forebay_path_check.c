/* SPDX-License-Identifier: Apache-2.0 */
/*
 * Checks the path mapping against the cases the Go side carries.
 *
 * The file view and the object view have to resolve to the same bytes, and
 * they are written in different languages, so the mapping is the one place
 * they can silently disagree. These cases are the ones in
 * internal/dataset's tests, spelled the same way on purpose.
 */
#include "forebay_path.h"

#include <stdio.h>
#include <string.h>

static int failures;

static void expect_key(const char *path, const char *want)
{
	char got[FOREBAY_KEY_MAX + 1];
	enum forebay_path_result r = forebay_path_key(path, got, sizeof(got));
	int ok = r == FOREBAY_PATH_OK && strcmp(got, want) == 0;

	printf("%-46s -> %-28s %s\n", path, got, ok ? "ok" : "FAILED");
	if (!ok) {
		printf("    wanted %s (%s)\n", want, forebay_path_reason(r));
		failures++;
	}
}

static void expect_refused(const char *what, const char *path,
			   enum forebay_path_result want)
{
	char got[FOREBAY_KEY_MAX + 1];
	enum forebay_path_result r = forebay_path_key(path, got, sizeof(got));

	printf("%-46s %s\n", what, r == want ? "ok" : "FAILED");
	if (r != want) {
		printf("    got %s, wanted %s\n", forebay_path_reason(r),
		       forebay_path_reason(want));
		failures++;
	}
}

/* pairs prints what each path maps to, so the Go side can check the two
 * implementations against each other rather than against two tables somebody
 * has to keep matching by eye.
 */
static void pairs(void)
{
	static const char *paths[] = {
		"/imagenet/v17/shard-00104",
		"imagenet/v17/shard-00104",
		"/imagenet/v17/train/part-0/shard",
		"//imagenet//v17//shard",
		"/imagenet/v17/shard/",
		"/imagenet/./v17/shard",
		"/",
		"",
		"/imagenet/../../etc/passwd",
		"/././.",
		"/imagenet/v17",
	};

	for (size_t i = 0; i < sizeof(paths) / sizeof(paths[0]); i++) {
		char key[FOREBAY_KEY_MAX + 1];
		enum forebay_path_result r =
			forebay_path_key(paths[i], key, sizeof(key));

		printf("%s\t%s\n", paths[i],
		       r == FOREBAY_PATH_OK ? key : "-");
	}
}

int main(int argc, char **argv)
{
	if (argc == 2 && strcmp(argv[1], "--pairs") == 0) {
		pairs();
		return 0;
	}
	/* The three names, in the order both views put them. */
	expect_key("/imagenet/v17/shard-00104", "imagenet/v17/shard-00104");
	expect_key("imagenet/v17/shard-00104", "imagenet/v17/shard-00104");

	/* An object may contain slashes: a shard laid out in directories is
	 * still one object, which is why the key is not split at three.
	 */
	expect_key("/imagenet/v17/train/part-0/shard",
		   "imagenet/v17/train/part-0/shard");

	/* Spellings that must not become different keys. */
	expect_key("//imagenet//v17//shard", "imagenet/v17/shard");
	expect_key("/imagenet/v17/shard/", "imagenet/v17/shard");

	expect_refused("a path that names nothing", "/", FOREBAY_PATH_EMPTY);
	expect_refused("an empty path", "", FOREBAY_PATH_EMPTY);
	expect_refused("climbing out of the export", "/imagenet/../../etc/passwd",
		       FOREBAY_PATH_TRAVERSAL);
	/* "." cannot escape, so it is dropped rather than refused: the Go side
	 * normalises before it reads, and two views that resolve one path
	 * differently are the thing RFC-0021 exists to prevent.
	 */
	expect_key("/imagenet/./v17/shard", "imagenet/v17/shard");
	expect_refused("a path that is only dots", "/././.",
		       FOREBAY_PATH_EMPTY);

	{
		/* A key longer than the bound is refused rather than truncated:
		 * a truncated key names a different object, and answering for
		 * one is worse than answering for none.
		 */
		char path[FOREBAY_KEY_MAX + 64];

		memset(path, 'x', sizeof(path) - 1);
		path[0] = '/';
		path[sizeof(path) - 1] = '\0';
		expect_refused("a key longer than the bound", path,
			       FOREBAY_PATH_TOO_LONG);
	}

	printf("%d failed\n", failures);
	return failures == 0 ? 0 : 1;
}
