# RFC-0005: Capacity pools and elastic leases

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 1 |
| **Depends on** | 0002, 0004 |

## Problem

Forebay's first claim is that idle compute-local NVMe can be lent to a storage fabric and taken back
without migrating data, without a rebalance, and without measurably slowing the job that owns the
node. This RFC is where that claim is either made good or shown to be false.

The architecture already supplies the idea that makes it plausible: borrowed capacity holds only
regenerable data, so reclamation is a delete. That idea settles *what* reclamation does. It settles
none of the things that actually break lease systems, which are timing, authority and partition.

Three questions have to be answered precisely, because a wrong answer to any of them turns a storage
system into something that occasionally freezes a training job.

1. When compute wants capacity back, how long may Forebay take, and what happens if it cannot?
2. Who decides how much capacity a node has lent, when the control plane and the node disagree?
3. What happens when the control plane is unreachable and a lease is still outstanding?

## What of this is built

This is the first RFC with an implementation behind it, so it says which parts are code. A document
that reads as description while half of it is intention is how someone comes to rely on a guarantee
nobody has written.

| Part of the design | State |
| --- | --- |
| The pools and the arithmetic between them | Built, `internal/pool` |
| Lease classes and the reclaim ladder | Built, `internal/lease` |
| Never worse off, with the shortfall reported rather than hidden | Built |
| The journal, replay, and refusing grants until it has been replayed | Built |
| Post-reclaim cooldown and the churn budget | Built |
| Allocating capacity as preallocated extents, one per lease | Built as an interface, with no caller yet. `fallocate` on Linux, and a build that cannot commit blocks says so at startup rather than implying it did. The agent binary starts, reports and exits, so nothing an operator can run grants a lease: the caller is the control plane, which is not built |
| Draining readers between invalidating and unlinking | **Not built.** Nothing serves an extent, so there is nobody to wait for. The access layer has to wait between those two steps rather than around them |
| Freeing the disk before the accounting, on every path | Built for release. **Not on reclaim**, where the lease manager owns the ladder and frees the accounting before the extents are gone, leaving a window in which a grant could be accepted against space still occupied |
| The reclaim deadline | **Stated and validated, not enforced.** An elastic grant is refused if no deadline is configured, so the promise cannot go missing quietly. Honouring it end to end means invalidating readers, which is the data path |
| Invalidate before unlink | Built. An extent is renamed out of reach before it is unlinked, and the rename is atomic, so an interrupted reclaim leaves a name no lease claims |
| Reconciling the journal against the device, and unlinking orphans | Built. Both directions: an extent no lease accounts for is unlinked, and a lease whose extent is gone is dropped |
| Any interface to a control plane | **Not built.** Accepting a grant is a local call, not a request. Nothing proposes grants over a network and nothing publishes the accounting back |
| Publishing pool accounting, so the control plane's view is a cache of the node's | **Not built** |

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| Unlinking preallocated extents returns capacity in well under a second | **Measured**, see below | The reclaim deadline cannot be met and the class deadlines need rethinking |
| Kubernetes gives a usable early signal that a pod needs local storage | Unverified, and the subject of RFC-0014 | Reclamation becomes reactive to pressure rather than anticipatory, which is slower and worse |
| A reclaim deadline of tens of seconds is compatible with pod admission | Unverified | The default is wrong and has to be derived from measurement |
| A client can be revoked without its cooperation, inside the deadline | **Measured against the specification**, see below | Reclamation cannot honour its contract while pNFS is the access path |
| Regenerable data is genuinely regenerable, including partially written extents | Reasoned | A reclaimed extent could be served as valid, which is data corruption rather than a cache miss |

The last one is the dangerous assumption in this document, and the design treats it as a correctness
requirement rather than a hope.

## Design

### Pools

Every node's NVMe divides into three pools. The division is bytes, not devices, and a pool is not a
filesystem.

| Pool | Sized by | Holds | Returned |
| --- | --- | --- | --- |
| Compute | Whatever the node has not given away | Whatever the workload writes | Not applicable, Forebay never holds it |
| Donated | Operator configuration | Durable data, through a backend driver | Never |
| Borrowed | Outstanding leases | Regenerable data only | On reclamation, by deletion |

