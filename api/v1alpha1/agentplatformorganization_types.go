package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DisplayNameAnnotation holds the name people see for the organization
	// (the portal shows it); the object's name stays the key. It is an
	// annotation, not a spec field, so a rename changes nothing the operator
	// keeps.
	DisplayNameAnnotation = "agent-platform.giantswarm.io/display-name"

	// OrganizationLabel is on the organization namespace: the name of its
	// AgentPlatformOrganization.
	OrganizationLabel = "agent-platform.giantswarm.io/organization"
	// ParentLabel is on the organization namespace: the parent namespace, the
	// one its AgentPlatformOrganization lives in.
	ParentLabel = "agent-platform.giantswarm.io/parent"
	// OrganizationUIDLabel is on the organization namespace: the UID of its
	// AgentPlatformOrganization. The operator finds the namespace by it, never
	// by name.
	OrganizationUIDLabel = "agent-platform.giantswarm.io/organization-uid"
)

// Condition types of an AgentPlatformOrganization.
const (
	// ConditionReady is True when the organization namespace exists and holds
	// every object the operator keeps there.
	ConditionReady = "Ready"
	// ConditionDefaultModelConfigured is False while the organization namespace
	// holds no default ModelConfig, so curated Agents cannot run. Not set yet.
	ConditionDefaultModelConfigured = "DefaultModelConfigured"
	// ConditionOrphaned is True when the object carries the installation
	// Kustomization's ownership labels but is missing from that
	// Kustomization's inventory: its file is gone from git. Its reason is
	// DeleteMerged or MissingFromGit. Not set yet.
	ConditionOrphaned = "Orphaned"
)

// AgentPlatformOrganizationSpec is what the organization gets.
type AgentPlatformOrganizationSpec struct {
	// Harnesses are the names of the curated Harnesses the organization gets.
	// +optional
	// +listType=set
	Harnesses []string `json:"harnesses,omitempty"`

	// Workers sizes the organization's WorkerPool.
	// +optional
	Workers Workers `json:"workers,omitempty"`
}

// Workers sizes the organization's WorkerPool.
type Workers struct {
	// Replicas is the number of warm workers of the organization's WorkerPool.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`
}

// AgentPlatformOrganizationStatus is what the operator observed.
type AgentPlatformOrganizationStatus struct {
	// Namespace is the organization namespace, created from the generateName
	// agents-<name>-.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// SnapshotPrefix is where the organization's worker snapshots live,
	// s3://<bucket>/<uid>. Empty when the operator has no snapshot bucket.
	// +optional
	SnapshotPrefix string `json:"snapshotPrefix,omitempty"`

	// Conditions: Ready, DefaultModelConfigured and Orphaned.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentPlatformOrganization is a share of the installation's Agent Platform
// for one team: it lives in a parent namespace and owns an organization
// namespace that holds its agents, MCP servers, models, credentials and
// members.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Organization namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50",message="the name of an AgentPlatformOrganization is at most 50 characters: the organization namespace is agents-<name>- plus 5 random characters"
// +kubebuilder:validation:XValidation:rule="!self.metadata.name.contains('.')",message="the name of an AgentPlatformOrganization holds no dot: the organization namespace agents-<name>- is a DNS label"
type AgentPlatformOrganization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec AgentPlatformOrganizationSpec `json:"spec,omitempty"`
	// +optional
	Status AgentPlatformOrganizationStatus `json:"status,omitempty"`
}

// AgentPlatformOrganizationList is a list of AgentPlatformOrganizations.
//
// +kubebuilder:object:root=true
type AgentPlatformOrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPlatformOrganization `json:"items"`
}
