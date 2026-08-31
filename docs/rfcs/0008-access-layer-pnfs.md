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

## What a spike already established

Investigated 2026-08-31, from the specification and against a dev cluster. Recorded here so this RFC
starts from the answer rather than the question.

**Revocation does not depend on the client cooperating.** [RFC 8435](https://www.rfc-editor.org/rfc/rfc8435.html)
defines fencing for the flexible file layout: the metadata server changes the synthetic uid or gid
owning the data file on the storage device, which implicitly revokes the credentials the client was
given. Reclamation therefore never has to wait out an NFS lease period, which was the failure this
RFC was most at risk of.

In the loosely coupled model that fencing is not per client: "the metadata server is not able to
fence off a single client, it is forced to fence off all clients." For Forebay that is tolerable,
because every fenced reader takes a cache miss on regenerable data. Because Forebay owns both the
metadata server and the data servers, the tightly coupled model is also available, where revocation
is per client through `NFS4ERR_BAD_STATEID`. Choosing between them is now a real decision this RFC
has to make rather than an unknown.

**The client exists on the target OS.** Stock Ubuntu 24.04, kernel 6.8, ships
`CONFIG_PNFS_FLEXFILE_LAYOUT=m` with the driver present as `nfs_layout_flexfiles`, alias
`nfs-layouttype4-4`, alongside `CONFIG_NFS_V4_1=y` and `CONFIG_NFS_V4_2=y`. The commitment in
RFC-0001 not to write a client survives contact with a real node image.

**What is still unmeasured** is end-to-end revocation latency under load with a real metadata server,
since neither of the findings above involved a running pNFS deployment. That measurement belongs in
RFC-0018.

## What this RFC must answer

- Which layout type to use, and why, with flexfiles the presumed starting point
- Whether to build on NFS-Ganesha with a custom FSAL, on the in-kernel server, or on something else
- Whether to be loosely or tightly coupled, given tight coupling costs a control protocol and buys per-client revocation
- End-to-end revocation latency under load, with a real metadata server rather than from the specification
- What the client experiences when a recall cannot complete in time
- The authentication and authorisation story, and whether AUTH_SYS is acceptable in a multi-tenant deployment
- What the fallback is for clients that cannot speak pNFS

## Constraints inherited from earlier RFCs

- No custom client. If this approach requires shipping one, the approach is wrong
- Reclamation deadlines set by RFC-0005 are fixed. The protocol has to fit them, not the other way round

## Structure

Follow [`template.md`](template.md). Assumptions carry a basis of measured, reasoned or unverified.
Alternatives must be real ones.
