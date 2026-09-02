// Package workload runs a steady writer and reports what it achieved in each
// interval, so something done to the node while it runs can be seen in the
// intervals it overlapped.
//
// It exists for RFC-0018's question of whether reclamation measurably harms the
// job that owns the node. A workload measured only in total would average a
// stall away, which is the shape of harm being looked for.
package workload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Sample is one interval of the writer's progress.
type Sample struct {
	// Start is when the interval opened, relative to the run.
	Start time.Duration
	// Bytes is what was written in it, and Elapsed how long it really took,
	// since the last interval is short.
	Bytes   int64
	Elapsed time.Duration
	// Slowest is the longest single write in the interval. A stall shows here
	// before it shows in a rate, because one blocked write among many is a
	// small dent in a mean and the whole of a maximum.
	Slowest time.Duration
}

// Rate reports megabytes a second for this interval.
func (s Sample) Rate() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Bytes) / s.Elapsed.Seconds() / (1 << 20)
}

// Config describes the writer.
type Config struct {
	// Dir is where it writes, which must be the filesystem under test, and
	// Name separates one writer's file from another's.
	Dir  string
	Name string
	// Block is one write. Interval is how often progress is banked.
	Block    int64
	Interval time.Duration
	// Limit bounds the file, which is rewritten from the start when reached so
	// a long run does not fill the disk it is measuring on.
	Limit int64
}

// Validate rejects a configuration that would measure something else.
func (c Config) Validate() error {
	switch {
	case c.Dir == "":
		return fmt.Errorf("workload: no directory")
	case c.Block <= 0:
		return fmt.Errorf("workload: block must be positive, got %d", c.Block)
	case c.Interval <= 0:
		return fmt.Errorf("workload: interval must be positive, got %s", c.Interval)
	case c.Limit < c.Block:
		return fmt.Errorf("workload: limit %d is smaller than one %d byte block", c.Limit, c.Block)
	}
	return nil
}

// Run writes until ctx is done, banking a sample every interval.
//
// Writes bypass the page cache, so what is measured is the device the borrowed
// capacity sits on. A buffered writer would report memory bandwidth and would
// not notice a device busy with something else, which is the whole question.
func Run(ctx context.Context, c Config) ([]Sample, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	name := c.Name
	if name == "" {
		name = "workload.dat"
	}
	path := filepath.Join(c.Dir, name)
	f, err := openDirect(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		f.Close()
		os.Remove(path)
	}()

	buf, err := alignedBuffer(int(c.Block))
	if err != nil {
		return nil, err
	}
	for i := range buf {
		buf[i] = byte(i)
	}

	var (
		s  = newSampler(time.Now(), c.Interval)
		at int64
	)
	for {
		select {
		case <-ctx.Done():
			return s.close(time.Now()), nil
		default:
		}

		if at+c.Block > c.Limit {
			at = 0
		}
		w := time.Now()
		n, err := f.WriteAt(buf, at)
		done := time.Now()
		if err != nil {
			return s.close(done), fmt.Errorf("workload: writing at %d: %w", at, err)
		}
		at += int64(n)
		s.record(int64(n), done.Sub(w), done)
	}
}

// sampler banks progress into intervals. Separate from the writing so the
// bookkeeping can be exercised without a device under it.
type sampler struct {
	start    time.Time
	open     time.Time
	interval time.Duration
	cur      Sample
	samples  []Sample
}

func newSampler(start time.Time, interval time.Duration) *sampler {
	return &sampler{start: start, open: start, interval: interval}
}

// record adds one write, closing the interval when it has run long enough.
func (s *sampler) record(n int64, took time.Duration, now time.Time) {
	s.cur.Bytes += n
	if took > s.cur.Slowest {
		s.cur.Slowest = took
	}
	if elapsed := now.Sub(s.open); elapsed >= s.interval {
		s.cur.Start = s.open.Sub(s.start)
		s.cur.Elapsed = elapsed
		s.samples = append(s.samples, s.cur)
		s.cur = Sample{}
		s.open = now
	}
}

// close banks whatever the last interval had, since a run rarely ends on a
// boundary and dropping the tail loses the interval a disturbance may be in.
func (s *sampler) close(now time.Time) []Sample {
	if s.cur.Bytes > 0 {
		s.cur.Start = s.open.Sub(s.start)
		s.cur.Elapsed = now.Sub(s.open)
		s.samples = append(s.samples, s.cur)
		s.cur = Sample{}
	}
	return s.samples
}

// Between returns the samples whose intervals overlap a window, which is how a
// disturbance is compared against the run that carried it.
func Between(samples []Sample, from, to time.Duration) []Sample {
	// A window holding no time holds no intervals. Without this the control
	// arm, whose window is an instant, reports a rate for a period in which
	// nothing was done to the node.
	if from >= to {
		return nil
	}
	var out []Sample
	for _, s := range samples {
		if s.Start+s.Elapsed > from && s.Start < to {
			out = append(out, s)
		}
	}
	return out
}

// Phases splits a run into what happened before something was done to the
// node, while it was, and after it finished.
//
// Here rather than in the caller because the boundaries are the experiment: an
// interval a disturbance partly covers belongs to during, and a run that ends
// mid-interval still has an after.
func Phases(samples []Sample, from, to time.Duration) (before, during, after []Sample) {
	return Between(samples, 0, from),
		Between(samples, from, to),
		Between(samples, to, time.Duration(1<<62))
}

// MedianRate summarises a stretch of samples.
func MedianRate(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	rates := make([]float64, len(samples))
	for i, s := range samples {
		rates[i] = s.Rate()
	}
	sort.Float64s(rates)
	return rates[len(rates)/2]
}

// WorstStall reports the longest single write across a stretch.
func WorstStall(samples []Sample) time.Duration {
	var worst time.Duration
	for _, s := range samples {
		if s.Slowest > worst {
			worst = s.Slowest
		}
	}
	return worst
}
