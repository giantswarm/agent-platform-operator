package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/giantswarm/agent-platform-operator/pkg/adminmirror"
)

// ChartSource is the agent chart's OCIRepository, as agent-manager composes
// it (agents.BuildOCIRepository): an organization's HelmReleases reference it
// by the chart's name, the last segment of URL.
type ChartSource struct {
	URL          string
	Semver       string
	SemverFilter string
	Interval     string
	APIVersion   string
}

// The OCIRepository fields ChartSource derives.
const (
	fieldURL      = "url"
	fieldInterval = "interval"
	fieldRef      = "ref"
	fieldSemver   = "semver"
)

// Name is the OCIRepository's name: the chart's.
func (c ChartSource) Name() string { return path.Base(c.URL) }

func (c ChartSource) object(ns string) *unstructured.Unstructured {
	ref := map[string]any{fieldSemver: c.Semver}
	if c.SemverFilter != "" {
		ref["semverFilter"] = c.SemverFilter
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": c.APIVersion,
		fieldKind:    "OCIRepository",
		"metadata":   map[string]any{fieldName: c.Name(), fieldNamespace: ns},
		"spec": map[string]any{
			fieldInterval: c.Interval,
			fieldURL:      c.URL,
			fieldRef:      ref,
		},
	}}
}

// ---- unstructured objects -------------------------------------------------

// object is an empty object of gvk.
func object(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

// ownLabels are the labels of what the reconciler writes.
func ownLabels(scope string) map[string]string {
	return map[string]string{ObjectLabel: scope, managedByLabel: managedByValue}
}

// stamp marks obj as the reconciler's own, of scope, in ns.
func stamp(obj *unstructured.Unstructured, ns, scope, from string) map[string]string {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	for k, v := range ownLabels(scope) {
		l[k] = v
	}
	obj.SetLabels(l)
	if from != "" {
		obj.SetAnnotations(map[string]string{CopiedFromAnnotation: from})
	}
	obj.SetNamespace(ns)
	return l
}

// create writes obj. An object of that name that exists already is left as
// it is: it is another writer's, or the reconciler's own that the read did
// not see yet, whose event reconciles the organization again.
func (r *OrganizationReconciler) create(ctx context.Context, obj client.Object, kind string) error {
	err := r.Client.Create(ctx, obj, client.FieldOwner(fieldManager))
	switch {
	case apierrors.IsAlreadyExists(err):
		logf.FromContext(ctx).V(1).Info("leaving an object the reconciler did not write", "namespace", obj.GetNamespace(), "kind", kind, "name", obj.GetName())
		return nil
	case err != nil:
		return fmt.Errorf("create %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	logf.FromContext(ctx).Info("organization object created", "namespace", obj.GetNamespace(), "kind", kind, "name", obj.GetName())
	return nil
}

// copyObject copies a platform object of gvk into ns. A missing source makes
// the organization not Ready; its creation reconciles the organization again.
func (r *OrganizationReconciler) copyObject(ctx context.Context, gvk schema.GroupVersionKind, name, ns string) error {
	src, err := r.platformObject(ctx, gvk, name)
	if err != nil {
		return err
	}
	return r.ensureScoped(ctx, ns, ScopeNamespace, copyOf(src, ns), objectRef(src), contains)
}

// copyOf is src's spec under the same name in ns, with no metadata of src's.
func copyOf(src *unstructured.Unstructured, ns string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": src.GetAPIVersion(),
		"kind":       src.GetKind(),
		"metadata":   map[string]any{fieldName: src.GetName(), "namespace": ns},
	}}
	if spec, ok := src.Object["spec"]; ok {
		obj.Object["spec"] = runtime.DeepCopyJSONValue(spec)
	}
	return obj
}

// ensureScoped creates obj as the reconciler's own of scope, or brings its
// own copy back to it when same(copy's spec, obj's spec) is false or its
// labels differ. An object of that name the reconciler did not write is left
// alone; for a curated copy that makes the organization not Ready, since the
// name is the curated object's.
func (r *OrganizationReconciler) ensureScoped(ctx context.Context, ns, scope string, obj *unstructured.Unstructured, from string, same func(have, want any) bool) error {
	want := stamp(obj, ns, scope, from)
	kind := obj.GetKind()
	existing := object(obj.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		if scope != ScopeCurated {
			return r.create(ctx, obj, kind)
		}
		// Other copies refer to a curated copy by name: an object of that
		// name created since the read is never taken for it.
		err = r.Client.Create(ctx, obj, client.FieldOwner(fieldManager))
		switch {
		case err == nil:
			logf.FromContext(ctx).Info("organization object created", "namespace", ns, "kind", kind, "name", obj.GetName())
			return nil
		case !apierrors.IsAlreadyExists(err):
			return fmt.Errorf("create %s %s/%s: %w", kind, ns, obj.GetName(), err)
		}
		err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), existing)
		if err == nil && existing.GetLabels()[ObjectLabel] == scope {
			return fmt.Errorf("create %s %s/%s: another reconcile created it; it is checked again on the next", kind, ns, obj.GetName())
		}
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", kind, ns, obj.GetName(), err)
	}
	if existing.GetLabels()[ObjectLabel] != scope {
		if scope == ScopeCurated {
			return &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the %s %s/%s is not the operator's copy of %s, so the operator cannot write it", kind, ns, obj.GetName(), from)}
		}
		logf.FromContext(ctx).V(1).Info("leaving an object the reconciler did not write", "namespace", ns, "kind", kind, "name", obj.GetName())
		return nil
	}
	if scope == ScopeCurated && existing.GetDeletionTimestamp() != nil {
		// A copy that is going is no copy to refer to: what refers to it
		// waits until it is gone and copied again.
		return fmt.Errorf("the %s %s/%s is being deleted; it is copied again once it is gone", kind, ns, obj.GetName())
	}
	merged := copiedLabels(existing.GetLabels(), want, scope == ScopeCurated)
	if same(existing.Object["spec"], obj.Object["spec"]) && maps.Equal(merged, existing.GetLabels()) {
		return nil
	}
	existing.Object["spec"] = obj.Object["spec"]
	existing.SetLabels(merged)
	if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update %s %s/%s: %w", kind, ns, obj.GetName(), err)
	}
	logf.FromContext(ctx).Info("organization object updated", "namespace", ns, "kind", kind, "name", obj.GetName())
	return nil
}

