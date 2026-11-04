package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

const (
	// ScopeCurated marks, as the value of ObjectLabel, a read-only copy of a
	// curated object of the platform namespace: a Harness, the WorkerPool and
	// its ServiceAccount, an Agent, an AgentTemplate or a RemoteMCPServer. The
	// copy is kept while its source is curated for the organization and is
	// brought back to its source on every reconcile.
	ScopeCurated = "curated"

	// The placement labels kagent reads from a WorkerPool: the actors of a
	// namespace select the workers of their own namespace's pool by them.
	workerPoolNameLabel      = "kagent.dev/worker-pool-name"
	workerPoolNamespaceLabel = "kagent.dev/worker-pool-namespace"

	// fieldName is the name field of an object's metadata and of a reference.
	fieldName = "name"
	// kagentLabelPrefix starts the labels kagent reads from an object's
	// metadata (kagent.dev/discovery on a RemoteMCPServer, say): a copy
	// carries its source's.
	kagentLabelPrefix = "kagent.dev/"

	reasonDefaultModelConfigMarked = "DefaultModelConfigMarked"
	reasonNoDefaultModelConfig     = "NoDefaultModelConfig"
	reasonSeveralDefaults          = "SeveralDefaultModelConfigs"
)

// CuratedConfig is what the reconciler copies from the platform namespace.
// The zero value copies nothing and watches none of the kinds.
type CuratedConfig struct {
	// WorkerPool is the name of the curated WorkerPool; empty copies nothing
	// curated.
	WorkerPool string
	// Kagent is the served version of api.kagent.dev: Agent, AgentTemplate,
	// Harness, ModelConfig, RemoteMCPServer.
	Kagent schema.GroupVersion
	// Substrate is the served version of ate.dev: WorkerPool.
	Substrate schema.GroupVersion
}

// Enabled reports whether the reconciler copies the curated objects.
func (c CuratedConfig) Enabled() bool { return c.WorkerPool != "" }

func (c CuratedConfig) agent() schema.GroupVersionKind    { return c.Kagent.WithKind("Agent") }
func (c CuratedConfig) template() schema.GroupVersionKind { return c.Kagent.WithKind("AgentTemplate") }
func (c CuratedConfig) harness() schema.GroupVersionKind  { return c.Kagent.WithKind("Harness") }
func (c CuratedConfig) modelConfig() schema.GroupVersionKind {
	return c.Kagent.WithKind("ModelConfig")
}
func (c CuratedConfig) server() schema.GroupVersionKind { return c.Kagent.WithKind("RemoteMCPServer") }
func (c CuratedConfig) pool() schema.GroupVersionKind   { return c.Substrate.WithKind("WorkerPool") }

// copied are the kinds of the curated copies.
func (c CuratedConfig) copied() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{c.harness(), c.pool(), c.agent(), c.template(), c.server()}
}

// Kinds are every kind the curated copies read, write or watch: the API
// server must serve them all.
func (c CuratedConfig) Kinds() []schema.GroupVersionKind {
	return append(c.copied(), c.modelConfig())
}

// curated is one reconcile's record of the copies it keeps, by kind and name,
// and of the kinds whose sources it read in full: only a copy of such a kind
// is pruned when it is not kept.
type curated struct {
	keep     map[schema.GroupVersionKind]map[string]bool
	complete map[schema.GroupVersionKind]bool
}

func (c *curated) kept(gvk schema.GroupVersionKind, name string) {
	if c.keep[gvk] == nil {
		c.keep[gvk] = map[string]bool{}
	}
	c.keep[gvk][name] = true
}

