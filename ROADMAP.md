# Roadmap

Forebay is a Kubernetes-native storage control plane for AI infrastructure. This document is both the
plan and the honest status of it.

**Nothing is shipped.** There is no code in this repository yet. Every row below carries a status, and
today none of them says `Shipped`. That is deliberate: a roadmap that reads like a datasheet before
the first commit is how open-source projects lose the people who would otherwise have helped.

| Status | Meaning |
| --- | --- |
| `Shipped` | Exists, is tested, and you can use it |
| `Designed` | An accepted RFC describes it in full |
| `Specified` | An RFC is written and under discussion |
| `Planned` | The problem and the questions are recorded, nobody has written the RFC |
| `Not planned` | Deliberately excluded, with a reason |

## The capability surface

A serious storage platform is judged on two things: the unglamorous capabilities everyone expects,
and whatever it does that nothing else can. Forebay needs both, and the second is worthless without
the first.

### Data services

| Capability | Status | RFC |
| --- | --- | --- |
| Snapshots | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| Instant writable clones, copy on write | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| Thin provisioning | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Compression | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Replication and disaster recovery | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Encryption at rest and in flight | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Tiering between hot and cold media | Planned | [0010](docs/rfcs/0010-autonomy-engine.md) |
| Deduplication | Not planned for v1 | — |
| Immutability and retention locks | Not planned for v1 | — |

Several of these are delegated rather than implemented. Where a backend already does snapshots or
replication well, Forebay drives it instead of reimplementing it, and declares honestly when a
backend cannot.

### Access

| Capability | Status | RFC |
| --- | --- | --- |
| pNFS and NFSv4.2, parallel by design | Specified | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| NFSv3 for compatibility | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| S3 object access | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| Block access through CSI | Planned | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| Shared metadata and lifecycle across protocols | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| SMB | Not planned | — |
| A single unified namespace across block, file and object | Not planned | [0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) |

### Management

| Capability | Status | RFC |
| --- | --- | --- |
| Declarative, intent-based API | Planned | [0009](docs/rfcs/0009-intent-and-policy-model.md) |
| Multi-tenancy and RBAC | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Quotas | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Quality of service, floors and ceilings | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Capacity reporting and planning | Planned | [0017](docs/rfcs/0017-observability.md) |
| Audit logging | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Non-disruptive upgrade | Planned | [0019](docs/rfcs/0019-upgrades-and-operations.md) |
| Draining a node, evacuating a rack | Planned | [0019](docs/rfcs/0019-upgrades-and-operations.md) |
| Telemetry, metrics and tracing | Planned | [0017](docs/rfcs/0017-observability.md) |

### Kubernetes native

Forebay is not a storage appliance with a Kubernetes adapter bolted on. Kubernetes is the only
orchestrator in the MVP, and the control plane's objects are Kubernetes objects.

| Capability | Status | RFC |
| --- | --- | --- |
| CRDs as the primary API | Planned | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| Operator reconciling desired state | Planned | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| CSI driver for volumes and ephemeral volumes | Planned | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| Snapshots and clones through the Kubernetes API | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| Node agent as a DaemonSet | Planned | [0004](docs/rfcs/0004-node-agent.md) |
| Reclamation driven by scheduler signals | Planned | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |

### Extensibility

Two seams, both contracts rather than internal interfaces, so a third party can implement against
them without a fork.

| Capability | Status | RFC |
| --- | --- | --- |
| Durable backend driver contract with capability negotiation | Specified | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Driver conformance suite, so a third-party driver can prove itself | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Ceph driver | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| S3 driver | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| OpenEBS driver | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Protocol plug-ins above the fast tier | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| Bring an existing array as a backend | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |

### What requires seeing the compute

These are the capabilities Forebay exists for. A storage system that cannot observe accelerators
cannot offer them, however good it is at everything above.

| Capability | Status | RFC |
| --- | --- | --- |
| Elastic capacity leased from compute-node NVMe and returned on demand | Specified | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |
| Reclamation by deletion, never by migration | Designed | [0002](docs/rfcs/0002-architecture-overview.md) |
| Placement that follows the accelerator, using GPU, NUMA, PCIe and NIC topology | Planned | [0003](docs/rfcs/0003-topology-model.md) |
| Rack-local fast tier | Specified | [0007](docs/rfcs/0007-fast-tier-data-path.md) |
| Shard-aware prefetch driven by dataset manifests | Planned | [0011](docs/rfcs/0011-prefetch-and-dataset-manifests.md) |
| Checkpoint fast acknowledgement with a stated durability policy | Planned | [0013](docs/rfcs/0013-checkpoint-path.md) |
| Datasets, versions, experiments and checkpoints as first-class objects | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| GB per second per GPU, and GPU stall attributed to storage | Planned | [0017](docs/rfcs/0017-observability.md) |
| Continuous autonomy across compute and storage signals | Planned | [0010](docs/rfcs/0010-autonomy-engine.md) |

