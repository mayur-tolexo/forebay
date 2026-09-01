# RFC-0027: KV cache spill and the inference path

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 4 |
| **Depends on** | 0005, 0007, 0026 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Every RFC in this corpus that describes a workload describes training. Datasets, epochs,
dataloaders, checkpoints. The infrastructure documents are workload-neutral and would read the same
for a serving fleet, but nothing anywhere names inference, and its storage problem is not the one the
rest of this project is designed around.

An LLM serving engine holds a KV cache in GPU memory, and that cache is where the reuse lives: a
shared prompt prefix computed once can serve every request that shares it. The cache is bounded by
HBM, which is the most expensive memory in the building, and when it overflows the engine has one
option, which is to throw a block away and recompute it on the next request that needs it.

Elastic allocation of that GPU memory is solved work and is not this document's problem.
[kvcached](https://github.com/ovg-project/kvcached) gives serving engines OS-style virtual memory for
the KV cache, so an engine reserves address space and has physical HBM mapped in on demand rather
than reserved at startup. It integrates with vLLM and SGLang and is in production use.
Whether Forebay should build that layer itself is a real alternative and belongs in this document's
alternatives section rather than being settled here, though the case against is strong enough to
state: it is a different mechanism on a different clock, and it already exists.

What it deliberately does not do is tier. There is no CPU or disk path: it manages what fits in HBM,
and what does not fit is recomputed. That is the seam this document is about, and it is a seam
Forebay is already built against, because compute-local NVMe next to the accelerator is precisely the
capacity that is idle on an inference node.

The reason to think the fit is good is that RFC-0007's cache model already describes KV blocks almost
without change. A KV block for a given prefix is immutable by construction, addressed by an identity
derived from its content, and regenerable at a known cost. That is the same argument the fast tier
makes about dataset blocks: no invalidation, no coherence protocol between nodes, and reclamation
that can drop data because the data can be produced again.

The reason to doubt it is the crossover, and it is much tighter here than anywhere else in this
project. Everywhere else Forebay competes with a fanned-out object store and wins comfortably. Here
it competes with recomputing the prefill on an accelerator, which is fast. There is some prefix
length below which reading a KV block back from NVMe is strictly worse than not having tried, and
where that point sits decides whether this document is worth writing at all. It is a measurement, it
is owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), and it should be taken before
anybody writes serving code.

## What this RFC must answer

- Whether an NVMe read of a KV block beats recomputing it, and above what prefix length, since below
  that length this whole path is worse than doing nothing
- What the unit is, given a serving engine's page or block size is set by the engine rather than by
  Forebay, and RFC-0007 chose a fixed block for reasons that may not survive contact with it
- Who decides to spill and who decides to fetch back, since the engine owns the eviction decision and
  Forebay owns the capacity, and neither can see what the other knows
- How a spill path is offered without a custom engine, given that RFC-0008 already refuses to ship a
  custom client and the same reasoning applies to forking vLLM or SGLang
- How this composes with an existing offload layer rather than replacing it, since projects that
  already tier KV cache to CPU and disk are the natural consumers of borrowed capacity
- What the reclaim contract is at inference timescales, given RFC-0005 reclaims on a deadline
  measured in seconds and a serving engine allocates and frees per token
- Whether a spilled KV block may be shared between requests of one tenant, and what that implies for
  RFC-0016, since prefix sharing is the entire value and it is also an inference channel
- Whether inference nodes have idle NVMe at all, which is assumed here and has not been checked: a
  training node's disk is idle between epochs, and a serving node's may simply be absent. This is
  RFC-0001's third kill criterion asked of a fleet nobody has measured, and
  [RFC-0018](0018-benchmark-and-falsification-suite.md) owns it as it owns the other four
- What the path from NVMe into GPU memory has to look like for the crossover to fall anywhere useful,
  since the contest against recompute is decided by how fast a block can move rather than by where it
  is stored, which makes [RFC-0026](0026-transport-and-throughput.md) load-bearing here in a way it
  is not for dataset reads

## Constraints inherited from earlier RFCs

- Borrowed capacity holds only regenerable data, which a KV block is
- Reclamation deletes rather than migrates, and a reader observes a miss and never stale bytes
- Forebay must never leave a node worse off than a node without Forebay, which here means never
  making a serving engine slower than recomputing would have been
- An unmeasured crossover is not a reason to build. RFC-0001 makes locality failing to pay a kill
  criterion for the whole project, and this document inherits that standard rather than an exemption
  from it

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
