# RFC-0020: The no-copy policy

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 1 |
| **Depends on** | 0002, 0006, 0007 |

## Problem

Storage systems spend most of their time copying bytes that did not need to move, and almost none of
those copies are visible to the person paying for them.

Data is copied on ingest, because the system wants it in its own format. It is copied to make a
clone. It is copied to move between tiers. It is copied to be served through a second protocol. It is
copied again inside the IO path, from device to kernel to userspace to protocol buffer, several times
per read. Each copy costs bandwidth, media wear, capacity and latency, and on a GPU cluster the
latency is paid by an accelerator that is doing nothing while it waits.

Forebay's architecture already forbids the largest copy of all: reclaiming borrowed capacity is a
delete rather than a migration. This RFC generalises that instinct into a rule the whole system is
held to.

## What of this is built

**The rules that bind on paths that exist, enforced where they bind rather than here.** This document
is a policy, and a policy package that other packages had to remember to call would be a policy
nobody enforced. Each rule lives in the code it constrains:

| Rule | Where it is enforced today |
| --- | --- |
| No copy to ingest | `internal/kube`. A dataset names an object that already exists in the store, and nothing is rewritten |
| No copy to reclaim | `internal/agent`. Reclamation is a rename and an unlink |
| No copy to promote | `internal/fasttier`. A fill is admitted a block at a time, and abandoning one costs a later miss rather than leaving a migration half done |
| No copy to clone | Decided in `internal/dataset`, which refuses rather than copying, and reachable from nothing. No path performs a clone yet |
| No copy to version | Nothing. There are no versions |
| No copy to serve a second protocol | Nothing. There is one protocol |
| Minimum copies in the IO path | Partly. The tier reads a block into one buffer; how many copies remain on a realistic stack is a measurement RFC-0018 owns |

Two things are stated here rather than left to be discovered. The clone rule is written and unreached,
so it is a decision waiting for a caller and not yet a property of the system. And the two
compression capabilities RFC-0006 declares are consulted by no code, because nothing Forebay writes
is compressed.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| Operators will not adopt a system that rewrites the data they already have | Reasoned, from what a migration project costs and how often one is declined | The register-in-place rule is defended against an objection nobody raises, and a native format would have been the better design |
| A backend that compresses transparently keeps compressing data registered in place, because nothing was rewritten | Reasoned, from what registering in place means: the object is untouched | Registered data quietly loses the backend's compression, and the capacity argument for delegating downward is wrong |
| Compression of the payloads this project moves is worth roughly three to one | Measured once, on one object in one environment: the 226 MiB compressed object RFC-0018 records, against the same payload read raw. One object is not a corpus | The capacity and bandwidth argument for compressing anything is weaker than stated, and the CPU it costs on a GPU node buys less |
| Sharing bytes between references is safe only when deletion is exact | Reasoned, and stated as this document's dangerous failure mode | Either data is lost by an eager collector or capacity is held by a lazy one, and both look like storage bugs rather than policy consequences |

## The policy

**A byte is written once. Everything else is a reference.**

Stated as rules the design must satisfy, each of which is testable.

| Rule | Meaning | Where it binds |
| --- | --- | --- |
| **No copy to clone** | Snapshots and clones are metadata. Cloning a dataset for an experiment moves no bytes, whatever its size | [0012](0012-dataset-object-model.md) |
| **No copy to version** | A new dataset version shares every unchanged extent with its predecessor. Only what actually changed is written | [0012](0012-dataset-object-model.md) |
| **No copy to serve a second protocol** | Reading a dataset over S3 and over pNFS reads the same extents. There is no per-protocol replica | [0021](0021-single-copy-multi-protocol.md) |
| **No copy to ingest** | Data already sitting in a backend is registered in place and becomes a dataset without being rewritten | [0006](0006-durable-backend-driver-contract.md) |
| **No copy to reclaim** | Borrowed capacity is dropped, never migrated | [0005](0005-capacity-pools-and-elastic-leases.md) |
| **No copy to promote** | Filling the fast tier is a cache fill that can be abandoned, not a migration that must complete | [0007](0007-fast-tier-data-path.md) |
| **Minimum copies in the IO path** | Device to client with as few intermediate buffers as the platform allows | [0007](0007-fast-tier-data-path.md), [0008](0008-access-layer-pnfs.md) |

The last rule is the one that needs the most care, because it is the one where the honest answer
depends on hardware. io_uring, direct IO and RDMA each remove copies where they are available, and
none of them is available everywhere. Forebay detects capability and degrades, rather than requiring
a fabric most clusters do not have.

### Compression rewrites, so it is not ours to apply on ingest

Compression is a copy under another name: it reads bytes and writes different ones. That collides
directly with registering data in place, and the collision is better resolved here than discovered
during implementation.