// keepCurated keeps the curated copies in ns. It follows a curated Agent's
// templateRef and the RemoteMCPServers the templates' tools name, not a
// subAgent's template or the ConfigMaps systemPromptFrom, outputSchemaFrom
// and promptTemplate name: the v1 curated set uses none of them. It sets the
// organization's DefaultModelConfigured condition in status, and removes the
// copies whose source is no longer curated for the organization.
func (r *OrganizationReconciler) keepCurated(ctx context.Context, org *v1alpha1.AgentPlatformOrganization, ns string, status *v1alpha1.AgentPlatformOrganizationStatus) error {
	c := r.Config.Curated
	models, err := r.modelConfigs(ctx, ns)
	if err != nil {
		// Unknown: no copy changes and the pool keeps its replicas.
		return err
	}
	defaultModel, cond := defaultModelConfig(models, ns, org.Generation)
	meta.SetStatusCondition(&status.Conditions, cond)

	prefix := r.snapshotPrefix(org)
	if prefix == "" {
		return errors.New("the curated Harnesses are not copied: the operator has no snapshot bucket")
	}
	k := &curated{keep: map[schema.GroupVersionKind]map[string]bool{}, complete: map[schema.GroupVersionKind]bool{}}
	errs := []error{r.copyWorkerPool(ctx, ns, workerReplicas(org, len(models) > 0), k)}

	for _, name := range org.Spec.Harnesses {
		k.kept(c.harness(), name)
		errs = append(errs, r.copyCurated(ctx, ns, c.harness(), name, func(spec map[string]any) error {
			return rewriteHarness(spec, c.WorkerPool, prefix+"/"+name)
		}))
	}
	k.complete[c.harness()] = true

	var servers []string
	agents, err := r.curatedSources(ctx, c.agent())
	if err != nil {
		errs = append(errs, err)
	} else {
		k.complete[c.agent()] = true
	}
	var templates []string
	for i := range agents {
		src := &agents[i]
		k.kept(c.agent(), src.GetName())
		if ref, _, _ := unstructured.NestedString(src.Object, "spec", "templateRef", fieldName); ref != "" {
			templates = append(templates, ref)
		}
		tmpl, _, _ := unstructured.NestedMap(src.Object, "spec", "template")
		servers = append(servers, serversOf(tmpl)...)
		errs = append(errs, r.ensureCurated(ctx, ns, src, func(spec map[string]any) error {
			if tmpl, ok := spec["template"].(map[string]any); ok {
				setModelConfig(tmpl, defaultModel)
			}
			// An inline harness runs where a Harness copy would, under a
			// prefix no Harness name reaches.
			if h, ok := spec["harness"].(map[string]any); ok {
				return rewriteHarness(h, c.WorkerPool, prefix+"/agents/"+src.GetName())
			}
			return nil
		}))
	}

	labelled, err := r.curatedSources(ctx, c.template())
	if err != nil {
		errs = append(errs, err)
	} else if k.complete[c.agent()] {
		k.complete[c.template()] = true
	}
	for i := range labelled {
		templates = append(templates, labelled[i].GetName())
	}
	slices.Sort(templates)
	templates = slices.Compact(templates)
	read := 0
	for _, name := range templates {
		k.kept(c.template(), name)
		errs = append(errs, r.copyCurated(ctx, ns, c.template(), name, func(spec map[string]any) error {
			read++
			servers = append(servers, serversOf(spec)...)
			setModelConfig(spec, defaultModel)
			return nil
		}))
	}

	// The RemoteMCPServers the curated Agents reach their tools through.
	slices.Sort(servers)
	for _, name := range slices.Compact(servers) {
		k.kept(c.server(), name)
		errs = append(errs, r.copyCurated(ctx, ns, c.server(), name, nil))
	}
	// Every server a kept copy names is known only when every template was read.
	if k.complete[c.template()] && read == len(templates) {
		k.complete[c.server()] = true
	}
	errs = append(errs, r.pruneCurated(ctx, ns, k))
	return errors.Join(errs...)
}

// workerReplicas is the worker replica rule: the organization's
// spec.workers.replicas, but 0 while its namespace holds no ModelConfig (no
// agent can run without a model) and while it is Orphaned.
func workerReplicas(org *v1alpha1.AgentPlatformOrganization, holdsModelConfig bool) int64 {
	if !holdsModelConfig || meta.IsStatusConditionTrue(org.Status.Conditions, v1alpha1.ConditionOrphaned) {
		return 0
	}
	return int64(org.Spec.Workers.Replicas)
}

