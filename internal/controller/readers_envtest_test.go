package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

func secretReaderCases(t *testing.T, c *cluster) {
	t.Run("a secret reader is bound in the organization namespace and nowhere else", func(t *testing.T) {
		org := c.organization(t, "org-readers", "readers")
		ns := c.readyNamespace(t, org)

		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: readerClusterRole}, rb))
		assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: readerClusterRole}, rb.RoleRef)
		assert.Equal(t, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: readerSA, Namespace: readerNS}}, rb.Subjects)
		assert.Equal(t, controller.ScopeSecretReader, rb.Labels[controller.ObjectLabel])

		assert.True(t, c.canGetSecret(t, ns.Name), "the reader cannot read Secrets in its organization namespace %s", ns.Name)
		assert.False(t, c.canGetSecret(t, org.Namespace), "the reader reads Secrets in the parent namespace %s", org.Namespace)
		assert.False(t, c.canGetSecret(t, readerNS), "the reader reads Secrets in its own namespace %s", readerNS)
	})

	t.Run("a changed secret reader binding is restored", func(t *testing.T) {
		org := c.organization(t, "org-readers-drift", "readers-drift")
		ns := c.readyNamespace(t, org)
		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: readerClusterRole}, rb))
		rb.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "someone-else", Namespace: readerNS}}
		require.NoError(t, c.admin.Update(t.Context(), rb))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := &rbacv1.RoleBinding{}
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: readerClusterRole}, got)) {
				assert.Equal(ct, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: readerSA, Namespace: readerNS}}, got.Subjects)
			}
		}, timeout, tick)
	})

	t.Run("a secret reader binding of a reader no longer configured is deleted", func(t *testing.T) {
		org := c.organization(t, "org-readers-gone", "readers-gone")
		ns := c.readyNamespace(t, org)
		stale := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "removed-secret-reader", Namespace: ns.Name, Labels: map[string]string{controller.ObjectLabel: controller.ScopeSecretReader}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: "removed-secret-reader"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "removed", Namespace: readerNS}},
		}
		// Another writer's binding is never the reconciler's to delete.
		other := stale.DeepCopy()
		other.Name, other.Labels = "another-writers-binding", nil
		require.NoError(t, c.admin.Create(t.Context(), stale))
		require.NoError(t, c.admin.Create(t.Context(), other))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			err := c.admin.Get(t.Context(), client.ObjectKeyFromObject(stale), &rbacv1.RoleBinding{})
			assert.True(ct, apierrors.IsNotFound(err), "the stale reader binding is still there: %v", err)
		}, timeout, tick)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(other), &rbacv1.RoleBinding{}))
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: readerClusterRole}, &rbacv1.RoleBinding{}))
	})
}

// canGetSecret asks the API server's authorizer whether the reader's
// ServiceAccount may get a Secret in ns.
func (c *cluster) canGetSecret(t *testing.T, ns string) bool {
	t.Helper()
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               "system:serviceaccount:" + readerNS + ":" + readerSA,
		Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + readerNS, "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: "get", Resource: "secrets", Name: "acme-kubeconfig"},
	}}
	require.NoError(t, c.admin.Create(t.Context(), review))
	return review.Status.Allowed
}
