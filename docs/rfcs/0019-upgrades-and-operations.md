# RFC-0019: Upgrades and operations

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0004, 0005 |

## Problem

Training runs last days or weeks. A storage system that requires a maintenance window is a storage
system that either blocks the cluster or never gets patched, and the second outcome is more common
and more dangerous.

This RFC covers upgrading Forebay underneath running work, and the day-two operations an operator
needs in order to trust it.

## What of this is built

**Drain, and the properties an upgrade already had without anyone writing them down.** The agent
holds a node lock, journals what it lent, replays that journal on startup and reconciles it against
the disk in both directions. Those are what make restarting it safe, and they were built for
crash recovery: an upgrade is a crash the operator chose the moment of.

`forebay-agent --drain` returns what a node lent and says what it could not. There is no control
plane to upgrade, no rack evacuation, and no skew enforcement beyond the journal's own format check.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| An upgrade is a crash whose moment the operator chose | Reasoned, and the reason this needs little new machinery: the agent already survives being killed | Upgrade paths are written that crash recovery does not cover, and the rarer one is the one that is tested |
| Losing the fast tier on upgrade is acceptable | Constraint from RFC-0001: what the tier holds is regenerable, so the cost is a cold cache | Upgrades are avoided because they are expensive, which is the outcome this document exists to prevent |
| An operator will roll back under pressure and read nothing | Reasoned, from how rollbacks happen | Rollback needs a runbook nobody reads, and the safe path is not the default one |
| Checkpoint staging is the one thing an upgrade may not interrupt | Reasoned, from RFC-0013: staged bytes are the only copy of themselves | A rolling upgrade destroys a job's progress, one node at a time, and looks like it worked |

## Design

### An upgrade is a crash the operator chose the moment of

Everything a restart needs already exists, because it was built for the case where nobody chose the
moment:

| | Survives a restart | How |
| --- | --- | --- |
| What the node lent | Yes | The journal, replayed at startup |
| The extents behind it | Yes | Reconciled against the disk in both directions |
| The fast tier's contents | No | It is a cache, and the lease it sits on is released and re-granted |
| The node lock | Yes | Released on exit, or broken by the liveness probe if the process wedged |

So a node agent upgrade is: stop, replace, start. The new process replays what the old one lent and
takes the lock. Nothing has to be coordinated and nothing has to be told.

**The tier is the cost.** A node comes back with an empty cache and refills it from the store, which
is a cold-start penalty and never a loss. Making it survive would need the index to be persistent and
the extent adopted rather than re-granted, and neither is worth it while the thing being protected is
a cache.

### Drain returns what it can, and refuses to take a checkpoint

An operator upgrading a node does not want to wait for terms to expire. Drain reclaims everything
reclaimable and stops.

| Class | Drained | Because |
| --- | --- | --- |
| `opportunistic` | Yes | It promises nothing |
| `elastic` | Yes | It promises a deadline, and drain is inside it |
| `guaranteed` | **No** | It is checkpoint staging, and the bytes are the only copy of themselves |

A drain that took a guaranteed lease would destroy a job's progress, one node at a time, while
reporting success. So drain reports what it could not return and **exits non-zero**, because an
operator scripting a rolling upgrade needs the rollout to stop rather than to read a log line.

That is the whole of the interlock: an upgrade cannot silently interrupt the one thing RFC-0013 says
must not be interrupted.

### The control plane, and what an in-flight decision is worth

The controller resolves every dataset on a pass and writes only what changed. An upgrade loses at
most one pass, and the next one recomputes everything from what it observes rather than from what it
remembered.

That is a property worth keeping rather than an accident: **the control plane holds no state that
would be lost.** Anything it knows it re-derives, so upgrading it is stopping it and starting it.

### Version skew, and the one place it is enforced

| Between | Rule | Enforced |
| --- | --- | --- |
| An agent and its own journal | The format is stamped and checked | Yes, and a mismatch is recoverable rather than fatal |
| An agent and the control plane | The control plane proposes and the node refuses what it cannot honour | By construction: a node never trusts a grant it cannot meet |
| A driver and the contract | The declaration carries a contract version | The declaration is validated when the backend is opened |
| A client and the read protocol | Not enforced | Named as a gap below |

