package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/residency"
)

// residencyOf is what a node holds of one dataset, in the form a publisher
// writes and an operator reads.
type residencyOf struct {
	Tenant   string  `json:"tenant"`
	Object   string  `json:"object"`
	Level    string  `json:"level"`
	Blocks   int     `json:"blocks"`
	Bytes    int64   `json:"bytes"`
	Total    int64   `json:"total"`
	Label    string  `json:"label"`
	Rack     string  `json:"rackLabel"`
	Fraction float64 `json:"fraction"`
}

// residencyReporter turns what the tier holds into what a scheduler can be
// told, and remembers what it last said.
//
// The agent holds no credential to write any of this. RFC-0016 puts the wider
// view on the controller, which does not run on a node a tenant's pods share,
// so this reports and the controller publishes.
type residencyReporter struct {
	tier    *fasttier.Cache
	backend *driver.Backend
	levels  *residency.Tracker
	// sizes remembers how large an object is, since it does not change: a
	// version is immutable, and asking the backend on every pass would put a
	// request per dataset per pass on a store that has readers waiting.
	sizes map[string]int64
}

func newResidencyReporter(tier *fasttier.Cache, backend *driver.Backend) *residencyReporter {
	return &residencyReporter{
		tier: tier, backend: backend,
		levels: residency.NewTracker(),
		sizes:  map[string]int64{},
	}
}

// report computes what this node holds of everything in its tier.
//
// A dataset whose size cannot be learned is left out rather than reported at
// some assumed fraction. A scheduler acting on a residency this node invented
// would place work for data that is not here.
func (r *residencyReporter) report(ctx context.Context) []residencyOf {
	held := r.tier.HeldBlocks()
	block := r.tier.BlockSize()

	// Anything that fell out of the tier since the last pass stops being
	// published, or a node advertises data it gave back.
	still := make(map[string]bool, len(held))
	for h := range held {
		still[trackKey(h)] = true
	}
	for key := range r.levels.Levels() {
		if !still[key] {
			r.levels.Forget(key)
		}
	}

	out := make([]residencyOf, 0, len(held))
	for h, blocks := range held {
		total, ok := r.sizeOf(ctx, h.Object)
		if !ok {
			continue
		}
		// Held bytes are counted in whole blocks, so an object whose last
		// block is a tail reads as slightly larger than it is. Clamped rather
		// than refused: the overshoot is at most one block and means the
		// object is entirely resident.
		bytes := min(int64(blocks)*block, total)
		fraction, err := residency.Fraction(bytes, total)
		if err != nil {
			continue
		}
		level := r.levels.Update(trackKey(h), fraction)
		out = append(out, residencyOf{
			Tenant: h.Tenant, Object: h.Object,
			Level: level.String(), Blocks: blocks, Bytes: bytes, Total: total,
			Label:    residency.Key(h.Tenant, h.Object),
			Rack:     residency.RackKey(h.Tenant, h.Object),
			Fraction: fraction,
		})
	}
	// Ordered, so two reports of one state read the same and a diff between
	// passes shows what changed rather than what was walked first.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Object < out[j].Object
	})
	return out
}

// sizeOf asks the backend how large an object is, once.
func (r *residencyReporter) sizeOf(ctx context.Context, object string) (int64, bool) {
	if n, ok := r.sizes[object]; ok {
		return n, true
	}
	// A backend that cannot say refuses the call, so there is no capability
	// check here: one error path covers a store that will not answer and a
	// driver that was never able to.
	n, err := r.backend.SizeOf(ctx, object)
	if err != nil || n <= 0 {
		return 0, false
	}
	r.sizes[object] = n
	return n, true
}

// trackKey identifies a dataset across passes.
func trackKey(h fasttier.Held) string { return h.Tenant + "\x00" + h.Object }

// residencyHandler serves what this node holds, for the controller that turns
// it into node labels.
func residencyHandler(r *residencyReporter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := json.MarshalIndent(r.report(req.Context()), "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, string(body))
	}
}
