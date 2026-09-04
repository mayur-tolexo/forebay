# RFC-0013: Checkpoint path

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0005, 0009 |

## Problem

A synchronised checkpoint across a large training job produces a burst of writes that a central
filesystem absorbs badly, and every rank waits for the slowest write. Staging locally and making data
durable afterwards is an old idea, and it is the right one, provided the durability promise is
precise.

The danger is the acknowledgement. A checkpoint that is reported complete before it is durable is a
correctness problem dressed as a performance win, and the difference only becomes visible when a node
is lost.

## What of this is built

**The vocabulary and the reservation rule, and no write path.** `internal/checkpoint` says what each
acknowledgement means, refuses a staging request that cannot be reserved, and refuses one that would
be staged into capacity somebody can take back.

Nothing writes. There is no aggregation across ranks, no rack-level staging and no upload. What
exists is the rule that stops the failure this problem statement names, which is worth having before
the path that could commit it.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A checkpoint being staged is the only copy of itself | Reasoned, and the reason it cannot live in revocable capacity: it is not regenerable until the previous checkpoint is the fallback | A reclaim takes the only copy of a job's progress, and the constraint that borrowed data is regenerable is broken by the feature that borrowed it |
| A user will take a fast acknowledgement if offered one and will not read what it means | Reasoned, from how every write-back cache has ever been used | The default is fast, and a node loss silently costs hours nobody was told about |
| Checkpoint size is known before the write begins | Unverified. A framework generally knows its own state size, and this design needs it to reserve | Reservation is a guess, and a checkpoint runs out of staged capacity part way |
| The previous durable checkpoint is an acceptable fallback | Reasoned, from how training restarts work | Losing the in-flight one is a correctness problem rather than a cost in time |

## Design

### Two acknowledgements, and exactly what each means

| Acknowledgement | Means | Survives | Does not survive |
| --- | --- | --- | --- |
| `staged` | The bytes are on this node's disk, in capacity nobody may take | The process, and a restart of the agent | The node |
| `durable` | The backend has them and its guarantee applies | Whatever the backend survives | Whatever the backend does not |

There is no third. A word like `committed` or `written` would mean whichever of these the reader
hoped for, and this is the document whose problem statement is about exactly that confusion.

**A `staged` acknowledgement names its own failure.** It is offered because waiting for durability
makes every rank wait for the slowest upload, and it is honest only if the user knows they are
trading a node loss against that wait.

### The default is `durable`, and that is the whole safety argument

A user who has not chosen gets the acknowledgement that cannot lose their work. The fast one is
available, it is not the default, and choosing it is a sentence in a manifest rather than an absence
of one.

This follows RFC-0009's rule that the defaults are the product. The failure this document opens with
is a correctness problem dressed as a performance win, and a design that made the win the default
would be dressing it.

### Staging capacity cannot be revocable, and this is not negotiable

RFC-0001 permits borrowing because borrowed data is regenerable: losing it costs a refetch. **A
checkpoint being staged is the exception, and it is the only one.** Until it is durable it is the
only copy of itself, so capacity holding it is capacity nobody may take back.

That is what `guaranteed` is for, and RFC-0005 already names checkpoint staging as its reason. So:

| | |
| --- | --- |
| Staging uses | A `guaranteed` lease, reserved before the first byte is written |
| Reserved for | The whole checkpoint, not the part written so far |
| Released | When the bytes are durable, and not before |
| If it cannot be reserved | The request is refused, and the writer falls back to writing straight through |

**A checkpoint is never staged into capacity somebody can reclaim.** A reclaim taking the only copy
of a job's progress would break the constraint the whole project rests on, and it would break it in
the one place where the data is worth the most.

RFC-0005 caps guaranteed leases as a share of the device, so a node cannot promise all of itself to
staging. A checkpoint larger than that share is refused, which is the correct answer: the alternative
is a node that has lent its whole disk to something it may not reclaim.

### A node lost between acknowledgement and durability

