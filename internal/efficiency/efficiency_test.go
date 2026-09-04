package efficiency

import (
	"errors"
	"math"
	"testing"
	"time"
)

const ms = time.Millisecond

// record adds n reads of one shape.
func record(s *Scoreboard, src Source, bytes int64, took time.Duration, n int) {
	for i := 0; i < n; i++ {
		s.Record(Read{Source: src, Bytes: bytes, Took: took})
	}
}

// TestTheTierIsCreditedAgainstThisNodesOwnMisses is the counterfactual: the
// only defensible estimate of what a hit would have cost is one the same node
// produced against the same backend.
func TestTheTierIsCreditedAgainstThisNodesOwnMisses(t *testing.T) {
	s := New()
	record(s, Backend, 4<<20, 41*ms, 9)
	record(s, Tier, 4<<20, 3*ms, 2)

	got := s.Estimate()
	if want := 76 * ms; got.Saved != want {
		t.Errorf("saved %v, want %v: two hits at 3ms against a 41ms median", got.Saved, want)
	}
	if got.Covered != 2 || got.Uncovered != 0 {
		t.Errorf("covered %d, uncovered %d, want 2 and 0", got.Covered, got.Uncovered)
	}
}

// TestALossIsReportedLikeAGain is the honesty requirement. This project has
// measured its own tier reading slower than the object store, and a scoreboard
// that clamped at zero would be the dishonesty RFC-0024 exists to prevent.
func TestALossIsReportedLikeAGain(t *testing.T) {
	s := New()
	record(s, Backend, 1<<20, 230*ms, 5)
	record(s, Tier, 1<<20, 1710*ms, 1)

	got := s.Estimate()
	if got.Saved >= 0 {
		t.Fatalf("a tier slower than the backend reported a saving of %v", got.Saved)
	}
	if want := -1480 * ms; got.Saved != want {
		t.Errorf("saved %v, want %v", got.Saved, want)
	}
}

// TestAHitWithNoComparableMissIsUncovered keeps the estimate from being
// extrapolated across buckets, which would invent the number it exists to
// defend.
func TestAHitWithNoComparableMissIsUncovered(t *testing.T) {
	s := New()
	record(s, Backend, 4<<20, 40*ms, 4)
	record(s, Tier, 4<<20, 4*ms, 3)
	// A size nothing missed on, so nothing says what it would have cost.
	record(s, Tier, 64<<20, 9*ms, 1)

	got := s.Estimate()
	if got.Covered != 3 || got.Uncovered != 1 {
		t.Errorf("covered %d, uncovered %d, want 3 and 1", got.Covered, got.Uncovered)
	}
	// The uncovered hit contributes nothing rather than an extrapolation.
	if want := 108 * ms; got.Saved != want {
		t.Errorf("saved %v, want %v: only the three covered hits", got.Saved, want)
	}
	if f := got.CoveredFraction(); math.Abs(f-0.75) > 1e-9 {
		t.Errorf("covered fraction %v, want 0.75", f)
	}
}

// TestNothingRecordedCoversNothing keeps an empty scoreboard from reporting
// complete coverage of no reads, which would read as a confident zero.
func TestNothingRecordedCoversNothing(t *testing.T) {
	got := New().Estimate()
	if got.CoveredFraction() != 0 {
		t.Errorf("an empty scoreboard reported coverage %v", got.CoveredFraction())
	}
	if got.Saved != 0 || got.Spread != 0 {
		t.Errorf("an empty scoreboard reported %+v", got)
	}
}

