// Package controller reconciles each Organization (security.giantswarm.io)
// whose spec.agentPlatform.enabled is true into its organization namespace.
//
// The organization namespace is agents-<name>: an Organization is
// cluster-scoped, so its name is unique in the cluster. The namespace is
// labelled with the organization's name and UID. The reconciler reads it from
// the API server, never from the cache, and writes only a namespace that
// carries the organization's UID: a namespace of that name that another
// writer created, or one left by an earlier Organization of the same name,
// makes the organization Ready=False with reason NameTaken. A name that does
// not fit in a namespace name (agents-<name> is a DNS label of at most 63
// characters) makes it Ready=False with reason NamespaceNameInvalid, and it
// gets no namespace. The namespace has no ownerReference.
//
// In the namespace the reconciler keeps, from the first reconcile on:
//
//   - the network policies copied from the platform namespace;
//   - the agent chart's OCIRepository;
//   - the Helm ServiceAccount the organization's HelmReleases run as, with its
//     RoleBinding to the ClusterRole Config.HelmClusterRole
//     (agent-platform-tenant-flux by default);
//   - a RoleBinding of each ServiceAccountBinding's ClusterRole to its
//     ServiceAccount in another namespace (Config.ServiceAccountBindings),
//     named after the ClusterRole, deleted when it is no longer configured;
//   - the RoleBinding organization-admins (admins.go), which binds
//     agent-platform:admin to the subjects of the organization's spec.admins,
//     follows that list and is deleted when the list is empty;
//   - read-only copies of the curated objects of the platform namespace
//     (Config.Curated): the Harnesses the organization's
//     spec.agentPlatform.harnesses names, pointed at the organization's own
//     pool and snapshot prefix with kagent's memory off; the curated
//     WorkerPool with its ServiceAccount, scaled by the worker replica rule
//     (workerReplicas); the Agents and AgentTemplates labelled
//     agent-platform.giantswarm.io/curated=true, with the template's
//     modelConfig set to the organization's default ModelConfig; and the
//     curated RemoteMCPServers their tools name. An Agent or AgentTemplate is
//     copied only when every object it refers to is a curated copy too
//     (references.go). A copy follows its source and the
//     default mark, an edit of it is reverted, and it goes when its source is
//     no longer curated for the organization;
//   - the ResourceQuota on object counts (Config.ObjectQuota);
//   - in commit mode (Config.Flux), the organization's Flux ServiceAccount
//     and Kustomization in the Flux namespace, both named after the
//     organization namespace and labelled with the organization's UID, and
//     the RoleBinding organization-flux in the organization namespace, the
//     ServiceAccount's only one. The Kustomization applies the
//     organization's path in git into its namespace as that ServiceAccount.
//
// The right to manage Organizations is bound by the installation: it binds
// agent-platform:organizations-admin for the whole cluster. The reconciler
// writes no binding for it.
//
// The reconciler writes the organization's state to status.agentPlatform and
// its conditions to status.conditions, with a merge patch under an optimistic
// lock, so the status fields of other writers (organization-operator's
// status.namespace) stay.
//
// With the installation's Kustomization configured, the reconciler sets the
// Orphaned condition of an organization that Kustomization applied: True
// while the organization is missing from its inventory (its file is gone from
// git), with reason DeleteMerged or MissingFromGit.
//
// Every organization with the Agent Platform enabled carries the finalizer
// v1alpha1.Finalizer. An Organization without the switch gets nothing. A
// switch-off is not supported yet: the agent-platform chart's admission
// policy refuses it. Where the reconciler finds the switch off on an
// organization with its finalizer all the same, it changes nothing, erases
// nothing and sets Ready=False with reason AgentPlatformDisabled (see
// reportSwitchOff). A deleted organization is
// erased in order (erase): its Flux Kustomization or HelmReleases and its
// Agents; its sessions through kagent, until Substrate holds no actor of it;
// its snapshot prefix, twice; its namespace; its Flux ServiceAccount; then
// the finalizer goes.
//
// The logic of the network policies, the OCIRepository, the Helm
// ServiceAccount and the WorkerPool and Harness copies is agent-manager's
// tenant reconciler (internal/tenants): it writes only objects it labels as
// its own and never changes or deletes one it did not write.
package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

