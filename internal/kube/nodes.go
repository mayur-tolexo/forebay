package kube

import (
	"context"
	"fmt"
	"sort"
)

// NodeResource is where nodes live: the core group, which has no name.
var NodeResource = Resource{Version: "v1", Plural: "nodes"}

// SliceResource is where EndpointSlices live, which is how an agent's address
// is found together with the node it runs on.
//
// EndpointSlices rather than pods, because a slice already carries the node
// name beside the address. Listing pods to find the same pair would be a
// wider read for a worse answer.
var SliceResource = Resource{Group: "discovery.k8s.io", Version: "v1", Plural: "endpointslices"}

// Node is as much of a node as this project reads.
type Node struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// EndpointSlice is as much of one as finding an agent needs.
type EndpointSlice struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Endpoints []struct {
		Addresses  []string `json:"addresses"`
		NodeName   *string  `json:"nodeName"`
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
	} `json:"endpoints"`
	Ports []struct {
		Name *string `json:"name"`
		Port *int32  `json:"port"`
	} `json:"ports"`
}

// Agent is one node and where to reach the agent on it.
type Agent struct {
	Node    string
	Address string
}

// Agents lists the ready endpoints of one service, paired with the node each
// runs on.
//
// A slice with no node name is skipped rather than guessed at: the whole point
// of the pairing is to know whose labels to write, and writing the wrong
// node's is worse than writing none.
func Agents(ctx context.Context, c *Client, namespace, service string) ([]Agent, error) {
	r := SliceResource
	r.Namespace = namespace

	var list struct {
		Items []EndpointSlice `json:"items"`
	}
	if err := c.List(ctx, r, &list); err != nil {
		return nil, err
	}

	var out []Agent
	for _, s := range list.Items {
		if s.Metadata.Labels["kubernetes.io/service-name"] != service {
			continue
		}
		port := int32(0)
		for _, p := range s.Ports {
			if p.Port != nil {
				port = *p.Port
				break
			}
		}
		if port == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			// An endpoint that is not ready is one whose agent said it should
			// not be sent work, and reading residency off it would publish a
			// level for a node that is failing.
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			if e.NodeName == nil || *e.NodeName == "" || len(e.Addresses) == 0 {
				continue
			}
			out = append(out, Agent{Node: *e.NodeName, Address: fmt.Sprintf("%s:%d", e.Addresses[0], port)})
		}
	}
	// Ordered, so two passes over one cluster do the same work in the same
	// order and a log of them can be compared.
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}
