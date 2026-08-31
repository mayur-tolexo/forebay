# RFC-0005: Capacity pools and elastic leases

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0004 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

This is the RFC that carries the project's first claim. Capacity is lent to the fabric and taken
back without migrating data, without a rebalance, and without the owning job noticing.

The design rests on one rule: borrowed capacity holds only regenerable data, so reclamation is a
delete. What that rule does not settle is the timing, the accounting and the behaviour under
partition, and those are where a lease model usually goes wrong.

## What this RFC must answer

- The lease classes, guaranteed, elastic and opportunistic, and what each actually promises
- The reclamation contract, including the time budget within which capacity must be returned and what happens if it is exceeded
- What happens to a read in flight against an extent that is being reclaimed
- How thrash is prevented when a workload repeatedly grows and shrinks
- Whether a lease is safe under a control plane partition, and what a node agent does when its lease expires and it cannot reach the control plane
- How capacity is accounted for, so that donated, borrowed and compute cannot double count the same bytes
- How reclamation interacts with a node that is already under memory or IO pressure, when the delete itself competes for resources

## Constraints inherited from earlier RFCs

- Reclamation is a delete and never a migration
- Compute always wins, and is not negotiated with
- The control plane is not in the IO path, so reclamation cannot depend on it being reachable

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
