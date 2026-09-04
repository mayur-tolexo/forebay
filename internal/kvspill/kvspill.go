// Package kvspill decides whether reading a KV block back beats recomputing
// it, and refuses every fetch that would not.
//
// RFC-0027's crossover is the tightest in this project: everywhere else the
// fast tier competes with a fanned-out object store, and here it competes with
// an accelerator recomputing a prefill. Below some prefix length fetching is
// strictly worse than not having tried, and RFC-0001's rule that a node with
// Forebay is never worse off than one without makes that a refusal rather than
// a preference.
package kvspill

import (
	"fmt"
	"math"
	"time"
)

// Cost is what each side of the contest costs on one node. Every field is
// measured on the node it describes rather than configured, because the answer
// is a property of that hardware and a shipped constant would be a guess about
// somebody else's.
type Cost struct {
	// PrefillTokensPerSecond is how fast this accelerator recomputes a prefix.
	PrefillTokensPerSecond float64
	// ReadLatency is what a read costs before any bytes move, which is what
	// makes short prefixes lose however fast the device is.
	ReadLatency time.Duration
	// ReadBytesPerSecond is how fast a block comes back.
	ReadBytesPerSecond float64
	// BytesPerToken is how large a KV block is per token. Set by the engine's
	// model and page size rather than by Forebay.
	BytesPerToken float64
}

// Validate refuses costs that cannot describe a contest.
func (c Cost) Validate() error {
	switch {
	case c.PrefillTokensPerSecond <= 0:
		return fmt.Errorf("kvspill: prefill rate must be positive, got %v tokens/s", c.PrefillTokensPerSecond)
	case c.ReadBytesPerSecond <= 0:
		return fmt.Errorf("kvspill: read rate must be positive, got %v bytes/s", c.ReadBytesPerSecond)
	case c.BytesPerToken <= 0:
		return fmt.Errorf("kvspill: a token's KV block must be some bytes, got %v", c.BytesPerToken)
	case c.ReadLatency < 0:
		return fmt.Errorf("kvspill: read latency must not be negative, got %s", c.ReadLatency)
	}
	return nil
}

// Recompute is what regenerating a prefix of this many tokens costs.
func (c Cost) Recompute(tokens int) time.Duration {
	return time.Duration(float64(tokens) / c.PrefillTokensPerSecond * float64(time.Second))
}

// Read is what fetching that prefix back costs, latency included.
func (c Cost) Read(tokens int) time.Duration {
	transfer := float64(tokens) * c.BytesPerToken / c.ReadBytesPerSecond
	return c.ReadLatency + time.Duration(transfer*float64(time.Second))
}

// Gate answers whether a fetch is worth making.
type Gate struct {
	cost Cost
	// breakEven is the shortest prefix for which reading wins, and exists says
	// whether any length does. On hardware where the transfer is slower per
	// token than the accelerator recomputes, the two lines never meet.
	breakEven int
	exists    bool
}

// New builds a gate from measured costs.
func New(c Cost) (*Gate, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	g := &Gate{cost: c}
	g.breakEven, g.exists = breakEven(c)
	return g, nil
}

// breakEven solves for the prefix length where reading stops losing.
//
// Per token, recomputing costs 1/prefill seconds and transferring costs
// bytes/rate seconds. The fixed read latency is paid once, so reading catches
// up only if the per-token difference is positive, and the crossing is that
// latency divided by the difference.
func breakEven(c Cost) (int, bool) {
	perToken := 1/c.PrefillTokensPerSecond - c.BytesPerToken/c.ReadBytesPerSecond
	if perToken <= 0 {
		// The transfer is at least as slow per token as recomputing, so no
		// prefix is long enough. A real outcome on plausible hardware.
		return 0, false
	}
	tokens := c.ReadLatency.Seconds() / perToken
	if tokens > math.MaxInt32 {
		// Long enough to be no answer. Reported as no crossing rather than as
		// a number nobody will reach, which would read as a usable threshold.
		return 0, false
	}
	// Ceiling, because the crossing usually falls between two tokens and the
	// token below it still loses.
	n := int(math.Ceil(tokens))
	if n < 1 {
		// Zero latency puts the crossing at the origin, and one token is the
		// shortest prefix there is to fetch.
		n = 1
	}
	return n, true
}

// BreakEven reports the shortest prefix worth fetching, and whether any length
// is. A gate that reports false refuses everything, which is the honest
// outcome rather than a fetch that is always slightly worse.
func (g *Gate) BreakEven() (int, bool) { return g.breakEven, g.exists }

// Worth reports whether fetching a prefix of this many tokens beats
// recomputing it.
func (g *Gate) Worth(tokens int) bool {
	return g.exists && tokens >= g.breakEven
}

// Explain says why, in the words an operator needs when a spill path appears
// to be doing nothing.
func (g *Gate) Explain(tokens int) string {
	if !g.exists {
		return fmt.Sprintf("reading never beats recomputing on this node: %v bytes per token at %v bytes/s "+
			"is slower than prefill at %v tokens/s, so no prefix is long enough",
			g.cost.BytesPerToken, g.cost.ReadBytesPerSecond, g.cost.PrefillTokensPerSecond)
	}
	if !g.Worth(tokens) {
		return fmt.Sprintf("a %d token prefix reads in %s against %s to recompute, and %d tokens is where that turns",
			tokens, g.cost.Read(tokens).Round(time.Microsecond),
			g.cost.Recompute(tokens).Round(time.Microsecond), g.breakEven)
	}
	return fmt.Sprintf("a %d token prefix reads in %s against %s to recompute",
		tokens, g.cost.Read(tokens).Round(time.Microsecond), g.cost.Recompute(tokens).Round(time.Microsecond))
}
