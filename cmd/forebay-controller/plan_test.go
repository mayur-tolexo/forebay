package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/intent"
	"github.com/mayur-tolexo/forebay/internal/kube"
	"github.com/mayur-tolexo/forebay/internal/leaseapi"
)

// declared builds one dataset, as the controller would have resolved it.
func declared(namespace, name string, bytes int64, latency intent.Latency) kube.Dataset {
	var d kube.Dataset
	d.Metadata.Namespace = namespace
	d.Metadata.Name = name
	d.Spec.Intent = intent.Intent{Latency: latency}
	d.Status = &kube.DatasetStatus{Present: bytes > 0, Bytes: bytes}
	return d
}

// TestOnlyWhatAskedForTheTierIsPlannedFor is the first half of the planner:
// capacity is lent against what users declared, not against everything stored.
func TestOnlyWhatAskedForTheTierIsPlannedFor(t *testing.T) {
	list := kube.DatasetList{Items: []kube.Dataset{
		declared("red", "a", 4<<30, intent.LatencyCached),
		declared("red", "b", 2<<30, intent.LatencyCached),
		declared("red", "c", 8<<30, intent.LatencyBestEffort),
		declared("blue", "d", 1<<30, intent.LatencyCached),
	}}

	got := wanted(list, intent.Floor{})
	if len(got) != 2 {
		t.Fatalf("planned for %d tenants, want two: %+v", len(got), got)
	}
	if got[0].Tenant != "blue" || got[0].Bytes != 1<<30 {
		t.Errorf("blue = %+v", got[0])
	}
	// Summed per tenant, and the best-effort dataset left out.
	if got[1].Tenant != "red" || got[1].Bytes != 6<<30 {
		t.Errorf("red = %+v, want 6GiB from the two cached datasets", got[1])
	}
}

// TestADatasetThatIsNotThereAsksForNothing matters because lending against a
// dataset nobody has uploaded takes space from datasets that exist.
func TestADatasetThatIsNotThereAsksForNothing(t *testing.T) {
	absent := declared("red", "a", 0, intent.LatencyCached)
	absent.Status.Present = false

	// And one nothing has resolved yet, which is every dataset on the pass
	// after it was created and has no status at all.
	unresolved := declared("red", "b", 4<<30, intent.LatencyCached)
	unresolved.Status = nil

	list := kube.DatasetList{Items: []kube.Dataset{absent, unresolved}}
	if got := wanted(list, intent.Floor{}); len(got) != 0 {
		t.Errorf("planned %+v for datasets the store does not have", got)
	}
}

// agentNode stands in for a node agent, recording what was proposed to it.
type agentNode struct {
	mu        sync.Mutex
	free      int64
	proposals []leaseapi.Proposal
	refuse    leaseapi.Refusal
	held      map[string]bool
}

func (n *agentNode) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/capacity" {
			json.NewEncoder(w).Encode(leaseapi.Capacity{Free: n.free, MeasuredAt: time.Now()})
			return
		}
		var p leaseapi.Proposal
		json.NewDecoder(r.Body).Decode(&p)
		n.proposals = append(n.proposals, p)
		switch {
		case n.refuse != "":
			json.NewEncoder(w).Encode(leaseapi.Decision{Reason: "no", Refusal: n.refuse})
		case n.held[p.ID]:
			json.NewEncoder(w).Encode(leaseapi.Decision{Granted: true, Already: true})
		default:
			if n.held == nil {
				n.held = map[string]bool{}
			}
			n.held[p.ID] = true
			json.NewEncoder(w).Encode(leaseapi.Decision{Granted: true})
		}
	})
}

func (n *agentNode) seen() []leaseapi.Proposal {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]leaseapi.Proposal(nil), n.proposals...)
}