// modelConfigs are the ModelConfigs of ns that are not going away.
func (r *OrganizationReconciler) modelConfigs(ctx context.Context, ns string) ([]metav1.PartialObjectMetadata, error) {
	gvk := r.Config.Curated.modelConfig()
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list ModelConfigs in %s: %w", ns, err)
	}
	out := make([]metav1.PartialObjectMetadata, 0, len(list.Items))
	for _, mc := range list.Items {
		if mc.DeletionTimestamp == nil {
			out = append(out, mc)
		}
	}
	return out, nil
}

// defaultModelConfig is the name of the one ModelConfig marked as the
// default ("" when none or several are) and the DefaultModelConfigured
// condition that says which.
func defaultModelConfig(models []metav1.PartialObjectMetadata, ns string, generation int64) (string, metav1.Condition) {
	var marked []string
	for _, mc := range models {
		if mc.Annotations[v1alpha1.DefaultModelConfigAnnotation] == "true" {
			marked = append(marked, mc.Name)
		}
	}
	slices.Sort(marked)
	cond := metav1.Condition{Type: v1alpha1.ConditionDefaultModelConfigured, Status: metav1.ConditionFalse, ObservedGeneration: generation}
	switch len(marked) {
	case 0:
		cond.Reason = reasonNoDefaultModelConfig
		cond.Message = fmt.Sprintf("no ModelConfig in %s is annotated %s: \"true\", so the curated Agents have no model", ns, v1alpha1.DefaultModelConfigAnnotation)
		return "", cond
	case 1:
		cond.Status, cond.Reason = metav1.ConditionTrue, reasonDefaultModelConfigMarked
		cond.Message = fmt.Sprintf("the default ModelConfig is %s/%s", ns, marked[0])
		return marked[0], cond
	default:
		cond.Reason = reasonSeveralDefaults
		cond.Message = fmt.Sprintf("the ModelConfigs %s in %s are all annotated %s: \"true\"; mark one, so the curated Agents have a model", strings.Join(marked, ", "), ns, v1alpha1.DefaultModelConfigAnnotation)
		return "", cond
	}
}

// curatedSources are the objects of gvk in the platform namespace labelled
// CuratedLabel=true.
func (r *OrganizationReconciler) curatedSources(ctx context.Context, gvk schema.GroupVersionKind) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.Client.List(ctx, list, client.InNamespace(r.Config.PlatformNamespace), client.MatchingLabels{v1alpha1.CuratedLabel: "true"}); err != nil {
		return nil, fmt.Errorf("list the curated %ss in %s: %w", gvk.Kind, r.Config.PlatformNamespace, err)
	}
	return list.Items, nil
}

// copyWorkerPool copies the curated WorkerPool with replicas and its own
// placement labels, and the ServiceAccount its workers run as. The copy's
// spec is the curated spec but replicas: admission
// (agent-platform-tenant-workerpool-copy) refuses a WorkerPool copy with a
// field the curated one does not have.
func (r *OrganizationReconciler) copyWorkerPool(ctx context.Context, ns string, replicas int64, k *curated) error {
	c := r.Config.Curated
	k.kept(c.pool(), c.WorkerPool)
	k.complete[c.pool()] = true
	src, err := r.platformObject(ctx, c.pool(), c.WorkerPool)
	if err != nil {
		return err
	}
	pool := copyOf(src, ns)
	if err := unstructured.SetNestedField(pool.Object, replicas, "spec", "replicas"); err != nil {
		return err
	}
	pool.SetLabels(map[string]string{workerPoolNameLabel: c.WorkerPool, workerPoolNamespaceLabel: ns})
	var errs []error
	if sa, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "serviceAccountName"); sa != "" {
		errs = append(errs, r.ensureServiceAccount(ctx, ns, sa, ScopeCurated))
	}
	return errors.Join(append(errs, r.ensureScoped(ctx, ns, ScopeCurated, pool, objectRef(src), equal))...)
}

// copyCurated copies the platform object of gvk named name into ns, its spec
// passed through rewrite (nil: unchanged).
func (r *OrganizationReconciler) copyCurated(ctx context.Context, ns string, gvk schema.GroupVersionKind, name string, rewrite func(spec map[string]any) error) error {
	src, err := r.platformObject(ctx, gvk, name)
	if err != nil {
		return err
	}
	return r.ensureCurated(ctx, ns, src, rewrite)
}

