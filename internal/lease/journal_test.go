package lease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// fakeJournal records what it was asked to save and can be made to fail.
type fakeJournal struct {
	saved   []Lease
	saves   int
	saveErr error
	loaded  []Lease
	loadErr error
}

func (f *fakeJournal) Save(ls []Lease) error {
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append([]Lease(nil), ls...)
	return nil
}

func (f *fakeJournal) Load() ([]Lease, error) { return f.loaded, f.loadErr }

// mustRestore replays a manager's journal, since one with a journal lends
// nothing until it knows what it already lent.
func mustRestore(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.Restore(t0); err != nil {
		t.Fatalf("Restore = %v", err)
	}
}

func tempJournal(t *testing.T) *FileJournal {
	t.Helper()
	return NewFileJournal(filepath.Join(t.TempDir(), "leases.json"))
}

func TestFileJournalRoundTrip(t *testing.T) {
	j := tempJournal(t)
	want := []Lease{
		{ID: "a", Class: Elastic, Size: 2 * pool.TiB, GrantedAt: t0, Term: 30 * time.Minute},
		{ID: "b", Class: Guaranteed, Size: 512 * pool.GiB, GrantedAt: t0, Term: time.Hour},
	}
	if err := j.Save(want); err != nil {
		t.Fatalf("Save = %v", err)
	}
	got, err := j.Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load returned %d leases, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Class != want[i].Class ||
			got[i].Size != want[i].Size || got[i].Term != want[i].Term ||
			!got[i].GrantedAt.Equal(want[i].GrantedAt) {
			t.Errorf("lease %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFileJournalIsReadableByAPerson(t *testing.T) {
	// An operator reading this during an incident should not have to decode
	// integers to find out what a lease is.
	j := tempJournal(t)
	if err := j.Save([]Lease{{ID: "a", Class: Elastic, Size: pool.GiB, GrantedAt: t0, Term: 30 * time.Minute}}); err != nil {
		t.Fatalf("Save = %v", err)
	}
	body, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	for _, want := range []string{`"class": "elastic"`, `"term": "30m0s"`, `"format": 1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("journal does not contain %s:\n%s", want, body)
		}
	}
}

func TestFileJournalMissingFileIsAFirstBootNotAFailure(t *testing.T) {
	got, err := tempJournal(t).Load()
	if err != nil {
		t.Fatalf("Load of a missing journal = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("Load returned %d leases, want none", len(got))
	}
}

func TestFileJournalRejectsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"not json", "{{{"},
		{"wrong format version", `{"format":99,"leases":[]}`},
		{"unknown class", `{"format":1,"leases":[{"id":"a","class":"magic","size_bytes":1,"granted_at":"2026-08-31T12:00:00Z","term":"1h"}]}`},
		{"bad term", `{"format":1,"leases":[{"id":"a","class":"elastic","size_bytes":1,"granted_at":"2026-08-31T12:00:00Z","term":"soon"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := tempJournal(t)
			if err := os.WriteFile(j.path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("WriteFile = %v", err)
			}
			if _, err := j.Load(); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Load = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestFileJournalLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	j := NewFileJournal(filepath.Join(dir, "leases.json"))
	for i := 0; i < 5; i++ {
		if err := j.Save([]Lease{{ID: "a", Class: Elastic, Size: pool.GiB, GrantedAt: t0, Term: time.Hour}}); err != nil {
			t.Fatalf("Save = %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the journal", len(entries))
	}
}

func TestAcceptJournalsBeforeHonouringTheGrant(t *testing.T) {
	f := &fakeJournal{}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))
	mustRestore(t, m)
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	if f.saves != 1 || len(f.saved) != 1 || f.saved[0].ID != "a" {
		t.Fatalf("journal saw saves=%d leases=%v, want the grant recorded", f.saves, f.saved)
	}
}

func TestAcceptUndoesTheGrantIfItCannotBeJournalled(t *testing.T) {
	// Capacity lent with no record of the lending leaks the moment the agent
	// restarts, so a grant that cannot be written must not be honoured.
	boom := errors.New("disk full")
	f := &fakeJournal{saveErr: boom}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))
	mustRestore(t, m)

	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); !errors.Is(err, boom) {
		t.Fatalf("Accept = %v, want the journal error", err)
	}
	if got := m.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s after a failed journal write, want 0", got)
	}
	if got := len(m.Leases()); got != 0 {
		t.Errorf("lease count = %d after a failed journal write, want 0", got)
	}
}

func TestRestoreRebuildsAccountingAndDropsExpired(t *testing.T) {
	f := &fakeJournal{loaded: []Lease{
		{ID: "live", Class: Elastic, Size: 1 * pool.TiB, GrantedAt: t0, Term: time.Hour},
		{ID: "stale", Class: Elastic, Size: 1 * pool.TiB, GrantedAt: t0, Term: time.Minute},
	}}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))

	res, err := m.Restore(t0.Add(30 * time.Minute))
	if err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != "stale" {
		t.Fatalf("Dropped = %v, want the expired lease", res.Dropped)
	}
	if got := m.Accounting().Borrowed; got != 1*pool.TiB {
		t.Errorf("Borrowed = %s, want only the live lease counted", got)
	}
	if err := m.Accounting().Validate(); err != nil {
		t.Errorf("accounting after restore = %v, want it to balance", err)
	}
}

