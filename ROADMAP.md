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
| Node agent as a DaemonSet | In progress | [0004](docs/rfcs/0004-node-agent.md) |
| Reclamation driven by scheduler signals | Planned | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) |

### Extensibility

Two seams, both contracts rather than internal interfaces, so a third party can implement against
them without a fork.

| Capability | Status | RFC |
| --- | --- | --- |
| Durable backend driver contract with capability negotiation | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Driver conformance suite, so a third-party driver can prove itself | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| Ceph driver | Planned | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
| S3 driver | In progress | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) |
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
| Placement that follows the accelerator, using GPU, NUMA, PCIe and NIC topology | In progress | [0003](docs/rfcs/0003-topology-model.md) |
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
| RFC process | [0000](docs/rfcs/0000-rfc-process.md) | **Accepted** |
| Thesis, scope and non-goals | [0001](docs/rfcs/0001-thesis-scope-and-non-goals.md) | **Accepted** |
| Architecture overview | [0002](docs/rfcs/0002-architecture-overview.md) | **Accepted** |
| Topology model | [0003](docs/rfcs/0003-topology-model.md) | **Accepted** |
| Node agent | [0004](docs/rfcs/0004-node-agent.md) | **Accepted** |
| Capacity pools and elastic leases | [0005](docs/rfcs/0005-capacity-pools-and-elastic-leases.md) | **Accepted** |
| Durable backend driver contract | [0006](docs/rfcs/0006-durable-backend-driver-contract.md) | **Accepted** |
| Fast tier data path | [0007](docs/rfcs/0007-fast-tier-data-path.md) | **Accepted** |
| Access layer, pNFS | [0008](docs/rfcs/0008-access-layer-pnfs.md) | **Accepted** |
| Benchmark and falsification suite | [0018](docs/rfcs/0018-benchmark-and-falsification-suite.md) | **Accepted** |

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

**Started.** Capacity accounting, the lease state machine and the lease journal are in
`internal/pool` and `internal/lease`, and the agent's startup path is in `internal/agent`: it owns
the pool directories, holds the node lock, replays the journal and reconciles it against the disk in
both directions.

Leases now put bytes on disk. Granting one preallocates a whole extent, reclaiming invalidates it
before unlinking it, and an interrupted reclaim leaves a file the next startup removes. Until this,
a lease was bookkeeping: the accounting could record a terabyte lent above an empty pool.

Topology discovery is in `internal/topology` and the agent uses it, so a node reads its own capacity
rather than being told, measured on the filesystem the pools sit on rather than summed across every
drive in the machine, and reduced by the space that filesystem already holds for everything which is
not Forebay. On a GPU node it identified both accelerators by vendor, found the NVMe,
declined to count an attached Ceph RBD as local capacity, and reported NUMA affinity as unknown
because the kernel says -1.

The agent stays running and reclaims on its own. It keeps a floor of free space, polls the
filesystem, and returns the shortfall when a workload takes space nobody declared. The reclaim is
timed against the deadline the elastic class promises, and overrunning it is an error rather than a
log line. While it runs it publishes a heartbeat, so a wedged agent is killed by its own liveness
probe and its replacement can take the lock, which the node lock alone could not do.

All of it has been run on a GPU node with two RTX 5090s and 1.86 TiB of NVMe: the capacity split,
unlinking capacity nothing accounted for, leaving donated data untouched, refusing every layout it
should refuse, reclaiming 128 MiB when a workload ate the headroom, and a wedged holder killed and
replaced under a real kubelet probe.

The backend seam exists. `driver` is the capability contract from
[0006](docs/rfcs/0006-durable-backend-driver-contract.md), where a driver declares what it can do and
anything undeclared is refused before the driver is reached, so emulation is unreachable rather than
merely forbidden. `driver/conformance` is importable, so a third party can demonstrate a driver for a
store this project has never seen.

