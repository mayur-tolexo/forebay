# RFC-0015: Failure model and split brain

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0004, 0005 |

## Problem

Storage is trusted because of how it behaves when things break, not how it behaves when they work.
This RFC enumerates what can fail, what the blast radius is, and what the system does while it is
broken.

The cases that matter most are the ones where a component is slow rather than dead. A degraded node
still answering requests is worse than a crashed one, because the miss path never triggers and the
failure is invisible to liveness checks.

## What of this is built

**The behaviours, and now the readiness that makes the worst one visible.** The node holds its lock,
refuses a grant it cannot honour, replays its journal, times its reclaims and treats an overrun as an
error. `internal/metrics` computes readiness from observed service time against two bounds, over a
window, so a node that is slow rather than dead reports it.

Nothing yet feeds reads into it, because the read path does not record its own service time, and
nothing serves it: the endpoint an orchestrator probes belongs to the same change as the read path's
instrumentation and neither is written. What exists is the decision and its rules.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| Borrowed data is regenerable, so losing it is a miss and never a loss | Constraint inherited from RFC-0001, and enforced by there being no write path into the tier | A cache is treated as a copy, and something is lost that nothing else holds |
| The node is the only authority on its own capacity | Constraint from RFC-0004, and the reason split brain cannot create conflicting accepted leases | Two control planes lend the same bytes twice and the node breaks a promise it never made |
| A partition is more likely than a corruption | Reasoned, from the failure rates of networks against filesystems with checksums | Effort goes to the wrong half, and the design is careful about the rarer event |
| A slow component is more damaging than a dead one | Reasoned, and the reason this document exists: a dead node fails a probe and is replaced, while a slow one keeps taking work | Readiness stays a ping, and the worst failure remains the invisible one |

## Design

### What fails, and what it costs

| Failure | Blast radius | What the client sees | Data loss |
| --- | --- | --- | --- |
| A node's process dies | That node's tier | Reads to it fail; a client with another route retries | None. The tier is regenerable and the extents are reclaimed on restart |
| A node's NVMe fails | That node's leases | Reads to it fail until the lease is dropped | None, by the same argument |
| A node's NIC fails | That node, from outside | Unreachable, which is the dead case and the easy one | None |
| A rack's switch fails | Every node in it | Those nodes are unreachable; the backend is not, so a client with a direct route is slower and correct | None |
| The durable backend fails | Everything not already resident | Misses fail. This cannot be masked and is not tried | None here. The backend owns durability and Forebay does not weaken it |
| The control plane is unreachable | New grants only | Nothing, until a lease expires | None |
| A node becomes slow | Everything routed to it | Waits, and this is the case the rest of this document is about | None, and that is what makes it insidious |

The column that matters is the last one, and it is the same answer every time. There is no write path
into the tier: a byte in it came from the durable store and can be fetched again. That is not a
property this document argues for, it is one RFC-0001 imposed and everything since has kept.

### A control plane that has gone away takes nothing with it

A lease is a duration the node granted, and a node keeps its promise without being reminded. So when
the control plane is unreachable:

| Works | Stops |
| --- | --- |
| Serving reads from the tier | New grants, since there is nobody to propose one |
| Fetching a miss from the backend | Renewal, so leases decay toward their term |
| Reclaiming capacity for compute | Publishing accounting, which resumes when the link does |
| Expiring a lease whose term ran out | |

Reclamation working without the control plane is the property the whole design is arranged around,
and it is why a node reads pods from its own kubelet rather than from the API server. A partition
that could stop a node giving compute its disk back would be a partition that takes the node down
with it.

**A partition ends with less lending, never with more.** Terms run out and are not renewed, so the
node drifts toward owning all of its own capacity. That is the safe direction, and it is safe by
construction rather than by an operator noticing.

### Split brain cannot create two accepted leases

Two control planes can both believe they granted the same bytes. Neither can make it true.

A lease exists when the node accepted it, and a node accepts against its own accounting, holding its
own lock, on the filesystem it measured itself. A second control plane proposing the same capacity
is refused for the same reason any over-commitment is refused: the node cannot honour it.

| | Can happen | Cannot happen |
| --- | --- | --- |
| Two control planes propose the same capacity | Yes | |
| Both proposals are accepted | | No. The second is refused against the accounting the first changed |
| Two control planes believe different things about a node | Yes, briefly | |
| A node lends more than it has | | No, and this is the invariant rather than a consequence |

So split brain is a disagreement about beliefs and never about bytes, and it resolves the way every
other stale belief resolves: the control plane re-reads the node's published accounting, which is the
truth because the node is the one holding the disk.

**This is why leases are not in etcd.** A lease written to a shared store would have two writers and
would need a consensus protocol to arbitrate them. Keeping the authority where the capacity is means
there is nothing to arbitrate.

### Detecting a component that is slow rather than dead

