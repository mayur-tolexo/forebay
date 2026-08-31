<p align="center">
  <img src="docs/diagrams/architecture.svg" alt="Forebay architecture" width="100%">
</p>

# Forebay

**Your GPU nodes are already full of NVMe that sits idle. Forebay turns it into the tier that keeps them fed.**

A forebay is the basin upstream of a hydro turbine. Its only job is to hold enough water that the
turbine never starves, and to absorb surges without passing them downstream. That is the whole idea
here, with GPUs in place of turbines.

Forebay is an open-source storage control plane for AI infrastructure. It borrows unused
compute-local NVMe, turns it into a rack-aware cache and scratch tier, and hands capacity straight
back the moment the compute job wants it. Durable storage stays wherever you already keep it: Ceph,
OpenEBS, S3, or the array you have already paid for.

> **Status: design phase.** There is no usable code yet. The architecture is being worked out in
> the open, one RFC at a time. See [the RFC index](docs/rfcs/README.md) and [ROADMAP.md](ROADMAP.md).
> Early design review is the most useful contribution right now.

## The claim, stated so it can be disproved

Forebay is a bet on two claims, and we keep them separate because they are not equally certain.

1. **Idle compute-local NVMe can be harvested safely.** Capacity can be lent to the storage fabric
   and taken back without migrating data, without a rebalance storm, and without measurably slowing
   the job that owns the node.
2. **That tier delivers more GB/s per GPU** than an equal-cost external array.

Claim 1 is a correctness argument and we are making it now, on paper, in the RFCs. Claim 2 is a
performance argument and **we have not measured it yet.** Every throughput number in this repo is
labelled as unproven until a benchmark says otherwise.

We are publishing the counterexample too, because it is the most likely way this project turns out
to be pointless. In one measured environment, fetching a 226 MiB object from Ceph RGW across eleven
OSDs in four parallel ranges took **0.23 s**, while reading the same payload from the node's own
local disk took **1.71 s**. Aggregate fan-out beat locality by roughly seven times. Different
hardware to what Forebay targets, but the lesson stands: *node-local is not automatically fast.*
Local wins only when a node's device bandwidth exceeds its achievable share of backend fan-out, and
where that crossover sits is exactly what [RFC-0018](docs/rfcs/README.md) sets out to measure.

If the crossover turns out to be unreachable on real hardware, the honest outcome is to say so.

## How it works

Every node's NVMe is split into three pools with different owners and different rules.

| Pool | Owner | Holds | Reclaimed by |
| --- | --- | --- | --- |
| **Compute** | The job on the node | Whatever the job wants | Never touched by Forebay |
| **Donated** | Forebay, permanently | Durable data, via a backend driver | Never reclaimed |
| **Borrowed** | Forebay, at the node's pleasure | Regenerable data only: cache, prefetch, scratch, checkpoint staging | Dropping it |

The rule that makes elasticity safe is the last cell in that table. Borrowed capacity never holds
anything whose loss matters, so reclaiming it is a delete rather than a migration. No storm, no
waiting for a rebalance, no negotiation with the job that needs its disk back.

Two seams keep the rest pluggable. Protocols plug in above, durable backends plug in below, and the
fast tier in the middle is the part Forebay actually owns. That middle is the product; everything
else is deliberately somebody else's.

## Why not just use what exists

Existing systems are good at what they were built for, and Forebay is not trying to replace most of
them. The gap is narrower than a feature matrix suggests.

An enterprise array sees its own media and nothing else. It cannot know that a GPU is idle, that a
cache is missing, or that a node's NVMe is doing nothing this hour, because none of that is visible
from inside the array. A control plane that watches compute *and* storage can act on facts an array
cannot observe, let alone express. That is the structural difference, and it does not depend on any
benchmark to be true.

Distributed caches such as Alluxio sit closer to this idea, but assume the cache tier is theirs.
Forebay's tier is on loan from the compute scheduler and can be revoked mid-read.

## Design principles

- **The control plane is never in the IO path.** It grants leases and sets policy ahead of time.
  With pNFS this stops being a discipline we maintain and becomes something the protocol enforces.
- **Compute always wins.** A job asking for its disk back is not negotiated with.
- **Nothing irreplaceable on borrowed capacity.** This is what makes instant reclamation possible.
- **We do not write a client.** The in-kernel Linux pNFS client is the client.
- **We do not write a durable store.** Ceph, OpenEBS and S3 already exist and are good.
- **Unproven means unproven.** Numbers are labelled until they are measured.

## Explicit non-goals

Forebay is not a NetApp ONTAP clone, and it is not an ONTAP replacement. It does not put durable
data on borrowed capacity. It does not present a single unified namespace across block, file and
object; interfaces stay separate and share metadata and lifecycle instead. It is not a fork of Ceph.
For v1 there is no GPUDirect Storage and no machine-learned access prediction, because manifests and
plain heuristics have to be shown to fall short first.

## Getting involved

The design is unfinished on purpose and the useful contribution today is argument, not code.

- Read [docs/architecture.md](docs/architecture.md) for the long version.
- Browse [the RFC index](docs/rfcs/README.md). Anything marked `Draft` is open for comment, and
  anything with no assignee is open to claim.
- Disagree in an issue. A well-argued objection to RFC-0001 is worth more than a patch right now.
- [CONTRIBUTING.md](CONTRIBUTING.md) covers the process, [GOVERNANCE.md](GOVERNANCE.md) covers who
  decides, and [SECURITY.md](SECURITY.md) covers disclosure.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
