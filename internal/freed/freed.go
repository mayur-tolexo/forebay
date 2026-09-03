// Package freed measures the gap between an unlink returning and the space it
// released being visible.
//
// RFC-0018 asks when freed capacity becomes available rather than when unlink
// returns, and the agent has since come to rest on the answer: a floor sized
// from a duration corrects the observed rate by what the agent gave back, which
// assumes the filesystem shows it by the next poll. If it does not, the rate
// reads high.
package freed

import (
	"errors"
	"fmt"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Free reports what a filesystem says is available.
type Free func() (int64, error)

// Result is one observation of a release.
type Result struct {
	// Unlink is how long the removal itself took to return.
	Unlink time.Duration
	// Visible is how long after that the space appeared. Zero means it was
	// already there in the first reading taken afterwards.
	Visible time.Duration
	// Saw is how much appeared, which is not always what was released: a
	// filesystem with other users moves underneath the measurement.
	Saw int64
	// Polls is how many readings it took, so a Visible of zero can be told
	// from a measurement that never looked.
	Polls int
}

var (
	// ErrNotSeen reports space that never appeared within the patience given.
	ErrNotSeen = errors.New("freed: the space did not appear")
	// ErrConfounded reports free space falling below where it started, which
	// means something else was taking it while this was waiting. The release
	// cannot be separated from that, and calling it not visible would blame
	// the filesystem for a writer.
	ErrConfounded = errors.New("freed: free space fell while waiting, so something else was taking it")
)

// Watch times a release: it reads free space, runs release, then reads again
// until at least want bytes have appeared.
//
// The first reading after the release is taken immediately, so a filesystem
// that had already accounted for it reports zero rather than one poll's worth.
func Watch(free Free, release func() error, want int64, every, patience time.Duration) (Result, error) {
	var r Result
	if want <= 0 {
		return r, fmt.Errorf("freed: want must be positive, got %d", want)
	}
	if every <= 0 || patience <= 0 {
		return r, fmt.Errorf("freed: poll %s and patience %s must both be positive", every, patience)
	}

	before, err := free()
	if err != nil {
		return r, fmt.Errorf("freed: reading free space: %w", err)
	}

	start := time.Now()
	if err := release(); err != nil {
		return r, fmt.Errorf("freed: releasing: %w", err)
	}
	r.Unlink = time.Since(start)

	deadline := time.Now().Add(patience)
	after := time.Now()
	// Free space only rises while this waits, because the only thing acting on
	// it is the release. Any fall is somebody else writing, and it is caught
	// against the most that has been seen rather than against the start: a
	// writer taking less than the release gives back leaves the total above
	// where it began while still making the number unattributable.
	peak := before
	for {
		now, err := free()
		if err != nil {
			return r, fmt.Errorf("freed: reading free space: %w", err)
		}
		r.Polls++
		got := now - before
		if got >= want {
			r.Saw = got
			r.Visible = time.Since(after)
			return r, nil
		}
		if now < peak {
			r.Saw = got
			r.Visible = time.Since(after)
			return r, fmt.Errorf("%w: %s went while waiting", ErrConfounded, pool.Bytes(peak-now))
		}
		peak = now
		if time.Now().After(deadline) {
			r.Saw = now - before
			r.Visible = time.Since(after)
			return r, fmt.Errorf("%w: %d of %d bytes after %s", ErrNotSeen, r.Saw, want, r.Visible.Round(time.Millisecond))
		}
		time.Sleep(every)
	}
}
