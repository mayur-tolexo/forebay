# RFC-0017: Observability

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0004 |

## Problem

Forebay's claims are about accelerators, so its metrics have to be about accelerators. Generic IOPS
and throughput figures do not answer the only question that matters, which is whether a GPU waited
and whether storage was the reason.

This RFC also gates RFC-0010, because autonomy without measurement is guessing with extra steps.

## What of this is built

**The registry, the node's metric set, the watch recording into it, and now the read path.** Every
read publishes how long it took, how many bytes it delivered and which side delivered them, so the
series this document names carry numbers rather than the zeros of a registered metric nobody writes
to. There is still no read identifier following a read across processes, and nothing yet re-reads the
two facts that decay.

The tier now publishes what it holds, what it saved and how much of its own hits that saving rests
on, and predictions publish what became of them as one series with a label rather than four series,
because an operator reads them against each other: refused against admitted is whether there is room,
and dropped against either is whether fetching keeps up.

Those three tier numbers are gauges read on the watch pass rather than written on every block. They
describe a level rather than an event, and a node with no read path publishes no sample for them at
all — a gauge nobody sets emits its declaration and no value, so having no tier stays a different
answer from a tier that saved nothing.

The bytes a prefetch fetched are deliberately not counted as delivered. They were fetched on nobody's
behalf, and counting them would make the backend look as though it served more than anyone asked
for.

`internal/metrics` holds the registry and emits the text exposition format directly, which is a
dozen lines against a client library that would be the largest dependency here. It enforces the
cardinality rule rather than documenting it: a label naming something a request carried is refused
with the reason, and `object`, `request_id` and `node` are refused by name.

`forebay-agent --metrics-addr` serves it. The whole set is registered before anything records, so a
scrape of an idle agent shows the series exist, which is what lets an alert fire on a number that
stopped moving. On a node, after four seconds of a one second watch:

```
forebay_watch_passes_total 13
forebay_headroom_bytes 1.40817819648e+12
```

Reclaim latency is labelled by the class actually taken, since only elastic promises a deadline and
reading both against it would judge opportunistic capacity by a promise it never made. Recording is
best effort: a metric that could not be written does not stop a reclaim, because the watch's job is
giving compute its disk back and failing that to report a number would be the wrong way round.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| A storage system cannot attribute a GPU stall to itself alone | Reasoned. The stall is observed on the accelerator and the cause is on the node; nothing in the read path can see the consumer's own scheduling | The headline metric is a correlation presented as a cause, which is the failure this document exists to prevent |
| Tenant and dataset are bounded label sets and object and request are not | Reasoned, from a tenant being a customer and a dataset being declared, while objects are per shard and requests are per read | Cardinality grows with traffic, and the metric store falls over at the moment the incident it would explain begins |
| An operator reads a dashboard during an incident and a metric name at no other time | Reasoned, from every operations postmortem this project's authors have taken part in | Effort goes into completeness rather than into the few numbers that decide an action |
| Silence is ambiguous and has to be made unambiguous by the emitter | Reasoned. A watch that died and a cluster where nothing is happening produce the same absence | An operator learns that reclamation stopped from the workload that ran out of disk |

## Design

### The headline metric is a correlation, and is labelled as one

The project is judged on GPU stall attributable to storage. Nothing in the read path can measure
that. A stall is time the accelerator spent with no work, and its cause is on the other side of a
dataloader this project does not own: a GPU idle because the loader was blocked on a read and a GPU
idle because the loader was blocked on the CPU look identical from here.

So this document does not claim the attribution. It publishes the two halves and the rule for
reading them together.

| | Measured by | Says |
| --- | --- | --- |
| `forebay_read_seconds` | the data path, per read | how long Forebay took to answer |
| `forebay_reads_in_flight` | the data path | whether the consumer was waiting on more than it had |
| accelerator idle | DCGM or the equivalent, which is not ours | whether the GPU had nothing to do |

An operator overlays them. Where Forebay's service time rises and accelerator idle rises with it,
storage is a candidate; where accelerator idle rises and service time does not, it is not. Publishing
a single number called *GPU stall caused by storage* would be a correlation wearing a causal name,
and RFC-0001's constraint that numbers are labelled measured or predicted applies to their names too.

**The one attribution that is honest is the one the consumer makes.** A dataloader that reports the
time it spent blocked on a read can attribute its own stall, because it is the thing that waited.
That needs a client-side hook, it belongs to whoever writes the client, and this document asks for it
rather than inventing it here.

### The core metric set

Small on purpose. Every metric here answers a question an operator acts on, and a metric nobody acts
on is a cardinality bill nobody budgeted for.

