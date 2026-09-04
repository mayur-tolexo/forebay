# RFC-0009: Intent and policy model

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0006, 0014 |

## Problem

Users should declare what they need rather than which mechanism to use. A dataset needs to survive
a rack failure, a scratch volume does not need to survive anything, and a checkpoint needs to be
durable within a stated window. Deciding how to satisfy those statements is the control plane's job.

Intent-based systems fail in a specific way: the vocabulary becomes either so vague that two users
mean different things by the same word, or so precise that it is just a configuration file with
better marketing.

## What of this is built

**The vocabulary, its resolution against a backend, and the refusals.** `internal/intent` turns a
declaration into the capabilities it needs and answers whether a given backend can serve it, naming
what is missing when it cannot. The dataset CRD carries it and the controller records the answer.

There is no admission webhook, so an unsatisfiable intent is refused in a status rather than at
`kubectl apply`. The vocabulary itself is enforced at apply time by the schema, which is the half of
declaration-time validation a CRD can do without one:

```
The Dataset "bad-word" is invalid: spec.intent.durability: Unsupported value:
"eleven-nines": supported values: "none", "backend", "replicated", "rack-tolerant"
```

A declaration inside the vocabulary that this cluster cannot meet is accepted and then recorded, with
both causes named:

```
NAME          OBJECT                PRESENT   SATISFIABLE   BYTES
wants-racks   forebay-ctl/present   true      false         1048576

intent: unsatisfiable: the backend does not declare replicate,
and this fleet cannot say which rack a node is in
```

The object is still there and the declaration is still the user's. What changed is the system's
ability to honour it.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A small closed vocabulary is more useful than an expressive one | Reasoned, from the failure this problem statement names: an expressive one becomes a configuration file, and a vague one means different things to two users | Users cannot say what they need and go around the system |
| Most users never override a default, so the defaults are the product | Reasoned, from every system with a default in it | Effort goes into options nobody sets while the default is wrong |
| A backend's declaration is true | Constraint from RFC-0006, which refuses an undeclared capability before the driver is reached | An intent resolves against a promise the backend cannot keep, and the failure arrives when the data is wanted |
| Two causes of unsatisfiability need one refusal | Reasoned. A user asked for something and did not get it; whether the reason was a backend or a fleet's own blindness is the operator's problem and not theirs | A user is told to change a backend they do not control, for a fact about topology they cannot see |

## Design

### The vocabulary is three axes and a closed set on each

| Axis | Values | Means |
| --- | --- | --- |
| `durability` | `none` | Losing it costs a refetch. Scratch |
| | `backend` | The durable store holds it, and its guarantee is the guarantee |
| | `replicated` | The store keeps more than one copy, and says so |
| | `rack-tolerant` | The copies are in different racks, and the fleet can say which rack a node is in |
| `latency` | `best-effort` | Read it from wherever it is |
| | `cached` | Keep it in the fast tier, and say so when it is not |
| `cost` | `cheapest` | Prefer the store, and do not spend borrowed capacity on it |
| | `balanced` | The default |

Nine words. Each maps to something a backend declares or a node can be asked for, and a word that
mapped to nothing would be marketing.

### Every value maps to a capability, or it is not in the vocabulary

| Value | Requires | From |
| --- | --- | --- |
| `durability: none` | nothing | |
| `durability: backend` | `read-range` | The mandatory core, so every backend satisfies it |
| `durability: replicated` | `replicate` | The backend's declaration |
| `durability: rack-tolerant` | `replicate`, and a fleet that knows its racks | The declaration, and RFC-0003's topology |
| `latency: cached` | a fast tier on the node serving it | The node, not the backend |
| `cost: cheapest` | nothing, and it forbids `latency: cached` | |

The last row is the only pair that contradicts: asking for the cheapest thing and asking to hold it
in borrowed memory-speed capacity are different requests. That contradiction is refused at
declaration rather than resolved by picking one, because picking one silently is the degradation
RFC-0001 forbids.

### One refusal, two causes

An intent that cannot be satisfied is refused the same way whether the reason is a backend that
cannot replicate or a fleet whose topology cannot say which rack a node is in. The user asked for
something and did not get it; which of those it was is the operator's problem.

The refusal names both, so the operator is not left guessing:

```
rack-tolerant needs replicate, which this backend does not declare,
and a fleet that can say which rack a node is in, which this one cannot
```

**A fleet's blindness is not the user's fault and is not hidden from the operator.** RFC-0003
discovers rack identity and is allowed not to find it, so an intent that needs it is unsatisfiable on
a fleet that cannot see it, and saying so is the only honest answer. The alternative, treating an
unknown rack as a distinct rack, would satisfy the intent by assuming the thing it was asked to
guarantee.

