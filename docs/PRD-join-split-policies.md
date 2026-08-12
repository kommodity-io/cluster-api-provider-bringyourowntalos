# PRD: BYOT Join/Split policies + bundle preflight

Tickets: [PLA-6475](https://linear.app/corti/issue/PLA-6475/add-policies-for-join-and-split), [PLA-6479](https://linear.app/corti/issue/PLA-6479/byot-adoption-of-nodes-with-retained-etcd-fails-ca-encryption-at-rest)

## Problem

BYOT adoption (`PLA-6443`) has two lifecycle holes:

1. **No declarative stance on destructive operations.** On delete the controller
   unconditionally resets machines, and adoption of a booted machine is possible
   only via `spec.forceReset`. There is no way to express "remove from management
   but leave the node running" or "adopt only if untouched".
2. **Silent PKI/encryption mismatch (PLA-6479).** Adopting a control-plane node
   that retains a *different* cluster's etcd into a fresh-pki cluster produces a
   node that is etcd-healthy but permanently `NotReady`: the API server cannot
   decrypt retained secrets (old secretbox key absent) and rejects kubelet certs
   (old signer CA ≠ new client CA). Nothing fails fast.

## What secrets.yaml is (context)

Talos **secrets bundle** (`talosctl gen secrets` output): single source of the
entire cluster PKI — k8s CA (crt+key), etcd CA, front-proxy/aggregator CA,
service-account signing key, bootstrap token, and
`secretboxEncryptionSecret` (API server encryption-at-rest key). Machineconfig
derives from it; etcd data can only be read by the bundle that produced its
secretbox key.

Consequence: a node retaining STATE is bound to one bundle. It can only:
- rejoin a cluster using **the same** bundle (no-wipe, lossless), or
- be wiped and join a cluster using **any other** bundle (destructive).

There is no bridge: adopting without wipe into a mismatched cluster is the
permanent-broken state of PLA-6479.

## Requirements

### R1 — Join/Split policies on `ByotMachine`

- New spec fields `joinPolicy`, `splitPolicy`: enum `None` | `Reset`, default `None`
  (CEL-validated on the CRD).
- `spec.forceReset` is **removed**; its behavior is subsumed by `joinPolicy: Reset`.
- Precedence: staticMachine > nodepool/controlPlane > default `None`
  (precedence realized in the kommodity-cluster chart, R4).

### R2 — Split semantics

- `splitPolicy: None` — delete the Machine CR; no talos reset. The node still
  leaves the cluster roster: core Cluster API's Machine deletion flow drains
  the workload-cluster Node and deletes the Node object before the infra
  resource is released (provider-independent). The byot provider only skips
  the Talos wipe, so the machine keeps its machineconfig and datastore and
  can be re-adopted without changes. Management-cluster secrets (bundle,
  talosconfig) retained (bundle is cluster-scoped, outlives machines).
- `splitPolicy: Reset` — wipe EPHEMERAL+STATE via talos reset (reboot to
  maintenance mode), then delete. Reuses the existing delete-reset path.
- Defaulting to `None` **changes current behavior** (delete currently always
  resets). Accepted: byot provider is unreleased.

### R3 — Join semantics with bundle preflight

Cluster bundle is immutable and cluster-scoped for cluster lifetime
(`<cluster>-talos` secret; never regenerated per join).

On adopt, probe in order:
1. Maintenance-mode endpoint open → node freshly reset/wiped → fresh-join path
   (no verification needed).
2. Authenticated API reachable (talosconfig from stored bundle) and running
   config's k8s CA SHA1 **matches** bundle → no-change adopt (restore as-is).
3. CA **mismatch** → fail fast: set Machine condition `JoinPreflightFailed`
   with a message naming the two CA fingerprints and instructing to set
   `joinPolicy: Reset`; emit warning event. No config is applied.

There is deliberately **no** cluster-conforms-to-node path (no
secrets-bundle injection into the new cluster). Preserving foreign cluster
data is out of product scope; manual remediation per PLA-6479 option A.

### R4 — Chart (kommodity-cluster)

- `controlplane.joinPolicy` / `controlplane.splitPolicy`, same pair per nodepool,
  per-staticMachine override. Chart resolves precedence and stamps the CRDs.
- Remove the `forceReset` value key (unreleased branch).

## Non-goals

- Rotating/re-issuing the cluster bundle.
- Adopting foreign retained data (no-wipe path into a different bundle).
  Manual dual-key EncryptionConfiguration migration is a documented runbook
  follow-up, not a product path: [PLA-6483](https://linear.app/corti/issue/PLA-6483).
- Changing behavior of the CAPI (Scaleway etc.) providers.

## Repos / change surface

- `cluster-api-provider-bringyourowntalos`: CRD (`ByotMachine(Template)` spec),
  machine reconciler (preflight, split policy), remove `forceReset`; tests.
- `kommodity` (branch `feat/PLA-6443-byot-provider`): chart values + templates +
  helm unit tests; regenerate byot CRDs under `pkg/provider/crds/byot/`.

## Verification

- Provider unit tests: preflight match / mismatch / maintenance-mode cases.
- Helm unit tests: precedence resolution (staticMachine > pool/CP > None).
- Manual: round-trip Split=None → Join on `paul-test`-style cluster; join
  mismatched node → explicit failure; `joinPolicy: Reset` → wipe + adopt.
