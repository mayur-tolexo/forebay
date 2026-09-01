package filedriver_test

import (
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
	for _, name := range []string{"../escape", "a/b", "", ".", ".."} {
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
