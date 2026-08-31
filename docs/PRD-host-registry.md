# PRD: BYOT host registry (discovery + claim/release)

Ticket: [PLA-6443](https://linear.app/corti/issue/PLA-6443/create-new-infra-provider-to-adopt-talos-machine-based-on-byoh)
follow-up (host pools were an explicit v1 non-goal in `docs/PRD.md`; this design
keeps the registry but drops the pool/parking layer — see Alternatives below).

## Problem

BYOT adopts pre-provisioned Talos machines by public IP. Today every
`ByotMachine` references one box directly via `spec.publicIP`, adopted on
demand. Two gaps:

1. **No host registry.** There is no record of available hosts, their
   hardware, or their Talos version. A `MachineDeployment` scale-out has no
   inventory to claim from; operators have no view of what is adoptable.
2. **No discovery.** Nothing probes a host before adoption to learn its disk
   layout, CPU, memory, or Talos version — information that would let claims
   select hosts by capability and let operators see what they have.

## Goal

A lightweight `ByotHost` registry: operators add a host as an IP-only record,
the controller discovers the host's features from the Talos maintenance API and
probes liveness periodically, and `ByotMachine` objects claim hosts from the
registry. Released hosts are reset to maintenance and return to the registry
for re-claim.

Hosts sit in **maintenance mode**, protected by a firewall (not authenticated
standby). Every claim is a full install from maintenance; there is no fast-join
or parked-config path.

## Decisions

| #   | Decision                                                                                                                                                                                                                                                                                          | Rationale                                                                                                                                                                                                                                |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | `ByotHost` is a manually-added, IP-only record; controller discovers features and probes liveness                                                                                                                                                                                                 | Operator owns host lifecycle; byot owns observation. No host auto-discovery/scanning                                                                                                                                                     |
| 2   | Hosts sit in maintenance mode, firewall-protected (not authenticated standby)                                                                                                                                                                                                                     | No standby bundle, no park config, no "configured but not joined" state. Simplest secure wait state                                                                                                                                      |
| 3   | Controller discovers features via the Talos maintenance API (`Version`, `Memory`, `Disks`, `Dmesg`, `LS`) using the existing insecure client                                                                                                                                                      | Same maintenance client byot already builds (`maintenanceClient`); no new Talos plumbing. `Read` is restricted in maintenance mode, so CPU comes from `Dmesg` parsing and interface names from `LS /sys/class/net`                       |
| 4   | Controller probes liveness periodically (~60s) and marks hosts `Unavailable` after consecutive failures; re-discovers features on recovery (`Unavailable` → `Available`)                                                                                                                          | Catches dead hosts before claim; keeps features fresh after outages                                                                                                                                                                      |
| 5   | A host is `Available` (claimable) only when maintenance-liveness is confirmed **and** features are discovered                                                                                                                                                                                     | Uniform claim gate; no claiming an undiscovered host, even via explicit `hostRef`                                                                                                                                                        |
| 6   | `ByotMachine` always claims a `ByotHost`; no direct `spec.publicIP` path                                                                                                                                                                                                                          | ByotHost is the single adoption unit; one code path; discovery always runs                                                                                                                                                               |
| 7   | Claim by `hostRef` (explicit) or `hostSelector` (labels + `failureDomain`) against `Available` hosts; `ByotHost.status.claimRef` optimistic CAS, host finalizer blocks delete while claimed                                                                                                       | Race-protected against concurrent scale-out                                                                                                                                                                                              |
| 8   | Selector claims are pure Kubernetes label selectors over `ByotHost` `metadata.labels`; conventionally include `byot.io/available: "true"` plus capability labels                                                                                                                                  | Standard k8s selection; no custom status-field selector. Controller lists `Available` hosts matching the selector and claims via `claimRef` CAS                                                                                          |
| 9   | Discovered features are exposed two ways: rich typed `status` (view-only) and a curated, low-cardinality, bucketed subset promoted to `metadata.labels` (prefixed `byot.io/`, controller-managed)                                                                                                 | Labels are the only thing label selectors match; promoting a subset enables capability-based claims without operator label bookkeeping. Hardware is fixed → no label churn                                                               |
| 10  | Promoted labels: `byot.io/available`, `cpu-cores`, `cpu-arch`, `memory-class` (4G/8G/16G/32G/64G/128G), `disk-type`, `disk-class` (20G/100G/250G/500G/1T), `platform`, `talos-version`, `failure-domain` (from `spec.failureDomain`). High-cardinality / non-selection fields stay in status only | Bucketed values stay stable and meaningful for selection; raw bytes/serials/bus paths are not selection-relevant. `failure-domain` is promoted so claim selectors can match the owning Machine's failureDomain for spread (see PLA-6629) |
| 11  | `byot.io/available: "true"` is a derived index; `status.phase` is the source of truth. Controller claims only `phase=Available` hosts even if a label race leaves a stale label                                                                                                                   | Label is an optimization for cheap label-selector filtering; status gates correctness                                                                                                                                                    |
| 12  | Release always resets (STATE+EPHEMERAL) → maintenance → `Available`                                                                                                                                                                                                                               | No `splitPolicy` choice; every release returns a clean, re-claimable maintenance host. Destructive by design                                                                                                                             |
| 13  | Release reset reuses `resetWithResolvedAuth` candidate order (cluster talosconfig, then insecure maintenance); reset is async, the liveness probe loop flips `Releasing` → `Available` when maintenance answers                                                                                   | No separate reset-confirmation gate; reuses the probe controller                                                                                                                                                                         |
| 14  | ByotHost is maintenance-only; a non-maintenance host (dead or foreign-configured) is `Unavailable` / `MaintenanceProbeFailed`; operator fixes out-of-band                                                                                                                                         | No foreign-takeover at the host layer; byot stays dumb about non-maintenance states                                                                                                                                                      |
| 15  | ByotHost deletion blocked by a finalizer while `claimRef` set; delete the owning `ByotMachine` first to release                                                                                                                                                                                   | No losing a claimed host to admin delete                                                                                                                                                                                                 |

## Non-goals

- Host pools / a pool CRD / a standby bundle / park config / fast-join. (See
  Alternatives for the path considered and rejected.)
- Authenticated standby (parked with a bundle). Hosts wait in maintenance,
  firewall-protected.
- Foreign-host takeover at the `ByotHost` layer. A host must be in maintenance
  to be `Available`; foreign-configured hosts are an operator error fixed
  out-of-band.
- Host auto-discovery / scanning. Hosts are added explicitly by IP.
- Chart-managed `ByotHost` objects. Operators add them manually.

## Alternatives considered

**Parked standby with a shared bundle (rejected).** An earlier design parked
hosts with the cluster's own bundle pointed at a dead endpoint (`169.25.0.1`)
so kubelet never registered — an authenticated, idle, not-joined state. Worker
claims would have been lossless (same bundle → `BundleMatch` preflight →
endpoint flip → fast join). Rejected because:

- It required a pool CRD owning a standby bundle + a park config rendered
  in-process from the cluster bundle (a philosophy shift away from byot's
  apply-only stance, PRD decision 3).
- Control-plane claims were never lossless (worker→CP type change + etcd
  membership is not endpoint-flippable), so the fast-join win was workers-only.
- Network KMS had to be reachable from parked public IPs for encryption to
  carry over losslessly.

The maintenance-registry design drops all of this: no pool, no park config, no
standby bundle, no fast-join, no in-process config rendering. Every claim is a
full install. Simpler, and consistent with byot's apply-only philosophy.

## API (group `infrastructure.cluster.x-k8s.io/v1alpha1`)

### ByotHost

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: ByotHost
metadata:
  name: host-7
  finalizers: [byot-host-protection]
  labels: # controller-managed discovery labels (see Exposure)
    byot.io/available: "true"
    byot.io/cpu-cores: "3"
    byot.io/cpu-arch: "amd64"
    byot.io/memory-class: "4G"
    byot.io/disk-type: "hdd"
    byot.io/disk-class: "20G"
    byot.io/platform: "scaleway"
    byot.io/talos-version: "v1.13.8"
    site: copenhagen # freeform operator labels, also matchable
spec:
  publicIP: "203.0.113.10" # immutable identity
  failureDomain: "par01" # first-class, matched by ByotMachine
status:
  phase: Probing | Available | Claimed | Releasing | Unavailable
  talosVersion: "v1.13.8" # discovered via Version RPC
  arch: amd64 # discovered via Version RPC
  platform: scaleway # discovered from Dmesg kernel cmdline (talos.platform=)
  hardware: # discovered — typed, detailed (view-only)
    cpu: # parsed from Dmesg (nr_cpu_ids / Num. cores per package)
      cores: 3
      packages: 1
      numaNodes: 1
    memory: "3981564Ki" # via Memory RPC (resource.Quantity: Meminfo.Memtotal)
    disks: # via Disks RPC
      - name: "/dev/sda"
        size: "20Gi" # resource.Quantity (bytes)
        type: "HDD"
        model: "sbs"
        systemDisk: true
        busPath: "/pci0000:00/0000:00:03.0/virtio1/host2/target2:0:0/2:0:0:0"
    networkInterfaces: # via LS /sys/class/net (names only; MAC unavailable)
      - eth0
      - bond0
  maintenanceMode: true # last probe result
  claimRef: # set when a ByotMachine claims this host
    kind: ByotMachine
    name: worker-md-xxx-abc
    namespace: default
    uid: ...
  lastProbedAt: "2025-..."
  conditions: [] # Discovered condition (True once populated; MaintenanceProbeFailed / DiscoveryFailed on failure)
```

### ByotMachine (modified)

```yaml
spec:
  hostRef: { name: host-7 } # explicit claim (one of hostRef/hostSelector)
  hostSelector: # selector claim — label-based (includes availability + capability labels)
    matchLabels:
      byot.io/available: "true"
      byot.io/disk-type: "ssd"
      byot.io/memory-class: "64G"
      site: copenhagen
  failureDomain: "par01" # matched against ByotHost.spec.failureDomain
  # spec.publicIP REMOVED — IP resolved from claimed ByotHost
  # joinPolicy, splitPolicy, talosConfigSecretRef REMOVED (Decisions 6/12;
  # see the ByotMachine spec table). Release always resets the host to
  # maintenance regardless — see Release.
```

`hostRef` and `hostSelector` are mutually exclusive (CEL validation). The
controller resolves the public IP from the claimed `ByotHost` into
`status.resolvedPublicIP` and uses it as the adoption target. `spec.publicIP`
is removed; there is no direct-IP adoption path.

## Exposure: status vs labels

Discovery's value is enabling capability-based claims without operator label
bookkeeping. Kubernetes label selectors only match `metadata.labels`, never
`status`, so discovered features must reach labels to be selectable. Hardware
is fixed for the life of a host, so promoted labels do not churn (they change
only on Talos upgrade or re-discovery after an outage).

**Status = typed, detailed, view-only.** The controller writes rich typed
fields under `status`:

- `status.talosVersion`, `status.arch`, `status.platform`: string.
- `status.hardware.cpu`: `{ cores int32, packages int32, numaNodes int32 }`
  — parsed from `Dmesg` (`nr_cpu_ids` / `Num. cores per package`).
- `status.hardware.memory`: `resource.Quantity` (idiomatic for RAM; from
  `Memory` RPC `Meminfo.Memtotal`).
- `status.hardware.disks[]`: `{ name, size resource.Quantity, type, model,
systemDisk bool, busPath string }` — from the `Disks` RPC.
- `status.hardware.networkInterfaces[]`: string names (from `LS
/sys/class/net`; MACs are unavailable because `Read` is restricted in
  maintenance mode).
- `status.conditions`: a `Discovered` condition (True once populated;
  `MaintenanceProbeFailed` / `DiscoveryFailed` on failure).

**Labels = curated, low-cardinality, selection-oriented.** The controller
promotes a fixed subset to `metadata.labels` (prefixed `byot.io/`) and keeps
them in sync with status. Bucketed, not raw, so values stay stable and
meaningful for selection:

| Label                    | Source               | Values                                                                                                |
| ------------------------ | -------------------- | ----------------------------------------------------------------------------------------------------- |
| `byot.io/available`      | phase                | `"true"` only when `phase=Available`; absent otherwise (claim/filter gate)                            |
| `byot.io/cpu-cores`      | `hardware.cpu.cores` | integer as string (e.g. `"3"`, `"16"`)                                                                |
| `byot.io/cpu-arch`       | `Version` arch       | `amd64` / `arm64`                                                                                     |
| `byot.io/memory-class`   | `hardware.memory`    | bucketed: `4G`, `8G`, `16G`, `32G`, `64G`, `128G`                                                     |
| `byot.io/disk-type`      | system disk's `type` | `nvme`, `ssd`, `hdd`, `sd`                                                                            |
| `byot.io/disk-class`     | system disk's `size` | bucketed: `20G`, `100G`, `250G`, `500G`, `1T`                                                         |
| `byot.io/platform`       | `platform`           | `scaleway`, `copenhagen`, ...                                                                         |
| `byot.io/talos-version`  | `talosVersion`       | `v1.13.8`, ...                                                                                        |
| `byot.io/failure-domain` | `spec.failureDomain` | operator-set physical FD, e.g. `par01`, `par02` (matched by claim selector for spread — see PLA-6629) |

NOT promoted (high cardinality or not selection-relevant; remain in status
only): disk model, serial, uuid, wwid, bus path, MAC, exact memory bytes,
non-system disks, TSC MHz, NUMA detail. Operators may add their own freeform
labels (e.g. `site: copenhagen`); the controller never overwrites operator labels.

Selector claims are pure label selectors and conventionally include
`byot.io/available: "true"` plus any capability labels. The controller lists
`Available` hosts matching the selector and claims one via `claimRef` CAS. A
`byot.io/available` label is a derived index; `status.phase` remains the source
of truth (the controller claims only `phase=Available` hosts even if a label
race leaves a stale `available: true`).

## Reconcile flows

### ByotHost controller (discovery + liveness)

1. On creation: the host starts with an empty `phase` (treated as `Probing`)
   and the controller runs initial discovery immediately. Build the maintenance
   client (`maintenanceClient`, insecure) against `spec.publicIP` and call the
   confirmed maintenance-mode surface: `Version` (Talos version + arch),
   `Memory` (RAM total), `Disks` (disk inventory), `Dmesg` (CPU cores/packages/NUMA via parsing `nr_cpu_ids` / `Num. cores per package`; platform via `talos.platform=` kernel cmdline), and `LS /sys/class/net` (interface names). `Read` is restricted in maintenance mode and returns empty; it is not used.
   On success populate `status.talosVersion`, `status.arch`, `status.platform`, `status.hardware` (cpu, memory, disks, networkInterfaces), `status.maintenanceMode=true`, set `phase=Available`. On failure set `phase=Probing` and requeue.
2. Periodically (~60s): probe maintenance liveness (`probeMaintenance`). On
   consecutive failures (e.g. 3) set `phase=Unavailable`,
   `status.maintenanceMode=false`, condition `MaintenanceProbeFailed`. On
   recovery (`Unavailable` → probe succeeds) re-discover features and set
   `phase=Available`.
3. A non-maintenance host (dead or foreign-configured) is `Unavailable` with
   reason `MaintenanceProbeFailed`. No foreign-takeover; operator fixes
   out-of-band.

### Claim (ByotMachine reconcile)

1. Resolve a target `ByotHost`:
   - `hostRef` set → that named host. Must be `phase=Available` (discovered +
     maintenance-live); else requeue.
   - `hostSelector` set → list `Available` hosts matching `matchLabels` +
     `failureDomain`; claim the first via optimistic CAS on
     `status.claimRef` (first writer wins; losers requeue and retry). A
     `ByotHost` with no `spec.failureDomain` is domain-agnostic and matches
     any ByotMachine (spread is only enforced for tagged hosts);
     `failureDomain` on `ByotHost` is optional.
2. Set `ByotHost.status.claimRef` to this ByotMachine; the host finalizer
   blocks `ByotHost` deletion while claimed. Set `ByotHost.phase=Claimed`.
3. Resolve `publicIP` from the claimed `ByotHost` into
   `status.resolvedPublicIP`.
4. Run the existing adopt flow (`adopt`, `preflightJoin`,
   `applyAndMarkAdopted`) against that IP. The host is in maintenance, so
   `preflightJoin` takes the maintenance-mode branch (insecure client, no
   bundle check) and applies the bootstrap config. Worker and CP claims are
   the same full-install path; the config applied is whatever CABPT generated
   for the owning `Machine`.
5. On success set `ByotMachine.status.ready=true`,
   `providerID=byot://<ip>`, addresses from the IP.

### Release (ByotMachine deletion)

1. `reconcileDelete` issues a reset (STATE+EPHEMERAL, reboot to maintenance)
   via `resetWithResolvedAuth` (cluster talosconfig, then insecure
   maintenance). Release always resets — there is no `splitPolicy: None`
   "leave running" path for claimed hosts; the host returns to maintenance
   for re-claim.
2. Set `ByotHost.phase=Releasing`. The reset is async (host reboots); the
   liveness probe loop flips `Releasing` → `Available` when maintenance
   answers again. No separate reset-confirmation gate.
3. Clear `ByotHost.status.claimRef` once the reset is issued. The host is
   re-claimable as soon as the liveness loop marks it `Available`.

### ByotHost deletion

A finalizer (`byot-host-protection`) blocks deletion while `claimRef` is set.
Delete the owning `ByotMachine` first (which releases + clears `claimRef`),
then the `ByotHost` deletes.

## Repos / change surface

- `cluster-api-provider-bringyourowntalos`:
  - New CRD: `ByotHost` (IP-only spec, discovered status, `claimRef`,
    finalizer).
  - New `ByotHost` controller: maintenance-mode discovery (`Version`, `Memory`, `Disks`, `Dmesg` parsing, `LS /sys/class/net`) + periodic liveness probe + promotion of curated bucketed labels. Reuses `maintenanceClient`.
  - `ByotMachine` controller: claim flow (`hostRef`/`hostSelector`), IP
    resolution from `ByotHost`, `claimRef` CAS, release-always-reset,
    `Releasing` → `Available` via the liveness loop. Remove `spec.publicIP`
    and the direct-IP adoption path.
  - Tests: discovery populates status + promotes labels; liveness marks `Unavailable` then
    recovers; claim race (two ByotMachines, one host, one winner); claim
    against `Probing`/`Unavailable` host requeues; release resets and returns
    to `Available`; deletion blocked while claimed; operator labels preserved
    across re-discovery.
- `kommodity` (chart): `ByotHost` CRD template (operators create instances
  manually, not chart-managed); regenerate byot CRDs under
  `pkg/provider/crds/byot/`. No pool templates.

## Verification

- Unit: discovery populates `status.hardware`/`talosVersion`/`arch`/`platform` from a mocked
  maintenance API (`Version`, `Memory`, `Disks`, `Dmesg`, `LS`); CPU cores parsed from `Dmesg`;
  curated bucketed labels promoted and kept in sync; `byot.io/available` flipped on phase
  transitions; liveness flips `Available` → `Unavailable` on probe failure
  and back with re-discovery; claim race resolved by `claimRef` CAS; claim
  against a non-`Available` host requeues; release resets (mocked) and the
  liveness loop returns the host to `Available`; `ByotHost` deletion blocked
  while `claimRef` set.
- Manual: add a `ByotHost` (IP only) on a firewall-protected maintenance-mode
  box; observe discovery populates status; scale a `MachineDeployment` →
  claim → full install → node joins; delete the `Machine` → host resets to
  maintenance → `Available` again; re-claim; delete a claimed `ByotHost` →
  blocked until the `ByotMachine` is deleted.
