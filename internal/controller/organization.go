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
//   - a mirror of every RoleBinding of the parent namespace to the ClusterRole
//     agent-platform:organizations-admin, with its User and Group subjects
//     only, removed when the parent binding goes.
//
// The logic of the first three is agent-manager's tenant reconciler
// (internal/tenants): it writes only objects it labels as its own and never
// changes or deletes one it did not write.
package controller

import (
	"context"
	"errors"
	"fmt"
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
	// NamespacePrefix starts the generateName of an organization namespace.
	NamespacePrefix = "agents-"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "agent-platform-operator"
	fieldManager   = "agent-platform-operator"

	reasonReconciled           = "Reconciled"
	reasonReconcileFailed      = "ReconcileFailed"
	reasonNamespaceTerminating = "NamespaceTerminating"
	reasonSourceMissing        = "SourceMissing"
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
	// SnapshotBucket is the bucket of the snapshot prefix s3://<bucket>/<uid>;
	// empty leaves the prefix empty.
	SnapshotBucket string
}

// CacheOptions limit what the manager caches: the organization namespaces,
// the reconciler's own ServiceAccounts, every RoleBinding (a parent's
// organizations-admin bindings carry no label to select them by) and, of the
// network policies, the platform namespace's and the reconciler's copies.
func CacheOptions(cfg Config, syncPeriod time.Duration) cache.Options {
	byObject := map[client.Object]cache.ByObject{
		&corev1.Namespace{}:      {Label: hasLabel(v1alpha1.OrganizationUIDLabel)},
		&corev1.ServiceAccount{}: {Label: hasLabel(ObjectLabel)},
	}
	if !cfg.NetworkPolicyGVK.Empty() {
		byObject[object(cfg.NetworkPolicyGVK)] = cache.ByObject{Namespaces: map[string]cache.Config{
			cfg.PlatformNamespace: {},
			cache.AllNamespaces:   {LabelSelector: hasLabel(ObjectLabel)},
		}}
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
	Config    Config
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
		Watches(&corev1.ServiceAccount{}, inOrganization, builder.WithPredicates(isOwn)).
		// A parent's organizations-admin binding reconciles the parent's
		// organizations; a mirror its own.
		Watches(&rbacv1.RoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.organizationsOfBinding))
	if !r.Config.NetworkPolicyGVK.Empty() {
		specOrLabels := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
		b = b.Watches(object(r.Config.NetworkPolicyGVK), handler.EnqueueRequestsFromMapFunc(r.organizationsOfPolicy), builder.WithPredicates(specOrLabels))
	}
	return b.Complete(r)
}

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
	if rb, ok := o.(*rbacv1.RoleBinding); ok && isOrganizationsAdmin(rb.RoleRef) {
		reqs = r.organizationsOf(ctx, rb.Namespace)
	}
	if _, ok := o.GetLabels()[ObjectLabel]; ok {
		reqs = append(reqs, r.organizationOfNamespace(ctx, o)...)
	}
	return reqs
}

// organizationsOfPolicy maps a platform network policy to every organization
// and a copy to its own.
func (r *OrganizationReconciler) organizationsOfPolicy(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() == r.Config.PlatformNamespace {
		return r.organizationsOf(ctx, "")
	}
	if _, ok := o.GetLabels()[ObjectLabel]; ok {
		return r.organizationOfNamespace(ctx, o)
	}
	return nil
}

func isOrganizationsAdmin(ref rbacv1.RoleRef) bool {
	return ref.APIGroup == rbacv1.GroupName && ref.Kind == "ClusterRole" && ref.Name == OrganizationsAdminClusterRole
}

// Reconcile keeps one organization.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	org := &v1alpha1.AgentPlatformOrganization{}
	if err := r.Client.Get(ctx, req.NamespacedName, org); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if org.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	status := org.Status.DeepCopy()
	status.SnapshotPrefix = r.snapshotPrefix(org)

	err := r.reconcile(ctx, org, status)
	ready := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: reasonReconciled, ObservedGeneration: org.Generation}
	failed, notReady := split(err)
	err = errors.Join(failed...)
	switch {
	case err != nil:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, reasonReconcileFailed, err.Error()
	case len(notReady) > 0:
		// Not an error to retry: the event that changes the cause (the
		// namespace's deletion, the source's creation) reconciles the
		// organization again.
		messages := make([]string, 0, len(notReady))
		for _, nr := range notReady {
			messages = append(messages, nr.message)
		}
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, notReady[0].reason, strings.Join(messages, "; ")
	}
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
		errs = append(errs, r.ensureServiceAccount(ctx, ns.Name, sa), r.ensureHelmRoleBinding(ctx, ns.Name))
	}
	if !r.Config.NetworkPolicyGVK.Empty() {
		for _, name := range r.Config.NetworkPolicies {
			errs = append(errs, r.copyObject(ctx, r.Config.NetworkPolicyGVK, name, ns.Name))
		}
	}
	errs = append(errs, r.mirrorAdmins(ctx, org.Namespace, ns.Name))
	return errors.Join(errs...)
}

// notReadyError is a cause of Ready=False that a retry does not change: the
// reconcile has done what it can.
type notReadyError struct{ reason, message string }

func (e *notReadyError) Error() string { return e.message }

// split sorts the errors joined in err into failures to retry and causes of
// Ready=False.
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
	var nr *notReadyError
	if errors.As(err, &nr) {
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
