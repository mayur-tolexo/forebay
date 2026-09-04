package dataset

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTheCMappingAgrees runs the FSAL's path mapping and checks it against
// this one.
//
// RFC-0021's whole claim is that the file view and the object view resolve to
// the same bytes, and they are written in two languages. The mapping is the
// one place they can silently disagree, and two tables somebody keeps matching
// by eye is not a check. This asks the C binary what it produced.
//
// It skips when the binary has not been built, since `make check` builds it
// and a bare `go test` should not require a C toolchain.
func TestTheCMappingAgrees(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate the source tree")
	}
	bin := filepath.Join(filepath.Dir(here), "..", "..", "fsal", "forebay-path-check")
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s is not built: %v", bin, err)
	}

	out, err := exec.Command(bin, "--pairs").Output()
	if err != nil {
		t.Fatalf("running the C mapping: %v", err)
	}

	var compared int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, key, found := strings.Cut(line, "\t")
		if !found {
			t.Fatalf("unreadable line from the C mapping: %q", line)
		}

		// The C side maps any path to a candidate key, because an FSAL walks
		// directories that are not references. This parser only accepts a
		// whole reference, so the two are compared where they answer the same
		// question: a key with all three names in it.
		if key == "-" || strings.Count(key, "/") < 2 {
			continue
		}
		compared++

		ref, err := ParseFilePath("/", path)
		if err != nil {
			t.Errorf("C mapped %q to %q and Go refused it: %v", path, key, err)
			continue
		}
		if got := ref.ObjectKey(); got != key {
			t.Errorf("%q: C says %q, Go says %q", path, key, got)
		}
	}
	if compared == 0 {
		t.Fatal("no paths were comparable, so this proved nothing")
	}

	// And where C refuses, this must too: a path one view serves and the other
	// does not is the same disagreement the other way round.
	for _, refused := range []string{"/", "", "/imagenet/../../etc/passwd", "/././."} {
		if _, err := ParseFilePath("/", refused); err == nil {
			t.Errorf("Go accepted %q, which the C mapping refuses", refused)
		} else if !errors.Is(err, ErrEmpty) && !errors.Is(err, ErrNotUnderRoot) && !errors.Is(err, ErrSeparator) {
			t.Errorf("Go refused %q for an unexpected reason: %v", refused, err)
		}
	}
}