// planner builds a proposer pointed at one fake node.
func planner(t *testing.T, n *agentNode, share float64) proposer {
	t.Helper()
	agent := httptest.NewServer(n.handler())
	t.Cleanup(agent.Close)
	host, port := hostPort(t, agent.URL)

	c := newCluster()
	c.slices = sliceFor("forebay-agent", "worker-3", host, port, true)
	api := httptest.NewServer(c.handler())
	t.Cleanup(api.Close)
	client, err := kube.New(kube.Config{Host: api.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return proposer{
		client: client, token: "t", service: "forebay-agent",
		namespace: "forebay", timeout: 2 * time.Second, share: share,
	}
}

// TestAPlannerAsksForNoMoreThanItsShare is the bound that keeps this from
// taking a node the compute is there to use.
func TestAPlannerAsksForNoMoreThanItsShare(t *testing.T) {
	node := &agentNode{free: 100 << 30}
	p := planner(t, node, 0.25)

	granted, refused, err := p.propose(context.Background(), []demand{{Tenant: "red", Bytes: 80 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	if granted != 1 || refused != 0 {
		t.Fatalf("granted %d refused %d", granted, refused)
	}
	seen := node.seen()
	if len(seen) != 1 {
		t.Fatalf("proposed %d times", len(seen))
	}
	if want := int64(25 << 30); seen[0].Bytes != want {
		t.Errorf("asked for %d bytes of a 100GiB node, want %d", seen[0].Bytes, want)
	}
}

// TestAPlannerAsksForNoMoreThanIsWanted keeps a small dataset from being given
// a quarter of every node.
func TestAPlannerAsksForNoMoreThanIsWanted(t *testing.T) {
	node := &agentNode{free: 100 << 30}
	p := planner(t, node, 0.25)

	if _, _, err := p.propose(context.Background(), []demand{{Tenant: "red", Bytes: 1 << 30}}); err != nil {
		t.Fatal(err)
	}
	if got := node.seen()[0].Bytes; got != 1<<30 {
		t.Errorf("asked for %d bytes for a 1GiB demand", got)
	}
}

// TestTheLeaseIdentifierIsStable is what makes a proposal idempotent: a
// control plane that restarted must ask for the capacity it already has rather
// than for a second copy of it.
func TestTheLeaseIdentifierIsStable(t *testing.T) {
	node := &agentNode{free: 100 << 30}
	p := planner(t, node, 0.25)
	want := []demand{{Tenant: "red", Bytes: 1 << 30}}

	granted, _, err := p.propose(context.Background(), want)
	if err != nil || granted != 1 {
		t.Fatalf("first pass granted %d: %v", granted, err)
	}
	granted, refused, err := p.propose(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	// Already held, so nothing was granted and nothing was refused.
	if granted != 0 || refused != 0 {
		t.Errorf("a second pass granted %d and had %d refused, want neither", granted, refused)
	}
	seen := node.seen()
	if seen[0].ID != seen[1].ID {
		t.Errorf("two passes proposed different identifiers: %q and %q", seen[0].ID, seen[1].ID)
	}
}

// TestARefusalIsCountedNotFatal matters because a node with no room is an
// ordinary answer, and a planner that stopped would leave every other node
// unasked.
func TestARefusalIsCountedNotFatal(t *testing.T) {
	node := &agentNode{free: 100 << 30, refuse: leaseapi.NoCapacity}
	p := planner(t, node, 0.25)

	granted, refused, err := p.propose(context.Background(), []demand{{Tenant: "red", Bytes: 1 << 30}})
	if err != nil {
		t.Fatalf("a refusal was reported as an error: %v", err)
	}
	if granted != 0 || refused != 1 {
		t.Errorf("granted %d refused %d, want 0 and 1", granted, refused)
	}
}

// TestANodeWithNoRoomIsNotAsked keeps the planner from proposing zero bytes,
// which a node would refuse as malformed for no reason.
func TestANodeWithNoRoomIsNotAsked(t *testing.T) {
	node := &agentNode{free: 0}
	p := planner(t, node, 0.25)

	granted, refused, err := p.propose(context.Background(), []demand{{Tenant: "red", Bytes: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	if granted != 0 || refused != 0 {
		t.Errorf("granted %d refused %d against a full node", granted, refused)
	}
	if got := node.seen(); len(got) != 0 {
		t.Errorf("a node with no free space was asked for %+v", got)
	}
}

// TestTwoTenantsShareOneNodesBudget covers the second tenant, which must not
// be given the whole share again.
func TestTwoTenantsShareOneNodesBudget(t *testing.T) {
	node := &agentNode{free: 40 << 30}
	p := planner(t, node, 0.5)

	_, _, err := p.propose(context.Background(), []demand{
		{Tenant: "blue", Bytes: 15 << 30},
		{Tenant: "red", Bytes: 15 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, s := range node.seen() {
		total += s.Bytes
	}
	if want := int64(20 << 30); total > want {
		t.Errorf("asked for %d bytes in total, more than the %d share", total, want)
	}
}

// TestNothingWantedAsksNobody keeps a cluster where no dataset declared the
// tier from being polled every pass.
func TestNothingWantedAsksNobody(t *testing.T) {
	node := &agentNode{free: 100 << 30}
	p := planner(t, node, 0.25)

	granted, refused, err := p.propose(context.Background(), nil)
	if err != nil || granted != 0 || refused != 0 {
		t.Errorf("an empty plan did something: %d %d %v", granted, refused, err)
	}
	if got := node.seen(); len(got) != 0 {
		t.Errorf("a node was asked with nothing wanted: %+v", got)
	}
}