const (
	// ObjectLabel marks what the reconciler writes in an organization
	// namespace, with why it is kept: ScopeNamespace objects always. An
	// object without it is never changed or deleted.
	ObjectLabel    = "agent-platform.giantswarm.io/organization-object"
	ScopeNamespace = "namespace"
	// CopiedFromAnnotation names the object a copy was made from.
	CopiedFromAnnotation = "agent-platform.giantswarm.io/copied-from"
	// NamespacePrefix starts the name of an organization namespace:
	// agents-<name>.
	NamespacePrefix = "agents-"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "agent-platform-operator"
	fieldManager   = "agent-platform-operator"

	reasonReconciled           = "Reconciled"
	reasonReconcileFailed      = "ReconcileFailed"
	reasonNamespaceTerminating = "NamespaceTerminating"
	reasonSourceMissing        = "SourceMissing"
	reasonNameTaken            = "NameTaken"
	reasonNamespaceNameInvalid = "NamespaceNameInvalid"
	reasonAgentPlatformOff     = "AgentPlatformDisabled"
)

// Config is what the reconciler keeps and where it copies it from.
type Config struct {
	// PlatformNamespace is where the network policies are copied from.
	PlatformNamespace string
	// NetworkPolicies are the names of the network policies copied from the
	// platform namespace, of the kind NetworkPolicyGVK names (empty: none).
	NetworkPolicies  []string
	NetworkPolicyGVK schema.GroupVersionKind
	// Chart is the agent chart's OCIRepository.
	Chart ChartSource
	// HelmServiceAccount is the ServiceAccount the organization's HelmReleases
	// run as; HelmClusterRole the ClusterRole it is bound to in the
	// organization namespace. Empty HelmServiceAccount keeps neither.
	HelmServiceAccount string
	HelmClusterRole    string
	// ServiceAccountBindings are the ClusterRoles bound in every
	// organization namespace to a ServiceAccount of another namespace; empty
	// binds none.
	ServiceAccountBindings []ServiceAccountBinding
	// SnapshotBucket is the bucket of the snapshot prefix s3://<bucket>/<uid>;
	// empty leaves the prefix empty. The curated copies need it.
	SnapshotBucket string
	// Curated is what the reconciler copies from the platform namespace; the
	// zero value copies nothing.
	Curated CuratedConfig
	// ObjectQuota are the hard limits of the ResourceQuota on object counts
	// in every organization namespace; empty keeps none.
	ObjectQuota corev1.ResourceList
	// Erasure is how a deleted organization is erased.
	Erasure ErasureConfig
	// Flux is commit mode; the zero value keeps nothing in git's name.
	Flux FluxConfig
}

// CacheOptions limit what the manager caches: the organization namespaces,
// the reconciler's own ServiceAccounts, ResourceQuotas and, in the Flux
// namespace, Kustomizations, the installation's Kustomization, every RoleBinding
// (a binding of an organization namespace that holds a name the reconciler
// needs carries no label of the reconciler's),
// the metadata of every ModelConfig and every Agent and, of the network
// policies and the other curated kinds, the platform namespace's and the
// reconciler's copies.
//
// The network policies and the curated kinds are cached for their watch
// only. The reconciler reads them, and the OCIRepository, as unstructured
// objects, which the client reads from the API server
// (client.CacheOptions.Unstructured is off): the operator starts on a cluster
// that does not serve the OCIRepository kind.
func CacheOptions(cfg Config, syncPeriod time.Duration) cache.Options {
	byObject := map[client.Object]cache.ByObject{
		&corev1.Namespace{}:      {Label: hasLabel(v1alpha1.OrganizationUIDLabel)},
		&corev1.ServiceAccount{}: {Label: hasLabel(ObjectLabel)},
		&corev1.ResourceQuota{}:  {Label: hasLabel(ObjectLabel)},
	}
	sourcesAndCopies := cache.ByObject{Namespaces: map[string]cache.Config{
		cfg.PlatformNamespace: {},
		cache.AllNamespaces:   {LabelSelector: hasLabel(ObjectLabel)},
	}}
	if !cfg.NetworkPolicyGVK.Empty() {
		byObject[object(cfg.NetworkPolicyGVK)] = sourcesAndCopies
	}
	if cfg.Curated.Enabled() {
		for _, gvk := range cfg.Curated.copied() {
			// The Agents are watched by their metadata in every namespace:
			// any Agent of an organization namespace counts for the worker
			// replica rule. The cache is keyed by kind, so an entry here
			// would narrow that watch too.
			if gvk != cfg.Curated.agent() {
				byObject[object(gvk)] = sourcesAndCopies
			}
		}
	}
	if f := cfg.Flux; f.watched() {
		// The organizations' Kustomizations and the installation's.
		namespaces := map[string]cache.Config{}
		if f.commitMode() {
			namespaces[f.Namespace] = cache.Config{LabelSelector: hasLabel(ObjectLabel)}
		}
		if f.orphans() {
			namespaces[f.Installation.Namespace] = cache.Config{}
		}
		byObject[object(f.Kustomization)] = cache.ByObject{Namespaces: namespaces, Transform: stripInventory(f.Installation)}
	}
	return cache.Options{
		SyncPeriod:       &syncPeriod,
		ByObject:         byObject,
		DefaultTransform: cache.TransformStripManagedFields(),
	}
}