// ensureChartSource keeps the agent chart's OCIRepository in ns at
// Config.Chart. It creates it, and on every reconcile brings the fields the
// operator derives (spec.url, spec.interval and the whole spec.ref) back to
// the configured ones, so a platform release that moves the chart (another
// registry, range or filter) reaches every organization, not only new ones.
// Every other spec field, label and annotation stays as it is, and a source
// at the configured spec is not written. An object of that name the operator
// did not write is left alone. No organization pins its own chart: the
// AgentPlatformOrganization has no such field, and only the operator writes
// the source.
func (r *OrganizationReconciler) ensureChartSource(ctx context.Context, ns string) error {
	want := r.Config.Chart.object(ns)
	wantLabels := stamp(want, ns, ScopeNamespace, "")
	kind := want.GetKind()
	existing := object(want.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.create(ctx, want, kind)
	case err != nil:
		return fmt.Errorf("get %s %s/%s: %w", kind, ns, want.GetName(), err)
	}
	if existing.GetLabels()[ObjectLabel] != ScopeNamespace {
		logf.FromContext(ctx).V(1).Info("leaving an object the reconciler did not write", "namespace", ns, "kind", kind, "name", want.GetName())
		return nil
	}
	spec, _ := existing.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	wantSpec, _ := want.Object["spec"].(map[string]any)
	labels := copiedLabels(existing.GetLabels(), wantLabels, false)
	same := maps.Equal(labels, existing.GetLabels())
	for k, v := range wantSpec {
		same = same && reflect.DeepEqual(spec[k], v)
	}
	if same {
		return nil
	}
	from := map[string]any{fieldURL: spec[fieldURL], fieldRef: spec[fieldRef], fieldInterval: spec[fieldInterval]}
	maps.Copy(spec, wantSpec)
	existing.Object["spec"] = spec
	existing.SetLabels(labels)
	if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update %s %s/%s: %w", kind, ns, want.GetName(), err)
	}
	logf.FromContext(ctx).Info("organization chart source updated", "namespace", ns, "kind", kind, "name", want.GetName(),
		"from", from, "to", wantSpec)
	return nil
}

