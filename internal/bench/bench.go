// Package bench measures one arm of a comparison against another.
//
// It exists for the crossover experiment in RFC-0018: where node-local
// bandwidth stops beating a node's achievable share of backend fan-out. The
// rule that document imposes is that both arms do the same work, so the plan
// is built once and every arm executes that same plan.
package bench

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"
)

// Reader is one way of getting a byte range, so an arm is a Reader and the
// measurement does not know which one it is holding.
type Reader interface {
	ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error)
}

// Plan is the work every arm performs, identically.
type Plan struct {
	// Object and Size name what is read, in full, once per run.
	Object string
	Size   int64
	// Block is the request size. Both arms use it, since a comparison at
	// different request sizes is not a comparison of locality.
	Block int64
	// Workers is how many readers ask at once.
	Workers int
}

// Validate rejects a plan that cannot be executed the same way twice.
func (p Plan) Validate() error {
	switch {
	case p.Object == "":
		return errors.New("bench: no object")
	case p.Size <= 0:
		return fmt.Errorf("bench: size must be positive, got %d", p.Size)
	case p.Block <= 0:
		return fmt.Errorf("bench: block must be positive, got %d", p.Block)
	case p.Workers <= 0:
		return fmt.Errorf("bench: workers must be positive, got %d", p.Workers)
	}
	return nil
}

// blocks splits the object on the block grid. The last one is short when the
// object does not divide evenly, and it is read at its real length so no arm
// is asked for bytes past the end.
func (p Plan) blocks() [][2]int64 {
	var out [][2]int64
	for off := int64(0); off < p.Size; off += p.Block {
		length := p.Block
		if rest := p.Size - off; rest < length {
			length = rest
		}
		out = append(out, [2]int64{off, length})
	}
	return out
}

// Result is one arm's run of one plan.
type Result struct {
	Arm     string
	Workers int
	Bytes   int64
	Elapsed time.Duration
	// Checksum is over the object as reassembled from the answers, so two arms
	// that disagree about the bytes cannot be compared on speed alone.
	Checksum uint64
}

// Rate reports megabytes a second, the unit the comparison is argued in.
func (r Result) Rate() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Bytes) / r.Elapsed.Seconds() / (1 << 20)
}

// Run executes a plan against one reader per worker.
//
// Readers are given rather than made here: a socket arm needs a connection
// each, and a backend arm needs a client that will open as many. Which of
// those a caller passes is what makes it an arm.
func Run(ctx context.Context, arm string, readers []Reader, p Plan) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	if len(readers) != p.Workers {
		return Result{}, fmt.Errorf("bench: %d readers for %d workers", len(readers), p.Workers)
	}

	work := p.blocks()
	parts := make([][]byte, len(work))
	errs := make([]error, p.Workers)

	// Interleaved rather than contiguous, because a reader walking a shard
	// front to back is not what a dataloader does to one file.
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < p.Workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(work); i += p.Workers {
				b, err := readers[w].ReadRange(ctx, p.Object, work[i][0], work[i][1])
				if err != nil {
					errs[w] = fmt.Errorf("block at %d: %w", work[i][0], err)
					return
				}
				parts[i] = b
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if err := errors.Join(errs...); err != nil {
		return Result{}, fmt.Errorf("bench: %s: %w", arm, err)
	}

	sum := fnv.New64a()
	var total int64
	for _, b := range parts {
		sum.Write(b)
		total += int64(len(b))
	}
	if total != p.Size {
		return Result{}, fmt.Errorf("bench: %s read %d bytes of a %d byte object", arm, total, p.Size)
	}
	return Result{Arm: arm, Workers: p.Workers, Bytes: total, Elapsed: elapsed, Checksum: sum.Sum64()}, nil
}

// Median reports the middle run of a repeated measurement, which is what a
// noisy shared backend makes the honest summary rather than the best one.
func Median(rs []Result) Result {
	if len(rs) == 0 {
		return Result{}
	}
	sorted := append([]Result(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Elapsed < sorted[j].Elapsed })
	return sorted[len(sorted)/2]
}
