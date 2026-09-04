# RFC-0024: Efficiency accounting and GPU hours lost to storage

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0017 |

## Problem

The project's entire justification is that accelerators wait for data and that waiting is expensive.
If Forebay cannot measure that number, it cannot prove it helps, and it cannot be argued for against
the alternative of buying a faster array.

This RFC defines the scoreboard. It is also the document most likely to be misused, because its
output ends up on a slide, and a number on a slide loses the sentence that qualified it.

So the design problem here is not arithmetic. It is building a scoreboard that cannot be quoted into
saying more than it knows.

## What of this is built

**The scoreboard, wired to the read path, and not the accelerator half of it.** `internal/efficiency`
takes the reads a node served, estimates what the ones served from the tier would have cost from the
backend, and reports the difference with the spread of the estimate it came from. `internal/dataserver`
records every block it answers, because it is the only place that knows which side served one. It refuses to convert that into GPU
hours or money without the operator supplying the two numbers only they have, and it reports a
negative result in the same place and the same way as a positive one.

What is not built is any observation of an accelerator. Forebay cannot see one, and the section below
says so rather than approximating it.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| Forebay cannot observe accelerator idle time, and nothing on the node's data path can | Reasoned, and the reason this document reports reader-seconds rather than GPU hours | The scoreboard measures something weaker than it needs to when a stronger signal was available |
| A read's cost from the backend is predicted well enough by other reads of similar size against the same backend from the same node | Reasoned, from the components of that cost: a round trip and a transfer, both of which scale with size and neither of which depends on which object it was | The counterfactual is wrong in a direction nobody can see, which is the failure this whole document exists to avoid |
| A tenant's reads are not all the same size, so most tier hits have comparable misses to be estimated from | Unverified. It depends on the dataloader, and this project has not measured one | Much of the traffic is unattributable, and the scoreboard reports a small covered fraction rather than a wrong number, which is the intended failure |
| The tier is sometimes slower than the backend | Measured. One dev node's local disk read 1.71 s against 0.23 s from the object store for the same payload | Nothing. This is the assumption that forces the scoreboard to handle a negative result properly, and if it is wrong the handling is merely unused |

## Design

### What is observed, and what is inferred

Three quantities, and they are not interchangeable.

| Quantity | Status | Where it comes from |
| --- | --- | --- |
| Reader-seconds spent inside Forebay reads | **Measured** | The read path times itself, per read, with the tenant and dataset it was for |
| Reader-seconds saved by the tier | **Estimated** | Each tier hit against what a miss of that size cost on the same node against the same backend |
| Accelerator hours saved | **Declared** | Only if an operator states how many accelerators a reader feeds and that the reader is on the critical path |

The third is not a measurement and is never presented as one. Forebay sees a socket, not a training
step: a read that took two seconds while the accelerator was busy on the previous batch cost nobody
anything, and Forebay cannot tell that read from one that stalled a step. Rather than model it, the
scoreboard makes the operator supply the conversion and stamps the result with the fact that they
did.

### The counterfactual is the node's own misses

The honest question is what would have happened without Forebay, and the only defensible answer is
one the same node produced. For each read served from the tier, the estimate of what it would have
cost is the median of the durations of *misses* — reads that did go to the backend — of comparable
size, from that node, against that backend, in the same window.

Comparable means the same power-of-two size bucket. A read's backend cost is a round trip plus a
transfer, and both scale with size and neither depends on which object it was, so size is the
variable that has to match and object identity is one that does not.

```
tier hit, 4 MiB, took 3 ms   ->   misses in the 4 MiB bucket: 41 ms median
                             ->   estimated saving: 38 ms, from 17 comparable misses
```

A bucket with no misses in it produces no estimate. Those reads are counted as **uncovered** and
reported as a fraction, because a scoreboard that quietly extrapolated across buckets would be
inventing the number it exists to defend.

### Every number carries the spread it came from

A median with no dispersion behind it invites a reader to treat an estimate as a measurement. So the
saving is reported with the interquartile range of the miss durations it was estimated from, in the
same units, in the same line. An estimate drawn from a bucket whose misses ranged from 8 ms to 900 ms
is a different claim from one drawn from a bucket that ranged from 40 to 44, and the reader is given
both.

### A negative result is reported exactly like a positive one

The tier is sometimes slower than the backend, and this project has measured that on its own dev
hardware: 1.71 s from local disk against 0.23 s from the object store, for the same payload. A
scoreboard that clamped at zero, or that reported "no saving" instead of a loss, would be the exact
dishonesty the problem statement warns about.

