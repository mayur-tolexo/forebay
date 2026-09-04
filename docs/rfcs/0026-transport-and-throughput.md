# RFC-0026: Transport and the high-throughput path

| | |
| --- | --- |
| **Status** | Accepted |
| **Phase** | 2 |
| **Depends on** | 0007, 0008 |

## Problem

Forebay exists to keep accelerators fed, so the path between a node's NVMe and a GPU's memory is the
part that has to be fast. Two properties matter and they are not the same thing. Throughput decides
whether a training job can stream a dataset at the rate the GPUs consume it. Latency decides whether
a dataloader stalls between shards.

The obvious reaction is to design a protocol built for the job. That reaction deserves resistance,
because a new protocol means a new client, and not writing a client is the largest single cost the
project has avoided. Anything proposed here has to be weighed against reintroducing that cost.

This RFC separates three questions that get conflated whenever protocols are discussed, and answers
them differently.

## What of this is built

**Fabric detection, and only the half of it that was missing.** RFC-0003 already discovered whether a
node exposes an InfiniBand device. This adds whether any of its ports is actually up, because the
failure modes section below turns on presence and health being different questions, and detection
that stopped at presence would select the transport that fails worst.

Nothing else here is built and most of it should not be until RFC-0018 has said which of the three
bottlenecks binds. That is the document's position rather than a gap in it.

## Assumptions

| Assumption | Basis | Risk if wrong |
| --- | --- | --- |
| Not writing a client is the largest cost this project has avoided | Constraint from RFC-0001, and the reason the in-kernel path is chosen at every branch below | The project is more timid than it needs to be, and a purpose-built protocol was available all along |
| A fast transport is reachable without a client, because Linux implements RPC-over-RDMA in-kernel and it composes with pNFS | Reasoned, from the specifications and from the kernel's own support, and untested by this project on any hardware | The standards path does not deliver the throughput it promises, and the bar below for replacing pNFS is met sooner than expected |
| A dataloader's cost is dominated by round trips rather than by bytes, at the shard counts these workloads use | Reasoned, from a thousand shards per step per rank against per-request latency, and unmeasured | Batching is solving the wrong problem and prefetch buys nothing, which RFC-0011 would show first |
| A present RDMA fabric is not necessarily a working one | Reasoned, from how RoCE degrades without correctly configured flow control: sharply, as hangs rather than as slowness | Detection selects a transport into its worst failure mode, which is why this is the part that got built |

## Three separate questions

| Question | What it is really asking |
| --- | --- |
| **Wire transport** | How bytes cross the network. TCP, RDMA, or something else |
| **Request pattern** | How many round trips it takes to ask for what you want |
| **Endpoint copies** | How many times a byte is copied between the device and GPU memory |

Almost every claim that "NFS is too slow for AI" is really a claim about one of these three, and
which one it is changes the answer completely.

## Design

### Transport: use RPC-over-RDMA, do not invent one

