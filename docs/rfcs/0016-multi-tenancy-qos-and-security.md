# RFC-0016: Multi-tenancy, QoS and security

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 4 |
| **Depends on** | 0005, 0009 |

## Problem

Forebay runs a privileged agent next to customer workloads and hands the same physical capacity to
one tenant after another. Both properties make isolation a design problem rather than a configuration
one.

The specific hazard this architecture creates is residual data. Borrowed capacity is dropped and
re-lent constantly, so content surviving into the next holder would be a vulnerability rather than a
bug. It is also the hazard with the worst shape: nothing fails, nothing is logged, and the tenant who
receives the bytes is the only party in a position to notice.

Five other RFCs defer a question here. This document has to answer them rather than pass them on.

## What of this is built

**The residual-data guarantee, and nothing else in this document.** `internal/agent` verifies before
the node lends anything that a freshly reserved extent reads as zeros, and refuses to lend if it does
not. That is the answer RFC-0019 and RFC-0005 were waiting on, and it is the one part of tenancy that
cannot be added later without a window in which the vulnerability was live.

Quota, the intent floor, per-credential capabilities and the access-path decision are designed here
and not written. They are additions to paths that exist. The guarantee is not.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| `fallocate` with mode 0 yields extents that read as zeros rather than as whatever was on the blocks | Measured on ext4 under Linux 6.8: 6 GiB of random bytes written and deleted, then 2 GiB reserved over those freed blocks and read in full, with every one of 2,147,483,648 bytes zero | Every reclaim leaks the previous tenant's bytes, silently, which is the vulnerability this document exists to close |
| A tenant's data in the tier is regenerable and already at rest in a durable store the tenant is authorised against | Constraint from RFC-0001, and the reason node-local encryption is refused below | Forebay holds the only copy of something, and a threat model built on "it is a cache" is wrong |
| The node is the only authority on its own capacity | Constraint from RFC-0004 | A capacity guarantee needs a global admission decision, and QoS becomes a distributed problem rather than a local one |
| A shared NVMe leaks throughput timing between tenants no matter what the filesystem does | Reasoned, from every published result on shared-device side channels | The limitation stated below is understated, and the tier is worse than this document admits |
| An administrator strengthening a user's request is a safe operation and weakening it is not | Reasoned, from what the intent vocabulary means: every word in it is a requirement, so raising one can only refuse more | The floor becomes a way to quietly serve a user less than they asked for, which is the failure a declarative interface is meant to prevent |

## Design

### The isolation boundary is the extent, and it is not a timing boundary

One extent per lease, one lease per tenant, created `O_EXCL` at mode `0640` and owned by the agent. A
tenant is never given a path: the access layer addresses a range within a lease and the agent
resolves it, so there is no name a tenant can construct that reaches another tenant's bytes.

What that does not do is hide access patterns. Two tenants sharing an NVMe share its queue, and one
can infer the other's read rate by measuring its own. Forebay does not prevent this and this document
will not claim it does. What bounds the damage is what is in the tier: bytes fetched from a durable
store on behalf of a tenant that was already authorised to read them. An inference attack against the
tier recovers the shape of an access pattern, not its content.

A tenant who cannot accept that should not share a node, which is a scheduling decision and belongs
to RFC-0022 rather than here.

### Residual data: the guarantee comes from allocation, not from release

The obvious answer is to zero an extent when it is reclaimed. It is refused. Reclamation happens when
compute wants its capacity back, and any path that has to write in order to free space is slowest
exactly when it is needed most. Zeroing a 64 GiB extent at 2 GiB/s is 32 seconds added to a promise
measured in seconds, and the promise is the product.

So the guarantee is moved to the other end. `fallocate` with mode 0 allocates unwritten extents:
blocks are committed, and a read of them returns zeros rather than their previous contents, because
the filesystem tracks that they have not been written. Reclamation stays an unlink and costs nothing,
and a new tenant cannot see an old one's bytes because a fresh extent has no old bytes to show.

