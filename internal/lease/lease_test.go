package lease

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

var t0 = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// node returns a manager for an 8 TiB device with 1 TiB compute and 2 TiB
// donated, leaving 5 TiB lendable.
func node(cfg Config) *Manager {
	return New(pool.Accounting{
		Capacity: 8 * pool.TiB,
		Compute:  1 * pool.TiB,
		Donated:  2 * pool.TiB,
	}, cfg)
}

// relaxed disables churn and cooldown so a test can exercise one thing.
func relaxed() Config {
	return Config{ReclaimWithin: 30 * time.Second, GuaranteedFraction: 0.25}
}

func grant(id string, c Class, size pool.Bytes) Lease {
	return Lease{ID: id, Class: c, Size: size, Term: time.Hour}
}

func TestClassStringAndValid(t *testing.T) {
	for _, tc := range []struct {
		c     Class
		want  string
		valid bool
	}{
		{Opportunistic, "opportunistic", true},
		{Elastic, "elastic", true},
		{Guaranteed, "guaranteed", true},
		{Class(9), "class(9)", false},
	} {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Class(%d).String() = %q, want %q", int(tc.c), got, tc.want)
		}
		if got := tc.c.Valid(); got != tc.valid {
			t.Errorf("Class(%d).Valid() = %v, want %v", int(tc.c), got, tc.valid)
		}
	}
}

func TestAcceptRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Lease
		want error
	}{
		{"unknown class", Lease{ID: "a", Class: Class(7), Size: 1 * pool.GiB, Term: time.Hour}, ErrBadClass},
		{"zero term", Lease{ID: "a", Class: Elastic, Size: 1 * pool.GiB}, ErrBadTerm},
		{"beyond capacity", grant("a", Elastic, 6*pool.TiB), pool.ErrInsufficient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := node(relaxed())
			if err := m.Accept(tc.l, t0); !errors.Is(err, tc.want) {
				t.Fatalf("Accept = %v, want %v", err, tc.want)
			}
			if got := m.Accounting().Borrowed; got != 0 {
				t.Fatalf("refused grant lent %s", got)
			}
		})
	}
}

func TestAcceptRejectsDuplicateID(t *testing.T) {
	m := node(relaxed())
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("first Accept = %v", err)
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate Accept = %v, want ErrDuplicate", err)
	}
	if got := m.Accounting().Borrowed; got != 1*pool.TiB {
		t.Fatalf("Borrowed = %s, want 1.00TiB", got)
	}
}

func TestGuaranteedIsCappedAgainstDeviceCapacity(t *testing.T) {
	// 25% of an 8 TiB device is 2 TiB, whatever the borrowed pool is doing.
	m := node(relaxed())
	if err := m.Accept(grant("g1", Guaranteed, 2*pool.TiB), t0); err != nil {
		t.Fatalf("Accept at the cap = %v, want nil", err)
	}
	if err := m.Accept(grant("g2", Guaranteed, 1*pool.GiB), t0); !errors.Is(err, ErrGuaranteeCap) {
		t.Fatalf("Accept past the cap = %v, want ErrGuaranteeCap", err)
	}
	// Capacity remains lendable in a cheaper class.
	if err := m.Accept(grant("e1", Elastic, 2*pool.TiB), t0); err != nil {
		t.Fatalf("elastic grant after guaranteed cap = %v, want nil", err)
	}
}

