package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// HostPhase is the lifecycle phase of a ByotHost. The ByotHost controller
// drives Probing → Available → Unavailable ↔ Available and Releasing →
// Available; the ByotMachine controller drives Available → Claimed and
// Claimed → Releasing.
type HostPhase string

const (
	// HostPhaseProbing means the controller is running the initial discovery
	// against the maintenance API. A host starts here on creation.
	HostPhaseProbing HostPhase = "Probing"

	// HostPhaseAvailable means the host is maintenance-live and discovered,
	// so it is claimable by a ByotMachine.
	HostPhaseAvailable HostPhase = "Available"

	// HostPhaseClaimed means a ByotMachine has claimed the host. The host is
	// being adopted and is expected to leave maintenance mode; liveness
	// probing is paused.
	HostPhaseClaimed HostPhase = "Claimed"

	// HostPhaseReleasing means the owning ByotMachine was deleted and a
	// reset to maintenance was issued. The liveness loop flips this back to
	// Available once maintenance answers again.
	HostPhaseReleasing HostPhase = "Releasing"

	// HostPhaseUnavailable means maintenance liveness could not be confirmed
	// (dead or foreign-configured host). The operator fixes it out-of-band.
	HostPhaseUnavailable HostPhase = "Unavailable"
)

// HostClaimRef identifies the ByotMachine that has claimed a ByotHost. It is
// set by the ByotMachine controller via optimistic compare-and-swap on
// status.claimRef; a host finalizer blocks deletion while it is set.
type HostClaimRef struct {
	// Kind of the referent.
	Kind string `json:"kind"`
	// Name of the referent.
	Name string `json:"name"`
	// Namespace of the referent.
	Namespace string `json:"namespace"`
	// UID of the referent (the ByotMachine's UID).
	UID string `json:"uid"`
}

// HostCPU holds discovered CPU topology, parsed from the kernel dmesg log.
type HostCPU struct {
	// Cores is the total number of logical CPUs (nr_cpu_ids).
	Cores int32 `json:"cores"`
	// Packages is the number of physical CPU packages.
	Packages int32 `json:"packages"`
	// NumaNodes is the number of NUMA nodes.
	NumaNodes int32 `json:"numaNodes"`
}

// HostDisk holds a discovered disk from the Talos Disks RPC.
type HostDisk struct {
	// Name is the device path, e.g. "/dev/sda".
	Name string `json:"name"`
	// Size is the disk capacity as a resource.Quantity.
	Size resource.Quantity `json:"size"`
	// Type is the disk type: NVME, SSD, HDD, SD.
	Type string `json:"type"`
	// Model is the disk model string, when available.
	// +optional
	Model string `json:"model,omitempty"`
	// SystemDisk is true for the Talos system disk.
	// +optional
	SystemDisk bool `json:"systemDisk,omitempty"`
	// BusPath is the PCI/bus path, when available.
	// +optional
	BusPath string `json:"busPath,omitempty"`
}