The fast tier's node-local half is in `internal/fasttier`
([0007](docs/rfcs/0007-fast-tier-data-path.md)): fixed-size blocks in a lease's extent, admission on
the second read so a single epoch cannot empty the cache, eviction preferring capacity that is
leaving anyway, and a revoked block read as a miss rather than an error.

The watch now sees pressure before it lands. `internal/kubelet`
([0014](docs/rfcs/0014-kubernetes-integration.md)) reads pods from the node's own kubelet rather than
the API server, so a partition cannot block reclamation, and counts what live pods have asked for and
not yet written, which polled free space cannot see until it is gone. On a GPU node, holding an 8 GiB
lease against a target six GiB below free space, polling saw nothing and the pod input saw 4 GiB and
took the lease back. That node also answered a question the RFC had marked unverified and not in the
input's favour: 3 of its 64 pods declared an ephemeral-storage request at all.

Something reads from the tier. `internal/dataserver`
([0008](docs/rfcs/0008-access-layer-pnfs.md)) answers a byte range from the fast tier where the
blocks are resident and from the durable backend where they are not, and absorbs the miss rather than
passing it on, because the caller above it will be an unmodified NFS client that cannot be told to
try again. It is the first caller `internal/fasttier` has outside its own tests and the first the
driver contract has at all. Serving the last block of an object needed the contract to grow an
optional `object-size`: without it an object smaller than one block is entirely tail and is never
cached, which five reads of a hundred-byte object made ten backend requests to demonstrate.

The pNFS half is a spike rather than code, and it answered its question. A 172-line FSAL over
NFS-Ganesha V6.5 advertises the flexible file layout and a stock Linux 6.8 client negotiates it,
reporting `pnfs=LAYOUT_FLEX_FILES` where the same export without the FSAL reports
`pnfs=not configured`. Two flexfiles helper symbols had to be exported from Ganesha first, and two
more are missing on the same terms without this FSAL needing them: they are public in the headers
and left off the version script, so an FSAL that calls them does not link. That is a patch upstream
rather than a fork.

A byte has moved through it. `fsal/` holds the protocol's second implementation in C and a read hook
for an NFS server's own file layer, and with those a stock Linux 6.8 client mounted an export and
read an object whose bytes summed to what the backend holds rather than to what that file layer would
have returned itself. Writing that client cost three behaviours the Go side had already learned: a
deadline that covers an exchange rather than each poll, a dial that is retried rather than given up
on, and a failure that is an error rather than a fallback filling a client's buffer with padding.

It is a demonstration and not a system. The namespace belonged to the NFS server and only the bytes
were Forebay's; a real metadata server hands out layouts and a real data server carries its own
handles.

The binary joins them. Until now `internal/fasttier`, the driver contract and the read path had
callers only in their own tests, so the agent guarded capacity nothing read from. Given a socket and
a backend it opens both, holds a tier over capacity it grants itself in the absence of a control
plane, and answers reads. On a GPU node it read a 64 MiB object back three times with the checksum
intact, and across three restarts, which is where two faults turned up that no test had: the tier's
lease outlived the process that granted it so the second start refused to serve, and serving without
watching printed that it was answering reads and then exited, taking the socket with it.

Putting a tier and a reclaiming agent in one process found a third, older than any of this. Nothing
told the tier when its capacity was taken back, so the extent was unlinked while the tier still held
it open: the agent reported returning 64 MiB and free space rose by four kilobytes, because blocks
belong to a descriptor rather than to a name. The holder is now told before the unlink rather than
after, which is the difference between the promise being kept and the accounting saying it was.

A driver reads from a real object store. `driver/s3driver`
([0006](docs/rfcs/0006-durable-backend-driver-contract.md)) signs its own requests, because a vendor
SDK would be larger than everything else here put together and this project takes no dependencies.
It declares read-range, object-size, write-object and delete-object, and declines snapshot and clone
rather than emulating them with a copy.