## Phases

Ordered by what has to be true before the next thing is worth building. Each phase names what would
make us stop, because the central claim can be wrong.

### Phase 0, design

Writing the architecture down before there is code to defend.

| Work | RFC | Status |
| --- | --- | --- |
| RFC process | [0000](docs/rfcs/0000-rfc-process.md) | Specified |
| Thesis, scope and non-goals | [0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) | Specified |
| Architecture overview | [0002](docs/rfcs/0002-architecture-overview.md) | Specified |
| Core design RFCs | 0003 to 0008 | Planned |

**Done when** the MVP RFCs are accepted and someone who has never spoken to us can read them and say
where they are wrong.

### Phase 1, prove the thesis

The smallest system that establishes whether idle compute-local NVMe can be harvested safely and
usefully. Everything here exists to make the benchmark meaningful.

Topology model ([0003](docs/rfcs/0003-topology-model.md)), node agent
([0004](docs/rfcs/0004-node-agent.md)), capacity pools and elastic leases
([0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md)), the backend driver contract with Ceph
and S3 drivers ([0006](docs/rfcs/0006-durable-backend-driver-contract.md)), the fast tier
([0007](docs/rfcs/0007-fast-tier-data-path.md)), pNFS access
([0008](docs/rfcs/0008-access-layer-pnfs.md)), Kubernetes integration
([0014](docs/rfcs/0014-kubernetes-integration.md)) and the falsification suite
([0018](docs/rfcs/0018-benchmark-and-falsification-suite.md)).

**Done when** a GPU job runs on a node whose spare NVMe is serving the fabric, capacity is reclaimed
mid-job without the job noticing, and the benchmark reports a number either way.

**We stop here if** reclaiming borrowed capacity measurably harms the owning job and no design fixes
it, or the fast tier cannot beat the durable backend's own parallel fan-out on target hardware. The
second is the serious one, and it is the counterexample described in the README.

### Phase 2, intent and autonomy

The part that makes Forebay a control plane rather than a cache. Intent and policy
([0009](docs/rfcs/0009-intent-and-policy-model.md)), the autonomy engine
([0010](docs/rfcs/0010-autonomy-engine.md)), and the observability needed to tell whether its
decisions were good ([0017](docs/rfcs/0017-observability.md)). Autonomy without measurement is
guessing with extra steps, so 0017 is not optional here.

**Done when** the system moves data on its own, every decision can be explained after the fact, and
operators trust it enough to leave it on.

### Phase 3, the AI layer

Prefetch and manifests ([0011](docs/rfcs/0011-prefetch-and-dataset-manifests.md)), the dataset object
model ([0012](docs/rfcs/0012-dataset-object-model.md)), and the checkpoint path
([0013](docs/rfcs/0013-checkpoint-path.md)).

This is where Forebay stops looking like generic storage. It comes after the thesis is settled,
because an elegant dataset API on a tier that does not pay for itself is decoration.

### Phase 4, production

Failure model ([0015](docs/rfcs/0015-failure-model.md)), multi-tenancy, QoS and security
([0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md)), and non-disruptive upgrades
([0019](docs/rfcs/0019-upgrades-and-operations.md)).

These are the reasons people trust storage, and none of them are interesting until the thing works.
Their absence is why Forebay will say pre-production for a long time, and saying so is more useful
than a version number implying otherwise.

## Deliberately excluded

| Not doing | Why |
| --- | --- |
| Durable data on borrowed capacity | It would make reclamation a migration, which is the storm the design exists to avoid |
| Writing a durable store | Ceph, OpenEBS and S3 exist, are good, and are already deployed where the users are |
| Writing a client | The in-kernel Linux pNFS client is the client. Shipping one across kernels is where storage projects bleed |
| SMB | No AI workload has asked for it |
| Deduplication | Expensive to do well, and AI datasets are poor candidates for it |
| A unified namespace across block, file and object | Multiplies consistency problems for a benefit nobody has requested |
| GPUDirect Storage in v1 | Real and probably valuable, but it constrains hardware and needs the rest of the path fast first |
| Machine-learned access prediction in v1 | Manifests and heuristics have to be shown to fall short before a model earns its operational cost |

Each exclusion can be revisited by an RFC that supersedes the one recording it.