// OrganizationReconciler keeps the organization namespaces.
type OrganizationReconciler struct {
	// Client reads from the cache and writes to the API server.
	Client client.Client
	// APIReader reads from the API server: the organization namespace is
	// looked up there, so a namespace the cache has not seen yet is found.
	APIReader client.Reader
	// Cache reads the installation Kustomization from the manager's cache,
	// which watches it; nil reads it from the API server.
	Cache  client.Reader
	Config Config
}

// SetupWithManager registers the controller: one work item per
// Organization.
func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inOrganization := handler.EnqueueRequestsFromMapFunc(r.organizationOfNamespace)
	isOwn := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[ObjectLabel]
		return ok
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("organization").
		For(&orgv1.Organization{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(organizationOfLabels)).
		// A ServiceAccount in an organization namespace, or an organization's
		// Flux ServiceAccount, which carries the organization's labels.
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.organizationOfObject), builder.WithPredicates(isOwn)).
		// The quota's own changes, not the usage the quota controller
		// records with every object it counts.
		Watches(&corev1.ResourceQuota{}, inOrganization, builder.WithPredicates(isOwn, quotaChanged)).
		// Any binding in an organization namespace: the reconciler's own, or
		// one that holds a name the reconciler needs.
		Watches(&rbacv1.RoleBinding{}, inOrganization)
	// A status write changes neither the spec (the generation) nor the
	// labels: nothing the reconciler copies or decides on.
	specOrLabels := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	var copied []schema.GroupVersionKind
	if !r.Config.NetworkPolicyGVK.Empty() {
		copied = append(copied, r.Config.NetworkPolicyGVK)
	}
	if c := r.Config.Curated; c.Enabled() {
		for _, gvk := range c.copied() {
			if gvk != c.agent() {
				copied = append(copied, gvk)
			}
		}
		// Any ModelConfig of an organization namespace: its creation, its
		// deletion and a move of the default mark.
		mc := &metav1.PartialObjectMetadata{}
		mc.SetGroupVersionKind(c.modelConfig())
		b = b.Watches(mc, inOrganization, builder.OnlyMetadata, builder.WithPredicates(predicate.AnnotationChangedPredicate{}))
		// Any Agent of an organization namespace (its creation and deletion
		// change the worker replica rule's count), a curated source and a
		// curated copy.
		agent := &metav1.PartialObjectMetadata{}
		agent.SetGroupVersionKind(c.agent())
		b = b.Watches(agent, handler.EnqueueRequestsFromMapFunc(r.organizationsOfAgent), builder.OnlyMetadata, builder.WithPredicates(specOrLabels))
	}
	if r.Config.Flux.watched() {
		b = b.Watches(object(r.Config.Flux.Kustomization), handler.EnqueueRequestsFromMapFunc(r.organizationsOfKustomization), builder.WithPredicates(r.kustomizationChanged()))
	}
	for _, gvk := range copied {
		b = b.Watches(object(gvk), handler.EnqueueRequestsFromMapFunc(r.organizationsOfCopy), builder.WithPredicates(specOrLabels))
	}
	return b.Complete(r)
}

// quotaChanged passes a ResourceQuota event but an update that changes
// neither its spec nor its labels.
var quotaChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, ok := e.ObjectOld.(*corev1.ResourceQuota)
	n, ok2 := e.ObjectNew.(*corev1.ResourceQuota)
	return !ok || !ok2 || !equality.Semantic.DeepEqual(o.Spec, n.Spec) || !maps.Equal(o.Labels, n.Labels)
}}

