<p align="center">
  <img src="docs/diagrams/architecture.svg" alt="Forebay architecture" width="100%">
</p>

<h1 align="center">Forebay</h1>

<p align="center">
  <strong>A Kubernetes-native storage control plane for AI infrastructure.</strong><br>
  Your GPU nodes are already full of NVMe that sits idle. Forebay turns it into the tier that keeps them fed.
</p>

<p align="center">
  <a href="LICENSE"><img alt="Licence" src="https://img.shields.io/badge/licence-Apache--2.0-4F46E5"></a>
  <img alt="Status" src="https://img.shields.io/badge/status-design%20phase-F59E0B">
  <img alt="Code" src="https://img.shields.io/badge/code-none%20yet-64748B">
  <a href="docs/rfcs/README.md"><img alt="RFCs" src="https://img.shields.io/badge/RFCs-19-14B8A6"></a>
</p>

---

A forebay is the basin upstream of a hydro turbine. Its only job is to hold enough water that the
turbine never starves, and to absorb surges without passing them downstream. That is the whole idea
here, with GPUs in place of turbines.

Forebay borrows unused compute-local NVMe, turns it into a rack-aware cache and scratch tier, and
hands capacity straight back the moment the compute job wants it. Durable storage stays wherever you
already keep it: Ceph, OpenEBS, S3, or the array you have already paid for.

