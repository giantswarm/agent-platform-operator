package controller_test

import (
	"context"
	"encoding/json"
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

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

const (
	timeout         = 30 * time.Second
	tick            = 100 * time.Millisecond
	kindClusterRole = "ClusterRole"
)

func TestOrganizationEnvtest(t *testing.T) {
	c := startEnvtest(t)

	t.Run("an organization yields its namespace and the objects kept there", func(t *testing.T) {
		org := c.organization(t, "payments")
		ns := c.readyNamespace(t, org)

		assert.Equal(t, "agents-payments", ns.Name)
		assert.Equal(t, "payments", ns.Labels[v1alpha1.OrganizationLabel])
		assert.Equal(t, string(org.UID), ns.Labels[v1alpha1.OrganizationUIDLabel])
		assert.Empty(t, ns.OwnerReferences)

		org = c.get(t, org)
		assert.Equal(t, ns.Name, agentsNamespace(org))
		assert.Equal(t, "s3://"+snapshotBucket+"/"+string(org.UID), ap(org).SnapshotPrefix)
		ready := meta.FindStatusCondition(org.Status.Conditions, orgv1.AgentPlatformReady)
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, org.Generation, ready.ObservedGeneration)

		policy := &networkingv1.NetworkPolicy{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: workerPolicy}, policy))
		assert.Equal(t, map[string]string{appLabel: workerApp}, policy.Spec.PodSelector.MatchLabels)
		assert.Equal(t, platformNS+"/"+workerPolicy, policy.Annotations[controller.CopiedFromAnnotation])

		oci := &unstructured.Unstructured{}
		oci.SetGroupVersionKind(ociGVK)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: agentChart}, oci))
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
		org := c.organization(t, "notes")
		ns := c.readyNamespace(t, org)
		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, rb))
		rb.Annotations = map[string]string{"example.com/note": "kept"}
		require.NoError(t, c.admin.Update(t.Context(), rb))

		// A spec change reconciles the organization once more; its Ready
		// condition observes the new generation after that reconcile.
		org = c.get(t, org)
		org.Spec.Capabilities.AgentPlatform.Workers = withReplicas(2)
		require.NoError(t, c.admin.Update(t.Context(), org))
		c.readyAt(t, org, org.Generation)

		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, rb))
		assert.Equal(t, "kept", rb.Annotations["example.com/note"])
	})

	t.Run("a missing platform network policy makes the organization not Ready", func(t *testing.T) {
		source := &networkingv1.NetworkPolicy{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: platformNS, Name: workerPolicy}, source))
		require.NoError(t, c.admin.Delete(t.Context(), source))

		org := c.organization(t, "nopolicy")
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, orgv1.AgentPlatformReady)
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
		org := c.organization(t, "drift")
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
		org := c.organization(t, "terminating")
		ns := c.readyNamespace(t, org)
		// envtest runs no namespace controller: the namespace stays
		// terminating.
		require.NoError(t, c.admin.Delete(t.Context(), ns))

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			ready := meta.FindStatusCondition(got.Status.Conditions, orgv1.AgentPlatformReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NamespaceTerminating", ready.Reason)
			}
			assert.Equal(ct, ns.Name, agentsNamespace(got))
		}, timeout, tick)
		assert.Len(t, c.namespacesOf(t, org), 1, "a terminating namespace was replaced while it still exists")
	})

	t.Run("an Organization without the switch, or with it off, gets nothing", func(t *testing.T) {
		none := &orgv1.Organization{ObjectMeta: metav1.ObjectMeta{Name: "kaas-only"}}
		require.NoError(t, c.admin.Create(t.Context(), none))
		off := &orgv1.Organization{
			ObjectMeta: metav1.ObjectMeta{Name: "switched-off"},
			Spec:       orgv1.OrganizationSpec{Capabilities: orgv1.OrganizationCapabilities{AgentPlatform: &orgv1.AgentPlatformSpec{Harnesses: []string{curatedHarness}}}},
		}
		require.NoError(t, c.admin.Create(t.Context(), off))
		// An enabled organization created after both is reconciled, so the
		// two had their turn.
		c.readyNamespace(t, c.organization(t, "switched-on"))

		for _, org := range []*orgv1.Organization{none, off} {
			got := c.get(t, org)
			assert.Empty(t, got.Finalizers, org.Name)
			assert.Equal(t, orgv1.OrganizationStatus{}, got.Status, org.Name)
			err := c.admin.Get(t.Context(), client.ObjectKey{Name: controller.NamespacePrefix + org.Name}, &corev1.Namespace{})
			assert.True(t, apierrors.IsNotFound(err), "%s got a namespace: %v", org.Name, err)
		}
	})

	t.Run("a switch-off that got past admission changes nothing and erases nothing", func(t *testing.T) {
		org := c.organizationWithAdmins(t, "switch-off", orgv1.OrganizationAdmin{Kind: orgv1.OrganizationAdminGroup, Name: adminGroup})
		ns := c.readyNamespace(t, org)
		key := client.ObjectKey{Namespace: ns.Name, Name: controller.AdminsBinding}
		c.adminsAre(t, key, group(adminGroup))

		// The same write changes the admins: the operator applies none of it.
		var generation int64
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			got.Spec.Capabilities.AgentPlatform.Enabled = false
			got.Spec.Admins = []orgv1.OrganizationAdmin{{Kind: orgv1.OrganizationAdminGroup, Name: leadGroup}}
			if assert.NoError(ct, c.admin.Update(t.Context(), got)) {
				generation = got.Generation
			}
		}, timeout, tick)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, orgv1.AgentPlatformReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "AgentPlatformDisabled", ready.Reason)
				assert.Contains(ct, ready.Message, "not supported yet")
				assert.Equal(ct, generation, ready.ObservedGeneration)
			}
		}, timeout, tick)

		got := c.get(t, org)
		assert.Contains(t, got.Finalizers, v1alpha1.Finalizer)
		assert.Equal(t, ns.Name, agentsNamespace(got))
		still := &corev1.Namespace{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(ns), still))
		assert.Nil(t, still.DeletionTimestamp, "the namespace of a switched-off organization is being deleted")
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, &corev1.ServiceAccount{}))
		c.adminsAre(t, key, group(adminGroup))

		// On again, it is Ready on the same namespace, with the new admins.
		got.Spec.Capabilities.AgentPlatform.Enabled = true
		require.NoError(t, c.admin.Update(t.Context(), got))
		c.readyAt(t, got, got.Generation)
		assert.Equal(t, ns.Name, c.readyNamespace(t, org).Name)
		c.adminsAre(t, key, group(leadGroup))
	})

	t.Run("the status fields and entries of another field manager stay", func(t *testing.T) {
		// Before the Agent Platform is on, organization-operator applies, as
		// its own field manager, status.namespace, its kaas entry of
		// status.namespaces and a condition of its own.
		org := &orgv1.Organization{ObjectMeta: metav1.ObjectMeta{Name: "shared-status"}}
		require.NoError(t, c.admin.Create(t.Context(), org))
		kaas := orgv1.OrganizationNamespace{Name: "org-shared-status", Capability: orgv1.CapabilityKaaS}
		raw, err := json.Marshal(struct {
			Status orgv1.OrganizationStatus `json:"status"`
		}{orgv1.OrganizationStatus{
			Namespace:  kaas.Name,
			Namespaces: []orgv1.OrganizationNamespace{kaas},
			Conditions: []metav1.Condition{{Type: "KaaSReady", Status: metav1.ConditionTrue, Reason: "Reconciled", LastTransitionTime: metav1.Now()}},
		}})
		require.NoError(t, err)
		applied := &unstructured.Unstructured{}
		require.NoError(t, json.Unmarshal(raw, &applied.Object))
		applied.SetGroupVersionKind(orgv1.GroupVersion.WithKind("Organization"))
		applied.SetName(org.Name)
		require.NoError(t, c.admin.Status().Apply(t.Context(), client.ApplyConfigurationFromUnstructured(applied),
			client.FieldOwner("organization-operator"), client.ForceOwnership))

		// On: the operator's first status write adds its entry to both lists.
		got := c.get(t, org)
		got.Spec.Capabilities.AgentPlatform = &orgv1.AgentPlatformSpec{Enabled: true, Harnesses: []string{curatedHarness}, Workers: withReplicas(1)}
		require.NoError(t, c.admin.Update(t.Context(), got))
		ns := c.readyNamespace(t, org)

		got = c.get(t, org)
		assert.Equal(t, kaas.Name, got.Status.Namespace)
		assert.ElementsMatch(t, []orgv1.OrganizationNamespace{kaas, {Name: ns.Name, Capability: orgv1.CapabilityAgentPlatform}}, got.Status.Namespaces)
		assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, "KaaSReady"), "conditions: %+v", got.Status.Conditions)
	})

	t.Run("a namespace of the organization's name that another writer created is not taken over", func(t *testing.T) {
		squatted := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.NamespacePrefix + "squatted"}}
		require.NoError(t, c.admin.Create(t.Context(), squatted))
		org := c.organization(t, "squatted")
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			ready := meta.FindStatusCondition(got.Status.Conditions, orgv1.AgentPlatformReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NameTaken", ready.Reason)
				assert.Contains(ct, ready.Message, squatted.Name)
			}
			assert.Empty(ct, agentsNamespace(got))
		}, timeout, tick)
		ns := &corev1.Namespace{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(squatted), ns))
		assert.Empty(t, ns.Labels[v1alpha1.OrganizationUIDLabel], "the operator took the namespace over")
		err := c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: helmSA}, &corev1.ServiceAccount{})
		assert.True(t, apierrors.IsNotFound(err), "the operator wrote into a namespace that is not the organization's: %v", err)

		// The organization never had the namespace: its deletion ends, and
		// the namespace stays.
		require.NoError(t, c.admin.Delete(t.Context(), org))
		c.gone(t, org)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKeyFromObject(squatted), ns))
		assert.Nil(t, ns.DeletionTimestamp, "the erasure deleted a namespace that was never the organization's")
	})

	t.Run("a name that does not fit in the namespace name gives a condition and no namespace", func(t *testing.T) {
		for _, name := range []string{strings.Repeat("a", 57), "team.payments"} {
			org := c.organization(t, name)
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.get(ct, org)
				ready := meta.FindStatusCondition(got.Status.Conditions, orgv1.AgentPlatformReady)
				if assert.NotNil(ct, ready) {
					assert.Equal(ct, metav1.ConditionFalse, ready.Status)
					assert.Equal(ct, "NamespaceNameInvalid", ready.Reason)
				}
				assert.Empty(ct, agentsNamespace(got))
			}, timeout, tick, name)
			err := c.admin.Get(t.Context(), client.ObjectKey{Name: controller.NamespacePrefix + name}, &corev1.Namespace{})
			assert.True(t, apierrors.IsNotFound(err), "%s: %v", name, err)
		}
		// The longest name that fits.
		c.readyNamespace(t, c.organization(t, strings.Repeat("a", 56)))
	})

	curatedCases(t, c)
	referenceCases(t, c)
	fluxCases(t, c)
	serviceAccountBindingCases(t, c)
	adminsCases(t, c)
	erasureCases(t, c)
}