// TestTheSpreadTravelsWithTheEstimate matters because a median with no
// dispersion behind it invites a reader to treat an estimate as a measurement.
func TestTheSpreadTravelsWithTheEstimate(t *testing.T) {
	tight := New()
	for _, d := range []time.Duration{40 * ms, 41 * ms, 42 * ms, 43 * ms} {
		tight.Record(Read{Source: Backend, Bytes: 1 << 20, Took: d})
	}
	tight.Record(Read{Source: Tier, Bytes: 1 << 20, Took: ms})

	wide := New()
	for _, d := range []time.Duration{8 * ms, 40 * ms, 42 * ms, 900 * ms} {
		wide.Record(Read{Source: Backend, Bytes: 1 << 20, Took: d})
	}
	wide.Record(Read{Source: Tier, Bytes: 1 << 20, Took: ms})

	t1, w1 := tight.Estimate(), wide.Estimate()
	if t1.Spread >= w1.Spread {
		t.Errorf("a tight bucket reported spread %v against a wide one's %v", t1.Spread, w1.Spread)
	}
	if t1.Spread == 0 {
		t.Error("an estimate was reported with no spread at all")
	}
}

// TestSizesAreNotAveragedTogether covers the bucketing: a 4KiB round trip and
// a 64MiB transfer estimated against each other would be dominated by whichever
// size was more common rather than by the read being estimated.
func TestSizesAreNotAveragedTogether(t *testing.T) {
	s := New()
	record(s, Backend, 4<<10, 5*ms, 3)
	record(s, Backend, 64<<20, 900*ms, 3)
	record(s, Tier, 4<<10, ms, 1)

	// Estimated against the small bucket alone, not against a blend.
	if want := 4 * ms; s.Estimate().Saved != want {
		t.Errorf("saved %v, want %v", s.Estimate().Saved, want)
	}
}

// TestMoneyAndHoursNeedTheOperator covers the refusal. Forebay sees a socket
// and not a training step, so both conversions are the operator's to declare.
func TestMoneyAndHoursNeedTheOperator(t *testing.T) {
	e := Estimate{Saved: time.Hour}

	if _, err := e.AcceleratorHours(Conversion{}); !errors.Is(err, ErrNoConversion) {
		t.Errorf("accelerator hours were reported without a declared count: %v", err)
	}
	if _, err := e.Money(Conversion{Accelerators: 8}); !errors.Is(err, ErrNoConversion) {
		t.Errorf("money was reported without a declared price: %v", err)
	}
	// A price with no accelerator count is still no basis for money.
	if _, err := e.Money(Conversion{PricePerHour: 3}); !errors.Is(err, ErrNoConversion) {
		t.Errorf("money was reported without a declared accelerator count: %v", err)
	}

	got, err := e.Money(Conversion{Accelerators: 8, PricePerHour: 3})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-24) > 1e-9 {
		t.Errorf("an hour saved for 8 accelerators at 3 an hour = %v, want 24", got)
	}
}

// TestALossConvertsToALoss keeps the sign through the conversion, since a
// scoreboard that reported a negative saving and a positive cost would be
// worse than one that reported neither.
func TestALossConvertsToALoss(t *testing.T) {
	got, err := Estimate{Saved: -time.Hour}.Money(Conversion{Accelerators: 2, PricePerHour: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got >= 0 {
		t.Errorf("an hour lost converted to %v", got)
	}
}

// TestABucketIsNotDisturbedByReadingIt matters because the reservoirs are
// appended to after an estimate is taken, and sorting the caller's slice in
// place would reorder a record of when reads happened.
func TestABucketIsNotDisturbedByReadingIt(t *testing.T) {
	s := New()
	for _, d := range []time.Duration{90 * ms, 10 * ms, 50 * ms} {
		s.Record(Read{Source: Backend, Bytes: 1 << 20, Took: d})
	}
	s.Record(Read{Source: Tier, Bytes: 1 << 20, Took: ms})
	s.Estimate()

	got := s.misses[bucket(1<<20)]
	want := []time.Duration{90 * ms, 10 * ms, 50 * ms}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the bucket was reordered by estimating from it: %v", got)
		}
	}
}

