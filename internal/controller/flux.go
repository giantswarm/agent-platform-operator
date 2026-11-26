package controller

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

const (
	// ScopeFlux marks, as the value of ObjectLabel, what the reconciler keeps
	// for an organization in the Flux namespace: its Kustomization and its
	// Flux ServiceAccount.
	ScopeFlux = "flux"
	// FluxRoleBinding is the RoleBinding in the organization namespace that
	// binds the organization's Flux ServiceAccount to FluxConfig.ClusterRole.
	FluxRoleBinding = "organization-flux"
)

// FluxConfig is commit mode: where an organization's contents come from git,
// applied by a Flux Kustomization of its own, and how the operator tells an
// organization whose file is gone from git.
type FluxConfig struct {
	// Namespace is the namespace of the installation's Flux source and SOPS
	// key, where the organization's Kustomization and Flux ServiceAccount
	// live, labelled v1alpha1.OrganizationUIDLabel with its UID (the label
	// the agent-platform chart's admission policy holds the operator's writes
	// there to). Empty: the installation has none.
	Namespace string
	// Kustomization is the served kind of Flux's Kustomization.
	Kustomization schema.GroupVersionKind
	// SourceKind and SourceName are the installation's Flux source in
	// Namespace (a GitRepository or an OCIRepository). An empty SourceName
	// creates no Kustomization and no Flux ServiceAccount.
	SourceKind string
	SourceName string
	// DecryptionSecret is the installation's SOPS key Secret in Namespace;
	// empty decrypts nothing.
	DecryptionSecret string
	// Path is the organization's path in the source, with {name} replaced
	// by its name.
	Path string
	// Interval is the Kustomization's spec.interval.
	Interval string
	// ClusterRole is what the organization's Flux ServiceAccount is bound to
	// in the organization namespace, and nowhere else.
	ClusterRole string
	// Installation is the installation's Kustomization that applies the
	// Organizations. A name turns the Orphaned condition on, for an
	// organization that any Kustomization applied, and the operator is not
	// ready while this one cannot be read. An empty name sets no Orphaned
	// condition.
	Installation types.NamespacedName
}

// commitMode reports whether the reconciler keeps a Kustomization and a Flux
// ServiceAccount per organization.
func (f FluxConfig) commitMode() bool { return f.Namespace != "" && f.SourceName != "" }

// orphans reports whether the reconciler sets the Orphaned condition.
func (f FluxConfig) orphans() bool { return f.Installation.Name != "" }

// appliedBy is the Flux Kustomization whose ownership labels o carries: the
// one that applied it from git. False when it carries neither or one only.
func appliedBy(o client.Object) (types.NamespacedName, bool) {
	l := o.GetLabels()
	k := types.NamespacedName{Namespace: l[v1alpha1.FluxNamespaceLabel], Name: l[v1alpha1.FluxNameLabel]}
	return k, k.Namespace != "" && k.Name != ""
}

// watched reports whether the reconciler watches Kustomizations.
func (f FluxConfig) watched() bool {
	return !f.Kustomization.Empty() && (f.commitMode() || f.orphans())
}

// path is the organization's path in the source.
func (f FluxConfig) path(org *orgv1.Organization) string {
	return strings.ReplaceAll(f.Path, "{name}", org.Name)
}

// fluxObjectLabels are the labels of the organization's objects in the Flux
// namespace: the erasure's selector (the UID), the organization (the
// watch's), and the reconciler's own.
func fluxObjectLabels(org *orgv1.Organization) map[string]string {
	l := ownLabels(ScopeFlux)
	l[v1alpha1.OrganizationUIDLabel] = string(org.UID)
	l[v1alpha1.OrganizationLabel] = org.Name
	return l
}

// keepFlux keeps the organization's Flux ServiceAccount in the Flux
// namespace, its RoleBinding in the organization namespace ns, and its
// Kustomization, which applies the organization's path into ns as that
// ServiceAccount. Both objects in the Flux namespace are named ns: unique,
// and agents-* like every organization Kustomization.
func (r *OrganizationReconciler) keepFlux(ctx context.Context, org *orgv1.Organization, ns string) error {
	f := r.Config.Flux
	if err := r.ensureFluxServiceAccount(ctx, org, ns); err != nil {
		return err
	}
	err := r.ensureRoleBinding(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: FluxRoleBinding, Namespace: ns, Labels: ownLabels(ScopeNamespace)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: f.ClusterRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ns, Namespace: f.Namespace}},
	})
	if err != nil {
		return err
	}
	return r.ensureKustomization(ctx, org, ns)
}

