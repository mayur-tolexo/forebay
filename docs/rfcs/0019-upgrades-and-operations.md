# RFC-0019: Upgrades and operations

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 4 |
| **Depends on** | 0014 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

Training runs last days or weeks. A storage system that requires a maintenance window is a storage
system that either blocks the cluster or never gets patched, and the second outcome is more common
and more dangerous.

This RFC covers upgrading Forebay underneath running work, and the day-two operations an operator
needs in order to trust it.

## What this RFC must answer

- How a node agent is upgraded without dropping the fast tier or the leases it holds
- How the control plane is upgraded, and what happens to in-flight decisions
- Version skew rules between control plane, agents, drivers and the access layer
- How a backend driver is upgraded or replaced under live data
- The runbooks an operator needs on day two, including draining a node and evacuating a rack
- How a bad upgrade is rolled back once data has been written under the new version

## Constraints inherited from earlier RFCs

- Compute always wins, which includes not being interrupted by a Forebay upgrade

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
