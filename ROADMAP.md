# Roadmap

Forebay is in Phase 0. Nothing below is shipped, and the phases are ordered by what has to be true
before the next thing is worth building rather than by what would be nicest to have.

Each phase names what would make us stop. A roadmap without exit criteria is a wish list, and this
project's central claim is one that can be wrong.

## Phase 0 — Design

Writing down the architecture and the reasoning behind it, in the open, before there is code to
defend.

| Work | RFC | Status |
| --- | --- | --- |
| RFC process | [0000](docs/rfcs/0000-rfc-process.md) | Draft |
| Thesis, scope and non-goals | [0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) | Draft |
| Architecture overview | [0002](docs/rfcs/0002-architecture-overview.md) | Draft |
| Core design RFCs drafted | 0003 to 0008 | Not started |

**Done when** the MVP RFCs are accepted and someone who has never spoken to us can read them and
say where they are wrong.

## Phase 1 — Prove the thesis

The smallest system that establishes whether idle compute-local NVMe can be harvested safely and
usefully. Everything here exists to make the benchmark in RFC-0018 meaningful.

| Work | RFC |
| --- | --- |
| Topology discovery and model | [0003](docs/rfcs/0003-topology-model.md) |
| Node agent | [0004](docs/rfcs/0004-node-agent.md) |
| Capacity pools and elastic leases | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |
| Durable backend driver contract, with Ceph and S3 drivers | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Fast tier data path | [0007](docs/rfcs/0007-fast-tier-data-path.md) |
| Access layer over pNFS | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| Kubernetes integration | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| Benchmark and falsification suite | [0018](docs/rfcs/0018-benchmark-and-falsification-suite.md) |

**Done when** a GPU job runs on a node whose spare NVMe is serving the fabric, capacity is reclaimed
mid-job without the job noticing, and the benchmark reports a number either way.

**We stop here if** reclaiming borrowed capacity measurably harms the owning job and no design fixes
it, or the fast tier cannot beat the durable backend's own parallel fan-out on target hardware. The
second is the serious one: it is the counterexample described in the README, and it would mean the
locality premise does not hold where it matters.

## Phase 2 — Intent and autonomy

The part that makes Forebay a control plane rather than a cache.

Intent and policy model (0009), the autonomy engine and its two control loops (0010), and the
observability needed to tell whether any of its decisions were good (0017). Autonomy without
measurement is guessing with extra steps, so 0017 is not optional here.

**Done when** the system moves data on its own, the reason for every decision is visible after the
fact, and operators trust it enough to leave it on.

## Phase 3 — The AI layer

Prefetch and dataset manifests (0011), the dataset, version, snapshot and clone object model (0012),
and the checkpoint path with fast acknowledgement and tiered durability (0013).

This is where Forebay stops looking like generic storage. It is deliberately after the thesis is
settled, because an elegant dataset API on top of a tier that does not pay for itself is decoration.

## Phase 4 — Production

Failure model and split brain (0015), multi-tenancy, QoS and security (0016), and non-disruptive
upgrades (0019).

These are the reasons people trust storage, and none of them are interesting until the thing works.
Their absence is why Forebay will say pre-production for a long time, and saying so is more useful
than a version number that implies otherwise.

## Not on the roadmap

GPUDirect Storage, machine-learned access prediction, a unified namespace across block, file and
object, and any durable data on borrowed capacity. Each is excluded for a reason recorded in
[RFC-0001](docs/rfcs/0001-thesis-scope-and-non-goals.md). If one of those reasons stops holding, the
way back in is an RFC that supersedes it.