// TestAZeroLengthReadHasABucket keeps a read of nothing from sharing a bucket
// with a one-byte read by accident of the size function.
func TestAZeroLengthReadHasABucket(t *testing.T) {
	if bucket(0) == bucket(1) {
		t.Error("a zero-byte read shares a bucket with a one-byte read")
	}
	if bucket(1<<20) == bucket(1<<21) {
		t.Error("sizes an octave apart share a bucket")
	}
}

// TestTheEvidenceIsBoundedAndRecent covers what keeps a long-running node from
// growing without limit, and what it costs: the estimate is a moving window,
// so a backend that got faster is estimated from what it does now rather than
// from everything it ever did.
func TestTheEvidenceIsBoundedAndRecent(t *testing.T) {
	s := New()
	// Far more than the bound, all slow, then a full window of fast ones.
	record(s, Backend, 1<<20, 900*ms, samplesPerBucket*3)
	record(s, Backend, 1<<20, 10*ms, samplesPerBucket)
	record(s, Tier, 1<<20, ms, 1)

	if n := len(s.misses[bucket(1<<20)]); n != samplesPerBucket {
		t.Errorf("kept %d samples in a bucket, want the bound of %d", n, samplesPerBucket)
	}
	if want := 9 * ms; s.Estimate().Saved != want {
		t.Errorf("saved %v, want %v: the slow reads aged out", s.Estimate().Saved, want)
	}
}

// TestHitsAreCountedNotKept matters because a node that stays up serves reads
// indefinitely, and holding one duration per hit would grow without bound for
// an estimate that needs only how many and how long in total.
func TestHitsAreCountedNotKept(t *testing.T) {
	s := New()
	record(s, Backend, 1<<20, 40*ms, 4)
	record(s, Tier, 1<<20, 10*ms, 1000)

	got := s.hits[bucket(1<<20)]
	if got.n != 1000 || got.total != 10000*ms {
		t.Errorf("tally = %+v, want 1000 hits totalling 10s", got)
	}
	if want := 30000 * ms; s.Estimate().Saved != want {
		t.Errorf("saved %v, want %v", s.Estimate().Saved, want)
	}
}

// TestTheSpreadIsTheEvidencesAndNotDividedByUse matters because the spread
// qualifies the estimate: it is how wide the backend durations were, and it
// must not shrink because many hits were estimated from them. A hundred hits
// drawn from one wide bucket are a hundred wide claims, not one narrow one.
func TestTheSpreadIsTheEvidencesAndNotDividedByUse(t *testing.T) {
	s := New()
	for _, d := range []time.Duration{10 * ms, 20 * ms, 80 * ms, 110 * ms} {
		s.Record(Read{Source: Backend, Bytes: 1 << 20, Took: d})
	}
	record(s, Tier, 1<<20, ms, 100)

	// The quartiles of that bucket are 110ms and 20ms.
	if got, want := s.Estimate().Spread, 90*ms; got != want {
		t.Errorf("spread %v over 100 hits, want %v: the evidence's own spread", got, want)
	}
}

// TestTheSpreadIsWeightedByWhereTheHitsWere matters because the saving comes
// mostly from the busy bucket, so the spread that qualifies it has to as well.
func TestTheSpreadIsWeightedByWhereTheHitsWere(t *testing.T) {
	s := New()
	for _, d := range []time.Duration{40 * ms, 40 * ms, 41 * ms, 41 * ms} {
		s.Record(Read{Source: Backend, Bytes: 1 << 20, Took: d})
	}
	record(s, Tier, 1<<20, ms, 999)

	for _, d := range []time.Duration{ms, 100 * ms, 200 * ms, 300 * ms} {
		s.Record(Read{Source: Backend, Bytes: 64 << 20, Took: d})
	}
	record(s, Tier, 64<<20, ms, 1)

	// 999 hits at a 1ms spread and one at 200ms comes to well under 10ms.
	if got := s.Estimate().Spread; got > 10*ms {
		t.Errorf("spread %v: one hit in a wide bucket outweighed 999 in a tight one", got)
	}
}
