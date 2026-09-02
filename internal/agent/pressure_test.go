package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

func TestTheShortfallIsTheLargestNeedNotTheSum(t *testing.T) {
	// A pod's declared request shows up in polled free space once it writes,
	// so adding them reclaims cache the node did not need to lose.
	got := Shortfall(
		Pressure{Source: "free space", Need: 4 << 20},
		Pressure{Source: "pod requests", Need: 6 << 20},
		Pressure{Source: "csi", Need: 1 << 20},
	)
	if got.Need != 6<<20 {
		t.Errorf("Need = %s, want the largest observation", got.Need)
	}
	if got.Source != "pod requests" {
		t.Errorf("Source = %q, want the one that drove it", got.Source)
	}
	if none := Shortfall(); none.Need != 0 || none.Source != "none" {
		t.Errorf("Shortfall() = %+v, want no need from no observations", none)
	}
}

func TestAWatchWithoutAHeadroomTargetIsRefused(t *testing.T) {
	// The value has no defensible default, so guessing one would put a number
	// nobody measured in the path that decides when a job loses its cache.
	a, _ := openAgent(t)
	free := func() (pool.Bytes, error) { return 0, nil }
	if err := a.Watch(context.Background(), WatchConfig{Interval: time.Second}, free, noReport); !errors.Is(err, ErrNoHeadroom) {
		t.Errorf("watch with no headroom = %v, want ErrNoHeadroom", err)
	}
	if err := a.Watch(context.Background(), WatchConfig{Headroom: 1 << 20}, free, noReport); err == nil {
		t.Error("watch with no interval was accepted")
	}
}

func TestPressureReclaimsExactlyWhatIsMissing(t *testing.T) {
	a, now := openAgent(t)
	const grant = 8 << 20
	for _, id := range []string{"a", "b"} {
		if err := a.Grant(grantable(id, grant), now); err != nil {
			t.Fatal(err)
		}
	}
	// Headroom is 12MiB and only 4MiB is free, so 8MiB has to come back.
	cfg := WatchConfig{Headroom: 12 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 4 << 20, nil }, now)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Observed.Need != 8<<20 {
		t.Errorf("Need = %s, want the 8MiB gap", tick.Observed.Need)
	}
	if tick.Reclaimed != grant {
		t.Errorf("Reclaimed = %s, want one lease returned", tick.Reclaimed)
	}
	if tick.Shortfall != 0 {
		t.Errorf("Shortfall = %s, want the need met", tick.Shortfall)
	}
	if got := a.Accounting().Borrowed; got != grant {
		t.Errorf("borrowed = %s, want the other lease kept", got)
	}
}

func TestEnoughFreeSpaceReclaimsNothing(t *testing.T) {
	// Reclaiming when nothing needs it is the failure that makes a cache
	// worthless, so the quiet path has to stay quiet.
	a, now := openAgent(t)
	if err := a.Grant(grantable("a", 8<<20), now); err != nil {
		t.Fatal(err)
	}
	cfg := WatchConfig{Headroom: 4 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 32 << 20, nil }, now)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Observed.Need != 0 || tick.Reclaimed != 0 {
		t.Errorf("tick = %+v, want nothing reclaimed", tick)
	}
	if got := a.Accounting().Borrowed; got != 8<<20 {
		t.Errorf("borrowed = %s, want the lease untouched", got)
	}
}

func TestAShortfallTheNodeCannotMeetIsReported(t *testing.T) {
	// The node is then in the state it would have been in with no lending at
	// all, and an operator has to see that rather than infer it.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 32 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 0, nil }, now)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Shortfall == 0 {
		t.Error("a shortfall nothing could satisfy was reported as met")
	}
}

