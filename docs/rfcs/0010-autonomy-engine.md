# RFC-0010: Autonomy engine

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 2 |
| **Depends on** | 0009, 0017 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

The control plane observes GPU utilisation, NVMe utilisation, network headroom, cache hit rates and
capacity pressure, and acts on them. This is the claim that Forebay is a control plane rather than a
cache, and it is also the part most likely to frighten an operator.

The design splits actuation by the cost of being wrong. A fast loop moves regenerable data every few
seconds, where a mistake costs one cache miss. A slow loop adjusts durable placement over hours,
where a mistake costs real traffic and needs a guard.

## What this RFC must answer

- What signals the loops consume, and how they are obtained without becoming a monitoring system in their own right
- What actions each loop may take, stated as a closed list rather than a general capability
- The safety envelope on the slow loop, including rate limits, quorum and human approval
- How oscillation is prevented when two loops or two control planes react to the same signal
- Whether the node agent's tuned values should adapt rather than be configured, specifically its
  headroom target, its post-reclaim cooldown and its churn budget, all of which currently ship as
  conservative guesses
- How a decision is explained after the fact, since an operator who cannot see why will turn it off
- What the kill switch is, and what the system does with it engaged

## Constraints inherited from earlier RFCs

- Almost all intelligence lives where being wrong costs a cache miss
- Autonomy that cannot be explained will not be trusted, and untrusted autonomy is disabled autonomy

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
