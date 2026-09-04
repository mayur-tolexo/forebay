# RFC-0022: Data-aware scheduling and warm start

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0003, 0007, 0014 |

## Problem

Kubernetes schedules a pod onto a node, and only then does anybody discover that the dataset it needs
is three racks away. The job spends its first minutes pulling terabytes across the fabric while eight
accelerators idle, and it does it again on every restart.

Forebay knows which nodes and which racks already hold the data a job is about to read. Handing that
back to the scheduler turns a cold start into a warm one. This is the clearest example of a capability
that needs both sides: a storage system that cannot observe scheduling cannot offer it, and a
scheduler that cannot observe cache residency cannot either.

## What of this is built

**The residency signal, and the hysteresis that makes it publishable.** `internal/residency` turns
how much of a dataset a node holds into a stable label value, quantised into three levels with a
margin around each boundary, so a node whose residency hovers at a threshold does not rewrite its
labels on every eviction.

**Both halves.** The agent reports what it holds at `/residency`, in the levels a label would carry,
and the controller reads every agent and writes the node labels. What is not built is the pre-fill,
which is RFC-0011's prefetch pointed at a dataset rather than a stream.

Labelling is off unless an operator asks for it, in its own RBAC file, because patching nodes is the
widest permission this project asks for.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A cold start on a large dataset costs minutes of accelerator time, and a warm one does not | Reasoned, from dataset sizes against fabric bandwidth, and unmeasured by this project | The whole feature is solving a problem nobody has |
| Residency changes often enough that an unquantised label would be rewritten constantly | Reasoned, from the tier being a cache under reclamation pressure: blocks arrive and leave continuously | The hysteresis is unnecessary complexity, and a plain fraction would have done |
| An operator will accept a node label per resident dataset, but not a scheduler plugin | Reasoned, from what each costs to adopt: a label is data, and a plugin is code in the scheduling path | The design is more timid than it needed to be, and a plugin would have scored better placements |
| The scheduler may ignore the hint, and the system must be correct when it does | Constraint inherited from RFC-0001, where influence is allowed and blocking is not | Forebay becomes a scheduling dependency, and a Forebay outage stops pods being placed |

## Design

### Labels, not a plugin

Three ways to tell a scheduler about residency: node labels the user matches with affinity, a
scheduler plugin, or a scoring extender.

It is labels. A plugin and an extender both put Forebay in the scheduling path, where being slow or
being down stops pods being placed, and adopting either means changing how a cluster's scheduler is
built or configured. A label is data that already-existing mechanisms consume: the user writes a
`preferredDuringSchedulingIgnoredDuringExecution` node affinity, and the scheduler weighs it against
everything else it knows.

That the hint is advisory then stops being a policy and becomes a structural fact. Forebay cannot
block a placement because nothing in the placement path calls Forebay.

It also settles the short-job question the stub asks. Forebay does not decide whether residency
should influence a job, because Forebay does not know how long a job will run and a model of that
would be invented rather than observed. A short job simply does not set the affinity, and a long one
weights it as heavily as its owner thinks right.

### A label per dataset, quantised, with a margin

The label's key names the dataset, and its value is how much of it the node holds.

A continuous value is not publishable. The tier is a cache under reclamation pressure, so residency
moves continuously, and a label carrying a percentage would be rewritten on every admission and
eviction, against the API server, for every dataset on every node. So the value is one of three
levels, and each boundary has a margin: rising past a threshold and falling back below it are
different numbers, so a node sitting near one does not flap.

```
fraction    0        0.20  0.25              0.70  0.75        1.0
level       none  <--------> some        <---------> most
                  rise 0.25                rise 0.75
                  fall 0.20                fall 0.70
```

Three levels rather than ten, because a scheduler weighing an affinity does not need precision it
cannot act on: what it needs to know is whether this node would be a warm start, a partial one, or a
cold one.

The key is derived from the tenant and dataset by hashing, because a Kubernetes label key is bounded
in length and a tenant and dataset name together are not. A collision means one dataset's residency
is reported for another's, which is a worse scheduling hint and never a wrong read, since nothing on
the read path consults a label.

### Who writes the label

The node knows its residency and the node must not be the thing that writes it.

RFC-0016 settles this without a new argument. A node-resident component holding a credential that can
patch node objects is a compromised node able to label every other node, and Kubernetes has no way to
scope that to one node for a credential that is not the kubelet's. Its answer there was that where a
node cannot be given a narrow enough credential it holds none, and asks the controller, which does
hold the wider view and does not run on a node a tenant's pods share.

So the agent holds no Kubernetes credential for this at all. It reports what it holds, and the
controller reads that and writes the labels.

The controller finds the agents through the endpoint slices of their service rather than by listing
pods. A slice already carries the node name beside the address, which is exactly the pair a label
write needs, so listing pods would be a wider read for a worse answer. An endpoint that is not ready
is skipped: a node that has said it should not be sent work should not have its residency published
as though it were healthy.

