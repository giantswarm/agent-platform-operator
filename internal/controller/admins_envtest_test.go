package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

// adminClusterRole is the ClusterRole an organization's admins hold in its
// namespace.
const adminClusterRole = "agent-platform:admin"

// adminGroup and leadGroup are an organization's admin groups.
const (
	adminGroup = "acme-admins"
	leadGroup  = "acme-leads"
)

func adminsCases(t *testing.T, c *cluster) {
	t.Run("the organization's admins are bound to agent-platform:admin in its namespace", func(t *testing.T) {
		org := c.organizationWithAdmins(t, "admins",
			orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup},
			orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: leadGroup},
		)
		ns := c.readyNamespace(t, org)

		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: controller.AdminsBinding}, rb))
		assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: adminClusterRole}, rb.RoleRef)
		assert.Equal(t, []rbacv1.Subject{group(adminGroup), group(leadGroup)}, rb.Subjects)
		assert.Equal(t, controller.ScopeAdmins, rb.Labels[controller.ObjectLabel])
	})

	t.Run("the admins binding follows the list, reverts an edit and goes with the last admin", func(t *testing.T) {
		org := c.organizationWithAdmins(t, "admins-follow", orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		ns := c.readyNamespace(t, org)
		key := client.ObjectKey{Namespace: ns.Name, Name: controller.AdminsBinding}
		c.adminsAre(t, key, group(adminGroup))

		c.setAdmins(t, org, orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminUser, Name: "dana@example.com"}, orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		c.adminsAre(t, key, user("dana@example.com"), group(adminGroup))

		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), key, rb))
		rb.Subjects = append(rb.Subjects, group("intruders"))
		require.NoError(t, c.admin.Update(t.Context(), rb))
		c.adminsAre(t, key, user("dana@example.com"), group(adminGroup))

		c.setAdmins(t, org)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			err := c.admin.Get(t.Context(), key, &rbacv1.RoleBinding{})
			assert.True(ct, apierrors.IsNotFound(err), "the admins binding is still there: %v", err)
		}, timeout, tick)
		c.readyAt(t, org, c.get(t, org).Generation)
	})

	t.Run("a removed admin loses access although a finalizer holds the admins binding", func(t *testing.T) {
		org := c.organizationWithAdmins(t, "admins-held", orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		ns := c.readyNamespace(t, org)
		key := client.ObjectKey{Namespace: ns.Name, Name: controller.AdminsBinding}
		c.adminsAre(t, key, group(adminGroup))
		require.True(t, c.canGetSecretAs(t, "member@example.com", []string{adminGroup}, ns.Name))
		rbGVK := rbacv1.SchemeGroupVersion.WithKind("RoleBinding")
		c.setFinalizers(t, rbGVK, ns.Name, controller.AdminsBinding, []string{"example.com/hold"})
		// t.Context() is done when the cleanups run.
		t.Cleanup(func() {
			patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
			rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: controller.AdminsBinding}}
			if err := c.admin.Patch(context.Background(), rb, patch); client.IgnoreNotFound(err) != nil {
				t.Errorf("release the admins binding: %v", err)
			}
		})

		c.setAdmins(t, org)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			assert.False(ct, c.canGetSecretAs(t, "member@example.com", []string{adminGroup}, ns.Name), "the removed admin group still holds agent-platform:admin in %s", ns.Name)
		}, timeout, tick)
	})

	t.Run("a held organization's admins binding follows the list, reverts an edit and goes with the last admin", func(t *testing.T) {
		org := c.organizationWithAdmins(t, "admins-hold",
			orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup},
			orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: leadGroup},
		)
		ns := c.readyNamespace(t, org)
		key := client.ObjectKey{Namespace: ns.Name, Name: controller.AdminsBinding}
		c.adminsAre(t, key, group(adminGroup), group(leadGroup))
		c.hold(t, org)

		c.setAdmins(t, org, orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminUser, Name: "dana@example.com"}, orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		c.adminsAre(t, key, user("dana@example.com"), group(adminGroup))

		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), key, rb))
		rb.Subjects = append(rb.Subjects, group("intruders"))
		require.NoError(t, c.admin.Update(t.Context(), rb))
		c.adminsAre(t, key, user("dana@example.com"), group(adminGroup))

		c.setAdmins(t, org)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			err := c.admin.Get(t.Context(), key, &rbacv1.RoleBinding{})
			assert.True(ct, apierrors.IsNotFound(err), "the admins binding is still there: %v", err)
		}, timeout, tick)
		// Still held: the namespace stays.
		got := c.get(t, org)
		assert.Equal(t, "AgentPlatformDisabled", meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady).Reason)
		still := &corev1.Namespace{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(ns), still))
		assert.Nil(t, still.DeletionTimestamp, "the namespace of a held organization is being deleted")
	})

	t.Run("a held organization writes no admins binding in a namespace of its name that another writer created", func(t *testing.T) {
		squatted := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.NamespacePrefix + "admins-squatted"}}
		require.NoError(t, c.admin.Create(t.Context(), squatted))
		org := c.organizationWithAdmins(t, "admins-squatted", orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, "NameTaken", ready.Reason)
			}
		}, timeout, tick)

		c.hold(t, org)
		c.setAdmins(t, org, orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: leadGroup})
		c.never(t, rbacv1.SchemeGroupVersion.WithKind("RoleBinding"), squatted.Name, controller.AdminsBinding)
	})

	t.Run("an organization without admins leaves another writer's organization-admins alone", func(t *testing.T) {
		org := c.organization(t, "admins-none")
		ns := c.readyNamespace(t, org)
		other := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: controller.AdminsBinding, Namespace: ns.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: adminClusterRole},
			Subjects:   []rbacv1.Subject{group("platform-team")},
		}
		require.NoError(t, c.admin.Create(t.Context(), other))

		c.poke(t, org)
		c.staysPresent(t, rbacv1.SchemeGroupVersion.WithKind("RoleBinding"), ns.Name, controller.AdminsBinding)
		c.readyAt(t, org, c.get(t, org).Generation)
	})
}

// organizationWithAdmins creates an Organization like organization, with
// admins in spec.admins.
func (c *cluster) organizationWithAdmins(t *testing.T, name string, admins ...orgv1.OrganizationAdmin) *orgv1.Organization {
	t.Helper()
	org := &orgv1.Organization{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: orgv1.OrganizationSpec{
			Admins:        admins,
			AgentPlatform: &orgv1.AgentPlatformSpec{Enabled: true, Harnesses: []string{curatedHarness}, Workers: withReplicas(1)},
		},
	}
	require.NoError(t, c.admin.Create(t.Context(), org))
	return org
}

// setAdmins sets org's spec.admins; none clears it.
func (c *cluster) setAdmins(t *testing.T, org *orgv1.Organization, admins ...orgv1.OrganizationAdmin) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		got := c.get(ct, org)
		got.Spec.Admins = admins
		assert.NoError(ct, c.admin.Update(t.Context(), got))
	}, timeout, tick)
}

// adminsAre waits until the admins binding key binds agent-platform:admin to
// subjects.
func (c *cluster) adminsAre(t *testing.T, key client.ObjectKey, subjects ...rbacv1.Subject) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		rb := &rbacv1.RoleBinding{}
		if assert.NoError(ct, c.admin.Get(t.Context(), key, rb)) {
			assert.Equal(ct, adminClusterRole, rb.RoleRef.Name)
			assert.Equal(ct, subjects, rb.Subjects)
		}
	}, timeout, tick)
}

func group(name string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: name}
}

func user(name string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}
}