func TestReclaimLadderDropsCheapestFirst(t *testing.T) {
	m := node(relaxed())
	for _, l := range []Lease{
		grant("elastic-old", Elastic, 1*pool.TiB),
		grant("guaranteed", Guaranteed, 1*pool.TiB),
		grant("opportunistic", Opportunistic, 1*pool.TiB),
	} {
		if err := m.Accept(l, t0); err != nil {
			t.Fatalf("Accept(%s) = %v", l.ID, err)
		}
	}

	res := m.Reclaim(2*pool.TiB, t0.Add(time.Minute))
	if res.Shortfall != 0 {
		t.Fatalf("Shortfall = %s, want 0", res.Shortfall)
	}
	want := []string{"opportunistic", "elastic-old"}
	if len(res.Dropped) != len(want) {
		t.Fatalf("Dropped = %v, want %v", res.Dropped, want)
	}
	for i, id := range want {
		if res.Dropped[i] != id {
			t.Errorf("Dropped[%d] = %s, want %s", i, res.Dropped[i], id)
		}
	}
	// The guaranteed lease is the one thing that must survive.
	if got := len(m.Leases()); got != 1 || m.Leases()[0].ID != "guaranteed" {
		t.Fatalf("remaining leases = %v, want only the guaranteed one", m.Leases())
	}
}

