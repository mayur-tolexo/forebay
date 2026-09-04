# RFC-0025: Cross-cluster and cross-region datasets

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 4 |
| **Depends on** | 0006, 0021 |

## Problem

GPU capacity is spread across clusters and regions, and datasets are not. A team with capacity free
in one region and its training data in another either copies the whole dataset and pays for it twice,
or leaves the accelerators idle.

Because dataset versions are immutable, a read-only copy in a second region is a well-defined thing
rather than a synchronisation problem. The hard part is not moving bytes. It is deciding what a
dataset means when it exists in more than one place, and what an operator is promised about
consistency, cost and residency.

## What of this is built

**Residency, because it is the one part that cannot be added afterwards.** `internal/residence`
decides whether a version may exist in a region, with deny winning over allow and an unnamed region
refused rather than assumed permitted. Everything else here — the transfer itself, the trigger, the
accounting — is machinery that can be built later against a rule that was right from the start. A
residency rule added later is a rule that was not enforced for everything already moved.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A published version is immutable, so a second copy of it never needs reconciling | Constraint from RFC-0021, and the reason distribution is tractable here and not for mutable filesystems | Distribution becomes a synchronisation problem, which is the thing this document claims to avoid |
| Two regions can agree they hold the same version by comparing content rather than by trusting a name | Reasoned, and available because RFC-0023 already records a digest at publication | Identity depends on names agreeing across administrative boundaries, which is the assumption that fails first |
| Some data may not legally leave a region, and getting that wrong is not recoverable by deleting the copy | Reasoned, from what a residency breach is: a disclosure that has already happened | Residency is treated as a preference and enforced late, which is the failure this document builds against |
| A region that pulled a dataset is the region that can choose not to | Reasoned, and the basis of the cost rule below | Cost lands on a party with no control over it, which makes the feature something operators disable |

## Design

### Identity is content, and location is not part of it

Two regions agree they hold the same version because the digest RFC-0023 records at publication
matches. Not because the names agree: names are administered locally and two clusters under different
administration will eventually disagree about one.

That has a consequence worth stating. A version existing in three regions is **one** version, with one
lineage node, and location is an attribute of where its bytes are rather than of what it is. Lineage
does not fork when a dataset is distributed, which is what the stub asks and what falls out of
identity being content.

### A remote copy is a cache, not a replica

This is the decision everything else follows from.

| | Replica | Cache |
| --- | --- | --- |
| Can be dropped without loss | No | Yes |
| Needs a durability commitment | Yes | No |
| Survives the origin going away | Yes | No |
| Needs lifecycle kept in sync | Yes | No |

Forebay's remote copies are caches. Dropping one loses nothing, because the origin still holds the
version and the version is immutable, so it can be fetched again. That is the same property borrowed
capacity has, and it means a remote copy inherits the reclamation model rather than needing a new one.

The cost is real and is not hidden: a region reading a distributed dataset depends on the origin being
reachable for anything not resident. A replica would survive the origin going away, and would need
every synchronisation mechanism this design avoids. RFC-0001's constraint that Forebay never holds
the only copy of anything is what makes the cache answer available at all.

### Distribution is declared, never inferred from demand

A version crosses a boundary because somebody said it should — an intent on the dataset, or an
explicit operation — and never because a job in another region read it.

Demand-triggered distribution is the tempting design and it is refused. A single read would move
terabytes across a region boundary while looking exactly like a read, the first job to touch a
dataset would pay for the whole transfer, and the bill would arrive attached to a cost centre that
made no decision. Worse, it would make a residency breach reachable by reading, which is the one
class of mistake that cannot be undone by deleting the copy.

### Residency is a rule the code cannot be run without

Some data may not leave a region, and a breach is a disclosure that has already happened: deleting the
copy afterwards does not undo it. So residency is not a policy consulted where somebody remembered to
consult it.

Three rules, and each exists because of a specific way this goes wrong.

**Deny wins over allow.** A version permitted in a region by one rule and forbidden by another is
forbidden. The alternative makes adding a permission able to silently remove a prohibition.

**An unnamed region is refused.** A destination the fleet cannot name is not a destination this
version may go to, on exactly the reasoning RFC-0003 uses for every other fact: an unknown never
satisfies a requirement. Treating unknown as permitted is how data reaches a region nobody meant to
include.

