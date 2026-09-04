// Package prefetch predicts what a reader will ask for next, and stops
// predicting when it turns out to be wrong.
//
// RFC-0011 rules out a hint API: the read path is the kernel's own NFS client
// and this project ships no client, so there is nowhere for a dataloader to
// say what it is about to read. What is declared arrives through the control
// plane before the job starts, and everything else has to be inferred from the
// reads themselves, which is what this does.
package prefetch

import (
	"fmt"
	"time"
)

// Block identifies one block of one stream, which is the unit the tier holds.
type Block struct {
	// Stream is the reader this block belongs to, per tenant and dataset, so
	// no prediction crosses a tenancy boundary.
	Stream string
	// Index is the block's position, and is what a stride is measured in.
	Index int64
}

// Config is what the detector needs, and every value in it is a guess that
// RFC-0018 owns rather than a tuned one.
type Config struct {
	// Depth is how many blocks ahead a confirmed stream is predicted.
	//
	// A depth rather than a duration: a predictor that ran ahead by a time
	// would run furthest ahead on the fastest device, which is where
	// prefetching matters least.
	Depth int
	// Confirmations is how many consecutive reads must share a stride before
	// anything is predicted, so three means two intervals that agree. Two
	// reads always have a stride, so predicting from them is predicting from
	// noise.
	Confirmations int
	// Window is how many recent predictions a stream's accuracy is measured
	// over.
	Window int
	// MinAccuracy is the share of recent predictions that must have been read
	// for a stream to keep being predicted.
	MinAccuracy float64
	// Patience is how long a prediction waits to be read before it counts as
	// wrong. Without it a stream that stopped reading would hold its accuracy
	// at whatever it was when it stopped.
	Patience time.Duration
}

// Validate refuses a configuration that would predict nothing or never stop.
func (c Config) Validate() error {
	switch {
	case c.Depth <= 0:
		return fmt.Errorf("prefetch: depth must be positive, got %d", c.Depth)
	case c.Confirmations < 3:
		return fmt.Errorf("prefetch: %d confirmations predicts from noise, since two reads always have a stride and three is the first that can agree", c.Confirmations)
	case c.Window <= 0:
		return fmt.Errorf("prefetch: window must be positive, got %d", c.Window)
	case c.MinAccuracy < 0 || c.MinAccuracy > 1:
		return fmt.Errorf("prefetch: accuracy floor %v is not a share", c.MinAccuracy)
	case c.Patience <= 0:
		return fmt.Errorf("prefetch: patience must be positive, or a stream that stopped reading keeps its accuracy forever")
	}
	return nil
}

// DefaultConfig is a conservative starting point, and every number in it is a
// guess RFC-0018 owns.
func DefaultConfig() Config {
	return Config{Depth: 8, Confirmations: 3, Window: 64, MinAccuracy: 0.5, Patience: time.Minute}
}

// stream is what is known about one reader.
type stream struct {
	last   int64
	stride int64
	// runs counts intervals that agreed with the current stride, so a run of
	// runs+2 reads has been seen.
	runs    int
	started bool

	// outstanding maps a predicted index to when it was predicted, so a
	// prediction can be judged read, or judged wrong once patience runs out.
	outstanding map[int64]time.Time
	// recent is the accuracy window, oldest first: true for a prediction that
	// was read.
	recent []bool
	// muted says the stream failed its accuracy floor and is not predicted.
	// mutedStride is the stride it failed on, and only a different one lifts
	// it: the stride that failed continuing is not evidence that it started
	// paying, and treating it as evidence makes the stream oscillate between
	// muted and predicting forever.
	muted       bool
	mutedStride int64
}

// Detector watches read streams and says what to fetch next.
//
// Not safe for concurrent use. It sits on the read path, which serialises per
// reader, and a lock here would be contention for a prediction.
type Detector struct {
	cfg     Config
	streams map[string]*stream
}

// New returns a detector, refusing a configuration that cannot work.
func New(cfg Config) (*Detector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Detector{cfg: cfg, streams: map[string]*stream{}}, nil
}

