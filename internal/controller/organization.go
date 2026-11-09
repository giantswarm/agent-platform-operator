// Package controller reconciles each AgentPlatformOrganization into its
// organization namespace.
//
// The organization namespace is created from the generateName agents-<name>-
// and labelled with the organization, its parent namespace and its UID. The
// reconciler finds it by the UID label, read from the API server and never
// from the cache, so a create that was interrupted before the status was
// written never makes a second namespace. The namespace has no ownerReference:
// the garbage collector never acts on a namespaced owner of a cluster-scoped
// object.
//
// In the namespace the reconciler keeps, from the first reconcile on:
//
//   - the network policies copied from the platform namespace;
//   - the agent chart's OCIRepository;
//   - the Helm ServiceAccount the organization's HelmReleases run as, with its
//     RoleBinding to the tenant Flux ClusterRole;
//   - a RoleBinding of each ServiceAccountBinding's ClusterRole to its
//     ServiceAccount in another namespace (Config.ServiceAccountBindings),
//     named after the ClusterRole, deleted when it is no longer configured;
//   - a mirror of every RoleBinding of the parent namespace to the ClusterRole
//     agent-platform:organizations-admin, and of every rbac-operator
//     RoleBinding there to cluster-admin, bound to
//     agent-platform:organizations-admin with the User and Group subjects
//     only, removed when the parent binding goes;
//   - read-only copies of the curated objects of the platform namespace
//     (Config.Curated): the Harnesses the organization's spec.harnesses names,
//     pointed at the organization's own pool and snapshot prefix with kagent's
//     memory off; the curated WorkerPool with its ServiceAccount, scaled by
//     the worker replica rule (workerReplicas); the Agents and AgentTemplates
//     labelled agent-platform.giantswarm.io/curated=true, with the template's
//     modelConfig set to the organization's default ModelConfig; and the
//     RemoteMCPServers their tools name. A copy follows its source and the
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
// With the installation's Kustomization configured, the reconciler sets the
// Orphaned condition of an organization that Kustomization applied: True
// while the organization is missing from its inventory (its file is gone from
// git), with reason DeleteMerged or MissingFromGit.
//
// Every organization carries the finalizer v1alpha1.Finalizer. A deleted
// organization is erased in order (erase): its Flux Kustomization or
// HelmReleases and its Agents; its sessions through kagent, until Substrate
// holds no actor of it; its snapshot prefix, twice; its namespace; its Flux
// ServiceAccount; then the finalizer goes.
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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
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

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

