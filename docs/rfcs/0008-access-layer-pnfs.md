# RFC-0008: Access layer over pNFS

| | |
| --- | --- |
| **Status** | Not started |
| **Phase** | 1 |
| **Depends on** | 0007 |

> **Unclaimed.** This file holds the problem statement and the questions the RFC has to answer.
> Nobody has written it yet. See [CONTRIBUTING.md](../../CONTRIBUTING.md) to claim it.

## Problem

pNFS separates a metadata server from data servers: a client requests a layout, then reads bulk
data directly and in parallel from the data servers. That is the architecture Forebay wants, already
standardised, with a mature in-kernel client shipped by Linux. The control plane becomes the metadata
server and node agents become data servers.

The risk sits precisely where the protocol meets the lease model. Forebay reclaims capacity on the
compute scheduler's timetable, which means layouts have to be recalled from clients that are actively
reading. Whether that is fast and well behaved, or slow and ugly, decides whether this approach
survives.

## What this RFC must answer

- Which layout type to use, and why, with flexfiles the presumed starting point
- Whether to build on NFS-Ganesha with a custom FSAL, on the in-kernel server, or on something else
- Layout recall latency and semantics under lease reclamation, which is the question this RFC exists to answer
- What the client experiences when a recall cannot complete in time
- The authentication and authorisation story, and whether AUTH_SYS is acceptable in a multi-tenant deployment
- What the fallback is for clients that cannot speak pNFS

## Constraints inherited from earlier RFCs

- No custom client. If this approach requires shipping one, the approach is wrong
- Reclamation deadlines set by RFC-0005 are fixed. The protocol has to fit them, not the other way round

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
