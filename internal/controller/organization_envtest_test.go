package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

const (
	timeout         = 30 * time.Second
	tick            = 100 * time.Millisecond
	mirrorParent    = "org-mirror"
	kindClusterRole = "ClusterRole"
	adminsBinding   = "platform-team"
	namesParent     = "org-names"
)

func TestOrganizationEnvtest(t *testing.T) {
	c := startEnvtest(t)

	t.Run("an organization yields its namespace and the objects kept there", func(t *testing.T) {
		org := c.organization(t, "org-acme", "payments")
		ns := c.readyNamespace(t, org)

		assert.True(t, strings.HasPrefix(ns.Name, "agents-payments-"), "namespace %s is not from the generateName agents-payments-", ns.Name)
		assert.Len(t, ns.Name, len("agents-payments-")+5)
		assert.Equal(t, "payments", ns.Labels[v1alpha1.OrganizationLabel])
		assert.Equal(t, "org-acme", ns.Labels[v1alpha1.ParentLabel])
		assert.Equal(t, string(org.UID), ns.Labels[v1alpha1.OrganizationUIDLabel])
		assert.Empty(t, ns.OwnerReferences)
		assert.Len(t, c.namespacesOf(t, org), 1)

		org = c.get(t, org)
		assert.Equal(t, "s3://"+snapshotBucket+"/"+string(org.UID), org.Status.SnapshotPrefix)
		ready := meta.FindStatusCondition(org.Status.Conditions, v1alpha1.ConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, org.Generation, ready.ObservedGeneration)

		policy := &networkingv1.NetworkPolicy{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: workerPolicy}, policy))
		assert.Equal(t, map[string]string{appLabel: workerApp}, policy.Spec.PodSelector.MatchLabels)
		assert.Equal(t, platformNS+"/"+workerPolicy, policy.Annotations[controller.CopiedFromAnnotation])

		oci := &unstructured.Unstructured{}
		oci.SetGroupVersionKind(ociGVK)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: "agent"}, oci))
		url, _, _ := unstructured.NestedString(oci.Object, "spec", "url")
		semver, _, _ := unstructured.NestedString(oci.Object, "spec", "ref", "semver")
		assert.Equal(t, "oci://gsoci.azurecr.io/charts/giantswarm/agent", url)
		assert.Equal(t, "2.x", semver)

		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, &corev1.ServiceAccount{}))
		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, rb))
		assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: helmClusterRole}, rb.RoleRef)
		assert.Equal(t, []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: helmSA, Namespace: ns.Name}}, rb.Subjects)
	})

	t.Run("another writer's annotation on the Helm RoleBinding stays", func(t *testing.T) {
		org := c.organization(t, "org-notes", "notes")
		ns := c.readyNamespace(t, org)
		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, rb))
		rb.Annotations = map[string]string{"example.com/note": "kept"}
		require.NoError(t, c.admin.Update(t.Context(), rb))

		// A spec change reconciles the organization once more; its Ready
		// condition observes the new generation after that reconcile.
		org = c.get(t, org)
		org.Spec.Workers.Replicas = 2
		require.NoError(t, c.admin.Update(t.Context(), org))
		c.readyAt(t, org, org.Generation)

		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, rb))
		assert.Equal(t, "kept", rb.Annotations["example.com/note"])
	})

	t.Run("a missing platform network policy makes the organization not Ready", func(t *testing.T) {
		source := &networkingv1.NetworkPolicy{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: platformNS, Name: workerPolicy}, source))
		require.NoError(t, c.admin.Delete(t.Context(), source))

		org := c.organization(t, "org-nopolicy", "nopolicy")
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "SourceMissing", ready.Reason)
				assert.Contains(ct, ready.Message, platformNS+"/"+workerPolicy)
			}
		}, timeout, tick)

		// The source's creation reconciles the organization again.
		source.ResourceVersion, source.UID, source.CreationTimestamp = "", "", metav1.Time{}
		require.NoError(t, c.admin.Create(t.Context(), source))
		ns := c.readyNamespace(t, org)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: workerPolicy}, &networkingv1.NetworkPolicy{}))
	})

	t.Run("a changed network policy copy is restored", func(t *testing.T) {
		org := c.organization(t, "org-drift", "drift")
		ns := c.readyNamespace(t, org)
		policy := &networkingv1.NetworkPolicy{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: workerPolicy}, policy))
		policy.Spec.PodSelector.MatchLabels = map[string]string{appLabel: "something-else"}
		require.NoError(t, c.admin.Update(t.Context(), policy))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := &networkingv1.NetworkPolicy{}
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: workerPolicy}, got)) {
				assert.Equal(ct, map[string]string{appLabel: workerApp}, got.Spec.PodSelector.MatchLabels)
			}
		}, timeout, tick)
	})

	t.Run("a terminating organization namespace makes the organization not Ready", func(t *testing.T) {
		org := c.organization(t, "org-terminating", "terminating")
		ns := c.readyNamespace(t, org)
		// envtest runs no namespace controller: the namespace stays
		// terminating.
		require.NoError(t, c.admin.Delete(t.Context(), ns))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NamespaceTerminating", ready.Reason)
			}
			assert.Equal(ct, ns.Name, got.Status.Namespace)
		}, timeout, tick)
		assert.Len(t, c.namespacesOf(t, org), 1, "a terminating namespace was replaced while it still exists")
	})

	t.Run("a lost status finds the namespace by its organization-uid label", func(t *testing.T) {
		org := c.organization(t, "org-lost", "billing")
		ns := c.readyNamespace(t, org)

		org = c.get(t, org)
		org.Status = v1alpha1.AgentPlatformOrganizationStatus{}
		require.NoError(t, c.admin.Status().Update(t.Context(), org))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			assert.Equal(ct, ns.Name, got.Status.Namespace)
			assert.True(ct, meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady))
		}, timeout, tick)
		assert.Len(t, c.namespacesOf(t, org), 1, "a reconcile after the lost status made a second namespace")
	})

	t.Run("the parent's organizations-admin bindings are mirrored", func(t *testing.T) {
		org := c.organization(t, mirrorParent, "platform")
		ns := c.readyNamespace(t, org)
		admins := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: adminsBinding, Namespace: mirrorParent},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: controller.OrganizationsAdminClusterRole},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice@example.com"},
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "acme-platform"},
				{Kind: rbacv1.ServiceAccountKind, Name: "automation", Namespace: mirrorParent},
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"},
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "system:serviceaccount:org-mirror:automation"},
			},
		}
		require.NoError(t, c.admin.Create(t.Context(), admins))
		// A binding to another role is not mirrored.
		require.NoError(t, c.admin.Create(t.Context(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "viewers", Namespace: mirrorParent},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "bob@example.com"}},
		}))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			mirror := &rbacv1.RoleBinding{}
			if !assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: adminsBinding}, mirror)) {
				return
			}
			assert.Equal(ct, admins.RoleRef, mirror.RoleRef)
			assert.Equal(ct, []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice@example.com"},
				{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "acme-platform"},
			}, mirror.Subjects)
		}, timeout, tick)
		err := c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: "viewers"}, &rbacv1.RoleBinding{})
		assert.True(t, apierrors.IsNotFound(err), "a binding to another role was mirrored: %v", err)

		// A change of the parent's subjects follows.
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(admins), admins))
		admins.Subjects = append(admins.Subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "acme-sre"})
		require.NoError(t, c.admin.Update(t.Context(), admins))
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			mirror := &rbacv1.RoleBinding{}
			if !assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: adminsBinding}, mirror)) {
				return
			}
			assert.Contains(ct, mirror.Subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "acme-sre"})
			assert.Len(ct, mirror.Subjects, 3)
		}, timeout, tick)

		// The mirror goes with the parent's binding.
		require.NoError(t, c.admin.Delete(t.Context(), admins))
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			err := c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: adminsBinding}, &rbacv1.RoleBinding{})
			assert.True(ct, apierrors.IsNotFound(err), "the mirror is still there: %v", err)
		}, timeout, tick)
	})

	t.Run("a parent binding whose name the organization namespace already holds is reported", func(t *testing.T) {
		org := c.organization(t, "org-taken", "taken")
		c.readyNamespace(t, org)
		// The Helm RoleBinding holds the name kagent-flux in the
		// organization namespace.
		taken := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: helmSA, Namespace: "org-taken"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: controller.OrganizationsAdminClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "carol@example.com"}},
		}
		require.NoError(t, c.admin.Create(t.Context(), taken))
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NameTaken", ready.Reason)
			}
		}, timeout, tick)

		require.NoError(t, c.admin.Delete(t.Context(), taken))
		c.readyNamespace(t, org)
	})

	t.Run("the CRD refuses a name longer than 50 characters or with a dot", func(t *testing.T) {
		createNamespace(t, c.admin, namesParent)
		long := &v1alpha1.AgentPlatformOrganization{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 51), Namespace: namesParent}}
		err := c.admin.Create(t.Context(), long)
		require.True(t, apierrors.IsInvalid(err), "a 51-character name was not refused as invalid: %v", err)
		assert.Contains(t, err.Error(), "at most 50 characters")

		dotted := &v1alpha1.AgentPlatformOrganization{ObjectMeta: metav1.ObjectMeta{Name: "team.payments", Namespace: namesParent}}
		err = c.admin.Create(t.Context(), dotted)
		require.True(t, apierrors.IsInvalid(err), "a name with a dot was not refused as invalid: %v", err)
		assert.Contains(t, err.Error(), "holds no dot")

		fifty := &v1alpha1.AgentPlatformOrganization{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 50), Namespace: namesParent}}
		require.NoError(t, c.admin.Create(t.Context(), fifty))
		c.readyNamespace(t, fifty)
	})
}