func TestAFailedMeasurementSkipsThePassRatherThanEndingTheWatch(t *testing.T) {
	// Reclaiming against a number the filesystem did not give would be guessing
	// at the moment the node can least afford it, but abandoning the watch
	// leaves the node unwatched exactly then. A failure that persists stops the
	// heartbeat, which is what liveness is for.
	a, _ := openAgent(t)
	before, err := LastProgress(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var passes int
	report := func(_ Tick, err error) {
		if err == nil {
			t.Error("a pass that could not measure free space reported success")
		}
		if passes++; passes == 3 {
			cancel()
		}
	}
	cfg := WatchConfig{Headroom: 1 << 20, Interval: time.Millisecond}
	if err := a.Watch(ctx, cfg, func() (pool.Bytes, error) {
		return 0, fmt.Errorf("device is gone")
	}, report); err != nil {
		t.Errorf("watch = %v, want it to keep polling through failures", err)
	}
	if passes < 3 {
		t.Errorf("the watch made %d passes, want it to continue after a failure", passes)
	}
	// Nothing claimed progress, so liveness can still condemn it.
	after, err := LastProgress(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Error("a failing watch kept its heartbeat fresh, so liveness would never kill it")
	}
}

func TestAWatchWithNowhereToReportIsRefused(t *testing.T) {
	// Every pass is a reclaim decision, and one nobody hears about is the
	// defect this signature exists to prevent.
	a, _ := openAgent(t)
	cfg := WatchConfig{Headroom: 1 << 20, Interval: time.Second}
	if err := a.Watch(context.Background(), cfg, func() (pool.Bytes, error) { return 0, nil }, nil); err == nil {
		t.Error("a watch with no reporter was accepted")
	}
}

func TestTheWatchKeepsTheHeartbeatFresh(t *testing.T) {
	// This is what makes the liveness probe meaningful: before the watch, the
	// binary started, reported and exited, so the heartbeat went stale at once.
	a, now := openAgent(t)
	first, err := LastProgress(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(context.Background(), WatchConfig{Headroom: 1 << 20, Interval: time.Second},
		func() (pool.Bytes, error) { return 32 << 20, nil }, now.Add(time.Minute)); err != nil {
		t.Fatalf("step: %v", err)
	}
	after, err := LastProgress(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.After(first) {
		t.Errorf("heartbeat did not advance: %s then %s", first, after)
	}
}

func TestTheWatchStopsWhenAsked(t *testing.T) {
	a, _ := openAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := WatchConfig{Headroom: 1 << 20, Interval: time.Hour}
	if err := a.Watch(ctx, cfg, func() (pool.Bytes, error) { return 32 << 20, nil }, noReport); err != nil {
		t.Errorf("watch = %v, want a clean stop", err)
	}
}

// noReport is for tests that assert on a refusal before any pass runs.
func noReport(Tick, error) {}

// stubSource is a source under the test's control.
type stubSource struct {
	name  string
	need  pool.Bytes
	err   error
	block chan struct{}
}

func (s stubSource) Name() string { return s.name }

func (s stubSource) Observe(ctx context.Context, _ WatchConfig, _ pool.Bytes) (pool.Bytes, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return s.need, s.err
}

func TestASourceSeesPressureFreeSpaceCannot(t *testing.T) {
	// The reason this seam exists. Free space is above the target, so polling
	// alone reclaims nothing, and the source knows the space is spoken for.
	a, now := openAgent(t)
	if err := a.Grant(lease.Lease{ID: "cache", Class: lease.Elastic, Size: 8 << 20, GrantedAt: now, Term: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	cfg := WatchConfig{Headroom: 4 << 20, Interval: time.Second}
	free := func() (pool.Bytes, error) { return 32 << 20, nil }

	tick, err := a.Step(context.Background(), cfg, free, now)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Reclaimed != 0 {
		t.Fatalf("free space alone reclaimed %s", tick.Reclaimed)
	}

	tick, err = a.Step(context.Background(), cfg, free, now, stubSource{name: "pods", need: 6 << 20})
	if err != nil {
		t.Fatalf("step with a source: %v", err)
	}
	if tick.Observed.Source != "pods" {
		t.Errorf("Observed.Source = %q, want the source that noticed", tick.Observed.Source)
	}
	if tick.Reclaimed == 0 {
		t.Error("the source reported a need and nothing came back")
	}
}

func TestTheLargestNeedWinsAndNeedsAreNeverSummed(t *testing.T) {
	// Two sources seeing the same pressure are not twice the pressure.
	// Summing them would reclaim double what the node needs.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 8 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 8 << 20, nil }, now,
		stubSource{name: "quiet", need: 1 << 20}, stubSource{name: "loud", need: 5 << 20})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Observed.Source != "loud" || tick.Observed.Need != 5<<20 {
		t.Errorf("Observed = %+v, want the largest need alone", tick.Observed)
	}
}

func TestAFailingSourceDoesNotBlindTheOthers(t *testing.T) {
	// The safety property of the design: losing one eye is not a reason to
	// close the other, and the pass has to say which eye it lost.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 8 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 4 << 20, nil }, now,
		stubSource{name: "broken", err: errors.New("kubelet unreachable")})
	if err != nil {
		t.Fatalf("a failing source ended the pass: %v", err)
	}
	if len(tick.Degraded) != 1 || !strings.Contains(tick.Degraded[0], "broken") {
		t.Errorf("Degraded = %v, want the failing source named", tick.Degraded)
	}
	// Free space still saw 4MiB missing, and that still drove the pass.
	if tick.Observed.Source != "free space" || tick.Observed.Need != 4<<20 {
		t.Errorf("Observed = %+v, want free space to have carried the pass", tick.Observed)
	}
}

func TestAPartialSourceIsActedOnAndReported(t *testing.T) {
	// A source that saw most of what it watches has a real floor to offer.
	// Discarding it would lose a shortfall the node actually has.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 8 << 20, Interval: time.Second}
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 8 << 20, nil }, now,
		stubSource{name: "pods", need: 6 << 20, err: &Partial{Err: errors.New("one pod unreadable")}})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if tick.Observed.Source != "pods" || tick.Observed.Need != 6<<20 {
		t.Errorf("Observed = %+v, want the partial source's need used", tick.Observed)
	}
	if len(tick.Degraded) != 1 || !strings.Contains(tick.Degraded[0], "unreadable") {
		t.Errorf("Degraded = %v, want the partial read reported too", tick.Degraded)
	}
}

func TestAHangingSourceDoesNotHoldUpThePass(t *testing.T) {
	// A source that never answers would otherwise delay the reclaim decision
	// for free space, which is the input most likely to still be right when
	// a source is unreachable.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 8 << 20, Interval: 40 * time.Millisecond}
	start := time.Now()
	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 4 << 20, nil }, now,
		stubSource{name: "hung", block: make(chan struct{})})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	// Bounded, not instant: a source gets minObserve even on a brisk
	// interval, and the point is that the wait ends rather than that it is
	// short.
	if elapsed := time.Since(start); elapsed > 3*minObserve {
		t.Errorf("the pass waited %s on a hung source", elapsed)
	}
	if len(tick.Degraded) != 1 {
		t.Errorf("Degraded = %v, want the hung source named", tick.Degraded)
	}
	if tick.Observed.Source != "free space" {
		t.Errorf("Observed.Source = %q, want free space to have carried the pass", tick.Observed.Source)
	}
}

func TestABriskIntervalStillGivesASourceTimeToAnswer(t *testing.T) {
	// Half of a short interval is not enough to read anything over a network.
	// A bound no source can meet would turn every pass degraded while looking
	// like the sources themselves were broken.
	a, now := openAgent(t)
	cfg := WatchConfig{Headroom: 8 << 20, Interval: 20 * time.Millisecond}

	tick, err := a.Step(context.Background(), cfg, func() (pool.Bytes, error) { return 8 << 20, nil }, now,
		slowSource{name: "pods", need: 3 << 20, after: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if len(tick.Degraded) != 0 {
		t.Errorf("Degraded = %v, want a source given time to answer", tick.Degraded)
	}
	if tick.Observed.Source != "pods" {
		t.Errorf("Observed.Source = %q, want the source that answered", tick.Observed.Source)
	}
}

// slowSource answers after a delay, or gives up if the context ends first.
type slowSource struct {
	name  string
	need  pool.Bytes
	after time.Duration
}

func (s slowSource) Name() string { return s.name }

func (s slowSource) Observe(ctx context.Context, _ WatchConfig, _ pool.Bytes) (pool.Bytes, error) {
	select {
	case <-time.After(s.after):
		return s.need, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
