package freed

import (
	"errors"
	"testing"
	"time"
)

// stepped returns a free-space reader that reports each value in turn and
// holds the last, so a test decides when the space appears.
func stepped(values ...int64) Free {
	var i int
	return func() (int64, error) {
		v := values[i]
		if i < len(values)-1 {
			i++
		}
		return v, nil
	}
}

// TestSpaceAlreadyThereIsZero covers the filesystem that accounts for an
// unlink before it returns, which is what the agent's rate correction assumes.
func TestSpaceAlreadyThereIsZero(t *testing.T) {
	got, err := Watch(stepped(1000, 2000), func() error { return nil }, 1000, time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visible > 50*time.Millisecond {
		t.Errorf("visible after %s, want about nothing", got.Visible)
	}
	if got.Polls != 1 {
		t.Errorf("took %d polls, want 1: it was there on the first look", got.Polls)
	}
	if got.Saw != 1000 {
		t.Errorf("saw %d bytes, want 1000", got.Saw)
	}
}

// TestSpaceThatArrivesLateIsTimed is the case the row exists for, and the one
// that would make the agent's rate read high.
func TestSpaceThatArrivesLateIsTimed(t *testing.T) {
	// Three readings show nothing, the fourth shows it all.
	got, err := Watch(stepped(1000, 1000, 1000, 1000, 2000), func() error { return nil },
		1000, 5*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Polls < 4 {
		t.Errorf("took %d polls, want at least four", got.Polls)
	}
	if got.Visible < 10*time.Millisecond {
		t.Errorf("visible after %s, want the polls it waited", got.Visible)
	}
}

// TestSpaceThatNeverArrivesIsAnError keeps a release that did not free
// anything from being reported as one that did, instantly.
func TestSpaceThatNeverArrivesIsAnError(t *testing.T) {
	got, err := Watch(stepped(1000), func() error { return nil }, 1000, time.Millisecond, 30*time.Millisecond)
	if !errors.Is(err, ErrNotSeen) {
		t.Fatalf("err = %v, want ErrNotSeen", err)
	}
	if got.Saw != 0 {
		t.Errorf("saw %d bytes, want none", got.Saw)
	}
	if got.Polls == 0 {
		t.Error("gave up without looking")
	}
}

// TestTheUnlinkIsTimedSeparately keeps the call's own cost out of the wait,
// since the row is about the difference between the two.
func TestTheUnlinkIsTimedSeparately(t *testing.T) {
	slow := func() error { time.Sleep(20 * time.Millisecond); return nil }
	got, err := Watch(stepped(0, 1000), slow, 1000, time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Unlink < 15*time.Millisecond {
		t.Errorf("unlink took %s, want the time it slept", got.Unlink)
	}
	if got.Visible > 15*time.Millisecond {
		t.Errorf("the wait was %s, which has the unlink inside it", got.Visible)
	}
}

// TestAFailedReleaseIsNotAMeasurement keeps a broken run from reporting a
// duration as though it measured something.
func TestAFailedReleaseIsNotAMeasurement(t *testing.T) {
	_, err := Watch(stepped(0), func() error { return errors.New("permission denied") },
		1000, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("a release that failed was measured")
	}
	if errors.Is(err, ErrNotSeen) {
		t.Error("a failed release was reported as space that did not appear")
	}
}

func TestArgumentsAreChecked(t *testing.T) {
	ok := func() error { return nil }
	for _, c := range []struct {
		name            string
		want            int64
		every, patience time.Duration
	}{
		{"nothing wanted", 0, time.Millisecond, time.Second},
		{"no poll interval", 1000, 0, time.Second},
		{"no patience", 1000, time.Millisecond, 0},
	} {
		if _, err := Watch(stepped(0), ok, c.want, c.every, c.patience); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestSpaceTakenWhileWaitingIsNotALag covers a filesystem something else is
// writing to. Free space falling below where it started means the release
// cannot be separated from the writer, and reporting it as space that never
// appeared would blame the filesystem for somebody else's IO.
func TestSpaceTakenWhileWaitingIsNotALag(t *testing.T) {
	// Starts at 1000 and falls: a writer is taking more than the release gave.
	got, err := Watch(stepped(1000, 900), func() error { return nil }, 1000, time.Millisecond, time.Second)
	_ = got
	if !errors.Is(err, ErrConfounded) {
		t.Fatalf("err = %v, want ErrConfounded", err)
	}
	if errors.Is(err, ErrNotSeen) {
		t.Error("a confounded run was reported as space that did not appear")
	}
	if got.Saw >= 0 {
		t.Errorf("saw %d, want the shortfall that gave it away", got.Saw)
	}
}

// TestSpaceTakenWhileStillAboveTheStartIsConfounded covers the case a check
// against the baseline alone would miss: a writer taking less than the release
// gives back leaves free space above where it began, and reporting that as a
// filesystem slow to show a release would blame the wrong thing.
func TestSpaceTakenWhileStillAboveTheStartIsConfounded(t *testing.T) {
	// Rises to 1600 as the release lands, then falls to 1400 as a writer takes
	// some. Never below the 1000 it started at, and never reaching the 2000
	// that would satisfy the release.
	_, err := Watch(stepped(1000, 1600, 1400, 1400), func() error { return nil },
		1000, time.Millisecond, 200*time.Millisecond)
	if !errors.Is(err, ErrConfounded) {
		t.Fatalf("err = %v, want ErrConfounded", err)
	}
	if errors.Is(err, ErrNotSeen) {
		t.Error("a writer taking space was reported as a release that never appeared")
	}
}
