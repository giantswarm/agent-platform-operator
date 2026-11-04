package controller_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

// curatedCases are the cases of the curated copies, the worker replica rule,
// the default ModelConfig and the object quota, run in TestOrganizationEnvtest.
func curatedCases(t *testing.T, c *cluster) {
	t.Run("an organization gets read-only copies of the curated objects", func(t *testing.T) {
		org := c.organizationWith(t, "org-curated", "curated", v1alpha1.AgentPlatformOrganizationSpec{Harnesses: []string{curatedHarness}})
		ns := c.readyNamespace(t, org).Name
		org = c.get(t, org)

		harness := c.eventually(t, harnessGVK, ns, curatedHarness, func(ct *assert.CollectT, h *unstructured.Unstructured) {
			assert.Equal(ct, curatedPool, str(h, "spec", "substrate", "workerPoolRef", "name"))
		})
		assert.Equal(t, "s3://"+snapshotBucket+"/"+string(org.UID)+"/"+curatedHarness, str(harness, "spec", "substrate", "snapshotPolicy", "location"))
		assert.Equal(t, harnessImage, str(harness, "spec", "workload", "image"))
		_, memory, _ := unstructured.NestedMap(harness.Object, "spec", "kagent", "memory")
		assert.False(t, memory, "kagent memory is on in the Harness copy")
		threshold, _, _ := unstructured.NestedInt64(harness.Object, "spec", "kagent", "compaction", "tokenThreshold")
		assert.EqualValues(t, 1000, threshold, "the compaction settings were not copied")
		_, summarizer, _ := unstructured.NestedMap(harness.Object, "spec", "kagent", "compaction", "summarizer", "modelConfigRef")
		assert.False(t, summarizer, "the Harness copy names the platform's summarizer ModelConfig")
		assert.Equal(t, controller.ScopeCurated, harness.GetLabels()[controller.ObjectLabel])
		assert.Equal(t, platformNS+"/"+curatedHarness, harness.GetAnnotations()[controller.CopiedFromAnnotation])

		pool := c.eventually(t, poolGVK, ns, curatedPool, nil)
		assert.Equal(t, workerImage, str(pool, "spec", "workerImage"))
		assert.Equal(t, poolSA, str(pool, "spec", "template", "serviceAccountName"))
		assert.Equal(t, curatedPool, pool.GetLabels()["kagent.dev/worker-pool-name"])
		assert.Equal(t, ns, pool.GetLabels()["kagent.dev/worker-pool-namespace"])
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: poolSA}, &corev1.ServiceAccount{}))

		agent := c.eventually(t, agentGVK, ns, curatedAgent, nil)
		assert.Equal(t, agentPrompt, str(agent, "spec", "template", promptField))
		assert.Equal(t, controller.ScopeCurated, agent.GetLabels()[controller.ObjectLabel])
		c.eventually(t, templateGVK, ns, curatedTemplate, nil)
		server := c.eventually(t, serverGVK, ns, musterServer, nil)
		assert.Equal(t, "https://muster.example.com/mcp", str(server, "spec", "url"))
		assert.Equal(t, "disabled", server.GetLabels()[discoveryLabel], "the copy runs kagent's tool discovery against muster")
		assert.NotContains(t, server.GetLabels(), "app.kubernetes.io/name")
		c.absent(t, agentGVK, ns, platformAgent)

		// A field added to a copy is removed, a changed one restored.
		require.NoError(t, unstructured.SetNestedField(harness.Object, "embeddings", "spec", "kagent", "memory", "modelConfigRef", "name"))
		require.NoError(t, c.admin.Update(t.Context(), harness))
		harness = c.eventually(t, harnessGVK, ns, curatedHarness, func(ct *assert.CollectT, h *unstructured.Unstructured) {
			_, memory, _ := unstructured.NestedMap(h.Object, "spec", "kagent", "memory")
			assert.False(ct, memory, "kagent memory added to the copy stays")
		})
		require.NoError(t, unstructured.SetNestedField(harness.Object, "s3://elsewhere/x", "spec", "substrate", "snapshotPolicy", "location"))
		require.NoError(t, c.admin.Update(t.Context(), harness))
		c.eventually(t, harnessGVK, ns, curatedHarness, func(ct *assert.CollectT, h *unstructured.Unstructured) {
			assert.Equal(ct, "s3://"+snapshotBucket+"/"+string(org.UID)+"/"+curatedHarness, str(h, "spec", "substrate", "snapshotPolicy", "location"))
		})
		agent = c.eventually(t, agentGVK, ns, curatedAgent, nil)
		require.NoError(t, unstructured.SetNestedField(agent.Object, "Do anything.", "spec", "template", promptField))
		require.NoError(t, c.admin.Update(t.Context(), agent))
		c.eventually(t, agentGVK, ns, curatedAgent, func(ct *assert.CollectT, a *unstructured.Unstructured) {
			assert.Equal(ct, agentPrompt, str(a, "spec", "template", promptField))
		})

		// A Harness taken out of spec.harnesses goes.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			got.Spec.Harnesses = nil
			assert.NoError(ct, c.admin.Update(t.Context(), got))
		}, timeout, tick)
		c.eventuallyAbsent(t, harnessGVK, ns, curatedHarness)
	})

	t.Run("the pool runs no workers without a ModelConfig or while the organization is Orphaned", func(t *testing.T) {
		org := c.organizationWith(t, "org-workers", "workers", v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: 3}})
		ns := c.readyNamespace(t, org).Name
		c.replicas(t, ns, 0)

		c.modelConfig(t, ns, "anthropic", false)
		c.replicas(t, ns, 3)

		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := c.get(ct, org)
			meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionOrphaned, Status: metav1.ConditionTrue, Reason: "MissingFromGit"})
			assert.NoError(ct, c.admin.Status().Update(t.Context(), got))
		}, timeout, tick)
		c.replicas(t, ns, 0)
	})

	t.Run("the curated Agents run on the one ModelConfig marked as the default", func(t *testing.T) {
		org := c.organization(t, "org-models", "models")
		ns := c.readyNamespace(t, org).Name
		c.defaultModel(t, org, ns, metav1.ConditionFalse, "")

		c.modelConfig(t, ns, "first", true)
		c.defaultModel(t, org, ns, metav1.ConditionTrue, "first")

		second := c.modelConfig(t, ns, "second", true)
		c.defaultModel(t, org, ns, metav1.ConditionFalse, "")

		// The mark moves: first loses it.
		first := c.read(t, modelGVK, ns, "first")
		first.SetAnnotations(nil)
		require.NoError(t, c.admin.Update(t.Context(), first))
		c.defaultModel(t, org, ns, metav1.ConditionTrue, second.GetName())
	})

	t.Run("a change of a curated source is copied into every organization", func(t *testing.T) {
		harnesses := v1alpha1.AgentPlatformOrganizationSpec{Harnesses: []string{curatedHarness}}
		one := c.organizationWith(t, "org-resync-one", "one", harnesses)
		two := c.organizationWith(t, "org-resync-two", "two", harnesses)
		namespaces := []string{c.readyNamespace(t, one).Name, c.readyNamespace(t, two).Name}

		source := c.read(t, harnessGVK, platformNS, curatedHarness)
		require.NoError(t, unstructured.SetNestedField(source.Object, harnessImage+"bbbb", "spec", "workload", "image"))
		require.NoError(t, c.admin.Update(t.Context(), source))

		added := newObject(agentGVK, platformNS, "cost-advisor", curatedLabels, agentSpec("Costs.", nil))
		require.NoError(t, c.admin.Create(t.Context(), added))

		for _, ns := range namespaces {
			c.eventually(t, harnessGVK, ns, curatedHarness, func(ct *assert.CollectT, h *unstructured.Unstructured) {
				assert.Equal(ct, harnessImage+"bbbb", str(h, "spec", "workload", "image"))
			})
			c.eventually(t, agentGVK, ns, "cost-advisor", nil)
		}

		// An Agent that is no longer curated goes from every organization.
		added = c.read(t, agentGVK, platformNS, "cost-advisor")
		added.SetLabels(nil)
		require.NoError(t, c.admin.Update(t.Context(), added))
		for _, ns := range namespaces {
			c.eventuallyAbsent(t, agentGVK, ns, "cost-advisor")
		}
	})

	t.Run("the object quota refuses objects over the count", func(t *testing.T) {
		org := c.organization(t, "org-quota", "quota")
		ns := c.readyNamespace(t, org).Name
		quota := &corev1.ResourceQuota{}
		require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: controller.ObjectQuotaName}, quota))
		assert.True(t, quota.Spec.Hard[corev1.ResourceConfigMaps].Equal(resource.MustParse(fmt.Sprint(configMapQuota))))
		// From the recorded usage on, the quota admission counts.
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			got := &corev1.ResourceQuota{}
			if assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKeyFromObject(quota), got)) {
				assert.NotEmpty(ct, got.Status.Hard)
			}
		}, timeout, tick)

		for i := range configMapQuota {
			require.NoError(t, c.admin.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cm-%d", i), Namespace: ns}}))
		}
		err := c.admin.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "one-too-many", Namespace: ns}})
		require.True(t, apierrors.IsForbidden(err), "a ConfigMap over the quota was not refused: %v", err)
		assert.Contains(t, err.Error(), "exceeded quota: "+controller.ObjectQuotaName)
	})
}

