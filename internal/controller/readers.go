package controller

import (
	"context"
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

// ScopeSecretReader marks, as the value of ObjectLabel, a RoleBinding that
// binds a SecretReader's ClusterRole in an organization namespace. One whose
// reader is no longer configured is deleted.
const ScopeSecretReader = "secret-reader"

// SecretReader is a ClusterRole bound to a ServiceAccount of another
// namespace in every organization namespace, so that ServiceAccount reads
// Secrets there and in no namespace it is not bound in: Substrate's
// credential providers, which read the Secret an actor's credential names
// in the organization namespace.
type SecretReader struct {
	ClusterRole    string
	ServiceAccount types.NamespacedName
}

// ParseSecretReaders reads a comma-separated list of
// <clusterrole>=<namespace>/<serviceaccount>. The RoleBinding of a reader is
// named after its ClusterRole, so a ClusterRole is listed once.
func ParseSecretReaders(s string) ([]SecretReader, error) {
	var out []SecretReader
	seen := map[string]bool{}
	for _, entry := range strings.Split(s, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		role, sa, ok := strings.Cut(entry, "=")
		ns, name, okSA := strings.Cut(sa, "/")
		if !ok || !okSA {
			return nil, fmt.Errorf("%q is not <clusterrole>=<namespace>/<serviceaccount>", entry)
		}
		role, ns, name = strings.TrimSpace(role), strings.TrimSpace(ns), strings.TrimSpace(name)
		if role == "" {
			return nil, fmt.Errorf("%q names no ClusterRole", entry)
		}
		// RBAC object names are path segments: a ClusterRole name is a valid
		// RoleBinding name.
		if msgs := content.IsPathSegmentName(role); len(msgs) > 0 {
			return nil, fmt.Errorf("%q: ClusterRole %q: %s", entry, role, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			return nil, fmt.Errorf("%q: namespace %q: %s", entry, ns, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			return nil, fmt.Errorf("%q: ServiceAccount %q: %s", entry, name, strings.Join(msgs, "; "))
		}
		if seen[role] {
			return nil, fmt.Errorf("ClusterRole %q is listed twice: its RoleBinding is named after it", role)
		}
		seen[role] = true
		out = append(out, SecretReader{ClusterRole: role, ServiceAccount: types.NamespacedName{Namespace: ns, Name: name}})
	}
	return out, nil
}

// keepSecretReaders keeps in ns one RoleBinding per configured SecretReader,
// named after its ClusterRole, and deletes the reconciler's reader bindings
// of a reader no longer configured.
func (r *OrganizationReconciler) keepSecretReaders(ctx context.Context, ns string) error {
	want := map[string]bool{}
	var errs []error
	for _, reader := range r.Config.SecretReaders {
		want[reader.ClusterRole] = true
		errs = append(errs, r.ensureRoleBinding(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: reader.ClusterRole, Namespace: ns, Labels: ownLabels(ScopeSecretReader)},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: reader.ClusterRole},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: reader.ServiceAccount.Name, Namespace: reader.ServiceAccount.Namespace,
			}},
		}))
	}
	have := &rbacv1.RoleBindingList{}
	if err := r.Client.List(ctx, have, client.InNamespace(ns), client.MatchingLabels{ObjectLabel: ScopeSecretReader}); err != nil {
		return errors.Join(append(errs, fmt.Errorf("list RoleBindings in %s: %w", ns, err))...)
	}
	for i := range have.Items {
		rb := &have.Items[i]
		if want[rb.Name] {
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
