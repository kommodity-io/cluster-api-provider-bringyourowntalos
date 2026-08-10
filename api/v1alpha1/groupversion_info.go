// Package v1alpha1 contains API Schema definitions for the infrastructure v1alpha1 API group.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// Group is the API group for all byot resources.
const Group = "infrastructure.cluster.x-k8s.io"

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
