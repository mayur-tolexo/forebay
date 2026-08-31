# RFC-0024: Efficiency accounting and GPU hours lost to storage

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 2 |
| **Depends on** | 0017 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

The project's entire justification is that accelerators wait for data and that waiting is
expensive. If Forebay cannot measure that number, it cannot prove it helps, and it cannot be argued
for against the alternative of simply buying a faster array.

This RFC defines the scoreboard: how much accelerator time was lost waiting on storage, which
datasets and which tenants it was lost to, and what it cost. It is the metric the whole system is
judged by, and it is also the most persuasive artefact the project can produce, because it converts
an architectural argument into a number a finance team recognises.

It has to be honest in both directions. A measurement that flatters Forebay is worth nothing.

## What this RFC must answer

- How accelerator idle time is attributed to storage rather than to the many other things that stall a training step
- How the counterfactual is estimated, since the honest question is how much time would have been lost without Forebay
- Cost attribution per dataset, per tenant and per job, and where the price of a GPU hour comes from
- How capacity contributed by a node is credited back to whoever donated it
- How to present this so it cannot be quoted misleadingly, given it will end up on slides
- What to report when the answer is that Forebay did not help

## Constraints inherited from earlier RFCs

- Numbers are labelled measured or estimated, never presented as the other
- A metric that cannot be reproduced by a sceptic is not a metric

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
