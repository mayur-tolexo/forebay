package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/internal/intent"
	"github.com/mayur-tolexo/forebay/internal/kube"
	"github.com/mayur-tolexo/forebay/internal/leaseapi"
)

// demand is how much tier capacity one tenant's datasets ask for.
//
// Per tenant, because a lease is charged to one and a quota is counted against
// one. A single lease covering every tenant's datasets would be capacity no
// quota could bound.
type demand struct {
	Tenant string
	Bytes  int64
}

// wanted totals what the datasets declare, for the tenants that asked for the
// tier at all.
//
// Only datasets the store actually has. A dataset declared and not yet
// uploaded asks for capacity to hold nothing, and lending against it would
// take space from datasets that exist.
func wanted(list kube.DatasetList, floor intent.Floor) []demand {
	byTenant := map[string]int64{}
	for _, d := range list.Items {
		// A dataset nothing has resolved yet has no status at all, which is
		// every dataset on the pass after it was created. It asks for nothing
		// until the store has been asked how large it is.
		if d.Status == nil || !d.Status.Present || d.Status.Bytes <= 0 {
			continue
		}
		// The floor is applied first, so a namespace whose administrator asked
		// for more durability is planned against what will be enforced.
		if floor.Apply(d.Spec.Intent).Latency != intent.LatencyCached {
			continue
		}
		byTenant[d.Metadata.Namespace] += d.Status.Bytes
	}

	out := make([]demand, 0, len(byTenant))
	for tenant, bytes := range byTenant {
		out = append(out, demand{Tenant: tenant, Bytes: bytes})
	}
	// Ordered, so two passes over one cluster propose in the same order and a
	// node's refusals are comparable between them.
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

// proposer asks nodes to lend capacity for the demand the datasets declare.
type proposer struct {
	client  *kube.Client
	token   string
	service string
	// namespace is where the agents' service lives.
	namespace string
	timeout   time.Duration
	// share bounds how much of one node this will ask for, since a node exists
	// to run compute and the pool arithmetic is the node's floor rather than
	// something a planner should aim at.
	share float64
}

// leaseID names the lease covering one tenant's demand on one node.
//
// Derived rather than random, which is what makes a proposal idempotent: a
// control plane that restarted proposes the same identifier and is told the
// node is already holding it, instead of asking for a second copy of capacity
// it already has.
func leaseID(tenant string) string { return "forebay-tier-" + tenant }

// propose asks every agent for what its share of the demand comes to.
//
// Every node is asked for the same amount rather than the demand being divided
// between them. A dataset is read from wherever the job lands, so capacity on
// one node does not serve a job on another, and dividing would leave every
// node holding a fraction that serves nobody.
func (p proposer) propose(ctx context.Context, want []demand) (granted, refused int, err error) {
	if len(want) == 0 {
		return 0, 0, nil
	}
	agents, err := kube.Agents(ctx, p.client, p.namespace, p.service)
	if err != nil {
		return 0, 0, fmt.Errorf("finding agents: %w", err)
	}

	var failed []error
	for _, a := range agents {
		c := leaseapi.NewClient(a.Address, p.token, p.timeout)
		capacity, err := c.Capacity(ctx)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: reading capacity: %w", a.Node, err))
			continue
		}
		budget := int64(float64(capacity.Free) * p.share)

		for _, d := range want {
			size := min(d.Bytes, budget)
			if size <= 0 {
				continue
			}
			decision, err := c.Propose(ctx, leaseapi.Proposal{
				ID: leaseID(d.Tenant), Tenant: d.Tenant,
				Class: "elastic", Bytes: size, Term: "24h",
			})
			if err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", a.Node, err))
				continue
			}
			switch {
			case decision.Granted && !decision.Already:
				granted++
				budget -= size
			case decision.Granted:
				// Already held. Not counted as granted, since nothing changed,
				// and not as refused, since the capacity is there.
			default:
				refused++
			}
		}
	}
	return granted, refused, joinAll(failed)
}

// describe renders one pass for an operator, naming the refusals by kind so
// that a cluster with no room reads differently from one that is backing off.
func describe(granted, refused int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "granted %d lease(s)", granted)
	if refused > 0 {
		fmt.Fprintf(&b, ", %d refused", refused)
	}
	return b.String()
}