That moves the whole guarantee onto a filesystem property, which is a bad place to leave it: it holds
on ext4 and XFS with extents, and a filesystem that exposed stale blocks would break it invisibly.
So the node checks rather than assumes. Before it will lend anything, it reserves a small extent the
same way it reserves a real one, reads it, and refuses to lend at all if any byte is not zero.

```
reserve a probe extent  ->  read it back  ->  all zeros?  ->  yes: the node may lend
                                                          ->  no:  the node refuses, and says why
```

Refusing to lend is the correct failure. A node that cannot prove it hands out clean capacity is a
node that should donate none, and an operator gets a node that stayed out of the pool rather than a
cluster that leaked.

The check costs one allocate, one read and one unlink at startup, and nothing per reclaim.

### QoS is local for capacity and is not promised for bandwidth

The node is the only authority on its own capacity, so a capacity guarantee needs no global admission
decision: a node either has the free space to accept a guaranteed lease or it refuses one, and that
is a decision made with local state in the request path.

Bandwidth is not like that. A guarantee of read throughput to a tenant depends on every other tenant
reading from the same device and the same NIC, and there is no local state that settles it. Forebay
therefore makes no bandwidth guarantee. It makes a weaker promise it can keep: compute always wins,
enforced by reclamation, so a tenant's borrowing never costs another tenant's workload its capacity.

Cross-node bandwidth QoS is owned by [RFC-0026](0026-transport-and-throughput.md), which owns the
transport, and it should not be attempted before RFC-0018 has said which bottleneck actually binds.

### Quota, and the clone that keeps deleted bytes alive

Two limits per tenant per node, because they constrain different things:

| Limit | What it bounds | Why it is separate |
| --- | --- | --- |
| Borrowed ceiling | How much reclaimable capacity one tenant may hold | Bounds how much of the tier one tenant can occupy, and costs other tenants only speed |
| Guaranteed ceiling | How much of the node's guaranteed share one tenant may reserve for checkpoint staging | Guaranteed capacity denies itself to everyone else by construction, so it must be scarcer than the ceiling above it |

The guaranteed ceiling is the one that matters. A tenant that could reserve a node's whole guaranteed
share would deny checkpoint staging to every other tenant on it, without exceeding any borrowed
ceiling and without doing anything the system would call an error.

**Who may clone across a tenancy boundary**, which RFC-0012 defers here: nobody, by default. A clone
shares bytes, so a cross-tenant clone makes one tenant's deletion depend on another tenant's
reference, and a tenant who can clone can pin their competitor's storage bill indefinitely by holding
a reference to a version its owner deleted. Cross-tenant cloning is therefore off unless the owning
tenant grants it, and when granted the bytes are charged to the *owner*, not the cloner, because the
owner is the party who can stop paying by revoking the grant. Charging the cloner would let a tenant
move cost onto someone who cannot decline it.

### The intent floor may strengthen a request and never weaken it

Every word in the intent vocabulary is a requirement, so raising one can only cause more requests to
be refused, never cause a request to be served with less than it asked for. That asymmetry is what
makes a floor safe: an administrator setting `durable` for a namespace cannot turn a user's `durable`
into `regenerable`, because the floor is applied as a maximum against each word's ordinal and a user
who asks for more keeps it.

An administrator cannot set a ceiling. A ceiling would let one silently serve a user less than they
declared, which is the exact failure a declarative interface exists to prevent, and a tenant who must
be limited is limited by quota and by refusal, both of which are visible.

### Identity, and what a compromised agent reaches

The agent holds two credentials and they have different blast radii.

| Credential | What it reaches | What bounds it |
| --- | --- | --- |
| Kubernetes | Datasets and their status | The controller RBAC, which can list datasets and patch their status and nothing else: not spec, not secrets, not pods |
| Durable backend | Whatever the credential can read in the store | The tenant it belongs to, and only while the node holds that tenant's lease |

The second is the dangerous one and it decides the answer to what RFC-0006 defers here. **A backend
capability is declared per credential, not per backend**, because a tenant may lack permission for
something the store itself supports, and a driver that reports the store's capability rather than the
credential's tells the planner a dataset can be served in a way that will fail at the first read. The
declaration is therefore taken from what the credential can actually do, probed once when the
credential is first used, and a capability that disappears under a dataset is RFC-0017's problem.

