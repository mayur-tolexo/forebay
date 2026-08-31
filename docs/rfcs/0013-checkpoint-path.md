# RFC-0013: Checkpoint path

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 3 |
| **Depends on** | 0007, 0009 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

A synchronised checkpoint across a large training job produces a burst of writes that a central
filesystem absorbs badly, and every rank waits for the slowest write. Staging locally and making data
durable afterwards is an old idea, and it is the right one, provided the durability promise is
precise.

The danger is the acknowledgement. A checkpoint that is reported complete before it is durable is a
correctness problem dressed as a performance win, and the difference only becomes visible when a node
is lost.

## What this RFC must answer

- What acknowledgement means, and the exact durability state implied by each policy
- The durability policy vocabulary, and how a user chooses between fast acknowledgement and strict durability
- What happens when a node is lost between acknowledgement and durability
- How writes are aggregated across ranks, and whether aggregation happens at rack level
- How the path behaves under a full synchronised checkpoint storm rather than a single writer
- How staging capacity is reserved, given that borrowed capacity can be reclaimed mid-checkpoint

## Constraints inherited from earlier RFCs

- Staging uses borrowed capacity, which is revocable
- A fast acknowledgement is only permitted where the stated policy allows it

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
