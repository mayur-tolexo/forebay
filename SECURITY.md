# Security policy

## Reporting a vulnerability

**Do not open a public issue.**

Report privately through GitHub's [private vulnerability
reporting](https://github.com/mayur-tolexo/forebay/security/advisories/new) on this repository. If
that is not available to you, contact the maintainer
[@mayur-tolexo](https://github.com/mayur-tolexo) directly and ask for a private channel before
sending any detail.

Please include what you found, how to reproduce it, and what an attacker gains. A working exploit is
not required and is not expected.

We aim to acknowledge a report within three working days and to give you an assessment and a plan
within ten. Forebay is a small project in its design phase, so those are honest intentions rather
than a contractual guarantee. You will be credited in the advisory unless you ask not to be.

## Supported versions

None yet. Forebay has not made a release. Once it does, this section will name the versions that
receive fixes.

## What we consider a vulnerability

Forebay will run a privileged agent on compute nodes and will hold capacity that is shared between
workloads, so its trust boundaries matter more than they would for a library. The following are in
scope, and are the areas where we would most like to be proven wrong.

- **Cross-tenant data exposure.** One tenant reading, inferring or corrupting another tenant's data
  through the cache tier, a reclaimed borrowed extent, or a backend driver.
- **Reclaimed capacity leaking its contents.** Borrowed capacity is dropped and re-lent constantly.
  Residual data surviving into the next holder is a vulnerability, not a bug.
- **Lease or capacity escalation.** A workload obtaining capacity, priority or a topology position
  it was not granted, including by lying to the node agent.
- **Node agent privilege escalation.** Escaping the agent's intended privileges, or using it to
  reach the host or another workload.
- **Control plane authorisation gaps.** Acting on a tenant, region, cluster or namespace outside the
  caller's grant. Because the control plane holds broad credentials to the systems it manages, an
  authorisation gap here is more serious than its blast radius first suggests.
- **Denial of service against compute.** Making a storage operation starve, stall or evict the GPU
  job that owns the node. Forebay's central promise is that compute always wins, so a way to break
  that promise is a security problem and not merely a performance one.
- **Supply chain.** Compromise of build, release or dependency integrity.

## What we do not consider a vulnerability

Missing hardening that no documented threat model claims, findings that require credentials the
attacker should not have in the first place, and denial of service that needs privileges equivalent
to already owning the cluster. Report them as ordinary issues and we will still read them.
