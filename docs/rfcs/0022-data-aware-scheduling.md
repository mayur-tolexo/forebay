# RFC-0022: Data-aware scheduling and warm start

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 2 |
| **Depends on** | 0003, 0007, 0014 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Kubernetes schedules a pod onto a node, and only then does anybody discover that the dataset it
needs is three racks away. The job then spends its first minutes pulling terabytes across the fabric
while eight accelerators idle, and it does this again on every restart.

Forebay knows something the scheduler does not: which nodes and which racks already hold the data
this job is about to read. Handing that back to the scheduler turns a cold start into a warm one, and
it costs nothing but plumbing. The inverse is also available. When placement is fixed and the data is
elsewhere, the control plane can pre-fill the destination rack before the pod is admitted, so the
first read is already local.

This is the clearest example of a capability that requires seeing both sides. A storage system that
cannot observe scheduling cannot offer it, and a scheduler that cannot observe cache residency cannot
either.

## What this RFC must answer

- How cache residency is exposed to the scheduler, whether as node labels, a scheduler plugin, or scoring extender
- How a job declares the datasets it will read, early enough for the information to be useful
- How warm start is triggered and budgeted, so pre-filling for one job does not evict data another job is reading
- What happens when the scheduler ignores the hint, which it is entitled to do
- How this behaves for gang-scheduled multi-node jobs where all ranks must land together
- Whether residency should influence scheduling at all for short jobs, where the fetch may be cheaper than the wait

## Constraints inherited from earlier RFCs

- Advisory only. Forebay influences scheduling and never blocks it
- Pre-filling uses borrowed capacity, which can be reclaimed at any moment

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
