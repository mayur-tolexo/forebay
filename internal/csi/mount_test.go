package csi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/csi"
)

// recorder stands in for the node's own tools.
type recorder struct {
	ran     [][]string
	err     error
	mounted map[string]bool
}

func (r *recorder) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.ran = append(r.ran, append([]string{name}, args...))
	if r.err != nil {
		return []byte("mount.nfs4: access denied by server"), r.err
	}
	return nil, nil
}

func (r *recorder) is(target string) (bool, error) { return r.mounted[target], nil }

func nfs(r *recorder) *csi.NFS {
	return &csi.NFS{Exec: r.exec, Mounted: r.is}
}

func TestAMountIsAlwaysReadOnly(t *testing.T) {
	// A published version is immutable, so this is not an option a caller
	// passes. A volume that asked to be writable would otherwise get it.
	r := &recorder{mounted: map[string]bool{}}
	target := filepath.Join(t.TempDir(), "x")
	if err := nfs(r).Mount(t.Context(), "10.0.0.9:/forebay/imagenet/v17", target, []string{"vers=4.1"}); err != nil {
		t.Fatal(err)
	}
	if len(r.ran) != 1 {
		t.Fatalf("ran %v", r.ran)
	}
	line := strings.Join(r.ran[0], " ")
	if !strings.Contains(line, "-o ro,vers=4.1") {
		t.Errorf("mounted with %q", line)
	}
	if !strings.Contains(line, "-t nfs4") {
		t.Errorf("not an nfs4 mount: %q", line)
	}
}

func TestARepeatedPublishDoesNotStackAMount(t *testing.T) {
	// The orchestrator repeats a publish it did not hear the answer to. A
	// second mount over the first hides it, and the unmount that follows
	// uncovers it rather than removing it.
	target := filepath.Join(t.TempDir(), "x")
	r := &recorder{mounted: map[string]bool{target: true}}
	if err := nfs(r).Mount(t.Context(), "10.0.0.9:/forebay/imagenet/v17", target, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.ran) != 0 {
		t.Errorf("mounted again: %v", r.ran)
	}
}

func TestUnmountingWhatIsNotMountedIsNotAnError(t *testing.T) {
	// Either the unpublish is a repeat or the mount never happened, and both
	// are the state being asked for. An error would leave the orchestrator
	// retrying forever.
	r := &recorder{mounted: map[string]bool{}}
	if err := nfs(r).Unmount(t.Context(), filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatal(err)
	}
	if len(r.ran) != 0 {
		t.Errorf("ran %v", r.ran)
	}
}

func TestTheMountPointGoesAwayWithTheMount(t *testing.T) {
	// The kubelet expects the target gone after an unpublish.
	dir := t.TempDir()
	target := filepath.Join(dir, "x")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	r := &recorder{mounted: map[string]bool{target: true}}
	if err := nfs(r).Unmount(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the mount point is still there: %v", err)
	}
}

func TestWhatMountSaidSurvivesTheFailure(t *testing.T) {
	// mount(8) puts the reason it refused on its output, and an operator with
	// only "exit status 32" has nothing to act on.
	r := &recorder{mounted: map[string]bool{}, err: errors.New("exit status 32")}
	err := nfs(r).Mount(t.Context(), "10.0.0.9:/forebay/x", filepath.Join(t.TempDir(), "x"), nil)
	if err == nil {
		t.Fatal("the mount reported success")
	}
	if !strings.Contains(err.Error(), "access denied by server") {
		t.Errorf("got %q", err)
	}
}

func TestAMountTableThatCannotBeReadIsNotAnEmptyOne(t *testing.T) {
	// Answering "not mounted" without having read the table mounts a second
	// time over the first on every repeated publish.
	n := &csi.NFS{
		Exec:    func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		Mounted: func(string) (bool, error) { return false, errors.New("no mount table here") },
	}
	if err := n.Mount(t.Context(), "src", filepath.Join(t.TempDir(), "x"), nil); err == nil {
		t.Error("a mount went ahead without knowing whether one was there")
	}
	if err := n.Unmount(t.Context(), "/target"); err == nil {
		t.Error("an unmount went ahead without knowing whether one was there")
	}
}

func TestAPathIsFoundInTheMountTableByPosition(t *testing.T) {
	// The mount point is the fifth field. A path also appears as a source, so
	// searching the line rather than reading the field finds mounts that are
	// not there.
	table := "" +
		"22 21 0:20 / /proc rw,relatime shared:5 - proc proc rw\n" +
		"25 21 0:23 / /var/lib/kubelet/pods/abc/x ro,relatime shared:9 - nfs4 10.0.0.9:/forebay/imagenet/v17 ro\n" +
		"31 21 0:24 / /tmp/space\\040here rw,relatime shared:11 - tmpfs tmpfs rw\n"

	for _, c := range []struct {
		path string
		want bool
	}{
		{"/var/lib/kubelet/pods/abc/x", true},
		{"/proc", true},
		{"/tmp/space here", true},
		// A source, not a mount point.
		{"10.0.0.9:/forebay/imagenet/v17", false},
		{"/var/lib/kubelet/pods/abc", false},
		{"/nowhere", false},
	} {
		if got := csi.InMountinfo([]byte(table), c.path); got != c.want {
			t.Errorf("%q: got %v, want %v", c.path, got, c.want)
		}
	}
}
