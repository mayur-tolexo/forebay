package csi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// NFS mounts a dataset from the access layer.
//
// It runs mount(8) rather than calling mount(2). An NFSv4 mount needs the
// helper: the kernel is handed an address and a set of options that the
// helper works out by talking to the server, and reimplementing that here
// would be a second NFS client in a project that has one.
type NFS struct {
	// Exec runs a command. It is here so the mount path can be exercised
	// without a kernel, and is the real one unless a test replaces it.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Mounted reports whether a path already carries a mount.
	Mounted func(target string) (bool, error)
}

// NewNFS builds a mounter that uses the node's own tools.
func NewNFS() *NFS {
	return &NFS{Exec: run, Mounted: mounted}
}

// Mount attaches source at target, read-only.
//
// Read-only is not an option a caller passes: a published version is
// immutable, so it is applied here and cannot be turned off by a volume that
// asked nicely.
func (n *NFS) Mount(ctx context.Context, source, target string, flags []string) error {
	already, err := n.Mounted(target)
	if err != nil {
		return err
	}
	if already {
		// The orchestrator repeats a publish it did not hear the answer to.
		// Mounting again would stack a second mount over the first, and the
		// unmount that follows would uncover it rather than remove it.
		return nil
	}
	// The kubelet creates the parent, not the target itself.
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("making the mount point: %w", err)
	}

	opts := append([]string{"ro"}, flags...)
	out, err := n.Exec(ctx, "mount", "-t", "nfs4", "-o", strings.Join(opts, ","), source, target)
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Unmount detaches target, and says nothing about a target that is already
// not mounted.
func (n *NFS) Unmount(ctx context.Context, target string) error {
	already, err := n.Mounted(target)
	if err != nil {
		return err
	}
	if !already {
		// Either the unpublish is a repeat or the mount never happened. Both
		// are the state the caller is asking for, and reporting an error
		// would leave the orchestrator retrying an unmount forever.
		removeIfEmpty(target)
		return nil
	}
	if out, err := n.Exec(ctx, "umount", target); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	removeIfEmpty(target)
	return nil
}

// removeIfEmpty takes the mount point away once nothing is on it.
//
// The kubelet expects the target gone after an unpublish. A failure is not
// reported and not returned: os.Remove refuses a directory with anything in
// it, which is the check, and the mount being down is what was asked for. A
// directory left behind is tidied by the kubelet's own cleanup.
func removeIfEmpty(target string) { os.Remove(target) }

// run executes a command and returns what it printed, which is where mount
// puts the reason it refused.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return out, err
}

// mounted reads the node's mount table.
//
// /proc/self/mountinfo rather than running mount(8) again: it is a file the
// kernel writes, so it cannot be out of date, and this is asked on every
// publish and unpublish.
func mounted(target string) (bool, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		// Refused rather than answered. Saying "not mounted" without having
		// read the table would mount a second time over the first on every
		// repeated publish, and the unmount would then uncover one rather
		// than remove it.
		return false, fmt.Errorf("reading the mount table: %w", err)
	}
	return inMountinfo(b, target), nil
}

// inMountinfo reports whether target is a mount point in this table.
//
// The mount point is the fifth field, and it is the one field guaranteed to be
// there before the optional ones start, so it is read by position from the
// front rather than by searching the line: a path can appear in a line as a
// device or a source without being a mount point.
func inMountinfo(table []byte, target string) bool {
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescape(fields[4]) == target {
			return true
		}
	}
	return false
}

// unescape reverses the octal escaping the kernel uses for a path with a
// space, a tab, a newline or a backslash in it.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for _, c := range []byte(s[i+1 : i+4]) {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