func TestReclaimNeverTouchesLiveGuaranteedAndReportsShortfall(t *testing.T) {
	m := node(relaxed())
	if err := m.Accept(grant("g", Guaranteed, 2*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}

	// Asking for more than every reclaimable byte must report the gap rather
	// than break the guarantee, leaving the node exactly as it would have been
	// with no lending at all.
	res := m.Reclaim(2*pool.TiB, t0.Add(time.Minute))
	if res.Reclaimed != 0 {
		t.Fatalf("Reclaimed = %s, want 0", res.Reclaimed)
	}
	if res.Shortfall != 2*pool.TiB {
		t.Fatalf("Shortfall = %s, want 2.00TiB", res.Shortfall)
	}
	if got := m.Accounting().Borrowed; got != 2*pool.TiB {
		t.Fatalf("Borrowed = %s, want the guarantee still held", got)
	}
}

func TestReclaimTakesExpiredGuaranteedCapacity(t *testing.T) {
	m := node(relaxed())
	l := grant("g", Guaranteed, 2*pool.TiB)
	l.Term = time.Minute
	if err := m.Accept(l, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	res := m.Reclaim(1*pool.TiB, t0.Add(2*time.Minute))
	if res.Shortfall != 0 || res.Reclaimed != 2*pool.TiB {
		t.Fatalf("Reclaimed %s shortfall %s, want the expired guarantee released",
			res.Reclaimed, res.Shortfall)
	}
}

func TestReclaimZeroIsANoOp(t *testing.T) {
	m := node(relaxed())
	if err := m.Accept(grant("e", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	if res := m.Reclaim(0, t0); res.Reclaimed != 0 || len(res.Dropped) != 0 {
		t.Fatalf("Reclaim(0) = %+v, want no-op", res)
	}
}

func TestExpireReturnsCapacityWithoutTheControlPlane(t *testing.T) {
	m := node(relaxed())
	short := grant("short", Elastic, 1*pool.TiB)
	short.Term = time.Minute
	if err := m.Accept(short, t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	if err := m.Accept(grant("long", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}

	res := m.Expire(t0.Add(2 * time.Minute))
	if res.Reclaimed != 1*pool.TiB || len(res.Dropped) != 1 || res.Dropped[0] != "short" {
		t.Fatalf("Expire = %+v, want only the expired lease released", res)
	}
	if got := m.Accounting().Borrowed; got != 1*pool.TiB {
		t.Fatalf("Borrowed = %s, want 1.00TiB", got)
	}
}

func TestCooldownBlocksImmediateRegrant(t *testing.T) {
	cfg := relaxed()
	cfg.MinTerm = 30 * time.Second
	m := node(cfg)
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	m.Reclaim(1*pool.TiB, t0)

	if err := m.Accept(grant("b", Elastic, 1*pool.TiB), t0.Add(time.Second)); !errors.Is(err, ErrCooldown) {
		t.Fatalf("Accept during cooldown = %v, want ErrCooldown", err)
	}
	if err := m.Accept(grant("b", Elastic, 1*pool.TiB), t0.Add(time.Minute)); err != nil {
		t.Fatalf("Accept after cooldown = %v, want nil", err)
	}
}

func TestChurningNodeStopsAcceptingCapacity(t *testing.T) {
	cfg := Config{ReclaimWithin: 30 * time.Second, GuaranteedFraction: 0.25, ChurnWindow: time.Hour, ChurnBudget: 2}
	m := node(cfg)
	now := t0
	for i, id := range []string{"a", "b"} {
		if err := m.Accept(grant(id, Elastic, 1*pool.TiB), now); err != nil {
			t.Fatalf("Accept(%s) = %v", id, err)
		}
		now = now.Add(time.Duration(i+1) * time.Minute)
		m.Reclaim(1*pool.TiB, now)
	}

	if !m.Churning(now) {
		t.Fatal("Churning = false, want true after reaching the budget")
	}
	if err := m.Accept(grant("c", Elastic, 1*pool.TiB), now); !errors.Is(err, ErrChurning) {
		t.Fatalf("Accept while churning = %v, want ErrChurning", err)
	}
	// Once the window has passed the node is lendable again.
	later := now.Add(2 * time.Hour)
	if m.Churning(later) {
		t.Fatal("Churning = true after the window passed, want false")
	}
	if err := m.Accept(grant("c", Elastic, 1*pool.TiB), later); err != nil {
		t.Fatalf("Accept after the window = %v, want nil", err)
	}
}

func TestLeasesAreOrderedByReclaimCost(t *testing.T) {
	m := node(relaxed())
	for _, l := range []Lease{
		grant("g", Guaranteed, 1*pool.GiB),
		grant("e", Elastic, 1*pool.GiB),
		grant("o", Opportunistic, 1*pool.GiB),
	} {
		if err := m.Accept(l, t0); err != nil {
			t.Fatalf("Accept(%s) = %v", l.ID, err)
		}
	}
	want := []string{"o", "e", "g"}
	got := m.Leases()
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("Leases() = %v, want order %v", got, want)
		}
	}
}

func TestExpiresAtUsesTermNotAbsoluteTime(t *testing.T) {
	// Grants carry a duration so a control plane with a skewed clock cannot
	// extend a lease past what the node agreed to.
	l := Lease{GrantedAt: t0, Term: 90 * time.Second}
	if got := l.ExpiresAt(); !got.Equal(t0.Add(90 * time.Second)) {
		t.Errorf("ExpiresAt() = %v, want %v", got, t0.Add(90*time.Second))
	}
	if l.Expired(t0.Add(89 * time.Second)) {
		t.Error("Expired before the term ran out")
	}
	if !l.Expired(t0.Add(90 * time.Second)) {
		t.Error("not Expired at the term boundary")
	}
}

func TestDefaultConfigIsUsable(t *testing.T) {
	m := node(DefaultConfig())
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept with DefaultConfig = %v", err)
	}
}

func TestReclaimKeepsTheRecordWhenAccountingRefuses(t *testing.T) {
	// Forcing the divergence the ordering fix exists to survive: the manager
	// believes it lent more than the accounting says is borrowed.
	m := node(relaxed())
	if err := m.Accept(grant("a", Elastic, 2*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	m.acct.Borrowed = 0 // the books now disagree with the lease

	res := m.Reclaim(1*pool.TiB, t0.Add(time.Minute))
	if res.Err == nil {
		t.Fatal("Reclaim swallowed an accounting failure, want it reported")
	}
	if !errors.Is(res.Err, pool.ErrOverRelease) {
		t.Errorf("Reclaim Err = %v, want ErrOverRelease", res.Err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("Dropped = %v, want nothing dropped", res.Dropped)
	}
	// The lease must survive, or its bytes are lost with nothing to attribute
	// them to and the divergence becomes permanent.
	if got := len(m.Leases()); got != 1 {
		t.Errorf("lease count = %d, want the record kept", got)
	}
	if res.Shortfall != 1*pool.TiB {
		t.Errorf("Shortfall = %s, want the full request", res.Shortfall)
	}
}

func TestCooldownSurvivesAShortChurnWindow(t *testing.T) {
	// A churn window shorter than the cooldown must not discard the history
	// the cooldown depends on.
	cfg := Config{ReclaimWithin: 30 * time.Second, GuaranteedFraction: 0.25, MinTerm: time.Minute,
		ChurnWindow: 5 * time.Second, ChurnBudget: 10}
	m := node(cfg)
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	m.Reclaim(1*pool.TiB, t0)

	// Past the churn window, still inside the cooldown.
	at := t0.Add(10 * time.Second)
	if err := m.Accept(grant("b", Elastic, 1*pool.TiB), at); !errors.Is(err, ErrCooldown) {
		t.Fatalf("Accept = %v, want ErrCooldown", err)
	}
}

func TestReclamationHistoryDoesNotGrowWithoutBound(t *testing.T) {
	// Churn disabled must still prune, or a long-lived agent accumulates one
	// timestamp per reclamation forever.
	cfg := Config{ReclaimWithin: 30 * time.Second, GuaranteedFraction: 0.25, MinTerm: time.Second}
	m := node(cfg)
	now := t0
	for i := 0; i < 200; i++ {
		if err := m.Accept(grant("x", Elastic, 1*pool.GiB), now); err != nil {
			t.Fatalf("Accept at %d = %v", i, err)
		}
		now = now.Add(time.Minute)
		m.Reclaim(1*pool.GiB, now)
		now = now.Add(time.Minute) // clear the cooldown before granting again
	}
	if got := len(m.reclaims); got > 2 {
		t.Errorf("reclamation history holds %d entries, want it pruned", got)
	}
}

func TestManagerIsSafeForConcurrentUse(t *testing.T) {
	// The agent accepts grants and reacts to pressure on separate goroutines,
	// so this asserts under -race what the doc comment promises.
	m := node(relaxed())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("lease-%d", i)
			for n := 0; n < 50; n++ {
				_ = m.Accept(grant(id, Elastic, 1*pool.GiB), t0)
				m.Reclaim(1*pool.GiB, t0)
				_ = m.Leases()
				_ = m.Accounting()
				m.Churning(t0)
				m.Expire(t0.Add(2 * time.Hour))
			}
		}(i)
	}
	wg.Wait()
	if err := m.Accounting().Validate(); err != nil {
		t.Fatalf("accounting diverged under concurrency: %v", err)
	}
}

func TestLeasesOrderMatchesReclaimOrder(t *testing.T) {
	// The doc comment on Leases claims this, so it is asserted rather than
	// trusted.
	m := node(relaxed())
	for _, l := range []Lease{
		grant("e2", Elastic, 1*pool.GiB),
		grant("o", Opportunistic, 1*pool.GiB),
		grant("e1", Elastic, 1*pool.GiB),
	} {
		if err := m.Accept(l, t0); err != nil {
			t.Fatalf("Accept(%s) = %v", l.ID, err)
		}
	}
	listed := m.Leases()
	res := m.Reclaim(3*pool.GiB, t0)
	for i, l := range listed {
		if res.Dropped[i] != l.ID {
			t.Fatalf("Leases() order %v does not match Dropped %v", listed, res.Dropped)
		}
	}
}

func TestReclaimOrderIsDeterministicForIdenticalLeases(t *testing.T) {
	// Leases granted in the same instant used to fall back to map iteration
	// order, so two calls could disagree about which to drop first.
	order := func() []string {
		m := node(relaxed())
		for _, id := range []string{"e3", "e1", "e2"} {
			if err := m.Accept(grant(id, Elastic, 1*pool.GiB), t0); err != nil {
				t.Fatalf("Accept(%s) = %v", id, err)
			}
		}
		var ids []string
		for _, l := range m.Leases() {
			ids = append(ids, l.ID)
		}
		return ids
	}
	first := order()
	for i := 0; i < 50; i++ {
		got := order()
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("order varies between runs: %v then %v", first, got)
			}
		}
	}
	if first[0] != "e1" || first[2] != "e3" {
		t.Errorf("tie broken as %v, want identifier order", first)
	}
}

func TestReclaimDeadlinePerClass(t *testing.T) {
	// The deadline is the promise the elastic class exists to make, so the
	// classes have to differ on it in a way a caller can act on.
	cfg := Config{ReclaimWithin: 30 * time.Second}

	if d, bounded := Opportunistic.ReclaimDeadline(cfg); !bounded || d != 0 {
		t.Errorf("Opportunistic = %v, %v; want 0 and bounded", d, bounded)
	}
	if d, bounded := Elastic.ReclaimDeadline(cfg); !bounded || d != 30*time.Second {
		t.Errorf("Elastic = %v, %v; want 30s and bounded", d, bounded)
	}
	// Guaranteed capacity is not reclaimed early at any speed, so a deadline
	// would be a promise the class does not make.
	if _, bounded := Guaranteed.ReclaimDeadline(cfg); bounded {
		t.Error("Guaranteed reported a reclaim deadline, want none")
	}
}

func TestDefaultConfigStatesTheReclaimContract(t *testing.T) {
	if got := DefaultConfig().ReclaimWithin; got != 30*time.Second {
		t.Errorf("DefaultConfig().ReclaimWithin = %v, want 30s", got)
	}
}

func TestElasticGrantIsRefusedWithoutADeadline(t *testing.T) {
	// A config that never set ReclaimWithin would make elastic capacity
	// reclaimable immediately, which is opportunistic wearing the wrong name.
	// Refusing is louder than silently downgrading the promise.
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, Config{GuaranteedFraction: 0.25})

	if err := m.Accept(grant("e", Elastic, 1*pool.TiB), t0); !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("elastic Accept without a deadline = %v, want ErrNoDeadline", err)
	}
	if got := m.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s after a refused grant, want 0", got)
	}
	// The other two classes do not depend on that deadline and still work.
	if err := m.Accept(grant("o", Opportunistic, 1*pool.TiB), t0); err != nil {
		t.Errorf("opportunistic Accept = %v, want nil", err)
	}
	if err := m.Accept(grant("g", Guaranteed, 1*pool.TiB), t0); err != nil {
		t.Errorf("guaranteed Accept = %v, want nil", err)
	}
}

