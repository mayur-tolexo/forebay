# RFC-0001: Thesis, scope and non-goals

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 0 |
| **Depends on** | — |

## Problem

A GPU cluster contains two expensive things: accelerators, and the NVMe attached to the nodes those
accelerators live in. The accelerators are scheduled carefully. The NVMe usually is not.

A node running an inference workload with a small working set may use a fraction of its local disk
for hours. A node between jobs uses none of it. Across a fleet this is a large amount of fast media
sitting still, bought and powered, doing nothing. Meanwhile the same cluster reads its training data
across the network from a central system that has to serve every node at once.

The consequence is not that storage is slow in the abstract. It is that GPUs wait. A GPU that is
blocked on data is the most expensive idle resource in the building, and the cost is paid per hour
whether or not anyone measures it.

The question this project exists to answer is whether the idle media and the starving accelerators
can be connected to each other safely.

## The thesis

Two claims, deliberately separated because they have different standards of proof.

**Claim 1, correctness.** Idle compute-local NVMe can be lent to a storage fabric and taken back
without migrating data, without a rebalance storm, and without measurably slowing the job that owns
the node.

**Claim 2, performance.** A tier built from that borrowed capacity delivers more GB/s per GPU than
an equal-cost external system.

Claim 1 is argued on paper and settled by design. It is the subject of RFC-0005.

Claim 2 is not settled and cannot be settled here. Forebay has no hardware to measure on at the time
of writing. Every throughput statement in this repository is labelled as predicted until RFC-0018
produces a number.

### The counterexample we start from

In one measured environment, fetching a 226 MiB compressed object from a Ceph RGW cluster across
eleven SSD OSDs using four parallel ranges took **0.23 s**, roughly 995 MB/s. Reading the same
payload from the same node's own local disk with O_DIRECT took **1.71 s**, roughly 400 MB/s, and did
not improve with concurrency.

Aggregate fan-out beat locality by about seven times on wall clock, for the same logical payload.
That is the number a job waiting on its data actually experiences.

On raw bandwidth the gap is closer to two and a half times, 995 MB/s against 400, because the object
crossing the network was compressed roughly three to one while the local read moved every byte. Both
numbers appear here because the distance between them is precisely the sort of thing a reader should
not have to reconstruct, and because a document arguing for honest measurement cannot quote the
flattering one alone.

That hardware is not what Forebay targets. Those were general-purpose nodes with LVM-backed volumes,
not GPU nodes with directly attached Gen5 NVMe. The comparison is not apples to apples and it does
not disprove anything.

It is recorded here, at the front, because it is the cleanest available reminder that **node-local is
not automatically fast**. A single device has a fixed ceiling. A backend spread over many devices
does not, until the network or the backend's own coordination becomes the limit. Locality wins only
when a node's device bandwidth exceeds the share of backend fan-out that node could otherwise
obtain, and nobody involved in this project currently knows where that crossover sits on modern GPU
hardware.

Finding out is the first serious engineering task, not a detail to be tidied up later.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A meaningful fraction of compute-local NVMe is idle for meaningful periods | Reasoned, from how GPU clusters are provisioned | If utilisation is already high, there is nothing to borrow and the project has no premise |
| Direct-attached NVMe on GPU nodes substantially outruns a node's achievable share of a shared backend | Unverified | Claim 2 fails, and Forebay is at best a convenience layer |
| Regenerable data is a large enough share of AI storage traffic to be worth a dedicated tier | Reasoned, from cache, prefetch, scratch and checkpoint staging patterns | The borrowed pool is too small to matter and only the donated pool is useful |
| Capacity can be reclaimed fast enough that compute never has to wait | Partly measured. Reclaim through the agent is 2.8 ms for 7 GiB, rising to 7.4 ms under concurrent write load, so the filesystem is not the constraint. End-to-end reclaim, which includes detecting the need and revoking readers, is unmeasured and is what RFC-0004 expects to dominate. See RFC-0005 | Claim 1 fails, and no amount of performance would justify the risk to jobs |
| Operators will donate a fixed slice of node NVMe permanently | Unverified, needs conversations with operators | The durable pool never forms and Forebay is a cache only |
| The in-kernel Linux pNFS client is production-viable for this access pattern | Partly verified. The flexfiles driver ships in the target node OS, see RFC-0008. Its behaviour under load is still unmeasured | The access layer needs rethinking, and possibly a client, which this document explicitly refuses to write |