A liveness probe answers whether a process is running. The failure this document is most concerned
with is a process that is running and answering slowly, which passes every probe while every client
waits on it.

So a node reports two different things:

| | Answers | Effect when it fails |
| --- | --- | --- |
| **Liveness** | Is the agent making progress at all, from the heartbeat RFC-0004 already writes | The agent is killed and its replacement takes the node lock |
| **Readiness** | Has the read path been answering quickly enough, over a recent window | The node is taken out of service and its work goes elsewhere, while it keeps running and keeps reclaiming |

Readiness is computed from observed service time rather than from a request that succeeded, because a
request that succeeded slowly is the failure. It uses the same measurements RFC-0017 publishes, so a
node that reports itself unready has already published the numbers that say why.

**Hysteresis, because a flapping node is worse than a slow one.** A node that crosses the bound
alternately is removed and restored repeatedly, and every removal moves work that then moves back.
Readiness therefore fails on a bound and recovers on a lower one, so a node that is marginal settles
into one state rather than oscillating between two.

**A node with nothing recent to judge is ready, and this is load bearing.** A node taken out of
service stops being sent reads, so it stops producing the samples that would show it had recovered.
Requiring evidence of health to return would make every removal permanent, which is a worse failure
than the one readiness was added to catch. Coming back on an absence of evidence is safe because the
same measurement removes it again within one window if it is still slow.

**Being unready does not stop reclaiming.** A node taken out of service is still the owner of its own
disk and still has compute on it. Refusing reads and refusing to give capacity back are different
decisions, and only one of them is a response to being slow.

### What a client sees, and what it can do about it

| Failure | Transparent | Because |
| --- | --- | --- |
| A block missing from the tier | Yes | The miss path fetches it, which is the tier's whole contract |
| A revoked block | Yes | RFC-0007 makes a revoked block a miss rather than an error |
| A node unready | To a client with another route | The layout it holds names a node, and getting another is the access layer's problem |
| A node gone mid-read | No | The read fails and the client retries, which for an unmodified NFS client is what its own timeout does |
| The backend gone | No | Nothing else has the bytes, and pretending otherwise would return something wrong |

The line this document will not cross is the last one. A miss that cannot be served is an error, and a
storage system that answers it with zeroes or with stale data has done something worse than fail.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Leases in etcd with a consensus protocol | One place to look, and a familiar shape | It creates the split brain it then has to solve, and puts the busiest fact in the system into its slowest store |
| Readiness as a synthetic probe read | Simple, and measures the real path | It measures a read nobody asked for, at a depth no client is using, and passes while real reads queue |
| Fencing a slow node by revoking its leases | Decisive, and stops it serving | It takes capacity from a node that is merely slow, which is a reclaim caused by a symptom rather than by demand |
| Serving stale bytes when the backend is unreachable | The read succeeds | It is the one failure mode a storage system may not have, and the tier holds no authority for what it caches |

## Failure modes

| Failure | What happens | Why it is acceptable |
| --- | --- | --- |
| Readiness is wrong and takes a healthy node out | Its work moves and the node keeps reclaiming | Conservative in the direction that costs throughput rather than correctness |
| Readiness is wrong and leaves a slow node in | The problem this document names remains | The bound is the operator's, is published, and the metric behind it is visible whether or not the bound fires |
| The heartbeat is written but the read path is wedged | Liveness passes and readiness fails | This is the case the split exists for, and the two answers disagreeing is the signal |
| Every node in a rack reports unready at once | The rack's work has nowhere to go | It is the correct report of a rack that has become slow, and hiding it would route work into it |
| A node is removed and never sent another read | It returns to ready when its window empties | The alternative is a removal that is permanent because the evidence needed to undo it only arrives when the node is in service |

## Performance implications

Readiness reads counters the data path already updates, on a request that is not in the data path.
Nothing here takes a lock a read holds, and a probe that is slow cannot make a read slow.

## Complexity

One endpoint and one window over numbers RFC-0017 already keeps. The complexity this document
avoids is a consensus protocol, and it avoids it by not creating the problem that would need one.

## Security and tenancy

Readiness says whether a node is serving quickly and carries no tenant, dataset or object. It is the
one surface here that can be public without leaking anything, which is deliberate: an orchestrator
has to reach it and giving it a credential to do so is a cost with no benefit.

## Open questions

- **What the readiness bound and its recovery bound should be**, which decides whether readiness
  protects a workload or merely describes the node. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which already carries it.
- **How the access layer learns a node is unready**, since a client holding a layout has been told
  where to read and readiness is a Kubernetes-shaped fact. Owned by
  [RFC-0008](0008-access-layer-pnfs.md), which owns what a client is told and when it is told again.
- **Whether a node that has been unready for a long time should return its leases**, which is a
  policy about a node that is not coming back rather than one that is briefly slow. No RFC owns it,
  because it needs the control plane to exist first and nothing yet proposes a grant over a network.
