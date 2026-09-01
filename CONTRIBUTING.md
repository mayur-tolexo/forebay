# Contributing to Forebay

Forebay is in its design phase. There is no usable code yet, which means the most valuable thing you
can contribute today is an argument, not a patch.

If you have run large GPU clusters, operated Ceph at scale, fought with pNFS, or watched a checkpoint
storm take out a filesystem, your objections to the RFCs are worth more to this project than any
feature.

## The fastest way to help

1. Read [RFC-0001](docs/rfcs/0001-thesis-scope-and-non-goals.md). It states what Forebay is betting
   on and the conditions under which that bet is wrong.
2. Try to break it. Open an issue with the failure case. Concrete beats abstract: naming the
   hardware, the workload and the numbers you have seen is far more useful than a general worry.
3. If you are right, we change the RFC and credit you in it.

## How RFCs work

Every substantial design decision lands as an RFC in [`docs/rfcs/`](docs/rfcs/README.md) before it is
implemented. This keeps the reasoning reviewable and stops decisions from being buried in a diff.

| Status | Meaning |
| --- | --- |
| `Draft` | Being written or actively discussed. Comment freely. |
| `Accepted` | Agreed. Implementation may start. |
| `Implemented` | Shipped, and the RFC describes what actually exists. |
| `Superseded` | Replaced. The header names the RFC that replaced it. |
| `Rejected` | Considered and declined. The RFC stays, with the reasoning intact. |

Rejected RFCs are never deleted. Knowing what was considered and turned down is most of the value of
having written them down at all.

To propose one: copy [`template.md`](docs/rfcs/template.md), take the next free number,
open a pull request with status `Draft`. Small numbers are already reserved by the index; do not
renumber someone else's RFC.

Every RFC must state its assumptions, the alternatives that were considered, the trade-off that
decided it, and the failure modes it introduces. An RFC that only describes the chosen design has
not done its job.

## Claiming work

Anything in the index with no assignee is open. Comment on the tracking issue and it is yours. If
you go quiet for a few weeks we will unassign it, with no hard feelings: life happens, and other
people should not be blocked waiting.

## Code, once there is code

Forebay is written in Go.

- Every exported function, method and non-obvious flow carries a short comment explaining why it
  exists or what constraint it satisfies. Comments that restate the code are noise, and reviewers
  will ask you to remove them.
- New code comes with tests. Total statement coverage must stay at or above 80 percent, which
  `make check` enforces. A change that cannot be tested should explain why in the pull request.
- `make check` passes before you ask for review. It runs gofmt, vet, the race-enabled test
  suite and the coverage gate, and CI runs exactly the same target so there are not two sets of
  rules to keep in step. It also enforces the documentation invariants in `internal/docscheck`,
  since a truncated table or a link to a file that moved is easier to catch than to notice.
- No speculative code. Constants, helpers and struct fields that nothing references yet do not get
  merged, however likely they look to be needed later.

## Pull requests

Write the description for someone who was not in the conversation. Say what changed and why the
change is correct. Skip the templated headings and just explain it in plain prose. Link the RFC the
change implements.

Sign your commits off under the [Developer Certificate of Origin](https://developercertificate.org/):

```
git commit -s
```

That sign-off is your statement that you have the right to contribute the code. Forebay does not use
a CLA and has no plans to.

## Reporting security problems

Do not open a public issue. See [SECURITY.md](SECURITY.md).
