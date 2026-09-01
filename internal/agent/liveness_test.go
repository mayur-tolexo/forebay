package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAFreshAgentIsImmediatelyLive(t *testing.T) {
	// The heartbeat is written as part of startup, so a probe that runs before
	// the agent has done anything else still finds progress. Without it the
	// first probe after a restart would kill the agent that had just taken the
	// lock, which is a restart loop rather than a liveness check.
	a, now := openAgent(t)
	if err := CheckLiveness(a.cfg.BorrowedDir, time.Minute, now); err != nil {
		t.Errorf("a freshly started agent is not live: %v", err)
	}
}

func TestAStalledAgentIsReportedStalled(t *testing.T) {
	// The case the whole mechanism exists for: a process that still holds the
	// lock and has stopped making progress. Nothing else can find it, because
	// the lock is held by a live file descriptor and the process answers no
	// questions.
	a, now := openAgent(t)
	err := CheckLiveness(a.cfg.BorrowedDir, time.Minute, now.Add(2*time.Minute))
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("liveness = %v, want ErrStalled", err)
	}
	// The window is the operator's, so a longer one forgives the same gap.
	if err := CheckLiveness(a.cfg.BorrowedDir, time.Hour, now.Add(2*time.Minute)); err != nil {
		t.Errorf("liveness within a longer window = %v, want nil", err)
	}
}

func TestAMissingHeartbeatIsStalledRatherThanFine(t *testing.T) {
	// An agent that never reached its first heartbeat holds the lock just as
	// effectively as one that stopped later, so absence cannot be treated as
	// nothing to worry about.
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckLiveness(root, time.Minute, time.Now()); !errors.Is(err, ErrStalled) {
		t.Errorf("liveness with no heartbeat = %v, want ErrStalled", err)
	}
}

func TestHeartbeatIsNeverReadHalfWritten(t *testing.T) {
	// Written through a rename, so a reader either sees the previous timestamp
	// or the new one. A probe that read a truncated timestamp would fail to
	// parse it and kill a healthy agent.
	a, now := openAgent(t)
	for i := range 20 {
		if err := a.Heartbeat(now.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		if _, err := LastProgress(a.cfg.BorrowedDir); err != nil {
			t.Fatalf("reading heartbeat %d: %v", i, err)
		}
	}
	// Every temporary file it wrote through has been renamed away.
	entries, err := os.ReadDir(a.cfg.BorrowedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == lockName || e.Name() == heartbeatName {
			continue
		}
		t.Errorf("%s was left behind by writing heartbeats", e.Name())
	}
}

func TestTheAgentsOwnFilesSurviveReconciliation(t *testing.T) {
	// Reconciliation unlinks everything no lease accounts for, which would
	// delete the agent's own state if it were not reserved. The lock was
	// reserved by name and the heartbeat would have been forgotten.
	a, now := openAgent(t)
	cfg := a.cfg
	if err := a.Grant(grantable("lease-a", 1<<20), now); err != nil {
		t.Fatal(err)
	}
	a.Close()

	restarted, rec, err := Open(cfg, testAccounting(), now)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	defer restarted.Close()
	if len(rec.OrphanExtents) != 0 {
		t.Errorf("OrphanExtents = %v, want the agent's own files left alone", rec.OrphanExtents)
	}
	if _, err := os.Stat(filepath.Join(cfg.BorrowedDir, heartbeatName)); err != nil {
		t.Errorf("the heartbeat was removed by reconciliation: %v", err)
	}
	if err := CheckLiveness(cfg.BorrowedDir, time.Minute, now); err != nil {
		t.Errorf("a restarted agent is not live: %v", err)
	}
}

func TestNoLeaseCanBeNamedOverTheAgentsFiles(t *testing.T) {
	// Identifiers arrive from the control plane. One shaped like the agent's
	// own state would let a grant overwrite the lock or the heartbeat, and a
	// release would unlink it.
	a, now := openAgent(t)
	for _, id := range []string{lockName, heartbeatName, agentFilePrefix, agentFilePrefix + "anything"} {
		if err := a.Grant(grantable(id, 1<<20), now); !errors.Is(err, ErrBadLeaseID) {
			t.Errorf("granting a lease named %q = %v, want ErrBadLeaseID", id, err)
		}
	}
}

func TestAKilledHeartbeatDoesNotLeakForever(t *testing.T) {
	// A liveness probe kills a wedged agent, so a SIGKILL between writing a
	// heartbeat and renaming it is routine rather than rare. The prefix that
	// keeps reconciliation off the agent's live files must not also put its
	// litter beyond reach.
	a, now := openAgent(t)
	cfg := a.cfg
	tmp, err := os.CreateTemp(cfg.BorrowedDir, agentFilePrefix+"heartbeat-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	a.Close()

	restarted, rec, err := Open(cfg, testAccounting(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	if len(rec.Leftovers) != 1 || rec.Leftovers[0] != filepath.Base(tmp.Name()) {
		t.Errorf("Leftovers = %v, want the killed heartbeat's temporary file", rec.Leftovers)
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Error("the leftover survived a restart")
	}
	// The live files are still there.
	if err := CheckLiveness(cfg.BorrowedDir, time.Minute, now.Add(time.Second)); err != nil {
		t.Errorf("the restarted agent is not live: %v", err)
	}
}

func TestAnUnreadableHeartbeatIsNotTreatedAsStalled(t *testing.T) {
	// Absent means the agent never got that far and is holding the lock.
	// Unreadable means the probe could not tell, and killing a healthy agent
	// over it would repeat forever without fixing the cause.
	a, now := openAgent(t)
	if err := os.WriteFile(filepath.Join(a.cfg.BorrowedDir, heartbeatName), []byte("not a time"), 0o640); err != nil {
		t.Fatal(err)
	}
	err := CheckLiveness(a.cfg.BorrowedDir, time.Minute, now)
	if !errors.Is(err, ErrUnreadable) {
		t.Errorf("liveness on a corrupt heartbeat = %v, want ErrUnreadable", err)
	}
	if errors.Is(err, ErrStalled) {
		t.Error("an unreadable heartbeat was reported as stalled")
	}
}
