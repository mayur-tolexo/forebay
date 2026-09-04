package lease

import (
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// autonomyConfig is a node whose cooldown and churn window are easy to reason
// about: eight doublings would pass the window, so the cap is reachable.
func autonomyConfig() Config {
	c := DefaultConfig()
	c.MinTerm = time.Second
	c.ChurnWindow = 10 * time.Second
	c.ChurnBudget = 100
	return c
}

// reclaimedAt records n reclaims at the given instants, which is what the
// cooldown counts.
func reclaimedAt(m *Manager, at ...time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reclaims = append(m.reclaims, at...)
}

// TestTheCooldownGrowsWhileReclaimsKeepHappening is the adaptation itself: how
// long the condition that caused a reclaim takes to pass is a property of the
// workload, so a constant is a guess about somebody else's cluster.
func TestTheCooldownGrowsWhileReclaimsKeepHappening(t *testing.T) {
	m := New(pool.Accounting{Capacity: 1 << 40}, autonomyConfig())
	now := time.Unix(1000, 0)

	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		reclaimedAt(m, now)
		m.mu.Lock()
		got, n := m.cooldownFor(now)
		m.mu.Unlock()
		if got != want {
			t.Errorf("after %d reclaims the cooldown is %s, want %s", i+1, got, want)
		}
		if n != i+1 {
			t.Errorf("counted %d reclaims, want %d", n, i+1)
		}
	}
}

// TestTheCooldownCannotOutgrowTheChurnWindow covers the bound. Past the window
// the churn budget stops the node accepting capacity anyway, so a longer
// cooldown is authority the node does not need.
func TestTheCooldownCannotOutgrowTheChurnWindow(t *testing.T) {
	cfg := autonomyConfig()
	m := New(pool.Accounting{Capacity: 1 << 40}, cfg)
	now := time.Unix(1000, 0)

	for i := 0; i < 20; i++ {
		reclaimedAt(m, now)
	}
	m.mu.Lock()
	got, _ := m.cooldownFor(now)
	m.mu.Unlock()

	if got > cfg.ChurnWindow {
		t.Errorf("cooldown %s exceeds the churn window %s", got, cfg.ChurnWindow)
	}
	if got != cfg.ChurnWindow {
		t.Errorf("cooldown %s, want the window %s after twenty reclaims", got, cfg.ChurnWindow)
	}
}

// TestAQuietWindowReturnsTheCooldownToItsBase covers the decay, which is not a
// separate mechanism: the multiplier counts reclaims inside the churn window,
// so a quiet window removes them.
func TestAQuietWindowReturnsTheCooldownToItsBase(t *testing.T) {
	cfg := autonomyConfig()
	m := New(pool.Accounting{Capacity: 1 << 40}, cfg)
	busy := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		reclaimedAt(m, busy)
	}

	later := busy.Add(cfg.ChurnWindow + time.Second)
	m.mu.Lock()
	got, n := m.cooldownFor(later)
	m.mu.Unlock()

	if got != cfg.MinTerm {
		t.Errorf("cooldown %s a window after the reclaims, want the base %s", got, cfg.MinTerm)
	}
	if n != 0 {
		t.Errorf("counted %d reclaims outside the window", n)
	}
}

// TestDisengagedAutonomyHoldsTheConfiguredValue covers the kill switch. It
// stops discretion, and holding the configured cooldown is what an operator
// reaching for the switch is asking for.
func TestDisengagedAutonomyHoldsTheConfiguredValue(t *testing.T) {
	cfg := autonomyConfig()
	cfg.Autonomy = false
	m := New(pool.Accounting{Capacity: 1 << 40}, cfg)
	now := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		reclaimedAt(m, now)
	}

	m.mu.Lock()
	got, _ := m.cooldownFor(now)
	m.mu.Unlock()
	if got != cfg.MinTerm {
		t.Errorf("cooldown %s with autonomy off, want the configured %s", got, cfg.MinTerm)
	}
}

