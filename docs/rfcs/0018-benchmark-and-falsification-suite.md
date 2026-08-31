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
- How results are published, including negative ones

## Constraints inherited from earlier RFCs

- Every kill criterion in RFC-0001 has a corresponding experiment here
- Negative results are published with the same prominence as positive ones

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