// organizationOfLabels maps an organization namespace to its organization.
func organizationOfLabels(_ context.Context, o client.Object) []reconcile.Request {
	l := o.GetLabels()
	if l[v1alpha1.OrganizationLabel] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: l[v1alpha1.OrganizationLabel]}}}
}

// organizationOfNamespace maps an object in an organization namespace to the
// organization.
func (r *OrganizationReconciler) organizationOfNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	ns := &corev1.Namespace{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: o.GetNamespace()}, ns); err != nil {
		return nil
	}
	return organizationOfLabels(ctx, ns)
}

// organizationOfObject maps an object that carries the organization labels
// (an organization's object in the Flux namespace) to its organization, and
// any other to the organization of its namespace.
func (r *OrganizationReconciler) organizationOfObject(ctx context.Context, o client.Object) []reconcile.Request {
	if reqs := organizationOfLabels(ctx, o); len(reqs) > 0 {
		return reqs
	}
	return r.organizationOfNamespace(ctx, o)
}

// allOrganizations maps to every organization the reconciler keeps: every
// Organization with the Agent Platform enabled or with its finalizer.
func (r *OrganizationReconciler) allOrganizations(ctx context.Context) []reconcile.Request {
	list := &orgv1.OrganizationList{}
	if err := r.Client.List(ctx, list); err != nil {
		logf.FromContext(ctx).Error(err, "listing the Organizations failed")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		org := &list.Items[i]
		if enabled(org) || controllerutil.ContainsFinalizer(org, v1alpha1.Finalizer) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(org)})
		}
	}
	return reqs
}

// organizationsOfCopy maps an object of the platform namespace (a network
// policy, a curated object) to every organization and a copy to its own.
func (r *OrganizationReconciler) organizationsOfCopy(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() == r.Config.PlatformNamespace {
		return r.allOrganizations(ctx)
	}
	if _, ok := o.GetLabels()[ObjectLabel]; ok {
		return r.organizationOfNamespace(ctx, o)
	}
	return nil
}

// organizationsOfAgent maps an Agent of the platform namespace (a curated
// source) to every organization, and any other Agent, a curated copy or not,
// to the organization of its namespace.
func (r *OrganizationReconciler) organizationsOfAgent(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() == r.Config.PlatformNamespace {
		return r.allOrganizations(ctx)
	}
	return r.organizationOfNamespace(ctx, o)
}

// Reconcile keeps one organization.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	org := &orgv1.Organization{}
	if err := r.Client.Get(ctx, req.NamespacedName, org); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if org.DeletionTimestamp != nil {
		return r.erase(ctx, org)
	}
	if !enabled(org) {
		// Without the switch the organization gets nothing. With the
		// finalizer the switch was on.
		if !controllerutil.ContainsFinalizer(org, v1alpha1.Finalizer) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, r.reportSwitchOff(ctx, org)
	}
	// The finalizer comes first: an organization deleted from here on is
	// erased.
	if !controllerutil.ContainsFinalizer(org, v1alpha1.Finalizer) {
		patch := client.MergeFromWithOptions(org.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(org, v1alpha1.Finalizer)
		if err := r.Client.Patch(ctx, org, patch, client.FieldOwner(fieldManager)); err != nil {
			return reconcile.Result{}, fmt.Errorf("add the finalizer: %w", err)
		}
	}
	status := stateOf(org)
	status.SnapshotPrefix = r.snapshotPrefix(org)
	// First: the worker replica rule reads it.
	orphanedErr := r.setOrphaned(ctx, org, status)

	ready, err := readyCondition(errors.Join(orphanedErr, r.reconcile(ctx, org, status)), org.Generation)
	meta.SetStatusCondition(&status.Conditions, ready)
	if uerr := r.writeStatus(ctx, org, status); uerr != nil {
		return reconcile.Result{}, errors.Join(err, uerr)
	}
	return reconcile.Result{}, err
}

// reportSwitchOff reports an organization whose Agent Platform was turned
// off, or whose spec.agentPlatform was removed, after it was on. A switch-off
// is not supported yet, and the agent-platform chart's admission policy
// refuses it; when it gets through all the same, the reconciler changes
// nothing and erases nothing: the namespace, its contents, the admins binding
// and the finalizer stay as they are. Only Ready says so. A delete of the
// Organization still erases it.
func (r *OrganizationReconciler) reportSwitchOff(ctx context.Context, org *orgv1.Organization) error {
	status := stateOf(org)
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonAgentPlatformOff,
		Message:            "spec.agentPlatform.enabled is off or spec.agentPlatform is gone: turning the Agent Platform off is not supported yet, and nothing was changed or erased; turn it on again, or delete the Organization to erase it",
		ObservedGeneration: org.Generation,
	})
	return r.writeStatus(ctx, org, status)
}

