# RFC-0020: The no-copy policy

| | |
| --- | --- |
| **Status** | Specified |
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

## Open questions

- The unit of sharing. Extent, object or shard, and how that interacts with backends whose native
  granularity differs.
- Whether register-in-place ingest can be offered for every backend, or only for those that expose
  stable addressing for existing data.
- How many copies the IO path actually has on a realistic stack, which is a measurement rather than a
  design decision and belongs in [0018](0018-benchmark-and-falsification-suite.md).
- Whether extent sharing between tenants is ever acceptable, given that it leaks the fact of
  identical data. The likely answer is no, and it costs real capacity.
