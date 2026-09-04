# RFC-0011: Prefetch and dataset manifests

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 3 |
| **Depends on** | 0007 |

## Problem

Training reads are unusually predictable. A dataloader walks shards in an order that is often known
in advance, and where it is not known it is frequently sequential. Hiding storage latency behind that
predictability is the most direct way to keep an accelerator fed.

The temptation is a learned model. RFC-0001 rules that out until manifests and plain heuristics have
been shown to fall short, which nobody has done.

## What of this is built

**The detector, the honesty that goes with it, and the rule that bounds it.** `internal/prefetch`
recognises a sequential or strided read stream, predicts what comes next, and stops predicting for a
stream whose recent predictions were not read. `internal/fasttier` now refuses a prefetched block
when placing it would evict one, which is the rule below and was previously written here and not in
the code.

`internal/dataserver` now drives it: every block it answers tells the detector what was read, and
what the detector predicts is fetched by one worker off the read path and offered to the tier as a
prediction. Prefetching is off unless a caller asks for it, because the depth and the accuracy floor
are guesses and a prediction costs bandwidth on a node whose bandwidth feeds an accelerator.

The manifest is still designed and not built. What is built is the half that works without one.

The manifest is designed here and not built. The tier's admission path already takes the flag a
manifest would set: `Admit` bypasses the second-read rule for a prefetched block, because a manifest
arrives before the first read and a second read is worse evidence.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A dataloader reads shards in an order that is sequential, strided, or declared | Reasoned, from how the common loaders iterate, and unverified against a real job by this project | The detector predicts nothing and prefetch is inert, which is the intended failure rather than a wrong one |
| A wrong prediction costs bandwidth and not correctness | Reasoned, and enforced below by prefetch never evicting | A wrong prediction evicts something about to be read, and prefetch makes the system slower in exactly the workloads it was built for |
| Admission on second read cannot serve the first epoch | Reasoned, and close to a proof: the second read of a shard arrives an epoch after the first | The manifest bypass is unnecessary and the tier was already good enough |
| A stream that has stopped being predictable is better left alone than predicted at low accuracy | Reasoned, from the cost being bandwidth on a node whose bandwidth feeds an accelerator | Prefetch gives up on streams it could still have helped, which is the accepted cost of the mute lifting only on a changed stride |

## Design

### There is no hint API, and that follows from the access layer

The obvious design is a call a dataloader makes to say what it is about to read. It is not offered,
because RFC-0008 puts the read path behind the kernel's own NFS client and this project deliberately
ships no client. There is nowhere for a dataloader to make that call that does not undo the decision
that keeps Forebay out of the IO path.

So a hint arrives out of band, through the control plane, as a property of the dataset rather than a
message on the read path. That is a real constraint and it shapes everything below: what Forebay
knows in advance is what somebody declared before the job started, and what it learns during the job
it has to infer from the reads themselves.

### The manifest is the declaration

A dataset may declare the order it will be read in: an ordered list of the objects making it up. It
is deliberately not a general access-pattern language. An ordered list is what a shard-based
dataloader actually has, and anything more expressive is a configuration file that will be wrong.

The manifest arrives before the first read, which is what makes it better evidence than a second one.
This answers what RFC-0007 deferred here: **admission on second read is not good enough**, and not
because it is inaccurate. It cannot fire in the first epoch at all, since the second read of a shard
arrives an epoch after the first, and for a job that reads each shard once it never fires. It is a
good rule for what it can see and it cannot see the thing that matters most.

### Predicting is on the read path and fetching is not

Observing a read is a map lookup and a subtraction, which is why it can happen where the read
happens. Fetching cannot: a read that waited for a speculative fetch would be paying for a guess made
on its behalf, which is the opposite of the point.

So predictions go to a bounded queue drained by one worker. Bounded, because a queue that grew would
trade the thing prefetch protects for the thing it provides, and a full one drops the prediction —
which costs a miss that would have happened anyway. One worker rather than several, because the aim
is to be ahead of a reader rather than to saturate the backend, and a pool of fetchers competing with
the reads they exist to help is the failure this is most likely to become.

A predicted block already resident is not fetched. A detector following a stride does not know what
the tier holds, so without that check the frontier would be refetched on every pass.

### Detection, where nothing was declared

A stream is recognised as sequential or strided from its own reads: a constant difference between
consecutive block numbers, confirmed a few times before anything is predicted. Confirmation is what
separates a pattern from a coincidence, and two reads always have a stride.

Prediction is bounded by depth rather than by time. A predictor that ran ahead by a duration would
run further ahead on a fast device, which is exactly the device where prefetching matters least.

### Prefetch never evicts

This is the rule that makes a wrong prediction cheap.

