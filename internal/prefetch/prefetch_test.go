package prefetch

import (
	"testing"
	"time"
)

var t0 = time.Unix(1000, 0)

// detector builds one, failing the test on a configuration it refuses.
func detector(t *testing.T, cfg Config) *Detector {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestNothingIsPredictedUntilTheStrideConfirms is the rule that separates a
// pattern from a coincidence: two reads always have a stride.
func TestNothingIsPredictedUntilTheStrideConfirms(t *testing.T) {
	d := detector(t, DefaultConfig())

	for i, index := range []int64{0, 1} {
		if got := d.Observe(Block{Stream: "s", Index: index}, t0); len(got) != 0 {
			t.Errorf("read %d predicted %d blocks before the stride confirmed", i, len(got))
		}
	}
	got := d.Observe(Block{Stream: "s", Index: 2}, t0)
	if len(got) != DefaultConfig().Depth {
		t.Fatalf("a confirmed stride predicted %d blocks, want %d", len(got), DefaultConfig().Depth)
	}
	if got[0].Index != 3 || got[len(got)-1].Index != 10 {
		t.Errorf("predicted %v..%v, want 3..10", got[0].Index, got[len(got)-1].Index)
	}
}

// TestAStrideOtherThanOneIsFollowed covers the strided case, which is what a
// sharded dataloader reading every nth block looks like.
func TestAStrideOtherThanOneIsFollowed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Depth = 3
	d := detector(t, cfg)

	d.Observe(Block{Stream: "s", Index: 100}, t0)
	d.Observe(Block{Stream: "s", Index: 104}, t0)
	got := d.Observe(Block{Stream: "s", Index: 108}, t0)

	want := []int64{112, 116, 120}
	if len(got) != len(want) {
		t.Fatalf("predicted %d blocks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Index != want[i] {
			t.Errorf("prediction %d is %d, want %d", i, got[i].Index, want[i])
		}
	}
}

// TestABackwardStrideIsAStride matters because a loader walking a shard in
// reverse is as predictable as one walking it forward.
func TestABackwardStrideIsAStride(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Depth = 2
	d := detector(t, cfg)

	d.Observe(Block{Stream: "s", Index: 50}, t0)
	d.Observe(Block{Stream: "s", Index: 49}, t0)
	got := d.Observe(Block{Stream: "s", Index: 48}, t0)

	if len(got) != 2 || got[0].Index != 47 || got[1].Index != 46 {
		t.Errorf("predicted %v, want 47 and 46", got)
	}
}

// TestARandomStreamIsInert is the intended failure: prefetch predicts nothing
// rather than predicting wrongly.
func TestARandomStreamIsInert(t *testing.T) {
	d := detector(t, DefaultConfig())
	for _, index := range []int64{9, 2, 44, 7, 900, 13, 6} {
		if got := d.Observe(Block{Stream: "s", Index: index}, t0); len(got) != 0 {
			t.Errorf("a random stream predicted %v", got)
		}
	}
	if d.Predicting("s") {
		t.Error("a random stream is being predicted")
	}
}

// TestARepeatedBlockNeitherConfirmsNorBreaks covers the read that says nothing
// about direction, which would otherwise reset a healthy stride.
func TestARepeatedBlockNeitherConfirmsNorBreaks(t *testing.T) {
	d := detector(t, DefaultConfig())
	d.Observe(Block{Stream: "s", Index: 0}, t0)
	d.Observe(Block{Stream: "s", Index: 1}, t0)
	d.Observe(Block{Stream: "s", Index: 2}, t0)
	if !d.Predicting("s") {
		t.Fatal("a confirmed stream was not predicting")
	}

	d.Observe(Block{Stream: "s", Index: 2}, t0)
	if !d.Predicting("s") {
		t.Error("re-reading the same block stopped the stream being predicted")
	}
}

// TestStreamsAreIndependent matters because a stream is per tenant and per
// dataset, so one reader's randomness must not stop another being predicted.
func TestStreamsAreIndependent(t *testing.T) {
	d := detector(t, DefaultConfig())
	for i := int64(0); i < 3; i++ {
		d.Observe(Block{Stream: "tidy", Index: i}, t0)
		d.Observe(Block{Stream: "messy", Index: i * i * 31}, t0)
	}
	if !d.Predicting("tidy") {
		t.Error("a sequential stream was not predicted")
	}
	if d.Predicting("messy") {
		t.Error("a random stream was predicted")
	}
}

