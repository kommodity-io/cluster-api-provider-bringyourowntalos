# PRD: BYOT teardown — drain and delete workload Nodes on Machine deletion

Tickets: [PLA-6507](https://linear.app/corti/issue/PLA-6507), [PLA-6506](https://linear.app/corti/issue/PLA-6506)

## Problem

Two coupled deletion bugs surface when BYOT static machines are removed:

1. **PLA-6507 — full cluster uninstall leaves workload Nodes up.** `helm
   uninstall` removes the `Cluster` and `Machine` CRs, but the workload-cluster
   Nodes stay `Ready` and the Talos VMs keep running. The cluster is gone from
   management, yet the workload is orphaned — teardown is incomplete.

2. **PLA-6506 — commenting out a nodepool does not remove its `Machine`.** The
   chart annotates `ByotMachine` and `Machine` with
   `helm.sh/resource-policy: keep`, so removing a static-machine entry from
   `values.yaml` and running `helm upgrade` leaves the `Machine`/`ByotMachine`
   in the cluster (no longer tracked by Helm), so the machine is never deleted
   and never leaves the workload roster.

### Root cause

Both trace to the same design assumption in the original join/split PRD
([PRD-join-split-policies.md](./PRD-join-split-policies.md), R2):

> `splitPolicy: None` — delete the Machine CR; no talos reset. The node still
> leaves the cluster roster: core Cluster API's Machine deletion flow drains
> the workload-cluster Node and deletes the Node object before the infra
> resource is released (provider-independent).

This assumption **does not hold for BYOT**. CAPI's Machine deletion flow drains
and deletes the workload Node through the **cluster cache** (a connection to
the workload API server established while the `Cluster` exists and is
reachable). On full `helm uninstall`, the `Cluster` is deleted first; the
cluster cache tears the workload connection down before the `Machine`
controllers reconcile deletion, so `reconcileNode` fails with *"error getting
client: connection to the workload cluster is down"* and the Machine is
removed without ever draining/deleting its Node. The Node survives in the
workload cluster (which is still up, owned by the Talos config the machines
retain).

PLA-6506 is the chart-side enabler: `helm.sh/resource-policy: keep` on
`ByotMachine`/`Machine` (intended so CAPI, not Helm, owns deletion) means a
removed-from-values `Machine` is never deleted by Helm on `upgrade`, and CAPI
only deletes a Machine when its owning `Cluster`/`MachineSet` triggers it —
which never happens for an orphaned static `Machine` with no parent trigger.
So the `Machine` lingers, its `ByotMachine` finalizer lingers, and the Node
stays.

## Goal

- **Teardown completeness (PLA-6507):** deleting a BYOT `Cluster` (helm
  uninstall) removes the workload `Node` objects for every adopted machine
  (drained then deleted), then releases the `ByotMachine`/`Machine` CRs.
- **Single-machine removal (PLA-6506):** removing a static-machine entry from
  values and running `helm upgrade` deletes the corresponding
  `ByotMachine`/`Machine` (and drains/deletes its workload Node), so the
  machine leaves the workload roster.

Both must honor `splitPolicy`: `Reset` wipes (STATE+EPHEMERAL) the machine
*before* releasing it; `None` leaves the Talos config/datastore intact but the
**Node is always removed from the workload roster** regardless of splitPolicy.

## Non-goals

- Changing CAPI's cluster-cache teardown ordering (upstream, out of scope).
- Preserving workload-cluster availability after the control plane is removed
  (a single-node control plane teardown inherently loses the workload API).
- Re-adopting orphaned workload Nodes (a re-adopt is a separate flow; this PRD
  only covers clean removal).
- Changing `splitPolicy` semantics (still `None` default = non-destructive to
  the Talos machine; `Reset` = wipe).

## Design

### D1 — byot owns workload-Node drain+delete on deletion

The byot `ByotMachine` reconciler gains a **deletion-time Node cleanup** step,
run *before* the split reset and finalizer release, using a short-lived
workload-cluster client built directly from the cluster's kubeconfig secret
(`<cluster>-kubeconfig`) plus the machine's `providerID`/`nodeRef` — **not**
the CAPI cluster cache (which is already torn down during Cluster deletion).

For each `ByotMachine` being deleted:

1. Resolve the workload Node:
   - Prefer `Machine.status.nodeRef.name` if set; else match by
     `spec.providerID` (`byot://<publicIP>`) against workload Nodes.
2. If a Node exists and the workload API is reachable:
   - Cordon + drain the Node (evict pods, respecting `PodDisruptionBudget`s
     with a timeout), then `DELETE` the Node object.
   - Failures (API unreachable, drain timeout) are **non-fatal to deletion**:
     record a `NodeCleanup` warning condition and proceed, so a deletion is
     never permanently blocked by an unreachable workload (e.g. the control
     plane already gone). The Node, if still up, will be reaped on the next
     adopt's reset or manually.
3. Then proceed with the existing `reconcileDelete`: `splitPolicy: Reset`
   wipes the Talos machine; `None` releases untouched; finalizer removed.

Rationale: byot is the only component that can reach the workload API
*independently* of the CAPI cluster cache (it already builds Talos clients per
public IP). Building a k8s client from the cluster kubeconfig secret is the
same pattern CAPI's cluster cache uses, but owned by the byot reconciler so it
survives `Cluster` deletion.

### D2 — chart: drop `helm.sh/resource-policy: keep` from `ByotMachine` and `Machine` for BYOT

Keep the annotation on the `Cluster`/`ByotCluster` (CAPI still owns cluster
teardown ordering), but **remove it from `ByotMachine` and `Machine`** so that:

- **PLA-6506:** removing a static-machine entry from values lets `helm upgrade`
  delete the now-untracked `Machine`/`ByotMachine` directly (Helm deletes
  resources no longer in the manifest), triggering D1's Node cleanup + the
  split reset via the byot finalizer.
- **PLA-6507:** on `helm uninstall`, Helm deletes `ByotMachine`/`Machine`
  directly (no `keep`), so D1 runs *before* the `Cluster` is torn down (Helm
  deletes in dependency order: machines before cluster), and the workload
  connection is still alive. CAPI still deletes the `Cluster` last.

Trade-off: without `keep`, a `helm uninstall` that races CAPI's own Machine
deletion could double-delete, but CAPI tolerates missing infra refs and the
byot finalizer is idempotent (Node already gone → no-op; reset already done →
no-op). The `keep` annotation was originally added to let CAPI own deletion,
but for BYOT static machines CAPI's deletion path is exactly what fails (D1),
so Helm-owned deletion with a byot finalizer is the correct owner.

### D3 — finalizer blocks until Node cleanup + split reset are attempted

The existing `byotMachineFinalizer` already blocks deletion until the split
reset succeeds (`splitPolicy: Reset`). Extend the deletion sequence to: Node
cleanup (D1, best-effort) → split reset (blocking for `Reset`, skipped for
`None`) → finalizer release. Node cleanup never blocks indefinitely (D1 step
2 non-fatal); the split reset still blocks for `Reset` (preserving the
guarantee that a `Reset` machine leaves wiped).

## API / CRD changes

None. No new spec fields; the deletion flow uses existing `spec.publicIP`,
`spec.providerID`, `Machine.status.nodeRef`, the `<cluster>-kubeconfig`
secret, and the existing `byotMachineFinalizer`. A new
`NodeCleanup` condition (`clusterv1.ConditionType`) is added to `ByotMachine`
status to surface drain/delete outcomes.

## Repos / change surface

- `cluster-api-provider-bringyourowntalos`:
  - `byotmachine_controller.go`: new `reconcileDelete` sequence
    (Node cleanup → split reset → finalizer), `cleanupWorkloadNode` helper
    (drain + delete via a kubeclient built from `<cluster>-kubeconfig`),
    `NodeCleanup` condition.
  - Unit tests for the deletion sequence (Node exists/absent, API
    unreachable, `Reset` vs `None`).
- `kommodity` (chart `kommodity-cluster`):
  - `templates/provider/byot/machines.yaml`: remove
    `helm.sh/resource-policy: keep` from the `ByotMachine` and `Machine`
    objects.
  - Helm unit tests: assert `keep` is absent on `ByotMachine`/`Machine`,
    present on `Cluster`/`ByotCluster`.

## Verification

1. **PLA-6507:** deploy a 2-node BYOT cluster (1 CP + 1 worker, Talos
   v1.13.8 on Scaleway); `helm uninstall`; assert workload `Node` objects are
   deleted (drained then gone) and, for `splitPolicy: Reset`, the VMs are wiped
   to maintenance mode.
2. **PLA-6506:** deploy the same cluster; remove the worker static-machine
   entry from values; `helm upgrade`; assert the worker
   `ByotMachine`/`Machine` are deleted, the workload `Node` is drained+deleted,
   and (for `Reset`) the worker VM is wiped.
3. **Regression:** `splitPolicy: None` deletion leaves the Talos config
   intact (machine re-adoptable) but the workload `Node` is removed; the CP
   Node cleanup is best-effort when the workload API is already gone (single-CP
   teardown), recorded as a `NodeCleanup` warning, and does not block
   deletion.

## Open questions

1. **Drain timeout + PDB handling:** default drain timeout (e.g. 5m) and
   whether to ignore `PodDisruptionBudget`-blocked evictions for forced
   teardown. Proposal: configurable via a future `spec.nodeDrainTimeout` with a
   sane default; for v1, a fixed timeout with `--ignore-daemonsets`,
   `--delete-emptydir-data`, and `--disable-eviction` fallback to force-delete
   on timeout.
2. **Worker-only teardown order:** when deleting the whole cluster, Helm
   deletes machines before the cluster, so the workload API (CP) is still up
   while workers are drained — good. But the CP `ByotMachine` deletion then
   drains the CP Node, which removes the API server mid-drain of any
   stragglers. Acceptable (the cluster is being destroyed); record a warning.
3. **Re-adopt of a `None`-split machine:** after D1 deletes its Node, a
   `None`-split machine keeps its Talos config + etcd; re-adopting it into the
   same cluster should restore the Node (the bundle matches). This is the
   existing re-adopt path, unchanged.