Prefetched blocks are admitted into capacity nothing is using. When the tier is full, prefetch stops
rather than choosing a victim, and the refusal says which of the two it was: a caller that only
wanted to know the tier was full sees that, and one deciding whether to keep predicting can tell a
full tier from a prediction it declined to make room for. A prediction that was wrong then costs bandwidth and a slab slot that
would otherwise have been idle, and it can never cost a block that was about to be read.

The alternative — letting prefetch evict on the grounds that its prediction is better evidence than a
block's age — is refused. It makes the failure mode of a bad prediction the failure mode of the whole
cache, and it does so under exactly the conditions where predictions are least reliable, because a
full tier means a busy node.

### A stream that stops being read stops being predicted

Predictions are remembered until they are read or age out. The share that were read is the stream's
accuracy, and a stream whose recent accuracy falls below a floor stops being predicted.

What lifts the mute is a *different* stride, not the same one continuing. That distinction is the
whole of the mechanism. A reader walking steadily whose predictions go unread — the stride is right
and the depth simply runs further ahead than the reader ever reaches — would satisfy "its stride
confirms again" on its very next two reads. It would unmute, predict, fail the floor, mute, and
repeat, wasting bandwidth on a fixed duty cycle forever. The stride that failed continuing is not
evidence that it started paying.

The cost of that rule is stated rather than hidden: a stream that would have recovered is given up on
until its pattern actually changes, and a reader that goes away is forgotten anyway.

This is RFC-0010's fast loop acting inside a limit: the depth and the floor are configured and the
engine does not move them, and being wrong costs bandwidth.

### How the benefit is measured

Two numbers, and the second is the one that falsifies the feature.

Prefetch accuracy is the share of predicted blocks that were subsequently read. It is available per
stream and is what the detector itself acts on.

The saving is RFC-0024's, unchanged: a prefetched block that is read is a tier hit, and the scoreboard
estimates what it would have cost from the backend. Prefetch that raises the hit rate without raising
the saving has bought nothing, and that is visible because the two are measured separately.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| A hint API the dataloader calls | Exact knowledge, no inference | Needs a client on the read path, and shipping one undoes RFC-0008's central decision. Every user would also have to change their loader |
| Learned prediction over collected traces | Handles patterns no heuristic will | Ruled out for v1 by RFC-0001 until heuristics are shown to fall short, and it needs traces to leave the cluster, which RFC-0016 constrains |
| Let prefetch evict | Prefetch stays useful on a full tier, which is when the node is busiest | Makes a bad prediction able to evict a block about to be read, under exactly the conditions where predictions are least reliable |
| Prefetch by time rather than depth | Naturally adapts to how fast the reader is going | Runs furthest ahead on the fastest device, which is where prefetching matters least, and turns a wrong prediction into a large one |
| Predict from the first confirmed stride | Helps sooner | Two reads always have a stride. Predicting from one is predicting from noise |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| The read stream is random | That stream | The stride never confirms and nothing is predicted. Prefetch is inert rather than wrong |
| A stride is confirmed and then abandoned | That stream | Predictions go unread, accuracy falls below the floor, and the stream stops being predicted until a different stride confirms |
| The tier is full | Every stream | Prefetch stops. Reads continue to be served and admitted on their own merits |
| A prediction is wrong at high volume | That node's bandwidth | Bounded by depth per stream and by the tier having no free capacity to put it in. The accuracy floor then stops it |
| A manifest is wrong | That dataset | The blocks it named are fetched and not read, which is the same waste as a bad prediction and is bounded the same way |

## Performance implications

The detector is a small amount of state per stream and a comparison per read. It is on the read path,
so it is on the path that matters, and it does no allocation for a stream that is not predicting.

Whether prefetch helps at all is unmeasured, because nothing fetches yet. That is stated rather than
implied.

## Complexity

Small. The detector is bounded state and arithmetic, and the rule that prefetch never evicts is what
keeps it from needing to interact with eviction policy at all.

What it makes harder later is any prefetch that would want to displace resident data, which is now a
decision to reverse rather than a knob to turn.

## Security and tenancy

A stream is per tenant and per dataset, as the tier's keys already are, so no prediction crosses a
tenancy boundary. Prefetch consumes borrowed capacity and is therefore inside the quota RFC-0016
sets, and it is reclaimable like any other borrowed byte.

An access pattern is disclosure, and predictions are derived from one. Nothing here leaves the node,
and what may leave is RFC-0016's trace-reduction rule rather than a separate one.

## Open questions

- **Whether real dataloaders read in patterns this detector recognises.** It decides whether any of
  this fires. Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns workload
  definition.
- **The prefetch depth and the accuracy floor**, which ship as guesses like every other constant in
  this project. Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), because they are
  measurements.
- **Whether prefetch should be allowed to evict once there is evidence its predictions beat a
  block's age.** This document says no on the reasoning above, not on evidence. Owned by
  [RFC-0007](0007-fast-tier-data-path.md), which owns eviction.
