# RFC-0003: Topology model

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0002 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Forebay places data by physical proximity to the accelerator that will read it, which means it has
to know what the hardware actually looks like: which nodes share a rack, which NVMe device sits on
the same PCIe root complex as which GPU, which NUMA node a NIC is attached to, and how much network
sits between any two nodes.

Most of that information is discoverable, some of it is only discoverable partially, and some of it
is wrong. Rack membership in particular is usually a label somebody typed. A placement engine that
trusts a bad topology will confidently make things worse, so the model has to represent uncertainty
rather than assume completeness.

## What this RFC must answer

- What is the minimum topology model that is actually useful, as opposed to the most complete one
- How rack and row membership are learned, and what to do when the only source is an operator label that may be wrong
- How PCIe, NUMA and GPU to NIC to NVMe affinity are discovered, and on which platforms that discovery is unavailable
- How the model represents unknown or low-confidence facts, so placement can degrade rather than guess
- How topology changes over time are handled, including nodes moving and hardware being replaced
- Who is authoritative when discovery and operator-supplied labels disagree

## Constraints inherited from earlier RFCs

- Capability detection over assumption. No environment is required to expose full topology

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
