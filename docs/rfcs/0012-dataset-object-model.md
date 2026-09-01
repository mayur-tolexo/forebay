# RFC-0012: Dataset, version, snapshot and clone model

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 3 |
| **Depends on** | 0006, 0009 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

AI work is organised around datasets, versions, experiments, checkpoints and models, not volumes
and LUNs. Making those first-class in the control plane is what lets a researcher clone a dataset for
an experiment without copying it, and lets an operator understand what is consuming capacity.

Cheap copy-on-write clones are the feature that makes this worth building. They are also the feature
whose implementation depends most on what the backend underneath can actually do.

## What this RFC must answer

- The object model, and the relationships between dataset, version, snapshot, clone, experiment, checkpoint and model
- How copy-on-write is achieved across backends with very different snapshot and clone primitives
- What happens when a backend cannot clone cheaply, given that silent degradation is forbidden
- Naming, identity and immutability rules for versions
- Garbage collection of data no longer referenced by any version, and how that interacts with clones
- What a reader still holding cached blocks from a deleted dataset version should see. The bytes are
  valid for an identity that no longer exists, so serving them is arguably correct and definitely
  surprising
- How a clone interacts with the borrowed tier, since a fan-out of clones from one golden dataset is the case where caching pays most

## Constraints inherited from earlier RFCs

- Backend capabilities are declared, not assumed
- An intent that cannot be satisfied is refused loudly

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
