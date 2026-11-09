package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

// TestOrganizationsOfBinding pins which parent bindings queue the parent's
// organizations. The envtest cases cannot: a binding there is often picked up
// by a reconcile that something else queued.
func TestOrganizationsOfBinding(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	org := &v1alpha1.AgentPlatformOrganization{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "org-acme"}}
	r := &OrganizationReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(org).Build()}
	rbacOperator := map[string]string{"giantswarm.io/managed-by": "rbac-operator"}
	role := func(name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
	}

	tests := []struct {
		name   string
		labels map[string]string
		ref    rbacv1.RoleRef
		queued bool
	}{
		{name: "organizations-admin", ref: role(OrganizationsAdminClusterRole), queued: true},
		{name: "rbac-operator's cluster-admin", labels: rbacOperator, ref: role("cluster-admin"), queued: true},
		{name: "a hand-written cluster-admin", ref: role("cluster-admin")},
		{name: "rbac-operator's binding to another role", labels: rbacOperator, ref: role("view")},
		{name: "rbac-operator's binding to a Role named cluster-admin", labels: rbacOperator, ref: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "cluster-admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "admins", Namespace: "org-acme", Labels: tt.labels}, RoleRef: tt.ref}
			var want []reconcile.Request
			if tt.queued {
				want = []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(org)}}
			}
			assert.Equal(t, want, r.organizationsOfBinding(t.Context(), rb))
		})
	}
}