// HostIdentity holds the reboot-stable hardware identity of a host, used to
// verify that the machine answering at spec.publicIP after a reboot (upgrade,
// reset, power cycle) is the same physical board the host record was created
// for. The primary identifier is the SMBIOS System Information UUID (firmware-
// burned, OS-independent); the first-up NIC hardware address is a secondary
// cross-check. Both are fetched over the Talos maintenance API (unverified
// TLS) and so are only as trustworthy as the network path to the host.
type HostIdentity struct {
	// SystemUUID is the SMBIOS Type 1 UUID (firmware-burned, survives Talos
	// upgrades and reinstalls). Empty when the firmware does not expose one.
	// +optional
	SystemUUID string `json:"systemUUID,omitempty"`
	// SerialNumber is the SMBIOS Type 1 board serial number, kept as an
	// additional cross-check. Empty when unavailable.
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`
	// Manufacturer is the SMBIOS Type 1 manufacturer string. Empty when
	// unavailable.
	// +optional
	Manufacturer string `json:"manufacturer,omitempty"`
	// ProductName is the SMBIOS Type 1 product name. Empty when unavailable.
	// +optional
	ProductName string `json:"productName,omitempty"`
	// HardwareAddr is the colon-separated MAC of the first-up physical NIC
	// (the address Talos uses for machine identity), from the network.HardwareAddr
	// resource. Spoofable from userspace and zero on some virtual NICs, so it is
	// a cross-check rather than a primary identity.
	// +optional
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}

// HostHardware holds the discovered hardware features of a host.
type HostHardware struct {
	// CPU is the discovered CPU topology.
	CPU HostCPU `json:"cpu"`
	// Memory is the total memory as a resource.Quantity (from Meminfo.Memtotal).
	Memory resource.Quantity `json:"memory"`
	// Disks is the discovered disk inventory.
	// +optional
	Disks []HostDisk `json:"disks,omitempty"`
	// NetworkInterfaces lists interface names from /sys/class/net. Per-
	// interface MACs are not collected here; the first-up NIC MAC used for
	// machine identity is in status.identity.hardwareAddr.
	// +optional
	NetworkInterfaces []string `json:"networkInterfaces,omitempty"`
	// GPUs is the discovered GPU summary (count, vendor, model, memory).
	// Nil when no GPU is present. Mixed models collapse to count+vendor only.
	// +optional
	GPUs *HostGPU `json:"gpus,omitempty"`
}

// HostGPU is the aggregated GPU summary discovered from the PCI bus via the
// kernel dmesg log. Nodes are assumed homogeneous; a mixed-GPU host keeps
// count/vendor but omits model and sets Mixed=true.
type HostGPU struct {
	// Vendor is the lowercase GPU vendor (nvidia, amd, intel). Omitted when
	// the host has GPUs from more than one vendor.
	// +optional
	Vendor string `json:"vendor,omitempty"`
	// Model is the normalized model family (e.g. "h100-pcie", "b300-sxm6"),
	// from the PCI device-id table. Empty for unknown devices or mixed hosts.
	// +optional
	Model string `json:"model,omitempty"`
	// DeviceID is the raw PCI device id (e.g. "2331") for triage of unknown
	// devices. Empty for mixed hosts (no single id to report).
	// +optional
	DeviceID string `json:"deviceId,omitempty"`
	// MemoryPerGPU is the per-GPU HBM as a resource.Quantity, derived from the
	// model table (not measured). Zero when the model is unknown or mixed.
	MemoryPerGPU resource.Quantity `json:"memoryPerGPU"`
	// Count is the number of discovered GPUs (display-class PCI devices from
	// a known vendor).
	Count int32 `json:"count"`
	// TotalMemory is Count times MemoryPerGPU. Zero when the model is unknown
	// or mixed.
	TotalMemory resource.Quantity `json:"totalMemory"`
	// Mixed is true when the host has GPUs of more than one vendor:device pair.
	// Model and DeviceID are empty in that case.
	// +optional
	Mixed bool `json:"mixed,omitempty"`
}

// ByotHostSpec defines the desired state of ByotHost. Operators add a host as
// an IP-only record; the controller discovers features and probes liveness.
type ByotHostSpec struct {
	// PublicIP is the immutable identity of the host. It must be reachable on
	// the Talos machine API port (50000) in maintenance mode.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="publicIP is immutable"
	PublicIP string `json:"publicIP"`

	// FailureDomain is the operator-set physical failure domain, promoted to
	// the byot.io/failure-domain label so claim selectors can match it.
	// +optional
	FailureDomain *string `json:"failureDomain,omitempty"`
}

// ByotHostStatus defines the observed state of ByotHost.
type ByotHostStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase HostPhase `json:"phase,omitempty"`

	// TalosVersion is the discovered Talos version (from the Version RPC).
	// +optional
	TalosVersion string `json:"talosVersion,omitempty"`

	// Arch is the discovered architecture (from the Version RPC).
	// +optional
	Arch string `json:"arch,omitempty"`

	// Platform is the discovered Talos platform (from the kernel cmdline
	// talos.platform= in dmesg).
	// +optional
	Platform string `json:"platform,omitempty"`

	// Hardware holds the discovered hardware features (view-only).
	// +optional
	Hardware *HostHardware `json:"hardware,omitempty"`

	// Identity holds the reboot-stable hardware identity (SMBIOS UUID + first
	// NIC MAC) used to verify the host is the same physical board across
	// reboots/upgrades. Populated by discovery; empty when the firmware or NIC
	// exposes nothing usable.
	// +optional
	Identity *HostIdentity `json:"identity,omitempty"`

	// MaintenanceMode is the last maintenance-liveness probe result.
	// Absent (the default before the first probe completes) is equivalent to
	// false: the host is not confirmed in maintenance mode.
	// +optional
	MaintenanceMode bool `json:"maintenanceMode,omitempty"`

	// ClaimRef is set when a ByotMachine has claimed this host.
	// +optional
	ClaimRef *HostClaimRef `json:"claimRef,omitempty"`

	// ProbeFailureCount is the number of consecutive maintenance probe
	// failures. Reset on success.
	// +optional
	ProbeFailureCount int32 `json:"probeFailureCount,omitempty"`

	// LastProbedAt records when the host was last probed.
	// +optional
	LastProbedAt *metav1.Time `json:"lastProbedAt,omitempty"`

	// Conditions defines current service state of the ByotHost.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// ByotHost condition types.
const (
	// HostDiscoveredCondition is True once discovery has populated the
	// hardware/version/platform status fields.
	HostDiscoveredCondition clusterv1.ConditionType = "Discovered"

	// HostMaintenanceProbeCondition reports the maintenance liveness probe.
	// False with MaintenanceProbeFailed when the host stops answering the
	// maintenance API.
	HostMaintenanceProbeCondition clusterv1.ConditionType = "MaintenanceProbe"

	// HostDiscoveryFailedCondition reports a discovery failure.
	HostDiscoveryFailedCondition clusterv1.ConditionType = "DiscoveryFailed"
)

// ByotHost condition reasons.
const (
	// HostDiscoveredReasonMixedGPUModels is set on HostDiscoveredCondition when
	// discovery found GPUs of more than one vendor:device pair. The host stays
	// non-Available so a ByotMachine cannot claim a mislabeled mixed node.
	HostDiscoveredReasonMixedGPUModels = "MixedGPUModels"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Host phase"
// +kubebuilder:printcolumn:name="PublicIP",type="string",JSONPath=".spec.publicIP",description="Public IP of the host"
// +kubebuilder:printcolumn:name="Talos",type="string",JSONPath=".status.talosVersion",description="Discovered Talos version"
// +kubebuilder:printcolumn:name="Arch",type="string",JSONPath=".status.arch",description="Discovered architecture"
// +kubebuilder:printcolumn:name="Claimed",type="string",JSONPath=".status.claimRef.name",description="ByotMachine claiming the host"
// +kubebuilder:printcolumn:name="GPUs",type="string",JSONPath=".status.hardware.gpus.model",description="Discovered GPU model family"
// +kubebuilder:printcolumn:name="FailureDomain",type="string",JSONPath=".spec.failureDomain",description="Failure domain"
// +kubebuilder:resource:shortName=byothost
// +kubebuilder:resource:shortName=byothosts

// ByotHost is the Schema for the byothosts API. It is a manually-added,
// IP-only record of a Talos host sitting in maintenance mode. The controller
// discovers hardware features from the Talos maintenance API and probes
// liveness periodically; ByotMachine objects claim hosts from the registry.
type ByotHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ByotHostSpec   `json:"spec,omitempty"`
	Status ByotHostStatus `json:"status,omitempty"`
}

// GetConditions returns the observations of the operational state of the ByotHost resource.
func (h *ByotHost) GetConditions() clusterv1.Conditions {
	return h.Status.Conditions
}

// SetConditions sets the underlying service state of the ByotHost to the predescribed clusterv1.Conditions.
func (h *ByotHost) SetConditions(conditions clusterv1.Conditions) {
	h.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ByotHostList contains a list of ByotHost.
type ByotHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ByotHost `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ByotHost{}, &ByotHostList{})
}