// enabled reports whether org has the Agent Platform switched on.
func enabled(org *orgv1.Organization) bool {
	return org.Spec.AgentPlatform != nil && org.Spec.AgentPlatform.Enabled
}

// platformSpec is org's spec.agentPlatform, the zero value when it has none.
func platformSpec(org *orgv1.Organization) orgv1.AgentPlatformSpec {
	if org.Spec.AgentPlatform == nil {
		return orgv1.AgentPlatformSpec{}
	}
	return *org.Spec.AgentPlatform
}

// platformStatus is org's status.agentPlatform, the zero value when it has
// none.
func platformStatus(org *orgv1.Organization) orgv1.AgentPlatformStatus {
	if org.Status.AgentPlatform == nil {
		return orgv1.AgentPlatformStatus{}
	}
	return *org.Status.AgentPlatform
}

// orgState is the part of an Organization's status the reconciler writes: its
// own block, status.agentPlatform, and status.conditions.
type orgState struct {
	orgv1.AgentPlatformStatus
	Conditions []metav1.Condition
}

// stateOf is a copy of the part of org's status the reconciler writes.
func stateOf(org *orgv1.Organization) *orgState {
	s := &orgState{}
	if ap := org.Status.AgentPlatform; ap != nil {
		ap.DeepCopyInto(&s.AgentPlatformStatus)
	}
	for i := range org.Status.Conditions {
		s.Conditions = append(s.Conditions, *org.Status.Conditions[i].DeepCopy())
	}
	return s
}

// applyTo sets s in org's status.
func (s *orgState) applyTo(org *orgv1.Organization) {
	ap := s.AgentPlatformStatus
	org.Status.AgentPlatform = ap.DeepCopy()
	org.Status.Conditions = s.Conditions
}

// writeStatus writes s to org's status when it changed it. The merge patch
// carries only what changed, so the fields of other writers stay; it
// replaces the whole conditions list, so it is sent under an optimistic lock.
func (r *OrganizationReconciler) writeStatus(ctx context.Context, org *orgv1.Organization, s *orgState) error {
	before := org.DeepCopy()
	s.applyTo(org)
	if equality.Semantic.DeepEqual(before.Status, org.Status) {
		return nil
	}
	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := r.Client.Status().Patch(ctx, org, patch, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// reconcile finds or creates the organization namespace, records it in
// status and keeps the objects in it.
func (r *OrganizationReconciler) reconcile(ctx context.Context, org *orgv1.Organization, status *orgState) error {
	ns, err := r.namespace(ctx, org)
	if err != nil {
		return err
	}
	status.Namespace = ns.Name
	if ns.DeletionTimestamp != nil || ns.Status.Phase == corev1.NamespaceTerminating {
		return &notReadyError{reason: reasonNamespaceTerminating, message: fmt.Sprintf("the organization namespace %s is terminating", ns.Name)}
	}
	var errs []error
	if r.Config.Chart.URL != "" {
		errs = append(errs, r.ensureExists(ctx, ns.Name, r.Config.Chart.object(ns.Name)))
	}
	if sa := r.Config.HelmServiceAccount; sa != "" {
		errs = append(errs, r.ensureServiceAccount(ctx, ns.Name, sa, ScopeNamespace), r.ensureHelmRoleBinding(ctx, ns.Name))
	}
	errs = append(errs, r.keepServiceAccountBindings(ctx, ns.Name), r.keepAdmins(ctx, org, ns.Name))
	if !r.Config.NetworkPolicyGVK.Empty() {
		for _, name := range r.Config.NetworkPolicies {
			errs = append(errs, r.copyObject(ctx, r.Config.NetworkPolicyGVK, name, ns.Name))
		}
	}
	if r.Config.Flux.commitMode() {
		errs = append(errs, r.keepFlux(ctx, org, ns.Name))
	}
	if r.Config.Curated.Enabled() {
		errs = append(errs, r.keepCurated(ctx, org, ns.Name, status))
	}
	// Last: until the quota controller records its usage, the quota refuses
	// every object it counts.
	errs = append(errs, r.ensureObjectQuota(ctx, ns.Name))
	return errors.Join(errs...)
}

// notReadyError is a cause of Ready=False that a retry does not change: the
// reconcile has done what it can.
type notReadyError struct{ reason, message string }

func (e *notReadyError) Error() string { return e.message }

// readyCondition is the Ready condition a reconcile that returned err leaves,
// and the error to retry: the failures in err, without its causes of
// Ready=False. A failure makes the reason ReconcileFailed; the message names
// the failures and then the causes of Ready=False, so a cause stays visible
// while a failure lasts.
func readyCondition(err error, generation int64) (metav1.Condition, error) {
	ready := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: reasonReconciled, ObservedGeneration: generation}
	failed, notReady := split(err)
	err = errors.Join(failed...)
	messages := make([]string, 0, 1+len(notReady))
	if err != nil {
		messages = append(messages, err.Error())
	}
	for _, nr := range notReady {
		messages = append(messages, nr.message)
	}
	switch {
	case err != nil:
		ready.Status, ready.Reason = metav1.ConditionFalse, reasonReconcileFailed
	case len(notReady) > 0:
		// Not an error to retry: the event that changes the cause (the
		// namespace's deletion, the source's creation) reconciles the
		// organization again.
		ready.Status, ready.Reason = metav1.ConditionFalse, notReady[0].reason
	}
	ready.Message = strings.Join(messages, "; ")
	return ready, err
}

// split sorts the errors joined in err into failures to retry and causes of
// Ready=False. Only a *notReadyError itself is a cause of Ready=False: an
// error that wraps one (fmt.Errorf with %w) is a failure, because it can wrap
// a failure too, and a failure must never be lost to a cause.
func split(err error) (failed []error, notReady []*notReadyError) {
	if err == nil {
		return nil, nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			f, n := split(e)
			failed, notReady = append(failed, f...), append(notReady, n...)
		}
		return failed, notReady
	}
	if nr, ok := err.(*notReadyError); ok { //nolint:errorlint // a wrapped cause is a failure, see above
		return nil, []*notReadyError{nr}
	}
	return []error{err}, nil
}

