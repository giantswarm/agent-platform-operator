// Package v1alpha1 holds the agent-platform.giantswarm.io/v1alpha1 API: the
// AgentPlatformOrganization.
//
// +kubebuilder:object:generate=true
// +groupName=agent-platform.giantswarm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the objects in this package.
	GroupVersion = schema.GroupVersion{Group: "agent-platform.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this package in a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &AgentPlatformOrganization{}, &AgentPlatformOrganizationList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