// ensureCurated keeps a read-only copy of src in ns: its spec passed through
// rewrite, exactly, so an edit of the copy is reverted.
func (r *OrganizationReconciler) ensureCurated(ctx context.Context, ns string, src *unstructured.Unstructured, rewrite func(spec map[string]any) error) error {
	obj := copyOf(src, ns)
	labels := map[string]string{}
	for k, v := range src.GetLabels() {
		if strings.HasPrefix(k, kagentLabelPrefix) {
			labels[k] = v
		}
	}
	obj.SetLabels(labels)
	if rewrite != nil {
		spec, _ := obj.Object["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			obj.Object["spec"] = spec
		}
		if err := rewrite(spec); err != nil {
			return fmt.Errorf("copy %s %s: %w", src.GetKind(), objectRef(src), err)
		}
	}
	return r.ensureScoped(ctx, ns, ScopeCurated, obj, objectRef(src), equal)
}

// platformObject reads the object of gvk named name in the platform
// namespace. A missing one makes the organization not Ready; its creation
// reconciles the organization again.
func (r *OrganizationReconciler) platformObject(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	src := object(gvk)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Config.PlatformNamespace, Name: name}, src)
	if apierrors.IsNotFound(err) {
		return nil, &notReadyError{reason: reasonSourceMissing, message: fmt.Sprintf("the platform %s %s/%s to copy is missing", gvk.Kind, r.Config.PlatformNamespace, name)}
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvk.Kind, r.Config.PlatformNamespace, name, err)
	}
	return src, nil
}

// pruneCurated deletes the curated copies in ns that k does not keep, of the
// kinds whose sources k read in full.
func (r *OrganizationReconciler) pruneCurated(ctx context.Context, ns string, k *curated) error {
	var errs []error
	for _, gvk := range r.Config.Curated.copied() {
		if !k.complete[gvk] {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.Client.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{ObjectLabel: ScopeCurated}); err != nil {
			errs = append(errs, fmt.Errorf("list the %s copies in %s: %w", gvk.Kind, ns, err))
			continue
		}
		for i := range list.Items {
			cp := &list.Items[i]
			if k.keep[gvk][cp.GetName()] {
				continue
			}
			if err := r.Client.Delete(ctx, cp); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("delete %s %s/%s: %w", gvk.Kind, ns, cp.GetName(), err))
				continue
			}
			logf.FromContext(ctx).Info("organization object deleted", "namespace", ns, "kind", gvk.Kind, "name", cp.GetName())
		}
	}
	return errors.Join(errs...)
}

// rewriteHarness points a Harness spec at the organization's own pool and
// snapshot location, turns kagent's memory off and leaves the compaction
// summarizer on the agent's own model: no platform ModelConfig is in an
// organization namespace.
func rewriteHarness(spec map[string]any, pool, location string) error {
	if err := unstructured.SetNestedField(spec, pool, "substrate", "workerPoolRef", fieldName); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(spec, location, "substrate", "snapshotPolicy", "location"); err != nil {
		return err
	}
	unstructured.RemoveNestedField(spec, "kagent", "memory")
	unstructured.RemoveNestedField(spec, "kagent", "compaction", "summarizer", "modelConfigRef")
	return nil
}

// setModelConfig names the organization's default ModelConfig in an agent
// template spec; with no default it names none.
func setModelConfig(template map[string]any, name string) {
	if name == "" {
		delete(template, "modelConfig")
		return
	}
	template["modelConfig"] = map[string]any{fieldName: name}
}

// serversOf are the RemoteMCPServers an agent template spec's tools name.
func serversOf(template map[string]any) []string {
	tools, _, _ := unstructured.NestedSlice(template, "tools")
	var names []string
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		kind, _, _ := unstructured.NestedString(tool, "mcp", "server", "kind")
		name, _, _ := unstructured.NestedString(tool, "mcp", "server", fieldName)
		if kind == "RemoteMCPServer" && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func objectRef(src *unstructured.Unstructured) string {
	return src.GetNamespace() + "/" + src.GetName()
}

// equal reports whether have is want, field for field: a field have holds
// and want does not is drift. Both are read from the API server with its
// defaults, so the defaults are no drift.
func equal(have, want any) bool { return reflect.DeepEqual(have, want) }