> ### Status: design phase
>
> **There is no code in this repository yet.** The architecture is being worked out in the open, one
> RFC at a time, and the [capability table](#capabilities) below marks every single line `Planned`
> or `Specified`. Nothing says `Shipped`.
>
> Design review is worth more to this project right now than any patch. Start with
> [RFC-0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) and try to break it.

## What it does for you

<p align="center">
  <img src="docs/diagrams/capabilities.svg" alt="What Forebay does for you" width="100%">
</p>

You describe an outcome. Forebay decides how to reach it, continuously, from what it can observe on
both the storage and the compute side. The work that disappears is the work that used to be yours:
sizing a cache tier by hand, migrating data to free up space, buying capacity the fleet already has,
and guessing why a GPU is waiting.

## The claim, stated so it can be disproved

Forebay is a bet on two claims, kept separate because they are not equally certain.

1. **Idle compute-local NVMe can be harvested safely.** Capacity can be lent to the storage fabric
   and taken back without migrating data, without a rebalance storm, and without measurably slowing
   the job that owns the node.
2. **That tier delivers more GB/s per GPU** than an equal-cost external array.

Claim 1 is a correctness argument and we are making it now, on paper, in the RFCs. Claim 2 is a
performance argument and **we have not measured it yet.** Every throughput number in this repository
is labelled unproven until a benchmark says otherwise.

We publish the counterexample too, because it is the most likely way this project turns out to be
pointless. In one measured environment, fetching a 226 MiB object from Ceph RGW across eleven OSDs in
four parallel ranges took **0.23 s**, while reading the same payload from the node's own local disk
took **1.71 s**. Aggregate fan-out beat locality by roughly seven times. Different hardware to what
Forebay targets, but the lesson stands: *node-local is not automatically fast.* Local wins only when
a node's device bandwidth exceeds its achievable share of backend fan-out, and where that crossover
sits is exactly what [RFC-0018](docs/rfcs/0018-benchmark-and-falsification-suite.md) exists to
measure.

If the crossover turns out to be unreachable on real hardware, the honest outcome is to say so.
[RFC-0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) lists five conditions under which this
project should be abandoned.

## How it works

Every node's NVMe is split into three pools with different owners and different rules.

| Pool | Owner | Holds | Reclaimed by |
| --- | --- | --- | --- |
| **Compute** | The job on the node | Whatever the job wants | Never touched by Forebay |
| **Donated** | Forebay, permanently | Durable data, via a backend driver | Never reclaimed |
| **Borrowed** | Forebay, at the node's pleasure | Regenerable data only: cache, prefetch, scratch, checkpoint staging | Dropping it |

The rule that makes elasticity safe is the last cell. Borrowed capacity never holds anything whose
loss matters, so reclaiming it is a delete rather than a migration. No storm, no waiting for a
rebalance, no negotiation with the job that needs its disk back.

## Kubernetes native

Forebay is not an appliance with a Kubernetes adapter bolted on. Kubernetes is the only orchestrator
in the MVP, its objects are the API, and the signals that drive reclamation come from the scheduler
itself.

- CRDs are the primary interface, with an operator reconciling desired state.
- A CSI driver covers volumes and ephemeral volumes; snapshots and clones go through the Kubernetes
  API rather than a side channel.
- The node agent is a DaemonSet, and it learns that compute wants its capacity back from pod
  admission, ephemeral-storage requests and eviction pressure.

## Extensible by contract

Forebay is pluggable at the edges and opinionated in exactly one place.

**Protocols plug in above.** pNFS and NFSv4.2 first, then NFSv3, S3 and CSI block.

**Durable backends plug in below,** through a contract that is deliberately not a lowest common
denominator. Each driver declares what it can do — snapshots, clones, thin provisioning, replication,
topology hints, ranged reads — and the control plane uses what exists and **refuses** an intent no
backend can satisfy. Silent degradation in a storage system is how data ends up less durable than its
owner believes. A conformance suite lets a third-party driver prove itself without a fork.

**The fast tier in the middle is owned outright and is not pluggable.** That is the deliberate part.
A system that is pluggable everywhere can only express what its backends have in common, and can only
act through knobs they expose, which makes autonomous optimisation impossible by construction. Owning
exactly one layer is what stops this from becoming an orchestrator for other people's storage.

## Capabilities

Full matrix with per-item status in [ROADMAP.md](ROADMAP.md). Summarised:

| Area | Highlights | Status |
| --- | --- | --- |
| **Data services** | Snapshots, instant CoW clones, thin provisioning, compression, replication, encryption, tiering | Planned |
| **Access** | pNFS and NFSv4.2, NFSv3, S3, CSI block, shared metadata across protocols | Specified / Planned |
| **Management** | Intent-based API, multi-tenancy, RBAC, quotas, QoS, audit, capacity reporting, non-disruptive upgrade | Planned |
| **Kubernetes** | CRDs, operator, CSI, DaemonSet agent, scheduler-driven reclamation | Planned |
| **Extensibility** | Backend driver contract with capability negotiation, conformance suite, protocol plug-ins | Specified / Planned |
| **Needs to see the compute** | Elastic NVMe leases, reclaim by deletion, accelerator-aware placement, rack-local tier, shard-aware prefetch, checkpoint fast-ack, dataset and version objects, GB/s per GPU | Specified / Planned |

The last row is why Forebay exists. Everything above it is table stakes that a storage platform has
to earn before anyone will trust the interesting part.

## Roadmap

| Phase | What | Ends when |
| --- | --- | --- |
| **0 · Design** | Architecture and RFCs, in the open, before there is code to defend | The MVP RFCs are accepted and a stranger can say where they are wrong |
| **1 · Prove the thesis** | Node agent, leases, backend drivers, fast tier, pNFS, Kubernetes, benchmarks | A GPU job runs while its spare NVMe serves the fabric, capacity is reclaimed mid-job unnoticed, and the benchmark reports a number either way |
| **2 · Intent and autonomy** | Intent model, the two control loops, observability | The system moves data on its own, every decision is explainable, and operators leave it on |
| **3 · The AI layer** | Prefetch and manifests, dataset and version objects, checkpoint path | Forebay stops looking like generic storage |
| **4 · Production** | Failure model, multi-tenancy and QoS, non-disruptive upgrades | The boring reasons people trust storage are all present |

Each phase in [ROADMAP.md](ROADMAP.md) also names what would make us stop.

## Why not just use what exists

Existing systems are good at what they were built for, and Forebay is not trying to replace most of
them. The gap is narrower than a feature matrix suggests.

An enterprise array sees its own media and nothing else. It cannot know that a GPU is idle, that a
cache is missing, or that a node's NVMe is doing nothing this hour, because none of that is visible
from inside the array. A control plane that watches compute *and* storage can act on facts an array
cannot observe, let alone express. That difference is structural, and it does not depend on any
benchmark to be true.

Burst buffers, BeeGFS On Demand and converged deployments of commercial parallel filesystems have all
established that compute-local media can serve storage, and
[RFC-0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) says so plainly rather than claiming
novelty. What none of them appears to do is treat the boundary between compute and storage as
continuously negotiable at fleet scale. That is the part worth attacking.

## Design principles

- **The control plane is never in the IO path.** With pNFS this stops being a discipline we maintain
  and becomes a property of the protocol.
- **Compute always wins.** A job asking for its disk back is not negotiated with.
- **Nothing irreplaceable on borrowed capacity.** This is what makes instant reclamation possible.
- **Refuse rather than degrade silently.** An intent no backend can satisfy is an error.
- **We do not write a client.** The in-kernel Linux pNFS client is the client.
- **We do not write a durable store.** Ceph, OpenEBS and S3 already exist and are good.
- **Unproven means unproven.** Numbers are labelled until they are measured.

## Explicit non-goals

Forebay does not clone any incumbent array's feature set, and it is not an array replacement. It does
not put durable data on borrowed capacity. It does not present a unified namespace across block, file
and object; interfaces stay separate and share metadata and lifecycle instead. It is not a fork of
Ceph. For v1 there is no GPUDirect Storage and no machine-learned access prediction, because
manifests and plain heuristics have to be shown to fall short first. Reasons for each are recorded in
[ROADMAP.md](ROADMAP.md).

## Getting involved

The design is unfinished on purpose, and the useful contribution today is argument, not code.

- Read [docs/architecture.md](docs/architecture.md) for the long version.
- Browse [the RFC index](docs/rfcs/README.md). Anything marked `Not started` has a problem statement
  and the questions it must answer, and is open to claim.
- Disagree in an issue. A well-argued objection to RFC-0001 is worth more than a patch right now.
- [CONTRIBUTING.md](CONTRIBUTING.md) covers process, [GOVERNANCE.md](GOVERNANCE.md) covers who
  decides, [SECURITY.md](SECURITY.md) covers disclosure.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
