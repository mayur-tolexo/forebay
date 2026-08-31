# RFC-0025: Cross-cluster and cross-region datasets

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 4 |
| **Depends on** | 0006, 0021 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

GPU capacity is spread across clusters and regions, and datasets are not. A team with capacity free
in one region and its training data in another either copies the whole dataset and pays for it twice,
or leaves the accelerators idle.

Because dataset versions are immutable and addressed by content and version rather than by location,
a read-only copy in a second region is a well-defined thing rather than a synchronisation problem.
That makes distribution tractable in a way that mutable filesystems never manage.

The hard part is not moving bytes. It is deciding what a dataset means when it exists in more than
one place, and what an operator is promised about consistency, cost and residency.

## What this RFC must answer

- What identity a dataset version has across clusters, and how two regions agree they hold the same thing
- Whether a remote version is a replica, a cache, or a distinct object, and what that implies for lifecycle
- How distribution is triggered, by intent, by observed demand, or explicitly
- How data residency and sovereignty constraints are expressed and enforced, since some data may not legally leave a region
- What happens to lineage when a version exists in several places
- How cost is attributed when one region pulls a dataset that another region paid to store

## Constraints inherited from earlier RFCs

- Only immutable, published versions cross a cluster boundary
- Residency constraints are enforced in code, not by convention

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