// eventually waits until the object of gvk ns/name exists and passes check
// (nil: any), and returns it.
func (c *cluster) eventually(t *testing.T, gvk schema.GroupVersionKind, ns, name string, check func(*assert.CollectT, *unstructured.Unstructured)) *unstructured.Unstructured {
	t.Helper()
	var got *unstructured.Unstructured
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if !assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u)) {
			return
		}
		if check != nil {
			check(ct, u)
		}
		got = u
	}, timeout, tick)
	return got
}

// read reads the object of gvk ns/name.
func (c *cluster) read(t *testing.T, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	require.NoError(t, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u))
	return u
}

// absent asserts that no object of gvk ns/name exists.
func (c *cluster) absent(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u)
	assert.True(t, apierrors.IsNotFound(err), "%s %s/%s exists: %v", gvk.Kind, ns, name, err)
}

// eventuallyAbsent waits until no object of gvk ns/name exists.
func (c *cluster) eventuallyAbsent(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u)
		assert.True(ct, apierrors.IsNotFound(err), "%s %s/%s is still there: %v", gvk.Kind, ns, name, err)
	}, timeout, tick)
}

// replicas waits until the organization's pool copy in ns runs n replicas.
func (c *cluster) replicas(t *testing.T, ns string, n int64) {
	t.Helper()
	c.eventually(t, poolGVK, ns, curatedPool, func(ct *assert.CollectT, pool *unstructured.Unstructured) {
		got, found, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		assert.True(ct, found)
		assert.Equal(ct, n, got)
	})
}

