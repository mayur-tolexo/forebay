# RFC-0017: Observability

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 2 |
| **Depends on** | 0004 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Forebay's claims are about accelerators, so its metrics have to be about accelerators. Generic IOPS
and throughput figures do not answer the only question that matters, which is whether a GPU waited
and whether storage was the reason.

This RFC also gates RFC-0010, because autonomy without measurement is guessing with extra steps.

## What this RFC must answer

- How GPU stall attributable to storage is measured, which is the metric the entire project is judged by
- The core metric set, including delivered GB per second per GPU, cache hit rate, lease churn, reclamation latency and tail latency
- How metrics are attributed to tenant, dataset and workload without unbounded cardinality
- How a request is traced across control plane, node agent and backend
- What an operator needs on one screen to decide whether Forebay is helping
- How the autonomy engine's decisions are recorded so they can be reviewed after the fact

## Constraints inherited from earlier RFCs

- Numbers are labelled measured or predicted, and never presented as the other

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
