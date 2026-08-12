# PRD: cluster-api-provider-bringyourowntalos (byot)

Ticket: [PLA-6443](https://linear.app/corti/issue/PLA-6443/create-new-infra-provider-to-adopt-talos-machine-based-on-byoh)

## Problem

Kommodity deploys sovereign Kubernetes clusters via Cluster API. Some target
environments (Gefion DC bare metal, pre-provisioned Scaleway instances) deliver
machines that already run Talos Linux in **maintenance mode**. No cloud API
owns these machines, so no existing infrastructure provider can manage them.

## Goal

A Cluster API infrastructure provider that **adopts** a Talos machine in
maintenance mode, identified by its public IP, by applying the
CABPT-generated machine configuration over the Talos machine API. After apply,
the machine installs with encrypted volumes (network KMS via Kommodity) and
joins the cluster.

Inspired by
[BYOH](https://github.com/vmware-tanzu/cluster-api-provider-bringyourownhost)
(unmaintained), minus the host agent: Talos in maintenance mode already exposes
everything needed over its API.

## Non-goals (v1)

- Configurable reset wipe scope: `splitPolicy: Reset` and `joinPolicy: Reset`
  always wipe both STATE and EPHEMERAL; a `spec.resetLabels` field may come
  later if demanded.
- Host pools / automatic claiming. Adoption is direct-reference only.
- Network provisioning, load balancers, or cluster-level reconciliation beyond
  reporting readiness.

## Decisions

| #   | Decision                                                 | Rationale                                                                                        |
| --- | -------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| 1   | Adopt pre-provisioned bare metal (Gefion DC)             | Machines provisioned outside CAPI; Kommodity owns config + encryption                            |
| 2 | Direct reference matching | `ByotMachine.spec.publicIP` set explicitly per machine; no agent, no pool |
| 7 | Authenticated adoption | `spec.talosConfigSecretRef` supplies the CURRENT talosconfig for foreign (already-configured) machines; machines we applied are reached via the cluster's own `<cluster>-talosconfig` secret |
| 8 | Config drift handling | `status.lastAppliedConfigSHA` tracks the applied machineconfig hash; changed bootstrap data is re-applied over mTLS |
| 3   | Machineconfig from CABPT bootstrap secret, applied as-is | Disk encryption (network KMS → Kommodity) configured in Talos templates, provider does not patch |
| 4   | Readiness = apply success                                | CAPI + Talos control plane provider already gate on node join; avoid duplicate node-watching     |
| 5   | Deletion honors `splitPolicy` (`Reset` wipes STATE+EPHEMERAL and blocks until the reset succeeds; `None` default releases the machine untouched) | Policies gate destructive actions; default is non-destructive so machines can be re-adopted losslessly |
| 6   | Library module, controllers run in-process in Kommodity  | Matches all-in-one Kommodity architecture (no standalone manager)                                |

## API (group `infrastructure.cluster.x-k8s.io/v1alpha1`)

- `ByotCluster`: `spec.controlPlaneEndpoint`; `status.ready=true` (no
  provisioning to wait for).
- `ByotMachine`: `spec.publicIP` (immutable identity); spec/status follow the
  CAPI v1beta1 infrastructure machine contract (`providerID`, `ready`,
  `addresses`, failure fields, conditions).
- `ByotMachineTemplate`: template wrapper of `ByotMachine` spec.

## Adoption reconcile flow (`ByotMachine`)

0. Deletion timestamp set → with `splitPolicy: Reset`, reset machine via
   `talosctl reset` (graceful=false, reboot=true, wipe STATE+EPHEMERAL), trying
   credentials in order: `spec.talosConfigSecretRef`, `<cluster>-talosconfig`,
   insecure (maintenance). Finalizer blocks deletion until a reset succeeds.
   With `splitPolicy: None` (default) the finalizer is released immediately and
   the machine keeps its configuration and datastore intact.
0bis. If `spec.joinPolicy: Reset` and not yet adopted: probe maintenance mode;
   if configured, issue authenticated reset (blocks adoption on failure), wait
   for maintenance, clear `status.lastAppliedConfigSHA`, then proceed.
0ter. Otherwise run the join preflight (see
   [PRD-join-split-policies.md](./PRD-join-split-policies.md)): maintenance mode
   or a bundle match proceeds to apply; a foreign bundle fails fast with
   `JoinPreflight=False`.
2. Ensure finalizer.
3. Resolve owner `Machine`; wait if missing.
4. Wait for `Machine.spec.bootstrap.dataSecretName`.
5. Read bootstrap secret (`value` key) → Talos machineconfig bytes; hash it.
6. If `status.ready` and hash unchanged → done.
7. Resolve auth:
   - Not ready + `spec.talosConfigSecretRef` set → mTLS with that talosconfig
     (foreign machine takeover).
   - Not ready + no secretRef → maintenance client, unverified TLS (machine in
     maintenance mode).
   - Ready + hash changed → mTLS with `<clusterName>-talosconfig` secret
     (re-application on our own machine).
8. Send `ApplyConfiguration` with mode `AUTO` to `<publicIP>:50000`.
9. On success: `providerID = byot://<publicIP>`, `status.addresses` from
   `publicIP`, `status.ready = true`, `status.lastAppliedConfigSHA` = hash.

Failure handling: transient errors requeue with backoff; bootstrap-data-missing
is a normal waiting state.

## Compatibility

- Cluster API **v1.10.x**, contract `v1beta1`.
- Talos machinery client **v1.13.x** (test target: Talos v1.13.8 on Scaleway,
  image `talos-scaleway-v1.13.8`, zone `prd-par02`).

## Test plan

1. Provision Scaleway instance from `talos-scaleway-v1.13.8` (sbx project,
   `prd-par02`), stays in maintenance mode with public IP.
2. Run Kommodity locally with `byot` enabled in
   `KOMMODITY_INFRASTRUCTURE_PROVIDERS`.
3. Apply Cluster + TalosControlPlane + Machine referencing a `ByotMachine`
   with the instance IP.
4. Verify: machine leaves maintenance mode, installs, encrypts STATE/EPHEMERAL,
   node joins.