// ensureFluxServiceAccount creates the organization's Flux ServiceAccount. It
// is read from the cache, which holds only labelled ServiceAccounts, and a
// miss is confirmed from the API server: a ServiceAccount of that name without
// the organization's UID is another writer's, and binding it would give its
// holder the organization.
func (r *OrganizationReconciler) ensureFluxServiceAccount(ctx context.Context, org *orgv1.Organization, name string) error {
	ns := r.Config.Flux.Namespace
	existing := &corev1.ServiceAccount{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	}
	switch {
	case apierrors.IsNotFound(err):
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: fluxObjectLabels(org)}}
		if err := r.Client.Create(ctx, sa, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("create ServiceAccount %s/%s: %w", ns, name, err)
		}
		logf.FromContext(ctx).Info("organization object created", "namespace", ns, "kind", "ServiceAccount", "name", name)
		return nil
	case err != nil:
		return fmt.Errorf("get ServiceAccount %s/%s: %w", ns, name, err)
	case existing.Labels[v1alpha1.OrganizationUIDLabel] != string(org.UID):
		return nameTaken("ServiceAccount", ns, name)
	case subset(existing.Labels, fluxObjectLabels(org)):
		return nil
	}
	// Its labels back: without them the cache and the watch lose it.
	for k, v := range fluxObjectLabels(org) {
		existing.Labels[k] = v
	}
	if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update ServiceAccount %s/%s: %w", ns, name, err)
	}
	logf.FromContext(ctx).Info("organization object updated", "namespace", ns, "kind", "ServiceAccount", "name", name)
	return nil
}

// ensureKustomization creates the organization's Kustomization, or brings
// its spec and labels back to what the reconciler keeps; fields the API
// server defaults, the status and other writers' metadata stay. It is read
// from the cache, and a miss is confirmed from the API server: without the
// Orphaned condition the cache holds only labelled Kustomizations, so one of
// that name without the labels would read as missing.
func (r *OrganizationReconciler) ensureKustomization(ctx context.Context, org *orgv1.Organization, name string) error {
	f := r.Config.Flux
	want := r.kustomization(org, name)
	existing := object(f.Kustomization)
	err := r.cacheReader().Get(ctx, client.ObjectKeyFromObject(want), existing)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(want), existing)
	}
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, want, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("create Kustomization %s/%s: %w", f.Namespace, name, err)
		}
		logf.FromContext(ctx).Info("organization object created", "namespace", f.Namespace, "kind", "Kustomization", "name", name)
		return nil
	case err != nil:
		return fmt.Errorf("get Kustomization %s/%s: %w", f.Namespace, name, err)
	case existing.GetLabels()[v1alpha1.OrganizationUIDLabel] != string(org.UID):
		return nameTaken("Kustomization", f.Namespace, name)
	case sameSpec(existing.Object["spec"], want.Object["spec"]) && subset(existing.GetLabels(), want.GetLabels()):
		return nil
	}
	existing.Object["spec"] = want.Object["spec"]
	labels := existing.GetLabels()
	for k, v := range want.GetLabels() {
		labels[k] = v
	}
	existing.SetLabels(labels)
	if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update Kustomization %s/%s: %w", f.Namespace, name, err)
	}
	logf.FromContext(ctx).Info("organization object updated", "namespace", f.Namespace, "kind", "Kustomization", "name", name)
	return nil
}

// kustomization is the organization's Kustomization: it applies the
// organization's path into its namespace ns, as its Flux ServiceAccount
// (Flux impersonates spec.serviceAccountName), so a merged file can write
// only what that ServiceAccount's one RoleBinding allows. It prunes, and on
// deletion waits until what it applied is gone; it is never suspended, since
// a suspended Kustomization skips its garbage collection on delete.
func (r *OrganizationReconciler) kustomization(org *orgv1.Organization, ns string) *unstructured.Unstructured {
	f := r.Config.Flux
	spec := map[string]any{
		"interval":           f.Interval,
		"path":               f.path(org),
		"prune":              true,
		"deletionPolicy":     "WaitForTermination",
		"suspend":            false,
		"sourceRef":          map[string]any{fieldKind: f.SourceKind, fieldName: f.SourceName, fieldNamespace: f.Namespace},
		"serviceAccountName": ns,
		"targetNamespace":    ns,
	}
	if f.DecryptionSecret != "" {
		spec["decryption"] = map[string]any{"provider": "sops", "secretRef": map[string]any{fieldName: f.DecryptionSecret}}
	}
	k := object(f.Kustomization)
	k.SetNamespace(f.Namespace)
	k.SetName(ns)
	k.SetLabels(fluxObjectLabels(org))
	k.Object["spec"] = spec
	return k
}

