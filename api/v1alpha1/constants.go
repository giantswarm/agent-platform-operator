// Package v1alpha1 holds the names the Agent Platform's organizations share:
// the labels and annotations on an organization (an Organization of
// security.giantswarm.io with spec.capabilities.agentPlatform.enabled) and on
// what the operator keeps for it, and its finalizer. The Organization kind
// itself, with the AgentPlatform condition types and their reasons, is
// organization-operator's (github.com/giantswarm/organization-operator/api/v1alpha1).
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
	// from git, the organization's AgentPlatformOrphaned condition is True
	// with reason DeleteMerged.
	DeletePullRequestAnnotation = "agent-platform.giantswarm.io/delete-pull-request"

	// FluxNameLabel and FluxNamespaceLabel are the ownership labels Flux's
	// kustomize-controller puts on what a Kustomization applies: the
	// Kustomization's name and namespace.
	FluxNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	FluxNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
)