// TestAPredictionThatIsReadCountsAsRead is the accuracy signal the detector
// acts on, and the number RFC-0011 says falsifies the feature.
func TestAPredictionThatIsReadCountsAsRead(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Window = 4
	d := detector(t, cfg)

	// Confirm, then read exactly what was predicted.
	for i := int64(0); i < 10; i++ {
		d.Observe(Block{Stream: "s", Index: i}, t0)
	}
	got, ok := d.Accuracy("s")
	if !ok {
		t.Fatal("no accuracy for a stream that had predictions judged")
	}
	if got != 1 {
		t.Errorf("accuracy %v for a stream that read every prediction, want 1", got)
	}
}

// TestAStreamThatStopsBeingReadStopsBeingPredicted is the limit on a wrong
// prediction at volume: it costs bandwidth, so it must not continue.
func TestAStreamThatStopsBeingReadStopsBeingPredicted(t *testing.T) {
	cfg := Config{Depth: 4, Confirmations: 3, Window: 4, MinAccuracy: 0.5, Patience: time.Second}
	d := detector(t, cfg)

	for i := int64(0); i < 3; i++ {
		d.Observe(Block{Stream: "s", Index: i}, t0)
	}
	if !d.Predicting("s") {
		t.Fatal("a confirmed stream was not predicting")
	}

	// Jump somewhere unpredicted, long enough later that patience has run out
	// on everything outstanding.
	late := t0.Add(2 * time.Second)
	d.Observe(Block{Stream: "s", Index: 5000}, late)

	if d.Predicting("s") {
		t.Error("a stream whose predictions all went unread is still being predicted")
	}
	if got, _ := d.Accuracy("s"); got != 0 {
		t.Errorf("accuracy %v after nothing predicted was read, want 0", got)
	}
}

// walkFrom reads n blocks at a fixed stride, advancing the clock a
// millisecond a read, and returns where it left off.
func walkFrom(d *Detector, at time.Time, index, stride int64, n int) (time.Time, int64) {
	for i := 0; i < n; i++ {
		d.Observe(Block{Stream: "s", Index: index}, at)
		index += stride
		at = at.Add(time.Millisecond)
	}
	return at, index
}

// TestAMutedStreamResumesOnlyOnADifferentStride is the rule that stops a
// stream oscillating. A steady reader whose predictions go unread would
// otherwise unmute on its very next reads, mute again, and waste bandwidth
// forever, because the stride that failed continuing is not evidence that it
// started paying.
func TestAMutedStreamResumesOnlyOnADifferentStride(t *testing.T) {
	// Patience so short that everything beyond the very next block ages out
	// unread, which is what drives accuracy under the floor.
	cfg := Config{Depth: 8, Confirmations: 3, Window: 20, MinAccuracy: 0.9, Patience: time.Nanosecond}
	d := detector(t, cfg)

	at, index := walkFrom(d, t0, 0, 1, 20)
	if d.Predicting("s") {
		t.Fatal("a stream far under the accuracy floor is still being predicted")
	}

	// The same stride, at length, must not bring it back.
	at, _ = walkFrom(d, at, index, 1, 40)
	if d.Predicting("s") {
		t.Error("the stride that failed brought the stream back by continuing")
	}

	// A different one does, since the pattern has actually changed. Four reads:
	// one to break the old stride, then three to establish and confirm the new.
	walkFrom(d, at, 5000, 4, 4)
	if !d.Predicting("s") {
		t.Error("a stream that took up a different stride is still muted")
	}
}

// TestAPredictionIsNotRepeated keeps a stream being walked steadily from
// re-issuing the same blocks on every read.
func TestAPredictionIsNotRepeated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Depth = 4
	d := detector(t, cfg)

	for i := int64(0); i < 3; i++ {
		d.Observe(Block{Stream: "s", Index: i}, t0)
	}
	// The next read predicts one new block: the other three are outstanding.
	got := d.Observe(Block{Stream: "s", Index: 3}, t0)
	if len(got) != 1 || got[0].Index != 7 {
		t.Errorf("predicted %v, want only the one block not already outstanding", got)
	}
}

