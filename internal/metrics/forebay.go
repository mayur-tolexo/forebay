package metrics

import "fmt"

// The names RFC-0017 publishes. Constants rather than strings at the call
// site, so a typo is a build failure and not a series that silently never
// appears on a dashboard.
const (
	ReadSeconds      = "forebay_read_seconds"
	ReadBytes        = "forebay_read_bytes_total"
	ReadsInFlight    = "forebay_reads_in_flight"
	TierHits         = "forebay_tier_hits_total"
	TierBytes        = "forebay_tier_bytes"
	LeaseBytes       = "forebay_lease_bytes"
	ReclaimSeconds   = "forebay_reclaim_seconds"
	ReclaimShortfall = "forebay_reclaim_shortfall_bytes"
	HeadroomBytes    = "forebay_headroom_bytes"
	WatchPasses      = "forebay_watch_passes_total"
	PoolReserve      = "forebay_pool_reserve_bytes"
	TopologyDegraded = "forebay_topology_degraded_total"
)

// readBuckets span a read that hits the tier and one that crosses a network to
// a store, which are three orders of magnitude apart. Buckets narrower than
// that put every read in one and answer nothing.
var readBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// reclaimBuckets span an idle device and one at its sustained write rate,
// which RFC-0018 measured two orders of magnitude apart, and reach past the
// deadline so an overrun is visible rather than merely absent.
var reclaimBuckets = []float64{
	0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 5, 30,
}

// Node registers everything a node agent publishes.
//
// One call, so a process cannot ship having forgotten a series that an alert
// depends on: the alert would then fire on an absence that means nothing.
func Node(r *Registry) error {
	for _, m := range []struct {
		name    string
		kind    Kind
		help    string
		buckets []float64
	}{
		{ReadSeconds, Histogram, "how long Forebay took to answer a read", readBuckets},
		{ReadBytes, Counter, "bytes delivered, by where they came from", nil},
		{ReadsInFlight, Gauge, "reads being answered right now", nil},
		{TierHits, Counter, "reads the fast tier answered without the backend", nil},
		{TierBytes, Gauge, "bytes resident in the fast tier", nil},
		{LeaseBytes, Gauge, "capacity lent, by how reclaimable it is", nil},
		{ReclaimSeconds, Histogram, "how long taking capacity back took", reclaimBuckets},
		{ReclaimShortfall, Counter, "capacity compute asked for that the node could not return", nil},
		{HeadroomBytes, Gauge, "the floor of free space being kept, which moves with the write rate", nil},
		{WatchPasses, Counter, "passes the pressure watch has made, so silence can be told from a stopped watch", nil},
		{PoolReserve, Gauge, "what the filesystem holds for everything which is not Forebay", nil},
		{TopologyDegraded, Counter, "facts this node could once discover and no longer can", nil},
	} {
		if err := r.Register(m.name, m.kind, m.help, m.buckets...); err != nil {
			return fmt.Errorf("metrics: registering the node set: %w", err)
		}
	}
	return nil
}
