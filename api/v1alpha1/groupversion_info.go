// Package v1alpha1 contains the hvmc API types HostConfig and MemoryLease
// (group hvmc.fax.fyi, version v1alpha1; docs/DESIGN.md §5).
//
// +kubebuilder:object:generate=true
// +groupName=hvmc.fax.fyi
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the hvmc API.
	GroupVersion = schema.GroupVersion{Group: "hvmc.fax.fyi", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this package with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&HostConfig{}, &HostConfigList{},
		&MemoryLease{}, &MemoryLeaseList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