| Policy | What is lost | What the job does |
| --- | --- | --- |
| `durable` | Nothing. The acknowledgement had not been given | Waits, or fails the write and retries |
| `staged` | That checkpoint | Restarts from the previous durable one |

The second row is the cost of the fast path and it is stated here rather than discovered. A job
checkpointing every twenty minutes with a `staged` acknowledgement loses up to twenty minutes of work
per node loss, which is a trade many jobs should take and none should take unknowingly.

### The storm, and why reservation happens first

A synchronised checkpoint is every rank writing at once. The failure mode is not the bandwidth; it is
a thousand ranks discovering at the same moment that there is nowhere to put it.

Reserving before the write means a rank learns it cannot stage **before** it has stopped computing,
and can write straight through instead. Reserving during the write means a rank stops, starts,
fails part way, and then does the slow thing anyway with the checkpoint half written.

Aggregation across ranks, and whether it happens at rack level, is not answered here. It needs the
rack tier RFC-0007 describes and does not have, and answering it against a component that does not
exist would be design fiction.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| One acknowledgement, always durable | Nothing to misunderstand | It gives up the entire reason for staging, which is that every rank waits for the slowest upload |
| Stage into elastic capacity, and copy out on reclaim | Staging uses the plentiful class | A reclaim promises a deadline, and copying a checkpoint to the backend does not fit inside one, so the promise breaks exactly when it is tested |
| Acknowledge fast by default | The benchmark looks better | It is the failure the problem statement names, made the default |
| Reserve as the write proceeds | No capacity held for a checkpoint that never comes | A rank finds out it cannot stage after it has stopped computing, which is the worst moment to find out |
| A third acknowledgement for replicated-but-not-durable | Finer control | Nothing here replicates between nodes, so it would name a state the system cannot be in |

## Failure modes

| Failure | What happens | Why it is acceptable |
| --- | --- | --- |
| The reservation cannot be met | The request is refused before writing | The writer writes through, which is what it would have done without this feature |
| The node is lost while staged | That checkpoint is lost | The policy said so, and the previous durable one is the fallback |
| The upload never completes | Capacity stays reserved | Better than releasing capacity that still holds the only copy; the operator sees a lease that is not shrinking |
| A framework understates its checkpoint size | Staging runs out part way | The reservation is what was asked for; a checkpoint larger than its reservation is the framework's error and is reported as one |
| Every node refuses to stage at once | The whole job writes through | Slow, and correct, and exactly the behaviour of a cluster without this feature |

## Performance implications

The reservation is one lease grant per checkpoint, on the path that already grants leases. It is not
per byte and not per rank-file.

The benefit is not measured. What staging buys against writing through depends on the ratio between a
node's device and its share of the store, which RFC-0018 has now measured on three machines and found
to vary by more than an order of magnitude. On a node whose store is faster than its disk, staging is
a slower path with extra failure modes, and the honest thing is that this feature is worth having on
some hardware and not on others.

## Complexity

A vocabulary, a reservation and a refusal. The complexity deliberately deferred is aggregation, the
upload path and anything rack-shaped, all of which need components that do not exist.

## Security and tenancy

Staged bytes are a tenant's data sitting on a node in capacity that tenant did not buy. They are
readable by whoever can read the pool directory, which is the node agent, so the isolation is the
same as the fast tier's and no weaker.

A tenant reserving guaranteed capacity denies it to others by construction, which makes staging a
quota question as much as a capacity one. RFC-0016 owns quota.

## Open questions

- **Whether staging is worth it on a given node**, since it depends on the ratio between the node's
  device and its achievable share of the store, and that varied by more than an order of magnitude
  across three machines. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what this project has measured.
- **How writes are aggregated across ranks, and whether at rack level**, which needs the rack tier.
  Owned by [RFC-0007](0007-fast-tier-data-path.md), which owns where a block lives.
- **How much guaranteed capacity a tenant may reserve for staging**, which is a quota question rather
  than a capacity one. Owned by [RFC-0016](0016-multi-tenancy-qos-and-security.md), which owns quota.
