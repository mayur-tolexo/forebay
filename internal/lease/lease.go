// Package lease manages capacity a node has lent revocably, and decides what
// to hand back when the workload on the node needs its disk.
package lease

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Class determines only one thing: how quickly lent capacity can be taken
// back. The ordinal is the reclaim order, cheapest first, so a class added
// later must be inserted at the right cost rather than appended.
type Class int

const (
	// Opportunistic capacity is dropped without warning. Prefetch and
	// speculative fill, where losing it costs one refetch.
	Opportunistic Class = iota
	// Elastic capacity is returned within the configured deadline. Cache and
	// scratch, the ordinary case.
	Elastic
	// Guaranteed capacity is not touched before its term expires. For work
	// that is regenerable in principle but ruinous to drop midway, such as a
	// checkpoint being staged across many ranks.
	Guaranteed
)

// String names the class for logs and errors.
func (c Class) String() string {
	switch c {
	case Opportunistic:
		return "opportunistic"
	case Elastic:
		return "elastic"
	case Guaranteed:
		return "guaranteed"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// Valid reports whether c is a class this package knows how to reclaim.
// Reclaiming an unknown class would mean guessing at its cost, so grants
// carrying one are refused instead.
func (c Class) Valid() bool {
	return c >= Opportunistic && c <= Guaranteed
}

// Lease is capacity lent on one node until its term runs out.
type Lease struct {
	// ID identifies the lease for reclamation and journalling.
	ID string
	// Class fixes how fast this capacity can be taken back.
	Class Class
	// Size is the capacity lent.
	Size pool.Bytes
	// GrantedAt is when the agent accepted the grant, on the agent's clock.
	GrantedAt time.Time
	// Term is a duration rather than an absolute expiry, so a control plane
	// with a skewed clock cannot extend a lease past what the agent agreed to.
	Term time.Duration
}

// ExpiresAt reports when the lease stops being honoured.
func (l Lease) ExpiresAt() time.Time { return l.GrantedAt.Add(l.Term) }

// Expired reports whether the term has run out. Expiry and reclamation
// converge on the same path, so losing the control plane and being asked for
// capacity are handled identically.
func (l Lease) Expired(now time.Time) bool { return !now.Before(l.ExpiresAt()) }

// Errors a grant can be refused with. Each is a refusal rather than a silent
// downgrade, so a caller always learns it did not get what it asked for.
var (
	ErrDuplicate    = errors.New("lease: id already granted")
	ErrBadClass     = errors.New("lease: unknown class")
	ErrBadTerm      = errors.New("lease: term must be positive")
	ErrGuaranteeCap = errors.New("lease: guaranteed capacity would exceed its cap")
	ErrChurning     = errors.New("lease: node is churning, not accepting grants")
	ErrCooldown     = errors.New("lease: node is within its post-reclaim cooldown")
)

// Config tunes how willingly a node lends and how hard it resists churn.
type Config struct {
	// GuaranteedFraction caps guaranteed leases as a share of device
	// capacity. The denominator is the device rather than the borrowed pool,
	// because the borrowed pool shrinks under pressure and a cap that shrank
	// with it would stop bounding anything exactly when it mattered.
	GuaranteedFraction float64
	// MinTerm is how long after a reclamation the node declines new grants,
	// so that a reclaim followed by a grant cannot oscillate.
	MinTerm time.Duration
	// ChurnWindow and ChurnBudget bound reclamations per unit time. Beyond the
	// budget the node reports itself as churning and stops accepting capacity,
	// since chronic churn is usually a scheduling problem rather than a
	// storage one.
	ChurnWindow time.Duration
	ChurnBudget int
}

// DefaultConfig is a starting point, not a tuned one. The values are
// deliberately conservative because none of them has been measured against a
// real workload yet.
func DefaultConfig() Config {
	return Config{
		GuaranteedFraction: 0.25,
		MinTerm:            30 * time.Second,
		ChurnWindow:        10 * time.Minute,
		ChurnBudget:        6,
	}
}

// Manager holds one node's leases and its capacity accounting together, so
// that a grant is only ever accepted if the arithmetic allows it.
//
// Safe for concurrent use. A node agent accepts grants and reacts to capacity
// pressure on different goroutines, so serialising here rather than asking
// every caller to do it removes a race nobody would remember to avoid.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	acct     pool.Accounting
	leases   map[string]Lease
	reclaims []time.Time
}

// New returns a manager for a node with the given starting accounting.
func New(acct pool.Accounting, cfg Config) *Manager {
	return &Manager{cfg: cfg, acct: acct, leases: make(map[string]Lease)}
}

// Accounting reports the node's current capacity split.
func (m *Manager) Accounting() pool.Accounting {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acct
}

// Leases returns the live leases in the order Reclaim would release them:
// cheapest class first, oldest first within a class.
func (m *Manager) Leases() []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leasesLocked()
}

