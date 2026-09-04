package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/internal/kube"
)

// prefix is what every label this project writes begins with, and is how a
// pass knows which of a node's labels are its to take back.
const prefix = "forebay.io/"

// held is one line of an agent's report. Only the fields a label needs are
// read: an agent that adds one must not need a controller that knows about it.
type held struct {
	Level string `json:"level"`
	Label string `json:"label"`
	Rack  string `json:"rackLabel"`
}

// residencyPass writes one round of node labels from what the agents report.
//
// The controller does this rather than the agents, because a node-resident
// component able to patch node objects is a compromised node able to label
// every other one, and RFC-0016 puts that credential here instead: the
// controller holds the wider view and does not run on a node a tenant's pods
// share.
type residencyPass struct {
	client  *kube.Client
	http    *http.Client
	service string
	// namespace is where the agents' service lives, which is also the only
	// namespace this reads endpoints from.
	namespace string
}

// run reconciles every node's labels with what its agent says it holds.
//
// One node failing does not stop the rest. A cluster where one agent is
// unreachable should still have correct labels everywhere else, and stopping
// would make the first broken node hide every other node's state.
func (p residencyPass) run(ctx context.Context) (labelled int, err error) {
	agents, err := kube.Agents(ctx, p.client, p.namespace, p.service)
	if err != nil {
		return 0, fmt.Errorf("finding agents: %w", err)
	}

	var failed []error
	for _, a := range agents {
		if err := p.one(ctx, a); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", a.Node, err))
			continue
		}
		labelled++
	}
	return labelled, joinAll(failed)
}

// one reconciles a single node.
func (p residencyPass) one(ctx context.Context, a kube.Agent) error {
	want, err := p.wanted(ctx, a)
	if err != nil {
		return err
	}
	node, err := p.node(ctx, a.Node)
	if err != nil {
		return err
	}

	patch := diff(node.Metadata.Labels, want)
	if len(patch) == 0 {
		// Nothing written when nothing changed, which is the point of the
		// hysteresis upstream: a pass per interval that patched every node
		// every time would be the label churn the levels exist to avoid.
		return nil
	}
	return p.client.PatchLabels(ctx, kube.NodeResource, a.Node, patch)
}

// wanted reads an agent's report and turns it into the labels its node should
// carry.
func (p residencyPass) wanted(ctx context.Context, a kube.Agent) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.Address+"/residency", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking %s: %w", a.Address, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asking %s: %s", a.Address, resp.Status)
	}
	// Bounded: a node holding a great many datasets is still a small answer,
	// and something else answering on that port should not be read in full.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var report []held
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("reading %s: %w", a.Address, err)
	}
	want := make(map[string]string, len(report)*2)
	for _, h := range report {
		if h.Level == "" || h.Label == "" {
			continue
		}
		want[h.Label] = h.Level
		if h.Rack != "" {
			// The rack label carries the node's own level. A rack is warm for
			// a gang when its nodes are, and combining them is the scheduler's
			// job rather than one node's.
			want[h.Rack] = h.Level
		}
	}
	return want, nil
}

// node reads one node's current labels.
func (p residencyPass) node(ctx context.Context, name string) (kube.Node, error) {
	var out kube.Node
	if err := p.client.Get(ctx, kube.NodeResource, name, &out); err != nil {
		return out, fmt.Errorf("reading node: %w", err)
	}
	return out, nil
}

// diff is what to patch: what changed, plus a null for every label of ours the
// node carries and should not.
//
// Only labels with this project's prefix are considered for removal. A
// controller that took back anything it did not recognise would delete labels
// an operator set by hand.
func diff(have map[string]string, want map[string]string) map[string]any {
	patch := map[string]any{}
	for k, v := range want {
		if have[k] != v {
			patch[k] = v
		}
	}
	for k := range have {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, still := want[k]; !still {
			patch[k] = nil
		}
	}
	return patch
}

// joinAll reports every node that failed rather than the first, since an
// operator fixing one wants to know whether it was alone.
func joinAll(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	sort.Strings(msgs)
	return fmt.Errorf("%d nodes could not be labelled: %s", len(errs), strings.Join(msgs, "; "))
}

// defaultResidencyTimeout bounds asking one agent, which is a request to a
// node that may be the very node that is wedged.
const defaultResidencyTimeout = 5 * time.Second