// modelConfig creates a ModelConfig in ns, marked as the default when
// marked.
func (c *cluster) modelConfig(t *testing.T, ns, name string, marked bool) *unstructured.Unstructured {
	t.Helper()
	mc := newObject(modelGVK, ns, name, nil, map[string]any{"provider": "Anthropic", "model": "claude"})
	if marked {
		mc.SetAnnotations(map[string]string{v1alpha1.DefaultModelConfigAnnotation: "true"})
	}
	require.NoError(t, c.admin.Create(t.Context(), mc))
	return mc
}

// defaultModel waits until org's DefaultModelConfigured is status and the
// curated Agent and AgentTemplate copies in ns name model ("" names none).
func (c *cluster) defaultModel(t *testing.T, org *v1alpha1.AgentPlatformOrganization, ns string, status metav1.ConditionStatus, model string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cond := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, v1alpha1.ConditionDefaultModelConfigured)
		if assert.NotNil(ct, cond) {
			assert.Equal(ct, status, cond.Status, "DefaultModelConfigured: %s", cond.Message)
		}
	}, timeout, tick)
	hasModel := func(ct *assert.CollectT, spec map[string]any) {
		name, found, _ := unstructured.NestedString(spec, "modelConfig", "name")
		if model == "" {
			assert.False(ct, found, "the copy names the ModelConfig %q", name)
			return
		}
		assert.Equal(ct, model, name)
	}
	c.eventually(t, agentGVK, ns, curatedAgent, func(ct *assert.CollectT, a *unstructured.Unstructured) {
		tmpl, _, _ := unstructured.NestedMap(a.Object, "spec", "template")
		hasModel(ct, tmpl)
	})
	c.eventually(t, templateGVK, ns, curatedTemplate, func(ct *assert.CollectT, tp *unstructured.Unstructured) {
		spec, _, _ := unstructured.NestedMap(tp.Object, "spec")
		hasModel(ct, spec)
	})
}

func str(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}