// leasesLocked is Leases without taking the mutex.
func (m *Manager) leasesLocked() []Lease {
	out := make([]Lease, 0, len(m.leases))
	for _, l := range m.leases {
		out = append(out, l)
	}
	sortByReclaimOrder(out)
	return out
}

// Accept records a grant if the node can honour it.
//
// The control plane proposes a grant; this is where the node decides whether
// it is real. Refusing here is what stops two control planes, or one stale
// one, from overcommitting a device that neither of them can see.
func (m *Manager) Accept(l Lease, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)

	if !l.Class.Valid() {
		return fmt.Errorf("%w: %d", ErrBadClass, int(l.Class))
	}
	if l.Term <= 0 {
		return fmt.Errorf("%w: %s", ErrBadTerm, l.Term)
	}
	if _, dup := m.leases[l.ID]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicate, l.ID)
	}
	if m.churning(now) {
		return fmt.Errorf("%w: %d reclaims in %s", ErrChurning, m.cfg.ChurnBudget, m.cfg.ChurnWindow)
	}
	if in, until := m.inCooldown(now); in {
		return fmt.Errorf("%w: %s remaining", ErrCooldown, until)
	}
	if l.Class == Guaranteed {
		limit := pool.Bytes(float64(m.acct.Capacity) * m.cfg.GuaranteedFraction)
		if m.guaranteedTotal()+l.Size > limit {
			return fmt.Errorf("%w: %s granted plus %s requested exceeds %s",
				ErrGuaranteeCap, m.guaranteedTotal(), l.Size, limit)
		}
	}
	if err := m.acct.Lend(l.Size); err != nil {
		return err
	}
	l.GrantedAt = now
	m.leases[l.ID] = l
	return nil
}

// Result reports what a reclamation achieved.
type Result struct {
	// Reclaimed is capacity actually handed back.
	Reclaimed pool.Bytes
	// Shortfall is what was asked for and could not be found. A non-zero
	// shortfall is reported rather than hidden: the node is then in the state
	// it would have been in with no lending at all, and the caller must be
	// able to see that rather than infer it.
	Shortfall pool.Bytes
	// Dropped names the leases released, cheapest first.
	Dropped []string
	// Err is set when the accounting refused to release capacity a lease
	// claimed to hold, which means the agent's books have diverged from what
	// it believes it lent. Reclamation stops at that point rather than
	// continuing over an accounting it can no longer trust.
	Err error
}

// Reclaim hands back at least need bytes if it can, dropping leases in
// ascending order of cost: expired first, then opportunistic, then elastic.
// Guaranteed leases are never released before their term expires.
func (m *Manager) Reclaim(need pool.Bytes, now time.Time) Result {
	m.mu.Lock()
	defer m.mu.Unlock()

	res := Result{}
	if need <= 0 {
		return res
	}

	// Expired leases are released first because they cost nothing at all, and
	// doing it here means the ladder below needs only one ordering rule.
	expired := m.expireLocked(now)
	res.Reclaimed += expired.Reclaimed
	res.Dropped = append(res.Dropped, expired.Dropped...)
	if expired.Err != nil {
		res.Err = expired.Err
		res.Shortfall = need - res.Reclaimed
		return res
	}

	if res.Reclaimed < need {
		for _, l := range m.leasesLocked() {
			if res.Reclaimed >= need {
				break
			}
			// A live guarantee is the one thing the ladder will not take.
			if l.Class == Guaranteed {
				continue
			}
			if err := m.releaseLocked(l); err != nil {
				res.Err = err
				break
			}
			res.Reclaimed += l.Size
			res.Dropped = append(res.Dropped, l.ID)
		}
	}

	if res.Reclaimed < need {
		res.Shortfall = need - res.Reclaimed
	}
	if len(res.Dropped) > 0 {
		m.reclaims = append(m.reclaims, now)
		m.prune(now)
	}
	return res
}