Two rows remain unverified: whether local media beats a node's share of a shared backend, and whether
operators will donate capacity at all. Those two are the project. Everything else is engineering.

One row has moved since this document was first written. The in-kernel client began unverified and is
now partly verified. The reclamation row was never unverified, only reasoned, and is now partly
measured. That is one assumption settled and one strengthened, which is worth being precise about in
a document whose whole argument is that nobody should overstate what has been measured.

## Landscape

The honest summary is that Forebay is not the first system to notice compute-local media, and the
sections below say so plainly. Several of these are closer to the idea than a feature comparison
would suggest, and pretending otherwise would only mean discovering it later.

| System | What it is | Relationship to this thesis |
| --- | --- | --- |
| Enterprise array platforms | Mature, general-purpose storage software on dedicated hardware | See only their own media. They cannot observe GPU utilisation or cache behaviour, so they cannot act on either. The gap is structural rather than a missing feature |
| Ceph | Distributed block, file and object store | Excellent durable substrate and a planned Forebay backend. Its placement is failure-domain aware, not accelerator aware, and rebalance is a slow, heavy actuator |
| WEKA | High-performance parallel filesystem, deployable converged on compute nodes | The closest commercial system to this idea, and converged mode already uses compute-node NVMe. Its documentation describes cores pinned by cgroup and sized at configuration time against fixed ratios, with Slurm configured to keep user workloads off them, and capacity changed by an administrator expanding cluster resources. Provisioned once and held, in other words, rather than arbitrated continuously from observed state |
| VAST Data | Disaggregated shared-everything storage | Assumes dedicated storage enclosures. Different bet: separate the tiers well rather than converge them |
| HPC storage platforms | Parallel and scale-out storage for large clusters | Dedicated hardware, and the same structural blindness to compute state as the row above |
| Lustre, BeeGFS | Parallel filesystems for HPC | Dedicated servers by default. BeeGFS On Demand builds an ad-hoc parallel filesystem across a job's compute nodes, which is genuine prior art for this thesis |
| Cray DataWarp and burst buffers generally | Job-scoped fast tier between compute and durable storage | The oldest form of this idea. Job-scoped and statically staged rather than continuously arbitrated across a fleet |
| Alluxio | Distributed cache over object storage | Closest in spirit on the caching side. Assumes the cache tier belongs to it, rather than being on loan and revocable mid-read |
| JuiceFS | POSIX filesystem over object storage with a metadata engine | Solves namespace and metadata over object storage. Not a compute-convergence play |
| OpenEBS | Kubernetes-native storage over local devices | Uses local disks well, as a durable target rather than an elastically reclaimable tier. A planned Forebay backend |
| MinIO | S3-compatible object storage | A durable target. Orthogonal |

### Where the gap actually is

Burst buffers, BeeOND and WEKA's converged mode all establish that compute-local media can serve
storage. That part is not novel and this document should not claim it is.

What none of them appears to do is treat the boundary between compute and storage as **continuously
negotiable at fleet scale, driven by observed state on both sides**. Burst buffers are staged for a
job and torn down with it. Converged deployments provision a slice and keep it. In each case the
split is decided once, by a human or a job script, and then holds.

Forebay's bet is that the split should be a running decision made by a control plane that can see
GPU utilisation, cache hit rates, network headroom and capacity pressure at the same time, and that
the value of seeing both sides at once is large enough to build a system around.

That is the claim to attack. If it is wrong, it is most likely wrong because the split rarely needs
to change, in which case static provisioning is sufficient and simpler, and Forebay is an elaborate
answer to a question nobody was asking.

## Kill criteria

Forebay should be abandoned, or fundamentally redesigned, if any of the following turns out to be
true.