const (
	// ObjectLabel marks what the reconciler writes in an organization
	// namespace, with why it is kept: ScopeNamespace objects always,
	// ScopeAdminMirror objects while their parent binding exists. An object
	// without it is never changed or deleted.
	ObjectLabel      = "agent-platform.giantswarm.io/organization-object"
	ScopeNamespace   = "namespace"
	ScopeAdminMirror = "organizations-admin-mirror"
	// CopiedFromAnnotation names the object a copy or a mirror was made from.
	CopiedFromAnnotation = "agent-platform.giantswarm.io/copied-from"
	// OrganizationsAdminClusterRole is the ClusterRole whose RoleBindings in a
	// parent namespace are mirrored into its organization namespaces.
	OrganizationsAdminClusterRole = "agent-platform:organizations-admin"
	// rbacOperatorLabel is the label rbac-operator sets, with the value
	// rbac-operator, on every RoleBinding it writes; its cluster-admin
	// bindings in a parent namespace are mirrored as organizations-admin.
	rbacOperatorLabel = "giantswarm.io/managed-by"
	rbacOperatorValue = "rbac-operator"
	// clusterAdminClusterRole is the role of rbac-operator's admin bindings.
	clusterAdminClusterRole = "cluster-admin"
	// NamespacePrefix starts the generateName of an organization namespace.
	NamespacePrefix = "agents-"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "agent-platform-operator"
	fieldManager   = "agent-platform-operator"

	reasonReconciled           = "Reconciled"
	reasonReconcileFailed      = "ReconcileFailed"
	reasonNamespaceTerminating = "NamespaceTerminating"
	reasonSourceMissing        = "SourceMissing"
	reasonNameTaken            = "NameTaken"
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
// (a parent's admin bindings carry no label common to all of them: its
// organizations-admin bindings have none, and its rbac-operator bindings are
// told apart by roleRef and label, isMirroredAdmin),
// the metadata of every ModelConfig and, of the network policies and the
// curated kinds, the platform namespace's and the reconciler's copies.
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
			byObject[object(gvk)] = sourcesAndCopies
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
// AgentPlatformOrganization.
func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inOrganization := handler.EnqueueRequestsFromMapFunc(r.organizationOfNamespace)
	isOwn := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[ObjectLabel]
		return ok
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("agentplatformorganization").
		For(&v1alpha1.AgentPlatformOrganization{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(organizationOfLabels)).
		// A ServiceAccount in an organization namespace, or an organization's
		// Flux ServiceAccount, which carries the organization's labels.
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.organizationOfObject), builder.WithPredicates(isOwn)).
		// The quota's own changes, not the usage the quota controller
		// records with every object it counts.
		Watches(&corev1.ResourceQuota{}, inOrganization, builder.WithPredicates(isOwn, quotaChanged)).
		// A parent's organizations-admin binding reconciles the parent's
		// organizations; a binding in an organization namespace its own.
		Watches(&rbacv1.RoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.organizationsOfBinding))
	// A status write changes neither the spec (the generation) nor the
	// labels: nothing the reconciler copies or decides on.
	specOrLabels := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	var copied []schema.GroupVersionKind
	if !r.Config.NetworkPolicyGVK.Empty() {
		copied = append(copied, r.Config.NetworkPolicyGVK)
	}
	if r.Config.Curated.Enabled() {
		copied = append(copied, r.Config.Curated.copied()...)
		// Any ModelConfig of an organization namespace: its creation, its
		// deletion and a move of the default mark.
		mc := &metav1.PartialObjectMetadata{}
		mc.SetGroupVersionKind(r.Config.Curated.modelConfig())
		b = b.Watches(mc, inOrganization, builder.OnlyMetadata, builder.WithPredicates(predicate.AnnotationChangedPredicate{}))
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
	if l[v1alpha1.ParentLabel] == "" || l[v1alpha1.OrganizationLabel] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: l[v1alpha1.ParentLabel], Name: l[v1alpha1.OrganizationLabel]}}}
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

// organizationsOf maps to every organization in namespace (every organization
// when namespace is empty).
func (r *OrganizationReconciler) organizationsOf(ctx context.Context, namespace string) []reconcile.Request {
	list := &v1alpha1.AgentPlatformOrganizationList{}
	if err := r.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "listing the AgentPlatformOrganizations failed", "namespace", namespace)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func (r *OrganizationReconciler) organizationsOfBinding(ctx context.Context, o client.Object) []reconcile.Request {
	var reqs []reconcile.Request
	if rb, ok := o.(*rbacv1.RoleBinding); ok && isMirroredAdmin(rb) {
		reqs = r.organizationsOf(ctx, rb.Namespace)
	}
	// Any binding in an organization namespace: the reconciler's own, or
	// one that holds a name the reconciler needs.
	return append(reqs, r.organizationOfNamespace(ctx, o)...)
}

// organizationsOfCopy maps an object of the platform namespace (a network
// policy, a curated object) to every organization and a copy to its own.
func (r *OrganizationReconciler) organizationsOfCopy(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() == r.Config.PlatformNamespace {
		return r.organizationsOf(ctx, "")
	}
	if _, ok := o.GetLabels()[ObjectLabel]; ok {
		return r.organizationOfNamespace(ctx, o)
	}
	return nil
}

// organizationsAdminRoleRef is the roleRef of every mirror.
var organizationsAdminRoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: OrganizationsAdminClusterRole}

