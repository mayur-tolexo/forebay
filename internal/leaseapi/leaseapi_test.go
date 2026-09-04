package leaseapi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

const token = "a-token-an-operator-set"

// node builds an agent over a temporary pool and serves the protocol for it.
func node(t *testing.T, cfg lease.Config, acct pool.Accounting) (*Client, *agent.Agent) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "borrowed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, _, err := agent.Open(agent.Config{
		BorrowedDir: dir,
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       cfg,
	}, acct, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })

	srv := httptest.NewServer(Handler(a, token))
	t.Cleanup(srv.Close)
	return NewClient(strings.TrimPrefix(srv.URL, "http://"), token, 2*time.Second), a
}

func offer(id string, bytes int64) Proposal {
	return Proposal{ID: id, Tenant: "red", Class: "elastic", Bytes: bytes, Term: "1h"}
}

// TestTheNodeDecidesNotTheControlPlane is RFC-0005's load-bearing inversion. A
// proposal is a question, and the node answers it from arithmetic only it can
// do.
func TestTheNodeDecidesNotTheControlPlane(t *testing.T) {
	cfg := lease.DefaultConfig()
	c, a := node(t, cfg, pool.Accounting{Capacity: 8 << 20})
	ctx := context.Background()

	got, err := c.Propose(ctx, offer("a", 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Granted {
		t.Fatalf("a proposal the node had room for was refused: %+v", got)
	}
	if a.Accounting().Borrowed != 4<<20 {
		t.Errorf("the node lent %s, want 4MiB", a.Accounting().Borrowed)
	}

	// More than the node has. The control plane may believe otherwise and it
	// does not matter, which is the whole point.
	got, err = c.Propose(ctx, offer("b", 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got.Granted {
		t.Error("a node granted more capacity than it has")
	}
	if got.Refusal != NoCapacity {
		t.Errorf("refusal = %q, want no-capacity", got.Refusal)
	}
	if got.Reason == "" {
		t.Error("a refusal carried no reason, so an operator cannot tell why")
	}
}

// TestARetriedProposalIsNotAFailure covers the control plane that timed out
// and asked again. Telling it no would make a retry that worked look like a
// failure, and it would abandon a lease the node is holding.
func TestARetriedProposalIsNotAFailure(t *testing.T) {
	c, _ := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20})
	ctx := context.Background()

	first, err := c.Propose(ctx, offer("a", 1<<20))
	if err != nil || !first.Granted {
		t.Fatalf("first proposal: %+v %v", first, err)
	}
	if first.Already {
		t.Error("a first proposal reported the lease as already held")
	}

	again, err := c.Propose(ctx, offer("a", 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Granted {
		t.Errorf("a retried proposal was refused: %+v", again)
	}
	if !again.Already {
		t.Error("a retry was reported as a fresh grant, so a caller cannot tell them apart")
	}
}

// TestARefusalSaysWhichKindItIs matters because a planner does different
// things with each: somewhere else may have room, and waiting helps a node
// that is backing off.
func TestARefusalSaysWhichKindItIs(t *testing.T) {
	cfg := lease.DefaultConfig()
	cfg.Quota = lease.Quota{Borrowed: 2 << 20}
	c, _ := node(t, cfg, pool.Accounting{Capacity: 1 << 30})
	ctx := context.Background()

	if got, _ := c.Propose(ctx, offer("a", 2<<20)); !got.Granted {
		t.Fatalf("the first proposal inside the quota was refused: %+v", got)
	}
	got, err := c.Propose(ctx, offer("b", 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got.Refusal != OverQuota {
		t.Errorf("a tenant over its quota was refused as %q, want over-quota", got.Refusal)
	}

	// A proposal that names no tenant under a quota is the same kind of no.
	unnamed := offer("c", 1<<20)
	unnamed.Tenant = ""
	if got, _ := c.Propose(ctx, unnamed); got.Refusal != OverQuota {
		t.Errorf("an unnamed tenant was refused as %q", got.Refusal)
	}
}

// TestAMalformedProposalIsNotRetryable keeps a planner from asking again for
// something no node will ever accept.
func TestAMalformedProposalIsNotRetryable(t *testing.T) {
	c, _ := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20})
	ctx := context.Background()

	for _, c2 := range []struct {
		name string
		p    Proposal
	}{
		{"no class", Proposal{ID: "a", Tenant: "red", Bytes: 1 << 20, Term: "1h"}},
		{"a class that does not exist", Proposal{ID: "a", Tenant: "red", Class: "cheap", Bytes: 1 << 20, Term: "1h"}},
		{"no term", Proposal{ID: "a", Tenant: "red", Class: "elastic", Bytes: 1 << 20}},
		{"no bytes", Proposal{ID: "a", Tenant: "red", Class: "elastic", Term: "1h"}},
	} {
		got, err := c.Propose(ctx, c2.p)
		if err != nil {
			t.Fatalf("%s: %v", c2.name, err)
		}
		if got.Granted {
			t.Errorf("%s was granted", c2.name)
		}
		if got.Refusal != Malformed {
			t.Errorf("%s was refused as %q, want malformed", c2.name, got.Refusal)
		}
	}
}

// TestCapacityAdmitsHowStaleItIs is RFC-0005's requirement that a control
// plane's view is a cache and says so.
func TestCapacityAdmitsHowStaleItIs(t *testing.T) {
	c, _ := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20, Reserved: 1 << 20})
	ctx := context.Background()

	if got, _ := c.Propose(ctx, offer("a", 2<<20)); !got.Granted {
		t.Fatal("the proposal was refused")
	}
	got, err := c.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != 8<<20 || got.Borrowed != 2<<20 || got.Reserved != 1<<20 {
		t.Errorf("capacity = %+v", got)
	}
	if got.MeasuredAt.IsZero() {
		t.Error("capacity was reported without saying when it was measured")
	}
	if age := got.Age(time.Now().Add(time.Minute)); age < time.Minute {
		t.Errorf("age = %s, want at least the minute that passed", age)
	}
}

// TestReleasingWhatIsNotThereIsDone covers the control plane retrying a
// release: it wanted the lease gone and it is gone, and a failure it can never
// clear would keep it retrying forever.
func TestReleasingWhatIsNotThereIsDone(t *testing.T) {
	c, a := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20})
	ctx := context.Background()

	if got, _ := c.Propose(ctx, offer("a", 2<<20)); !got.Granted {
		t.Fatal("the proposal was refused")
	}
	if err := c.Release(ctx, "a"); err != nil {
		t.Fatalf("releasing a held lease: %v", err)
	}
	if a.Accounting().Borrowed != 0 {
		t.Errorf("%s is still lent after a release", a.Accounting().Borrowed)
	}
	if err := c.Release(ctx, "a"); err != nil {
		t.Errorf("releasing a lease already gone: %v", err)
	}
	if err := c.Release(ctx, "never-existed"); err != nil {
		t.Errorf("releasing a lease that never existed: %v", err)
	}
}

