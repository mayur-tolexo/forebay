# RFC-0010: Autonomy engine

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0009, 0017 |

## Problem

The control plane observes what a cluster is doing and acts on it. This is the claim that Forebay is
a control plane rather than a cache, and it is also the part most likely to frighten an operator.

Actuation splits by the cost of being wrong. A fast loop moves regenerable data every few seconds,
where a mistake costs one cache miss. A slow loop adjusts durable placement over hours, where a
mistake costs real traffic and needs a guard.

The frightening part is not the loops. It is that an autonomous system tends to acquire authority it
was never given, one reasonable-looking increment at a time, and the increments are individually
defensible. This document is mostly about the boundary rather than the behaviour.

## What of this is built

**The adaptive cooldown, the kill switch, and the rule that produced them.** The node's post-reclaim
cooldown grows while reclaims keep happening and decays when they stop, and it says why it is as long
as it is. `forebay-agent --autonomy=false` holds it at its configured value while leaving
reclamation and expiry untouched. The churn budget deliberately does not adapt, and the principle
separating them — autonomy may act inside a limit and may not move one — is the whole of this
document's contribution to the code.

The fast loop's other actions exist already as mechanisms without a loop driving them: admission on
second read, eviction by reclaim order. The slow loop is not built and has nothing to move: there is
no durable placement in this system yet.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| An operator who cannot see why a decision was taken will switch autonomy off | Reasoned, and treated as a requirement rather than a prediction | Effort goes into behaviour instead of explanation, and the result is disabled |
| A signal worth acting on is already produced by doing the work | Reasoned, from what the node already computes: free space, borrowed bytes, read durations, reclaim times | Either the loops act on less than they could, or the boundary against becoming a monitoring system was drawn too tightly |
| Repeated reclaims in a short window mean the node is being asked for capacity it should not have lent, not that the workload is briefly noisy | Unverified. It is the premise of backing off, and nobody has measured how reclaims cluster on a real cluster | The node backs off from lending when it should have kept lending, and donates less than it safely could |
| Being refused a grant costs a cache miss and nothing more | Constraint from RFC-0001, and the reason the cooldown is allowed to adapt at all | An adaptive cooldown becomes an expensive mistake rather than a cheap one, and belongs behind the slow loop's guard |

## Design

### Two loops, split by what a mistake costs

| | Fast loop | Slow loop |
| --- | --- | --- |
| Period | Seconds | Hours |
| Moves | Regenerable data | Durable placement |
| A mistake costs | One cache miss | Real traffic, and possibly a migration |
| Guard | None needed | Rate limit, and an operator's approval for anything that moves durable bytes |
| Built | Partly | No |

The split is the design. Almost all of the intelligence goes where being wrong is cheap, which is
RFC-0001's constraint, and this document's job is to keep it there.

### What a loop may consume

A loop may consume signals the node already produces as a side effect of doing its work. It may not
cause a new collector to exist.

That is a boundary rather than an optimisation. A control plane that grows its own monitoring stack
becomes one, and then it is a monitoring system with a storage feature. It also settles a question
that would otherwise recur: Forebay does not read accelerator utilisation, because it cannot observe
one and RFC-0024 already established what follows from that.

The signals, then, are exactly these: free space on the pool, bytes lent by class, read durations and
their source, reclaim timestamps and durations, and the tier's hit and miss counts.

### What a loop may do

A closed list, because a general capability is not reviewable.

| Loop | Action | Status |
| --- | --- | --- |
| Fast | Admit a block to the tier | Built as a mechanism, no loop |
| Fast | Evict a block, or drop a lease in reclaim order | Built as a mechanism, no loop |
| Fast | Lengthen or shorten the post-reclaim cooldown, within its bound | **Built** |
| Fast | Ask the control plane for capacity | Not built |
| Slow | Move durable placement | Not built, and gated on an operator's approval when it is |

Anything not on this list is not an action the engine may take. Adding a row is a change to this
document, which is the point of writing it as a list.

### Autonomy may act inside a limit and may not move one

This is the rule that decides every case, and it is what separates the two tuned values RFC-0005
handed here.

**The cooldown adapts.** It exists to stop a reclaim being followed immediately by a grant that
causes the next reclaim. How long that takes depends on the workload, so a constant is a guess about
somebody else's cluster. Being wrong costs a refused grant, which costs a cache miss, which is the
cheap side of the split. So it grows while reclaims keep happening and decays when they stop.

**The churn budget does not adapt.** It is the bound: past it the node declares itself churning and
stops accepting capacity, because chronic churn is usually a scheduling problem rather than a storage
one. An engine that could raise its own churn budget would be an engine that removes its own limit
when it starts hitting it, which is the failure mode operators are right to fear.

The cooldown's growth is bounded by the churn window for the same reason. Past that point the churn
budget would stop the node anyway, so a cooldown longer than the window is authority the engine does
not need in order to do its job.