// TestForgetDropsAReaderThatWentAway keeps a detector on a long-lived node
// from holding state for every stream it ever saw.
func TestForgetDropsAReaderThatWentAway(t *testing.T) {
	d := detector(t, DefaultConfig())
	for i := int64(0); i < 3; i++ {
		d.Observe(Block{Stream: "s", Index: i}, t0)
	}
	d.Forget("s")
	if d.Predicting("s") {
		t.Error("a forgotten stream is still being predicted")
	}
	if _, ok := d.Accuracy("s"); ok {
		t.Error("a forgotten stream still reports accuracy")
	}
}

// TestAConfigurationThatCannotWorkIsRefused covers the values that would make
// the detector predict from noise or never give up.
func TestAConfigurationThatCannotWorkIsRefused(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the default configuration was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*Config)
	}{
		{"no depth", func(c *Config) { c.Depth = 0 }},
		{"two confirmations, which is noise", func(c *Config) { c.Confirmations = 2 }},
		{"no window", func(c *Config) { c.Window = 0 }},
		{"an accuracy floor above one", func(c *Config) { c.MinAccuracy = 1.5 }},
		{"a negative accuracy floor", func(c *Config) { c.MinAccuracy = -0.1 }},
		{"no patience", func(c *Config) { c.Patience = 0 }},
	} {
		cfg := DefaultConfig()
		c.edit(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
		if _, err := New(cfg); err == nil {
			t.Errorf("%s built a detector", c.name)
		}
	}
}

// TestAccuracyIsJudgedOnAFullWindow keeps a stream from being muted before it
// has said enough to be judged on. The stride is held steady throughout, so
// the only thing that can stop the stream being predicted is the accuracy
// floor, which is what this is about.
func TestAccuracyIsJudgedOnAFullWindow(t *testing.T) {
	// Patience so short that every prediction beyond the very next block ages
	// out unread, which drives accuracy far under the floor.
	cfg := Config{Depth: 8, Confirmations: 3, Window: 50, MinAccuracy: 0.9, Patience: time.Nanosecond}
	d := detector(t, cfg)

	walk := func(d *Detector, from, to int64) {
		for i := from; i < to; i++ {
			d.Observe(Block{Stream: "s", Index: i}, t0.Add(time.Duration(i)*time.Millisecond))
		}
	}

	walk(d, 0, 6)
	if !d.Predicting("s") {
		t.Fatal("a stream with a steady stride was muted before its window filled")
	}
	if got, _ := d.Accuracy("s"); got >= cfg.MinAccuracy {
		t.Fatalf("accuracy %v is not yet under the floor, so this proves nothing", got)
	}

	// Far past a full window of judgements, the floor applies.
	walk(d, 6, 200)
	if d.Predicting("s") {
		t.Error("a stream well under the accuracy floor is still being predicted")
	}
}

// TestAccuracyIsRecentAndNotHistorical matters twice over. A stream that was
// accurate for an hour and has just stopped being must be given up on now
// rather than after its history has been outweighed, and the record it is
// judged from must not grow for as long as the reader lives.
func TestAccuracyIsRecentAndNotHistorical(t *testing.T) {
	// Patience long enough that a sequential reader reaches its own
	// predictions, so the long run really is accurate.
	cfg := Config{Depth: 8, Confirmations: 3, Window: 10, MinAccuracy: 0.5, Patience: 10 * time.Millisecond}
	d := detector(t, cfg)

	at, index := walkFrom(d, t0, 0, 1, 100)
	if got, _ := d.Accuracy("s"); got != 1 {
		t.Fatalf("accuracy %v over a run that read every prediction, want 1", got)
	}

	// The reader leaves. Everything outstanding ages out unread.
	d.Observe(Block{Stream: "s", Index: index + 5000}, at.Add(time.Second))

	got, _ := d.Accuracy("s")
	if got >= cfg.MinAccuracy {
		t.Errorf("accuracy %v after every outstanding prediction went unread: a long good history diluted the recent failure", got)
	}
}