// organization creates an AgentPlatformOrganization in a new parent namespace.
func (c *cluster) organization(t *testing.T, parent, name string) *v1alpha1.AgentPlatformOrganization {
	t.Helper()
	createNamespace(t, c.admin, parent)
	org := &v1alpha1.AgentPlatformOrganization{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parent},
		Spec:       v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: 1}},
	}
	require.NoError(t, c.admin.Create(t.Context(), org))
	return org
}

// readyNamespace waits until org is Ready and returns its namespace.
func (c *cluster) readyNamespace(t *testing.T, org *v1alpha1.AgentPlatformOrganization) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		got := c.get(ct, org)
		if !assert.NotEmpty(ct, got.Status.Namespace) {
			return
		}
		assert.True(ct, meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady), "conditions: %+v", got.Status.Conditions)
		assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Name: got.Status.Namespace}, ns))
	}, timeout, tick)
	return ns
}

// readyAt waits until org's Ready condition is True for generation.
func (c *cluster) readyAt(t *testing.T, org *v1alpha1.AgentPlatformOrganization, generation int64) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
		if assert.NotNil(ct, ready) {
			assert.Equal(ct, metav1.ConditionTrue, ready.Status)
			assert.Equal(ct, generation, ready.ObservedGeneration)
		}
	}, timeout, tick)
}

// get reads org as the admin.
func (c *cluster) get(t require.TestingT, org *v1alpha1.AgentPlatformOrganization) *v1alpha1.AgentPlatformOrganization {
	got := &v1alpha1.AgentPlatformOrganization{}
	require.NoError(t, c.admin.Get(context.Background(), client.ObjectKeyFromObject(org), got))
	return got
}

// namespacesOf are the namespaces labelled with org's UID.
func (c *cluster) namespacesOf(t *testing.T, org *v1alpha1.AgentPlatformOrganization) []corev1.Namespace {
	t.Helper()
	list := &corev1.NamespaceList{}
	require.NoError(t, c.admin.List(t.Context(), list, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}))
	return list.Items
}
