package dataset

import (
	"errors"
	"testing"
)

// TestTheTwoViewsNameTheSameThing is the feature. A shard read over the file
// view and fetched over the object view must resolve to one reference, or the
// two views are two datasets that agree by convention.
func TestTheTwoViewsNameTheSameThing(t *testing.T) {
	want := Ref{Dataset: "imagenet", Version: "v17", Object: "shard-00104"}

	key := want.ObjectKey()
	filePath := want.FilePath("/datasets")

	fromKey, err := ParseObjectKey(key)
	if err != nil {
		t.Fatal(err)
	}
	fromPath, err := ParseFilePath("/datasets", filePath)
	if err != nil {
		t.Fatal(err)
	}
	if fromKey != want || fromPath != want {
		t.Errorf("key gave %+v and path gave %+v, want %+v", fromKey, fromPath, want)
	}
	if fromKey != fromPath {
		t.Errorf("the two views resolved to different references: %+v and %+v", fromKey, fromPath)
	}
	// And the same backend address, which is what makes them one copy rather
	// than two that happen to match: the tier keys on this.
	res := fixed{"imagenet/v17": {Dataset: "imagenet", Version: "v17"}}
	byKey, err := Backend(fromKey, res)
	if err != nil {
		t.Fatal(err)
	}
	byPath, err := Backend(fromPath, res)
	if err != nil {
		t.Fatal(err)
	}
	if byKey != byPath {
		t.Errorf("the two views addressed different bytes: %q and %q", byKey, byPath)
	}
}

// TestAnObjectMayContainSlashes matters because a shard laid out in
// directories is still one object, and splitting on every slash would read its
// first directory as the version.
func TestAnObjectMayContainSlashes(t *testing.T) {
	got, err := ParseObjectKey("imagenet/v17/train/part-0/shard-00104")
	if err != nil {
		t.Fatal(err)
	}
	want := Ref{Dataset: "imagenet", Version: "v17", Object: "train/part-0/shard-00104"}
	if got != want {
		t.Errorf("parsed %+v, want %+v", got, want)
	}
	if round := got.ObjectKey(); round != "imagenet/v17/train/part-0/shard-00104" {
		t.Errorf("rendering it back gave %q", round)
	}
}

// TestAKeyMissingAPartIsRefused keeps a reference without a version from
// addressing a dataset's bytes without saying which of them.
func TestAKeyMissingAPartIsRefused(t *testing.T) {
	for _, key := range []string{"", "imagenet", "imagenet/v17", "imagenet//shard", "/imagenet/v17"} {
		if _, err := ParseObjectKey(key); err == nil {
			t.Errorf("%q was accepted", key)
		}
	}
}

// TestALeadingSlashIsNotADatasetCalledNothing covers the key an S3 client is
// as likely to send with a slash as without.
func TestALeadingSlashIsNotADatasetCalledNothing(t *testing.T) {
	got, err := ParseObjectKey("/imagenet/v17/shard-00104")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dataset != "imagenet" {
		t.Errorf("parsed dataset %q from a key with a leading slash", got.Dataset)
	}
}

// TestAPathOutsideTheExportIsRefused matters because trimming instead would
// let a caller address something the export does not hold.
func TestAPathOutsideTheExportIsRefused(t *testing.T) {
	if _, err := ParseFilePath("/datasets", "/elsewhere/imagenet/v17/shard"); !errors.Is(err, ErrNotUnderRoot) {
		t.Errorf("a path outside the export gave %v", err)
	}
	// A traversal that resolves back outside is the same refusal, since the
	// path is cleaned before it is checked.
	if _, err := ParseFilePath("/datasets", "/datasets/../elsewhere/a/b/c"); !errors.Is(err, ErrNotUnderRoot) {
		t.Errorf("a path escaping the export by traversal gave %v", err)
	}
	// A root that is the filesystem root holds everything, so nothing is
	// outside it.
	if _, err := ParseFilePath("/", "/imagenet/v17/shard"); err != nil {
		t.Errorf("a path under the root export was refused: %v", err)
	}
}

// TestAPathIsCleanedBeforeItIsRead keeps two spellings of one path from
// producing two references, and so two cache keys for one set of bytes.
func TestAPathIsCleanedBeforeItIsRead(t *testing.T) {
	want := Ref{Dataset: "imagenet", Version: "v17", Object: "shard"}
	for _, p := range []string{
		"/datasets/imagenet/v17/shard",
		"/datasets//imagenet/v17/shard",
		"/datasets/./imagenet/v17/shard",
		"/datasets/other/../imagenet/v17/shard",
	} {
		got, err := ParseFilePath("/datasets", p)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		if got != want {
			t.Errorf("%q parsed to %+v, want %+v", p, got, want)
		}
	}
}