The journal is where skew is actually caught. A newer journal read by an older agent fails its format
check, which is recoverable: the pool is discarded and the node starts empty, having returned its
capacity. **A rollback therefore ends with less lending, never more**, which is the same safe
direction a partition has in RFC-0015, and for the same reason: the node is the authority and the
conservative answer is to own its own disk.

### A driver replaced under live data

A backend's declaration is read when it is opened, so a driver that lost a capability declares less
after the upgrade. The controller re-resolves every dataset on its next pass and records the ones
whose intent can no longer be met.

Nothing is deleted and no data is touched. RFC-0009 already made that the behaviour for an
unsatisfiable intent, and a driver upgrade is just another way to reach it.

### Day two

| Operation | Steps |
| --- | --- |
| Upgrade a node | Drain. If it exits non-zero, wait for the staging to finish and drain again. Stop the agent, replace it, start it |
| Take a node out of service without upgrading it | Let readiness do it, or drain and leave it stopped: the second returns the capacity, the first does not |
| Evacuate a rack | Not answered here. It needs placement, which needs a control plane that places |
| Roll back | Stop, replace with the older binary, start. The journal's format check discards what the older one cannot read, and the node returns its capacity |
| See whether it worked | The watch's pass counter moves, and the lease gauge shows what came back |

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Hand the tier to the new process over a socket | The cache survives an upgrade | It makes two versions share a data structure at the moment they differ most, to protect something regenerable |
| Drain by waiting for terms to expire | No new mechanism | Terms are hours, and an operator patching a security hole cannot wait for them |
| Let drain take guaranteed leases | Every node drains promptly | It destroys a checkpoint, one node at a time, while reporting success |
| Refuse to start on a journal from a newer version | Nothing is silently discarded | It leaves a node unable to start during a rollback, which is when it is needed most |
| Version the read protocol now | Skew is caught everywhere | There is one client and one server, both built together, and a version negotiation nobody has skewed yet is a guess about a shape |

## Failure modes

| Failure | What happens | Why it is acceptable |
| --- | --- | --- |
| Drain cannot return everything | It exits non-zero and names what is held | A rolling upgrade stops, which is the correct response to a node staging a checkpoint |
| The new agent cannot read the journal | It discards the pool and starts empty | Everything the journal describes is regenerable, and the node returns capacity it can no longer account for |
| The old process did not release the lock | The liveness probe kills it and the new one takes the lock | This is RFC-0004's existing mechanism, not a new one |
| An upgrade lands mid-reclaim | The next startup finishes it | Invalidate-before-unlink means an interrupted reclaim leaves a name no lease claims, which reconciliation removes |
| Every node drains at once | The cluster's whole cache is cold | Slow, and correct, and the reason a rollout should be rolling |

## Performance implications

Drain is a reclaim, which RFC-0018 measured at between 3.7 ms and 773 ms depending on the device's
state. An upgrade's cost is dominated by the cold cache afterwards, not by the drain.

## Complexity

One flag and a runbook. The complexity avoided is a handover protocol between two versions of the
agent, which would be the most delicate code in the project in order to protect a cache.

## Security and tenancy

Drain returns capacity and does not read what was in it. RFC-0016 owns what reclaimed capacity
carries into its next holder, and drain uses the same path as any other reclaim, so it inherits
whatever that answer turns out to be rather than needing its own.

An upgrade replaces a binary that holds credentials to a durable store. Where those come from is
RFC-0016's, and this document adds no new place they live.

## Open questions

- **How a rack is evacuated**, which needs a control plane that places data before it can move it.
  No RFC owns it, because placement does not exist and a runbook for it would be fiction.
- **Whether the read protocol needs a version negotiation**, which is a guess about a shape until two
  versions of it have to coexist. Owned by
  [RFC-0008](0008-access-layer-pnfs.md), which owns what a client is told.
- **What reclaimed capacity carries into its next holder**, which drain inherits rather than answers.
  Owned by [RFC-0016](0016-multi-tenancy-qos-and-security.md), which already carries it.