Donated capacity is not leased. It is given once, and if an operator wants it back they drain the
node like any other storage maintenance. Everything below concerns the borrowed pool.

### The node agent is the authority

The control plane grants leases. **The node agent decides whether a grant is real.**

This inversion is the load-bearing decision in this RFC. The agent holds the only ground truth about
its own device: total capacity, what the kubelet has committed, what is donated, and what is
currently lent. A grant from the control plane is a proposal, which the agent accepts only if its own
accounting says the capacity exists.

Everything awkward about distributed leases becomes tractable once authority sits at the node.

- Two control planes cannot over-commit a node, because neither of them is doing the arithmetic.
- A stale control-plane view cannot cause over-allocation, only a rejected grant.
- Reclamation never requires reaching the control plane, so it cannot be blocked by a partition.

The cost is that the control plane's view of fleet capacity is always a slightly stale cache, and
capacity reporting has to say so rather than presenting it as exact.

### Lease classes

A lease is a grant of up to N bytes on one node, in one class, until an expiry. The class determines
only one thing: how quickly the capacity can be taken back.

| Class | Reclaim deadline | For | Reclaimed |
| --- | --- | --- | --- |
| `opportunistic` | None, immediate | Prefetch, speculative fill | First, without warning |
| `elastic` | Bounded, `reclaimWithin` | Cache, scratch | Second, within the deadline |
| `guaranteed` | Not before expiry | Checkpoint staging in progress | Never early. The expiry is the promise |

An elastic lease without a configured deadline is refused rather than granted. A zero deadline would
make elastic capacity reclaimable immediately, which is the opportunistic class under another name,
and a promise that quietly evaporates is worse than one never made.

`guaranteed` exists because some regenerable data is expensive to regenerate at a bad moment. A
checkpoint staged halfway through a synchronised write across a thousand ranks is regenerable in
principle and catastrophic to drop in practice. The class buys a bounded window, never an open one,
and a guaranteed lease that cannot be granted is refused rather than downgraded silently.

### The reclaim ladder

```mermaid
flowchart LR
    sig["Compute needs N bytes<br/>admission · ephemeral request · fs pressure"]
    o["Drop opportunistic leases<br/>immediate"]
    e["Expire elastic leases<br/>within reclaimWithin"]
    g["Guaranteed leases<br/>untouched until expiry"]
    ok["Capacity returned"]
    no["Shortfall reported<br/>compute fails exactly as it would<br/>on a node without Forebay"]

    sig --> o
    o -->|still short| e
    e -->|still short| g
    o -->|enough| ok
    e -->|enough| ok
    g --> no

    classDef fast fill:#CCFBF1,stroke:#0D9488,stroke-width:1.5px,color:#042F2E
    classDef control fill:#E0E7FF,stroke:#4F46E5,stroke-width:1.5px,color:#1E1B4B
    classDef warn fill:#FEF3C7,stroke:#B45309,stroke-width:1.5px,color:#451A03
    class o,e fast
    class sig control
    class g,no warn
    class ok control
```

Within a step of the ladder, already-expired leases go first because releasing them costs nothing at
all, and the remainder are dropped oldest-first, age standing in as a proxy for coldness.

Leases granted in the same instant tie on both of those keys, and the identifier breaks the tie. That
is not decoration. Without it the order falls back to however the leases came out of a map, which Go
randomises, so two calls could disagree about which of two identical leases to drop. It was a real
defect, found by running the tests repeatedly rather than once, and anyone tempted to simplify the
comparison should know what it is for. Nothing
better is available without hit-rate data per lease, which is a thing to revisit once RFC-0017 can
supply it.

The last box is the invariant that matters most, and it is stated here as a rule the implementation
is held to:

> **Forebay must never leave a node worse off than if Forebay were not installed.**

If every reclaimable byte has been returned and compute still cannot be satisfied, the node is in the
state it would have been in anyway, and the failure belongs to the cluster's capacity planning rather
than to Forebay. A design that could exceed that bound, for instance by holding guaranteed leases
covering most of a device, is a design that can take a node down. Guaranteed capacity is therefore
capped, and the cap is enforced by the agent at grant time.

