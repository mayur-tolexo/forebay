// Command forebay-freed answers when capacity the agent released becomes
// visible, as against when the unlink returned.
//
// RFC-0018 asks it, and the agent has since come to rest on the answer: a
// headroom configured as a duration corrects the observed write rate by what
// the agent gave back, which assumes the filesystem shows it by the next poll.
// Where it does not, the rate reads high and the floor comes out too large.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mayur-tolexo/forebay/internal/freed"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/topology"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forebay-freed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir       = flag.String("dir", "", "a directory on the filesystem under test")
		sysroot   = flag.String("sysroot", "/", "root the filesystem is measured under")
		mountinfo = flag.String("mountinfo", "/proc/self/mountinfo", "where mounts are read from")
		size      = flag.Int64("extent-bytes", 1<<30, "how large each extent is")
		extents   = flag.Int("extents", 8, "how many are released at once, as a reclaim releases several")
		every     = flag.Duration("poll", time.Millisecond, "how often free space is read while waiting")
		patience  = flag.Duration("patience", 30*time.Second, "how long to wait before calling it not visible")
		repeat    = flag.Int("repeat", 5, "how many times to measure")
	)
	flag.Parse()

	if err := checkFlags(*dir, *size, *extents, *repeat, *every, *patience); err != nil {
		return err
	}

	// Described once for the operator, then polled with a single syscall.
	if device := topology.DescribePool(*sysroot, *mountinfo, *dir).Device; device != "" {
		fmt.Printf("measuring %s on %s\n", *dir, device)
	}
	free := func() (int64, error) { return available(*dir) }

	total := *size * int64(*extents)
	fmt.Printf("releasing %s as %d extents, %d times, polling every %s\n\n",
		pool.Bytes(total), *extents, *repeat, *every)
	fmt.Printf("%-6s %14s %14s %8s %14s\n", "run", "unlink", "then visible", "polls", "appeared")

	for i := 0; i < *repeat; i++ {
		paths, err := fill(*dir, *size, *extents, i)
		if err != nil {
			return err
		}
		got, err := freed.Watch(free, func() error { return remove(paths) }, total, *every, *patience)
		row(os.Stdout, i+1, got, err)
	}
	return nil
}

// checkFlags rejects a run that would measure nothing.
func checkFlags(dir string, size int64, extents, repeat int, every, patience time.Duration) error {
	switch {
	case dir == "":
		return fmt.Errorf("--dir is required")
	case size <= 0 || extents <= 0 || repeat <= 0:
		return fmt.Errorf("--extent-bytes, --extents and --repeat must be positive")
	case every <= 0 || patience <= 0:
		return fmt.Errorf("--poll and --patience must be positive")
	case every >= patience:
		return fmt.Errorf("polling every %s inside a patience of %s looks once", every, patience)
	}
	return nil
}

// row prints one measurement, including one that gave up.
//
// Space that never appeared is the answer this is looking for rather than a
// failure to look, so it is a row with its reason on it instead of an exit.
func row(w io.Writer, n int, r freed.Result, err error) {
	fmt.Fprintf(w, "%-6d %14s %14s %8d %14s", n,
		r.Unlink.Round(time.Microsecond), r.Visible.Round(time.Millisecond),
		r.Polls, pool.Bytes(r.Saw))
	if err != nil {
		fmt.Fprintf(w, "   %v", err)
	}
	fmt.Fprintln(w)
}

// fill lays down extents the way the agent does, committing the blocks so the
// space is really taken and really has to come back.
func fill(dir string, size int64, n, round int) ([]string, error) {
	var paths []string
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("freed-%d-%d.extent", round, i))
		if err := allocate(path, size); err != nil {
			remove(paths)
			return nil, err
		}
		paths = append(paths, path)
	}
	// Let the filesystem settle before the reading that the measurement starts
	// from, or the allocation's own lag is inside the release's.
	time.Sleep(250 * time.Millisecond)
	return paths, nil
}

// remove unlinks what fill laid down, which is the release being timed.
func remove(paths []string) error {
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
