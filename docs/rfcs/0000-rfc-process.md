# RFC-0000: RFC process

| | |
| --- | --- |
| **Status** | Accepted |
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
`Implemented` when the code exists and the RFC has been updated to describe what was actually built,
which is rarely identical to what was proposed.

### What accepted requires

Four things, all checkable by someone who did not write the document.

1. **Every assumption carries a basis**, one of measured, reasoned or unverified, and the basis is
   the truth rather than the flattering reading of it. An assumption marked measured on evidence that
   does not address it is worse than one marked unverified, because it stops anyone looking again.
2. **Every open question is answered, or deferred to a named owner.** Naming an owner is not enough
   on its own: the owner has to carry the item, which means the question appears in that document
   rather than only in the one deferring it. A question nobody owns is acceptable when the document
   says so and why, which is usually that it is a question about people rather than engineering.
3. **No claim in it is one we could not defend**, including claims about what has been measured, what
   another document requires, and what the code does.
4. **It says which parts are built**, including when the answer is none. A document that reads as
   description while half of it is intention is how someone comes to rely on behaviour nobody wrote.
   This applies to any RFC describing a system, which is every one of them except this process
   document and the thesis, neither of which describes anything that could be built.

Accepted does not mean finished. A later measurement can supersede an accepted RFC, and one already
has: RFC-0021 superseded a non-goal in RFC-0001.

### What the template requires, and what it does not

The template's nine sections are for RFCs that describe a system. A process RFC such as this one
has no performance implications and no tenancy boundary, and inventing those sections to satisfy a
count would be exactly the kind of filled-in-because-the-field-exists writing the rest of this
document argues against.

So: problem, design, alternatives, failure modes and open questions are required of every RFC.
Assumptions, performance implications, complexity, and security and tenancy are required of any RFC
describing a system, which is all of them except this one and RFC-0001. A process document and a
statement of the thesis have no tenancy boundary and no complexity estimate, and writing those
sections to satisfy a count is the thing this exemption exists to prevent.

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

### Checks that do not need a person

Four things have been got wrong often enough to be worth checking rather than remembering, and all
four were found in review rather than imagined.

Three of them run as tests in `internal/docscheck`, so `make check` and CI enforce them without
anyone choosing to:

- No markdown table contains a blank line, since GitHub ends a table at the first one and the rows
  after it render as literal text. This has happened twice.
- Every relative link resolves.
- Every open question in an accepted RFC names an owner or says why it has none.

The fourth cannot be automated honestly and is done by hand:

- Every claim that another RFC owns something resolves to a **real item** in that document. A
  keyword search is not sufficient and has produced false passes here more than once, matching
  "network headroom" in an unrelated sentence and "tail latency" in a metric list while neither
  document contained the delegated question. Verifying it means reading the target for the specific
  item, which is judgement rather than string matching.

A check that can pass while the thing is untrue is worse than no check, because it converts a gap
into a confident all-clear. A check that exists only as a sentence in a document is worse still,
because a reader believes it is running.

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

- **How much of this survives contact with more than a handful of contributors.** No RFC owns this,
  deliberately: it is answered by having contributors rather than by deciding anything now, and it
  is deferred to the point where there are some. It will be revised then.
