// Package metrics emits what RFC-0017 says an operator acts on, in the text
// exposition format a scrape reads.
//
// Written directly rather than taken from a library, because the format is a
// dozen lines and a client library would be the largest dependency in a
// repository that has none.
//
// The rule the registry enforces is the one that keeps a metric store standing
// during the incident it would explain: a label may name something a human
// declared and may not name something a request carried.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind says how a series is read, which decides how it is written out.
type Kind int

const (
	// Counter only ever rises, so a scrape reads a rate from it.
	Counter Kind = iota
	// Gauge is a level, and may fall.
	Gauge
	// Histogram is a distribution, which is what a tail latency needs: a mean
	// hides the slow reads that are the whole complaint.
	Histogram
)

func (k Kind) String() string {
	switch k {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	default:
		return "histogram"
	}
}

// allowed is the closed set of label names this project publishes.
//
// Closed on purpose. Every name here identifies something a human declared or
// a set this project fixed, and adding one is a decision rather than an
// accident: an object or a request identifier as a label is one series per
// shard or per read, and the bill arrives exactly when traffic does.
var allowed = map[string]bool{
	"tenant": true, "dataset": true, "class": true, "source": true, "fact": true,
}

// ErrLabel rejects a label this project does not publish.
type ErrLabel struct{ Name string }

func (e *ErrLabel) Error() string {
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf("metrics: %q is not a label this project publishes, which are %s: "+
		"a label names what somebody declared, never what a request carried", e.Name, strings.Join(names, ", "))
}

// Labels names one series within a metric.
type Labels map[string]string

// Registry holds every series a process publishes.
//
// One registry per process, passed to whatever records into it, rather than a
// package-level default: a global would make two tests share state and would
// make it impossible to assert what a component published without asserting
// what every other component did too.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
	order   []string
}

// metric is one name, its kind, and every labelled series under it.
type metric struct {
	kind    Kind
	help    string
	buckets []float64
	series  map[string]*series
	order   []string
}

// series is one set of label values.
type series struct {
	labels Labels
	value  float64
	// counts is per bucket and count/sum are the whole distribution, which a
	// histogram needs and the other kinds leave empty.
	counts []uint64
	count  uint64
	sum    float64
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{metrics: map[string]*metric{}}
}

// Register declares a metric before anything records into it.
//
// Declared up front so a scrape of an idle process still shows the series
// exist. A metric that appears only once something happened cannot be alerted
// on for not happening, which is the whole of why the watch publishes a pass
// counter.
func (r *Registry) Register(name string, kind Kind, help string, buckets ...float64) error {
	if name == "" {
		return fmt.Errorf("metrics: a metric needs a name")
	}
	if kind == Histogram && len(buckets) == 0 {
		return fmt.Errorf("metrics: %s is a histogram and needs buckets, since a distribution with none is a mean wearing a costume", name)
	}
	if !sort.Float64sAreSorted(buckets) {
		return fmt.Errorf("metrics: %s has unsorted buckets, which a scrape reads as cumulative and would misread", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.metrics[name]; ok {
		return fmt.Errorf("metrics: %s is already registered", name)
	}
	r.metrics[name] = &metric{kind: kind, help: help, buckets: buckets, series: map[string]*series{}}
	r.order = append(r.order, name)
	return nil
}

// key identifies one series, and refuses a label this project does not publish.
func key(labels Labels) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(labels))
	for n := range labels {
		if !allowed[n] {
			return "", &ErrLabel{Name: n}
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(labels[n])
	}
	return b.String(), nil
}

// at finds or creates one series, holding the lock.
func (r *Registry) at(name string, labels Labels) (*metric, *series, error) {
	m, ok := r.metrics[name]
	if !ok {
		return nil, nil, fmt.Errorf("metrics: %s was not registered", name)
	}
	k, err := key(labels)
	if err != nil {
		return nil, nil, err
	}
	s, ok := m.series[k]
	if !ok {
		s = &series{labels: labels}
		if m.kind == Histogram {
			s.counts = make([]uint64, len(m.buckets))
		}
		m.series[k] = s
		m.order = append(m.order, k)
	}
	return m, s, nil
}

// Add moves a counter or a gauge.
func (r *Registry) Add(name string, labels Labels, delta float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, s, err := r.at(name, labels)
	if err != nil {
		return err
	}
	if m.kind == Histogram {
		return fmt.Errorf("metrics: %s is a histogram, so observe into it rather than adding", name)
	}
	if m.kind == Counter && delta < 0 {
		return fmt.Errorf("metrics: %s is a counter and cannot fall by %v", name, delta)
	}
	s.value += delta
	return nil
}

// Set puts a gauge at a level.
func (r *Registry) Set(name string, labels Labels, value float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, s, err := r.at(name, labels)
	if err != nil {
		return err
	}
	if m.kind != Gauge {
		return fmt.Errorf("metrics: %s is a %s, and only a gauge can be set", name, m.kind)
	}
	s.value = value
	return nil
}

// Observe records one measurement into a histogram.
func (r *Registry) Observe(name string, labels Labels, v float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, s, err := r.at(name, labels)
	if err != nil {
		return err
	}
	if m.kind != Histogram {
		return fmt.Errorf("metrics: %s is a %s, and only a histogram is observed into", name, m.kind)
	}
	for i, upper := range m.buckets {
		if v <= upper {
			s.counts[i]++
		}
	}
	s.count++
	s.sum += v
	return nil
}

// WriteTo emits every series in the text exposition format.
//
// Deterministic in the order metrics were registered and series first seen, so
// a diff between two scrapes is readable and a test can assert on the whole
// output rather than on a parse of it.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	for _, name := range r.order {
		m := r.metrics[name]
		if m.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, m.help)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, m.kind)
		for _, k := range m.order {
			s := m.series[k]
			if m.kind != Histogram {
				fmt.Fprintf(&b, "%s%s %s\n", name, render(s.labels, ""), number(s.value))
				continue
			}
			// Cumulative, which is what the format means by a bucket, and the
			// infinite one carries the total so a scrape can compute a
			// quantile without the sum.
			var running uint64
			for i, upper := range m.buckets {
				running = s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name, render(s.labels, number(upper)), running)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", name, render(s.labels, "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", name, render(s.labels, ""), number(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", name, render(s.labels, ""), s.count)
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// render writes a label set, adding le for a histogram bucket.
func render(labels Labels, le string) string {
	if len(labels) == 0 && le == "" {
		return ""
	}
	names := make([]string, 0, len(labels))
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", n, labels[n])
	}
	if le != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "le=%q", le)
	}
	b.WriteByte('}')
	return b.String()
}

// number formats without an exponent where it can, since a scrape reads either
// and a human reads one of them.
func number(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Handler serves the registry to a scrape.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteTo(w)
	})
}