**The cap is a fraction of device capacity, not of the borrowed pool.** An earlier draft of this
document said the borrowed pool, which is wrong: the borrowed pool shrinks as capacity is reclaimed,
so a cap with that denominator would loosen at exactly the moment it needed to bind. A node under
pressure would find its guarantee ceiling falling towards the guarantees it had already issued. The
device is a stable denominator and bounds the real quantity, which is how much of a node can be
pinned at once.

### Measured: unlink is not the problem

Reclaim latency was measured on a dev node on 2026-08-31, ext4 on LVM, Ubuntu 24.04, kernel
6.8.0-137.

| Case | Result |
| --- | --- |
| Unlink 4 GiB written with `O_DIRECT` | 2.6 ms |
| Unlink 8 GiB across four files, under concurrent `O_DIRECT` write load | 2.5 ms |

The agent's own path was measured on 2026-09-01 on a GPU node with local NVMe, once extents existed
to reclaim: granting a 2 GiB lease, which preallocates the extent, took 5 ms, and reclaiming it,
which invalidates and then unlinks, took 3.6 ms. `fallocate` committed the blocks rather than sizing
a sparse file, confirmed by the pool occupying 2147487744 bytes on disk against a 2 GiB request.

The failure path was exercised on the same node against a 64 MiB filesystem, since a device with
free space cannot demonstrate what happens without it. A 512 MiB grant that the accounting allowed
was refused by the device with `no space left on device`, the accounting rolled back to nothing, no
partial extent was left in the pool, and a later honest grant succeeded.

Four orders of magnitude inside a thirty second deadline, and concurrent write load made no
measurable difference. The reclaim deadline is therefore not set by the filesystem, which means it is
set by the access layer, and that is where the remaining risk lives.

One caveat on that number: it measures how long the `unlink` call takes to return, not how quickly
the freed capacity becomes observably available to a competing writer. Filesystems may free extents
lazily. RFC-0018 should measure the second thing, since the second thing is what compute actually
waits for.

### Reclamation must be an unlink

Borrowed capacity is allocated as whole preallocated extents, large and few, never as many small
files. Reclaiming is then unlinking a handful of large objects, which returns capacity without
writing anything.

This matters because reclamation happens exactly when the node is under pressure. Any reclaim path
that needs to write, compact or rewrite in order to free space will be at its slowest precisely when
it is most needed. The design forbids it.

### In-flight reads become misses, never errors

An extent can be reclaimed while a client is reading it. The rule is that the reader observes a cache
miss and refetches from the durable backend. It must never observe an IO error, and it must never
observe stale or partial bytes.

The agent therefore invalidates before it unlinks. An extent moves to `invalid` first, at which point
new reads refuse it and in-flight reads are failed with a retryable signal that the access layer
converts into a backend fetch. Only once no reader holds it is it unlinked.

**This is where the elastic deadline meets the access layer, and it is a hard constraint on
RFC-0008.** With pNFS the reader holds a layout, and invalidating an extent means taking that layout
away.

The version of this risk the document was originally written around has been answered. Recall is the
polite path and waiting for it is not the only option: flexfiles fencing lets the metadata server
revoke a client's credentials directly, without the client cooperating, so reclamation never has to
wait out an NFS lease period. What remains unmeasured is how long revocation actually takes against a
running metadata server under load, and whether it fences one client or every reader of an extent.
RFC-0008 owns both, and RFC-0018 has to measure the first.

### Leases expire, and expiry is fail-safe

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Free: capacity not lent
    Free --> Leased: agent accepts a grant
    Leased --> Serving: extents allocated and filled
    Serving --> Serving: renewed before expiry
    Serving --> Reclaiming: compute needs capacity
    Serving --> Reclaiming: expired without renewal
    Reclaiming --> Free: invalidated then unlinked
    note right of Reclaiming
        Both paths converge.
        Losing the control plane and being
        asked for capacity look the same here.
    end note