func TestOnlyTakingALiveElasticLeaseEngagesTheDeadline(t *testing.T) {
	// Bounded is what tells the agent whether a deadline applies at all, so
	// the classes that promise nothing must not set it. Opportunistic capacity
	// is taken without warning, and a lease whose term ran out was never asked
	// for: neither engaged a promise, and timing them against one would report
	// a deadline as kept that was never made.
	for _, c := range []struct {
		name  string
		class Class
		want  bool
	}{
		{"elastic promises a deadline", Elastic, true},
		{"opportunistic promises nothing", Opportunistic, false},
	} {
		m := New(pool.Accounting{Capacity: 8 * pool.TiB}, DefaultConfig())
		l := Lease{ID: "a", Class: c.class, Size: 1 * pool.TiB, Term: time.Hour}
		if err := m.Accept(l, t0); err != nil {
			t.Fatalf("%s: accepting: %v", c.name, err)
		}
		if got := m.Reclaim(1*pool.TiB, t0).Bounded; got != c.want {
			t.Errorf("%s: Bounded = %v, want %v", c.name, got, c.want)
		}
	}

	// An expired elastic lease is released by the same call, and must not.
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, DefaultConfig())
	if err := m.Accept(Lease{ID: "a", Class: Elastic, Size: 1 * pool.TiB, Term: time.Minute}, t0); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	res := m.Reclaim(1*pool.TiB, t0.Add(time.Hour))
	if len(res.Dropped) != 1 {
		t.Fatalf("Dropped = %v, want the expired lease released", res.Dropped)
	}
	if res.Bounded {
		t.Error("an expired lease engaged the reclaim deadline, but its term simply ran out")
	}
}