// organization creates an Organization with the Agent Platform enabled, the
// curated Harness and one worker.
func (c *cluster) organization(t *testing.T, name string) *orgv1.Organization {
	t.Helper()
	return c.organizationWith(t, name, orgv1.AgentPlatformSpec{Harnesses: []string{curatedHarness}, Workers: withReplicas(1)})
}

// organizationWith creates an Organization named name whose
// spec.capabilities.agentPlatform is spec, with the switch on.
func (c *cluster) organizationWith(t *testing.T, name string, spec orgv1.AgentPlatformSpec) *orgv1.Organization {
	t.Helper()
	spec.Enabled = true
	org := &orgv1.Organization{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       orgv1.OrganizationSpec{Capabilities: orgv1.OrganizationCapabilities{AgentPlatform: &spec}},
	}
	require.NoError(t, c.admin.Create(t.Context(), org))
	return org
}

// readyNamespace waits until org is Ready and returns its namespace.
func (c *cluster) readyNamespace(t *testing.T, org *orgv1.Organization) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		got := c.get(ct, org)
		if !assert.NotEmpty(ct, agentsNamespace(got)) {
			return
		}
		assert.True(ct, meta.IsStatusConditionTrue(got.Status.Conditions, orgv1.AgentPlatformReady), "conditions: %+v", got.Status.Conditions)
		assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Name: agentsNamespace(got)}, ns))
	}, timeout, tick)
	return ns
}