// contains reports whether every field of want is in have with the same
// value: the API server adds defaults to what was written, which is no drift.
func contains(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !contains(h[k], v) {
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
			if !contains(h[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(have, want)
	}
}

func subset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// copiedLabels are the labels an object holding have gets: want on top of
// have. With takesSourceKagentLabels, as for a curated copy, the labels the
// copy takes from its source (kagentLabelPrefix) are want's only, so a label
// the source lost goes; labels other writers add stay.
func copiedLabels(have, want map[string]string, takesSourceKagentLabels bool) map[string]string {
	merged := make(map[string]string, len(have)+len(want))
	for k, v := range have {
		if takesSourceKagentLabels && strings.HasPrefix(k, kagentLabelPrefix) {
			continue
		}
		merged[k] = v
	}
	maps.Copy(merged, want)
	return merged
}

// ---- typed objects ----------------------------------------------------------

// ensureServiceAccount creates the ServiceAccount, the reconciler's own of
// scope; one that exists is kept as it is (a ServiceAccount has nothing to
// drift). A curated one is the workers' identity, so one of that name that is
// not curated, another writer's or the reconciler's own of another scope,
// makes the organization not Ready. The cache holds only ServiceAccounts
// labelled ObjectLabel, so a name it misses that create finds taken is read
// from the API server.
func (r *OrganizationReconciler) ensureServiceAccount(ctx context.Context, ns, name, scope string) error {
	key := client.ObjectKey{Namespace: ns, Name: name}
	existing := &corev1.ServiceAccount{}
	err := r.Client.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: ownLabels(scope)}}
		if scope != ScopeCurated {
			return r.create(ctx, sa, "ServiceAccount")
		}
		err = r.Client.Create(ctx, sa, client.FieldOwner(fieldManager))
		if err == nil {
			logf.FromContext(ctx).Info("organization object created", "namespace", ns, "kind", "ServiceAccount", "name", name)
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ServiceAccount %s/%s: %w", ns, name, err)
		}
		err = r.APIReader.Get(ctx, key, existing)
	}
	switch {
	case err != nil:
		return fmt.Errorf("get ServiceAccount %s/%s: %w", ns, name, err)
	case scope == ScopeCurated && existing.Labels[ObjectLabel] != scope:
		return &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the ServiceAccount %s/%s is not the operator's curated one, so the curated WorkerPool's workers cannot run as it", ns, name)}
	}
	return nil
}

// ensureHelmRoleBinding binds the Helm ClusterRole to the Helm
// ServiceAccount in ns.
func (r *OrganizationReconciler) ensureHelmRoleBinding(ctx context.Context, ns string) error {
	name := r.Config.HelmServiceAccount
	return r.ensureRoleBinding(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: ownLabels(ScopeNamespace)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: r.Config.HelmClusterRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: ns}},
	})
}

// ensureRoleBinding creates want, or brings the reconciler's own binding of
// the same scope back to it: its subjects and the copied-from annotation;
// annotations of other writers are kept. A RoleBinding's roleRef cannot change, so the
// reconciler's own binding to another role is replaced.
func (r *OrganizationReconciler) ensureRoleBinding(ctx context.Context, want *rbacv1.RoleBinding) error {
	ns, name := want.Namespace, want.Name
	existing := &rbacv1.RoleBinding{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get RoleBinding %s/%s: %w", ns, name, err)
	case existing.Labels[ObjectLabel] != want.Labels[ObjectLabel]:
		// Another object holds the name: it is left as it is, and the
		// organization is not Ready until the name is free.
		return &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the RoleBinding %s/%s is not the operator's %s binding, so the operator cannot write it", ns, name, want.Labels[ObjectLabel])}
	case existing.RoleRef == want.RoleRef && reflect.DeepEqual(existing.Subjects, want.Subjects) && existing.Annotations[CopiedFromAnnotation] == want.Annotations[CopiedFromAnnotation]:
		return nil
	case existing.RoleRef == want.RoleRef:
		existing.Subjects = want.Subjects
		// Only the reconciler's own annotation: another writer's stay.
		if from := want.Annotations[CopiedFromAnnotation]; from != "" {
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[CopiedFromAnnotation] = from
		} else {
			delete(existing.Annotations, CopiedFromAnnotation)
		}
		if err := r.Client.Update(ctx, existing, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("update RoleBinding %s/%s: %w", ns, name, err)
		}
		logf.FromContext(ctx).Info("organization object updated", "namespace", ns, "kind", "RoleBinding", "name", name)
		return nil
	default:
		if err := r.Client.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("replace RoleBinding %s/%s: %w", ns, name, err)
		}
	}
	return r.create(ctx, want, "RoleBinding")
}

