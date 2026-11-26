package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"
)

// templateRefField is an Agent's and a sub-agent's reference to an
// AgentTemplate.
const templateRefField = "templateRef"

// referenceCases are the cases of the reference rule of the curated copies,
// run in TestOrganizationEnvtest: a curated Agent is copied only when every
// object it refers to is a curated copy in the organization namespace.
func referenceCases(t *testing.T, c *cluster) {
	t.Run("a curated Agent on a Harness outside spec.capabilities.agentPlatform.harnesses is left out until the organization adds it", func(t *testing.T) {
		org := c.organizationWith(t, "refs-harness", orgv1.AgentPlatformSpec{Workers: withReplicas(1)})
		ns := c.readyNamespace(t, org).Name
		// The curated AgentTemplate names no Harness: its copy shows that
		// the curated objects were copied.
		c.eventually(t, templateGVK, ns, curatedTemplate, nil)
		c.never(t, agentGVK, ns, curatedAgent)
		c.absent(t, harnessGVK, ns, curatedHarness)

		setHarnesses := func(harnesses []string) {
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				got := c.get(ct, org)
				got.Spec.Capabilities.AgentPlatform.Harnesses = harnesses
				assert.NoError(ct, c.admin.Update(t.Context(), got))
			}, timeout, tick)
		}
		setHarnesses([]string{curatedHarness})
		c.eventually(t, harnessGVK, ns, curatedHarness, nil)
		c.eventually(t, agentGVK, ns, curatedAgent, nil)

		// The Harness goes out of spec.capabilities.agentPlatform.harnesses: so
		// does the Agent's copy.
		setHarnesses(nil)
		c.eventuallyAbsent(t, agentGVK, ns, curatedAgent)
		c.eventuallyAbsent(t, harnessGVK, ns, curatedHarness)
	})

	t.Run("a curated Agent is copied only when each object it refers to is curated", func(t *testing.T) {
		org := c.organization(t, "refs")
		ns := c.readyNamespace(t, org).Name
		c.eventually(t, agentGVK, ns, curatedAgent, nil)

		server := nameRef("ref-server")
		server[kindField] = serverGVK.Kind
		for _, tc := range []struct {
			desc string
			// agent is the curated Agent's spec; ref the object it refers
			// to, of gvk.
			agent map[string]any
			gvk   schema.GroupVersionKind
			ref   string
			spec  map[string]any
		}{
			{
				desc:  "its templateRef",
				agent: map[string]any{harnessRefField: nameRef(curatedHarness), templateRefField: nameRef("ref-template")},
				gvk:   templateGVK, ref: "ref-template", spec: templateSpec("Referenced.", nil),
			},
			{
				desc: "a sub-agent's templateRef",
				agent: agentSpec("With a sub-agent.", map[string]any{toolsField: []any{map[string]any{"subAgent": map[string]any{
					nameField: "helper", descriptionField: "Helps.", templateRefField: nameRef("ref-subagent"),
				}}}}),
				gvk: templateGVK, ref: "ref-subagent", spec: templateSpec("Sub-agent.", nil),
			},
			{
				desc:  "the RemoteMCPServer of a tool",
				agent: agentSpec("With a server.", map[string]any{toolsField: []any{map[string]any{"mcp": map[string]any{"server": server}}}}),
				gvk:   serverGVK, ref: "ref-server", spec: map[string]any{"url": "https://tools.example.com/mcp", descriptionField: "tools"},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				name := "refs-" + tc.ref
				c.createSource(t, tc.gvk, tc.ref, nil, tc.spec)
				c.createSource(t, agentGVK, name, curatedLabels, tc.agent)

				// Not curated: no copy, also while the organization holds an
				// object of that name of its own.
				own := newObject(tc.gvk, ns, tc.ref, nil, tc.spec)
				require.NoError(t, c.admin.Create(t.Context(), own))
				c.never(t, agentGVK, ns, name)
				require.NoError(t, c.admin.Delete(t.Context(), own))
				c.eventuallyAbsent(t, tc.gvk, ns, tc.ref)

				// Curated: the Agent is copied with it.
				c.setSourceLabels(t.Context(), t, tc.gvk, tc.ref, curatedLabels)
				c.eventually(t, tc.gvk, ns, tc.ref, nil)
				c.eventually(t, agentGVK, ns, name, nil)

				// No longer curated: the Agent's copy goes.
				c.setSourceLabels(t.Context(), t, tc.gvk, tc.ref, nil)
				c.eventuallyAbsent(t, agentGVK, ns, name)
				c.eventuallyAbsent(t, tc.gvk, ns, tc.ref)
			})
		}

		t.Run("a ConfigMap, curated or not", func(t *testing.T) {
			const name, prompt = "refs-configmap", "ref-prompt"
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: prompt, Namespace: platformNS, Labels: curatedLabels}, Data: map[string]string{"prompt": "Help."}}
			require.NoError(t, c.admin.Create(t.Context(), cm))
			t.Cleanup(func() { _ = c.admin.Delete(context.WithoutCancel(t.Context()), cm) })
			c.createSource(t, agentGVK, name, curatedLabels, map[string]any{
				harnessRefField: nameRef(curatedHarness),
				templateField:   map[string]any{"systemPromptFrom": map[string]any{nameField: prompt, "key": "prompt"}},
			})
			c.never(t, agentGVK, ns, name)
			c.absent(t, corev1.SchemeGroupVersion.WithKind("ConfigMap"), ns, prompt)
		})
	})

	t.Run("a curated AgentTemplate whose name the organization holds leaves the Agent that refers to it out", func(t *testing.T) {
		org := c.organization(t, "refs-taken")
		ns := c.readyNamespace(t, org).Name
		const name, ref = "refs-taken", "ref-taken"
		own := newObject(templateGVK, ns, ref, nil, templateSpec("The organization's.", nil))
		require.NoError(t, c.admin.Create(t.Context(), own))

		c.createSource(t, templateGVK, ref, curatedLabels, templateSpec("Curated.", nil))
		c.createSource(t, agentGVK, name, curatedLabels, map[string]any{harnessRefField: nameRef(curatedHarness), templateRefField: nameRef(ref)})
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			ready := meta.FindStatusCondition(c.get(ct, org).Status.Conditions, orgv1.AgentPlatformReady)
			if assert.NotNil(ct, ready) {
				assert.Equal(ct, metav1.ConditionFalse, ready.Status)
				assert.Equal(ct, "NameTaken", ready.Reason)
			}
		}, timeout, tick)
		c.never(t, agentGVK, ns, name)
		assert.Equal(t, "The organization's.", str(c.read(t, templateGVK, ns, ref), "spec", promptField), "the organization's AgentTemplate was overwritten")

		// The name is free: the template and the Agent are copied, at the
		// next reconcile (the sync period: the organization's own objects
		// are not watched).
		require.NoError(t, c.admin.Delete(t.Context(), own))
		c.eventuallyAbsent(t, templateGVK, ns, ref)
		c.poke(t, org)
		c.eventually(t, agentGVK, ns, name, nil)
		c.eventually(t, templateGVK, ns, ref, func(ct *assert.CollectT, tp *unstructured.Unstructured) {
			assert.Equal(ct, "Curated.", str(tp, "spec", promptField))
		})
	})

	t.Run("a copy outlives no copy it refers to, and one being deleted is no copy to refer to", func(t *testing.T) {
		org := c.organization(t, "refs-held")
		ns := c.readyNamespace(t, org).Name
		const ref, name, later = "ref-held", "refs-held", "refs-held-later"
		c.createSource(t, templateGVK, ref, curatedLabels, templateSpec("Held.", nil))
		c.createSource(t, agentGVK, name, curatedLabels, map[string]any{harnessRefField: nameRef(curatedHarness), templateRefField: nameRef(ref)})
		c.eventually(t, agentGVK, ns, name, nil)
		c.eventually(t, templateGVK, ns, ref, nil)

		// The template is no longer curated: the Agent's copy goes first,
		// and the template's only once the Agent's is gone.
		c.setFinalizers(t, agentGVK, ns, name, []string{holdFinalizer})
		c.setSourceLabels(t.Context(), t, templateGVK, ref, nil)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			assert.NotNil(ct, c.read(t, agentGVK, ns, name).GetDeletionTimestamp())
		}, timeout, tick)
		c.staysPresent(t, templateGVK, ns, ref)
		c.setFinalizers(t, agentGVK, ns, name, nil)
		c.eventuallyAbsent(t, agentGVK, ns, name)
		c.eventuallyAbsent(t, templateGVK, ns, ref)

		// Curated again, then its copy is being deleted: an Agent that
		// refers to it is copied only once it is copied again.
		c.setSourceLabels(t.Context(), t, templateGVK, ref, curatedLabels)
		c.eventually(t, agentGVK, ns, name, nil)
		c.setFinalizers(t, templateGVK, ns, ref, []string{holdFinalizer})
		require.NoError(t, c.admin.Delete(t.Context(), c.read(t, templateGVK, ns, ref)))
		c.createSource(t, agentGVK, later, curatedLabels, map[string]any{harnessRefField: nameRef(curatedHarness), templateRefField: nameRef(ref)})
		c.never(t, agentGVK, ns, later)
		c.staysPresent(t, agentGVK, ns, name)
		c.setFinalizers(t, templateGVK, ns, ref, nil)
		c.eventually(t, agentGVK, ns, later, nil)
		c.eventually(t, templateGVK, ns, ref, func(ct *assert.CollectT, tp *unstructured.Unstructured) {
			assert.Nil(ct, tp.GetDeletionTimestamp())
		})
	})
}