**A version already somewhere it may not be is a finding, not a fact of life.** The same check answers
"may it move here" and "should it be here", so a rule tightened after the fact reports what is now in
breach instead of applying only to future transfers.

### What crosses the boundary, and what does not

The access path does not cross. A region serves its own pods from its own export, over the local
network path, and what crosses a boundary is the transfer that fills that region's storage.

This answers what RFC-0016 deferred here. RFC-0016's use of AUTH_SYS relies on network policy
establishing which tenant is calling, and it warned that a cluster boundary is where that stops
holding. It stops holding for an export that crosses a boundary — and none does. `RPCSEC_GSS` is
therefore not required by this design, and mounting an export across a cluster boundary is
unsupported rather than merely discouraged, because AUTH_SYS there is not authentication at all.

The transfer itself is authenticated by the credential it runs under, which is a backend credential
under RFC-0006 rather than a filesystem identity.

### Cost

The pulling region pays the transfer. The origin keeps paying to store the version.

The rule is chosen for who can act on it: a region that decided to pull can decide not to, and an
origin cannot stop another region reading a dataset it published. Charging the origin for a transfer
it did not request would make publishing a dataset an open-ended liability, and the predictable
response to that is to stop publishing.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Remote copies as replicas | Survive the origin going away, and a region can be self-sufficient | Needs durability commitments and lifecycle synchronisation across administrative boundaries, which is the problem immutability was supposed to remove |
| Distribute on observed demand | No operator action, and data arrives where it is used | A single read moves terabytes and bills whoever read first, and makes a residency breach reachable by reading |
| Identity by name and version | Human-readable, no digest to compute | Names are administered locally, and two clusters under different administration will eventually disagree about one, silently |
| Residency as a label checked by a controller | Simple, familiar, easy to add later | Checked where somebody remembered to check. A breach is not recoverable, so the check has to be the only way through |
| Charge the origin for transfers out | The origin published it, so it caused the demand | Makes publishing an open-ended liability with no control over the size of it, and the predictable response is not to publish |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| The origin region is unreachable | Remote regions' misses | Reads of resident blocks continue; anything else fails. This is the cost of caches over replicas, and it is stated rather than mitigated |
| A residency rule is tightened after a version moved | That version | The check answers both questions, so the version is reported as in breach rather than quietly grandfathered |
| A destination region cannot be named | That transfer | Refused. Unknown is not permission |
| Two regions disagree about a version's name | Nothing | Identity is the digest, so they still agree about the version |
| A transfer is interrupted | That region's copy | It is a cache fill, so a partial one is discarded and refetched. No reconciliation, because there is nothing to reconcile against |

## Performance implications

The residency check is a comparison against two small lists, off the read path entirely: it runs when
a transfer is planned, not when a block is read.

Transfer performance is unmeasured and nothing in this project has moved bytes between regions.

## Complexity

Small so far, because only the rule is built. What the document adds is mostly refusal: no replicas,
no demand-triggered distribution, no export across a boundary.

What it makes harder later is offering true replicas, which several of these answers now rest on.

## Security and tenancy

Residency is the tenancy question here and the design's answer is above. Two others are worth naming.

A digest identifies content, so two regions holding identical data hold identical digests. Comparing
digests across an administrative boundary would disclose that two organisations hold the same data,
which is why identity is compared within a distribution relationship somebody established rather than
broadcast.

And the transfer credential is a backend credential that reaches another region's store. It is the
widest thing this design introduces, and it is bounded the way RFC-0016 bounds every other one:
per tenant, and only for as long as the transfer it authorises.

## Open questions

- **How a distribution intent is expressed**, since it is a property of a dataset and the dataset
  surface belongs elsewhere. Owned by [RFC-0014](0014-kubernetes-integration.md), which owns what
  this project puts in a cluster.
- **Whether a region's own capacity accounting should distinguish a distributed cache from local
  borrowing.** They are the same bytes under the same reclamation rules, and an operator may still
  want to see which is which. Owned by [RFC-0005](0005-capacity-pools-and-elastic-leases.md), which
  owns what a pool reports.
- **What a transfer costs between real regions**, which decides whether declared distribution is
  usable or whether the latency of arranging it defeats the purpose. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), and it needs two clusters this project does
  not have.