A pass writes only what changed, and takes back only labels with this project's prefix. Writing every
node every interval would be precisely the label churn the levels exist to prevent, and taking back
anything unrecognised would delete labels an operator set by hand. The hysteresis lives on the agent rather than the
controller, because it is the agent that knows what it last said and a controller restarting would
otherwise republish everything at whatever the fraction happened to be.

A dataset whose size the agent cannot learn is left out of the report rather than reported at an
assumed share. A scheduler acting on a residency the node invented would place work for data that is
not there, which is worse than placing it with no hint at all.

### Racks, for jobs that land together

A gang-scheduled job's ranks land together or not at all, so scoring individual nodes for a gang
scores the wrong unit: a rack that holds the data on one node out of eight is not a warm start for a
job that needs all eight.

Residency is therefore published at both granularities, the node's own and its rack's, from the
topology RFC-0003 already discovers. A single-node job matches the node label; a gang matches the
rack label. Neither needs Forebay to know which kind of job it is looking at, because the job's owner
picks the label they mean.

### Warm start is prefetch, and inherits its rule

Pre-filling a destination before a pod is admitted is prefetch with a manifest, which RFC-0011
already designed. That means the budget question the stub asks is already answered: **prefetch never
evicts**, so pre-filling for one job cannot take capacity from a job that is reading. When the tier
is full the pre-fill stops, and the arriving job reads from the backend exactly as it would have.

What warm start adds over ordinary prefetch is when it starts, not what it may do.

### When the scheduler ignores the hint

Nothing happens. The pod lands where the scheduler chose, the first reads miss, and they are served
from the backend at backend speed, which is what would have happened without any of this.

That is worth stating plainly because it is the property that makes the feature safe to ship
half-working. There is no state to reconcile, no placement to undo, and no failure to report.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| A scheduler plugin | Real scoring, exact residency, no label cardinality | Puts Forebay in the scheduling path, where slow or down stops pods being placed. Also requires changing how the cluster's scheduler is built |
| A scoring extender | Same scoring without rebuilding the scheduler | Still a synchronous call in the scheduling path, with the same failure coupling and a webhook to operate |
| One label carrying a percentage | Precise, one key per dataset | Rewritten on every admission and eviction, for every dataset on every node, against the API server. The precision is not actionable by an affinity |
| One label listing every resident dataset | Bounded key count | Label values are bounded in length, and the list is unbounded. It also rewrites on every change to any dataset |
| Let Forebay decide whether a job should care about residency | Users would not have to think about it | Needs a model of how long a job runs, which Forebay cannot observe and would be inventing. RFC-0024 refused the same move for the same reason |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| Labels are stale | Scheduling quality | A job is placed for residency that has since been reclaimed. It reads from the backend, which is the cold start it would have had anyway |
| The publisher is down | Scheduling quality | Labels freeze at their last values and decay in usefulness. Nothing blocks |
| Two datasets hash to the same key | Scheduling quality, for those two | One dataset's residency is reported for the other's. A worse hint, never a wrong read |
| The scheduler ignores the affinity | None | The placement is whatever the scheduler wanted, and reads miss |
| Pre-fill is running when the tier fills | None | It stops, because prefetch never evicts |
| Residency oscillates around a threshold | The API server | Bounded by the margin, which is what it exists for |

## Performance implications

The quantisation is arithmetic per dataset per node, on whatever cadence the publisher runs at. The
cost that matters is label writes against the API server, and the margin is what bounds it: a node
writes only when residency crosses a boundary in the direction it is not already on.

How often that happens in practice is unmeasured, because nothing publishes yet.

## Complexity

Small, and deliberately so. The design's main work is choosing the mechanism that needs no new
component in anyone's scheduling path.

What it makes harder later is moving to a plugin, since users will have written affinities against
these labels and the labels would have to keep working.

## Security and tenancy

A residency label discloses that a node holds part of a named dataset. The key is a hash of tenant
and dataset, so it does not name either in plain text, but it is not a secret: anyone who can list
nodes can test whether a dataset they can name is resident, which reveals that the dataset exists and
roughly where it is.

That is a disclosure to cluster-scoped readers rather than to tenants, and it is the same class of
information RFC-0016 constrains for traces. A deployment where node labels are visible to tenants
should not publish them, and that is an operator decision rather than something this design can take
back.

## Open questions

- **What a cold start actually costs on a large dataset**, which is the premise of the whole feature.
  Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what this project
  measures.
- **How often residency crosses a level boundary in practice**, which decides whether the margin is
  enough to make labels publishable at all. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **How a job declares the datasets it will read.** This document assumes it arrives with the pod and
  does not fix the form, because the form belongs with the rest of the Kubernetes surface. Owned by
  [RFC-0014](0014-kubernetes-integration.md), which owns what this project puts in a cluster.