// setFinalizers sets the finalizers of the object of gvk ns/name.
func (c *cluster) setFinalizers(t *testing.T, gvk schema.GroupVersionKind, ns, name string, finalizers []string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if !assert.NoError(ct, c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u)) {
			return
		}
		u.SetFinalizers(finalizers)
		assert.NoError(ct, c.admin.Update(t.Context(), u))
	}, timeout, tick)
}

// staysPresent asserts that the object of gvk ns/name exists for a while.
func (c *cluster) staysPresent(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	require.Never(t, func() bool {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		return apierrors.IsNotFound(c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u))
	}, 2*time.Second, tick, "%s %s/%s went", gvk.Kind, ns, name)
}

// createSource creates the platform object of gvk named name, deleted when the
// test ends.
func (c *cluster) createSource(t *testing.T, gvk schema.GroupVersionKind, name string, labels map[string]string, spec map[string]any) {
	t.Helper()
	src := newObject(gvk, platformNS, name, labels, spec)
	require.NoError(t, c.admin.Create(t.Context(), src))
	t.Cleanup(func() {
		if err := c.admin.Delete(context.WithoutCancel(t.Context()), src); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete %s %s: %v", gvk.Kind, name, err)
		}
	})
}

// setSourceLabels sets the labels of the platform object of gvk named name.
func (c *cluster) setSourceLabels(ctx context.Context, t *testing.T, gvk schema.GroupVersionKind, name string, labels map[string]string) {
	t.Helper()
	c.editSource(ctx, t, gvk, name, func(source *unstructured.Unstructured) error {
		source.SetLabels(labels)
		return nil
	})
}

// never asserts that no object of gvk ns/name appears for a while: the
// reconciles the last change started have run.
func (c *cluster) never(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	require.Never(t, func() bool {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		return c.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u) == nil
	}, 2*time.Second, tick, "%s %s/%s was copied", gvk.Kind, ns, name)
}
