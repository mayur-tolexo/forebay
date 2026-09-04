package filedriver_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mayur-tolexo/forebay/driver/conformance"
	"github.com/mayur-tolexo/forebay/driver/filedriver"
)

func TestFileDriverConforms(t *testing.T) {
	// The reference driver has to pass the suite the project ships, or the
	// suite is describing something nothing implements.
	root := t.TempDir()
	content := []byte("a dataset shard that was already here")
	if err := os.WriteFile(filepath.Join(root, "shard"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	d, err := filedriver.New(root)
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, conformance.Fixture{
		Driver:         d,
		Object:         "shard",
		Content:        content,
		WritablePrefix: "written",
	})
}

func TestAnObjectNameCannotEscapeTheRoot(t *testing.T) {
	// Object names arrive from a control plane, and one that resolved outside
	// the directory would read or delete something that is not a dataset.
	d, err := filedriver.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// "a/b" is absent on purpose: a key naming levels is ordinary, and it is
	// only a way out of the root that has to be refused.
	for _, name := range []string{
		"../escape", "a/../../escape", "/absolute", "a//b", "a/./b",
		"a/../b", "", ".", "..", "a/",
	} {
		if _, err := d.ReadRange(t.Context(), name, 0, 1); err == nil {
			t.Errorf("reading %q was allowed", name)
		}
		if err := d.DeleteObject(t.Context(), name); err == nil {
			t.Errorf("deleting %q was allowed", name)
		}
	}
}

func TestAnObjectIsNotOverwritten(t *testing.T) {
	// Objects are immutable, so writing over one would change what a cached
	// copy of it means.
	root := t.TempDir()
	d, err := filedriver.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WriteObject(t.Context(), "o", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteObject(t.Context(), "o", []byte("second")); err == nil {
		t.Error("an existing object was replaced")
	}
}

func TestAKeyCanNameLevels(t *testing.T) {
	// The namespace an NFS export presents is a tree, and it is mapped onto
	// keys that spell the levels out. A driver that took only single names
	// could serve the root and nothing under it.
	root := t.TempDir()
	d, err := filedriver.New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("shard bytes")
	if err := d.WriteObject(t.Context(), "imagenet/v17/shard-0", content); err != nil {
		t.Fatal(err)
	}

	size, err := d.SizeOf(t.Context(), "imagenet/v17/shard-0")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Errorf("size was %d, want %d", size, len(content))
	}
	got, err := d.ReadRange(t.Context(), "imagenet/v17/shard-0", 0, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("read %q, want %q", got, content)
	}

	// Each level lists the one below it, which is what a walk down the tree
	// does one readdir at a time.
	for _, c := range []struct {
		prefix string
		want   string
		dir    bool
	}{
		{"", "imagenet", true},
		{"imagenet", "v17", true},
		{"imagenet/v17", "shard-0", false},
	} {
		entries, err := d.List(t.Context(), c.prefix, "", 16)
		if err != nil {
			t.Fatalf("listing %q: %v", c.prefix, err)
		}
		if len(entries) != 1 {
			t.Fatalf("listing %q gave %d entries, want 1", c.prefix, len(entries))
		}
		if entries[0].Name != c.want || entries[0].Dir != c.dir {
			t.Errorf("listing %q gave %q dir=%v, want %q dir=%v",
				c.prefix, entries[0].Name, entries[0].Dir, c.want, c.dir)
		}
	}
}

func TestALevelIsNotAnObject(t *testing.T) {
	// A caller that tells a level from an object by asking how large it is
	// gets told every level is a file if a directory answers with its own
	// bookkeeping size.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "imagenet"), 0o750); err != nil {
		t.Fatal(err)
	}
	d, err := filedriver.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if size, err := d.SizeOf(t.Context(), "imagenet"); err == nil {
		t.Errorf("a directory reported a size of %d", size)
	}
	// Named, not merely refused: reading a directory fails on its own once
	// the bytes are asked for, and a caller that has to tell "this is not an
	// object" from "this object would not read" cannot use that.
	if _, err := d.ReadRange(t.Context(), "imagenet", 0, 1); !errors.Is(err, filedriver.ErrBadObject) {
		t.Errorf("reading a directory gave %v, want ErrBadObject", err)
	}
	if _, err := d.SizeOf(t.Context(), "imagenet"); !errors.Is(err, filedriver.ErrBadObject) {
		t.Errorf("sizing a directory gave %v, want ErrBadObject", err)
	}
}

func TestListingAnObjectIsAnEmptyLevel(t *testing.T) {
	// An object store has nothing under a key that names an object, and the
	// two stores have to answer alike or a namespace works on one only.
	root := t.TempDir()
	d, err := filedriver.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WriteObject(t.Context(), "shard", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	entries, err := d.List(t.Context(), "shard", "", 16)
	if err != nil {
		t.Fatalf("listing an object: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("listing an object gave %d entries, want 0", len(entries))
	}
}
