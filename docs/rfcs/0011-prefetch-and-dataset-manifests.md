# RFC-0011: Prefetch and dataset manifests

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 3 |
| **Depends on** | 0007 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Training reads are unusually predictable. A dataloader walks shards in an order that is often known
in advance, and where it is not known it is frequently sequential. Hiding storage latency behind that
predictability is the most direct way to keep an accelerator fed.

The temptation is to reach for a learned model immediately. RFC-0001 rules that out for v1 on the
grounds that manifests and plain heuristics have to be shown to fall short first.

## What this RFC must answer

- The manifest format, and how a dataset declares its shard layout and expected access order
- The hint API, and how a dataloader tells Forebay what it is about to read
- Sequential and strided detection for the case where no hint is available
- The prefetch budget, and how prefetching is prevented from evicting data that is about to be read
- How the benefit is measured, so that prefetching can be shown to help rather than assumed to
- Whether admitting a block to the cache on its second read is good enough, or whether admission
  needs access-pattern knowledge the fast tier cannot see on its own. A manifest arrives before the
  first read, which is better evidence than a second one
- What happens when a prediction is wrong at high volume

## Constraints inherited from earlier RFCs

- No learned models in v1. Heuristics and manifests first
- Prefetch consumes borrowed capacity, which can be reclaimed at any time

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
