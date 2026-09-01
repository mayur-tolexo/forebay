package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/topology"
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

// testCapacity is small enough that any filesystem able to hold a temporary
// directory can deliver it.
//
// Tests used to declare eight terabytes, which is a number no test machine
// has, and the agent now refuses a configuration promising more than the disk
// can hand over. Sizing these from the real filesystem was the obvious repair
// and the wrong one: it made hermetic tests depend on the host's free space
// and let them skip themselves, which is how a regression test stops being
// one. A test that exercises startup rather than sizing should not have to
// measure anything.
const testCapacity = 8 << 20

// nodeArgs is a valid configuration against a temporary directory.
func nodeArgs(t *testing.T, root string) []string {
	t.Helper()
	return []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--capacity-bytes=" + strconv.FormatInt(testCapacity, 10),
		"--compute-bytes=" + strconv.FormatInt(testCapacity/8, 10),
		"--donated-bytes=" + strconv.FormatInt(testCapacity/4, 10),
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

func TestAnOperatorCanSupplyWhatDiscoveryWillNotVouchFor(t *testing.T) {
	// The point of wiring discovery in: an operator should not have to tell a
	// machine how big its own disks are.
	root := t.TempDir()
	args := []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--sysroot=" + filepath.Join("..", "..", "internal", "topology", "testdata", "gpu-node"),
		"--capacity-bytes=" + strconv.FormatInt(testCapacity, 10),
	}
	if err := withArgs(t, args...); err != nil {
		t.Fatalf("startup with an operator-supplied capacity = %v, want nil", err)
	}
}

func TestStartupRefusesStorageItCannotProveIsLocal(t *testing.T) {
	// The pools sit on a temporary directory whose backing device cannot be
	// identified from the fixture mount table. An unknown never satisfies a
	// requirement, and being local is a requirement for lending, so this
	// refuses rather than offering storage that might be a network volume.
	root := t.TempDir()
	err := withArgs(t,
		"--borrowed-dir="+filepath.Join(root, "borrowed"),
		"--donated-dir="+filepath.Join(root, "donated"),
		"--journal="+filepath.Join(root, "state", "leases.json"),
		"--sysroot="+filepath.Join(root, "empty-sysroot"),
		"--mountinfo="+filepath.Join(root, "no-mount-table"),
	)
	if err == nil {
		t.Fatal("startup succeeded on storage of unknown locality, want a refusal")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("refusal = %v, want it to say why", err)
	}
}

func TestAnExplicitCapacityOverridesDiscovery(t *testing.T) {
	root := t.TempDir()
	args := append(nodeArgs(t, root),
		"--sysroot="+filepath.Join("..", "..", "internal", "topology", "testdata", "gpu-node"))
	if err := withArgs(t, args...); err != nil {
		t.Fatalf("startup with an explicit capacity = %v, want nil", err)
	}
}

func TestTheAgentWillNotLendSpaceTheFilesystemHasAlreadyGivenAway(t *testing.T) {
	// The defect this guards: capacity was the filesystem's total size, so on a
	// node whose disk was 1.83 TiB with 559 GiB already used by the operating
	// system and container images, the agent offered the whole 1.83 TiB. The
	// lease manager would then grant into space that does not exist, fill the
	// node's root filesystem and take the kubelet and every pod down with it.
	//
	// The numbers are synthetic so this asserts on every machine. An earlier
	// version measured the real filesystem and skipped when it could not,
	// which let the regression test for a node-killing defect pass without
	// checking anything.
	const total, available = 10 << 40, 4 << 40
	fs := topology.PoolStorage{
		TotalBytes:     topology.DiscoveredValue[int64](total),
		AvailableBytes: topology.DiscoveredValue[int64](available),
	}
	root := t.TempDir()

	deliverable, ok := deliverableBytes(fs, root)
	if !ok {
		t.Fatal("deliverableBytes could not measure an empty pool on a described filesystem")
	}
	// An empty pool holds nothing, so what can be delivered is what is free.
	if deliverable != available {
		t.Fatalf("deliverable = %s, want the %s that is free", deliverable, pool.Bytes(available))
	}
	// The whole point: the six terabytes in use by others are not Forebay's
	// to lend, so they land in the compute reserve.
	if reserve := pool.Bytes(total) - deliverable; reserve != total-available {
		t.Errorf("reserve = %s, want the %s already in use", reserve, pool.Bytes(total-available))
	}
}

