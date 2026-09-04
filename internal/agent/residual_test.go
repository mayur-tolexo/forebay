package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/lease"
)

// TestFreshCapacityReadsAsZeros is the guarantee itself: reclamation is an
// unlink, so nothing scrubs an extent on the way out and a new holder must not
// be able to see what the last one wrote.
func TestFreshCapacityReadsAsZeros(t *testing.T) {
	dir := t.TempDir()

	// Written first, so the blocks the probe is likely to be given have
	// something on them to leak.
	prior := filepath.Join(dir, "prior.dat")
	if err := os.WriteFile(prior, bytes.Repeat([]byte{0xA5}, probeSize), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(prior); err != nil {
		t.Fatal(err)
	}

	if err := VerifyNoResidualData(dir); err != nil {
		t.Fatalf("a fresh extent did not read as zeros: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, probeName)); !os.IsNotExist(err) {
		t.Error("the probe was left in the pool")
	}
}

// TestVerifyRefusesAPoolItCannotUse keeps a node that cannot run the check from
// being treated as one that passed it.
func TestVerifyRefusesAPoolItCannotUse(t *testing.T) {
	if err := VerifyNoResidualData(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a pool directory that is not there was reported as clean")
	}
}

// TestAStaleProbeIsNotAFinding covers the crash case: a probe left by an
// interrupted startup would otherwise fail O_EXCL and be reported as residual
// data, which would take a healthy node out of the pool for the wrong reason.
func TestAStaleProbeIsNotAFinding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, probeName), []byte("left over"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := VerifyNoResidualData(dir); err != nil {
		t.Errorf("a probe left by a crash was reported as a residual-data finding: %v", err)
	}
}

// TestResidualDataIsReportedWithItsOffset covers the finding, which cannot be
// produced by a filesystem that behaves, so the reader is driven directly.
func TestResidualDataIsReportedWithItsOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dirty")
	body := make([]byte, 2<<20)
	body[1<<20+7] = 0xFF
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	err = scanZeros(f, int64(len(body)))
	if !errors.Is(err, ErrResidualData) {
		t.Fatalf("a byte left from a previous holder was not reported as residual data: %v", err)
	}
	// The offset is what tells an operator whether one block leaked or the
	// whole guarantee is absent.
	if want := "byte 1048583"; !strings.Contains(err.Error(), want) {
		t.Errorf("the finding does not say where it was: %v", err)
	}
}

// TestAShortReservationIsItsOwnDefect keeps a reservation that committed less
// than it claimed from being reported as a clean pool.
func TestAShortReservationIsItsOwnDefect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(path, make([]byte, 4096), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	err = scanZeros(f, 8<<20)
	if err == nil {
		t.Fatal("an extent that stopped short was reported as clean")
	}
	if errors.Is(err, ErrResidualData) {
		t.Errorf("a short reservation was blamed on residual data: %v", err)
	}
}

// TestAnExactlySizedExtentIsNotShort matters because the reader stops on the
// same signal in both cases, and a whole clean extent must not be read as one
// that stopped short.
func TestAnExactlySizedExtentIsNotShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact")
	if err := os.WriteFile(path, make([]byte, 2<<20), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := scanZeros(f, 2<<20); err != nil {
		t.Errorf("an extent that was exactly its size was reported as short: %v", err)
	}
}

// TestOpenRefusesToLendFromADirtyPool is the whole point: a node that cannot
// prove its capacity is clean must not reach the state where it can accept a
// grant. The check is replaced rather than provoked, because a filesystem that
// behaves will not produce a finding and the obvious stand-in — a directory
// the probe cannot be created in — proves nothing for a process running as
// root, which is how the agent runs on a node.
func TestOpenRefusesToLendFromADirtyPool(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}

	saved := verifyPool
	t.Cleanup(func() { verifyPool = saved })
	verifyPool = func(string) error { return ErrResidualData }

	a, _, err := Open(cfg, acct(), time.Unix(0, 0))
	if !errors.Is(err, ErrResidualData) {
		if a != nil {
			a.Close()
		}
		t.Fatalf("a pool that could not be proved clean was opened and can lend: %v", err)
	}
}

