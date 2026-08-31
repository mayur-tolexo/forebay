# RFC-0000: RFC process

| | |
| --- | --- |
| **Status** | Draft |
| **Phase** | 0 |
| **Depends on** | — |

## Problem

Storage systems fail in ways that are invisible in a code review. The interesting parts are what
happens when a node is slow rather than dead, what the system does with a half-written extent, and
which of two bad options was chosen and why. None of that survives in a diff, and a year later
nobody can reconstruct whether a decision was reasoned or accidental.

Forebay is also making a claim that might be wrong. A process that only records decisions, and not
the conditions under which they should be revisited, would let a wrong bet quietly become an
assumption.

## Design

Every substantial design decision is an RFC in this directory before it is implemented.

Substantial means: it changes an interface others depend on, it introduces a failure mode, it
constrains what can be built later, or reasonable engineers would disagree about it. Bug fixes,
refactors and anything reversible in an afternoon are not RFCs.

### Lifecycle

```
Draft ──► Accepted ──► Implemented
  │           │
  │           └──► Superseded
  └──► Rejected
```

An RFC opens as `Draft` in a pull request. Discussion happens in the pull request. It becomes
`Accepted` when maintainers agree it is ready and every open question is either answered or
explicitly deferred in the document. It becomes `Implemented` when the code exists and the RFC has
been updated to describe what was actually built, which is rarely identical to what was proposed.

`Superseded` RFCs name their replacement. `Rejected` RFCs keep their reasoning and are never
deleted. Neither is edited into agreement with a later decision, because the record of what was
believed and when is the thing worth keeping.

### What every RFC must contain

Assumptions, alternatives, failure modes, and complexity. The template enforces the shape.

Assumptions carry a basis: measured, reasoned, or unverified. This matters more here than in most
projects because Forebay has an unproven performance thesis, and an unverified assumption that gets
quietly restated as fact is how a project talks itself into a wrong architecture.

Alternatives must be real. Listing a strawman to dismiss it is worse than listing nothing, because
it creates the appearance of rigour without any.

### Deciding

By argument. Evidence outranks seniority: a measurement beats an opinion, and an opinion grounded in
production experience beats an unsupported preference. A maintainer breaking a tie says why, in
writing, in the RFC.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Design in issues and pull requests | Lower friction, faster | The reasoning ends up scattered across threads and is unfindable within months |
| A design document per release | Fewer documents to maintain | Bundles unrelated decisions, so disagreeing with one means arguing with all of them |
| No formal process until there are users | Nothing to maintain early | The decisions that are hardest to reverse are made before the first user arrives |

## Failure modes

The obvious one is ceremony: an RFC process can turn into a queue that stops anyone from building
anything. The mitigation is the narrow definition of substantial above, and a bias toward accepting
with recorded open questions rather than holding out for completeness.

The subtler one is RFCs drifting from the code, so the documents describe a system that no longer
exists. `Implemented` requires updating the RFC to match what shipped, and an RFC that does not
match reality should be treated as a bug report against itself.

## Open questions

How much of this survives contact with more than a handful of contributors. It will be revised.
