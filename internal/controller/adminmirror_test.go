package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/pkg/adminmirror"
)

// TestOrganizationsOfBinding pins that the RoleBinding watch queues the
// parent's organizations for exactly the bindings adminmirror mirrors. The
// envtest cases cannot: a binding there is often picked up by a reconcile
// that something else queued.
func TestOrganizationsOfBinding(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	org := &v1alpha1.AgentPlatformOrganization{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "org-acme"}}
	r := &OrganizationReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(org).Build()}

	for _, c := range adminmirror.Cases() {
		t.Run(c.Binding.Name, func(t *testing.T) {
			rb := c.Binding.DeepCopy()
			rb.Namespace = org.Namespace
			var want []reconcile.Request
			if c.IsMirroredAdmin {
				want = []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(org)}}
			}
			assert.Equal(t, want, r.organizationsOfBinding(t.Context(), rb))
		})
	}
}
