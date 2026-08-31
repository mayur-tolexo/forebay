# Glossary

Terms used across the RFCs, with the meaning Forebay gives them. Several are ordinary words with a
narrow meaning here, which is exactly the kind of thing that causes arguments later.

| Term | Meaning |
| --- | --- |
| **Compute pool** | The slice of a node's NVMe owned by the workload running on it. Forebay never touches it |
| **Donated pool** | A slice of node NVMe permanently committed to Forebay by an operator. Holds durable data and is never reclaimed |
| **Borrowed pool** | Node NVMe lent to Forebay revocably. Holds regenerable data only, so it can be reclaimed by deletion |
| **Regenerable** | Data whose loss costs time but not correctness: cache, prefetch, scratch, checkpoint staging. The defining property of the borrowed pool |
| **Lease** | A grant of borrowed capacity to Forebay, with a class and a reclamation contract |
| **Reclamation** | Returning borrowed capacity to compute. Always a delete, never a migration |
| **Fast tier** | The layer Forebay owns: borrowed capacity on the local node and on rack peers, with its placement and prefetch logic |
| **Durable backend** | An external system holding durable data, reached through a driver. Ceph, OpenEBS, S3, or an existing array |
| **Capability negotiation** | A driver declaring what it can do, so the control plane uses what exists and refuses what does not, rather than degrading silently |
| **Intent** | A declaration of what is needed, such as durability or latency, as opposed to which mechanism to use |
| **Fast loop** | Autonomy operating on the borrowed tier every few seconds, where a wrong decision costs a cache miss |
| **Slow loop** | Autonomy operating on durable placement over hours, where a wrong decision costs real traffic and is guarded |
| **Crossover** | The point at which a node's local device bandwidth exceeds its achievable share of backend fan-out. Whether it is reachable is the project's central open question |
| **Seam** | An extension point. Forebay has two: protocols above the fast tier and durable drivers below it |
