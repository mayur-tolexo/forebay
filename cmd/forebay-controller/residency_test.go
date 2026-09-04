package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/kube"
)

// cluster stands in for an API server and a set of agents, recording what was
// patched onto which node.
type cluster struct {
	mu      sync.Mutex
	nodes   map[string]map[string]string
	patches map[string]map[string]any
	slices  string
}

func newCluster() *cluster {
	return &cluster{
		nodes:   map[string]map[string]string{},
		patches: map[string]map[string]any{},
	}
}

func (c *cluster) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "endpointslices"):
			fmt.Fprint(w, c.slices)
		case r.Method == http.MethodPatch:
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			var body struct {
				Metadata struct {
					Labels map[string]any `json:"labels"`
				} `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			c.patches[name] = body.Metadata.Labels
			fmt.Fprint(w, `{}`)
		default:
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": name, "labels": c.nodes[name]},
			})
		}
	})
}

func (c *cluster) patched(node string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patches[node]
}

// sliceFor renders an EndpointSlice naming one agent on one node.
func sliceFor(service, node, host string, port int, ready bool) string {
	return fmt.Sprintf(`{"items":[{"metadata":{"name":"s","labels":{"kubernetes.io/service-name":%q}},
		"ports":[{"port":%d}],
		"endpoints":[{"addresses":[%q],"nodeName":%q,"conditions":{"ready":%t}}]}]}`,
		service, port, host, node, ready)
}

// agentSaying starts something that answers /residency with the given report.
func agentSaying(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// passOver builds a pass against a fake cluster and one agent.
func passOver(t *testing.T, c *cluster, agent *httptest.Server, node string, ready bool) residencyPass {
	t.Helper()
	host, port := hostPort(t, agent.URL)
	c.slices = sliceFor("forebay-agent", node, host, port, ready)

	api := httptest.NewServer(c.handler())
	t.Cleanup(api.Close)
	client, err := kube.New(kube.Config{Host: api.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return residencyPass{
		client: client, http: &http.Client{Timeout: 2 * time.Second},
		service: "forebay-agent", namespace: "forebay",
	}
}

func hostPort(t *testing.T, u string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(u, "http://")
	i := strings.LastIndex(trimmed, ":")
	if i < 0 {
		t.Fatalf("no port in %q", u)
	}
	var port int
	fmt.Sscanf(trimmed[i+1:], "%d", &port)
	return trimmed[:i], port
}

// TestANodeIsLabelledFromItsOwnAgent is the whole loop: the controller holds
// the credential and the node holds the knowledge, so the label a scheduler
// matches comes from one and is written by the other.
func TestANodeIsLabelledFromItsOwnAgent(t *testing.T) {
	c := newCluster()
	agent := agentSaying(t, `[{"level":"most","label":"forebay.io/resident-aaa","rackLabel":"forebay.io/resident-aaa-rack"}]`)
	p := passOver(t, c, agent, "worker-3", true)

	n, err := p.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("labelled %d nodes, want 1", n)
	}
	got := c.patched("worker-3")
	if got["forebay.io/resident-aaa"] != "most" {
		t.Errorf("node label = %v, want most", got["forebay.io/resident-aaa"])
	}
	if got["forebay.io/resident-aaa-rack"] != "most" {
		t.Errorf("rack label = %v, want most", got["forebay.io/resident-aaa-rack"])
	}
}

// TestNothingIsWrittenWhenNothingChanged is what the hysteresis upstream is
// for: a pass per interval that patched every node every time would be exactly
// the label churn the levels exist to avoid.
func TestNothingIsWrittenWhenNothingChanged(t *testing.T) {
	c := newCluster()
	c.nodes["worker-3"] = map[string]string{"forebay.io/resident-aaa": "most"}
	agent := agentSaying(t, `[{"level":"most","label":"forebay.io/resident-aaa"}]`)
	p := passOver(t, c, agent, "worker-3", true)

	if _, err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.patched("worker-3"); got != nil {
		t.Errorf("a node whose labels already matched was patched with %v", got)
	}
}

// TestALabelIsTakenBackWhenTheDataGoes matters because a stale label is worse
// than none: it sends a job to a node for data that has been reclaimed.
func TestALabelIsTakenBackWhenTheDataGoes(t *testing.T) {
	c := newCluster()
	c.nodes["worker-3"] = map[string]string{
		"forebay.io/resident-aaa": "most",
		"kubernetes.io/hostname":  "worker-3",
		"team":                    "research",
	}
	// The agent now holds nothing.
	agent := agentSaying(t, `[]`)
	p := passOver(t, c, agent, "worker-3", true)

	if _, err := p.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.patched("worker-3")
	if v, ok := got["forebay.io/resident-aaa"]; !ok || v != nil {
		t.Errorf("the stale label was not removed: %v", got)
	}
	// Everything else is left alone. A controller that took back what it did
	// not recognise would delete labels an operator set by hand.
	for _, keep := range []string{"kubernetes.io/hostname", "team"} {
		if _, touched := got[keep]; touched {
			t.Errorf("the pass touched %q, which is not its label", keep)
		}
	}
}

// TestAnUnreadyAgentIsNotAsked keeps a node that said it should not be sent
// work from having its residency published as though it were healthy.
func TestAnUnreadyAgentIsNotAsked(t *testing.T) {
	c := newCluster()
	agent := agentSaying(t, `[{"level":"most","label":"forebay.io/resident-aaa"}]`)
	p := passOver(t, c, agent, "worker-3", false)

	n, err := p.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("labelled %d nodes from an endpoint that was not ready", n)
	}
	if got := c.patched("worker-3"); got != nil {
		t.Errorf("an unready node was labelled: %v", got)
	}
}

// TestOneUnreachableAgentDoesNotHideTheRest matters because stopping at the
// first failure would make one broken node conceal every other node's state.
func TestOneUnreachableAgentDoesNotHideTheRest(t *testing.T) {
	c := newCluster()
	good := agentSaying(t, `[{"level":"some","label":"forebay.io/resident-bbb"}]`)
	host, port := hostPort(t, good.URL)
	// One reachable agent and one on TEST-NET-1, which is reserved for
	// documentation and routes nowhere, so it fails rather than reaching the
	// other agent by sharing its port.
	c.slices = fmt.Sprintf(`{"items":[{"metadata":{"name":"s","labels":{"kubernetes.io/service-name":"forebay-agent"}},
		"ports":[{"port":%d}],
		"endpoints":[
			{"addresses":[%q],"nodeName":"worker-3","conditions":{"ready":true}},
			{"addresses":["192.0.2.1"],"nodeName":"worker-9","conditions":{"ready":true}}]}]}`, port, host)

	api := httptest.NewServer(c.handler())
	defer api.Close()
	client, err := kube.New(kube.Config{Host: api.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	p := residencyPass{
		client: client, http: &http.Client{Timeout: 500 * time.Millisecond},
		service: "forebay-agent", namespace: "forebay",
	}

	n, err := p.run(context.Background())
	if err == nil {
		t.Error("an unreachable agent was not reported")
	}
	if n != 1 {
		t.Errorf("labelled %d nodes, want the one that answered", n)
	}
	if got := c.patched("worker-3")["forebay.io/resident-bbb"]; got != "some" {
		t.Errorf("the reachable node was not labelled: %v", got)
	}
}

// TestDiffOnlyTouchesOurOwnLabels is the rule that keeps a controller from
// deleting what an operator set.
func TestDiffOnlyTouchesOurOwnLabels(t *testing.T) {
	have := map[string]string{
		"forebay.io/a": "most",
		"forebay.io/b": "some",
		"team":         "research",
	}
	want := map[string]string{"forebay.io/a": "some"}

	got := diff(have, want)
	if got["forebay.io/a"] != "some" {
		t.Errorf("a changed level was not written: %v", got)
	}
	if v, ok := got["forebay.io/b"]; !ok || v != nil {
		t.Errorf("a label no longer wanted was not removed: %v", got)
	}
	if _, touched := got["team"]; touched {
		t.Errorf("a label this project did not set was touched: %v", got)
	}
	if len(diff(have, have)) != 0 {
		t.Error("an unchanged set produced a patch")
	}
}

// TestAQuietPassReportsNothingWritten matters because the count is the only
// thing an operator sees. A pass that reported a write every interval while
// writing nothing would say the opposite of what the design does, and the
// hysteresis would look like it had failed.
func TestAQuietPassReportsNothingWritten(t *testing.T) {
	c := newCluster()
	c.nodes["worker-3"] = map[string]string{"forebay.io/resident-aaa": "most"}
	agent := agentSaying(t, `[{"level":"most","label":"forebay.io/resident-aaa"}]`)
	p := passOver(t, c, agent, "worker-3", true)

	n, err := p.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a pass that wrote nothing reported %d writes", n)
	}

	// And a pass that does have something to say counts it.
	c.nodes["worker-3"] = map[string]string{"forebay.io/resident-aaa": "some"}
	n, err = p.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("a pass that raised a level reported %d writes", n)
	}
}
