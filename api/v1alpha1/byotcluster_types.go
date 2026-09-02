package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ByotClusterSpec defines the desired state of ByotCluster.
type ByotClusterSpec struct {
	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint"`
}

// ByotClusterStatus defines the observed state of ByotCluster.
type ByotClusterStatus struct {
	// Ready denotes that the cluster infrastructure is ready. Since adopted
	// clusters require no provisioning, this is set to true immediately.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Conditions defines current service state of the ByotCluster.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Cluster infrastructure is ready"

// ByotCluster is the Schema for the byotclusters API. It acts as the
// infrastructure cluster reference for clusters made of adopted Talos
// machines. No infrastructure is provisioned.
type ByotCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ByotClusterSpec   `json:"spec,omitempty"`
	Status ByotClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the observations of the operational state of the ByotCluster resource.
func (c *ByotCluster) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the underlying service state of the ByotCluster to the predescribed clusterv1.Conditions.
func (c *ByotCluster) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ByotClusterList contains a list of ByotCluster.
type ByotClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ByotCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ByotCluster{}, &ByotClusterList{})
}