So the saving is a signed quantity, printed with its sign, and the report says in words when it is
negative that the tier cost this node time. There is no separate path for a bad result, because a
separate path is a path that can be left out.

### Cost and credit

Forebay never guesses a price. An operator supplies the price of an accelerator hour, and without it
the scoreboard reports seconds and refuses to report money. A default price would be a number
Forebay invented appearing in a currency, which is worse than no number.

Capacity contributed by a node is credited in byte-seconds lent, by class, which is what the lease
manager already knows. It is deliberately not converted into money either: what a lent byte-second is
worth depends on what the node would otherwise have done with it, and that is the operator's
question.

### Attribution

Per tenant and per dataset, which is what the metrics already carry. Per job is not offered: Forebay
does not know what a job is, and a label it filled in by guessing which reads belonged together would
be attribution invented rather than observed. A tenant who wants per-job attribution runs a reader
per job and gets it from the dataset label.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Read GPU utilisation from the accelerator and attribute idle windows to storage | The number everyone actually wants, directly | Attribution is the whole problem: an idle accelerator may be waiting for storage, the network, a barrier, a slow rank, or Python. Claiming a share of it needs a model this project cannot validate, and a wrong model here discredits everything else |
| Compare against a fixed backend latency figure from the vendor | Simple, stable, no bucketing | A number from a datasheet is not what this cluster's backend does under this cluster's load, and the comparison would flatter Forebay exactly when the backend is busy — the moment the tier is most likely to be argued for |
| A/B the same job with and without Forebay | The real counterfactual, unarguable | Needs the job run twice on the same hardware in the same conditions, which is a benchmark rather than a scoreboard. RFC-0018 owns doing it deliberately; it cannot run continuously |
| Report only measured quantities and no counterfactual | Unimpeachable | Answers a question nobody asked. Reader-seconds spent says nothing about whether the system helped, which is the only question that matters |
| Estimate from misses across all sizes | More comparable misses, so fewer uncovered reads | Averages a 4 KiB round trip against a 64 MiB transfer, and the result would be dominated by whichever size was most common rather than by the read being estimated |

## Failure modes

| Failure | What the scoreboard does |
| --- | --- |
| No misses in a bucket | Those reads are uncovered, counted and reported as a fraction. No estimate is produced for them |
| Almost everything is uncovered | The covered fraction is small and stated, which is the scoreboard reporting that it cannot answer rather than answering badly |
| The backend is degraded during the window | The estimate is inflated, and the spread widens with it, which is the only signal available. Stated as a known limitation rather than corrected for |
| The tier is slower than the backend | A negative saving, reported with its sign and in words |
| An operator supplies no price | Seconds are reported and money is not. No default |
| An operator supplies an accelerator count that is wrong | The declared conversion is wrong, and the report says on the same line that the number was declared rather than measured |

## Performance implications

The scoreboard is arithmetic over samples the read path already takes, and its memory is bounded in
both directions. Tier hits are counted rather than kept, since the estimate needs how many there were
and how long they took in total. Backend durations are kept per bucket up to a fixed number, oldest
dropped first.

That bound is not only a memory decision. It makes the estimate a moving window, which is what the
counterfactual wanted anyway: what a read would cost from the backend now is better answered by
recent misses than by every miss the node has ever served. A backend that got faster stops being
credited for how slow it used to be.

Predicted, not measured: nothing serves the read path at a rate that would make this matter yet.

## Complexity

Small, and deliberately smaller than the problem. Most of the difficulty in this document is in what
it refuses to compute.

What it makes harder to change later is the bucketing: reads are attributed to power-of-two buckets
at the moment they are recorded, so changing the bucket function invalidates comparisons across the
change. That is stated so that a change to it is understood as a break rather than a tuning.

## Security and tenancy

The scoreboard reports per tenant and per dataset, so it inherits the disclosure question RFC-0016
answers: a saving broken down by dataset reveals dataset sizes and access volumes. A tenant sees
their own; an operator sees the node's. Nothing here leaves the cluster, and what may leave is
RFC-0016's trace-reduction rule rather than a separate one.

## Open questions

- **Whether a dataloader's reads are varied enough in size for most tier hits to have comparable
  misses.** It decides whether the covered fraction is usable in practice, and it is a property of
  workloads rather than of this design. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns workload definition.
- **Whether an accelerator's idle time can be attributed to storage at all**, well enough to be worth
  reporting. This document says no and reports something weaker; it is not a permanent answer. Owned
  by this document, and it should be reopened only against a model somebody can validate.