// releaseLocked returns a lease's capacity and then forgets the lease.
//
// The order matters. Forgetting first would leave the bytes counted as
// borrowed with nothing to attribute them to, which is capacity lost until the
// agent restarts, so the record is only dropped once the capacity is back.
func (m *Manager) releaseLocked(l Lease) error {
	if err := m.acct.Return(l.Size); err != nil {
		return fmt.Errorf("lease %s: %w", l.ID, err)
	}
	delete(m.leases, l.ID)
	return nil
}

// Expire releases leases whose term has run out and returns their capacity.
//
// Renewal needs the control plane but expiry does not, so a node cut off from
// the control plane drifts toward giving capacity back to compute, which is
// the safe direction to fail in.
func (m *Manager) Expire(now time.Time) Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.expireLocked(now)
}

// expireLocked is Expire without taking the mutex.
func (m *Manager) expireLocked(now time.Time) Result {
	res := Result{}
	for _, l := range m.leasesLocked() {
		if !l.Expired(now) {
			continue
		}
		if err := m.releaseLocked(l); err != nil {
			res.Err = err
			return res
		}
		res.Reclaimed += l.Size
		res.Dropped = append(res.Dropped, l.ID)
	}
	return res
}

// Churning reports whether the node has reclaimed too often to be worth
// lending to again yet.
func (m *Manager) Churning(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)
	return m.churning(now)
}

// churning counts reclamations inside the churn window.
func (m *Manager) churning(now time.Time) bool {
	if m.cfg.ChurnBudget <= 0 || m.cfg.ChurnWindow <= 0 {
		return false
	}
	cutoff := now.Add(-m.cfg.ChurnWindow)
	n := 0
	for _, t := range m.reclaims {
		if t.After(cutoff) {
			n++
		}
	}
	return n >= m.cfg.ChurnBudget
}

// retention is how long reclamation history has to be kept. Both the churn
// budget and the cooldown read that history, so keeping only the churn window
// would let a short window silently discard the record the cooldown needs.
func (m *Manager) retention() time.Duration {
	if m.cfg.MinTerm > m.cfg.ChurnWindow {
		return m.cfg.MinTerm
	}
	return m.cfg.ChurnWindow
}

// prune drops reclamation history nothing can still need, so that a
// long-running agent does not accumulate one timestamp per reclamation for the
// life of the process.
func (m *Manager) prune(now time.Time) {
	r := m.retention()
	if r <= 0 {
		m.reclaims = m.reclaims[:0]
		return
	}
	cutoff := now.Add(-r)
	kept := m.reclaims[:0]
	for _, t := range m.reclaims {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	m.reclaims = kept
}

// inCooldown reports whether a reclamation is recent enough that lending again
// would risk oscillation, and how long remains.
func (m *Manager) inCooldown(now time.Time) (bool, time.Duration) {
	if m.cfg.MinTerm <= 0 || len(m.reclaims) == 0 {
		return false, 0
	}
	last := m.reclaims[len(m.reclaims)-1]
	if until := last.Add(m.cfg.MinTerm).Sub(now); until > 0 {
		return true, until
	}
	return false, 0
}

// guaranteedTotal sums capacity currently pinned by guaranteed leases.
func (m *Manager) guaranteedTotal() pool.Bytes {
	var total pool.Bytes
	for _, l := range m.leases {
		if l.Class == Guaranteed {
			total += l.Size
		}
	}
	return total
}

// sortByReclaimOrder orders leases cheapest to drop first, oldest first within
// a class, age standing in for coldness.
//
// The identifier breaks remaining ties so the order is total. Without it,
// leases granted in the same instant fall back to the order they came out of
// the map, which Go randomises, and two calls could then disagree about which
// of two identical leases to drop.
func sortByReclaimOrder(ls []Lease) {
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].Class != ls[j].Class {
			return ls[i].Class < ls[j].Class
		}
		if !ls[i].GrantedAt.Equal(ls[j].GrantedAt) {
			return ls[i].GrantedAt.Before(ls[j].GrantedAt)
		}
		return ls[i].ID < ls[j].ID
	})
}