It passes the conformance suite against Ceph RGW, not only against a fake of one. That distinction
earned itself: a store answers a read running off the end of an object two different ways, a 416 for
an offset at the end and a short 206 for a length that overruns, and a fake is written to agree with
whatever the driver already does. The signer is checked against Amazon's own published vector, which
is the one part that is either exactly right or silently wrong.

The agent reads from that store rather than from a directory. `--backend-s3-endpoint` and
`--backend-s3-bucket` stand in for `--backend-dir`, one or the other and never both, since a node
reading from two stores would serve whichever the flags named first and nothing in a client's answer
would show it. Credentials come from the environment: a flag is visible in ps and in a container's
own spec.

On a GPU node, against Ceph RGW, a client read a 9 MiB object through the agent and the checksum
matched the object. Deleting the object from the store and reading it again returned the same
checksum, which is the fast tier answering rather than the backend, while a read of a range it had
never held went to the store and failed as it should.

The suite that is meant to kill this exists, and has not. `internal/bench` and `cmd/forebay-bench`
run the crossover experiment from [0018](docs/rfcs/0018-benchmark-and-falsification-suite.md): one
plan against every arm, checksummed so arms that disagree about the bytes cannot be compared on
speed, and the conditions printed before the number. Reading a 256 MiB object with the tier's extent
evicted from the page cache, so the bytes come off the device, the tier serves 341 MiB/s at one
reader against the backend's 72, and 2003 against 110 at sixteen. There is no crossover inside the
sweep. Left in the page cache the same tier reads 465 and 4419, so caching was worth up to two and a
half times and is not where the result comes from.

Reclamation does not harm the job that owns the node, which is the other criterion that could have
ended it. Taking back 16 GiB across 32 leases leaves a writer's rate and its worst single write where
they were, in every state the device has, read against a control arm that lends the same capacity and
never takes it back. What that did change is a number this document used to rest on: a reclaim is
3.7 ms on an idle device and 142 to 773 ms on one held at its sustained write rate, which is two
orders of magnitude nearer the deadline than the earlier figure implied.

The headroom target has been measured, and it is not a number. What a node has to keep free is what
its workload writes while the watch is not looking, which is a rate times a poll interval, and the
same drive gives rates sixty times apart depending on whether its cache is spent. So it is configured
as a duration and converted each pass against the rate the agent observes, corrected for what the
agent itself gave back: without that correction a reclaim would read as the workload slowing down and
the floor would shrink in the pass that had just proved it too small.

What is left is most of it. One of the watch's three inputs is still missing, the CSI one. No Ceph
driver exists, there is no peer fetch and no control plane interface, and nothing an unmodified job
can mount: the access layer is a spike that proves a client can read Forebay's bytes, not the layer.

**Done when** a GPU job runs on a node whose spare NVMe is serving the fabric, capacity is reclaimed
mid-job without the job noticing, and the benchmark reports a number either way.

**We stop here if** reclaiming borrowed capacity measurably harms the owning job and no design fixes
it, or the fast tier cannot beat the durable backend's own parallel fan-out on target hardware. The
second is the serious one, and it is the counterexample described in the README.

Both have now been put to one node, and neither fired. That is one node, one backend and one day: it
is enough to have stopped the project and not enough to declare it right, which is the asymmetry a
kill criterion has. The store's own first-touch reads varied between 34 and 110 MiB/s across runs, so
what the tier beat is a number with a spread on it.

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

KV cache spill and the inference path ([0027](docs/rfcs/0027-kv-cache-spill.md)) sits here by
ordering rather than by theme, and it is conditional rather than planned. Everything else in this
roadmap is shaped by training. Serving is the other half of what a GPU fleet does, and the fast tier
already describes KV blocks almost without change, because they are immutable, addressed by content
and regenerable. What is not established is that reading one back from NVMe beats recomputing it,
which is a far tighter contest than any other Forebay enters, and RFC-0018 owns the measurement. If
it comes back badly the RFC is rejected rather than deferred.

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
