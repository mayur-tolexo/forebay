package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// withArgs runs the binary's entry point under a fresh flag set, so tests do
// not inherit each other's parsed state.
func withArgs(t *testing.T, args ...string) error {
	t.Helper()
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	flag.CommandLine = flag.NewFlagSet("forebay-agent", flag.ContinueOnError)
	flag.CommandLine.SetOutput(os.Stderr)
	os.Args = append([]string{"forebay-agent"}, args...)
	return run()
}

// nodeArgs is a valid configuration against a temporary directory.
func nodeArgs(t *testing.T, root string) []string {
	t.Helper()
	return []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--capacity-bytes=" + strconv.FormatInt(int64(8*pool.TiB), 10),
		"--compute-bytes=" + strconv.FormatInt(int64(1*pool.TiB), 10),
		"--donated-bytes=" + strconv.FormatInt(int64(2*pool.TiB), 10),
	}
}

func TestVersionFlagPrintsAndStops(t *testing.T) {
	if err := withArgs(t, "--version"); err != nil {
		t.Fatalf("--version = %v, want nil", err)
	}
}

func TestStartupSucceedsAgainstAFreshNode(t *testing.T) {
	if err := withArgs(t, nodeArgs(t, t.TempDir())...); err != nil {
		t.Fatalf("startup = %v, want nil", err)
	}
}

func TestStartupCleansUpCapacityNothingAccountsFor(t *testing.T) {
	root := t.TempDir()
	args := nodeArgs(t, root)
	if err := withArgs(t, args...); err != nil {
		t.Fatalf("first startup = %v", err)
	}

	orphan := filepath.Join(root, "borrowed", "leaked")
	if err := os.WriteFile(orphan, []byte("x"), 0o640); err != nil {
		t.Fatalf("planting orphan: %v", err)
	}
	if err := withArgs(t, args...); err != nil {
		t.Fatalf("second startup = %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Error("orphan extent survived startup")
	}
}

func TestARefusedLayoutIsReportedRatherThanRun(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "pool")
	err := withArgs(t,
		"--borrowed-dir="+shared,
		"--donated-dir="+shared,
		"--journal="+filepath.Join(root, "leases.json"),
		"--capacity-bytes=1024",
	)
	if !errors.Is(err, agent.ErrSamePool) {
		t.Fatalf("shared pool directories = %v, want ErrSamePool", err)
	}
}

func TestAMissingReclaimDeadlineStopsStartup(t *testing.T) {
	// Without one, every elastic grant would be refused, so a node would lend
	// nothing and be unable to say why. Failing at startup is louder.
	args := append(nodeArgs(t, t.TempDir()), "--reclaim-within=0")
	if err := withArgs(t, args...); err == nil {
		t.Fatal("startup with no reclaim deadline succeeded, want it refused")
	}
}

func TestImpossibleAccountingIsRefused(t *testing.T) {
	root := t.TempDir()
	err := withArgs(t,
		"--borrowed-dir="+filepath.Join(root, "borrowed"),
		"--donated-dir="+filepath.Join(root, "donated"),
		"--journal="+filepath.Join(root, "leases.json"),
		"--capacity-bytes=1024",
		"--compute-bytes=4096",
	)
	if !errors.Is(err, pool.ErrOvercommit) {
		t.Fatalf("compute beyond capacity = %v, want ErrOvercommit", err)
	}
}