1. **Reclamation is not free.** Taking borrowed capacity back measurably harms the job that owns the
   node, and no design removes the harm. Compute always wins is the project's central promise, and a
   system that cannot keep it should not exist.
2. **Locality does not pay.** On representative GPU hardware, the fast tier cannot beat the durable
   backend's own parallel fan-out for the access patterns that matter. This is the counterexample
   above, generalised.
3. **There is nothing to borrow.** Measurement across real fleets shows compute-local NVMe is
   already well utilised, or idle only in windows too short to be useful.
4. **The split does not need to move.** Static provisioning captures nearly all the available value,
   making the control plane's central function unnecessary.
5. **Operators will not donate capacity.** If nobody will permanently commit a slice of node NVMe,
   the durable pool never forms and only the cache half of the design survives.

The first four are measurements and belong to RFC-0018, which now carries an experiment for each.
The fifth is not a measurement at all: whether operators will permanently commit capacity is a
question about incentives, answered by asking them rather than by instrumenting anything, and it is
listed below as open for that reason.

The project treats all five as the highest priority work rather than as risks to be managed around.

## Non-goals

| Not doing | Why |
| --- | --- |
| Cloning a mature array platform's feature set | Thirty years of accumulated features is not a race that can be won directly, and trying invites a comparison on the incumbent's terms |
| Durable data on borrowed capacity | It would make reclamation a migration, which is exactly the storm the design exists to avoid |
| Writing a durable store | Ceph, OpenEBS and S3 exist, are good, and are already deployed where the users are |
| Writing a client | Shipping and supporting a client across kernels and distributions is where storage projects bleed. The in-kernel pNFS client is the client |
| Concurrent block, file and object access to the same bytes | Not possible in any meaningful sense. File and object over one copy **is** a goal, see [RFC-0021](0021-single-copy-multi-protocol.md). Block shares the control plane, namespace and snapshots, but not the representation |
| GPUDirect Storage in v1 | Real, and probably valuable, but it constrains hardware and depends on the rest of the path already being fast. Later, behind capability detection |
| Machine-learned access prediction in v1 | Manifests and sequential heuristics have to be shown to fall short before a model is worth its operational cost |
| Forking Ceph | The value is above the data plane. A fork is a permanent maintenance tax paid for leverage this project does not need |
| Copying data that did not have to move | A byte is written once and everything else is a reference. See [RFC-0020](0020-no-copy-policy.md) |
| Caching mutable data | A published dataset version is written once and then immutable, and a change produces a new version with a new identity. That is a constraint the project adopts rather than a property it discovered, and it is what removes invalidation, coherence between nodes, and any window in which two readers see different bytes. Mutable data is scratch: node-local, never shared, never fetched from a peer. [RFC-0012](0012-dataset-object-model.md) owns the naming and identity rules that give the constraint its mechanics |

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Build only a distributed cache, no durable pool and no control plane | Much smaller, and Alluxio proves the shape works | Gives up the part that is actually differentiated, and leaves the tier unable to survive its own reclamation policy |
| Build a new distributed durable store from scratch | Full control of the data path, no backend limitations | Years of work to reach a level of trust that Ceph already has, before the interesting question is even reached |
| Contribute the idea upstream into Ceph | Reuses an enormous amount of engineering and an existing community | Requires Ceph to know about GPUs and schedulers, which is not its concern and should not become one |
| Static provisioning, no elasticity | Far simpler, and matches how converged deployments work today | Concedes the thesis before testing it. If kill criterion 4 holds, this is the correct answer and Forebay should stop |

## Open questions

- **Where the locality crossover sits on current GPU hardware.** Everything depends on it. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **What fraction of real storage traffic is genuinely regenerable**, which sets the ceiling on how
  much the borrowed pool can ever matter. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **Whether operators will permanently commit capacity.** No RFC owns this, and that is deliberate
  rather than an oversight: it is a question about incentives, answered by asking operators, and no
  amount of instrumentation substitutes. It is deferred to the review that decides whether Phase 1
  has succeeded, since a fast tier nobody will donate to is a different product.
