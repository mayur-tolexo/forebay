// Package efficiency answers whether the fast tier saved anyone time, and
// refuses to answer more than it knows.
//
// RFC-0024 defines the scoreboard. The hard part is not the arithmetic: the
// output ends up on a slide, and a number on a slide loses the sentence that
// qualified it. So a saving is signed, carries the spread of the estimate it
// came from, and never becomes an accelerator hour or a currency without an
// operator supplying the conversion.
package efficiency

import (
	"fmt"
	"math/bits"
	"sort"
	"time"
)

// Source says where a read was served from, which is what makes one read
// evidence and another the thing being estimated.
type Source int

const (
	// Tier was served from borrowed capacity on this node.
	Tier Source = iota
	// Backend went to the durable store, and is what a tier hit is measured
	// against.
	Backend
)

// Read is one read the node served.
type Read struct {
	Source Source
	Bytes  int64
	Took   time.Duration
}

// bucket puts a read in a power-of-two size bucket.
//
// A backend read costs a round trip plus a transfer, and both scale with size
// while neither depends on which object it was, so size is the variable that
// has to match. Bucketing at record time is what makes the estimate cheap, and
// it is also what makes a change to this function a break in comparability
// rather than a tuning.
func bucket(n int64) int {
	if n <= 0 {
		return 0
	}
	return bits.Len64(uint64(n))
}

// samplesPerBucket bounds the backend durations kept for one size.
//
// It makes the estimate a moving window rather than a history, which is what
// the counterfactual wanted anyway: what a read would cost from the backend
// now is better answered by recent misses than by every miss the node has ever
// served.
const samplesPerBucket = 256

// tally is what tier hits in one bucket amount to.
//
// Counted rather than kept. The estimate needs how many there were and how
// long they took in total, and holding each one would grow without bound on a
// node that stays up.
type tally struct {
	n     int
	total time.Duration
}

// Scoreboard accumulates reads and estimates what the tier saved.
//
// Not safe for concurrent use. The read path serialises its own recording, and
// a lock here would be on the hot path for a number nobody reads per request.
type Scoreboard struct {
	// misses holds recent backend durations per bucket, which are the evidence
	// the counterfactual is drawn from. Bounded, and oldest first.
	misses map[int][]time.Duration
	// hits counts what the tier served per bucket, which is what is being
	// estimated.
	hits map[int]tally
}

// New returns an empty scoreboard.
func New() *Scoreboard {
	return &Scoreboard{misses: map[int][]time.Duration{}, hits: map[int]tally{}}
}

// Record adds one read.
func (s *Scoreboard) Record(r Read) {
	b := bucket(r.Bytes)
	if r.Source == Backend {
		kept := append(s.misses[b], r.Took)
		if len(kept) > samplesPerBucket {
			kept = kept[len(kept)-samplesPerBucket:]
		}
		s.misses[b] = kept
		return
	}
	t := s.hits[b]
	t.n, t.total = t.n+1, t.total+r.Took
	s.hits[b] = t
}

// Estimate is what the tier saved, and what that claim rests on.
type Estimate struct {
	// Saved is signed. The tier is sometimes slower than the backend, and this
	// project has measured that on its own hardware, so a loss is reported in
	// the same place and the same way as a gain.
	Saved time.Duration
	// Spread is the interquartile range of the backend durations the estimate
	// was drawn from, weighted the same way the estimate was. An estimate from
	// a bucket ranging over two orders of magnitude is a different claim from
	// one drawn from a tight bucket, and a reader is owed both.
	Spread time.Duration
	// Covered and Uncovered count tier hits that could and could not be
	// estimated. A hit in a bucket with no misses in it is not extrapolated
	// across buckets, because a scoreboard that did that would be inventing
	// the number it exists to defend.
	Covered, Uncovered int
}

// CoveredFraction is the share of tier hits the estimate actually rests on.
// Zero hits is reported as zero rather than as complete coverage.
func (e Estimate) CoveredFraction() float64 {
	total := e.Covered + e.Uncovered
	if total == 0 {
		return 0
	}
	return float64(e.Covered) / float64(total)
}

// Estimate reports what the tier saved, from the node's own misses.
//
// Summed in durations rather than floats, so the answer does not depend on the
// order the buckets happened to be walked in.
func (s *Scoreboard) Estimate() Estimate {
	var e Estimate
	var spreadWeighted time.Duration

	for b, hits := range s.hits {
		misses := s.misses[b]
		if len(misses) == 0 {
			e.Uncovered += hits.n
			continue
		}
		e.Saved += median(misses)*time.Duration(hits.n) - hits.total
		spreadWeighted += interquartile(misses) * time.Duration(hits.n)
		e.Covered += hits.n
	}
	if e.Covered > 0 {
		e.Spread = spreadWeighted / time.Duration(e.Covered)
	}
	return e
}

// median returns the middle of a set of durations, without disturbing the
// caller's slice: the reservoirs are appended to after this is called.
func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// interquartile returns the spread between the quartiles, which is the
// dispersion a median hides.
//
// For a handful of samples the quartiles fall at or near the ends, so the
// spread reported is close to the full range. That overstates dispersion,
// which is the direction to be wrong in: it makes a thinly evidenced estimate
// look thinly evidenced.
func interquartile(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)*3/4] - s[len(s)/4]
}

// Conversion is what only an operator can supply.
//
// Both are declared rather than measured, and a report says so on the same
// line as the number, because Forebay sees a socket and not a training step: a
// read that took two seconds while the accelerator worked on the previous
// batch cost nobody anything, and nothing on the data path can tell it from
// one that stalled a step.
type Conversion struct {
	// Accelerators is how many an affected reader feeds, declared by the
	// operator along with the claim that the reader is on the critical path.
	Accelerators float64
	// PricePerHour is what one accelerator hour costs. Zero means the operator
	// gave none, and no money is reported.
	PricePerHour float64
}

// ErrNoConversion reports a request for accelerator hours or money that the
// operator supplied no basis for. There is no default: a price Forebay
// invented, appearing in a currency, is worse than no number at all.
var ErrNoConversion = fmt.Errorf("efficiency: no operator-declared conversion, so only seconds can be reported")

// AcceleratorHours converts a saving using what the operator declared.
func (e Estimate) AcceleratorHours(c Conversion) (float64, error) {
	if c.Accelerators <= 0 {
		return 0, ErrNoConversion
	}
	return e.Saved.Hours() * c.Accelerators, nil
}

// Money converts a saving into the operator's own currency.
func (e Estimate) Money(c Conversion) (float64, error) {
	if c.PricePerHour <= 0 {
		return 0, ErrNoConversion
	}
	hours, err := e.AcceleratorHours(c)
	if err != nil {
		return 0, err
	}
	return hours * c.PricePerHour, nil
}