func TestRestoreDropsLeasesTheNodeCanNoLongerFit(t *testing.T) {
	// The node's shape can change while it is down. Compute keeps whatever it
	// now needs, and the leases that no longer fit are given up rather than
	// overcommitting the device.
	f := &fakeJournal{loaded: []Lease{
		{ID: "big", Class: Elastic, Size: 4 * pool.TiB, GrantedAt: t0, Term: time.Hour},
		{ID: "small", Class: Elastic, Size: 1 * pool.TiB, GrantedAt: t0, Term: time.Hour},
	}}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB, Compute: 5 * pool.TiB}, relaxed(), WithJournal(f))

	res, err := m.Restore(t0)
	if err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != "big" {
		t.Fatalf("Dropped = %v, want the lease that no longer fits", res.Dropped)
	}
	if err := m.Accounting().Validate(); err != nil {
		t.Errorf("accounting after restore = %v, want it to balance", err)
	}
}

func TestRestoreStartsEmptyWhenTheJournalIsUnreadable(t *testing.T) {
	// A journal that cannot be read is recoverable, because everything it
	// describes is regenerable. The error is still returned so it is reported.
	f := &fakeJournal{loadErr: ErrCorrupt}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB, Borrowed: 2 * pool.TiB}, relaxed(), WithJournal(f))

	if _, err := m.Restore(t0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore = %v, want ErrCorrupt", err)
	}
	if got := m.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s, want the borrowed pool discarded", got)
	}
	if f.saves == 0 {
		t.Error("the reset was not written back to the journal")
	}
}

func TestRestoreWithoutAJournalIsANoOp(t *testing.T) {
	m := node(relaxed())
	if _, err := m.Restore(t0); err != nil {
		t.Fatalf("Restore = %v, want nil", err)
	}
}

func TestReleasePathsArePersisted(t *testing.T) {
	f := &fakeJournal{}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))
	mustRestore(t, m)
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}
	before := f.saves
	if res := m.Reclaim(1*pool.TiB, t0); len(res.Dropped) != 1 {
		t.Fatalf("Reclaim = %+v, want one lease dropped", res)
	}
	if f.saves != before+1 {
		t.Errorf("saves = %d, want the reclamation persisted", f.saves)
	}
	if len(f.saved) != 0 {
		t.Errorf("journal still holds %v, want it empty", f.saved)
	}
}

func TestJournalSurvivesAManagerRestart(t *testing.T) {
	// The whole point: what a node lent is still known after it comes back.
	j := tempJournal(t)
	acct := pool.Accounting{Capacity: 8 * pool.TiB, Compute: 1 * pool.TiB}

	first := New(acct, relaxed(), WithJournal(j))
	mustRestore(t, first)
	if err := first.Accept(grant("a", Elastic, 2*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v", err)
	}

	second := New(acct, relaxed(), WithJournal(j))
	if _, err := second.Restore(t0.Add(time.Minute)); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if got := second.Accounting().Borrowed; got != 2*pool.TiB {
		t.Fatalf("Borrowed after restart = %s, want 2.00TiB", got)
	}
	if got := second.Leases(); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("leases after restart = %v, want the lease recovered", got)
	}
}

func TestParseClassRoundTrips(t *testing.T) {
	for _, c := range []Class{Opportunistic, Elastic, Guaranteed} {
		got, err := ParseClass(c.String())
		if err != nil || got != c {
			t.Errorf("ParseClass(%q) = %v, %v, want %v", c.String(), got, err, c)
		}
	}
	if _, err := ParseClass("nonsense"); !errors.Is(err, ErrBadClass) {
		t.Errorf("ParseClass(nonsense) = %v, want ErrBadClass", err)
	}
}

