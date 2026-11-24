// Package v1alpha1 holds the names the Agent Platform's organizations share:
// the labels and annotations on an organization (an Organization of
// security.giantswarm.io with spec.agentPlatform.enabled) and on what the
// operator keeps for it, its finalizer, and its condition types and reasons.
// The Organization kind itself is organization-operator's
// (github.com/giantswarm/organization-operator/api/v1alpha1).
package v1alpha1

const (
	// DisplayNameAnnotation holds the name people see for the organization
	// (the portal shows it); the object's name stays the key. It is an
	// annotation, not a spec field, so a rename changes nothing the operator
	// keeps.
	DisplayNameAnnotation = "agent-platform.giantswarm.io/display-name"

	// OrganizationLabel is on the organization namespace and on the
	// organization's objects in the Flux namespace: the name of its
	// Organization.
	OrganizationLabel = "agent-platform.giantswarm.io/organization"
	// OrganizationUIDLabel is on the organization namespace and on the
	// organization's objects in the Flux namespace: the UID of its
	// Organization. The operator writes only a namespace that carries its
	// organization's UID, so it never takes over a namespace of the same name
	// that another writer created.
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

	// Finalizer is on every Organization whose Agent Platform the operator
	// keeps: the operator removes it once the deleted organization is erased.
	Finalizer = "agent-platform.giantswarm.io/erasure"

	// DeletePullRequestAnnotation is on an Organization whose delete pull
	// request (the one removing its file and its content path from git)
	// agent-manager opened: the pull request's URL. Once the file is gone
	// from git, the organization is Orphaned with reason DeleteMerged.
	DeletePullRequestAnnotation = "agent-platform.giantswarm.io/delete-pull-request"

	// FluxNameLabel and FluxNamespaceLabel are the ownership labels Flux's
	// kustomize-controller puts on what a Kustomization applies: the
	// Kustomization's name and namespace.
	FluxNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	FluxNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
)

// Condition types the operator writes to an Organization's
// status.conditions.
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
