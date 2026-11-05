package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ScopeServiceAccountBinding marks, as the value of ObjectLabel, a
// RoleBinding that binds a ServiceAccountBinding's ClusterRole in an
// organization namespace. One whose ClusterRole is no longer configured is
// deleted.
const ScopeServiceAccountBinding = "service-account-binding"

// ServiceAccountBinding is a ClusterRole bound to a ServiceAccount of another
// namespace in every organization namespace, by a RoleBinding named after the
// ClusterRole: the ServiceAccount holds the ClusterRole's rules there and in
// no namespace it is not bound in. Substrate's credential providers read the
// Secret an actor's credential names in the organization namespace this way.
type ServiceAccountBinding struct {
	ClusterRole    string               `json:"clusterRole"`
	ServiceAccount types.NamespacedName `json:"serviceAccount"`
}

// ParseServiceAccountBindings reads a JSON list of
// {"clusterRole": ..., "serviceAccount": {"namespace": ..., "name": ...}};
// empty is none. The RoleBinding is named after its ClusterRole, so a
// ClusterRole is listed once.
func ParseServiceAccountBindings(s string) ([]ServiceAccountBinding, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []ServiceAccountBinding
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("not a JSON list of {clusterRole, serviceAccount: {namespace, name}}: %w", err)
	}
	if dec.More() {
		return nil, errors.New("not a JSON list of {clusterRole, serviceAccount: {namespace, name}}: data after the list")
	}
	seen := map[string]bool{}
	for _, b := range out {
		// RBAC object names are path segments: a ClusterRole name is a valid
		// RoleBinding name.
		if b.ClusterRole == "" {
			return nil, fmt.Errorf("ServiceAccount %s binds no ClusterRole", b.ServiceAccount)
		}
		if msgs := content.IsPathSegmentName(b.ClusterRole); len(msgs) > 0 {
			return nil, fmt.Errorf("ClusterRole %q: %s", b.ClusterRole, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsDNS1123Label(b.ServiceAccount.Namespace); len(msgs) > 0 {
			return nil, fmt.Errorf("ClusterRole %q: namespace %q: %s", b.ClusterRole, b.ServiceAccount.Namespace, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsDNS1123Subdomain(b.ServiceAccount.Name); len(msgs) > 0 {
			return nil, fmt.Errorf("ClusterRole %q: ServiceAccount %q: %s", b.ClusterRole, b.ServiceAccount.Name, strings.Join(msgs, "; "))
		}
		if seen[b.ClusterRole] {
			return nil, fmt.Errorf("ClusterRole %q is listed twice: its RoleBinding is named after it", b.ClusterRole)
		}
		seen[b.ClusterRole] = true
	}
	return out, nil
}

// boundClusterRole reports whether name is the RoleBinding name of a
// configured ServiceAccountBinding.
func (r *OrganizationReconciler) boundClusterRole(name string) bool {
	for _, b := range r.Config.ServiceAccountBindings {
		if b.ClusterRole == name {
			return true
		}
	}
	return false
}

// keepServiceAccountBindings keeps in ns one RoleBinding per configured
// ServiceAccountBinding, named after its ClusterRole, and deletes the
// reconciler's bindings of a ClusterRole no longer configured.
func (r *OrganizationReconciler) keepServiceAccountBindings(ctx context.Context, ns string) error {
	var errs []error
	for _, b := range r.Config.ServiceAccountBindings {
		errs = append(errs, r.ensureRoleBinding(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: b.ClusterRole, Namespace: ns, Labels: ownLabels(ScopeServiceAccountBinding)},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: b.ClusterRole},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: b.ServiceAccount.Name, Namespace: b.ServiceAccount.Namespace,
			}},
		}))
	}
	have := &rbacv1.RoleBindingList{}
	if err := r.Client.List(ctx, have, client.InNamespace(ns), client.MatchingLabels{ObjectLabel: ScopeServiceAccountBinding}); err != nil {
		return errors.Join(append(errs, fmt.Errorf("list RoleBindings in %s: %w", ns, err))...)
	}
	for i := range have.Items {
		rb := &have.Items[i]
		if r.boundClusterRole(rb.Name) {
			continue
		}
		if err := r.Client.Delete(ctx, rb); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("delete RoleBinding %s/%s: %w", ns, rb.Name, err))
			continue
		}
		logf.FromContext(ctx).Info("organization object deleted", "namespace", ns, "kind", "RoleBinding", "name", rb.Name)
	}
	return errors.Join(errs...)
}