// isMirroredAdmin reports whether a parent namespace's RoleBinding makes its
// subjects admins of the parent's organizations: a binding to
// organizations-admin, or a binding to cluster-admin that rbac-operator wrote
// (rbacOperatorLabel). In a Giant Swarm Organization's namespace rbac-operator
// writes the cluster-admin binding of the installation's customer admin groups
// (write-all-customer-group) and a copy of every RoleBindingTemplate that
// selects the organization; a cluster-admin binding written by hand is not
// mirrored.
func isMirroredAdmin(rb *rbacv1.RoleBinding) bool {
	ref := rb.RoleRef
	if ref.APIGroup != rbacv1.GroupName || ref.Kind != kindClusterRole {
		return false
	}
	return ref.Name == OrganizationsAdminClusterRole ||
		(ref.Name == clusterAdminClusterRole && rb.Labels[rbacOperatorLabel] == rbacOperatorValue)
}

// Reconcile keeps one organization.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	org := &v1alpha1.AgentPlatformOrganization{}
	if err := r.Client.Get(ctx, req.NamespacedName, org); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if org.DeletionTimestamp != nil {
		return r.erase(ctx, org)
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
	status := org.Status.DeepCopy()
	status.SnapshotPrefix = r.snapshotPrefix(org)
	// First: the worker replica rule reads it.
	orphanedErr := r.setOrphaned(ctx, org, status)

	ready, err := readyCondition(errors.Join(orphanedErr, r.reconcile(ctx, org, status)), org.Generation)
	meta.SetStatusCondition(&status.Conditions, ready)
	if !equality.Semantic.DeepEqual(&org.Status, status) {
		org.Status = *status
		if uerr := r.Client.Status().Update(ctx, org, client.FieldOwner(fieldManager)); uerr != nil {
			return reconcile.Result{}, errors.Join(err, fmt.Errorf("update status: %w", uerr))
		}
	}
	return reconcile.Result{}, err
}

// reconcile finds or creates the organization namespace, records it in
// status and keeps the objects in it.
func (r *OrganizationReconciler) reconcile(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, status *v1alpha1.AgentPlatformOrganizationStatus) error {
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
	errs = append(errs, r.keepServiceAccountBindings(ctx, ns.Name))
	if !r.Config.NetworkPolicyGVK.Empty() {
		for _, name := range r.Config.NetworkPolicies {
			errs = append(errs, r.copyObject(ctx, r.Config.NetworkPolicyGVK, name, ns.Name))
		}
	}
	errs = append(errs, r.mirrorAdmins(ctx, org.Namespace, ns.Name))
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

// namespace is the organization namespace: the one labelled with the
// organization's UID, read from the API server, or a new one.
func (r *OrganizationReconciler) namespace(ctx context.Context, org *v1alpha1.AgentPlatformOrganization) (*corev1.Namespace, error) {
	list := &corev1.NamespaceList{}
	if err := r.APIReader.List(ctx, list, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}); err != nil {
		return nil, fmt.Errorf("list the namespaces labelled %s=%s: %w", v1alpha1.OrganizationUIDLabel, org.UID, err)
	}
	switch len(list.Items) {
	case 0:
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		return nil, fmt.Errorf("more than one namespace is labelled %s=%s: %s", v1alpha1.OrganizationUIDLabel, org.UID, strings.Join(names, ", "))
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: NamespacePrefix + org.Name + "-",
		Labels: map[string]string{
			v1alpha1.OrganizationLabel:    org.Name,
			v1alpha1.ParentLabel:          org.Namespace,
			v1alpha1.OrganizationUIDLabel: string(org.UID),
			managedByLabel:                managedByValue,
		},
	}}
	if err := r.Client.Create(ctx, ns, client.FieldOwner(fieldManager)); err != nil {
		return nil, fmt.Errorf("create the organization namespace: %w", err)
	}
	logf.FromContext(ctx).Info("organization namespace created", "namespace", ns.Name)
	return ns, nil
}

func (r *OrganizationReconciler) snapshotPrefix(org *v1alpha1.AgentPlatformOrganization) string {
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
