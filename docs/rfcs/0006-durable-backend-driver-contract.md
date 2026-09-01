# RFC-0006: Durable backend driver contract

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0002 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Forebay does not write a durable store. It drives someone else's: Ceph, OpenEBS, S3, or an array
the operator already owns. The contract between the control plane and those backends is one of the
project's two seams.

The contract must not be a lowest common denominator. Backends differ enormously in what they can
do, and a contract that only exposes the intersection would throw away the capabilities that make
each backend worth using.

## What this RFC must answer

- The capability vocabulary: snapshots, clones, thin provisioning, replication, topology hints, ranged reads, and what else is load bearing
- How an intent is refused when no backend can satisfy it, and how that refusal reaches the user
- How the same logical operation is expressed against backends with very different primitives
- What a conformance suite for a driver looks like, so a third-party driver can prove it works
- How the contract is versioned as capabilities are added, without breaking existing drivers
- Whether the donated pool is a driver of its own or simply devices contributed to a durable store that is already running

## Constraints inherited from earlier RFCs

- Silent degradation is forbidden. A backend that cannot satisfy an intent causes a loud refusal
- Two implementations, Ceph and S3, ship together so the contract is designed against more than one backend

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
