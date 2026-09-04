# RFC-0027: KV cache spill and the inference path

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 4 |
| **Depends on** | 0005, 0007, 0026 |

## Problem

Every RFC in this corpus that describes a workload describes training. Nothing anywhere names
inference, and its storage problem is not the one the rest of this project is designed around.

An LLM serving engine holds a KV cache in GPU memory, and that cache is where the reuse lives: a
shared prompt prefix computed once can serve every request that shares it. The cache is bounded by
HBM, and when it overflows the engine has one option, which is to throw a block away and recompute it
on the next request that needs it.

Elastic allocation of that GPU memory is solved work and is not this document's problem.
[kvcached](https://github.com/ovg-project/kvcached) gives serving engines OS-style virtual memory for
the KV cache, integrates with vLLM and SGLang, and is in production use. What it deliberately does not
do is tier: what does not fit in HBM is recomputed. That is the seam this document is about, and it is
one Forebay is already built against, because compute-local NVMe next to the accelerator is precisely
the capacity that is idle on an inference node — if it is idle, which nobody has checked.

The reason to think the fit is good is that RFC-0007's cache model already describes KV blocks almost
without change: immutable by construction, addressed by an identity derived from content, regenerable
at a known cost.

The reason to doubt it is the crossover, and it is much tighter here than anywhere else in this
project. Everywhere else Forebay competes with a fanned-out object store. Here it competes with
recomputing the prefill on an accelerator, which is fast.

## What of this is built

**The break-even rule, and nothing that could be used without it.** `internal/kvspill` computes the
prefix length above which reading a block back beats recomputing it, from costs measured on the node
it runs on, and refuses every fetch below it. If the numbers say reading never wins, it says so once
and refuses everything, which is the honest outcome and the one this project's own standard demands.

No spill path, no serving integration, no engine hook. RFC-0001 makes an unmeasured crossover a kill
criterion rather than a caveat, and this document inherits that standard rather than an exemption
from it. What is built is the thing that decides whether anything else should be.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A KV block for a given prefix is immutable and regenerable at a known cost | Reasoned, from what a KV block is: a function of the tokens before it | The tier's whole model stops applying, since reclamation that drops data depends on the data being reproducible |
| Inference nodes have idle NVMe | **Unverified**, and this is RFC-0001's third kill criterion asked of a fleet nobody has measured. A training node's disk is idle between epochs; a serving node's may be absent | There is no capacity to borrow and the document describes a system with nothing to run on |
| Reading a block back can beat recomputing it above some prefix length | **Unverified**, and the tightest crossover in this project | Below every practical prefix length this path is worse than doing nothing, which is why the break-even is enforced rather than assumed |
| A serving engine will offer a block to an external tier rather than needing to be forked | Reasoned, from existing engines already having CPU and disk offload paths | The only way in is a fork, and the same argument that refused a custom NFS client refuses it |
| Prefix sharing across tenants would be an oracle on prompts | Reasoned, and stronger than the equivalent argument for datasets: a hit tells you somebody else computed that exact prefix | Sharing is refused where it was safe, costing capacity |

## Design

### The break-even is a gate, not a note in a document

Two costs, both measured on the node:

```
recompute(t)  =  t / prefill_tokens_per_second
read(t)       =  read_latency  +  (t x bytes_per_token) / read_bytes_per_second
```

Reading wins only above the prefix length where those cross. Below it, fetching is strictly worse
than not having tried, and RFC-0001's rule that Forebay must never leave a node worse off than a node
without Forebay makes that a refusal rather than a preference.

The crossing may not exist. If the transfer rate per token is slower than the accelerator recomputes,
the two lines never meet and no prefix is long enough. That is a real outcome on plausible hardware
and it is reported once, plainly, rather than becoming a fetch that is always a little bit worse.

This is why the gate is the only thing built. A spill path without it is a feature that makes a
serving fleet slower and reports a cache hit rate while doing it.

### The unit is the engine's, not ours

RFC-0007 chose a fixed block for a cache Forebay fills itself. Here the engine fixes the page size,
and a spill path that re-blocked would be splitting and joining on the hot path to satisfy a
convention of ours.

So a spilled block is stored whole, at whatever size the engine uses. That means variable-length
blocks, which is the same structural question RFC-0020 raised for compressing the tier and which
RFC-0007 already carries: a slab that addresses blocks by slot and a fixed size cannot hold them.
This document does not solve it and does not need to first, because the gate comes before the store.

### The engine decides what to evict; Forebay decides what it can hold

Neither side can see what the other knows. The engine knows which blocks it is about to need; Forebay
knows how much capacity there is and when it is about to be reclaimed.

So spill is **offered, not taken**. The engine offers a block it has decided to evict, and Forebay
accepts or declines on capacity alone. A decline is not an error: the engine was going to drop the
block anyway, and it does.

Fetching back is the engine's decision, and the gate above is Forebay's answer to it. The engine asks
whether a block is available and worth reading; Forebay answers no when it is below the break-even,
even when the block is resident.

### Reclamation does not change for inference

RFC-0005 reclaims on a deadline measured in seconds, and a serving engine allocates and frees per
token. That looks like a mismatch and is not one, because a KV block is regenerable: reclaiming one
costs a recompute, which is exactly what would have happened without Forebay.

What matters is that the engine never waits for a reclaim. A miss is answered immediately as a miss,
and the engine recomputes, which is its existing path. Forebay's reclaim deadline bounds when capacity
comes back and never bounds how long a request takes.

### Sharing, and why it stops at the tenant

Prefix sharing is the entire value: one computed prefix serving every request that shares it.

Within a tenant, spilled blocks are shared, which is the ordinary case and needs no argument. Across
tenants they are not, and the argument here is stronger than the one RFC-0016 makes for datasets. A
cross-tenant hit does not merely reveal that two tenants hold identical data; it reveals that another
tenant submitted a prompt with that exact prefix, and the latency difference makes it testable
without any access to the block. That is an oracle on other people's prompts, and no capacity saving
justifies it.

### Composing rather than replacing

Engines that already tier KV cache to CPU and disk are the natural consumers of borrowed capacity, not
competitors to it. Forebay's role underneath such a layer is to be the capacity it spills into, one
that can be reclaimed and says so.

Forking an engine is refused on the same reasoning RFC-0008 refuses a custom NFS client: it is a
maintenance obligation across versions of somebody else's project, forever, and it makes adoption a
migration.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Build the elastic HBM allocation layer too | One project owning the whole memory path | A different mechanism on a different clock, and it already exists and is in production use. Rebuilding it would be the largest thing in this document and the least novel |
| Fork vLLM or SGLang to add a spill path | Complete control, no integration constraints | A maintenance obligation across versions of somebody else's project, forever, and the same argument that refused a custom NFS client |
| Spill without a break-even gate, and measure later | Ships sooner, and the data would come from real traffic | Makes a serving fleet slower while reporting a cache hit rate, which is the specific dishonesty this project's own standards forbid |
| Re-block to Forebay's fixed block size | Reuses the tier as built, no variable-length problem | Splitting and joining on the hot path to satisfy a convention of ours, in the one place where the contest is decided by how fast a block moves |
| Share prefixes across tenants | Much higher hit rate, since popular prefixes are popular across everyone | An oracle on other tenants' prompts, testable by latency alone |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| The break-even does not exist on this hardware | The whole feature | Reported once and every fetch refused. The node behaves as a node without Forebay, which is the floor RFC-0001 sets |
| Costs were measured wrong, too optimistic | Serving latency | Fetches that lose to recompute. This is the failure the gate exists to prevent and it is only as good as its inputs, which is why they are measured per node rather than configured |
| Capacity is reclaimed under a serving engine | That engine's hit rate | Misses, which the engine answers by recomputing. No request waits |
| Inference nodes turn out to have no spare NVMe | The whole feature | Nothing to borrow. Named as a kill criterion above rather than discovered during integration |
| An engine offers blocks faster than capacity can take them | Nothing | Declines are ordinary. The engine was dropping the block regardless |

## Performance implications

The gate is arithmetic on four numbers, evaluated per fetch decision. It is on the serving path, which
is why it is arithmetic and not a lookup.

Everything else is unmeasured, and this document's position is that it should stay unbuilt until it is
not.

## Complexity

Small, deliberately. The document's contribution is a decision procedure and a refusal to build
against an unmeasured crossover.

What it makes harder later is spilling at Forebay's block size, since the unit is now the engine's by
decision rather than by omission.

## Security and tenancy

The sharing boundary above is the whole of it, and it is stricter than RFC-0016's rule for datasets
rather than an application of it. A prefix hit is an oracle on another tenant's prompt content,
testable by latency without access to any block, and the usual mitigations for a timing channel do not
survive the fact that the timing difference *is* the feature.

Within a tenant, a spilled block is subject to the same quota and the same reclamation as any other
borrowed byte, and the residual-data guarantee RFC-0016 built covers it without change: the capacity a
spilled block occupied reads as zeros for its next holder.

## Open questions

- **Whether an NVMe read of a KV block beats recomputing it, and above what prefix length.** The
  question this document is gated on. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), and it should be measured before anybody
  writes serving code.
- **Whether inference nodes have idle NVMe at all.** RFC-0001's third kill criterion asked of a fleet
  nobody has measured. Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns the
  other four.
- **What the path from NVMe into GPU memory has to look like** for the crossover to fall anywhere
  useful, since the contest is decided by how fast a block moves. Owned by
  [RFC-0026](0026-transport-and-throughput.md), which owns transport and is load-bearing here in a
  way it is not for dataset reads.
- **How variable-length blocks are stored**, since the unit is the engine's. Owned by
  [RFC-0007](0007-fast-tier-data-path.md), which already carries the same question for compression.
