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

// ParseClass turns a class name back into a Class, for reading state that was
// written as text.
func ParseClass(s string) (Class, error) {
	switch s {
	case "opportunistic":
		return Opportunistic, nil
	case "elastic":
		return Elastic, nil
	case "guaranteed":
		return Guaranteed, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrBadClass, s)
	}
}

// ReclaimDeadline reports how long this class may take to hand capacity back,
// and whether any deadline applies. Guaranteed capacity has none, because its
// term is the promise: it is not reclaimed early at any speed.
//
// A zero deadline means different things per class and the difference matters.
// For opportunistic capacity it means immediately. For elastic it means the
// config never set one, which Accept refuses rather than treat as immediate,
// since silently collapsing elastic into opportunistic would hand back
// capacity the caller expected to keep for a bounded time.
//
// This states the contract. Enforcing it end to end also requires invalidating
// readers, which lives in the data path rather than here, so a caller that
// holds extents is responsible for finishing inside the returned budget.
func (c Class) ReclaimDeadline(cfg Config) (time.Duration, bool) {
	switch c {
	case Opportunistic:
		return 0, true
	case Elastic:
		return cfg.ReclaimWithin, true
	default:
		return 0, false
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
	// ErrNotRestored is a grant arriving before the journal has been replayed.
	// Accepting then would count capacity twice once the replay caught up, so
	// a manager with a journal lends nothing until it knows what it already
	// lent.
	ErrNotRestored = errors.New("lease: journal has not been replayed yet")
	// ErrNoDeadline is an elastic grant under a config that never set
	// ReclaimWithin. Accepting it would make elastic capacity reclaimable
	// immediately, which is the opportunistic class wearing the wrong name.
	ErrNoDeadline = errors.New("lease: elastic leases need a reclaim deadline")
	// ErrNoSuchLease is a release naming a lease this manager does not hold.
	ErrNoSuchLease = errors.New("lease: no such lease")
)

// Config tunes how willingly a node lends and how hard it resists churn.
type Config struct {
	// ReclaimWithin is how long an elastic lease may take to return its
	// capacity once it has been asked for. It is the promise the elastic class
	// exists to make, and it bounds the data path rather than this package:
	// dropping a lease here is a map operation, while the time is spent
	// invalidating readers and unlinking extents.
	ReclaimWithin time.Duration
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
		ReclaimWithin:      30 * time.Second,
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
	journal  Journal
	// restored records whether the journal has been replayed. A manager with
	// no journal has nothing to replay and starts ready.
	restored bool
}

// Option configures a Manager at construction.
type Option func(*Manager)

// WithJournal persists lease state, so that an agent restart does not forget
// what it lent. Without one a Manager keeps its leases in memory only, which
// is useful in tests and wrong on a real node.
func WithJournal(j Journal) Option {
	return func(m *Manager) {
		m.journal = j
		m.restored = false
	}
}

// New returns a manager for a node with the given starting accounting.
func New(acct pool.Accounting, cfg Config, opts ...Option) *Manager {
	m := &Manager{cfg: cfg, acct: acct, leases: make(map[string]Lease), restored: true}
	for _, o := range opts {
		o(m)
	}
	return m
}

// persistLocked writes current state to the journal, if there is one.
func (m *Manager) persistLocked() error {
	if m.journal == nil {
		return nil
	}
	return m.journal.Save(m.leasesLocked())
}

// Restored reports whether the manager knows what it already lent, and so
// whether it will accept grants.
func (m *Manager) Restored() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restored
}

