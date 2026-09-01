# Roadmap

Forebay is a Kubernetes-native storage control plane for AI infrastructure. This document is both the
plan and the honest status of it.

**Nothing is usable yet.** The first packages exist and are tested, but nothing is wired to a device
or a cluster, so no row below says `Shipped`. That is deliberate: a roadmap that reads like a
datasheet before anything runs is how open-source projects lose the people who would otherwise have
helped.

| Status | Meaning |
| --- | --- |
| `Shipped` | Exists, is tested, and you can use it |
| `In progress` | Code exists and is tested, but nothing is wired up end to end |
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
| Compression, delegated to the backend for data registered in place | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md), [0020](docs/rfcs/0020-no-copy-policy.md) |
| Replication and disaster recovery | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Encryption at rest and in flight | Planned | [0016](docs/rfcs/0016-multi-tenancy-qos-and-security.md) |
| Tiering between hot and cold media | Planned | [0010](docs/rfcs/0010-autonomy-engine.md) |
| No copy to clone, version, tier or serve a second protocol | Specified | [0020](docs/rfcs/0020-no-copy-policy.md) |
| Register data in place, no copy on ingest | Specified | [0020](docs/rfcs/0020-no-copy-policy.md) |
| Extent sharing between dataset versions | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| Minimum-copy IO path, io_uring and RDMA where available | Planned | [0020](docs/rfcs/0020-no-copy-policy.md) |
| Deduplication across unrelated data | Not planned for v1 | [0020](docs/rfcs/0020-no-copy-policy.md) |
| Immutability and retention locks | Not planned for v1 | — |

Several of these are delegated rather than implemented. Where a backend already does snapshots or
replication well, Forebay drives it instead of reimplementing it, and declares honestly when a
backend cannot.

### Access

| Capability | Status | RFC |
| --- | --- | --- |
| pNFS and NFSv4.2, parallel by design | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| NFSv3 for compatibility | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| S3 object access | Planned | [0008](docs/rfcs/0008-access-layer-pnfs.md) |
| Block access through CSI | Planned | [0014](docs/rfcs/0014-kubernetes-integration.md) |
| Write once, read as file **and** object over the same bytes | Specified | [0021](docs/rfcs/0021-single-copy-multi-protocol.md) |
| Block under the same namespace, policy and snapshots | Specified | [0021](docs/rfcs/0021-single-copy-multi-protocol.md) |
| Snapshot export between block and object | Planned | [0021](docs/rfcs/0021-single-copy-multi-protocol.md) |
| SMB | Not planned | — |
| Concurrent block access to the same bytes as file or object | Not possible | [0021](docs/rfcs/0021-single-copy-multi-protocol.md) |

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
| Durable backend driver contract with capability negotiation | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
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
| Elastic capacity leased from compute-node NVMe and returned on demand | In progress | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |
| Reclamation by deletion, never by migration | In progress | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |
| Placement that follows the accelerator, using GPU, NUMA, PCIe and NIC topology | Planned | [0003](docs/rfcs/0003-topology-model.md) |
| Rack-local fast tier | Planned | [0007](docs/rfcs/0007-fast-tier-data-path.md) |
| Shard-aware prefetch driven by dataset manifests | Planned | [0011](docs/rfcs/0011-prefetch-and-dataset-manifests.md) |
| Checkpoint fast acknowledgement with a stated durability policy | Planned | [0013](docs/rfcs/0013-checkpoint-path.md) |
| Datasets, versions, experiments and checkpoints as first-class objects | Planned | [0012](docs/rfcs/0012-dataset-object-model.md) |
| GB per second per GPU, and GPU stall attributed to storage | Planned | [0017](docs/rfcs/0017-observability.md) |
| Continuous autonomy across compute and storage signals | Planned | [0010](docs/rfcs/0010-autonomy-engine.md) |
| Data-aware scheduling, telling the scheduler where the data already is | Planned | [0022](docs/rfcs/0022-data-aware-scheduling.md) |
| Warm start, pre-filling a rack before the pod is admitted | Planned | [0022](docs/rfcs/0022-data-aware-scheduling.md) |
| Lineage from dataset version to experiment to checkpoint to model | Planned | [0023](docs/rfcs/0023-lineage-and-reproducibility.md) |
| GPU hours lost to storage, costed per dataset and per tenant | Planned | [0024](docs/rfcs/0024-efficiency-accounting.md) |
| Cross-cluster and cross-region immutable dataset distribution | Planned | [0025](docs/rfcs/0025-cross-cluster-datasets.md) |

## Phases

Ordered by what has to be true before the next thing is worth building. Each phase names what would
make us stop, because the central claim can be wrong.

### Phase 0, design

Writing the architecture down before there is code to defend.

Statuses below are RFC lifecycle states from [RFC-0000](docs/rfcs/0000-rfc-process.md), not the
capability states above. An RFC is `Draft` while it is being argued with and `Accepted` once its
assumptions carry an honest basis and its open questions are answered or deferred to a named owner.

| Work | RFC | Status |
| --- | --- | --- |
| Thesis, scope and non-goals | [0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) | **Accepted** |
| RFC process | [0000](docs/rfcs/0000-rfc-process.md) | Draft |
| Architecture overview | [0002](docs/rfcs/0002-architecture-overview.md) | Draft |
| Node agent | [0004](docs/rfcs/0004-node-agent.md) | Draft |
| Capacity pools and elastic leases | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) | Draft |
| Topology, drivers, fast tier, access layer | 0003, 0006, 0007, 0008 | Not started |

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

**Started.** Capacity accounting, the lease state machine and the lease journal are implemented and
tested in `internal/pool` and `internal/lease`. A node now survives a restart knowing what it lent.
None of it is wired to a device, an agent or a control plane, so nothing reclaims real capacity yet,
and the restore path has therefore only ever run under test. The first real caller will be the one to
find whatever the unit tests did not, which is a thing for RFC-0004 to carry rather than assume
settled.

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
| Deduplication across unrelated data | Expensive to do well. Extent sharing between versions of the same dataset gives most of the benefit for almost none of the cost |
| Concurrent block access to the same bytes as file or object | Not achievable. A block volume is an opaque range with a client-owned filesystem inside it, so there are no objects in there to serve |
| GPUDirect Storage in v1 | Real and probably valuable, but it constrains hardware and needs the rest of the path fast first |
| Machine-learned access prediction in v1 | Manifests and heuristics have to be shown to fall short before a model earns its operational cost |

Each exclusion can be revisited by an RFC that supersedes the one recording it.
