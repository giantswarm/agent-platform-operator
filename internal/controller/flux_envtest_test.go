package controller_test

import (
	"context"
	"slices"
	"testing"
	"time"

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
		labels := map[string]string{
			v1alpha1.OrganizationUIDLabel:  string(org.UID),
			v1alpha1.OrganizationLabel:     "ledger",
			v1alpha1.ParentLabel:           "org-git",
			controller.ObjectLabel:         controller.ScopeFlux,
			"app.kubernetes.io/managed-by": "agent-platform-operator",
		}
		k := c.eventually(t, kustomizationGVK, fluxNS, ns, nil)
		assert.Equal(t, labels, k.GetLabels())
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
		assert.Equal(t, labels, sa.Labels)

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
		// into another organization's namespace, the parent namespace or the
		// Flux namespace it is refused.
		other := c.readyNamespace(t, c.organization(t, "org-git-other", "ledger")).Name
		asFlux := c.as(t, "system:serviceaccount:"+fluxNS+":"+ns)
		secret := func(namespace string) *corev1.Secret {
			return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tenant-credential-merged", Namespace: namespace}}
		}
		require.NoError(t, asFlux.Create(t.Context(), secret(ns)))
		for _, elsewhere := range []string{other, "org-git", fluxNS} {
			err := asFlux.Create(t.Context(), secret(elsewhere))
			assert.True(t, apierrors.IsForbidden(err), "a merged file was applied into %s: %v", elsewhere, err)
		}

		// A change of the Kustomization is reverted.
		edit := func(fields ...string) func(*unstructured.Unstructured, any) {
			return func(got *unstructured.Unstructured, v any) {
				require.NoError(t, unstructured.SetNestedField(got.Object, v, fields...))
			}
		}
		c.editKustomization(t, ns, edit("spec", "targetNamespace"), other)
		c.editKustomization(t, ns, edit("spec", "serviceAccountName"), "kustomize-controller")
		c.eventually(t, kustomizationGVK, fluxNS, ns, func(ct *assert.CollectT, got *unstructured.Unstructured) {
			assert.Equal(ct, ns, str(got, "spec", "targetNamespace"))
			assert.Equal(ct, ns, str(got, "spec", "serviceAccountName"))
		})
		// An added field alone, which applies elsewhere, is reverted too.
		c.editKustomization(t, ns, edit("spec", "kubeConfig"), map[string]any{"secretRef": nameRef("other-kubeconfig")})
		c.eventually(t, kustomizationGVK, fluxNS, ns, func(ct *assert.CollectT, got *unstructured.Unstructured) {
			_, added, _ := unstructured.NestedMap(got.Object, "spec", "kubeConfig")
			assert.False(ct, added, "an added spec.kubeConfig stayed")
		})
		// And one added below a field the operator sets.
		c.editKustomization(t, ns, edit("spec", "decryption", "serviceAccountName"), "kustomize-controller")
		c.eventually(t, kustomizationGVK, fluxNS, ns, func(ct *assert.CollectT, got *unstructured.Unstructured) {
			assert.Empty(ct, str(got, "spec", "decryption", "serviceAccountName"), "an added spec.decryption.serviceAccountName stayed")
		})

		// The Flux ServiceAccount's labels come back: without them the cache
		// and the watch lose it.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := &corev1.ServiceAccount{}
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: fluxNS, Name: ns}, got)) {
				delete(got.Labels, controller.ObjectLabel)
				assert.NoError(ct, c.admin.Update(t.Context(), got))
			}
		}, timeout, tick)
		// A spec change reconciles the organization: the stripped
		// ServiceAccount no longer does.
		org = c.get(t, org)
		org.Spec.Workers.Replicas = 2
		require.NoError(t, c.admin.Update(t.Context(), org))
		c.readyAt(t, org, org.Generation)
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: fluxNS, Name: ns}, sa))
		assert.Equal(t, labels, sa.Labels)
	})

	t.Run("Flux objects that lost the organization's UID label are not taken over and hold the erasure before the namespace", func(t *testing.T) {
		org := c.organization(t, "org-git-stripped", "ledger")
		ns := c.readyNamespace(t, org).Name
		org = c.get(t, org)
		c.eventually(t, kustomizationGVK, fluxNS, ns, nil)
		// Out of the cache too (no organization-object label): the operator
		// confirms the miss from the API server.
		strip := func(got *unstructured.Unstructured, _ any) {
			l := got.GetLabels()
			delete(l, v1alpha1.OrganizationUIDLabel)
			delete(l, controller.ObjectLabel)
			got.SetLabels(l)
		}
		c.editKustomization(t, ns, strip, nil)
		saKey := client.ObjectKey{Namespace: fluxNS, Name: ns}
		editSA := func(edit func(map[string]string)) {
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				sa := &corev1.ServiceAccount{}
				if assert.NoError(ct, c.admin.Get(t.Context(), saKey, sa)) {
					edit(sa.Labels)
					assert.NoError(ct, c.admin.Update(t.Context(), sa))
				}
			}, timeout, tick)
		}
		editSA(func(l map[string]string) { delete(l, v1alpha1.OrganizationUIDLabel); delete(l, controller.ObjectLabel) })
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NameTaken", ready.Reason)
			}
		}, timeout, tick)
		got := c.read(t, kustomizationGVK, fluxNS, ns)
		assert.NotContains(t, got.GetLabels(), v1alpha1.OrganizationUIDLabel, "the operator took over a Kustomization without the organization's UID")
		sa := &corev1.ServiceAccount{}
		require.NoError(t, c.admin.Get(t.Context(), saKey, sa))
		assert.NotContains(t, sa.Labels, v1alpha1.OrganizationUIDLabel, "the operator took over a ServiceAccount without the organization's UID")

		// The erasure does not find them by the label: it stops in its first
		// step, before the namespace goes, naming each and both labels.
		stopsAt := func(kind string) {
			t.Helper()
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.get(ct, org)
				ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
				if assert.NotNil(ct, ready) {
					assert.Equal(ct, "Deleting", ready.Reason)
					assert.Contains(ct, ready.Message, "the "+kind+" "+fluxNS+"/"+ns+", named after the organization namespace, does not carry "+v1alpha1.OrganizationUIDLabel)
					assert.Contains(ct, ready.Message, controller.ObjectLabel+"="+controller.ScopeFlux)
				}
				assert.Equal(ct, v1alpha1.ErasureWorkloads, got.Status.ErasureStep)
			}, timeout, tick)
			require.Len(t, c.namespacesOf(t, org), 1, "the namespace went while the %s was unlabelled", kind)
		}
		restore := func(l map[string]string) {
			l[v1alpha1.OrganizationUIDLabel] = string(org.UID)
			l[controller.ObjectLabel] = controller.ScopeFlux
		}
		require.NoError(t, c.admin.Delete(t.Context(), org))
		stopsAt("Kustomization")

		// The Kustomization's labels back: the ServiceAccount still holds it.
		c.editKustomization(t, ns, func(got *unstructured.Unstructured, _ any) {
			l := got.GetLabels()
			restore(l)
			got.SetLabels(l)
		}, nil)
		c.eventuallyAbsent(t, kustomizationGVK, fluxNS, ns)
		stopsAt("ServiceAccount")

		// Both labels back, the erasure ends.
		editSA(restore)
		c.finalizeNamespaceWhenDeleted(t, ns)
		c.gone(t, org)
		err := c.admin.Get(t.Context(), saKey, &corev1.ServiceAccount{})
		assert.True(t, apierrors.IsNotFound(err), "the Flux ServiceAccount outlived the organization: %v", err)
	})

	t.Run("an unlabelled Flux object deleted by hand holds the erasure until a finalizer lets it go", func(t *testing.T) {
		org := c.organization(t, "org-git-terminating", "ledger")
		ns := c.readyNamespace(t, org).Name
		org = c.get(t, org)
		kustomization, serviceAccount := c.holdFluxObjects(t, ns)
		t.Cleanup(func() {
			c.release(context.Background(), kustomization)
			c.release(context.Background(), serviceAccount)
		})
		c.editKustomization(t, ns, func(got *unstructured.Unstructured, _ any) {
			l := got.GetLabels()
			delete(l, v1alpha1.OrganizationUIDLabel)
			delete(l, controller.ObjectLabel)
			got.SetLabels(l)
		}, nil)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			sa := &corev1.ServiceAccount{}
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKeyFromObject(serviceAccount), sa)) {
				delete(sa.Labels, v1alpha1.OrganizationUIDLabel)
				delete(sa.Labels, controller.ObjectLabel)
				assert.NoError(ct, c.admin.Update(t.Context(), sa))
			}
		}, timeout, tick)
		require.NoError(t, c.admin.Delete(t.Context(), kustomization))
		require.NoError(t, c.admin.Delete(t.Context(), serviceAccount))

		waitsFor := func(kind, step string, namespaces int) {
			t.Helper()
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.get(ct, org)
				ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
				if assert.NotNil(ct, ready) {
					assert.Equal(ct, "Deleting", ready.Reason)
					assert.Contains(ct, ready.Message, "waiting until these are gone: ")
					assert.Contains(ct, ready.Message, kind+" "+fluxNS+"/"+ns)
				}
				assert.Equal(ct, step, got.Status.ErasureStep)
			}, timeout, tick)
			require.Never(t, func() bool {
				got := &v1alpha1.AgentPlatformOrganization{}
				err := c.admin.Get(context.Background(), client.ObjectKeyFromObject(org), got)
				return err != nil || got.Status.ErasureStep != step
			}, time.Second, tick, "the erasure left the %s step while the %s was terminating", step, kind)
			require.Len(t, c.namespacesOf(t, org), namespaces)
		}
		require.NoError(t, c.admin.Delete(t.Context(), org))
		waitsFor("Kustomization", v1alpha1.ErasureWorkloads, 1)

		c.finalizeNamespaceWhenDeleted(t, ns)
		c.release(t.Context(), kustomization)
		waitsFor("ServiceAccount", v1alpha1.ErasureOutsideObjects, 0)

		c.release(t.Context(), serviceAccount)
		c.gone(t, org)
		require.Empty(t, c.namespacesOf(t, org))
	})

	t.Run("a parent binding named like the organization's Flux binding is not mirrored", func(t *testing.T) {
		org := c.organization(t, "org-git-taken", "ledger")
		ns := c.readyNamespace(t, org).Name
		c.create(t, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: controller.FluxRoleBinding, Namespace: "org-git-taken"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: controller.OrganizationsAdminClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "dave@example.com"}},
		})
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NameTaken", ready.Reason)
				assert.Contains(ct, ready.Message, "org-git-taken/"+controller.FluxRoleBinding)
			}
		}, timeout, tick)
		// The organization keeps its Flux binding and its Kustomization.
		rb := &rbacv1.RoleBinding{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: controller.FluxRoleBinding}, rb))
		assert.Equal(t, organizationFluxCR, rb.RoleRef.Name)
		c.eventually(t, kustomizationGVK, fluxNS, ns, nil)
	})

	t.Run("an organization missing from the installation Kustomization's inventory is Orphaned until it is back", func(t *testing.T) {
		const parent = "org-orphans"
		createNamespace(t, c.admin, parent)
		// Flux's inventory ID of an AgentPlatformOrganization.
		entry := func(name string) any {
			return map[string]any{"id": parent + "_" + name + "_agent-platform.giantswarm.io_AgentPlatformOrganization", "v": "v1alpha1"}
		}
		c.create(t, newObject(kustomizationGVK, fluxNS, installationKS, nil, map[string]any{"path": "./management-clusters/test", "prune": true, "deletionPolicy": "Orphan"}))
		// The status kustomize-controller records: settled (Ready for its
		// generation), or reconciling, with the inventory of the last apply.
		setStatus := func(settled bool, entries ...any) {
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.read(t, kustomizationGVK, fluxNS, installationKS)
				condition := map[string]any{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded"}
				if !settled {
					condition = map[string]any{"type": "Reconciling", "status": "True", "reason": "Progressing"}
				}
				got.Object["status"] = map[string]any{
					"observedGeneration": got.GetGeneration(),
					"conditions":         []any{condition},
					"inventory":          map[string]any{"entries": entries},
				}
				assert.NoError(ct, c.admin.Status().Update(t.Context(), got))
			}, timeout, tick)
		}
		setInventory := func(entries ...any) { setStatus(true, entries...) }
		setInventory(entry("treasury"))

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

		// While Flux reconciles, an organization it labelled may not be in the
		// inventory yet: it is not Orphaned for that.
		setStatus(false)
		assert.Never(t, func() bool {
			cond := meta.FindStatusCondition(c.get(t, org).Status.Conditions, v1alpha1.ConditionOrphaned)
			return cond == nil || cond.Status != metav1.ConditionFalse
		}, 2*time.Second, tick, "Orphaned changed while the installation Kustomization reconciled")

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

		// The two-step delete: the delete pull request merged (DeleteMerged,
		// no workers), then the organization is deleted. Its workers run
		// again until its sessions are gone, so kagent can suspend its
		// actors; its Kustomization goes first, its Flux ServiceAccount last.
		setInventory()
		c.orphaned(t, org, metav1.ConditionTrue, v1alpha1.ReasonDeleteMerged)
		c.replicas(t, ns, 0)
		c.sessions.add(ns, "alice@example.com", "44444444-4444-4444-4444-444444444444")
		kustomization, serviceAccount := c.holdFluxObjects(t, ns)
		org = c.get(t, org)
		c.observeErasure(t, org, ns, kustomization, serviceAccount)
		require.NoError(t, c.admin.Delete(t.Context(), org))
		c.gone(t, org)
		events := c.log.of(ns, org.Status.SnapshotPrefix)
		t.Logf("erasure: %v", events)
		workers := all(events, evWorkers)
		require.Len(t, workers, 3, "the pool's replicas: %v", workers)
		assert.Equal(t, []string{"0", "3", "0"}, []string{workers[0].detail, workers[1].detail, workers[2].detail})
		assert.Less(t, slices.Index(events, workers[1]), firstIndex(events, evSessionsListed), "the workers did not run before the sessions were listed")
		assert.Greater(t, slices.Index(events, workers[2]), firstIndex(events, evNoActor))
		for _, e := range events[:max(firstIndex(events, evKustomizationDeleted), 0)] {
			assert.Equal(t, evWorkers, e.what, "%s came before the Kustomization was deleted", e)
		}
		assert.Equal(t, len(events)-1, lastIndex(events, evFluxSADeleted), "the Flux ServiceAccount was not the last object deleted")

		// An organization applied without git, missing from the same
		// inventory, is never Orphaned.
		applied := c.organizationWith(t, parent, "applied", v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: 3}})
		appliedNS := c.readyNamespace(t, applied).Name
		c.modelConfig(t, appliedNS, "anthropic", false)
		c.replicas(t, appliedNS, 3)
		assert.Nil(t, meta.FindStatusCondition(c.get(t, applied).Status.Conditions, v1alpha1.ConditionOrphaned))
	})
}

// editKustomization sets one value of the organization Kustomization fluxNS/name, as the admin.
func (c *cluster) editKustomization(t *testing.T, name string, set func(*unstructured.Unstructured, any), v any) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		got := c.read(t, kustomizationGVK, fluxNS, name)
		set(got, v)
		assert.NoError(ct, c.admin.Update(t.Context(), got))
	}, timeout, tick)
}

// finalizeNamespaceWhenDeleted stands in for the namespace controller until
// the test ends: once ns is terminating, it ends it.
func (c *cluster) finalizeNamespaceWhenDeleted(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			ns := &corev1.Namespace{}
			if err := c.admin.Get(ctx, client.ObjectKey{Name: name}, ns); err == nil && ns.DeletionTimestamp != nil {
				ns.Spec.Finalizers = nil
				_ = c.admin.SubResource("finalize").Update(ctx, ns)
			}
		}
	}()
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
