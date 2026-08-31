# RFC-0004: Node agent

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0002, 0003 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

The node agent is the only Forebay component on the node. It discovers devices and topology,
owns the split between the compute, donated and borrowed pools, enforces lease decisions made by the
control plane, and serves the fast tier.

It is privileged, it sits next to customer workloads, and it holds capacity that other tenants will
later use. That combination makes its blast radius larger than its line count suggests.

## What this RFC must answer

- What privileges the agent genuinely needs, and how to hold as few as possible
- How the agent learns that compute wants capacity back, given Kubernetes signals such as admission, ephemeral-storage requests and eviction pressure
- What happens to outstanding leases when the agent crashes, and how state is recovered on restart
- Whether the agent can be upgraded without dropping the fast tier it is serving
- The local API surface, and what remains available when the control plane is unreachable
- How the agent behaves when it is degraded rather than dead, which is the harder case

## Constraints inherited from earlier RFCs

- The agent never blocks a compute workload. Compute always wins
- Nothing irreplaceable is held in the borrowed pool

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
