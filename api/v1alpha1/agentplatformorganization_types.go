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

	// CuratedLabel, set to "true" on an Agent or AgentTemplate in the platform
	// namespace, makes it curated: the operator copies it, read-only, into
	// every organization namespace.
	CuratedLabel = "agent-platform.giantswarm.io/curated"
	// DefaultModelConfigAnnotation, set to "true" on a ModelConfig in an
	// organization namespace, makes it the organization's default model
	// (giantswarm-platform-manager#789): the one curated Agents run on. Exactly
	// one marked ModelConfig is the default; none or more than one means there
	// is none.
	DefaultModelConfigAnnotation = "agent-platform.giantswarm.io/is-default-model-config"

	// Finalizer is on every AgentPlatformOrganization: the operator removes it
	// once the deleted organization is erased.
	Finalizer = "agent-platform.giantswarm.io/erasure"

	// DeletePullRequestAnnotation is on an AgentPlatformOrganization whose
	// delete pull request (the one removing its file and its content path from
	// git) agent-manager opened: the pull request's URL. Once the file is gone
	// from git, the organization is Orphaned with reason DeleteMerged.
	DeletePullRequestAnnotation = "agent-platform.giantswarm.io/delete-pull-request"

	// FluxNameLabel and FluxNamespaceLabel are the ownership labels Flux's
	// kustomize-controller puts on what a Kustomization applies: the
	// Kustomization's name and namespace.
	FluxNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	FluxNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
)

// Erasure steps of a deleted AgentPlatformOrganization, in order
// (status.erasureStep). The last step, the finalizer's removal, leaves no
// object to carry it.
const (
	// ErasureWorkloads deletes the organization's Flux Kustomization, or its
	// HelmReleases where it has none, and its Agents, and waits until they
	// are gone.
	ErasureWorkloads = "Workloads"
	// ErasureSessions deletes every session of the organization's agents
	// through kagent and waits until Substrate holds no actor of the
	// organization.
	ErasureSessions = "Sessions"
	// ErasureSnapshots empties the organization's snapshot prefix, and again
	// after the snapshot erase delay.
	ErasureSnapshots = "Snapshots"
	// ErasureNamespace deletes the organization namespace and waits until it
	// is gone.
	ErasureNamespace = "Namespace"
	// ErasureOutsideObjects deletes what the operator created for the
	// organization outside its namespace: the Flux ServiceAccount.
	ErasureOutsideObjects = "OutsideObjects"
)

// Condition types of an AgentPlatformOrganization.
const (
	// ConditionReady is True when the organization namespace exists and holds
	// every object the operator keeps there.
	ConditionReady = "Ready"
	// ConditionDefaultModelConfigured is True while exactly one ModelConfig of
	// the organization namespace carries DefaultModelConfigAnnotation, and
	// False while none or more than one does, so curated Agents cannot run.
	// Set only where the operator copies the curated objects.
	ConditionDefaultModelConfigured = "DefaultModelConfigured"
	// ConditionOrphaned is True when the object carries the installation
	// Kustomization's ownership labels but is missing from that
	// Kustomization's inventory: its file is gone from git. Its reason is
	// DeleteMerged or MissingFromGit. It is False while the object is in the
	// inventory, and absent on an object without the ownership labels (one
	// applied without git).
	ConditionOrphaned = "Orphaned"
)

// Reasons of the Orphaned condition.
const (
	// ReasonDeleteMerged: the organization's file is gone from git and it
	// carries DeletePullRequestAnnotation, so its delete pull request merged.
	ReasonDeleteMerged = "DeleteMerged"
	// ReasonMissingFromGit: the organization's file is gone from git without
	// a delete pull request, a git mistake.
	ReasonMissingFromGit = "MissingFromGit"
	// ReasonInInventory: the organization's file is in git.
	ReasonInInventory = "InInventory"
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
	// Replicas is the number of warm workers of the organization's WorkerPool
	// while the organization can use them: the pool runs 0 while the
	// organization namespace holds no ModelConfig and while the organization
	// is Orphaned. A deleted organization runs Replicas until its sessions
	// are gone, so kagent can suspend its actors, and 0 after.
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

	// ErasureStep is the erasure step the operator is on once the
	// organization is deleted: Workloads, Sessions, Snapshots, Namespace or
	// OutsideObjects. Empty while the organization is not deleted.
	// +optional
	// +kubebuilder:validation:Enum=Workloads;Sessions;Snapshots;Namespace;OutsideObjects
	ErasureStep string `json:"erasureStep,omitempty"`

	// SnapshotsErasedAt is when the operator first emptied the snapshot
	// prefix of the deleted organization; it empties it once more after the
	// snapshot erase delay.
	// +optional
	SnapshotsErasedAt *metav1.MicroTime `json:"snapshotsErasedAt,omitempty"`

	// LastRevisionInGit is the installation Kustomization's
	// status.lastAppliedRevision (e.g. <branch>@sha1:<sha> for a
	// GitRepository) from the last time the organization was in that
	// Kustomization's settled inventory: a revision whose tree holds the
	// organization's file. Kept while the organization is Orphaned, so a file
	// gone from git can be found. Empty when no such revision was recorded:
	// on an organization applied without git, and on one already missing from
	// the inventory when the operator first ran with this field.
	// +optional
	LastRevisionInGit string `json:"lastRevisionInGit,omitempty"`

	// Conditions: Ready, DefaultModelConfigured and Orphaned. While the
	// organization is erased, Ready is False with reason Deleting and its
	// message names the step.
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
// +kubebuilder:printcolumn:name="Erasure",type=string,JSONPath=`.status.erasureStep`,priority=1
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
