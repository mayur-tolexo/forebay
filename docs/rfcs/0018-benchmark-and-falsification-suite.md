# RFC-0018: Benchmark and falsification suite

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0007, 0008 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

RFC-0001 states five conditions under which Forebay should be abandoned. This RFC turns them into
experiments, and it is the most important document in Phase 1.

Its purpose is not to produce a favourable number. It is to find out, as cheaply and as early as
possible, whether the locality premise holds on hardware that matters. A benchmark suite designed to
make the project look good would be worse than none.

## What this RFC must answer

- The experiment that locates the crossover between node-local bandwidth and a node's achievable share of backend fan-out
- How compression is held constant while locating that crossover. The measurement this project was
  founded on compared a compressed 226 MiB object crossing the network against 687 MiB read raw from
  local disk, so roughly three to one of the apparent advantage was compression rather than fan-out.
  A backend that serves compressed bytes against a tier that serves raw ones is not a comparison of
  locality, and the suite has to say which side compression sits on before the number means anything
- Whether compressing the fast tier pays for the CPU it costs, given that CPU on a GPU node competes
  with the dataloader
- How compute impact during reclamation is measured, so that the job is unaffected can be shown rather than asserted
- Workload definitions that reflect real training and inference access patterns rather than synthetic sequential reads
- The scaling curve method across one node, one rack, ten racks and beyond, and where scaling is expected to stop
- Hardware profiles the results are valid for, since a result on one generation of NVMe and NIC may not transfer
- How idle compute-local NVMe is measured across a real fleet, and over what window, since capacity
  that is idle only in short bursts is not capacity worth borrowing
- How much value static provisioning would have captured on the same workload, since if the split
  rarely needs to move then a simpler system is the right answer
- What fraction of a real workload's storage traffic is regenerable, since that sets the ceiling on
  how much a tier holding only regenerable data can ever be worth
- How long revoking a reader actually takes against a running metadata server under load, since the
  elastic reclaim deadline cannot be honoured if revocation is slower than it
- What headroom a node has to keep free for reclamation to stay ahead of a workload's writes, which
  is the central tuning value of the agent's pressure design and currently has no defensible default
- Whether hostNetwork earns its cost on the data path, measured against an ordinary pod network with
  the extra hop it implies, since the answer decides how much isolation the agent gives up
- What the driver conformance suite runs against, since proving a driver needs a real backend and a
  contributor may not have one
- What cache block size the fast tier should use, since it trades index size against read
  amplification and the number should come from measurement rather than from taste
- Whether a rack-local hop beats going straight to a fanned-out backend, which is the crossover
  question asked one hop further out and decides whether the rack tier exists at all
- How long the fast tier should wait on a peer before abandoning it for the backend, which has to be
  shorter than the backend read it is avoiding or trying is worse than not trying
- How large the fast tier's record of first reads has to be before admission on second read fires at
  all, since a bound too small to span two reads of the same block admits nothing and a bound too
  large costs memory the cache could have used
- How much of a reader's working set one lease holds, since reclamation drops whole leases and that
  number is what turns a per-block refetch cost into the size of the burst a reader actually feels
- Whether reading a KV cache block back from borrowed NVMe beats recomputing the prefill that
  produced it, and above what prefix length, since below some length the read is strictly worse than
  not having tried and that point decides whether [RFC-0027](0027-kv-cache-spill.md) is worth writing
- Whether an inference-serving node has idle NVMe to borrow at all, which is the third kill
  criterion asked of a fleet the existing experiments do not cover: a training node's disk is idle
  between epochs, and a serving node's may be absent or already busy
- How results are published, including negative ones

## Constraints inherited from earlier RFCs

- Every kill criterion in RFC-0001 has a corresponding experiment here
- Negative results are published with the same prominence as positive ones

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
