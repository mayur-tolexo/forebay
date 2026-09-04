package lease

import (
	"errors"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// quotaManager builds a node whose capacity is far larger than the quota, so a
// refusal can only come from the quota rather than from the pool arithmetic.
func quotaManager(t *testing.T, q Quota) *Manager {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Quota = q
	return New(pool.Accounting{Capacity: 1 << 40}, cfg)
}

func grantTo(m *Manager, tenant, id string, c Class, size pool.Bytes) error {
	return m.Accept(Lease{ID: id, Tenant: tenant, Class: c, Size: size, Term: time.Hour}, time.Unix(0, 0))
}

// TestOneTenantCannotTakeTheNode is the bound itself: the pool arithmetic only
// stops the node overcommitting, not one tenant holding all of it.
func TestOneTenantCannotTakeTheNode(t *testing.T) {
	m := quotaManager(t, Quota{Borrowed: 10 << 30, Guaranteed: 2 << 30})

	if err := grantTo(m, "red", "a", Elastic, 6<<30); err != nil {
		t.Fatalf("a grant inside the quota was refused: %v", err)
	}
	if err := grantTo(m, "red", "b", Elastic, 6<<30); !errors.Is(err, ErrTenantQuota) {
		t.Errorf("a grant taking red to 12GiB against a 10GiB ceiling gave %v", err)
	}
	// Another tenant is unaffected, since the ceiling is per tenant.
	if err := grantTo(m, "blue", "c", Elastic, 6<<30); err != nil {
		t.Errorf("blue was refused for red's holdings: %v", err)
	}
}

// TestTheGuaranteedCeilingIsScarcer covers the limit that matters: guaranteed
// capacity denies itself to everyone else, so a tenant well inside its
// borrowed ceiling must still be stopped from taking the staging share.
func TestTheGuaranteedCeilingIsScarcer(t *testing.T) {
	m := quotaManager(t, Quota{Borrowed: 100 << 30, Guaranteed: 4 << 30})

	if err := grantTo(m, "red", "a", Guaranteed, 3<<30); err != nil {
		t.Fatalf("staging inside the ceiling was refused: %v", err)
	}
	err := grantTo(m, "red", "b", Guaranteed, 3<<30)
	if !errors.Is(err, ErrTenantQuota) {
		t.Errorf("6GiB of staging against a 4GiB ceiling gave %v", err)
	}
	// The same tenant is still far inside its borrowed ceiling, so an elastic
	// lease of the same size must be accepted.
	if err := grantTo(m, "red", "c", Elastic, 3<<30); err != nil {
		t.Errorf("an elastic lease was refused by the guaranteed ceiling: %v", err)
	}
}

// TestAnUnnamedTenantIsRefusedUnderAQuota closes the way around the bound: a
// caller that omits the name would otherwise be unlimited, and every unnamed
// lease would be counted against the same empty tenant.
func TestAnUnnamedTenantIsRefusedUnderAQuota(t *testing.T) {
	m := quotaManager(t, Quota{Borrowed: 10 << 30})
	if err := grantTo(m, "", "a", Elastic, 1<<30); !errors.Is(err, ErrNoTenant) {
		t.Errorf("an unnamed lease under a quota gave %v", err)
	}

	// With no quota the name is not needed, since nothing is counted.
	free := New(pool.Accounting{Capacity: 1 << 40}, DefaultConfig())
	if err := grantTo(free, "", "a", Elastic, 1<<30); err != nil {
		t.Errorf("an unnamed lease on a node with no quota was refused: %v", err)
	}
}

// TestReleasedCapacityStopsCounting matters because a quota that only ever
// grew would stop a tenant that had given everything back.
func TestReleasedCapacityStopsCounting(t *testing.T) {
	m := quotaManager(t, Quota{Borrowed: 10 << 30})
	if err := grantTo(m, "red", "a", Elastic, 8<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release("a", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := grantTo(m, "red", "b", Elastic, 8<<30); err != nil {
		t.Errorf("a tenant that gave its capacity back was still counted for it: %v", err)
	}
}

// TestAQuotaThatBoundsNothingIsRefused covers the misconfiguration that would
// otherwise read as a limit in force: a guaranteed ceiling above the borrowed
// one can never be reached.
func TestAQuotaThatBoundsNothingIsRefused(t *testing.T) {
	if err := (Quota{Borrowed: 1 << 30, Guaranteed: 4 << 30}).Validate(); !errors.Is(err, ErrBadQuota) {
		t.Errorf("a guaranteed ceiling above the borrowed one gave %v", err)
	}
	for _, q := range []Quota{
		{},
		{Borrowed: 4 << 30, Guaranteed: 1 << 30},
		{Guaranteed: 1 << 30},
	} {
		if err := q.Validate(); err != nil {
			t.Errorf("%+v was refused: %v", q, err)
		}
	}
}

// TestTheTenantSurvivesARestart is what stops a tenant exceeding its quota by
// waiting for one: a node that replayed its journal without the tenant would
// count every restored lease against nobody.
func TestTheTenantSurvivesARestart(t *testing.T) {
	j := NewFileJournal(t.TempDir() + "/leases.json")
	cfg := DefaultConfig()
	cfg.Quota = Quota{Borrowed: 10 << 30}

	first := New(pool.Accounting{Capacity: 1 << 40}, cfg, WithJournal(j))
	if _, err := first.Restore(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := grantTo(first, "red", "a", Elastic, 8<<30); err != nil {
		t.Fatal(err)
	}

	second := New(pool.Accounting{Capacity: 1 << 40}, cfg, WithJournal(j))
	if _, err := second.Restore(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := grantTo(second, "red", "b", Elastic, 8<<30); !errors.Is(err, ErrTenantQuota) {
		t.Errorf("red held 8GiB across the restart and was granted 8GiB more: %v", err)
	}
}

// TestTheNodesOwnTierIsNotATenant covers the grant that would otherwise break
// the moment a quota was configured: the agent lends the fast tier its
// capacity itself, and that lease belongs to the operator rather than to
// anyone the quota is meant to bound.
func TestTheNodesOwnTierIsNotATenant(t *testing.T) {
	m := quotaManager(t, Quota{Borrowed: 1 << 30})
	if err := grantTo(m, NodeTenant, "tier", Elastic, 100<<30); err != nil {
		t.Fatalf("the node could not lend its own tier under a quota: %v", err)
	}
	// A real tenant is still bounded, and is not credited with the tier.
	if err := grantTo(m, "red", "a", Elastic, 2<<30); !errors.Is(err, ErrTenantQuota) {
		t.Errorf("a tenant over its ceiling gave %v", err)
	}
}