// namespace is the organization namespace agents-<name>, read from the API
// server, or a new one. A namespace of that name without the organization's
// UID is not the organization's: the reconciler does not take it over.
func (r *OrganizationReconciler) namespace(ctx context.Context, org *orgv1.Organization) (*corev1.Namespace, error) {
	name, err := namespaceName(org)
	if err != nil {
		return nil, err
	}
	ns := &corev1.Namespace{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Name: name}, ns)
	switch {
	case err == nil && ns.Labels[v1alpha1.OrganizationUIDLabel] == string(org.UID):
		return ns, nil
	case err == nil:
		return nil, &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the namespace %s exists and is not this organization's (it does not carry %s=%s), so the operator does not take it over", name, v1alpha1.OrganizationUIDLabel, org.UID)}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("get the namespace %s: %w", name, err)
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			v1alpha1.OrganizationLabel:    org.Name,
			v1alpha1.OrganizationUIDLabel: string(org.UID),
			managedByLabel:                managedByValue,
		},
	}}
	if err := r.Client.Create(ctx, ns, client.FieldOwner(fieldManager)); err != nil {
		return nil, fmt.Errorf("create the organization namespace %s: %w", name, err)
	}
	logf.FromContext(ctx).Info("organization namespace created", "namespace", ns.Name)
	return ns, nil
}

// namespaceName is the name of org's namespace, agents-<name>, or a cause of
// Ready=False when that is not a valid namespace name (a DNS label of at
// most 63 characters): org gets no namespace.
func namespaceName(org *orgv1.Organization) (string, error) {
	name := NamespacePrefix + org.Name
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return "", &notReadyError{reason: reasonNamespaceNameInvalid, message: fmt.Sprintf("the organization gets no namespace: %s is not a valid namespace name: %s", name, strings.Join(errs, "; "))}
	}
	return name, nil
}

func (r *OrganizationReconciler) snapshotPrefix(org *orgv1.Organization) string {
	if r.Config.SnapshotBucket == "" {
		return ""
	}
	return "s3://" + r.Config.SnapshotBucket + "/" + string(org.UID)
}

// hasLabel selects the objects that carry key.
func hasLabel(key string) labels.Selector {
	req, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		panic(err) // the keys are constants
	}
	return labels.NewSelector().Add(*req)
}
