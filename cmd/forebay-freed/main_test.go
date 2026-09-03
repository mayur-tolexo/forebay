package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/freed"
)

func TestCheckFlags(t *testing.T) {
	if err := checkFlags("/pool", 1<<20, 4, 3, time.Millisecond, time.Second); err != nil {
		t.Fatalf("a usable configuration was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"no directory", checkFlags("", 1<<20, 4, 3, time.Millisecond, time.Second)},
		{"no size", checkFlags("/p", 0, 4, 3, time.Millisecond, time.Second)},
		{"no extents", checkFlags("/p", 1<<20, 0, 3, time.Millisecond, time.Second)},
		{"no repeats", checkFlags("/p", 1<<20, 4, 0, time.Millisecond, time.Second)},
		{"no poll", checkFlags("/p", 1<<20, 4, 3, 0, time.Second)},
		{"polling slower than the patience", checkFlags("/p", 1<<20, 4, 3, time.Second, time.Second)},
	} {
		if c.err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestRowKeepsAFailedRun covers the case the experiment exists to find: space
// that never appeared is a measurement, not an error to exit on.
func TestRowKeepsAFailedRun(t *testing.T) {
	var out strings.Builder
	row(&out, 3, freed.Result{Unlink: 2 * time.Millisecond, Visible: 40 * time.Millisecond, Polls: 12, Saw: 1 << 20},
		errors.New("the space did not appear"))
	got := out.String()
	for _, want := range []string{"3", "2ms", "40ms", "12", "1.00MiB", "did not appear"} {
		if !strings.Contains(got, want) {
			t.Errorf("the row does not carry %q: %q", want, got)
		}
	}

	out.Reset()
	row(&out, 1, freed.Result{Polls: 1}, nil)
	if strings.Contains(out.String(), "did not appear") {
		t.Errorf("a good run carried an error: %q", out.String())
	}
}

// TestRemoveToleratesWhatIsAlreadyGone keeps a partly cleaned run from
// reporting a failure that is not one, since remove is also the cleanup path
// when laying the extents down fails halfway.
func TestRemoveToleratesWhatIsAlreadyGone(t *testing.T) {
	dir := t.TempDir()
	here := filepath.Join(dir, "here")
	if err := os.WriteFile(here, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := remove([]string{here, filepath.Join(dir, "gone")}); err != nil {
		t.Errorf("remove reported %v for a file that was already gone", err)
	}
	if _, err := os.Stat(here); !os.IsNotExist(err) {
		t.Error("remove left the file it was given")
	}
}

// TestFillCleansUpAfterItself covers the path where laying the extents down
// fails partway: what was committed has to come back, or a failed run leaves
// the filesystem holding capacity nothing accounts for.
func TestFillCleansUpAfterItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-there")
	paths, err := fill(dir, 1<<20, 4, 0)
	if err == nil {
		t.Fatal("laying extents down in a directory that is not there succeeded")
	}
	if len(paths) != 0 {
		t.Errorf("a failed fill returned %d paths, want none", len(paths))
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("a failed fill left %d files behind", len(entries))
	}
}
