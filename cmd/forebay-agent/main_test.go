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
	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/filedriver"
	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/metrics"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/residency"
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

// TestRecordPublishesEveryPass covers the counter that makes silence
// unambiguous: a watch that died and a cluster where nothing is happening
// produce the same absence of events, and only a number that always moves
// tells them apart.
func TestRecordPublishesEveryPass(t *testing.T) {
	reg := metrics.New()
	if err := metrics.Node(reg); err != nil {
		t.Fatal(err)
	}
	// A pass in which nothing at all happened.
	record(reg, agent.Tick{Target: 1 << 30})

	var out strings.Builder
	reg.WriteTo(&out)
	got := out.String()
	if !strings.Contains(got, metrics.WatchPasses+" 1") {
		t.Errorf("a quiet pass was not counted:\n%s", got)
	}
	if !strings.Contains(got, metrics.HeadroomBytes+" 1.073741824e+09") {
		t.Errorf("the floor this pass kept was not published:\n%s", got)
	}
}

// TestReclaimLatencyIsLabelledByTheClassTaken matters because only elastic
// promises a deadline, and reading both classes against it would judge
// opportunistic capacity by a promise it never made.
func TestReclaimLatencyIsLabelledByTheClassTaken(t *testing.T) {
	for _, c := range []struct {
		bounded bool
		want    string
	}{{true, `class="elastic"`}, {false, `class="opportunistic"`}} {
		reg := metrics.New()
		metrics.Node(reg)
		record(reg, agent.Tick{Reclaimed: 1 << 20, Elapsed: 250 * time.Millisecond, Bounded: c.bounded})

		var out strings.Builder
		reg.WriteTo(&out)
		got := out.String()
		if !strings.Contains(got, c.want) {
			t.Errorf("bounded=%v did not label the class %s:\n%s", c.bounded, c.want, got)
		}
		if !strings.Contains(got, "_sum{"+c.want+"} 0.25") {
			t.Errorf("bounded=%v recorded the wrong latency:\n%s", c.bounded, got)
		}
	}
}

// TestAShortfallIsPublished covers the number an operator most needs, since it
// says the node is where it would have been with no lending at all.
func TestAShortfallIsPublished(t *testing.T) {
	reg := metrics.New()
	metrics.Node(reg)
	record(reg, agent.Tick{Shortfall: 4096})

	var out strings.Builder
	reg.WriteTo(&out)
	if !strings.Contains(out.String(), metrics.ReclaimShortfall+" 4096") {
		t.Errorf("a shortfall was not published:\n%s", out.String())
	}
}

// TestRecordWithoutARegistryIsSafe covers an agent started with no metrics
// address, which must reclaim exactly as it did before.
func TestRecordWithoutARegistryIsSafe(t *testing.T) {
	record(nil, agent.Tick{Reclaimed: 1, Shortfall: 1})
}

// openAgent builds an agent over a temporary pool, with the journal outside it
// since startup reaps the pool.
func openAgent(t *testing.T, capacity pool.Bytes) *agent.Agent {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "borrowed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: dir,
		JournalPath: filepath.Join(root, "leases.json"),
		Lease:       lease.Config{ReclaimWithin: time.Second, GuaranteedFraction: 0.5},
	}, pool.Accounting{Capacity: capacity}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// TestDrainReturnsWhatCanBeReturned covers the ordinary upgrade: a node lent