```

Renewal requires the control plane. Reclamation does not.

Under a partition the agent keeps serving existing leases until they expire, then returns the
capacity. Forebay degrades toward giving capacity back to compute, which is the correct direction to
fail in, and the fleet loses cache rather than gaining a hazard. A partition that outlasts every
lease term ends with a cluster that has no fast tier and no incident.

### Thrash

A node whose job repeatedly grows and shrinks could churn capacity continuously, paying the cost of
filling a cache that is about to be dropped.

Two mechanisms, both at the agent since it holds the facts:

- **A post-reclaim cooldown.** For a configured period after returning capacity, the agent declines
  new grants, so a reclaim followed immediately by a grant cannot oscillate. It bounds only lending,
  never reclamation, because compute is not made to wait for a cooldown.
- **A churn budget.** The agent tracks reclamations per unit time and declines new grants once a node
  exceeds its budget, reporting itself as churning. The control plane routes cache elsewhere and
  RFC-0017 surfaces the node, since chronic churn is usually a scheduling problem wearing a storage
  costume.

### Surviving an agent restart

Leases are journalled to local disk before they are honoured. A grant that cannot be written is not
honoured at all, because capacity lent with no record of the lending leaks the moment the agent comes
back. On restart the agent replays the journal, reconciles it against what is actually on the device,
expires anything past its deadline, and only then accepts new grants. Extents present on disk with no
journal entry are unlinked, because capacity that nobody has a record of lending is capacity that has
leaked.

**The journal is rewritten whole rather than appended to.** A node holds tens of leases, so writing
the entire set costs nothing, and it avoids torn records, replay ordering and compaction, none of
which earn their keep at that volume. The write goes to a temporary file that is flushed and renamed
over the target, with the directory flushed too, since a rename is only durable once its directory
entry is.

**A journal that cannot be read is recoverable rather than fatal.** Everything it describes is
regenerable, so an agent that cannot parse its own journal discards the borrowed pool, starts empty
and reports the failure. There is no repair path because there is nothing worth repairing. This is
the regenerable-only rule paying for itself in code that does not have to exist.

**Reconciliation drops as well as restores.** A lease whose term ran out while the node was down is
gone. So is one the accounting can no longer fit, because the node's shape may have changed while it
was away and compute keeps whatever it now needs. Duplicate identifiers are treated as corruption
rather than collapsed, since lending for a record twice and keeping it once would leave the
accounting permanently above the leases justifying it, with the difference unreclaimable.

**A node that has not replayed its journal lends nothing.** Accepting a grant before the replay would
count that capacity twice as soon as the replay caught up, so the refusal is enforced rather than
left to whoever starts the agent. Reclamation is deliberately not gated the same way: handing
capacity back to compute is safe from any state, and making compute wait on a replay would invert the
rule the whole design rests on.

### Accounting

The agent publishes, and the control plane caches, one number per pool. Compute is derived from node
allocatable minus what the kubelet has committed. Donated is configuration. Borrowed is the sum of
accepted leases. The three plus free space must equal device capacity, and the agent reports a
discrepancy rather than papering over it, because in a system whose entire promise is about capacity,
arithmetic that does not balance is a defect and not a rounding detail.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Control plane is authoritative on capacity | One place to reason about, simpler mental model | Reclamation would depend on reaching it, which breaks the promise exactly when a partition coincides with pressure. Split brain would also become able to over-commit a node |
| No classes, one reclaim deadline | Much simpler, one number to explain | Either prefetch is treated as precious, wasting the cheapest reclaim available, or checkpoint staging is treated as disposable, which loses work |
| Reclaim by migrating extents elsewhere | Preserves the cache, avoids refetch cost | This is the rebalance storm the architecture exists to avoid, and it is slowest when the node is busiest |
| Leases without expiry, released explicitly | No renewal traffic, simpler steady state | A lost control plane leaves capacity lent forever, so the failure mode is capacity leaking away from compute, which is the wrong direction |
| Let guaranteed leases cover the whole borrowed pool | Strongest promise to a checkpoint writer | Makes it possible for Forebay to starve a node, which violates the never-worse-off invariant |

## Failure modes

**Reclaim misses its deadline.** The agent reports the shortfall and continues reclaiming. Compute
sees the delay it would have seen on a full node. The metric that matters here is not whether it ever
happens but how far past the deadline it goes, which RFC-0017 must expose as a distribution rather
than an average.

**A reader holds an extent open indefinitely.** Invalidation cannot complete, so unlink cannot
proceed. The agent must be able to break the reference after a bounded wait, which means the access
layer needs a forced revocation path. If that path does not exist, one stuck client can pin capacity
that a job needs.

**The journal disagrees with the disk.** Treated as a leak and cleaned up on the disk side, never by
inventing a lease. Silent divergence here would show up much later as a node that mysteriously has
less capacity than it should.

**A partially written extent is served after reclamation.** This is the only failure in this document
that is data corruption rather than a slowdown. Invalidate-before-unlink and refusing reads on
`invalid` extents are what prevent it, and both belong in the earliest tests rather than in hardening.

**Clock skew between agent and control plane.** Expiry is evaluated on the agent's clock. Grants
carry a duration rather than an absolute time, so a skewed control plane cannot extend a lease past
what the agent believes it agreed to.

## Performance implications

Predicted, not measured. Nothing here has run.

The intended cost of reclamation on the compute path is close to the cost of unlinking a few large
files, which should be well under a second. Whether that holds under simultaneous IO pressure is
unknown and belongs in RFC-0018 as a named experiment: reclaim latency while the node is saturated,
reported as a distribution.

The cost of being wrong about a reclaim is a cache miss and a refetch from the backend, which is the
same cost the fast tier exists to avoid. A node that churns therefore pays continuously, which is why
the churn budget exists.

## Complexity

The agent-side lease manager, its journal and the invalidate-before-unlink path are new and are
where the difficulty concentrates. The control-plane side is comparatively ordinary bookkeeping.

The constraint this imposes on everything downstream is the never-worse-off invariant. Any later
feature that wants to hold capacity harder, for any reason, has to argue against it, and the
guaranteed class already shows what that argument has to look like: a bounded window, a cap, and a
refusal rather than a silent downgrade.

## Security and tenancy

Reclaimed capacity is re-lent to a different tenant. Contents must not survive that transition, which
means an extent is not returned to the free pool until its contents are unrecoverable by the next
holder. Doing that cheaply, without writing over the whole extent at the worst possible moment, is
not solved in this document and is owned by RFC-0016.

A node agent that accepts grants it should not, or reports capacity it does not have, can starve its
own node's compute. The agent is the authority on capacity precisely so that this is contained to one
node rather than fleet-wide, but a compromised agent is a denial of service against its own workload
and RFC-0016 has to say so explicitly.

## Open questions

- **The default value of `reclaimWithin`.** Thirty seconds appears in the API sketch and is a guess.
  It has to come from pod admission behaviour and measured end-to-end reclaim rather than be chosen.
  Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **How quickly freed capacity becomes observably available to a competing writer**, as opposed to
  how quickly `unlink` returns, since the second is what compute actually waits for. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **Whether tight coupling between the metadata server and the node agents is worth the control
  protocol it requires**, given it buys per-client revocation rather than fencing every reader of an
  extent. Owned by [RFC-0008](0008-access-layer-pnfs.md).
- **Whether the guaranteed cap should be a fixed fraction or set by policy**, and whether a node may
  refuse guaranteed leases outright. Owned by [RFC-0009](0009-intent-and-policy-model.md).
- **How the churn budget and the cooldown are chosen.** The shipped defaults are conservative
  guesses. The values are owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), and whether
  either should adapt rather than be configured is owned by
  [RFC-0010](0010-autonomy-engine.md).
- **Whether oldest-first remains the right tiebreak within a class** once per-lease hit rates exist
  to do better. Owned by [RFC-0007](0007-fast-tier-data-path.md), which owns eviction, using the
  measurements from [RFC-0017](0017-observability.md).
- **Whether donated capacity should ever be reclaimable under extreme compute pressure.** The current
  answer is no. No RFC owns this, deliberately: it trades a durability promise against a compute one,
  and that is an operator's decision rather than an engineering one. Revisit when operators have
  opinions.
