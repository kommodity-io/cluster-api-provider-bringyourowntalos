package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ProviderIDPrefix is the scheme prefix of provider IDs assigned to adopted machines.
const ProviderIDPrefix = "byot://"

// LocalObjectReference contains enough information to locate a resource in the same namespace.
type LocalObjectReference struct {
	// Name of the referent.
	Name string `json:"name"`
}

// ByotMachineSpec defines the desired state of ByotMachine.
//
// A ByotMachine claims a ByotHost from the registry and adopts the Talos
// machine at the host's public IP. Either hostRef (explicit) or hostSelector
// (label-based) selects the host; they are mutually exclusive.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.hostRef) && has(self.hostSelector))",message="hostRef and hostSelector are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.hostRef) || has(self.hostSelector)",message="one of hostRef or hostSelector is required"
type ByotMachineSpec struct {
	// HostRef names the ByotHost to claim explicitly. The host must be
	// Available (maintenance-live and discovered). Mutually exclusive with
	// hostSelector.
	// +optional
	HostRef *LocalObjectReference `json:"hostRef,omitempty"`

	// HostSelector selects a ByotHost to claim by label selector. The
	// controller lists Available hosts matching the selector (plus the
	// failureDomain, when set) and claims the first via claimRef CAS.
	// Mutually exclusive with hostRef.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`

	// TalosConfigSecretRef references a Secret holding a talosconfig (YAML
	// under the "talosconfig" key) for the machine's CURRENT configuration.
	// Use it to adopt a machine that is already booted and configured (e.g.
	// foreign cluster we are taking over). Machines without a secretRef are
	// assumed to be in maintenance mode on first contact; after adoption the
	// cluster's own talosconfig secret is used.
	// +optional
	TalosConfigSecretRef *LocalObjectReference `json:"talosConfigSecretRef,omitempty"`

	// ProviderID is set by the controller once the machine has been adopted.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// FailureDomain is the unique name of the failure domain the machine is
	// placed in. When claiming via hostSelector, it is matched against the
	// ByotHost's spec.failureDomain.
	// +optional
	FailureDomain *string `json:"failureDomain,omitempty"`
}

// ByotMachineStatus defines the observed state of ByotMachine.
type ByotMachineStatus struct {
	// Ready denotes that the machine has been adopted: the machine
	// configuration was applied successfully over the Talos maintenance API.
	// +optional
	Ready bool `json:"ready"`

	// ResolvedHost is the name of the ByotHost this machine has claimed. The
	// public IP for adoption is read from that host.
	// +optional
	ResolvedHost string `json:"resolvedHost,omitempty"`

	// ResolvedPublicIP is the public IP of the claimed ByotHost, used as the
	// adoption target.
	// +optional
	ResolvedPublicIP string `json:"resolvedPublicIP,omitempty"`

	// LastAppliedConfigSHA records the SHA256 hash of the last machine
	// configuration applied. When the bootstrap data changes, the controller
	// re-applies the new configuration over the authenticated Talos API.
	// The hash is cleared once a reset has been confirmed (the machine no
	// longer carries the applied configuration).
	// +optional
	LastAppliedConfigSHA string `json:"lastAppliedConfigSHA,omitempty"`

	// LastResetAt records when a machine reset was last issued. Used to
	// rate-limit reset attempts while the machine reboots.
	// +optional
	LastResetAt *metav1.Time `json:"lastResetAt,omitempty"`

	// Addresses contains the machine's addresses.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// Conditions defines current service state of the ByotMachine.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".metadata.ownerReferences[?(@.kind==\"Machine\")].name",description="Machine object which owns this ByotMachine"
// +kubebuilder:printcolumn:name="PublicIP",type="string",JSONPath=".status.resolvedPublicIP",description="Public IP of the adopted machine"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Machine adopted"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID",description="Provider ID of the adopted machine"

// ByotMachine is the Schema for the byotmachines API. It claims a
// ByotHost from the host registry and adopts the Talos machine at the host's
// public IP by applying the cluster's bootstrap machine configuration over
// the Talos machine API.
type ByotMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ByotMachineSpec   `json:"spec,omitempty"`
	Status ByotMachineStatus `json:"status,omitempty"`
}

// GetConditions returns the observations of the operational state of the ByotMachine resource.
func (m *ByotMachine) GetConditions() clusterv1.Conditions {
	return m.Status.Conditions
}

// SetConditions sets the underlying service state of the ByotMachine to the predescribed clusterv1.Conditions.
func (m *ByotMachine) SetConditions(conditions clusterv1.Conditions) {
	m.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ByotMachineList contains a list of ByotMachine.
type ByotMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ByotMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ByotMachine{}, &ByotMachineList{})
}
