# RFC-0023: Lineage, provenance and immutable versions

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 3 |
| **Depends on** | 0012, 0021 |

## Problem

Six months after a model ships, somebody asks which data produced it. The honest answer is usually a
guess assembled from job logs, a bucket path that has since been overwritten, and somebody's memory.

Dataset versions here are immutable and share extents rather than being copied, so keeping every
version is cheap enough to be the default. Once versions are immutable and free, recording what was
trained on what is a matter of writing down edges in a graph.

The temptation is to present that graph as proof. It is not proof, and a lineage system that
overstates what it knows is worse than none: it converts an honest guess into a confident wrong
answer, which is the thing an auditor is there to catch.

## What of this is built

**The graph, and the distinction that keeps it honest.** `internal/lineage` holds nodes and edges
where every edge records whether Forebay observed it or somebody asserted it, and answers an ancestry
query with what it could not see attached to the answer rather than omitted from it.

What is not built is anything that produces edges. Observed edges come from the read path and the
checkpoint path, and neither is written. The graph is the part that has to be right first, because a
graph that could not tell an observation from an assertion would be unfixable afterwards.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A job's own report of what it read cannot be relied on | Reasoned, from what goes wrong: not dishonesty but stale configuration, a path that moved, and a rerun that picked up a different version | Observed edges are unnecessary machinery and a declaration would have done |
| Forebay observes only what it served, so observed lineage is a lower bound and never a complete account | Reasoned, and close to a definition: a job that read around Forebay is invisible to it | The graph is presented as complete, which is the failure this document exists to prevent |
| Immutability enforced above a store cannot bind that store's own owner | Reasoned, from where the credentials are: an operator with admin rights on the backend can rewrite the objects | Tamper-evidence is unnecessary and the guarantee could have been stated more strongly |
| Keeping every version is affordable because versions share extents | Constraint from RFC-0020, and the reason lineage can be the default rather than an option | Retaining lineage costs capacity per version, and it becomes something operators switch off |

## Design

### Nodes and edges

Four kinds of node and three kinds of edge, which is deliberately fewer than the domain has.

| Node | What it is |
| --- | --- |
| Version | One immutable dataset version, as RFC-0012 defines it |
| Run | One execution of a job |
| Checkpoint | One saved training state |
| Model | One thing somebody decided to ship |

The edges between them are as few, and each is a sentence rather than a category.

| Edge | Asserts |
| --- | --- |
| `read` | A run read a version |
| `produced` | A run produced a checkpoint |
| `promoted` | A checkpoint became a model |

Those three between those four kinds cannot form a cycle, and that is worth noticing rather than
checking for. Nothing leads back into a run or into a version, so a traversal cannot loop and there
is no cycle check to forget or to get wrong. A fourth relation that closed the graph would have to be
added to the same table, which is where the property is stated.

The directions differ, and the traversal has to respect that rather than pick one. A run's ancestors
are the versions it read, which its own edges point at; a checkpoint's ancestor is the run that
produced it, which points at the checkpoint. Storing every edge pointing at its ancestor instead
would make the graph read backwards everywhere it is written.

RFC-0012 asks here whether experiment, checkpoint and model should become kinds of dataset rather
than attachments. They are separate kinds, because a checkpoint is not read the way a dataset is and
giving it a dataset's lifecycle would make every checkpoint a candidate for the fast tier. What they
share is versioning, and they get it by referring to versions rather than by being them.

### Every edge says how it is known

This is the whole design.

| Basis | Meaning |
| --- | --- |
| **Observed** | Forebay saw it happen on a path it served |
| **Declared** | Somebody asserted it |

A `read` edge can be observed, because a read of a version goes through the address RFC-0021 fixed. A
`produced` edge can be observed when the checkpoint is staged through the node, since RFC-0013 makes
that a guaranteed lease Forebay grants. A `promoted` edge is always declared, because promoting a
checkpoint to a model is a decision rather than an event, and no amount of instrumentation makes a
decision observable.

Mixing the two without saying which is which is what turns a useful record into a misleading one, and
it is why the distinction is in the type rather than in a convention.

### An answer carries what it could not see

Forebay observes only what it served. A job that read a dataset directly from the object store is
invisible to it, and no design fixes that from inside Forebay.

So an ancestry query does not return a graph. It returns a graph and an account of itself: how many
of its edges were observed, how many were asserted, and which versions in it can no longer be
retrieved. A caller that wants to say "this model was trained on these versions" can see, on the same
answer, how much of that is Forebay's word and how much is somebody else's.

The alternative — returning the graph and letting the caller ask about completeness separately — is
how the misleading version gets built, because the second call is the one that gets skipped.

### What immutability actually guarantees

Less than it sounds like, and this is worth stating plainly.

A published version is immutable to every path Forebay controls: no write, no rename, no partial
overwrite, per RFC-0021. It is not immutable to an operator with admin credentials on the durable
store, who can delete or rewrite the objects underneath, and no property of a control plane above a
store can prevent that.