// sameSpec reports whether the Kustomization spec have is want, at every
// level: every field of want with its value, and no other field but a zero
// one (false, "", 0, an empty map or list), which is what the API server
// defaults. An added field (patches, postBuild, kubeConfig,
// decryption.serviceAccountName) is a change, and is reverted. The
// Kustomization CRD defaults no non-zero spec field; one that did would be
// rewritten on every reconcile, with no generation change.
func sameSpec(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !sameSpec(h[k], v) {
				return false
			}
		}
		for k, v := range h {
			if _, kept := w[k]; !kept && !zero(v) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !sameSpec(h[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(have, want)
	}
}

// zero reports whether v is a field's zero value.
func zero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case int64:
		return x == 0
	case float64:
		return x == 0
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

func nameTaken(kind, ns, name string) error {
	return &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the %s %s/%s is not the operator's for this organization, so the operator cannot write it", kind, ns, name)}
}

// setOrphaned sets the Orphaned condition in status from the inventory of the
// Kustomization whose ownership labels the organization carries, whichever
// it is. An organization without those labels was not applied from git and
// has no Orphaned condition. While the inventory cannot be read (the
// Kustomization is missing, or has not recorded one yet) the condition stays
// as it was: a passing gap must neither stop an organization's workers nor
// start an orphan's.
func (r *OrganizationReconciler) setOrphaned(ctx context.Context, org *orgv1.Organization, status *orgState) error {
	f := r.Config.Flux
	owner, fromGit := appliedBy(org)
	if !f.orphans() || !fromGit {
		// Without the installation Kustomization nothing sets it: one left
		// from an earlier configuration would stop the workers for good.
		// Without the ownership labels it was not applied from git.
		meta.RemoveStatusCondition(&status.Conditions, v1alpha1.ConditionOrphaned)
		status.LastAppliedRevision = ""
		return nil
	}
	ks := object(f.Kustomization)
	err := r.cacheReader().Get(ctx, owner, ks)
	if apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Info("the Kustomization that applied the organization is missing; Orphaned stays as it was", "kustomization", owner)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get the Kustomization %s that applied the organization: %w", owner, err)
	}
	// Flux labels an object when it applies it, and records it in the
	// inventory only once the apply and its health checks succeeded: until
	// then a new organization would read as missing from git.
	if why := unsettled(ks); why != "" {
		logf.FromContext(ctx).V(1).Info("the Kustomization that applied the organization is not settled; Orphaned stays as it was", "kustomization", owner, "why", why)
		return nil
	}
	inventory, found, err := unstructured.NestedMap(ks.Object, "status", "inventory")
	if err != nil || !found {
		logf.FromContext(ctx).V(1).Info("the Kustomization that applied the organization has no inventory; Orphaned stays as it was", "kustomization", owner)
		return nil
	}
	// An inventory without entries is empty: Flux omits an empty list.
	entries, _ := inventory["entries"].([]any)
	// Flux's inventory ID: <namespace>_<name>_<group>_<kind>, with an empty
	// namespace for the cluster-scoped Organization.
	id := "_" + org.Name + organizationEntrySuffix
	cond := metav1.Condition{
		Type: v1alpha1.ConditionOrphaned, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonInInventory,
		Message:            fmt.Sprintf("in the inventory of the Kustomization %s", owner),
		ObservedGeneration: org.Generation,
	}
	if inInventory(entries, id) {
		// A settled Kustomization's inventory is the one of its last applied
		// revision, so that revision's tree holds the organization's file.
		if revision, _, _ := unstructured.NestedString(ks.Object, "status", "lastAppliedRevision"); revision != "" {
			status.LastAppliedRevision = revision
		}
	} else {
		cond.Status, cond.Reason = metav1.ConditionTrue, v1alpha1.ReasonMissingFromGit
		cond.Message = fmt.Sprintf("missing from the inventory of the Kustomization %s: its file is gone from git; the workers are stopped", owner)
		if pr := org.GetAnnotations()[v1alpha1.DeletePullRequestAnnotation]; pr != "" {
			cond.Reason = v1alpha1.ReasonDeleteMerged
			cond.Message = fmt.Sprintf("missing from the inventory of the Kustomization %s after the delete pull request %s: pending erase; the workers are stopped", owner, pr)
		}
	}
	meta.SetStatusCondition(&status.Conditions, cond)
	return nil
}

// cacheReader reads the watched unstructured kinds (the Kustomizations) from
// the manager's cache, or from the API server without one: the client reads
// every unstructured object from the API server.
func (r *OrganizationReconciler) cacheReader() client.Reader {
	if r.Cache != nil {
		return r.Cache
	}
	return r.APIReader
}

