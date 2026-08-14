package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ByotMachineTemplateSpec defines the desired state of ByotMachineTemplate.
type ByotMachineTemplateSpec struct {
	Template ByotMachineTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true

// ByotMachineTemplate is the Schema for the byotmachinetemplates API.
type ByotMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ByotMachineTemplateSpec `json:"spec,omitempty"`
}

// ByotMachineTemplateResource describes the data needed to create a ByotMachine from a template.
type ByotMachineTemplateResource struct {
	// Standard object's metadata.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the machine.
	Spec ByotMachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ByotMachineTemplateList contains a list of ByotMachineTemplate.
type ByotMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ByotMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ByotMachineTemplate{}, &ByotMachineTemplateList{})
}