| Metric | Type | Labels | Question |
| --- | --- | --- | --- |
| `forebay_read_seconds` | histogram | `tenant`, `source` | Is Forebay slow, and from the tier or the backend |
| `forebay_read_bytes_total` | counter | `tenant`, `source` | What is it delivering, and from where |
| `forebay_reads_in_flight` | gauge | | Is the consumer queueing on it |
| `forebay_tier_hits_total` | counter | `tenant` | Is the cache worth having |
| `forebay_tier_bytes` | gauge | | How much of the tier is resident |
| `forebay_lease_bytes` | gauge | `class` | What is lent, by how reclaimable it is |
| `forebay_reclaim_seconds` | histogram | `class` | Is the deadline being met |
| `forebay_reclaim_shortfall_bytes` | counter | | Did the node fail to give capacity back |
| `forebay_headroom_bytes` | gauge | | What floor is being kept, which moves |
| `forebay_watch_passes_total` | counter | | Is the watch alive, which silence cannot say |
| `forebay_pool_reserve_bytes` | gauge | | What the filesystem holds for everything not ours |
| `forebay_topology_degraded_total` | counter | `fact` | Did this node stop being able to see something |

`source` is `tier` or `backend` and has two values forever. `class` is the three lease classes.
`fact` is the small set of things topology discovers.

### Cardinality is bounded by what is declared, never by what is requested

The rule is one line: **a label may name something a human declared, and may not name something a
request carried.**

| Allowed | Refused |
| --- | --- |
| `tenant`, because a tenant is a customer and there are as many as there are contracts | `object`, because there is one per shard and a dataset has millions |
| `dataset`, because a user declared it and `kubectl get` lists them | `request_id`, because there is one per read |
| `class`, `source`, `fact`, because each is a closed set in this document | `node` on a control-plane metric, because the control plane sees every node and the node already labels its own |

A node's own metrics carry no `node` label. The scrape knows which node it came from, and adding it
doubles the series count to say something the collector already recorded.

Where a high-cardinality identifier is genuinely needed to explain a number, it goes in a log line
carrying the same request identifier, not in a label.

### Following one read across three processes

There is no tracing library here, for the same reason there is no Kubernetes client library: it
would be the largest dependency in the repository and it would buy a format this project can emit in
a dozen lines.

A read carries an identifier from the access layer to the node agent to the backend driver. Each
process logs it once, structured, with what it did and how long it took. Correlating them is a
search on that identifier.

| | Carries | Where it comes from |
| --- | --- | --- |
| Access layer | `read-id` | Generated per read, or taken from the client's own if it has one |
| Node agent | the same | The wire protocol's request already has a field for it |
| Backend driver | the same | An HTTP header the store echoes into its own access log |

That last row is the one worth having: when a read is slow and the store's log agrees it was slow,
the argument about whose fault it is ends.

### Silence is made unambiguous by the emitter

A watch that has died and a cluster where nothing is happening produce the same absence of events,
so the watch publishes a counter that increments on every pass whether or not it found anything.
`forebay_watch_passes_total` not increasing is the alert, and it is an alert on a counter that must
move rather than on an event that may not.

The same reasoning applies to reclamation. A node that has reclaimed nothing for a day is either
healthy or blind, and the pass counter tells them apart.

### Readiness is a latency, not a ping

A node agent that answers slowly is worse than one that has stopped: a stopped one fails its liveness
probe and is replaced, while a slow one keeps accepting reads that the miss path never rescues,
and every client waits on it.

Readiness therefore fails when the read path's recent service time exceeds a configured bound, and
recovers when it falls back under. The bound is an operator's number because it is a promise to their
workload, and this document has not measured a default, which RFC-0018 owns.

Liveness stays what RFC-0004 made it: a heartbeat, so a wedged agent is killed and its replacement
can take the node lock.

### Two facts that decay after startup

RFC-0003 discovers a node's capacity and the reserve its filesystem holds for everything which is
not Forebay, once, at startup. Both keep moving afterwards, and nothing watches them.

| Fact | How it decays | What it costs |
| --- | --- | --- |
| The reserve | Container images, logs and neighbouring workloads keep writing | The node lends capacity something else has already taken |
| Topology | A kernel or driver change makes a device, a NUMA node or a link invisible | Placement degrades silently and looks like a hardware change nobody made |

Both are re-read on the watch's own interval, since it is already polling the filesystem and the cost
of one more read is nothing. A reserve that has grown reduces what may be lent immediately. A
topology fact that has become unknown increments `forebay_topology_degraded_total` and is reported by
name, because *this node can no longer see its NUMA layout* is an operator action and a silent
placement change is not.

**A fact that got poorer is reported and never quietly acted on in the other direction.** A reserve
that shrank might be a workload that finished or a measurement that got worse, and lending against
the difference would turn a bad reading into a promise.

### A backend that lost a capability under a dataset relying on it