### Levels resolve by specificity, and a tenant sets a floor rather than a ceiling

| | Sets | Wins |
| --- | --- | --- |
| Tenant default | What a dataset gets when it says nothing | Where the dataset is silent |
| Dataset | What this one needs | Where it is stronger than the tenant's floor |
| Tenant floor | The least a dataset may ask for | Where the dataset asks for less |

A tenant may raise the floor and may not lower a dataset's request. That asymmetry is deliberate: an
administrator protecting data from a careless user is a case worth serving, and one weakening a
user's stated requirement without telling them is the silent degradation this project refuses.

Where a dataset asks for less than the floor, the refusal says so rather than quietly upgrading it,
because a user who asked for `none` and got `replicated` will be billed for it.

### The defaults are the product

| Axis | Default | Why |
| --- | --- | --- |
| `durability` | `backend` | The store is already durable and its guarantee is what the user bought |
| `latency` | `best-effort` | Borrowed capacity is scarce and a user who has not said they need it should not be spending it |
| `cost` | `balanced` | |

A dataset that declares nothing gets the store's own durability and no borrowed capacity, which is
the behaviour of a system Forebay is not installed on. That is the right default: installing this
should change nothing until somebody asks it to.

### Validation at declaration, as far as a CRD can

| Checked | Where | When |
| --- | --- | --- |
| The word is in the vocabulary | The CRD's schema | At `kubectl apply` |
| The pair is not contradictory | The controller | On the next pass |
| The backend can satisfy it | The controller | On the next pass |
| The fleet can satisfy it | The controller | On the next pass |

Only the first is at apply time, and the rest need an admission webhook this project has not written.
That is a gap and is named as one: a user learns their intent was unsatisfiable a pass later rather
than immediately, and the answer is in a status they have to look at.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| A numeric SLO, such as nines of durability | Precise, and comparable between systems | A number nobody can verify per dataset is a promise nobody can keep, and it would be a configuration file with better marketing |
| Free-form policy expressions | Expressive, and any future need fits | It is the failure mode this problem statement opens with |
| Resolve a contradiction by precedence | Every intent is satisfiable | The user asked for two things and gets one, silently, which is the degradation RFC-0001 forbids |
| Treat an unknown rack as its own rack | `rack-tolerant` works on any fleet | It satisfies the intent by assuming the thing it was asked to guarantee |
| A tenant ceiling as well as a floor | Administrators can cap spending | It weakens a user's stated requirement without telling them, and cost belongs in quota rather than in intent |

## Failure modes

| Failure | What happens | Why it is acceptable |
| --- | --- | --- |
| A backend loses a capability an intent relied on | The next pass records the dataset as unsatisfiable | The declaration is still what the user wants; what changed is the system's ability to honour it, and deleting their data over it would be worse |
| Topology becomes able to see racks after a fix | Datasets that were unsatisfiable become satisfiable | Resolution is per pass, so this needs nothing but time |
| A user declares an intent no backend here will ever satisfy | It stays unsatisfiable and says why | Refusal over silent degradation, and the reason names both causes |
| The vocabulary needs a tenth word | It is added, and old declarations keep meaning what they meant | Closed sets are additive; a value that changed meaning would not be |

## Performance implications

Resolution is a set membership test against a declaration the driver already published, on the
controller's own pass. It is not in the read path and touches nothing a read holds.

## Complexity

One package with a vocabulary, a mapping and a refusal. The complexity deliberately avoided is a
policy engine: this is nine words and a table, and it can be read in full by whoever has to operate
it.

## Security and tenancy

An intent is a user's declaration and carries no credential. Resolution reads a backend's
declaration, which is the operator's, and a fleet's topology, which is the operator's, so the answer
tells a user whether their request can be met without telling them anything about the infrastructure
that would meet it beyond the words they used.

A tenant floor is set by whoever administers the tenant, and RFC-0016 owns who that is.

## Open questions

- **Whether an admission webhook should refuse an unsatisfiable intent at apply time**, so a user
  learns immediately rather than a pass later. Owned by
  [RFC-0014](0014-kubernetes-integration.md), which owns what this project puts in front of the API
  server.
- **Who may set a tenant's floor**, which is an authorisation question rather than a vocabulary one.
  Owned by [RFC-0016](0016-multi-tenancy-qos-and-security.md), which owns tenancy.
- **What `latency: cached` means when the fast tier is on a node the client is not reading from**,
  since a tier is per node and an intent is per dataset. Owned by
  [RFC-0007](0007-fast-tier-data-path.md), which owns where a block lives.