What is available is tamper-evidence. A version records a digest of its contents when it is
published, and the digest is part of the lineage node rather than of the mutable metadata. A rewritten
object then produces a different digest and the mismatch is detectable, which is the honest form of
the guarantee: Forebay can tell you that something changed, and cannot stop it.

### Deletion, and what a lineage reference keeps alive

A version's bytes and its lineage node have different lifetimes, and conflating them is the mistake
this section exists to avoid.

Deleting a version unnames it and eventually frees its bytes. Its lineage node stays, with its digest
and its edges, because the historical fact that a run read it does not stop being true. A query that
reaches such a node reports it as **recorded but not retrievable**, which is a different answer from
absent and a much more useful one: it says the model was trained on data that existed and is gone,
rather than saying nothing.

Lineage therefore does not pin bytes. A graph that kept every referenced version alive would grow
without bound and would make the cheapest thing in the system, keeping a record, into the most
expensive.

### Retention locks, and what they cost

A version may be locked, which refuses deletion until the lock expires. Regulated users need this.

The cost is stated rather than hidden: a locked version's bytes cannot be freed, so a lock takes
capacity out of the pool for its whole term. That makes it a quota decision rather than a metadata
one, and a lock that would exceed a tenant's quota is refused at the moment it is asked for rather
than discovered when the pool fills. RFC-0016 owns the quota it is refused against.

A lock is on a version rather than on a lineage node, because a lineage node costs nothing to keep
and locking it would be locking the wrong thing.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Trust the job's own report of what it read | Simple, complete, needs no instrumentation | The failure is not dishonesty, it is stale configuration and a rerun that picked up a different version — the exact cases lineage is consulted about |
| One edge type with a free-text relation | Extensible without changing the model | Extensible into meaning nothing. Three relations that are checkable beat any number that are not |
| Record only observed edges | Nothing in the graph is anyone's word but ours | Loses promotion entirely, since deciding to ship a model is not an observable event, and the graph stops answering the question it was built for |
| Pin every version a lineage node refers to | Reproducibility that always works | Makes keeping a record the most expensive thing in the system, and grows without bound. Retention locks offer the same thing where it is actually wanted, with the cost visible |
| Make checkpoints and models kinds of dataset | One lifecycle, one set of machinery | Gives a checkpoint a dataset's lifecycle, including candidacy for the fast tier, which is capacity spent on bytes nothing reads twice |

## Failure modes

| Failure | Blast radius | What happens |
| --- | --- | --- |
| A job reads around Forebay | That run's lineage | Its reads are unobserved. The answer says how much of itself was observed, so the gap is visible rather than silent |
| A version's bytes are deleted | Queries reaching it | Reported as recorded but not retrievable, which is the true answer |
| The backend's objects are rewritten by an admin | Trust in the whole graph | Detectable by digest mismatch and not preventable. Stated as the limit of what immutability above a store can mean |
| A cycle is asserted | The graph | Not constructible. The kinds a relation may run between form a graph with no way back into a run or a version |
| A lock would exceed a tenant's quota | That tenant | Refused when asked for, rather than discovered when the pool fills |
| Edges accumulate for a job that reran a thousand times | The graph's size | Bounded by nothing here, and stated as such: this document does not solve graph growth |

## Performance implications

Adding an edge is a map write and a cycle check that walks backwards from the new edge. Querying is a
traversal. Neither is on the read path, which is what makes lineage affordable to keep by default.

Predicted, not measured. No graph in this project has had anything in it.

## Complexity

Small, and most of it is the provenance distinction, which is one field and a great deal of
discipline about where it is set.

What it makes harder later is any consolidation of the four node kinds, since callers will have
written queries that depend on a checkpoint not being a dataset.

## Security and tenancy

A lineage graph is a disclosure surface: it says which datasets a tenant holds, which runs read them,
and what shipped. It is scoped per tenant like everything else, and a cross-tenant edge is the same
question as a cross-tenant clone, which RFC-0016 answers — off unless the owner grants it.

The digest deserves care. It identifies content, so two tenants holding identical data would record
identical digests, and comparing them across a tenancy boundary would disclose that fact. Digests are
therefore visible only within the tenant that recorded them, which is the same rule RFC-0016 applies
to extent sharing and for the same reason.

## Open questions

- **How a run is identified**, since a rerun of the same job is a different run and something has to
  say so without trusting the job to name itself. Owned by
  [RFC-0014](0014-kubernetes-integration.md), which owns what this project can see of a workload.
- **How the graph is bounded** for a job that reran ten thousand times. This document does not solve
  it and says so. Owned by this document, and it should be answered before anything writes edges at
  volume rather than after.
- **Whether tamper-evidence is worth the cost of digesting at publication**, which reads every byte
  of a version once. Owned by [RFC-0018](0018-benchmark-and-falsification-suite.md), because what it
  costs is a measurement.