func TestFileJournalRejectsDuplicateIdentifiers(t *testing.T) {
	// A duplicate would be lent for twice and kept once, leaving the
	// accounting permanently above the leases that justify it.
	j := tempJournal(t)
	body := `{"format":1,"leases":[
	  {"id":"a","class":"elastic","size_bytes":1099511627776,"granted_at":"2026-08-31T12:00:00Z","term":"1h0m0s"},
	  {"id":"a","class":"elastic","size_bytes":1099511627776,"granted_at":"2026-08-31T12:00:00Z","term":"1h0m0s"}
	]}`
	if err := os.WriteFile(j.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if _, err := j.Load(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load with a duplicate id = %v, want ErrCorrupt", err)
	}
}

func TestRestoreReportsPersistFailureThroughTheErrorOnly(t *testing.T) {
	// One error channel per method, so a caller never has to check two places.
	boom := errors.New("disk full")
	f := &fakeJournal{
		saveErr: boom,
		loaded: []Lease{
			{ID: "stale", Class: Elastic, Size: 1 * pool.TiB, GrantedAt: t0, Term: time.Minute},
		},
	}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))

	res, err := m.Restore(t0.Add(time.Hour))
	if !errors.Is(err, boom) {
		t.Fatalf("Restore = %v, want the journal error", err)
	}
	if res.Err != nil {
		t.Errorf("Result.Err = %v, want the failure only on the returned error", res.Err)
	}
}

func TestManagerWithAJournalLendsNothingUntilRestored(t *testing.T) {
	f := &fakeJournal{}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))

	if m.Restored() {
		t.Error("Restored() = true before any replay, want false")
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); !errors.Is(err, ErrNotRestored) {
		t.Fatalf("Accept before restore = %v, want ErrNotRestored", err)
	}
	if got := m.Accounting().Borrowed; got != 0 {
		t.Errorf("Borrowed = %s after a refused grant, want 0", got)
	}

	if _, err := m.Restore(t0); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	if !m.Restored() {
		t.Error("Restored() = false after a clean replay, want true")
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept after restore = %v, want nil", err)
	}
}

func TestManagerWithNoJournalNeedsNoRestore(t *testing.T) {
	// Nothing to replay means nothing to wait for.
	m := node(relaxed())
	if !m.Restored() {
		t.Error("Restored() = false without a journal, want true")
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept = %v, want nil", err)
	}
}

func TestReclamationIsNeverGatedOnTheReplay(t *testing.T) {
	// Compute always wins. Handing capacity back is safe from any state, and
	// making a job wait on a journal replay would invert that rule.
	f := &fakeJournal{loaded: []Lease{
		{ID: "a", Class: Elastic, Size: 2 * pool.TiB, GrantedAt: t0, Term: time.Hour},
	}}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))
	if _, err := m.Restore(t0); err != nil {
		t.Fatalf("Restore = %v", err)
	}

	// Force the manager back to unrestored to prove reclaim still works there.
	m.mu.Lock()
	m.restored = false
	m.mu.Unlock()

	res := m.Reclaim(1*pool.TiB, t0.Add(time.Minute))
	if res.Reclaimed != 2*pool.TiB || res.Err != nil {
		t.Fatalf("Reclaim while unrestored = %+v, want capacity returned", res)
	}
	if exp := m.Expire(t0.Add(2 * time.Hour)); exp.Err != nil {
		t.Fatalf("Expire while unrestored = %v, want nil", exp.Err)
	}
}

func TestAnUnreadableJournalStillLeavesTheNodeAbleToLend(t *testing.T) {
	// Corruption is recoverable, so the node starts empty and carries on. It
	// would be worse to leave a node unable to lend for the life of the
	// process because one file could not be parsed.
	f := &fakeJournal{loadErr: ErrCorrupt}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))

	if _, err := m.Restore(t0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Restore = %v, want ErrCorrupt", err)
	}
	if !m.Restored() {
		t.Fatal("Restored() = false after recovering from corruption, want true")
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); err != nil {
		t.Fatalf("Accept after recovery = %v, want nil", err)
	}
}

func TestAJournalThatCannotBeWrittenLeavesTheNodeUnrestored(t *testing.T) {
	// The reset could not be recorded, so the next restart reads the same
	// unreadable file. Lending against state that cannot be persisted is the
	// leak this whole mechanism exists to prevent.
	f := &fakeJournal{loadErr: ErrCorrupt, saveErr: errors.New("disk full")}
	m := New(pool.Accounting{Capacity: 8 * pool.TiB}, relaxed(), WithJournal(f))

	if _, err := m.Restore(t0); err == nil {
		t.Fatal("Restore = nil, want the write failure reported")
	}
	if m.Restored() {
		t.Error("Restored() = true though the reset could not be written, want false")
	}
	if err := m.Accept(grant("a", Elastic, 1*pool.TiB), t0); !errors.Is(err, ErrNotRestored) {
		t.Fatalf("Accept = %v, want ErrNotRestored", err)
	}
}
