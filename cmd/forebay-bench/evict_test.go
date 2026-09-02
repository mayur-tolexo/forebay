package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEvictDropsWhatWasRead checks the eviction rather than trusting it. A
// benchmark that assumes it went cold reports a cached read as a device read,
// and nothing in the number would say so.
func TestEvictDropsWhatWasRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fadvise and mincore are Linux only")
	}
	path := filepath.Join(t.TempDir(), "extent")
	// Larger than a page by enough that a partial eviction is visible.
	data := make([]byte, 8<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Read it back so the pages are certainly resident.
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}

	before, after, err := evict(path)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if before == 0 {
		t.Fatal("nothing was resident before the eviction, so it proves nothing")
	}
	if after*20 > before {
		t.Errorf("%d of %d pages still resident, want nearly none", after, before)
	}
}

func TestEvictRefusesWhatItCannotMeasure(t *testing.T) {
	if _, _, err := evict(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("evicting a file that is not there succeeded")
	}
	if runtime.GOOS != "linux" {
		return
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := evict(empty); err == nil {
		t.Error("evicting an empty file succeeded, which measures nothing")
	}
}
