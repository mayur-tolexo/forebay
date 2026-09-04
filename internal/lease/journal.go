package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// journalFormat is the on-disk layout version. It is written so that a future
// reader can recognise a file it does not understand instead of guessing.
const journalFormat = 1

// ErrCorrupt is a journal that cannot be read back.
//
// It is recoverable rather than fatal: everything the journal describes is
// regenerable, so an agent that cannot read its own journal can discard the
// borrowed pool and start empty. That is a luxury most journals do not have,
// and it is why this one needs no repair path.
var ErrCorrupt = errors.New("lease: journal is unreadable")

// Journal persists lease state so that an agent restart does not forget what
// it lent. Capacity nobody has a record of lending is capacity that has leaked.
type Journal interface {
	// Save records the complete set of live leases.
	Save(leases []Lease) error
	// Load returns what was last saved. A journal that has never been written
	// returns no leases and no error, since a first boot is not a failure.
	Load() ([]Lease, error)
}

// record is the on-disk form of a lease.
//
// It is deliberately not the in-memory struct. Class and Term are written as
// text so an operator reading the file during an incident sees "elastic" and
// "30m0s" rather than two integers they have to decode.
type record struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant,omitempty"`
	Class     string    `json:"class"`
	SizeBytes int64     `json:"size_bytes"`
	GrantedAt time.Time `json:"granted_at"`
	Term      string    `json:"term"`
}

// document is the whole journal, rewritten in full on every change.
type document struct {
	Format int      `json:"format"`
	Leases []record `json:"leases"`
}

// FileJournal stores lease state in a single file, rewritten atomically.
//
// Whole-file rewrite rather than an append-only log because a node holds tens
// of leases, so the write is trivial, and it avoids torn records, replay
// ordering and compaction for a volume that never justifies them.
//
// Concurrent Save calls cannot corrupt the file, since each writes its own
// temporary file and rename is atomic, but they do not merge either: the last
// rename wins and the other writer's state is lost. One journal therefore
// belongs to one Manager, which serialises its own writes.
type FileJournal struct {
	path string
}

// NewFileJournal returns a journal backed by the file at path. The containing
// directory must already exist.
func NewFileJournal(path string) *FileJournal { return &FileJournal{path: path} }

// Save writes every live lease, replacing the previous contents.
//
// The write goes to a temporary file which is flushed and then renamed over
// the target, so a crash midway leaves the previous journal intact rather than
// a half-written one. The directory is flushed too, because the rename itself
// is only durable once the directory entry is.
func (j *FileJournal) Save(leases []Lease) error {
	f := document{Format: journalFormat, Leases: make([]record, 0, len(leases))}
	for _, l := range leases {
		f.Leases = append(f.Leases, record{
			ID:        l.ID,
			Tenant:    l.Tenant,
			Class:     l.Class.String(),
			SizeBytes: int64(l.Size),
			GrantedAt: l.GrantedAt,
			Term:      l.Term.String(),
		})
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("lease: encoding journal: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(j.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(j.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("lease: creating journal temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("lease: writing journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("lease: flushing journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lease: closing journal: %w", err)
	}
	if err := os.Rename(tmpName, j.path); err != nil {
		return fmt.Errorf("lease: replacing journal: %w", err)
	}
	return syncDir(dir)
}

// syncDir flushes a directory so a rename into it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("lease: opening journal directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("lease: flushing journal directory: %w", err)
	}
	return nil
}

// Load reads the journal back. A missing file is a first boot, not a failure.
func (j *FileJournal) Load() ([]Lease, error) {
	body, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lease: reading journal: %w", err)
	}

	var f document
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if f.Format != journalFormat {
		return nil, fmt.Errorf("%w: format %d, expected %d", ErrCorrupt, f.Format, journalFormat)
	}

	out := make([]Lease, 0, len(f.Leases))
	// Identifiers must be unique. A duplicate would be lent for once per
	// record and kept once, leaving the accounting permanently higher than the
	// leases that justify it and the difference unreclaimable. Hand-editing
	// this file during an incident is exactly how one would appear.
	seen := make(map[string]struct{}, len(f.Leases))
	for _, r := range f.Leases {
		if _, dup := seen[r.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate lease id %q", ErrCorrupt, r.ID)
		}
		seen[r.ID] = struct{}{}

		c, err := ParseClass(r.Class)
		if err != nil {
			return nil, fmt.Errorf("%w: lease %s: %v", ErrCorrupt, r.ID, err)
		}
		term, err := time.ParseDuration(r.Term)
		if err != nil {
			return nil, fmt.Errorf("%w: lease %s term %q: %v", ErrCorrupt, r.ID, r.Term, err)
		}
		out = append(out, Lease{
			ID:        r.ID,
			Tenant:    r.Tenant,
			Class:     c,
			Size:      pool.Bytes(r.SizeBytes),
			GrantedAt: r.GrantedAt,
			Term:      term,
		})
	}
	return out, nil
}