// mirrorAdmins keeps in ns a mirror of every admin binding of parent that
// counts now (adminmirror.Mirrors: one being deleted does not): the same
// name, the roleRef organizations-admin, and the subjects
// adminmirror.MirroredSubjects keeps: Users and Groups whose names do not
// start with system:. ServiceAccounts are never mirrored, so the parent's
// automation cannot bind itself into every organization, and system: names
// are the API server's own identities. A mirror whose parent binding is gone,
// being deleted, or left without subjects, is deleted.
func (r *OrganizationReconciler) mirrorAdmins(ctx context.Context, parent, ns string) error {
	parents := &rbacv1.RoleBindingList{}
	if err := r.Client.List(ctx, parents, client.InNamespace(parent)); err != nil {
		return fmt.Errorf("list RoleBindings in %s: %w", parent, err)
	}
	want := map[string]*rbacv1.RoleBinding{}
	var errs []error
	for i := range parents.Items {
		pb := &parents.Items[i]
		if !adminmirror.Mirrors(pb) {
			continue
		}
		if pb.Name == FluxRoleBinding && r.Config.Flux.commitMode() {
			// The name of the organization's Flux binding: the mirror would
			// take it and leave the organization without its Kustomization.
			errs = append(errs, &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the RoleBinding %s/%s has the name the operator keeps for the organization's Flux binding, so it is not mirrored; rename it", parent, pb.Name)})
			continue
		}
		if r.boundClusterRole(pb.Name) {
			// The name of a ServiceAccount binding: the mirror would take it
			// and leave that ServiceAccount without its ClusterRole here.
			errs = append(errs, &notReadyError{reason: reasonNameTaken, message: fmt.Sprintf("the RoleBinding %s/%s has the name the operator keeps for its binding of the ClusterRole %s, so it is not mirrored; rename it", parent, pb.Name, pb.Name)})
			continue
		}
		subjects := adminmirror.MirroredSubjects(pb.Subjects)
		if len(subjects) == 0 {
			continue
		}
		want[pb.Name] = &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: pb.Name, Namespace: ns,
				Labels:      ownLabels(ScopeAdminMirror),
				Annotations: map[string]string{CopiedFromAnnotation: parent + "/" + pb.Name},
			},
			// Never the parent's roleRef: an rbac-operator binding refers
			// to cluster-admin, which the operator may not bind.
			RoleRef:  organizationsAdminRoleRef,
			Subjects: subjects,
		}
	}
	mirrors := &rbacv1.RoleBindingList{}
	if err := r.Client.List(ctx, mirrors, client.InNamespace(ns), client.MatchingLabels{ObjectLabel: ScopeAdminMirror}); err != nil {
		errs = append(errs, fmt.Errorf("list RoleBindings in %s: %w", ns, err))
	}
	for i := range mirrors.Items {
		m := &mirrors.Items[i]
		if _, ok := want[m.Name]; ok {
			continue
		}
		if err := r.Client.Delete(ctx, m); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("delete RoleBinding %s/%s: %w", ns, m.Name, err))
			continue
		}
		logf.FromContext(ctx).Info("organization object deleted", "namespace", ns, "kind", "RoleBinding", "name", m.Name)
	}
	for _, rb := range want {
		errs = append(errs, r.ensureRoleBinding(ctx, rb))
	}
	return errors.Join(errs...)
}
