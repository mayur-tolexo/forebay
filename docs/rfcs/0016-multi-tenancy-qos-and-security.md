# RFC-0016: Multi-tenancy, QoS and security

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 4 |
| **Depends on** | 0005, 0009 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Forebay runs a privileged agent next to customer workloads and hands the same physical capacity to
one tenant after another. Both properties make isolation a design problem rather than a configuration
one.

The specific hazard created by this architecture is residual data. Borrowed capacity is dropped and
re-lent constantly, and content surviving into the next holder would be a vulnerability rather than a
bug.

## What this RFC must answer

- The isolation boundary in the fast tier, and what prevents one tenant inferring another's access pattern
- How reclaimed capacity is guaranteed to carry nothing into its next holder, and what that costs in reclamation time
- Where QoS is enforced, and whether a guarantee can be made without a global admission decision
- Encryption at rest on borrowed and donated capacity, and in flight between agents
- Identity for nodes, agents and tenants, and what a compromised node agent can reach
- How tenant and region scoping is enforced in code, given the control plane holds broad credentials to the systems it manages

## Constraints inherited from earlier RFCs

- Residual data in reclaimed capacity is a vulnerability
- A denial of service against the compute workload is a security problem, because compute always wins is a promise

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
