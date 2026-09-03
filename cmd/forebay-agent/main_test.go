package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bytes"
	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
	"github.com/mayur-tolexo/forebay/internal/lease"
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
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--capacity-bytes=" + strconv.FormatInt(testCapacity, 10),
		"--reserved-bytes=" + strconv.FormatInt(testCapacity/8, 10),
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
	// A journal inside the pool startup reaps is removed at the first restart,
	// and the next grant fails on a directory that is gone.
	root := t.TempDir()
	pool := filepath.Join(root, "pool")
	err := withArgs(t,
		"--borrowed-dir="+pool,
		"--journal="+filepath.Join(pool, "state", "leases.json"),
		"--capacity-bytes=1024",
	)
	if !errors.Is(err, agent.ErrNestedPools) {
		t.Fatalf("journal inside the pool = %v, want ErrNestedPools", err)
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
		"--journal="+filepath.Join(root, "leases.json"),
		"--capacity-bytes=1024",
		"--reserved-bytes=4096",
	)
	if !errors.Is(err, pool.ErrOvercommit) {
		t.Fatalf("reserved beyond capacity = %v, want ErrOvercommit", err)
	}
}

func TestAnOperatorCanSupplyWhatDiscoveryWillNotVouchFor(t *testing.T) {
	// The point of wiring discovery in: an operator should not have to tell a
	// machine how big its own disks are.
	root := t.TempDir()
	args := []string{
		"--borrowed-dir=" + filepath.Join(root, "borrowed"),
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
	args[2] = "--capacity-bytes=" + strconv.FormatInt(available/4, 10)
	if err := withArgs(t, args...); err != nil {
		t.Errorf("a quarter of what is free was refused: %v", err)
	}
}

func TestNothingIsCreatedBeforeTheLayoutIsChecked(t *testing.T) {
	// Measuring used to create the pool directories first, so a layout the
	// agent was about to reject got written to disk anyway.
	root := t.TempDir()
	borrowed := filepath.Join(root, "borrowed")
	args := []string{
		"--borrowed-dir=" + borrowed,
		// Inside the pool startup reaps, which is refused.
		"--journal=" + filepath.Join(borrowed, "state", "leases.json"),
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
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		"--sysroot=" + filepath.Join("..", "..", "internal", "topology", "testdata", "gpu-node"),
		"--mountinfo=" + filepath.Join(root, "no-mount-table"),
	}
	if err := withArgs(t, args...); err == nil {
		t.Error("startup succeeded with nothing measurable and nothing given, want a refusal")
	}
}

func TestADeclaredReserveIsHonouredWhenNothingCanBeMeasured(t *testing.T) {
	// The refusal here names --reserved-bytes as the way through, and the check
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
		"--journal=" + filepath.Join(root, "state", "leases.json"),
		// The fixture mount table puts / on a pcie NVMe, so the pools pass the
		// locality check and startup reaches the reserve.
		"--sysroot=" + filepath.Join(fixtures, "gpu-node"),
		"--mountinfo=" + filepath.Join(fixtures, "mountinfo"),
	}
	if err := withArgs(t, args...); err == nil {
		t.Error("startup succeeded with the reserve unmeasurable and none declared, want a refusal")
	} else if !strings.Contains(err.Error(), "--reserved-bytes") {
		t.Errorf("refusal = %v, want it to name the flag that resolves it", err)
	}

	// Now take that advice, which must actually work.
	if err := withArgs(t, append(args, "--reserved-bytes=1048576")...); err != nil {
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

// kubeletServing answers /stats/summary with a node filesystem of the given
// size, which is the one field the pod source is checked against.
func kubeletServing(t *testing.T, capacity int64) (host string, port int) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"node":{"fs":{"capacityBytes":%d,"availableBytes":1}},"pods":[]}`, capacity)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), p
}

// tokenFile writes a service account token for the source to read.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestThePodSourceIsRefusedOnADifferentDevice(t *testing.T) {
	// Identity, not size. Pods are charged against the kubelet's filesystem,
	// so if the pools are on a second device a pod writing everything it
	// asked for takes nothing from Forebay, and acting on that would reclaim
	// a job's cache for pressure that cannot reach it.
	host, port := kubeletServing(t, 1<<40)
	// A path that exists but is not the pool directory's filesystem cannot be
	// conjured in a unit test, so the refusal is driven by size here and the
	// device comparison has its own test in internal/topology.
	fs := topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](2 << 40)}

	_, err := podSource(context.Background(), kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: filepath.Join(t.TempDir(), "absent"), BorrowedDir: t.TempDir(), Pools: fs})
	if err == nil {
		t.Fatal("the pod source was accepted while watching another filesystem")
	}
	if !strings.Contains(err.Error(), "say nothing about the space Forebay lends") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

func TestThePodSourceIsAcceptedOnTheSameDevice(t *testing.T) {
	// Two directories under one temporary root are on one filesystem, so the
	// device check answers yes and the size never has to be consulted. The
	// kubelet here reports a deliberately different size to prove that.
	host, port := kubeletServing(t, 1<<40)
	root := t.TempDir()
	kubeletRoot, borrowed := filepath.Join(root, "kubelet"), filepath.Join(root, "borrowed")
	for _, d := range []string{kubeletRoot, borrowed} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs := topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](9 << 40)}

	src, err := podSource(context.Background(), kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: kubeletRoot, BorrowedDir: borrowed, Pools: fs})
	if err != nil {
		t.Fatalf("podSource: %v", err)
	}
	if src.Name() != "pod requests" {
		t.Errorf("Name = %q", src.Name())
	}
}

func TestTheFallbackNamesThePathItCouldNotReach(t *testing.T) {
	// If the pools' own directory is the unreachable one, a message blaming
	// the kubelet root sends an operator to check a mount that is fine.
	const size = 2013991550976
	host, port := kubeletServing(t, size)
	fs := topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](size + 1)}
	borrowed := filepath.Join(t.TempDir(), "absent")

	_, err := podSource(context.Background(), kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: t.TempDir(), BorrowedDir: borrowed, Pools: fs})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), borrowed) {
		t.Errorf("the refusal names the wrong path: %v", err)
	}
}

func TestThePodSourceFallsBackToSizeWhenTheKubeletDirIsUnreachable(t *testing.T) {
	// The agent not being able to see the kubelet's directory is a packaging
	// detail, not evidence about the device, so it falls back rather than
	// refusing a node that is fine.
	const size = 2013991550976
	host, port := kubeletServing(t, size)
	fs := topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](size)}

	if _, err := podSource(context.Background(), kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: filepath.Join(t.TempDir(), "absent"), BorrowedDir: t.TempDir(), Pools: fs}); err != nil {
		t.Fatalf("podSource: %v", err)
	}
}

func TestThePodSourceIsRefusedWhenNothingCanBeCompared(t *testing.T) {
	// No device to compare and no size either. Guessing the two filesystems
	// match is the mistake this check exists to prevent.
	host, port := kubeletServing(t, 1<<40)
	_, err := podSource(context.Background(), kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: filepath.Join(t.TempDir(), "absent"), BorrowedDir: t.TempDir(), Pools: topology.PoolStorage{TotalBytes: topology.UnknownValue[int64]()}})
	if err == nil {
		t.Fatal("the pod source was accepted against an unmeasured filesystem")
	}
}

// settle waits for the source to reach a steady answer, since the attempt to
// build it runs behind the pass rather than inside it.
func settle(t *testing.T, src agent.Source, want bool) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		_, err = src.Observe(context.Background(), agent.WatchConfig{Headroom: 1}, 0)
		if (err == nil) == want {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

func TestAnUnreachableKubeletDoesNotStopTheAgentStarting(t *testing.T) {
	// On a reboot the agent and the kubelet come up together, and the agent
	// may well be first. Exiting would crash-loop it through exactly the
	// minutes when image pulls are filling the disk, and the free-space watch
	// it could have been running is the one that still works.
	//
	// Port 1 refuses connections, which is what an unreachable kubelet looks
	// like from here.
	got := optionalSources(kubeletOptions{
		Host: "127.0.0.1", Port: 1, TokenFile: tokenFile(t),
		Root: t.TempDir(), BorrowedDir: t.TempDir(),
		Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](1 << 40)},
	})
	if len(got) != 1 {
		t.Fatalf("got %d sources, want the input to exist and report itself broken", len(got))
	}
	// Present but failing, so the watch names it every pass. An input that
	// was asked for and silently absent looks exactly like a quiet cluster.
	if err := settle(t, got[0], false); err == nil {
		t.Error("an unreachable kubelet reported no problem")
	}
}

func TestProvingTheInputDoesNotRunInsideAPass(t *testing.T) {
	// A kubelet too slow to answer within the startup probe must not then be
	// waited on by the watch. Proving it is retried behind the passes, so a
	// pass costs nothing while that happens, and a check bounded by the pass
	// would instead tie its budget to how often free space is polled.
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, `{"node":{"fs":{"capacityBytes":1099511627776,"availableBytes":1}},"pods":[]}`)
	}))
	defer slow.Close()
	host, port := hostPortOf(t, slow.URL)

	src := optionalSources(kubeletOptions{
		Host: host, Port: port, TokenFile: tokenFile(t),
		Root: t.TempDir(), BorrowedDir: t.TempDir(),
		Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](1099511627776)},
	})[0]

	// The pass returns without waiting on the kubelet, and says why.
	start := time.Now()
	_, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 1}, 0)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the pass waited %s on the kubelet handshake", elapsed)
	}
	if err == nil {
		t.Error("a source that is not proved yet reported no problem")
	}
}

func TestAHealthyStartSaysNothingAboutBeingDegraded(t *testing.T) {
	// The first pass finding the input unproved would put a degraded line in
	// front of an operator on every restart and rollout, which is how the one
	// that matters gets skimmed past.
	const size = 1099511627776
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pods" {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		fmt.Fprintf(w, `{"node":{"fs":{"capacityBytes":%d,"availableBytes":1}},"pods":[]}`, size)
	}))
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	root := t.TempDir()
	kubeletRoot, borrowed := filepath.Join(root, "kubelet"), filepath.Join(root, "borrowed")
	for _, d := range []string{kubeletRoot, borrowed} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := optionalSources(kubeletOptions{
		Host: host, Port: port, TokenFile: tokenFile(t),
		Root: kubeletRoot, BorrowedDir: borrowed,
		Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](size)},
	})[0]

	// The very first pass, with no waiting and no retry: working already.
	if _, err := src.Observe(context.Background(), agent.WatchConfig{Headroom: 1}, 0); err != nil {
		t.Errorf("a healthy kubelet reported degraded on the first pass: %v", err)
	}
}

func TestThePodInputStartsWorkingWhenTheKubeletArrives(t *testing.T) {
	// Building it once and giving up would mean a kubelet that came up a
	// second after the agent was never used for the life of the process.
	var mu sync.Mutex
	var calls int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		refusing := calls == 1
		mu.Unlock()
		if refusing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/pods" {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		fmt.Fprint(w, `{"node":{"fs":{"capacityBytes":1099511627776,"availableBytes":1}},"pods":[]}`)
	}))
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	root := t.TempDir()
	kubeletRoot, borrowed := filepath.Join(root, "kubelet"), filepath.Join(root, "borrowed")
	for _, d := range []string{kubeletRoot, borrowed} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := optionalSources(kubeletOptions{
		Host: host, Port: port, TokenFile: tokenFile(t),
		Root: kubeletRoot, BorrowedDir: borrowed,
		Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](1099511627776)},
	})[0]

	if err := settle(t, src, true); err != nil {
		t.Errorf("the input did not start working once the kubelet did: %v", err)
	}
}

// hostPortOf splits a test server's URL.
func hostPortOf(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), port
}

func TestNoKubeletAskedForMeansNoPodInput(t *testing.T) {
	// The watch is reactive by default and says so; asking for nothing is not
	// a failure to report.
	if got := optionalSources(kubeletOptions{Host: "", Port: 10250, TokenFile: "", Root: "", BorrowedDir: "", Pools: topology.PoolStorage{}}); got != nil {
		t.Errorf("got %d sources without being asked for any", len(got))
	}
}

func TestAWorkingKubeletBecomesASource(t *testing.T) {
	// The other half: the same path returns the source when it can be built,
	// so the fallback above is not swallowing a working input.
	const size = 2013991550976
	host, port := kubeletServing(t, size)
	root := t.TempDir()
	kubeletRoot, borrowed := filepath.Join(root, "kubelet"), filepath.Join(root, "borrowed")
	for _, d := range []string{kubeletRoot, borrowed} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := optionalSources(kubeletOptions{Host: host, Port: port, TokenFile: tokenFile(t), Root: kubeletRoot, BorrowedDir: borrowed, Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](size)}})
	if len(got) != 1 {
		t.Fatalf("got %d sources from a working kubelet, want 1", len(got))
	}
	if got[0].Name() != "pod requests" {
		t.Errorf("Name = %q", got[0].Name())
	}
}

func TestADeviceMismatchNeverAsksTheKubelet(t *testing.T) {
	// This check runs on every pass, and a mismatch is an answer that will
	// not change while the process lives. Asking the kubelet first would pull
	// its whole stats response every pass for ever, to reach a verdict two
	// stat calls already had.
	var mu sync.Mutex
	var requests int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		fmt.Fprint(w, `{"node":{"fs":{"capacityBytes":1,"availableBytes":1}},"pods":[]}`)
	}))
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)

	// /dev is its own mount on every platform this runs on. Asserted rather
	// than assumed, because a test that quietly stopped comparing two
	// filesystems would pass for the wrong reason.
	borrowed := t.TempDir()
	if same, err := topology.SameFilesystem("/dev", borrowed); err != nil || same {
		t.Fatalf("this test needs two filesystems: SameFilesystem(/dev, %s) = %v, %v", borrowed, same, err)
	}

	_, err := podSource(context.Background(), kubeletOptions{
		Host: host, Port: port, TokenFile: tokenFile(t),
		Root: "/dev", BorrowedDir: borrowed,
		Pools: topology.PoolStorage{TotalBytes: topology.DiscoveredValue[int64](1)},
	})
	if err == nil {
		t.Fatal("two filesystems were accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Errorf("the kubelet was asked %d time(s) for a verdict stat already had", requests)
	}
}

func TestTheAgentAnswersReadsWhenAskedTo(t *testing.T) {
	// Until this the fast tier and the driver contract had callers only in
	// their own tests, so the agent guarded capacity nothing read from. This
	// is the first time the pieces answer a read in the binary.
	dir := t.TempDir()
	backend := filepath.Join(dir, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 3*(1<<20)+128)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(backend, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       lease.Config{ReclaimWithin: 30 * time.Second},
	}, pool.Accounting{Capacity: testCapacity, Reserved: 0}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fb%d", time.Now().UnixNano()))
	defer os.Remove(sock)
	serving, err := serveReads(a, servingOptions{
		Socket: sock, Backend: backendOptions{Dir: backend},
		TierBytes: testCapacity / 2, BlockBytes: 1 << 20, FirstReads: 64,
	})
	if err != nil {
		t.Fatalf("serving: %v", err)
	}
	defer serving.stop()

	c, err := dataserver.Dial("unix", sock, dataserver.ClientConfig{MaxReply: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A read that spans whole blocks and the short tail, so the answer comes
	// from the backend through the tier and back out over the socket.
	got, err := c.ReadRange("t1", "obj", 0, int64(len(content)))
	if err != nil {
		t.Fatalf("reading through the agent: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read back %d bytes, want %d", len(got), len(content))
	}
}

func TestServingIsRefusedWithoutWhatItNeeds(t *testing.T) {
	dir := t.TempDir()
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       lease.Config{ReclaimWithin: 30 * time.Second},
	}, pool.Accounting{Capacity: testCapacity}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, c := range []struct {
		name string
		opts servingOptions
	}{
		{"no backend", servingOptions{Socket: "/tmp/x", TierBytes: 1 << 20, BlockBytes: 1 << 20}},
		{"no tier capacity", servingOptions{Socket: "/tmp/x", Backend: backendOptions{Dir: dir}, BlockBytes: 1 << 20}},
		{"a backend that is not there", servingOptions{Socket: "/tmp/x", Backend: backendOptions{Dir: filepath.Join(dir, "absent")}, TierBytes: 1 << 20, BlockBytes: 1 << 20}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if s, err := serveReads(a, c.opts); err == nil {
				s.stop()
				t.Error("accepted")
			}
		})
	}
}

func TestAnAgentThatRestartsCanServeAgain(t *testing.T) {
	// A lease outlives the process that granted it, so a restart replays the
	// tier's from the journal and granting it again is refused. Found on a
	// node rather than here: the second start reported "id already granted"
	// and served nothing, which is a state a machine reaches on its own.
	dir := t.TempDir()
	backend := filepath.Join(dir, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("bytes that outlive a restart")
	if err := os.WriteFile(filepath.Join(backend, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       lease.Config{ReclaimWithin: 30 * time.Second},
	}
	acct := pool.Accounting{Capacity: testCapacity}

	for run := 1; run <= 3; run++ {
		a, _, err := agent.Open(cfg, acct, time.Now())
		if err != nil {
			t.Fatalf("run %d: opening: %v", run, err)
		}
		sock := filepath.Join(os.TempDir(), fmt.Sprintf("fb%d-%d", time.Now().UnixNano(), run))
		serving, err := serveReads(a, servingOptions{
			Socket: sock, Backend: backendOptions{Dir: backend},
			// A different size each run, so adopting the old lease rather
			// than replacing it would serve the wrong capacity.
			TierBytes: pool.Bytes(testCapacity / int64(run+1)), BlockBytes: 1 << 20, FirstReads: 8,
		})
		if err != nil {
			a.Close()
			t.Fatalf("run %d: serving: %v", run, err)
		}
		c, err := dataserver.Dial("unix", sock, dataserver.ClientConfig{MaxReply: 1 << 20})
		if err != nil {
			t.Fatalf("run %d: dialling: %v", run, err)
		}
		got, err := c.ReadRange("t1", "obj", 0, int64(len(content)))
		if err != nil {
			t.Errorf("run %d: reading: %v", run, err)
		} else if !bytes.Equal(got, content) {
			t.Errorf("run %d: read back %q", run, got)
		}
		c.Close()
		serving.stop()
		a.Close()
		os.Remove(sock)
	}
}

func TestReclaimingTakesTheTierWithIt(t *testing.T) {
	// An unlinked extent whose descriptor is still open keeps its blocks, so
	// the tier has to let go before the file does. Found on a node: the agent
	// reported returning 64MiB and free space rose by 4KiB.
	dir := t.TempDir()
	backend := filepath.Join(dir, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 8<<20)
	for i := range content {
		content[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(backend, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       lease.Config{ReclaimWithin: 30 * time.Second},
	}, pool.Accounting{Capacity: 512 << 20}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fbr%d", time.Now().UnixNano()))
	defer os.Remove(sock)
	sv, err := serveReads(a, servingOptions{
		Socket: sock, Backend: backendOptions{Dir: backend},
		TierBytes: 64 << 20, BlockBytes: 1 << 20, FirstReads: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sv.stop()

	c, err := dataserver.Dial("unix", sock, dataserver.ClientConfig{MaxReply: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Three reads: the second admits, the third hits.
	for i := 0; i < 3; i++ {
		if _, err := c.ReadRange("t1", "obj", 0, int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, resident := sv.tier.Stats(); resident == 0 {
		t.Fatal("nothing was cached, so this proves nothing about losing it")
	}

	if _, err := a.ReclaimCapacity(64<<20, time.Now()); err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if _, _, resident := sv.tier.Stats(); resident != 0 {
		t.Errorf("%d blocks are still held after the capacity was returned", resident)
	}
	// Counted rather than printed, since the hook runs inside the window the
	// reclaim deadline is measured over.
	if sv.Dropped() == 0 {
		t.Error("giving up the tier was not counted, so nothing can report it")
	}

	// And the read still works, from the backend rather than from a file
	// nobody can reach.
	before := sv.srv.Stats()
	got, err := c.ReadRange("t1", "obj", 0, 1<<20)
	if err != nil {
		t.Fatalf("reading after reclamation: %v", err)
	}
	if !bytes.Equal(got, content[:1<<20]) {
		t.Error("the bytes after reclamation are not the object's")
	}
	if after := sv.srv.Stats(); after.Hits != before.Hits {
		t.Errorf("a read after reclamation came from the tier, which no longer has capacity")
	}
}

func TestGivingUpTheCacheIsReportedOnEveryPassThatDoesIt(t *testing.T) {
	// A shortfall pass reclaims too, and can empty the tier doing it. Not
	// counting it there loses the number and attributes it to whichever
	// later pass happens to report one.
	var dropped atomic.Int64
	sv := &serving{dropped: &dropped}
	var last int64

	dropped.Store(100)
	first := cacheGivenUp(sv, &last)
	dropped.Store(110)
	second := cacheGivenUp(sv, &last)

	if !strings.Contains(first, "100") {
		t.Errorf("first report = %q, want the 100 blocks it gave up", first)
	}
	if !strings.Contains(second, "10") || strings.Contains(second, "110") {
		t.Errorf("second report = %q, want only the 10 since the last one", second)
	}
	if got := cacheGivenUp(sv, &last); got != "" {
		t.Errorf("a pass that gave up nothing said %q", got)
	}
}

func TestNotServingReportsNothingAboutTheCache(t *testing.T) {
	var last int64
	if got := cacheGivenUp(nil, &last); got != "" {
		t.Errorf("an agent that is not serving said %q", got)
	}
}

// TestDescribeHeadroomSaysWhichFloor covers the startup line, since a duration
// and a size are the same field to an operator reading one line and the two
// behave differently under load.
func TestDescribeHeadroomSaysWhichFloor(t *testing.T) {
	got := describeHeadroom(agent.WatchConfig{Headroom: 4 << 20})
	if !strings.Contains(got, "4.00MiB") {
		t.Errorf("a fixed floor reads %q", got)
	}
	got = describeHeadroom(agent.WatchConfig{HeadroomFor: 5 * time.Second, MinHeadroom: 1 << 20})
	if !strings.Contains(got, "5s") || !strings.Contains(got, "1.00MiB") {
		t.Errorf("a duration floor reads %q, want the duration and the minimum", got)
	}
}

// TestMovingTargetOnlyExplainsAMovingFloor keeps the reclaim line quiet when
// the floor is a size the operator configured and already knows.
func TestMovingTargetOnlyExplainsAMovingFloor(t *testing.T) {
	tick := agent.Tick{Target: 8 << 20, Rate: 4 << 20}
	if got := movingTarget(agent.WatchConfig{Headroom: 8 << 20}, tick); got != "" {
		t.Errorf("a fixed floor explained itself: %q", got)
	}
	got := movingTarget(agent.WatchConfig{HeadroomFor: 2 * time.Second, MinHeadroom: 1}, tick)
	for _, want := range []string{"8.00MiB", "2s", "4.00MiB/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("the reclaim line does not say %q: %q", want, got)
		}
	}
}