NFS already has an RDMA transport. [RFC 8166](https://www.rfc-editor.org/rfc/rfc8166.html) defines
RPC-over-RDMA version 1 and [RFC 8267](https://www.rfc-editor.org/rfc/rfc8267.html) binds NFS to it.
Linux implements it in-kernel, it composes with NFSv4.1 and NFSv4.2, and therefore with pNFS.

That means the fast transport is available without writing a client, and the commitment made in
RFC-0001 survives.

```mermaid
flowchart LR
    q["Is the path fast enough?"]
    t["Transport bound?<br/>use RPC-over-RDMA"]
    r["Round-trip bound?<br/>batch the requests"]
    c["Copy bound?<br/>GPUDirect Storage · io_uring"]
    n["None of the above<br/>a new protocol is not the answer"]

    q --> t
    q --> r
    q --> c
    q --> n

    classDef control fill:#E0E7FF,stroke:#4F46E5,stroke-width:1.5px,color:#1E1B4B
    classDef fast fill:#CCFBF1,stroke:#0D9488,stroke-width:1.5px,color:#042F2E
    classDef warn fill:#FEF3C7,stroke:#B45309,stroke-width:1.5px,color:#451A03
    class q control
    class t,r,c fast
    class n warn
```

Every environment does not have RDMA, so the transport is detected rather than required, and TCP
remains a correct if slower path. RFC-0003 owns that detection.

### Endpoint copies: GPUDirect Storage, and io_uring on our side

The copy count between device and GPU memory is where latency quietly accumulates. Two mechanisms
apply at different ends of the path.

**GPUDirect Storage** moves data from the NIC or the device into GPU memory without staging it
through host memory. It works over NFS on RDMA, which means the standards path above does not
foreclose it. RFC-0001 keeps it out of v1 because it constrains hardware and only pays once the rest
of the path is fast, and nothing here changes that. It does mean v1 must not make choices that
prevent it later.

**io_uring** applies on the data server, which is our own node agent, and needs no protocol change at
all. It reduces syscall overhead on the serving side, where we control the code.

Neither of these is a protocol question, which is exactly the point.

### Request pattern: the one place a new protocol might be justified

This is where NFS genuinely has nothing to offer, and where AI workloads differ most from what NFS
was designed for.

A dataloader walking a sharded dataset issues many small reads. Each is a round trip. At a thousand
shards per step, per rank, the cost is dominated by round trips rather than by bytes, and no
transport fixes that because the transport is not what is slow. Batching is what fixes it, and NFS
has no way to express "give me these five hundred shards".

Two ways to get batching without a new general-purpose protocol:

- **Prefetch, which is already planned.** If RFC-0011 predicts the next shards correctly, the round
  trips happen ahead of the read and the dataloader never waits. Prefetch is a request-pattern fix
  disguised as a caching feature, and it is already in the roadmap. It should be measured before
  anything is designed to replace it.
- **A narrow batched-fetch sideband.** An optional endpoint on the node agent that takes a list of
  shard identifiers and streams them back in one exchange. It is not a filesystem protocol, it does
  not replace pNFS, and a client that does not speak it loses nothing but batching. It would be used
  by an integrated dataloader, not mounted.

A sideband is acceptable where a general protocol is not, because it is optional. Nobody has to
install it to use Forebay, and it cannot become a thing we are obliged to support on every kernel.

### What would justify replacing pNFS entirely

Recorded so that the bar is explicit rather than argued case by case. All three would have to hold.

1. Measurement shows RPC framing and marshalling, not transport and not round trips, dominates at the
   rates we care about.
2. Neither prefetch nor a batched sideband closes the gap.
3. The gap is large enough to justify shipping and supporting a client across kernels and
   distributions, forever.

Nobody has measured any of this. Until someone does, a new protocol is speculation with a large
maintenance bill attached.

## Complexity

Small so far, because almost nothing here is built. What the document adds is a decision procedure
rather than a mechanism.

What it makes harder later is the thing it intends to: a purpose-built protocol now has three
conditions to meet before it can be proposed, and meeting them requires measurements rather than
argument.

## Alternatives considered

| Alternative | Trade-off | Why not |
| --- | --- | --- |
| Design a purpose-built protocol now | Full control of framing, batching and flow control | Reintroduces the client we deliberately refused to write, before any measurement says the protocol is the problem |
| NVMe-oF for the fast path | Very low latency, mature, hardware offload | Block semantics, so it cannot serve the file and object views RFC-0021 depends on. Plausible later for the block path specifically |
| TCP only, keep it simple | One path to test, works everywhere | Concedes the throughput ceiling on exactly the hardware Forebay targets |
| Require RDMA | Best case performance, simpler tuning | Excludes most clusters, and capability detection is cheap by comparison |

## Failure modes

RDMA failure modes are not TCP's. A congested or misconfigured fabric fails in ways that look like
corruption or hangs rather than slow transfers, and RoCE without properly configured flow control
degrades sharply rather than gracefully. Detection has to include whether the fabric is healthy, not
merely whether it is present.

A batched sideband creates a second path to the same data, and therefore a second place for a
consistency bug. It must share the fast tier's invalidation path rather than keeping its own view,
or reclamation could be honoured on one path and not the other.

## Performance implications

All predicted. Nothing here has been measured, and the whole point of the RFC is that the ordering of
work depends on measurements nobody has taken.

## Security and tenancy

A transport choice is a tenancy question, because in-flight encryption depends on it. RDMA's
encryption story is offload-dependent and not uniformly available, and TLS over TCP costs throughput
on the path that exists to be fast. This document does not decide it, and says so in the open
questions rather than leaving a reader to assume traffic between agents is protected.

A batched sideband is a second door to the same data and needs the same authorisation as the first.
It shares the fast tier's invalidation path, which is stated in the failure modes above, and it must
share its authorisation too or it becomes a way around RFC-0016's answers.

## Open questions

- **Which of the three bottlenecks actually binds on target hardware.** This is the question, and it
  orders everything else in this document. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), which owns what this project measures.
- **Whether prefetch alone closes the round-trip gap** for realistic dataloader patterns, which
  decides whether a batched sideband is needed at all. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md).
- **Whether GPUDirect Storage over NFS on RDMA works with a metadata server we wrote**, or only with
  the configurations its vendor tests. Owned by
  [RFC-0018](0018-benchmark-and-falsification-suite.md), and it needs hardware this project does not
  have, which is the access problem that document already records.
- **Whether the block path should use NVMe-oF rather than CSI over the general path.** Owned by this
  document, which owns transport, and answerable once a block path exists to route.
- **Whether traffic between agents is encrypted in flight**, deferred here by
  [RFC-0016](0016-multi-tenancy-qos-and-security.md). Owned by this document, because whether it can
  be afforded depends on which transport is chosen and that is the first question above.
- **Whether a bandwidth QoS guarantee can be made across nodes**, deferred here by
  [RFC-0016](0016-multi-tenancy-qos-and-security.md), which makes none. Owned by this document, and
  it should not be attempted before the first question is answered.