// InstallationCheck is a readiness check: not ready while the installation
// Kustomization cannot be read, so a mistyped --installation-kustomization
// or a missing right to read Kustomizations shows on the pod, and an
// operator that comes up before Flux created it becomes ready once it
// exists. Nil without one.
func InstallationCheck(reader client.Reader, f FluxConfig) func(*http.Request) error {
	if !f.orphans() {
		return nil
	}
	return func(req *http.Request) error {
		ks := object(f.Kustomization)
		if err := reader.Get(req.Context(), f.Installation, ks); err != nil {
			return fmt.Errorf("--installation-kustomization %s: %w", f.Installation, err)
		}
		return nil
	}
}

// unsettled is why the Kustomization ks's inventory may lag what it applied,
// or empty: it is settled when it is Ready for its current generation and not
// Reconciling.
func unsettled(ks *unstructured.Unstructured) string {
	observed, _, _ := unstructured.NestedInt64(ks.Object, "status", "observedGeneration")
	if observed < ks.GetGeneration() {
		return "its generation is not observed yet"
	}
	conditions, _, _ := unstructured.NestedSlice(ks.Object, "status", "conditions")
	ready := false
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case m["type"] == "Reconciling" && m["status"] == string(metav1.ConditionTrue):
			return "it is reconciling"
		case m["type"] == "Ready" && m["status"] == string(metav1.ConditionTrue):
			ready = true
		}
	}
	if !ready {
		return "it is not Ready"
	}
	return ""
}

func inInventory(entries []any, id string) bool {
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["id"] == id {
			return true
		}
	}
	return false
}

// organizationEntrySuffix ends the inventory ID of every Organization.
var organizationEntrySuffix = "_" + orgv1.GroupVersion.Group + "_Organization"

// organizationInventory is the cache's transform of a Kustomization: the
// managed fields go, and its inventory keeps the Organizations' entries only,
// the ones the reconciler reads. A Kustomization's inventory lists every
// object it applied, and with the Orphaned condition on every Kustomization
// of the cluster is cached, since any one of them may apply an Organization.
func organizationInventory(o any) (any, error) {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return o, nil
	}
	u.SetManagedFields(nil)
	inventory, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "inventory")
	m, isMap := inventory.(map[string]any)
	if !ok || !isMap {
		return u, nil
	}
	entries, ok := m["entries"].([]any)
	if !ok {
		return u, nil
	}
	kept := []any{}
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok {
			if id, _ := entry["id"].(string); strings.HasSuffix(id, organizationEntrySuffix) {
				kept = append(kept, e)
			}
		}
	}
	m["entries"] = kept
	return u, nil
}

// organizationsOfKustomization maps a Kustomization to the organizations it
// applied (those that carry its ownership labels), and an organization's own
// Kustomization to its organization.
func (r *OrganizationReconciler) organizationsOfKustomization(ctx context.Context, o client.Object) []reconcile.Request {
	reqs := organizationOfLabels(ctx, o)
	if !r.Config.Flux.orphans() {
		return reqs
	}
	list := &orgv1.OrganizationList{}
	err := r.Client.List(ctx, list, client.MatchingLabels{v1alpha1.FluxNameLabel: o.GetName(), v1alpha1.FluxNamespaceLabel: o.GetNamespace()})
	if err != nil {
		logf.FromContext(ctx).Error(err, "listing the Organizations of a Kustomization failed", "kustomization", client.ObjectKeyFromObject(o))
		return reqs
	}
	for i := range list.Items {
		if kept(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// kustomizationChanged passes, of an organization's own Kustomization, its
// spec and label changes, and of any other Kustomization, which may apply an
// Organization, its inventory and last applied revision changes and its moves
// into and out of settled; not the rest of the status kustomize-controller
// records on every reconcile.
func kustomizationChanged() predicate.Funcs {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() || !reflect.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) {
			return true
		}
		if _, own := e.ObjectNew.GetLabels()[ObjectLabel]; own {
			return false
		}
		o, ok := e.ObjectOld.(*unstructured.Unstructured)
		n, ok2 := e.ObjectNew.(*unstructured.Unstructured)
		if !ok || !ok2 {
			return true
		}
		// kustomize-controller patches the conditions several times per
		// reconcile: only a move into or out of settled, a new inventory or
		// a new applied revision can change an organization's Orphaned
		// condition or its last applied revision.
		if (unsettled(o) == "") != (unsettled(n) == "") {
			return true
		}
		or, _, _ := unstructured.NestedString(o.Object, "status", "lastAppliedRevision")
		nr, _, _ := unstructured.NestedString(n.Object, "status", "lastAppliedRevision")
		if or != nr {
			return true
		}
		oi, _, _ := unstructured.NestedFieldNoCopy(o.Object, "status", "inventory")
		ni, _, _ := unstructured.NestedFieldNoCopy(n.Object, "status", "inventory")
		return !reflect.DeepEqual(oi, ni)
	}}
}