// TestTheKillSwitchDoesNotStopAPromise is the distinction the whole design
// rests on. Reclamation and expiry are promises rather than discretion, and a
// switch that stopped them would turn off the safety property instead of the
// intelligence.
func TestTheKillSwitchDoesNotStopAPromise(t *testing.T) {
	cfg := autonomyConfig()
	cfg.Autonomy = false
	m := New(pool.Accounting{Capacity: 1 << 40}, cfg)
	now := time.Unix(1000, 0)

	if err := m.Accept(Lease{ID: "a", Class: Elastic, Size: 1 << 30, Term: time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	if got := m.Reclaim(1<<30, now); got.Reclaimed != 1<<30 {
		t.Errorf("reclaimed %s with autonomy off, want the whole lease", got.Reclaimed)
	}

	// Past the cooldown the reclaim itself started, so what is being tested is
	// expiry rather than the refusal.
	after := now.Add(cfg.ChurnWindow + time.Second)
	if err := m.Accept(Lease{ID: "b", Class: Elastic, Size: 1 << 30, Term: time.Minute}, after); err != nil {
		t.Fatal(err)
	}
	if got := m.Expire(after.Add(2 * time.Minute)); got.Reclaimed != 1<<30 {
		t.Errorf("expired %s with autonomy off, want the whole lease", got.Reclaimed)
	}
}

// TestTheRefusalStatesItsArithmetic matters because a refusal is invisible: a
// node briefly backing off and one that has been thrashing for an hour are the
// same silence unless the node says which it is.
func TestTheRefusalStatesItsArithmetic(t *testing.T) {
	m := New(pool.Accounting{Capacity: 1 << 40}, autonomyConfig())
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		reclaimedAt(m, now)
	}

	err := m.Accept(Lease{ID: "a", Class: Elastic, Size: 1 << 20, Term: time.Hour}, now)
	if err == nil {
		t.Fatal("a grant inside the cooldown was accepted")
	}
	for _, want := range []string{"remaining of 4s", "backed off from 1s over 3 reclaims", "in 10s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// TestTheChurnBudgetDoesNotAdapt is the bound autonomy may not move. An engine
// that raised its own churn budget when it started hitting it would be one
// that removes its own limit.
func TestTheChurnBudgetDoesNotAdapt(t *testing.T) {
	cfg := autonomyConfig()
	cfg.ChurnBudget = 3
	m := New(pool.Accounting{Capacity: 1 << 40}, cfg)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		reclaimedAt(m, now)
	}

	// Past the cooldown, so what refuses this is the churn budget alone.
	past := now.Add(cfg.ChurnWindow + time.Second)
	reclaimedAt(m, past)
	for i := 0; i < 2; i++ {
		reclaimedAt(m, past)
	}
	if !m.Churning(past.Add(time.Millisecond)) {
		t.Error("three reclaims against a budget of three was not churning")
	}
}

// TestARefusalThatHasNotBackedOffSaysSo matters because the two refusals mean
// different things to an operator: one node is waiting out its configured
// cooldown after a single reclaim, and the other has been reclaiming
// repeatedly. Reporting the second's words for the first would invent a
// history.
func TestARefusalThatHasNotBackedOffSaysSo(t *testing.T) {
	m := New(pool.Accounting{Capacity: 1 << 40}, autonomyConfig())
	now := time.Unix(1000, 0)
	reclaimedAt(m, now)

	err := m.Accept(Lease{ID: "a", Class: Elastic, Size: 1 << 20, Term: time.Hour}, now)
	if err == nil {
		t.Fatal("a grant inside the cooldown was accepted")
	}
	if !strings.Contains(err.Error(), "has not backed off") {
		t.Errorf("a single reclaim was reported as a backoff: %v", err)
	}
	if strings.Contains(err.Error(), "reclaims") {
		t.Errorf("a refusal that had not backed off quoted a reclaim count: %v", err)
	}
}