// Restore rebuilds lease state from the journal and reconciles it with the
// accounting, returning what it had to drop.
//
// Until this has succeeded a manager with a journal refuses grants, because
// accepting one before the replay would count that capacity twice as soon as
// the replay caught up. Reclamation is never gated on it: handing capacity
// back to compute is safe from any state, and making compute wait on a replay
// would invert the one rule the whole design rests on.
//
// A journal it cannot read is not fatal. Everything recorded there is
// regenerable, so the manager starts empty and the error is returned for the
// caller to report rather than to recover from. Leases the accounting can no
// longer fit are dropped for the same reason: the node's shape may have
// changed while it was down, and compute keeps whatever it now needs.
func (m *Manager) Restore(now time.Time) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	res := Result{}
	if m.journal == nil {
		return res, nil
	}

	loaded, err := m.journal.Load()
	if err != nil {
		m.leases = make(map[string]Lease)
		m.acct.Borrowed = 0
		if saveErr := m.persistLocked(); saveErr != nil {
			// The reset could not be recorded, so the next restart would read
			// the same unreadable file. Staying unrestored keeps the node from
			// lending against a state it cannot persist.
			return res, fmt.Errorf("%w (and could not reset it: %v)", err, saveErr)
		}
		m.restored = true
		return res, err
	}

	m.leases = make(map[string]Lease, len(loaded))
	m.acct.Borrowed = 0
	for _, l := range loaded {
		if l.Expired(now) {
			res.Dropped = append(res.Dropped, l.ID)
			continue
		}
		if lendErr := m.acct.Lend(l.Size); lendErr != nil {
			res.Unfittable = append(res.Unfittable, l.ID)
			continue
		}
		m.leases[l.ID] = l
	}
	if len(res.Dropped) > 0 || len(res.Unfittable) > 0 {
		if err := m.persistLocked(); err != nil {
			return res, fmt.Errorf("lease: restored state could not be journalled: %w", err)
		}
	}
	m.restored = true
	return res, nil
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

	if !m.restored {
		return ErrNotRestored
	}
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
	// An elastic lease promises capacity back within a bounded time. A config
	// that never set one would quietly make that promise meaningless.
	if l.Class == Elastic {
		if d, _ := l.Class.ReclaimDeadline(m.cfg); d <= 0 {
			return fmt.Errorf("%w: ReclaimWithin is %s", ErrNoDeadline, m.cfg.ReclaimWithin)
		}
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

	// Journalled before it is honoured. If the record cannot be written the
	// grant is undone, because capacity lent with no record of the lending is
	// capacity that leaks the moment the agent restarts.
	if err := m.persistLocked(); err != nil {
		if rollbackErr := m.releaseLocked(l); rollbackErr != nil {
			return fmt.Errorf("%w (and rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Release drops one lease by identifier and returns the capacity it held.
//
// Reclaim chooses what to drop by cost, which is right when the goal is to
// free a quantity. This is for when a specific lease is known to be wrong, such
// as one whose extent has gone missing, where dropping the cheapest lease
// instead would be both useless and destructive.
func (m *Manager) Release(id string, now time.Time) (pool.Bytes, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.leases[id]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNoSuchLease, id)
	}
	if err := m.releaseLocked(l); err != nil {
		return 0, err
	}
	if err := m.persistLocked(); err != nil {
		return l.Size, err
	}
	return l.Size, nil
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
	// Unfittable names leases that were valid but that the accounting could no
	// longer accommodate, which happens when a node comes back smaller than it
	// went away. They are reported apart from Dropped because ordinary ageing
	// and a node losing capacity are different events with different causes.
	Unfittable []string
	// Err carries a failure from a method that returns only a Result, such as
	// the accounting refusing to release capacity a lease claimed to hold,
	// which means the agent's books have diverged from what it believes it
	// lent. Work stops at that point rather than continuing over an accounting
	// it can no longer trust. Methods that return an error use that instead,
	// so a caller never has to check two places.
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
		if err := m.persistLocked(); err != nil && res.Err == nil {
			res.Err = err
		}
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
	res := m.expireLocked(now)
	if len(res.Dropped) > 0 {
		if err := m.persistLocked(); err != nil && res.Err == nil {
			res.Err = err
		}
	}
	return res
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
