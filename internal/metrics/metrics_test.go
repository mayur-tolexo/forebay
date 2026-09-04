package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestALabelMustNameSomethingDeclared is the rule that keeps a metric store
// standing during the incident it would explain. An object or a request
// identifier is one series per shard or per read.
func TestALabelMustNameSomethingDeclared(t *testing.T) {
	r := New()
	if err := r.Register("forebay_read_bytes_total", Counter, "bytes delivered"); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("forebay_read_bytes_total", Labels{"tenant": "acme"}, 1); err != nil {
		t.Errorf("a declared label was refused: %v", err)
	}
	for _, bad := range []string{"object", "request_id", "node", "pod"} {
		err := r.Add("forebay_read_bytes_total", Labels{bad: "x"}, 1)
		var e *ErrLabel
		if !errors.As(err, &e) {
			t.Errorf("label %q was accepted, want it refused", bad)
			continue
		}
		if !strings.Contains(err.Error(), "never what a request carried") {
			t.Errorf("the refusal for %q does not say why: %v", bad, err)
		}
	}
}

// TestAnIdleProcessStillShowsItsSeries covers the reason metrics are declared
// up front: a metric that appears only once something happened cannot be
// alerted on for not happening.
func TestAnIdleProcessStillShowsItsSeries(t *testing.T) {
	r := New()
	if err := r.Register("forebay_watch_passes_total", Counter, "passes the watch made"); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if _, err := r.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "# TYPE forebay_watch_passes_total counter") {
		t.Errorf("an idle registry did not declare its metric:\n%s", got)
	}
}

// TestCountersDoNotFall keeps a rate from being computed off a number that
// went backwards, which a scrape reads as a counter reset.
func TestCountersDoNotFall(t *testing.T) {
	r := New()
	r.Register("c", Counter, "")
	if err := r.Add("c", nil, -1); err == nil {
		t.Error("a counter was decremented")
	}
	r.Register("g", Gauge, "")
	if err := r.Add("g", nil, -1); err != nil {
		t.Errorf("a gauge could not fall: %v", err)
	}
}

// TestKindsAreNotInterchangeable keeps a histogram from being set and a gauge
// from being observed into, either of which produces a series that reads as
// something it is not.
func TestKindsAreNotInterchangeable(t *testing.T) {
	r := New()
	r.Register("h", Histogram, "", 1, 2)
	r.Register("g", Gauge, "")
	if err := r.Set("h", nil, 1); err == nil {
		t.Error("a histogram was set")
	}
	if err := r.Add("h", nil, 1); err == nil {
		t.Error("a histogram was added to")
	}
	if err := r.Observe("g", nil, 1); err == nil {
		t.Error("a gauge was observed into")
	}
	if err := r.Add("missing", nil, 1); err == nil {
		t.Error("an unregistered metric was recorded into")
	}
}

// TestAHistogramNeedsBuckets covers the declaration that would otherwise be a
// mean wearing a costume, since tail latency is the whole reason for one.
func TestAHistogramNeedsBuckets(t *testing.T) {
	r := New()
	if err := r.Register("h", Histogram, ""); err == nil {
		t.Error("a histogram with no buckets was registered")
	}
	if err := r.Register("h2", Histogram, "", 2, 1); err == nil {
		t.Error("unsorted buckets were accepted, which a cumulative reader would misread")
	}
	if err := r.Register("ok", Histogram, "", 1, 2); err != nil {
		t.Errorf("sorted buckets were refused: %v", err)
	}
	if err := r.Register("ok", Gauge, ""); err == nil {
		t.Error("a name was registered twice")
	}
}

// TestHistogramBucketsAreCumulative covers the format's own meaning: a bucket
// counts everything at or under its bound, and the infinite one is the total.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New()
	r.Register("forebay_read_seconds", Histogram, "how long a read took", 0.001, 0.01, 0.1)
	for _, v := range []float64{0.0005, 0.005, 0.05, 5} {
		if err := r.Observe("forebay_read_seconds", Labels{"source": "tier"}, v); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	r.WriteTo(&out)
	got := out.String()

	for _, want := range []string{
		`forebay_read_seconds_bucket{source="tier",le="0.001"} 1`,
		`forebay_read_seconds_bucket{source="tier",le="0.01"} 2`,
		`forebay_read_seconds_bucket{source="tier",le="0.1"} 3`,
		`forebay_read_seconds_bucket{source="tier",le="+Inf"} 4`,
		`forebay_read_seconds_count{source="tier"} 4`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "forebay_read_seconds_sum{source=\"tier\"} 5.0555") {
		t.Errorf("the sum is wrong:\n%s", got)
	}
}

// TestTheHandlerSaysWhatItIs covers the content type, since a scrape that
// cannot tell the format reads nothing.
func TestTheHandlerSaysWhatItIs(t *testing.T) {
	r := New()
	r.Register("forebay_tier_bytes", Gauge, "bytes resident in the tier")
	r.Set("forebay_tier_bytes", nil, 1<<20)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "forebay_tier_bytes 1.048576e+06") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestTheNodeSetRegistersEverythingItPromises covers the whole published set,
// since a process that shipped without a series leaves an alert firing on an
// absence that means nothing.
func TestTheNodeSetRegistersEverythingItPromises(t *testing.T) {
	r := New()
	if err := Node(r); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	r.WriteTo(&out)
	got := out.String()

	for _, name := range []string{
		ReadSeconds, ReadBytes, ReadsInFlight, TierHits, TierBytes, LeaseBytes,
		ReclaimSeconds, ReclaimShortfall, HeadroomBytes, WatchPasses, PoolReserve, TopologyDegraded,
	} {
		if !strings.Contains(got, "# TYPE "+name+" ") {
			t.Errorf("%s was not registered", name)
		}
		if !strings.Contains(got, "# HELP "+name+" ") {
			t.Errorf("%s carries no help, so a dashboard shows a name and no meaning", name)
		}
	}
	if err := Node(r); err == nil {
		t.Error("registering the node set twice was accepted")
	}
}

// TestTheReadBucketsSpanBothSources matters because a tier read and a backend
// read are three orders of magnitude apart, and buckets that span one of them
// put every read in the first or the last and answer nothing.
func TestTheReadBucketsSpanBothSources(t *testing.T) {
	if readBuckets[0] > 0.0002 {
		t.Errorf("the fastest bucket is %v, which a tier read lands under", readBuckets[0])
	}
	if last := readBuckets[len(readBuckets)-1]; last < 1 {
		t.Errorf("the slowest bucket is %v, which a slow backend read exceeds", last)
	}
	if last := reclaimBuckets[len(reclaimBuckets)-1]; last < 30 {
		t.Errorf("the reclaim buckets stop at %v, short of the deadline an overrun has to be visible against", last)
	}
}
