# RFC-0014: Kubernetes integration

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0004, 0005 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Kubernetes is the MVP's only orchestrator. It supplies the signals that tell Forebay compute wants
its capacity back, and it is how users will ask for storage in the first place.

The interesting part is not the CSI driver. It is that Forebay's notion of borrowed capacity and
Kubernetes' notion of node resources have to agree, or the scheduler and the storage system will each
believe they own the same bytes.

## What this RFC must answer

- The CRD set, and what belongs in a CRD as opposed to control plane state
- How borrowed capacity is represented to the scheduler, and whether it should be visible to it at all
- Which Kubernetes signals drive reclamation, including admission, ephemeral-storage requests and eviction pressure
- The CSI driver's mode of operation and its interaction with the node agent
- How the operator behaves when the control plane is unreachable
- How this integration avoids constraining a later Slurm or bare-metal adapter

## Constraints inherited from earlier RFCs

- Kubernetes is the only orchestrator in the MVP, but the node agent interface should not assume it forever

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
