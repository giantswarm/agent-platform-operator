package controller

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"
)

const (
	// ScopeAdmins marks, as the value of ObjectLabel, the RoleBinding that
	// binds the organization's admins (spec.admins) in its namespace.
	ScopeAdmins = "admins"
	// AdminsBinding is the name of that RoleBinding.
	AdminsBinding = "organization-admins"
	// AdminClusterRole is the ClusterRole the organization's admins hold in
	// its namespace.
	AdminClusterRole = "agent-platform:admin"
)

// keepAdmins keeps the RoleBinding AdminsBinding in ns, which binds
// AdminClusterRole to the subjects of org's spec.admins, in their order. With
// no admins the reconciler's binding is deleted: the organization has no
// admins binding, and that is no error. A binding of that name that another
// writer holds is left as it is.
func (r *OrganizationReconciler) keepAdmins(ctx context.Context, org *orgv1.Organization, ns string) error {
	if len(org.Spec.Admins) == 0 {
		return r.deleteAdminsBinding(ctx, ns)
	}
	subjects := make([]rbacv1.Subject, 0, len(org.Spec.Admins))
	for _, a := range org.Spec.Admins {
		// The API server's default for a User or a Group: without it the
		// binding read back differs from the one wanted.
		subjects = append(subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: a.Kind, Name: a.Name})
	}
	return r.ensureRoleBinding(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: AdminsBinding, Namespace: ns, Labels: ownLabels(ScopeAdmins)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: AdminClusterRole},
		Subjects:   subjects,
	})
}

// deleteAdminsBinding deletes the reconciler's admins binding in ns, if any.
func (r *OrganizationReconciler) deleteAdminsBinding(ctx context.Context, ns string) error {
	rb := &rbacv1.RoleBinding{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: AdminsBinding}, rb)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get RoleBinding %s/%s: %w", ns, AdminsBinding, err)
	case rb.Labels[ObjectLabel] != ScopeAdmins:
		return nil
	}
	// The UID: a binding of that name that another writer created since the
	// read is not deleted.
	if err := r.Client.Delete(ctx, rb, client.Preconditions{UID: &rb.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete RoleBinding %s/%s: %w", ns, AdminsBinding, err)
	}
	logf.FromContext(ctx).Info("organization object deleted", "namespace", ns, "kind", "RoleBinding", "name", AdminsBinding)
	return nil
}
