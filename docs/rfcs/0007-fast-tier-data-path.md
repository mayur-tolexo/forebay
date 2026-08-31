# RFC-0007: Fast tier data path

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0005, 0006 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

The fast tier is the part Forebay owns outright: borrowed NVMe on the node, the same on rack
peers, and the logic deciding what lives where. Everything differentiated about the project happens
here, and so does most of the difficulty.

Its unusual constraint is that the storage underneath it can be revoked mid-read. Ordinary caches do
not have to survive their own capacity disappearing on someone else's schedule.

## What this RFC must answer

- The unit of caching, whether that is an object, an extent, a shard or a file range, and how that choice interacts with AI dataset access patterns
- Admission and eviction policy, and how eviction interacts with an imminent lease reclamation
- The consistency model, and what a reader is promised when the underlying durable object changes
- How rack-local peer fetch is coordinated, and how a peer is chosen
- How a read in flight is handled when its backing capacity is revoked
- Whether rack-local fetch beats going straight to a fanned-out backend, which may make the rack tier unnecessary

## Constraints inherited from earlier RFCs

- Regenerable data only. Anything here can be dropped without loss
- The control plane is not consulted on the read path

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