// readyAt waits until org's Ready condition is True for generation.
func (c *cluster) readyAt(t *testing.T, org *orgv1.Organization, generation int64) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, orgv1.AgentPlatformReady)
		if assert.NotNil(ct, ready) {
			assert.Equal(ct, metav1.ConditionTrue, ready.Status)
			assert.Equal(ct, generation, ready.ObservedGeneration)
		}
	}, timeout, tick)
}

// get reads org as the admin.
func (c *cluster) get(t require.TestingT, org *orgv1.Organization) *orgv1.Organization {
	got := &orgv1.Organization{}
	require.NoError(t, c.admin.Get(context.Background(), client.ObjectKeyFromObject(org), got))
	return got
}

// ap is org's status.capabilities.agentPlatform, the zero value before the
// operator wrote it.
func ap(org *orgv1.Organization) orgv1.AgentPlatformStatus {
	if org.Status.Capabilities.AgentPlatform == nil {
		return orgv1.AgentPlatformStatus{}
	}
	return *org.Status.Capabilities.AgentPlatform
}

// agentsNamespace is the name of the agentPlatform entry of org's
// status.namespaces, "" before the operator wrote it.
func agentsNamespace(org *orgv1.Organization) string {
	for _, ns := range org.Status.Namespaces {
		if ns.Capability == orgv1.CapabilityAgentPlatform {
			return ns.Name
		}
	}
	return ""
}

// erasureStep is the reason of org's AgentPlatformErasing condition while it
// is True, "" before the erasure.
func erasureStep(org *orgv1.Organization) string {
	erasing := meta.FindStatusCondition(org.Status.Conditions, orgv1.AgentPlatformErasing)
	if erasing == nil || erasing.Status != metav1.ConditionTrue {
		return ""
	}
	return erasing.Reason
}

// namespacesOf are the namespaces labelled with org's UID.
func (c *cluster) namespacesOf(t *testing.T, org *orgv1.Organization) []corev1.Namespace {
	t.Helper()
	list := &corev1.NamespaceList{}
	require.NoError(t, c.admin.List(t.Context(), list, client.MatchingLabels{v1alpha1.OrganizationUIDLabel: string(org.UID)}))
	return list.Items
}