func TestCapacityAlreadyLentIsStillOurs(t *testing.T) {
	// Free space alone would shrink the ceiling by everything currently lent,
	// so a node would forget a little more of its own capacity on every
	// restart. What the pools already hold is added back.
	const available = 4 << 40
	fs := topology.PoolStorage{
		TotalBytes:     topology.DiscoveredValue[int64](10 << 40),
		AvailableBytes: topology.DiscoveredValue[int64](available),
	}
	root := t.TempDir()
	empty, ok := deliverableBytes(fs, root)
	if !ok {
		t.Fatal("deliverableBytes failed on an empty pool")
	}

	const extent = 1 << 20
	if err := os.WriteFile(filepath.Join(root, "extent"), make([]byte, extent), 0o640); err != nil {
		t.Fatal(err)
	}
	held, ok := deliverableBytes(fs, root)
	if !ok {
		t.Fatal("deliverableBytes failed once the pool held something")
	}
	// Allocated blocks, so the extent costs about a megabyte rather than
	// exactly one, but it must be counted as ours rather than lost.
	if grew := held - empty; grew < extent/2 || grew > 4*extent {
		t.Errorf("deliverable grew by %s after Forebay wrote %s of its own, want it added back",
			grew, pool.Bytes(extent))
	}
}

func TestAnOverriddenCapacityIsStillCheckedAgainstTheDisk(t *testing.T) {
	// The refusal an operator meets first tells them to pass --capacity-bytes.
	// While the ceiling lived inside the discovery branch, following that
	// advice switched off the guard against promising space the filesystem
	// does not have, so our own instructions led to the node-killing case.
	root := t.TempDir()
	fs := topology.DescribePool("/", "/proc/self/mountinfo", root)
	available, ok := fs.AvailableBytes.Known()
	if !ok {
		t.Skip("cannot measure the test filesystem")
	}

	args := []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		// Far more than any filesystem could hand over, declared by hand.
		"--capacity-bytes=" + strconv.FormatInt(available*16, 10),
	}
	err := withArgs(t, args...)
	if err == nil {
		t.Fatal("an overridden capacity beyond the disk was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "can deliver") {
		t.Errorf("error = %v, want it to say what the filesystem can actually deliver", err)
	}

	// A modest slice of the same filesystem is a perfectly good override and
	// must still be allowed: the check is a ceiling, not a demand that
	// capacity equal the disk.
	args[3] = "--capacity-bytes=" + strconv.FormatInt(available/4, 10)
	if err := withArgs(t, args...); err != nil {
		t.Errorf("a quarter of what is free was refused: %v", err)
	}
}

func TestPoolsSplitAcrossFilesystemsAreRefused(t *testing.T) {
	// One capacity figure describes one filesystem. With the pools on two, the
	// donated bytes are deducted from a device that never held them and the
	// device that did is never measured, so the accounting cannot be right.
	on := func(device string) topology.PoolStorage {
		return topology.PoolStorage{Device: device}
	}
	if err := requireOneFilesystem(on("nvme0n1p2"), on("sdb1")); err == nil {
		t.Error("two devices were accepted, want a refusal")
	} else if !strings.Contains(err.Error(), "cannot describe both") {
		t.Errorf("error = %v, want it to say one capacity figure cannot describe both", err)
	}
	if err := requireOneFilesystem(on("nvme0n1p2"), on("nvme0n1p2")); err != nil {
		t.Errorf("one device for both pools = %v, want nil", err)
	}
	// An unidentified device is not a mismatch. Discovery has already refused
	// it for not being provably local, and an operator who overrode that has
	// taken the numbers on.
	for _, pair := range [][2]string{{"nvme0n1p2", ""}, {"", "sdb1"}, {"", ""}} {
		if err := requireOneFilesystem(on(pair[0]), on(pair[1])); err != nil {
			t.Errorf("requireOneFilesystem(%q, %q) = %v, want nil", pair[0], pair[1], err)
		}
	}
}

func TestNothingIsCreatedBeforeTheLayoutIsChecked(t *testing.T) {
	// Measuring used to create the pool directories first, so a pair the agent
	// was about to reject got written to disk anyway.
	root := t.TempDir()
	borrowed := filepath.Join(root, "borrowed")
	args := []string{
		"--borrowed-dir=" + borrowed,
		"--donated-dir=" + filepath.Join(borrowed, "nested"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--capacity-bytes=" + strconv.FormatInt(int64(8*pool.TiB), 10),
	}
	if err := withArgs(t, args...); !errors.Is(err, agent.ErrNestedPools) {
		t.Fatalf("startup = %v, want ErrNestedPools", err)
	}
	if _, err := os.Stat(borrowed); err == nil {
		t.Error("the pool directory was created despite the layout being rejected")
	}
}

func TestTheReserveRefusesToGuess(t *testing.T) {
	// A filesystem that did not say how much of it is free cannot have the
	// space already in use worked out, and assuming none is how the node ends
	// up lending storage that is spoken for.
	root := t.TempDir()
	unmeasured := topology.PoolStorage{
		TotalBytes:     topology.DiscoveredValue[int64](1 << 40),
		AvailableBytes: topology.UnknownValue[int64](),
	}
	if _, ok := deliverableBytes(unmeasured, root); ok {
		t.Error("deliverableBytes answered for a filesystem that did not say what is free")
	}

	// And the refusal has to reach the operator with the flag that resolves
	// it, since discovery cannot proceed without this number.
	args := []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--sysroot=" + filepath.Join("..", "..", "internal", "topology", "testdata", "gpu-node"),
		"--mountinfo=" + filepath.Join(root, "no-mount-table"),
	}
	if err := withArgs(t, args...); err == nil {
		t.Error("startup succeeded with nothing measurable and nothing given, want a refusal")
	}
}

