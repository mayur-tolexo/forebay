# RFC-0015: Failure model and split brain

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 4 |
| **Depends on** | 0005, 0007 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Storage is trusted because of how it behaves when things break, not how it behaves when they work.
This RFC enumerates what can fail, what the blast radius is, and what the system does while it is
broken.

The cases that matter most are the ones where a component is slow rather than dead. A degraded node
still answering requests is worse than a crashed one, because the miss path never triggers and the
failure is invisible to liveness checks.

## What this RFC must answer

- Behaviour under node loss, rack loss, NVMe failure, NIC failure, switch failure, backend failure and control plane loss
- What happens to leases when the control plane is unreachable, which is the case that decides whether the lease design is safe
- How split brain is prevented when two control planes could grant conflicting leases
- How a slow component is detected, given that liveness checks will not find it
- What a client sees during each failure, and which failures are transparent
- Which failures can cause data loss, stated plainly rather than avoided

## Constraints inherited from earlier RFCs

- Borrowed data is regenerable, so its loss is a cache miss and never a data loss
- Durable data is the backend's responsibility, and Forebay must not weaken the guarantee the backend makes

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
