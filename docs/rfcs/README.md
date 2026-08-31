# Forebay RFCs

Every substantial design decision in Forebay is written down here before it is implemented.

The point is not ceremony. It is that a storage system's hard parts are its failure modes and its
trade-offs, and those are invisible in a diff. An RFC has to state what it assumed, what else was
considered, and what it makes worse, so that someone can disagree before the code exists rather than
after it has users.

Rejected RFCs stay in the repository. Knowing what was considered and declined, and why, is most of
the value of writing them down.

## Status

Nothing is accepted yet. Forebay is in Phase 0 and the whole index is open for argument.

RFC-0021 supersedes the unified-namespace non-goal in RFC-0001. That reversal is recorded rather than
edited away, which is the point of keeping these.

| # | Title | Status | Phase | Depends on |
| --- | --- | --- | --- | --- |
| [0000](0000-rfc-process.md) | RFC process | Draft | 0 | — |
| [0001](0001-thesis-scope-and-non-goals.md) | Thesis, scope and non-goals | Draft | 0 | — |
| [0002](0002-architecture-overview.md) | Architecture overview | Draft | 0 | 0001 |
| [0003](0003-topology-model.md) | Topology model | Not started | 1 | 0002 |
| [0004](0004-node-agent.md) | Node agent | Draft | 1 | 0002, 0003 |
| [0005](0005-capacity-pools-and-elastic-leases.md) | Capacity pools and elastic leases | Draft | 1 | 0002, 0004 |
| [0006](0006-durable-backend-driver-contract.md) | Durable backend driver contract | Not started | 1 | 0002 |
| [0007](0007-fast-tier-data-path.md) | Fast tier data path | Not started | 1 | 0005, 0006 |
| [0008](0008-access-layer-pnfs.md) | Access layer over pNFS | Not started | 1 | 0007 |
| [0009](0009-intent-and-policy-model.md) | Intent and policy model | Not started | 2 | 0006 |
| [0010](0010-autonomy-engine.md) | Autonomy engine | Not started | 2 | 0009, 0017 |
| [0011](0011-prefetch-and-dataset-manifests.md) | Prefetch and dataset manifests | Not started | 3 | 0007 |
| [0012](0012-dataset-object-model.md) | Dataset, version, snapshot and clone model | Not started | 3 | 0006, 0009 |
| [0013](0013-checkpoint-path.md) | Checkpoint path | Not started | 3 | 0007, 0009 |
| [0014](0014-kubernetes-integration.md) | Kubernetes integration | Not started | 1 | 0004, 0005 |
| [0015](0015-failure-model.md) | Failure model and split brain | Not started | 4 | 0005, 0007 |
| [0016](0016-multi-tenancy-qos-and-security.md) | Multi-tenancy, QoS and security | Not started | 4 | 0005, 0009 |
| [0017](0017-observability.md) | Observability | Not started | 2 | 0004 |
| [0018](0018-benchmark-and-falsification-suite.md) | Benchmark and falsification suite | Not started | 1 | 0007, 0008 |
| [0019](0019-upgrades-and-operations.md) | Upgrades and operations | Not started | 4 | 0014 |
| [0020](0020-no-copy-policy.md) | The no-copy policy | Draft | 1 | 0002, 0006, 0007 |
| [0021](0021-single-copy-multi-protocol.md) | Single-copy multi-protocol access | Draft | 3 | 0012, 0020 |
| [0022](0022-data-aware-scheduling.md) | Data-aware scheduling and warm start | Not started | 2 | 0003, 0007, 0014 |
| [0023](0023-lineage-and-reproducibility.md) | Lineage, provenance and immutable versions | Not started | 3 | 0012, 0021 |
| [0024](0024-efficiency-accounting.md) | Efficiency accounting, GPU hours lost to storage | Not started | 2 | 0017 |
| [0025](0025-cross-cluster-datasets.md) | Cross-cluster and cross-region datasets | Not started | 4 | 0006, 0021 |
| [0026](0026-transport-and-throughput.md) | Transport and the high-throughput path | Draft | 2 | 0007, 0008 |

`Not started` means the file holds a problem statement and the questions the RFC has to answer, but
nobody has written it. Those are the ones to claim.

## Writing one

Copy [`template.md`](template.md), take the next free number, open a pull request with status
`Draft`. Do not renumber an existing RFC, including a rejected one.

Read [0000](0000-rfc-process.md) first. It is short.
