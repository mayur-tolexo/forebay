# RFC-0009: Intent and policy model

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 2 |
| **Depends on** | 0006 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Users should declare what they need rather than which mechanism to use. A dataset needs to survive
a rack failure, a scratch volume does not need to survive anything, and a checkpoint needs to be
durable within a stated window. Deciding how to satisfy those statements is the control plane's job.

Intent-based systems fail in a specific way: the vocabulary becomes either so vague that two users
mean different things by the same word, or so precise that it is just a configuration file with
better marketing.

## What this RFC must answer

- The intent vocabulary for durability, latency and cost, and how each maps to something a backend can be asked for
- How intents are resolved against declared backend capabilities, and what happens when none fit
- What happens when an intent is unsatisfiable for reasons that have nothing to do with a backend,
  such as a durability requirement that spans racks on a fleet whose topology cannot say which rack a
  node is in. The refusal has to be the same, and the cause is different
- How conflicting intents from different levels, such as tenant defaults against a per-volume request, are resolved
- What the defaults are, since most users will never override them and the defaults are therefore the real product
- How an intent is validated at declaration time rather than discovered to be unsatisfiable later

## Constraints inherited from earlier RFCs

- Refusal over silent degradation
- An intent that no backend can satisfy is an error, not a best effort

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