// reclaimable capacity and gets all of it back without waiting for terms.
func TestDrainReturnsWhatCanBeReturned(t *testing.T) {
	a := openAgent(t, 8<<20)
	now := time.Now()
	for i, c := range []lease.Class{lease.Opportunistic, lease.Elastic} {
		id := fmt.Sprintf("l-%d", i)
		if err := a.Grant(lease.Lease{ID: id, Class: c, Size: 1 << 20, Term: time.Hour}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := drainNode(a, now); err != nil {
		t.Fatalf("draining a node holding only reclaimable capacity: %v", err)
	}
	if got := a.Accounting().Borrowed; got != 0 {
		t.Errorf("%s was still lent after a drain", got)
	}
}

// TestDrainWillNotTakeACheckpoint is the interlock between this and RFC-0013.
// A drain that took a guaranteed lease would destroy a job's progress, one
// node at a time, while reporting success.
func TestDrainWillNotTakeACheckpoint(t *testing.T) {
	a := openAgent(t, 8<<20)
	now := time.Now()
	if err := a.Grant(lease.Lease{ID: "staging", Class: lease.Guaranteed, Size: 1 << 20, Term: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	err := drainNode(a, now)
	if err == nil {
		t.Fatal("a drain took a guaranteed lease and reported success")
	}
	for _, want := range []string{"only copy", "drain again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if got := a.Accounting().Borrowed; got == 0 {
		t.Error("the checkpoint's capacity was returned anyway")
	}
}

// TestDrainingAnEmptyNodeSucceeds keeps a rolling upgrade from stopping at a
// node that had lent nothing.
func TestDrainingAnEmptyNodeSucceeds(t *testing.T) {
	if err := drainNode(openAgent(t, 8<<20), time.Now()); err != nil {
		t.Errorf("draining a node that lent nothing failed: %v", err)
	}
}

// TestStillHeldDoesNotBlameACheckpointForAFault matters because the two reasons
// call for opposite things: waiting is right for staging and wrong for a lease
// the ladder should have taken, which an operator would otherwise wait on
// forever.
func TestStillHeldDoesNotBlameACheckpointForAFault(t *testing.T) {
	err := stillHeld([]lease.Lease{
		{ID: "stuck", Class: lease.Elastic, Size: 2 << 20},
		{ID: "staging", Class: lease.Guaranteed, Size: 1 << 20},
	})
	if err == nil {
		t.Fatal("a lease the ladder should have taken was reported as a clean drain")
	}
	if !strings.Contains(err.Error(), "should have been able to take") {
		t.Errorf("a stuck lease was reported as a checkpoint to wait on: %v", err)
	}

	if err := stillHeld(nil); err != nil {
		t.Errorf("a node holding nothing was reported as still held: %v", err)
	}
	if err := stillHeld([]lease.Lease{{Class: lease.Guaranteed, Size: 1 << 20}}); err == nil ||
		!strings.Contains(err.Error(), "only copy") {
		t.Errorf("staging alone was not named as staging: %v", err)
	}
}

// TestDrainOutcome covers every way a drain can end, including the two an
// agent cannot be driven into from a command line.
func TestDrainOutcome(t *testing.T) {
	clean := agent.Reclamation{}
	staging := []lease.Lease{{ID: "ckpt", Class: lease.Guaranteed, Size: 1 << 20}}

	if err := drainOutcome(clean, nil, nil); err != nil {
		t.Errorf("a node that returned everything was not drained: %v", err)
	}

	// A deadline missed is a promise broken, not capacity kept: the node did
	// drain, and failing it here would stop a rollout that should continue.
	if err := drainOutcome(clean, fmt.Errorf("x: %w", agent.ErrDeadlineMissed), nil); err != nil {
		t.Errorf("a slow but complete drain was reported as a failure: %v", err)
	}

	// An extent that could not be unlinked is capacity that did not come back,
	// whatever the accounting says.
	if err := drainOutcome(clean, errors.New("could not unlink"), nil); err == nil {
		t.Error("extents that could not be unlinked were reported as a clean drain")
	}

	// Reclaim stopping part way leaves leases held for a reason that is not a
	// checkpoint, so it must not be reported as one.
	part := agent.Reclamation{Result: lease.Result{Err: errors.New("accounting")}}
	err := drainOutcome(part, nil, staging)
	if err == nil || !strings.Contains(err.Error(), "stopped part way") {
		t.Errorf("a reclaim that stopped part way was reported as %v", err)
	}

	if err := drainOutcome(clean, nil, staging); err == nil ||
		!strings.Contains(err.Error(), "only copy") {
		t.Errorf("held staging was reported as %v", err)
	}
}

// TestTheTierIsPublishedAsALevel covers the gauges the watch pass sets. They
// describe a level rather than an event, which is why they are read on the
// pass instead of written on every block.
func TestTheTierIsPublishedAsALevel(t *testing.T) {
	dir := t.TempDir()
	backend := filepath.Join(dir, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 3*(1<<20))
	if err := os.WriteFile(filepath.Join(backend, "obj"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       lease.Config{ReclaimWithin: 30 * time.Second},
	}, pool.Accounting{Capacity: testCapacity}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fb%d", time.Now().UnixNano()))
	defer os.Remove(sock)
	sv, err := serveReads(a, servingOptions{
		Socket: sock, Backend: backendOptions{Dir: backend},
		TierBytes: testCapacity / 2, BlockBytes: 1 << 20, FirstReads: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sv.stop()

	// Twice, so the second read is admitted and the tier holds something.
	for i := 0; i < 2; i++ {
		if _, err := sv.srv.ReadRange(context.Background(), "t1", "obj", 0, 1<<20); err != nil {
			t.Fatal(err)
		}
	}

	reg := metrics.New()
	if err := metrics.Node(reg); err != nil {
		t.Fatal(err)
	}
	recordTier(reg, sv)

	var out strings.Builder
	if _, err := reg.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, metrics.TierBytes+" 1.048576e+06") {
		t.Errorf("the tier holds a block and did not say so:\n%s", got)
	}
	// The saving rests on no comparable miss yet, so the cover is zero and the
	// number is published as such rather than withheld.
	if !sampled(got, metrics.TierSavingCover) {
		t.Errorf("the covered share of the saving was not published:\n%s", got)
	}
}

// sampled reports whether a metric has a value in the exposition, as against
// merely being declared. An unset gauge emits its HELP and TYPE and no sample,
// and matching the name as a substring finds the HELP line instead.
func sampled(text, name string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			return true
		}
	}
	return false
}

// TestNothingServingPublishesNoTier keeps a node that is not serving from
// reporting a tier that saved nothing, which is a different claim from having
// no tier at all. A gauge nobody sets emits no sample, so the difference
// survives all the way to the scrape.
func TestNothingServingPublishesNoTier(t *testing.T) {
	reg := metrics.New()
	if err := metrics.Node(reg); err != nil {
		t.Fatal(err)
	}
	recordTier(reg, nil)

	var out strings.Builder
	if _, err := reg.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{metrics.TierBytes, metrics.TierSavedSeconds, metrics.TierSavingCover} {
		if sampled(out.String(), name) {
			t.Errorf("a node with no read path published %s", name)
		}
	}
}

// TestPrefetchIsOffUnlessTheFlagSaysSo covers the default. RFC-0011's depth
// and accuracy floor are guesses, and a wrong prediction spends bandwidth on a
// node whose bandwidth feeds an accelerator, so an operator has to ask.
func TestPrefetchIsOffUnlessTheFlagSaysSo(t *testing.T) {
	if got := prefetchConfig(false, 8, 0.5); got != nil {
		t.Errorf("prefetch was configured without being asked for: %+v", got)
	}

	got := prefetchConfig(true, 12, 0.75)
	if got == nil {
		t.Fatal("prefetch was asked for and not configured")
	}
	if got.Depth != 12 || got.MinAccuracy != 0.75 {
		t.Errorf("the flags did not reach the configuration: %+v", got)
	}
	// The rest comes from the default, which the detector has to accept.
	if err := got.Validate(); err != nil {
		t.Errorf("the configuration the flags built is one the detector refuses: %v", err)
	}
}

// TestABadPrefetchFlagIsRefusedAtStartup keeps a node from running with a
// prefetch configuration that predicts from noise, which is what a depth or a
// floor outside its bounds would do.
func TestABadPrefetchFlagIsRefusedAtStartup(t *testing.T) {
	for _, c := range []struct {
		name  string
		depth int
		floor float64
	}{
		{"no depth", 0, 0.5},
		{"a floor above one", 8, 1.5},
	} {
		cfg := prefetchConfig(true, c.depth, c.floor)
		if cfg == nil {
			t.Fatalf("%s: nothing was configured", c.name)
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// residencyPieces builds a tier over a backend holding one object, which is
// what a reporter needs to say anything.
func residencyPieces(t *testing.T, object string, size int) (*fasttier.Cache, *driver.Backend) {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, object), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := filedriver.New(store)
	if err != nil {
		t.Fatal(err)
	}
	back, err := driver.Open(fd)
	if err != nil {
		t.Fatal(err)
	}
	tier, err := fasttier.New(fasttier.Config{BlockSize: 1 << 20, FirstReadLimit: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tier.Close() })
	extent := filepath.Join(dir, "extent")
	if err := os.WriteFile(extent, make([]byte, 64<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tier.AddCapacity("lease", extent); err != nil {
		t.Fatal(err)
	}
	return tier, back
}

// hold makes n blocks of an object resident, the way the read path does.
func hold(t *testing.T, tier *fasttier.Cache, object string, n int) {
	t.Helper()
	body := make([]byte, 1<<20)
	for i := 0; i < n; i++ {
		k := fasttier.Key{Tenant: "t1", Block: fasttier.BlockRef{Backend: "store", Object: object, Index: int64(i)}}
		tier.Read(k)
		if _, err := tier.Admit(k, body, false); err != nil {
			t.Fatal(err)
		}
		tier.Read(k)
		if admitted, err := tier.Admit(k, body, false); err != nil || !admitted {
			t.Fatalf("holding block %d: admitted=%v err=%v", i, admitted, err)
		}
	}
}

// TestResidencyReportsWhatTheNodeActuallyHolds is what a controller turns into
// a node label, so it has to describe this node rather than this node's guess.
func TestResidencyReportsWhatTheNodeActuallyHolds(t *testing.T) {
	const object = "shard"
	tier, back := residencyPieces(t, object, 16<<20)
	hold(t, tier, object, 13)

	got := newResidencyReporter(tier, back).report(context.Background())
	if len(got) != 1 {
		t.Fatalf("reported %d datasets, want one: %+v", len(got), got)
	}
	if got[0].Level != "most" {
		t.Errorf("13 of 16 blocks reported as %q, want most", got[0].Level)
	}
	if got[0].Blocks != 13 || got[0].Total != 16<<20 {
		t.Errorf("reported %d blocks of %d bytes", got[0].Blocks, got[0].Total)
	}
	// The label key is what the scheduler matches, so it travels with the
	// report rather than being derived twice.
	if got[0].Label != residency.Key("t1", object) {
		t.Errorf("label = %q, want the key a scheduler matches", got[0].Label)
	}
	if got[0].Rack == got[0].Label {
		t.Error("the rack label is the same as the node label")
	}
}

// TestAFullyResidentObjectIsNotOverCounted covers the tail block, which is
// counted whole and would otherwise make an object read as larger than it is.
func TestAFullyResidentObjectIsNotOverCounted(t *testing.T) {
	const object = "shard"
	// Four and a bit blocks, so the fifth is a tail.
	tier, back := residencyPieces(t, object, 4<<20+4096)
	hold(t, tier, object, 5)

	got := newResidencyReporter(tier, back).report(context.Background())
	if len(got) != 1 {
		t.Fatalf("reported %d datasets, want one", len(got))
	}
	if got[0].Fraction > 1 {
		t.Errorf("fraction = %v, which says the node holds more than exists", got[0].Fraction)
	}
	if got[0].Level != "most" {
		t.Errorf("a fully resident object reported as %q", got[0].Level)
	}
}

// TestADatasetWhoseSizeIsUnknownIsLeftOut matters because a scheduler acting
// on a residency this node invented would place work for data that is not
// here.
func TestADatasetWhoseSizeIsUnknownIsLeftOut(t *testing.T) {
	const object = "shard"
	tier, back := residencyPieces(t, object, 16<<20)
	hold(t, tier, object, 8)

	// A block of an object the backend does not have.
	k := fasttier.Key{Tenant: "t1", Block: fasttier.BlockRef{Backend: "store", Object: "ghost", Index: 0}}
	tier.Read(k)
	tier.Admit(k, make([]byte, 1<<20), false)
	tier.Read(k)
	if admitted, err := tier.Admit(k, make([]byte, 1<<20), false); err != nil || !admitted {
		t.Fatalf("seeding the ghost: %v %v", admitted, err)
	}

	got := newResidencyReporter(tier, back).report(context.Background())
	for _, r := range got {
		if r.Object == "ghost" {
			t.Error("a dataset whose size could not be learned was reported anyway")
		}
	}
	if len(got) != 1 {
		t.Errorf("reported %d datasets, want only the one whose size is known", len(got))
	}
}

// TestAReclaimedDatasetStopsBeingPublished keeps a node from advertising data
// it gave back, which is the failure that makes a stale label worse than none.
func TestAReclaimedDatasetStopsBeingPublished(t *testing.T) {
	const object = "shard"
	tier, back := residencyPieces(t, object, 16<<20)
	hold(t, tier, object, 13)

	r := newResidencyReporter(tier, back)
	if got := r.report(context.Background()); len(got) != 1 {
		t.Fatalf("reported %d datasets before reclamation", len(got))
	}

	// The lease goes, and the blocks with it.
	tier.Revoke("lease")
	if got := r.report(context.Background()); len(got) != 0 {
		t.Errorf("a node still reports %+v after its tier was reclaimed", got)
	}
	if levels := r.levels.Levels(); len(levels) != 0 {
		t.Errorf("the tracker still carries %v", levels)
	}
}

// TestTheReportIsOrdered matters because two reports of one state that differ
// only in order read as two different states.
func TestTheReportIsOrdered(t *testing.T) {
	tier, back := residencyPieces(t, "a", 8<<20)
	hold(t, tier, "a", 7)

	r := newResidencyReporter(tier, back)
	first := r.report(context.Background())
	for i := 0; i < 10; i++ {
		got := r.report(context.Background())
		if len(got) != len(first) {
			t.Fatalf("report %d has %d entries, want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j].Object != first[j].Object || got[j].Tenant != first[j].Tenant {
				t.Fatalf("report %d is ordered differently at %d", i, j)
			}
		}
	}
}

// TestNoTokenFileIsNoLeaseEndpoint covers the default. A lease endpoint that
// granted disk to anything which could reach the port would hand a node's
// capacity to the first thing on the network to ask, so an operator has to
// set a token before one is served at all.
func TestNoTokenFileIsNoLeaseEndpoint(t *testing.T) {
	got, err := leaseToken("")
	if err != nil {
		t.Fatalf("no token file is not an error: %v", err)
	}
	if got != "" {
		t.Errorf("a token appeared from nowhere: %q", got)
	}
}

// TestAnEmptyTokenFileIsRefused matters because it is an operator who meant to
// set one. Treating it as absent would serve the endpoint open, which is the
// opposite of what they asked for.
func TestAnEmptyTokenFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	for _, body := range []string{"", "\n", "   \n\t"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := leaseToken(path); err == nil {
			t.Errorf("a token file holding %q was accepted", body)
		}
	}
}

// TestATokenIsTrimmed covers the newline a file written by an operator or
// mounted from a secret carries, which would otherwise be part of the token
// and would refuse every control plane that sent the token without it.
func TestATokenIsTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := leaseToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("token = %q, want it trimmed", got)
	}
}

// TestAnUnreadableTokenFileStopsTheAgent keeps a node from starting with a
// lease endpoint nobody can reach and nobody notices is unreachable.
func TestAnUnreadableTokenFileStopsTheAgent(t *testing.T) {
	if _, err := leaseToken(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a token file that is not there was accepted")
	}
}

// agentFor builds an agent over a temporary pool for the endpoint tests.
func agentFor(t *testing.T) *agent.Agent {
	t.Helper()
	root := t.TempDir()
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}, pool.Accounting{Capacity: testCapacity}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// askFor makes one request against a served address.
func askFor(t *testing.T, addr, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("asking %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestTheLeaseEndpointIsNotServedWithoutAToken is the default, and it is the
// one that matters: a node whose capacity anything on the network can claim is
// a node anything can fill, and the disk it fills belongs to the workload.
func TestTheLeaseEndpointIsNotServedWithoutAToken(t *testing.T) {
	reg := metrics.New()
	if err := metrics.Node(reg); err != nil {
		t.Fatal(err)
	}
	stop, addr, err := serveMetrics("127.0.0.1:0", reg, nil, nil, agentFor(t), "")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if got := askFor(t, addr, "/capacity", ""); got != http.StatusNotFound {
		t.Errorf("capacity answered %d with no token configured, want 404", got)
	}
	if got := askFor(t, addr, "/metrics", ""); got != http.StatusOK {
		t.Errorf("metrics answered %d, and a scrape needs no token", got)
	}
}

// TestTheLeaseEndpointNeedsItsToken covers the guard once the endpoint exists.
func TestTheLeaseEndpointNeedsItsToken(t *testing.T) {
	const token = "s3cret"
	stop, addr, err := serveMetrics("127.0.0.1:0", metrics.New(), nil, nil, agentFor(t), token)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if got := askFor(t, addr, "/capacity", ""); got != http.StatusUnauthorized {
		t.Errorf("capacity answered %d without a token, want 401", got)
	}
	if got := askFor(t, addr, "/capacity", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("capacity answered %d to the wrong token, want 401", got)
	}
	if got := askFor(t, addr, "/capacity", token); got != http.StatusOK {
		t.Errorf("capacity answered %d to the right token, want 200", got)
	}
}

// TestAQuotaStillLetsTheNodeLendItsOwnTier is the interaction that would
// otherwise stop a node serving the moment an operator set a quota: the tier
// is a lease the agent grants itself, and it belongs to the operator rather
// than to any tenant the quota is meant to bound.
func TestAQuotaStillLetsTheNodeLendItsOwnTier(t *testing.T) {
	dir := t.TempDir()
	backend := filepath.Join(dir, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend, "obj"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := lease.DefaultConfig()
	// A ceiling far below what the tier needs, so a tier counted against it
	// would be refused.
	cfg.Quota = lease.Quota{Borrowed: 1 << 20, Guaranteed: 1 << 20}
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       cfg,
	}, pool.Accounting{Capacity: testCapacity}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	sock := filepath.Join(os.TempDir(), fmt.Sprintf("fb%d", time.Now().UnixNano()))
	defer os.Remove(sock)
	sv, err := serveReads(a, servingOptions{
		Socket: sock, Backend: backendOptions{Dir: backend},
		TierBytes: 8 << 20, BlockBytes: 1 << 20, FirstReads: 8,
	})
	if err != nil {
		t.Fatalf("a node with a quota could not lend itself a tier: %v", err)
	}
	defer sv.stop()

	// And a tenant is still bounded by it.
	err = a.Grant(lease.Lease{
		ID: "red-1", Tenant: "red", Class: lease.Elastic, Size: 4 << 20, Term: time.Hour,
	}, time.Now())
	if !errors.Is(err, lease.ErrTenantQuota) {
		t.Errorf("a tenant over the ceiling gave %v, want the quota refusal", err)
	}
}

// TestAQuotaThatBoundsNothingStopsTheAgent keeps a node from running with a
// limit an operator believes is in force and which can never be reached.
func TestAQuotaThatBoundsNothingStopsTheAgent(t *testing.T) {
	dir := t.TempDir()
	cfg := lease.DefaultConfig()
	// A guaranteed ceiling above the borrowed one bounds nothing.
	cfg.Quota = lease.Quota{Borrowed: 1 << 20, Guaranteed: 4 << 20}

	_, _, err := agent.Open(agent.Config{
		BorrowedDir: filepath.Join(dir, "borrowed"),
		JournalPath: filepath.Join(dir, "state", "leases.json"),
		Lease:       cfg,
	}, pool.Accounting{Capacity: testCapacity}, time.Now())
	if err == nil {
		t.Fatal("a node started with a quota that bounds nothing")
	}
	if !errors.Is(err, lease.ErrBadQuota) {
		t.Errorf("the refusal was %v, want it to name the quota", err)
	}
}
