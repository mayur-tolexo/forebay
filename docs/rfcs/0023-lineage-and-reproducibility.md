# RFC-0023: Lineage, provenance and immutable versions

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 3 |
| **Depends on** | 0012, 0021 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Six months after a model ships, somebody asks which data produced it. The honest answer is usually
a guess assembled from job logs, a bucket path that has since been overwritten, and somebody's
memory.

Dataset versions in Forebay are immutable and share extents rather than being copied, which means
keeping every version is cheap enough to be the default rather than an indulgence. Once versions are
immutable and free, recording what was trained on what becomes a matter of writing down edges in a
graph.

The result is reproducibility that does not depend on anyone remembering to be careful, and an audit
trail that regulated users increasingly have to produce.

## What this RFC must answer

- The lineage graph: which entities are nodes, and what an edge asserts
- How an experiment or training job is attributed to the exact dataset version it read, without trusting the job to report honestly
- How checkpoints and models are linked back through the runs that produced them
- What immutability actually guarantees, and what an operator with admin rights can still change
- How lineage survives dataset deletion, and whether a version referenced by lineage can ever be reclaimed
- Whether write-once retention locks are needed for regulated users, and what that costs elsewhere in the design

## Constraints inherited from earlier RFCs

- Versions are immutable and share extents under the no-copy policy
- Nothing here may require a copy to be made

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