**Out-of-tree drivers are not loaded**, which is the other question RFC-0006 defers here. A driver is
code that runs inside the agent holding credentials to a customer's durable store; loading one from
outside the build is handing that to an artifact nobody in this project reviewed. Drivers are
compiled in. The cost is that adding a backend needs a release rather than a plugin, which is a real
cost and the right one to pay.

**Whether a node-restricted Kubernetes credential is sufficient**, which RFC-0014 defers here: it is
sufficient for the adapter, and the CSI driver is where it stops being sufficient. The adapter needs
the pods bound to its own node, which `NodeRestriction` gives it. A CSI node plugin, as usually
deployed, is given cluster-wide read on PersistentVolumes so it can resolve a volume handle, and that
is a wider view than a compromised node should hold. The resolution is that the volume's identity
must arrive in the `NodePublishVolume` request rather than be looked up, so the node plugin needs no
cluster-wide read at all. Where that is not possible the node plugin runs with no Kubernetes
credential and asks the controller, which does hold the wider view and does not run on a node a
tenant's pods share.

### What authenticates a tenant on the access path

RFC-0008 defers this here, and the answer is that AUTH_SYS is not authentication. It asserts a uid
the client chooses, so any tenant who can reach an export can claim any identity in it.

That does not make AUTH_SYS unusable, because it is not being asked to authenticate. Each tenant gets
its own export namespace, reachable only from that tenant's pods, enforced by network policy, and the
network path is what establishes which tenant is calling. AUTH_SYS then carries the identity *within*
an already-authenticated tenant, which is what it is adequate for.

This is acceptable exactly as far as the network policy is. In a deployment where a tenant can reach
another tenant's export — a flat network, a cluster without enforced policy, anything crossing a
cluster boundary — it is not acceptable and `RPCSEC_GSS` is required. That case is real and this
document does not pretend otherwise; it is deferred to RFC-0025, which owns crossing cluster
boundaries, because that is where it first stops being avoidable.

### What a trace may carry out of the cluster that produced it

RFC-0018's workload experiments need real access traces, and a trace is disclosure: it reveals a
dataset's structure and, through characteristic sizes and shard counts, often its identity.

A trace that leaves is reduced to four things — inter-arrival deltas from the trace's own start, read
sizes, offsets expressed relative to the dataset's start, and a dataset identifier that is a hash
salted per export with a salt that is never published. Object keys, paths, tenant identifiers and
wall-clock times do not leave.

What survives that reduction is the dataset's shape: its size, its shard boundaries, and how a
dataloader reuses it across epochs. That is not removable, because it is the entire content of a
useful benchmark trace. So the reduction does not make a trace safe to publish; it bounds what
publishing one discloses, and publication still needs the producing tenant's consent. An RFC that
claimed anonymisation made consent unnecessary would be claiming something no trace reduction has
ever achieved.

### Encryption

Not at rest on borrowed capacity, and this is a decision rather than an omission. The tier holds
regenerable bytes that are already at rest in the durable store under that store's own encryption,
on a node the tenant's own workload is already running on. Encrypting them again costs the read path
what the tier exists to provide, against a threat — physical removal of an NVMe from a GPU node —
that is better answered underneath Forebay by a self-encrypting drive or dm-crypt, where it costs the
same for every consumer of that device rather than only for this one.

In flight between agents is a different question with a different answer, and it belongs to
RFC-0026 with the rest of the transport, because whether it can be afforded depends on which path the
transport takes.

### Scoping is a type, not a convention

