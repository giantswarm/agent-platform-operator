package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

// fluxCases are the cases of commit mode, run in TestOrganizationEnvtest:
// the organization's Flux Kustomization and ServiceAccount, and the Orphaned
// condition.
func fluxCases(t *testing.T, c *cluster) {
	t.Run("an organization gets a Kustomization that applies its path as a ServiceAccount bound in its namespace only", func(t *testing.T) {
		org := c.organization(t, "org-git", "ledger")
		ns := c.readyNamespace(t, org).Name
		org = c.get(t, org)

		// Both named after the organization namespace, with the label the
		// erasure selects them by.
		k := c.eventually(t, kustomizationGVK, fluxNS, ns, nil)
		assert.Equal(t, string(org.UID), k.GetLabels()[v1alpha1.OrganizationUIDLabel])
		assert.Equal(t, map[string]any{
			"interval":           "10m",
			"path":               "./agent-platform/organizations/org-git/ledger",
			"prune":              true,
			"deletionPolicy":     "WaitForTermination",
			"suspend":            false,
			"sourceRef":          map[string]any{"kind": "GitRepository", nameField: fluxSource, "namespace": fluxNS},
			"decryption":         map[string]any{"provider": "sops", "secretRef": nameRef(sopsSecret)},
			"serviceAccountName": ns,
			"targetNamespace":    ns,
		}, k.Object["spec"])
		sa := &corev1.ServiceAccount{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: fluxNS, Name: ns}, sa))
		assert.Equal(t, string(org.UID), sa.Labels[v1alpha1.OrganizationUIDLabel])

		// One binding of the ServiceAccount, in the organization namespace.
		subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: ns, Namespace: fluxNS}
		bindings := &rbacv1.RoleBindingList{}
		require.NoError(t, c.admin.List(t.Context(), bindings))
		var bound []string
		for _, rb := range bindings.Items {
			for _, s := range rb.Subjects {
				if s == subject {
					bound = append(bound, rb.Namespace+"/"+rb.Name+" -> "+rb.RoleRef.Name)
				}
			}
		}
		assert.Equal(t, []string{ns + "/" + controller.FluxRoleBinding + " -> " + organizationFluxCR}, bound)
		crbs := &rbacv1.ClusterRoleBindingList{}
		require.NoError(t, c.admin.List(t.Context(), crbs))
		for _, crb := range crbs.Items {
			assert.NotContains(t, crb.Subjects, subject, "ClusterRoleBinding %s binds the organization's Flux ServiceAccount", crb.Name)
		}

		// What kustomize-controller does with a merged file: apply it as
		// spec.serviceAccountName. Into the organization namespace it lands;
		// into another organization's namespace or the Flux namespace it is
		// refused.
		other := c.readyNamespace(t, c.organization(t, "org-git-other", "ledger")).Name
		asFlux := c.as(t, "system:serviceaccount:"+fluxNS+":"+ns)
		secret := func(namespace string) *corev1.Secret {
			return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tenant-credential-merged", Namespace: namespace}}
		}
		require.NoError(t, asFlux.Create(t.Context(), secret(ns)))
		for _, elsewhere := range []string{other, fluxNS} {
			err := asFlux.Create(t.Context(), secret(elsewhere))
			assert.True(t, apierrors.IsForbidden(err), "a merged file was applied into %s: %v", elsewhere, err)
		}

		// A change of the Kustomization is reverted.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.read(t, kustomizationGVK, fluxNS, ns)
			assert.NoError(ct, unstructured.SetNestedField(got.Object, other, "spec", "targetNamespace"))
			assert.NoError(ct, unstructured.SetNestedField(got.Object, "kustomize-controller", "spec", "serviceAccountName"))
			assert.NoError(ct, c.admin.Update(t.Context(), got))
		}, timeout, tick)
		c.eventually(t, kustomizationGVK, fluxNS, ns, func(ct *assert.CollectT, got *unstructured.Unstructured) {
			assert.Equal(ct, ns, str(got, "spec", "targetNamespace"))
			assert.Equal(ct, ns, str(got, "spec", "serviceAccountName"))
		})
	})

	t.Run("an organization missing from the installation Kustomization's inventory is Orphaned until it is back", func(t *testing.T) {
		const parent = "org-orphans"
		createNamespace(t, c.admin, parent)
		// Flux's inventory ID of an AgentPlatformOrganization.
		entry := func(name string) any {
			return map[string]any{"id": parent + "_" + name + "_agent-platform.giantswarm.io_AgentPlatformOrganization", "v": "v1alpha1"}
		}
		installation := newObject(kustomizationGVK, fluxNS, installationKS, nil, map[string]any{"path": "./management-clusters/test", "prune": true, "deletionPolicy": "Orphan"})
		installation.Object["status"] = map[string]any{"inventory": map[string]any{"entries": []any{entry("treasury")}}}
		c.create(t, installation)
		setInventory := func(entries ...any) {
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.read(t, kustomizationGVK, fluxNS, installationKS)
				assert.NoError(ct, unstructured.SetNestedSlice(got.Object, entries, "status", "inventory", "entries"))
				assert.NoError(ct, c.admin.Update(t.Context(), got))
			}, timeout, tick)
		}

		// Applied by the installation Kustomization, as Flux labels it.
		fromGit := map[string]string{v1alpha1.FluxNameLabel: installationKS, v1alpha1.FluxNamespaceLabel: fluxNS}
		org := &v1alpha1.AgentPlatformOrganization{
			ObjectMeta: metav1.ObjectMeta{Name: "treasury", Namespace: parent, Labels: fromGit},
			Spec:       v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: 3}},
		}
		c.create(t, org)
		ns := c.readyNamespace(t, org).Name
		c.modelConfig(t, ns, "anthropic", false)
		c.orphaned(t, org, metav1.ConditionFalse, v1alpha1.ReasonInInventory)
		c.replicas(t, ns, 3)

		// The file is gone from git: Orphaned, and the workers stop.
		setInventory()
		c.orphaned(t, org, metav1.ConditionTrue, v1alpha1.ReasonMissingFromGit)
		c.replicas(t, ns, 0)

		// agent-manager's delete pull request marked it: the delete merged.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			got.Annotations = map[string]string{v1alpha1.DeletePullRequestAnnotation: "https://github.com/giantswarm/test-configs/pull/7"}
			assert.NoError(ct, c.admin.Update(t.Context(), got))
		}, timeout, tick)
		c.orphaned(t, org, metav1.ConditionTrue, v1alpha1.ReasonDeleteMerged)
		c.replicas(t, ns, 0)

		// The file is back: no longer Orphaned, and the workers run again.
		setInventory(entry("treasury"))
		c.orphaned(t, org, metav1.ConditionFalse, v1alpha1.ReasonInInventory)
		c.replicas(t, ns, 3)

		// An organization applied without git, missing from the same
		// inventory, is never Orphaned.
		applied := c.organizationWith(t, parent, "applied", v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: 3}})
		appliedNS := c.readyNamespace(t, applied).Name
		c.modelConfig(t, appliedNS, "anthropic", false)
		c.replicas(t, appliedNS, 3)
		assert.Nil(t, meta.FindStatusCondition(c.get(t, applied).Status.Conditions, v1alpha1.ConditionOrphaned))
	})
}

// orphaned waits until org's Orphaned condition has status and reason.
func (c *cluster) orphaned(t *testing.T, org *v1alpha1.AgentPlatformOrganization, status metav1.ConditionStatus, reason string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cond := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionOrphaned)
		if assert.NotNil(ct, cond) {
			assert.Equal(ct, status, cond.Status)
			assert.Equal(ct, reason, cond.Reason)
		}
	}, timeout, tick)
}

// as is a client of the API server as user.
func (c *cluster) as(t *testing.T, user string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(c.rest)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	cl, err := client.New(cfg, client.Options{Scheme: c.admin.Scheme()})
	require.NoError(t, err)
	return cl
}