| Position | Cost |
| --- | --- |
| Compress everything on ingest | Rewrites every byte a user already has, so adoption becomes a migration project, which is the thing operators refuse |
| Never compress | Gives up a large capacity and bandwidth win. On real code and data pages a measurement of zstd at level 3 returned roughly three to one |
| **Delegate below, compress only what we write** | Two mechanisms instead of one, and a capability the driver contract has to express |

The third is the position this document takes.

Data registered in place stays exactly as the backend holds it, and whatever compression that backend
already applies keeps applying, because nothing has been rewritten.

Data Forebay writes itself divides, and the two halves get opposite answers.

A fast-tier fill is regenerable, so a frame that will not decompress is a miss and the read falls
through to the backend. Compressing it risks nothing that a re-fetch cannot restore, and it is the
half where compression is defensible.

Checkpoint staging is not regenerable, and an earlier draft of this document said it was. RFC-0013
and RFC-0016 both rest on the opposite: a guaranteed lease holds bytes that are the only copy of
themselves until they reach durable storage, which is why a drain refuses to take one. A frame that
will not decompress there is a lost job rather than a miss. So Forebay does not compress checkpoint
staging, and would need a verified round trip on the write path before it could.

Three consequences follow. The driver contract has to let a backend declare whether it compresses and
whether Forebay can ask it to, which is [RFC-0006](0006-durable-backend-driver-contract.md). The
fast tier as built stores fixed-size blocks in slabs and addresses them by slot, so compressing it is
not a flag but a change to how a block is addressed — the trade is structural before it is a CPU
question, and RFC-0007 owns it. And the CPU it would cost is spent on a GPU node, where CPU is not
spare: it competes with the dataloader feeding the accelerator this project exists to keep busy.

## What a copy is still allowed for

A policy with no exceptions is a slogan. Copies remain legitimate in exactly three cases.

**Durability.** Replicas are copies, and they are the point. The rule constrains gratuitous copies,
not redundancy.

**Locality, where it is a cache.** Filling the fast tier duplicates bytes on purpose. It is
permitted because the duplicate is regenerable and can be abandoned at any moment, which is what
separates a cache from a migration.

**A backend that cannot do better.** If a driver has no server-side clone, a clone must either copy
or be refused. The control plane says which, out loud, rather than copying silently and calling it
instant.

## Why this is worth stating as policy

Every one of these rules is easy to hold at the start and easy to lose one exception at a time. A
system that copies on ingest because it was convenient, then copies per protocol because the
adapter was simpler that way, ends up with four copies of a dataset and no single decision to point
at.

Writing the rule down means a pull request that introduces a copy has to argue for it.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Copy on ingest into a native format | Full control of layout, simpler internals, better compression opportunities | Doubles capacity on day one and makes adoption a migration project, which is the thing operators refuse |
| Per-protocol representations | Each protocol gets an ideal layout and simpler code | Multiplies capacity by the number of protocols and makes consistency between them a permanent bug source |
| Copy to promote into the fast tier, treated as a move | Simpler accounting, one authoritative location | Turns reclamation back into a migration, which breaks the project's central promise |

## Failure modes

The dangerous failure is a reference outliving what it points at. If clones, versions and protocol
views all share extents, then deleting the last real reference has to be exact. Garbage collection
that is too eager loses data, and garbage collection that is too lazy silently holds capacity that
operators believe they freed. RFC-0012 owns that problem and it is the hardest part of this policy.

The second is amplification. Sharing extents between versions means one badly placed extent can be
hot for many logical datasets at once, so a placement mistake has a larger blast radius than it would
if every dataset owned its bytes.

## What this document settles

**The unit of sharing is the object, as the backend addresses it.** It is the only granularity every
backend exposes, and a policy stated in a unit some drivers cannot name would be unenforceable in
exactly the drivers that most needed it. Sharing below an object — one version reusing an unchanged
part of another — is a real want and a harder problem, and it belongs to
[RFC-0012](0012-dataset-object-model.md) with the rest of versioning.

**Register-in-place is not offered for every backend.** It needs the backend to address existing data
stably, which is why it is a declared capability rather than an assumption: a driver says whether it
can, and a dataset on a driver that cannot is refused rather than silently copied.

**Extent sharing between tenants is off unless the owning tenant grants it.** Sharing bytes discloses
that two tenants hold identical data, which is a disclosure neither of them agreed to, and it makes
one tenant's deletion depend on another's reference.
[RFC-0016](0016-multi-tenancy-qos-and-security.md) settles the terms: granted by the owner, charged
to the owner, revocable by the owner. The capacity this costs is real and is the price of the
default being off.

## Open questions

- **How many copies the IO path actually has on a realistic stack.** A measurement rather than a
  design decision. Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what
  this project measures.
- **Whether compressing the fast tier is worth what it costs**, which is two questions rather than
  one: whether a block cache addressed by slot can hold variable-length blocks without giving up
  what makes it fast, owned by [RFC-0007](0007-fast-tier-data-path.md), and whether the CPU it takes
  from the dataloader is repaid, owned by [RFC-0018](0018-benchmark-and-falsification-suite.md).