A declaration is a snapshot of a backend that can be reconfigured. A dataset resolved against a store
that could snapshot, on a store that no longer can, is a promise the system has stopped being able to
keep.

The controller already re-resolves every dataset on its own interval. It compares the capabilities
the backend now declares against those the dataset's intent needs, and where one has gone the dataset
records it in status and the condition is what an operator sees. The dataset is not deleted and its
data is not touched: the declaration is still what the user wants, and what changed is the system's
ability to honour it.

### One screen

| Panel | Says |
| --- | --- |
| Read service time, tier against backend | Whether Forebay is fast, and which half is answering |
| Tier hit rate | Whether the cache is earning the capacity it borrowed |
| Accelerator idle, overlaid on service time | Whether storage is a candidate for the stall |
| Lent bytes by class, and headroom | What the node has promised and what it is keeping back |
| Reclaim latency against its deadline | Whether the promise that makes lending safe is being met |
| Watch passes, and shortfall | Whether the thing that keeps it safe is running, and whether it failed |

Six panels, and the third is the one the project is judged on. An operator who can see only that
storage is fast and the GPU is idle has learned that storage is not the problem, which is the most
common true answer and the one a storage dashboard is least willing to give.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Publish a single *GPU stall caused by storage* number | One number an executive can read | It is a correlation with a causal name, and the first time it is wrong it costs more credibility than it ever bought |
| A tracing library and a collector | Spans, sampling and a UI, for free | The largest dependency in a repository that has none, to emit a format this can emit in a dozen lines |
| Label metrics by object | Every question answerable from metrics alone | One series per shard: the cardinality bill arrives exactly when traffic does |
| Readiness as a liveness ping | Simple, and what most systems do | A slow agent passes it, and a slow agent is the failure that hurts, because the miss path never fires |
| Re-read topology only on events | Cheaper, and event-driven is tidier | There is no event for *a driver upgrade made this invisible*, which is the case that matters |

## Failure modes

| Failure | What happens | Why it is acceptable |
| --- | --- | --- |
| The metrics endpoint is unreachable | The scrape gaps and the panels break | It is out of the IO path entirely: reads do not consult it and do not stop when it stops |
| A histogram's buckets are wrong for the hardware | Tail latency is unreadable in the range that matters | Buckets are configuration, and the wrong ones are visible as everything landing in one bucket |
| The reserve re-read is itself wrong | The node lends less than it could, or more | Lending less is a lost opportunity and lending more is a broken promise, so the direction that is acted on is the conservative one |
| Readiness flaps | The node is taken in and out of service | The bound carries hysteresis, and a flapping node is reported rather than hidden |
| A tenant label is high cardinality after all | The metric store degrades | The declaration is the limit: a tenant that does not exist in the control plane cannot appear in a label |

## Performance implications

The read path gains one histogram observation and two counter increments per read, which is tens of
nanoseconds against a read measured in hundreds of microseconds at best. The watch gains two
filesystem reads per pass, on a path that already does one.

Nothing here is on the miss path's critical section, and nothing takes a lock the data path holds.

## Complexity

One package that owns the registry and the exposition format, and one endpoint. The exposition format
is text, is a dozen lines to emit, and is stable enough that this project can write it directly rather
than depend on a library to.

The complexity this document deliberately does not take on is a collector, a storage backend or a
dashboard. It emits what a scrape reads and stops.

## Security and tenancy

The metrics endpoint carries tenant names, which are customer identities, so it is not public. Where
it binds is the operator's, and the agent takes an address rather than choosing one, since a default
that guessed wrong would be a customer list on a public port. RFC-0016 is unwritten and will own the
rule for operator surfaces when it exists; until then this document states the requirement and does
not pretend somewhere else has already settled it.

A tenant may not see another tenant's series. Since the node's endpoint carries every tenant on it,
the endpoint is an operator surface and a per-tenant view is the control plane's to build from it,
which is RFC-0016's problem rather than this one's.

No metric carries a credential, an endpoint or an object name. An object name is a customer's data
by another route, and the cardinality rule that keeps it out of a label keeps it out of a scrape.

## Open questions

- **What readiness bound a node should use**, since a slow agent is worse than a stopped one and the
  threshold is a promise to a workload. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns the values this project has not
  measured.
- **Whether a dataloader will carry a read identifier**, which decides whether the one honest
  attribution of a stall is available at all. No RFC owns it, because it is a question about what
  somebody else's client is willing to do rather than about design here.
- **How the autonomy engine's decisions are recorded.** Owned by
  [RFC-0010](0010-autonomy-engine.md), which already carries it, since a record of a decision needs
  the decision to exist first and no loop has been written.
- **What a per-tenant view of these metrics looks like**, which is a control plane surface rather
  than a node one. Owned by [RFC-0016](0016-multi-tenancy-qos-and-security.md), which owns what one
  tenant may see of another.
