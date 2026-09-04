package kvspill

import (
	"strings"
	"testing"
	"time"
)

// fast is a node where reading can win: the device moves a token's block far
// faster than the accelerator recomputes it, so only the fixed read latency
// has to be paid off.
func fast() Cost {
	return Cost{
		PrefillTokensPerSecond: 1000,
		ReadLatency:            time.Millisecond,
		ReadBytesPerSecond:     4 << 30,
		BytesPerToken:          128 << 10,
	}
}

func gate(t *testing.T, c Cost) *Gate {
	t.Helper()
	g, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestAShortPrefixIsNotWorthReading is the rule RFC-0001 turns into a refusal:
// below the crossing, fetching is strictly worse than not having tried.
func TestAShortPrefixIsNotWorthReading(t *testing.T) {
	g := gate(t, fast())
	n, ok := g.BreakEven()
	if !ok {
		t.Fatal("no break-even on a node where reading is far faster than recompute")
	}

	if g.Worth(n - 1) {
		t.Errorf("a %d token prefix was judged worth reading, one below the break-even of %d", n-1, n)
	}
	if !g.Worth(n) {
		t.Errorf("the break-even prefix of %d tokens was refused", n)
	}
	if !g.Worth(n * 10) {
		t.Error("a prefix well above the break-even was refused")
	}
}

// TestTheBreakEvenIsWhereTheCostsActuallyCross checks the arithmetic against
// the two cost functions rather than against a number written twice.
func TestTheBreakEvenIsWhereTheCostsActuallyCross(t *testing.T) {
	c := fast()
	g := gate(t, c)
	n, ok := g.BreakEven()
	if !ok {
		t.Fatal("no break-even")
	}

	if c.Read(n) > c.Recompute(n) {
		t.Errorf("at the break-even of %d tokens, reading takes %s against %s to recompute",
			n, c.Read(n), c.Recompute(n))
	}
	if n > 1 && c.Read(n-1) <= c.Recompute(n-1) {
		t.Errorf("one token below the break-even, reading already won: %s against %s",
			c.Read(n-1), c.Recompute(n-1))
	}
}

// TestSomeHardwareNeverPays is a real outcome rather than a defensive branch:
// if the transfer is slower per token than the accelerator recomputes, the two
// lines never meet however long the prefix.
func TestSomeHardwareNeverPays(t *testing.T) {
	c := fast()
	// A device an order of magnitude slower than the accelerator, per token.
	c.ReadBytesPerSecond = c.BytesPerToken * c.PrefillTokensPerSecond / 10

	g := gate(t, c)
	if _, ok := g.BreakEven(); ok {
		t.Fatal("a node where reading always loses reported a break-even")
	}
	for _, tokens := range []int{1, 1000, 1 << 20} {
		if g.Worth(tokens) {
			t.Errorf("a %d token prefix was judged worth reading on hardware that never pays", tokens)
		}
	}
	if !strings.Contains(g.Explain(1000), "never beats recomputing") {
		t.Errorf("the explanation does not say it never pays: %s", g.Explain(1000))
	}
}

// TestBreakingEvenExactlyAtParityDoesNotCount covers the boundary the
// arithmetic sits on: a per-token difference of zero is not a crossing that
// arrives later, it is no crossing at all.
func TestBreakingEvenExactlyAtParityDoesNotCount(t *testing.T) {
	c := fast()
	c.ReadBytesPerSecond = c.BytesPerToken * c.PrefillTokensPerSecond

	if _, ok := gate(t, c).BreakEven(); ok {
		t.Error("a node where the two costs rise at the same rate reported a break-even")
	}
}

// TestACrossingTooLongToReachIsNoCrossing keeps a threshold nobody will ever
// hit from reading as a usable one.
func TestACrossingTooLongToReachIsNoCrossing(t *testing.T) {
	c := fast()
	// Barely faster per token, with an enormous fixed latency: the lines do
	// cross, at a prefix no request will ever have.
	c.ReadBytesPerSecond = c.BytesPerToken * c.PrefillTokensPerSecond * 1.0000001
	c.ReadLatency = time.Hour

	if _, ok := gate(t, c).BreakEven(); ok {
		t.Error("a crossing beyond any real prefix was reported as a break-even")
	}
}

// TestNoLatencyStillNeedsAToken keeps a zero-latency device from producing a
// break-even of zero, since one token is the shortest prefix there is.
func TestNoLatencyStillNeedsAToken(t *testing.T) {
	c := fast()
	c.ReadLatency = 0

	n, ok := gate(t, c).BreakEven()
	if !ok {
		t.Fatal("no break-even on a device with no latency")
	}
	if n != 1 {
		t.Errorf("break-even = %d tokens with no read latency, want 1", n)
	}
}

// TestCostsThatDescribeNoContestAreRefused covers the inputs that would make
// the arithmetic produce a confident answer from nothing.
func TestCostsThatDescribeNoContestAreRefused(t *testing.T) {
	if err := fast().Validate(); err != nil {
		t.Fatalf("a usable cost was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*Cost)
	}{
		{"no prefill rate", func(c *Cost) { c.PrefillTokensPerSecond = 0 }},
		{"a negative prefill rate", func(c *Cost) { c.PrefillTokensPerSecond = -1 }},
		{"no read rate", func(c *Cost) { c.ReadBytesPerSecond = 0 }},
		{"a token with no bytes", func(c *Cost) { c.BytesPerToken = 0 }},
		{"negative latency", func(c *Cost) { c.ReadLatency = -time.Second }},
	} {
		cost := fast()
		c.edit(&cost)
		if err := cost.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
		if _, err := New(cost); err == nil {
			t.Errorf("%s built a gate", c.name)
		}
	}
}

// TestTheRefusalSaysWhichWayItWent matters because a spill path that appears
// to be doing nothing has two very different causes, and an operator needs to
// know which.
func TestTheRefusalSaysWhichWayItWent(t *testing.T) {
	g := gate(t, fast())
	n, _ := g.BreakEven()

	below := g.Explain(n - 1)
	if !strings.Contains(below, "where that turns") {
		t.Errorf("a refusal below the break-even does not say where it turns: %s", below)
	}
	if strings.Contains(below, "never beats") {
		t.Errorf("a prefix merely too short was reported as hardware that never pays: %s", below)
	}
	if above := g.Explain(n * 4); strings.Contains(above, "where that turns") {
		t.Errorf("a prefix worth reading was explained as a refusal: %s", above)
	}
}