Tenant and region scoping is enforced by making an unscoped address unconstructable. A reference
carries its tenant, and the code that turns a reference into a placement cannot be handed something
that has not been scoped, because there is no such value to hand it. A convention — remembering to
filter by tenant in each query — fails the first time somebody adds a query, and the control plane
holds credentials broad enough that the first time is enough.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Zero an extent when it is reclaimed | Obvious, self-evidently correct, needs no filesystem property | Writes in order to free space, so it is slowest exactly when compute is waiting. It converts a seconds-scale promise into a minutes-scale one, and the promise is the product |
| Discard the whole device between tenants | A stronger guarantee than unwritten extents, and hardware-enforced | A node hosts many tenants at once. There is no moment when the device belongs to one of them |
| Encrypt each extent with a per-lease key and drop the key on reclaim | Crypto-erase makes residual data unreadable rather than absent, and costs nothing at reclaim | Moves the cost to every read instead of every reclaim, on the path the tier exists to make fast. Worth revisiting if the filesystem assumption ever fails |
| Trust `fallocate` without checking | Free | Puts the entire guarantee on an unverified property of somebody else's filesystem, and its failure mode is silent. The check costs one extent at startup |
| Per-tenant nodes, no sharing | Removes the timing side channel and most of this document | Removes the thesis with it. Idle capacity is spread thinly across nodes that belong to other people, which is the whole reason this project exists |
| A global admission controller for bandwidth | Real QoS rather than a weaker promise | Needs a global view in the request path, and the node's authority over its own capacity is what makes the rest of the system able to survive a partition |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| The zero check fails on a node | That node donates nothing | The node refuses to lend and says why. The cluster loses one node's capacity and leaks nothing |
| The zero check passes and the filesystem later changes behaviour | That node's tenants | Undetected until the next restart, because the check runs at startup. A per-grant check was considered and refused: it would put an allocate and a read in the grant path |
| Network policy is not enforced | Every tenant on the access path | AUTH_SYS becomes what it looks like: no authentication at all. This is the load-bearing dependency in the access-path answer and it is stated so it can be checked |
| A node agent is compromised | That node's tenants' backend credentials | The attacker reads what those credentials read, for as long as the leases are held. The Kubernetes credential adds datasets and their status and nothing more |
| A tenant reserves the whole guaranteed share | Every other tenant on that node | Checkpoint staging is refused for everyone else. This is what the guaranteed ceiling exists to prevent and why it is separate from the borrowed one |
| A cross-tenant clone outlives its owner's deletion | The owning tenant's bill | Bounded by the grant: the owner revokes and the reference dies. Charging the owner is what makes revocation the owner's decision |

## Performance implications

The zero check adds one `fallocate`, one read and one unlink to startup. On the ext4 pool measured a
4 MiB probe took 10 ms, and it happens once per process rather than once per grant.

The property itself was measured at a scale where a leak would be visible: 6 GiB of random bytes
written and deleted, 2 GiB reserved over the freed blocks, every byte read. None were non-zero. That
is evidence for ext4 under Linux 6.8 and for nothing else, which is why the node checks at startup
rather than this document asserting it.

Reclamation is unchanged, which is the point. The alternative that would have made this document
simpler is the one that would have made reclaim latency worse, and reclaim latency is the promise.

Everything else here is predicted rather than measured, because none of it is built.

## Complexity

The check is small and self-contained. Quota is arithmetic against state the lease manager already
holds. The intent floor is a comparison against ordinals that already exist.

What this makes harder to change later is the residual-data answer itself: it commits the project to
a filesystem property, so a future backing store that is not a Linux filesystem with unwritten
extents — a raw device, a network filesystem — needs a different guarantee and cannot inherit this
one. The per-lease encryption alternative is kept in this document for exactly that reason.

## Security and tenancy

This document is the tenancy one, so the answer is the whole of it. What is worth restating is what
it does *not* claim: no bandwidth guarantee, no defence against timing inference on a shared device,
and no authentication on the access path independent of network policy. Each is stated where a reader
would otherwise assume the opposite.

## Open questions

- **Whether a per-grant zero check is affordable**, which would close the window where a filesystem
  changed behaviour after startup. It puts an allocate and a read in the grant path, and whether that
  is affordable is a measurement nobody has taken. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what this project measures.
- **Whether `RPCSEC_GSS` is required once an export is reachable across a cluster boundary.** The
  answer above holds only as far as network policy does, and a cluster boundary is where it stops.
  Owned by [RFC-0025](0025-cross-cluster-datasets.md), which owns crossing one.
- **Encryption in flight between agents.** Owned by
  [RFC-0026](0026-transport-and-throughput.md), because whether it can be afforded depends on which
  transport is chosen.
