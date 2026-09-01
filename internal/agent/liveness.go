package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// heartbeatName holds the time the agent last made progress. The reserved
// prefix keeps reconciliation off it and stops a lease being named over it.
const heartbeatName = agentFilePrefix + "heartbeat"

var (
	// ErrStalled reports an agent holding the lock and making no progress.
	ErrStalled = errors.New("agent: no progress within the liveness window")
	// ErrUnreadable reports a heartbeat that could not be read at all. It is
	// kept apart from ErrStalled because killing a healthy agent over a bad
	// mount would repeat forever without fixing the cause.
	ErrUnreadable = errors.New("agent: heartbeat could not be read")
)

// Heartbeat records that the agent is still making progress.
//
// A file rather than an endpoint, because a wedged process cannot answer for
// itself. Published by rename, so a probe never reads a half-written timestamp.
func (a *Agent) Heartbeat(now time.Time) error {
	dir := a.cfg.BorrowedDir
	tmp, err := os.CreateTemp(dir, agentFilePrefix+"heartbeat-*")
	if err != nil {
		return fmt.Errorf("agent: creating heartbeat: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(now.UTC().Format(time.RFC3339Nano)); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("agent: writing heartbeat: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("agent: closing heartbeat: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, heartbeatName)); err != nil {
		os.Remove(name)
		return fmt.Errorf("agent: publishing heartbeat: %w", err)
	}
	return nil
}

// LastProgress reads when an agent last reported progress in this pool.
func LastProgress(borrowedDir string) (time.Time, error) {
	raw, err := os.ReadFile(filepath.Join(borrowedDir, heartbeatName))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, fmt.Errorf("%w: never written", ErrStalled)
		}
		return time.Time{}, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: not a time: %v", ErrUnreadable, err)
	}
	return at, nil
}

// CheckLiveness is what a liveness probe runs against a pool.
//
// Readiness cannot do this job: an unready process still holds the lock, and
// only killing it frees the lock. A heartbeat never written is stalled too.
func CheckLiveness(borrowedDir string, staleAfter time.Duration, now time.Time) error {
	if staleAfter <= 0 {
		return fmt.Errorf("agent: liveness window must be positive, got %s", staleAfter)
	}
	at, err := LastProgress(borrowedDir)
	if err != nil {
		return err
	}
	if age := now.Sub(at); age > staleAfter {
		return fmt.Errorf("%w: last progress %s ago, window is %s",
			ErrStalled, age.Round(time.Millisecond), staleAfter)
	}
	return nil
}