// TestOpenRunsTheCheckAgainstItsOwnPool keeps the check from being run against
// somewhere other than the capacity the node is about to lend.
func TestOpenRunsTheCheckAgainstItsOwnPool(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}

	saved := verifyPool
	t.Cleanup(func() { verifyPool = saved })
	var checked string
	verifyPool = func(dir string) error { checked = dir; return nil }

	a, _, err := Open(cfg, acct(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if checked != cfg.BorrowedDir {
		t.Errorf("the check ran against %q, want the borrowed pool %q", checked, cfg.BorrowedDir)
	}
}

// nothing returns no bytes and no reason, which is outside the ReaderAt
// contract and is exactly what would make the scan loop forever.
type nothing struct{}

func (nothing) ReadAt([]byte, int64) (int, error) { return 0, nil }

// TestAReaderThatReturnsNothingDoesNotHang matters because a check that hangs
// is worse than one that fails: the node never finishes starting, and never
// says why.
func TestAReaderThatReturnsNothingDoesNotHang(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- scanZeros(nothing{}, 1<<20) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a reader that returned nothing was reported as a clean extent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scan did not terminate")
	}
}

// TestOpenUsesTheRealCheck closes the gap the seam opens: every other test here
// replaces verifyPool, so none of them would notice if the wiring pointed at
// something that always passes.
func TestOpenUsesTheRealCheck(t *testing.T) {
	if reflect.ValueOf(verifyPool).Pointer() != reflect.ValueOf(VerifyNoResidualData).Pointer() {
		t.Error("Open runs something other than the residual-data check")
	}
}

// TestTheProbeRunsAfterReclamation covers the ordering that decides whether a
// node can recover. The probe needs space, so a node that crashed with a full
// pool must reach reclamation first or it would fail on ENOSPC and refuse to
// lend forever, having never freed the room it needed to prove itself.
func TestTheProbeRunsAfterReclamation(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		BorrowedDir: filepath.Join(root, "borrowed"),
		JournalPath: filepath.Join(root, "state", "leases.json"),
		Lease:       lease.DefaultConfig(),
	}
	if err := os.MkdirAll(cfg.BorrowedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// An extent no lease claims, which reconciliation removes.
	orphan := filepath.Join(cfg.BorrowedDir, "orphan")
	if err := os.WriteFile(orphan, []byte("occupying the pool"), 0o640); err != nil {
		t.Fatal(err)
	}

	saved := verifyPool
	t.Cleanup(func() { verifyPool = saved })
	var orphanStillThere bool
	verifyPool = func(string) error {
		_, err := os.Stat(orphan)
		orphanStillThere = err == nil
		return nil
	}

	a, _, err := Open(cfg, acct(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if orphanStillThere {
		t.Error("the probe ran while the pool was still full of what a crash left")
	}
}

// TestTheScanJudgesOnlyWhatItWasAsked matters because a reader can hold more
// than the extent, and bytes past it are not this check's to call residual
// data. The size is deliberately not a multiple of the read buffer, since that
// is the only shape in which the last read reaches past the extent at all.
func TestTheScanJudgesOnlyWhatItWasAsked(t *testing.T) {
	const extent = 1<<20 + 10

	path := filepath.Join(t.TempDir(), "over")
	body := make([]byte, 2<<20)
	body[extent+90] = 0xFF
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := scanZeros(f, extent); err != nil {
		t.Errorf("a byte past the extent was reported against it: %v", err)
	}

	// The same byte, inside an extent that reaches it, must still be found.
	if err := scanZeros(f, 2<<20); !errors.Is(err, ErrResidualData) {
		t.Errorf("a byte inside the extent was not reported: %v", err)
	}
}