// Observe records a read and returns the blocks worth fetching next.
//
// Nothing is returned until the stride has confirmed, and nothing is returned
// for a stream whose recent predictions went unread.
func (d *Detector) Observe(b Block, now time.Time) []Block {
	s := d.streams[b.Stream]
	if s == nil {
		s = &stream{outstanding: map[int64]time.Time{}}
		d.streams[b.Stream] = s
	}

	d.score(s, b.Index, now)
	d.track(s, b.Index, now)

	if !s.started || s.runs+2 < d.cfg.Confirmations || s.muted {
		return nil
	}
	out := make([]Block, 0, d.cfg.Depth)
	for i := 1; i <= d.cfg.Depth; i++ {
		next := b.Index + s.stride*int64(i)
		if _, already := s.outstanding[next]; already {
			continue
		}
		s.outstanding[next] = now
		out = append(out, Block{Stream: b.Stream, Index: next})
	}
	return out
}

// track advances the stride, and resets the run when the stride changes.
//
// A muted stream is unmuted here rather than by its accuracy recovering,
// because a muted stream is not predicted and so produces no evidence that
// could recover it. Only a stride different from the one it failed on lifts
// the mute: a stream reading steadily whose predictions go unread would
// otherwise unmute on its very next reads and mute again, forever.
func (d *Detector) track(s *stream, index int64, now time.Time) {
	if !s.started {
		s.last, s.started = index, true
		return
	}
	stride := index - s.last
	s.last = index
	if stride == 0 {
		// A repeated block says nothing about direction, so it neither
		// confirms the stride nor breaks it.
		return
	}
	if stride == s.stride {
		s.runs++
		if s.muted && stride != s.mutedStride && s.runs+2 >= d.cfg.Confirmations {
			s.muted = false
			s.recent = s.recent[:0]
		}
		return
	}
	s.stride, s.runs = stride, 0
}

// score judges outstanding predictions: this read confirms one, and patience
// running out condemns the rest.
func (d *Detector) score(s *stream, index int64, now time.Time) {
	if _, predicted := s.outstanding[index]; predicted {
		delete(s.outstanding, index)
		d.record(s, true)
	}
	for at, when := range s.outstanding {
		if now.Sub(when) >= d.cfg.Patience {
			delete(s.outstanding, at)
			d.record(s, false)
		}
	}
}

// record adds one judged prediction and mutes the stream if too few of the
// recent ones were read.
func (d *Detector) record(s *stream, read bool) {
	s.recent = append(s.recent, read)
	if len(s.recent) > d.cfg.Window {
		s.recent = s.recent[len(s.recent)-d.cfg.Window:]
	}
	// Judged only on a full window. A stream muted on its first few
	// predictions would be muted before it had said anything.
	if len(s.recent) < d.cfg.Window {
		return
	}
	// Only on the transition. One read expires many outstanding predictions,
	// so this runs several times in a row, and re-muting an already muted
	// stream would record the stride it has since been reset to rather than
	// the one that failed.
	if !s.muted && accuracy(s.recent) < d.cfg.MinAccuracy {
		s.muted, s.mutedStride = true, s.stride
		s.outstanding = map[int64]time.Time{}
		// The run is reset with it, so the stream has to establish a pattern
		// again rather than resume mid-stride.
		s.stride, s.runs = 0, 0
	}
}

// Accuracy reports the share of one stream's recent predictions that were
// read, and whether there is a stream to report on.
func (d *Detector) Accuracy(name string) (float64, bool) {
	s := d.streams[name]
	if s == nil || len(s.recent) == 0 {
		return 0, false
	}
	return accuracy(s.recent), true
}

// Predicting reports whether a stream is currently being predicted, which is
// what an operator asking why nothing is being fetched needs to know.
func (d *Detector) Predicting(name string) bool {
	s := d.streams[name]
	return s != nil && s.started && !s.muted && s.runs+2 >= d.cfg.Confirmations
}

// Forget drops a stream, for a reader that has gone away.
func (d *Detector) Forget(name string) { delete(d.streams, name) }

// accuracy is the share of judged predictions that were read.
func accuracy(recent []bool) float64 {
	var read int
	for _, ok := range recent {
		if ok {
			read++
		}
	}
	return float64(read) / float64(len(recent))
}