func TestADeclaredReserveIsHonouredWhenNothingCanBeMeasured(t *testing.T) {
	// The refusal here names --compute-bytes as the way through, and the check
	// that raised it never looked at the flag, so an operator taking the advice
	// got the same refusal back. A remedy the refusing code ignores is worse
	// than no remedy: it sends people to a dead end with confidence.
	//
	// The pool holds a directory the agent cannot walk, which is all it takes
	// for what Forebay already holds to become unmeasurable. A directory left
	// behind by another uid does it on a real node.
	if os.Geteuid() == 0 {
		t.Skip("running as root, which walks into an unreadable directory anyway")
	}
	root := t.TempDir()
	borrowed := filepath.Join(root, "borrowed")
	blocked := filepath.Join(borrowed, "blocked")
	if err := os.MkdirAll(blocked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restored so the temporary directory can be cleaned up again.
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

	fixtures := filepath.Join("..", "..", "internal", "topology", "testdata")
	args := []string{
		"--borrowed-dir=" + borrowed,
		"--donated-dir=" + filepath.Join(root, "donated"),
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		// The fixture mount table puts / on a pcie NVMe, so the pools pass the
		// locality check and startup reaches the reserve.
		"--sysroot=" + filepath.Join(fixtures, "gpu-node"),
		"--mountinfo=" + filepath.Join(fixtures, "mountinfo"),
	}
	if err := withArgs(t, args...); err == nil {
		t.Error("startup succeeded with the reserve unmeasurable and none declared, want a refusal")
	} else if !strings.Contains(err.Error(), "--compute-bytes") {
		t.Errorf("refusal = %v, want it to name the flag that resolves it", err)
	}

	// Now take that advice, which must actually work.
	if err := withArgs(t, append(args, "--compute-bytes=1048576")...); err != nil {
		t.Errorf("declaring the reserve the refusal asked for = %v, want nil", err)
	}
}

func TestTheLivenessProbeIsItsOwnInvocation(t *testing.T) {
	// The process being judged may be wedged, so it cannot answer for itself.
	// The probe runs as a separate execution against the pool on disk, which
	// is what lets a kubelet kill the holder and free the lock.
	root := t.TempDir()
	borrowed := filepath.Join(root, "borrowed")

	// Nothing has started here, so there is no progress to find.
	err := withArgs(t, "--liveness", "--borrowed-dir="+borrowed)
	if !errors.Is(err, agent.ErrStalled) {
		t.Errorf("liveness against an empty pool = %v, want ErrStalled", err)
	}

	// Start an agent, which writes its first heartbeat as part of startup.
	if err := withArgs(t, nodeArgs(t, root)...); err != nil {
		t.Fatalf("startup: %v", err)
	}
	if err := withArgs(t, "--liveness", "--borrowed-dir="+borrowed); err != nil {
		t.Errorf("liveness after a clean startup = %v, want nil", err)
	}
	// A window shorter than the heartbeat's age condemns it, which is the
	// operator's dial rather than the agent's.
	if err := withArgs(t, "--liveness", "--borrowed-dir="+borrowed, "--stale-after=1ns"); !errors.Is(err, agent.ErrStalled) {
		t.Errorf("liveness with a 1ns window = %v, want ErrStalled", err)
	}
	// And it needs to be told where to look.
	if err := withArgs(t, "--liveness"); err == nil {
		t.Error("liveness with no pool directory succeeded, want a refusal")
	}
}

func TestAnUnjudgeableHeartbeatDoesNotFailTheProbe(t *testing.T) {
	// Splitting the error is only worth anything if the probe acts on it: a
	// bad mount that fails the probe kills a healthy agent every time, and the
	// restart never fixes the mount.
	root := t.TempDir()
	borrowed := filepath.Join(root, "borrowed")
	if err := withArgs(t, nodeArgs(t, root)...); err != nil {
		t.Fatalf("startup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(borrowed, ".forebay-heartbeat"), []byte("rubbish"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := withArgs(t, "--liveness", "--borrowed-dir="+borrowed); err != nil {
		t.Errorf("liveness on an unreadable heartbeat = %v, want it to pass rather than kill", err)
	}
}

func TestWatchingWithoutAHeadroomTargetIsRefused(t *testing.T) {
	// The value has no defensible default, so the agent refuses rather than
	// putting a guessed number in the path that decides when a job loses its
	// cache. Same treatment as a missing reclaim deadline.
	root := t.TempDir()
	err := withArgs(t, append(nodeArgs(t, root), "--watch")...)
	if !errors.Is(err, agent.ErrNoHeadroom) {
		t.Errorf("watch with no headroom = %v, want ErrNoHeadroom", err)
	}
}