// TestWithoutTheTokenNothingIsGranted is the whole reason the surface is
// guarded: a node whose capacity anyone on the network can claim is a node
// anyone can fill, and the disk they fill belongs to the workload.
func TestWithoutTheTokenNothingIsGranted(t *testing.T) {
	c, a := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20})
	wrong := NewClient(strings.TrimPrefix(c.base, "http://"), "not-the-token", 2*time.Second)
	ctx := context.Background()

	got, err := wrong.Propose(ctx, offer("a", 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got.Granted {
		t.Fatal("a proposal with the wrong token was granted")
	}
	if !strings.Contains(got.Reason, "token") {
		t.Errorf("the refusal does not name the token: %q", got.Reason)
	}
	if a.Accounting().Borrowed != 0 {
		t.Errorf("%s was lent to an unauthorised caller", a.Accounting().Borrowed)
	}
	if _, err := wrong.Capacity(ctx); err == nil {
		t.Error("capacity was disclosed to an unauthorised caller")
	}
	if err := wrong.Release(ctx, "a"); err == nil {
		t.Error("an unauthorised caller could release a lease")
	}
}

// TestANodeThatCannotBeReachedIsAnAnswer keeps a planner from having two paths
// for the two ways a node says no.
func TestANodeThatCannotBeReachedIsAnAnswer(t *testing.T) {
	// TEST-NET-1, reserved for documentation and routing nowhere.
	c := NewClient("192.0.2.1:9099", token, 300*time.Millisecond)

	got, err := c.Propose(context.Background(), offer("a", 1<<20))
	if err != nil {
		t.Fatalf("an unreachable node produced an error rather than an answer: %v", err)
	}
	if got.Granted {
		t.Error("an unreachable node granted a lease")
	}
	if got.Refusal != Unavailable {
		t.Errorf("refusal = %q, want unavailable", got.Refusal)
	}
}

// TestAProposalTooLargeToBeOneIsRefused bounds what a body can cost, since
// this endpoint is reachable by anything that has the token.
func TestAProposalTooLargeToBeOneIsRefused(t *testing.T) {
	c, _ := node(t, lease.DefaultConfig(), pool.Accounting{Capacity: 8 << 20})
	huge := offer("a", 1<<20)
	huge.Tenant = strings.Repeat("x", 128<<10)

	got, err := c.Propose(context.Background(), huge)
	if err != nil {
		t.Fatal(err)
	}
	if got.Granted {
		t.Error("a proposal larger than the bound was granted")
	}
	if got.Refusal != Malformed {
		t.Errorf("refusal = %q, want malformed", got.Refusal)
	}
	// The reason says what to fix. A truncated body fails to parse either way,
	// so what the bound buys is this sentence rather than the refusal itself.
	if !strings.Contains(got.Reason, "larger than") {
		t.Errorf("the refusal does not say the proposal was too large: %q", got.Reason)
	}
}

// TestANodeBackingOffSaysSoRatherThanNoRoom matters because a planner does
// opposite things with the two: it looks elsewhere for no capacity, and it
// waits for a node that is in its post-reclaim cooldown.
func TestANodeBackingOffSaysSoRatherThanNoRoom(t *testing.T) {
	cfg := lease.DefaultConfig()
	cfg.MinTerm = time.Hour
	c, a := node(t, cfg, pool.Accounting{Capacity: 1 << 30})
	ctx := context.Background()

	if got, _ := c.Propose(ctx, offer("a", 1<<20)); !got.Granted {
		t.Fatal("the first proposal was refused")
	}
	// Taking the capacity back is what starts the cooldown, and the cooldown
	// is what the next proposal has to be told about.
	if _, err := a.ReclaimCapacity(1<<20, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := c.Propose(ctx, offer("b", 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got.Granted {
		t.Fatal("a node inside its cooldown granted a lease")
	}
	if got.Refusal != BackingOff {
		t.Errorf("refusal = %q, want backing-off: this node has room and is declining to use it", got.Refusal)
	}
}

// TestAProposalBecomesALease covers the conversion, which is where a wire
// format meets the manager's own types.
func TestAProposalBecomesALease(t *testing.T) {
	got, err := Proposal{ID: "a", Tenant: "red", Class: "guaranteed", Bytes: 1 << 20, Term: "90m"}.Lease()
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != lease.Guaranteed {
		t.Errorf("class = %v", got.Class)
	}
	if got.Term != 90*time.Minute {
		t.Errorf("term = %v", got.Term)
	}
	if got.Tenant != "red" || got.Size != 1<<20 {
		t.Errorf("lease = %+v", got)
	}
	if _, err := (Proposal{ID: "a", Class: "elastic", Bytes: -1, Term: "1h"}).Lease(); err == nil {
		t.Error("a negative size was accepted")
	}
}