```
reclaims in the window   1     2     3     4       -> cooldown  base  2x  4x  8x, capped at the window
no reclaim for a window                            -> back to base
```

The decay is not a separate mechanism. The multiplier counts reclaims inside the churn window, so a
quiet window removes them and the cooldown returns to its base without anything having to notice.

### Oscillation

Three things prevent it, and only the first is new.

The cooldown itself is the direct answer: an action cannot immediately cause its own inverse.

The node is the only authority on its own capacity, from RFC-0004, so two control planes reacting to
the same signal cannot make the node act twice. They can both ask; only the node grants.

And the fast loop's actions are idempotent in the direction that matters. Admitting a block that is
already there does nothing, and dropping a lease that is already gone does nothing, so a duplicated
decision is not a doubled action.

### How a decision is explained

An action carries the values that caused it, at the moment it was taken, in the thing the operator
already reads. A refusal is where this is most needed, because a refusal is invisible: the node did
nothing, and doing nothing looks the same whatever the reason.

So the cooldown's refusal names its own arithmetic — how long remains, and how long the cooldown
currently is against its base and why. An operator who reads it can tell a node that is briefly
backing off from one that has been thrashing for an hour, which are the same silence otherwise.

This is deliberately not a decision log. A log is a thing to build and then to keep, and the
explanation an operator needs is at the point of the decision rather than in a history of them.
A history becomes worth building when the slow loop exists, and it belongs to that change.

### The kill switch

With autonomy disengaged the node keeps every promise and takes no discretionary action.

The distinction matters more than the switch. Reclamation is not autonomy: compute always wins is a
promise the node made, and a kill switch that stopped reclamation would turn off the safety property
rather than the intelligence. Expiry is not autonomy either, for the same reason.

What stops is discretion. The cooldown holds at its configured base rather than adapting, the fast
loop stops admitting on its own judgement, and the slow loop stops proposing. A node with autonomy
off is a node that behaves exactly as its configuration says, which is what an operator reaching for
the switch is asking for.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| One loop at one period | Simpler, one place to reason about | Forces one guard for two costs. Either the cheap action waits for an approval it does not need, or the expensive one does not get one |
| Let the engine tune every configured value | Removes the guesses, which are real | Removes the operator's bounds along with them. The engine would raise its own churn budget the moment it started hitting it |
| Central planner with a cluster-wide view | Better decisions, since a node cannot see contention it is not part of | Puts a global dependency in the request path and breaks the node's authority over its own capacity, which is what makes a partition survivable |
| Learned policy over collected traces | The decisions might genuinely be better | Unexplainable, and this document assumes unexplained autonomy is disabled autonomy. It also needs traces to leave the cluster, which RFC-0016 constrains |
| No adaptation at all, ship constants | Nothing to fear, nothing to explain | The constants are guesses about somebody else's cluster, and the cheap half of the split is exactly where a guess should be replaced by a response |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| The cooldown grows when it should not | That node's lending | It donates less than it could for up to one churn window, then returns to base. A cache miss, repeatedly, which is the cheap side by construction |
| The cooldown never grows because reclaims are spread out | That node | It behaves as it did before this document, which is the shipped constant |
| Reclaims cluster for a reason that is not the node's fault | That node's lending | The node backs off from a problem it did not cause. Stated as the unverified assumption above, and the reason the growth is bounded rather than unbounded |
| Autonomy is switched off | Everything discretionary | Promises are still kept: reclamation, expiry and refusal all continue. The node stops adapting |
| An operator reads a refusal and cannot tell why | Trust | The failure this document treats as fatal, which is why the refusal states its arithmetic rather than its conclusion |

## Performance implications

The cooldown's multiplier is a count of timestamps the node already keeps for the churn budget, taken
on a path that already walks them. Nothing measurable.

Predicted, not measured: no cluster has run this.

## Complexity

Small, because most of this document is a boundary rather than a mechanism. The adaptive cooldown is
arithmetic over an existing series.

What it makes harder to change later is the closed action list. Adding an action means editing this
document, which is intended friction and will feel like an obstacle at exactly the moment somebody
wants to skip it.

## Security and tenancy

Autonomy acts on a node's own capacity and takes no action across a tenancy boundary. The signals it
consumes are the node's own, so no loop needs a view of another tenant's behaviour, and the closed
action list contains nothing that could move one tenant's data on account of another's.

The kill switch is an operator control and not a tenant one. A tenant who could disengage a node's
autonomy could hold capacity a backing-off node would have stopped lending.

## Open questions

- **How reclaims actually cluster on a real cluster**, which is the premise of backing off at all. If
  they are spread rather than bunched the multiplier never rises and the mechanism is inert. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what this project measures.
- **What the base cooldown and the churn budget should be**, which this document does not change:
  they remain conservative guesses. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), as RFC-0005 already recorded.
- **Whether the slow loop needs a quorum rather than one operator's approval.** It depends on what
  moving durable placement can cost, and nothing in this system moves any yet. Owned by this
  document, and it should be answered by the change that builds the slow loop.
