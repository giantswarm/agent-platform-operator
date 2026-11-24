package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// This file holds the reference rule of the curated copies: a curated Agent
// or AgentTemplate is copied into an organization namespace only when every
// object it refers to is there as a curated copy too. A copy with a reference
// that does not resolve to a curated copy would bind to an object of that name
// that the organization writes itself (its Flux ServiceAccount creates Agents,
// AgentTemplates and RemoteMCPServers in its namespace), so a curated,
// read-only Agent would run the organization's template or tools.

// The kinds a curated object refers to.
const (
	kindAgent        = "Agent"
	kindHarness      = "Harness"
	kindTemplate     = "AgentTemplate"
	kindServer       = "RemoteMCPServer"
	kindConfigMap    = "ConfigMap"
	kindSecret       = "Secret"
	kindEgressPolicy = "EgressPolicy"
)

// reference is an object a curated object refers to, by kind and name, in its
// own namespace.
type reference struct{ kind, name string }

func (r reference) String() string { return r.kind + " " + r.name }

// state is what a reconcile knows of whether a curated object can be copied.
type state int

const (
	// met: the object and everything it refers to are, or can be, curated
	// copies in the organization namespace.
	met state = iota
	// unmet: a reference does not resolve to a curated copy: the object is
	// not copied, and its copy goes.
	unmet
	// unknown: a read or a write failed. The object is not written, and no
	// copy of its kinds goes on this reconcile.
	unknown
)

// verdict is a state with why, for unmet and unknown.
type verdict struct {
	state  state
	reason string
}

// verdictOf is the verdict of a copy that err left: a cause of Ready=False
// (the source is missing, the name is taken by another writer's object) is
// unmet; any other failure is unknown. As in split, only a *notReadyError
// itself is a cause: one wrapped or joined with others may hide a failure,
// and a failure must never delete a copy.
func verdictOf(err error, what string) verdict {
	if err == nil {
		return verdict{state: met}
	}
	if nr, ok := err.(*notReadyError); ok { //nolint:errorlint // a wrapped cause is a failure, see split
		return verdict{state: unmet, reason: fmt.Sprintf("%s: %s", what, nr.message)}
	}
	return verdict{state: unknown, reason: fmt.Sprintf("%s: %v", what, err)}
}

// curatedSet is one reconcile's view of the curated Agents, AgentTemplates
// and RemoteMCPServers of the platform namespace, and of the Harness copies
// of the organization, through which it decides which objects are copied.
type curatedSet struct {
	// agents, templates and servers are the sources labelled
	// CuratedLabel=true, by name. A kind whose list failed is in listErr.
	agents    []unstructured.Unstructured
	templates map[string]*unstructured.Unstructured
	servers   map[string]*unstructured.Unstructured
	listErr   map[string]error
	// harnesses are the verdicts of the Harness copies spec.agentPlatform.harnesses names.
	harnesses map[string]verdict
	// checked are the verdicts read from the sources alone; copied those
	// of the copies written (copy).
	checked, copied map[reference]verdict
	visiting        map[reference]bool
	// write keeps the copy of a source whose references are met; nil writes
	// nothing.
	write func(ctx context.Context, kind string, src *unstructured.Unstructured) error
	errs  []error
}

func newCuratedSet(harnesses map[string]verdict) *curatedSet {
	return &curatedSet{
		templates: map[string]*unstructured.Unstructured{},
		servers:   map[string]*unstructured.Unstructured{},
		listErr:   map[string]error{},
		harnesses: harnesses,
		checked:   map[reference]verdict{},
		copied:    map[reference]verdict{},
		visiting:  map[reference]bool{},
	}
}

// add records the curated sources of kind, or the error of their list.
func (s *curatedSet) add(kind string, items []unstructured.Unstructured, err error) {
	if err != nil {
		s.listErr[kind] = err
		return
	}
	for i := range items {
		switch kind {
		case kindTemplate:
			s.templates[items[i].GetName()] = &items[i]
		case kindServer:
			s.servers[items[i].GetName()] = &items[i]
		default:
			s.agents = append(s.agents, items[i])
		}
	}
}

// roots are the objects copied for their own sake: the curated Agents, then
// the curated AgentTemplates. A RemoteMCPServer is copied only for an object
// that refers to it.
func (s *curatedSet) roots() []reference {
	var out []reference
	for i := range s.agents {
		out = append(out, reference{kind: kindAgent, name: s.agents[i].GetName()})
	}
	for _, name := range slices.Sorted(maps.Keys(s.templates)) {
		out = append(out, reference{kind: kindTemplate, name: name})
	}
	return out
}

// check is the verdict of ref from the sources alone: nothing is written.
func (s *curatedSet) check(ctx context.Context, ref reference) verdict {
	return s.resolve(ctx, ref, false)
}

// copy writes the copy of ref, the objects it refers to first, when check
// found it met, and returns the verdict of the copies written.
func (s *curatedSet) copy(ctx context.Context, ref reference) verdict {
	if v := s.check(ctx, ref); v.state != met {
		return v
	}
	return s.resolve(ctx, ref, true)
}

// unsure reports whether a verdict of this reconcile is unknown, or a list
// failed: then no copy of an Agent, AgentTemplate or RemoteMCPServer goes.
func (s *curatedSet) unsure() bool {
	if len(s.listErr) > 0 {
		return true
	}
	for _, m := range []map[reference]verdict{s.checked, s.copied} {
		for _, v := range m {
			if v.state == unknown {
				return true
			}
		}
	}
	return false
}

func (s *curatedSet) resolve(ctx context.Context, ref reference, write bool) verdict {
	memo := s.checked
	if write {
		memo = s.copied
	}
	if v, ok := memo[ref]; ok {
		return v
	}
	if s.visiting[ref] {
		// A template that reaches itself through its sub-agents.
		return verdict{state: unmet, reason: fmt.Sprintf("the %s refers to itself", ref)}
	}
	s.visiting[ref] = true
	v := s.resolveOnce(ctx, ref, write)
	delete(s.visiting, ref)
	memo[ref] = v
	return v
}

func (s *curatedSet) resolveOnce(ctx context.Context, ref reference, write bool) verdict {
	var src *unstructured.Unstructured
	switch ref.kind {
	case kindHarness:
		if v, ok := s.harnesses[ref.name]; ok {
			return v
		}
		return verdict{state: unmet, reason: fmt.Sprintf("the Harness %s is not in the organization's spec.agentPlatform.harnesses", ref.name)}
	case kindAgent:
		for i := range s.agents {
			if s.agents[i].GetName() == ref.name {
				src = &s.agents[i]
			}
		}
	case kindTemplate:
		src = s.templates[ref.name]
	case kindServer:
		src = s.servers[ref.name]
	default:
		// ConfigMaps, Secrets and EgressPolicies are never copied: a
		// reference to one would bind to the organization's own.
		return verdict{state: unmet, reason: fmt.Sprintf("the %s is not copied into organization namespaces", ref)}
	}
	if err := s.listErr[ref.kind]; err != nil {
		return verdict{state: unknown, reason: err.Error()}
	}
	if src == nil {
		return verdict{state: unmet, reason: fmt.Sprintf("the %s is not curated: it is missing or not labelled curated", ref)}
	}
	// An unmet reference decides, whatever else is unknown.
	var unsure *verdict
	for _, dep := range referencesOf(ref.kind, src) {
		v := s.resolve(ctx, dep, write)
		switch {
		case v.state == unmet:
			return verdict{state: unmet, reason: v.reason}
		case v.state == unknown && unsure == nil:
			unsure = &v
		}
	}
	if unsure != nil {
		return *unsure
	}
	if !write || s.write == nil {
		return verdict{state: met}
	}
	err := s.write(ctx, ref.kind, src)
	s.errs = append(s.errs, err)
	return verdictOf(err, "the copy of the "+ref.String())
}

// referencesOf are the objects a source of kind refers to in its namespace.
// A Harness spec inline in an Agent refers to nothing the copy keeps: its
// pool and snapshot location are rewritten and its model references removed.
func referencesOf(kind string, src *unstructured.Unstructured) []reference {
	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	switch kind {
	case kindAgent:
		var refs []reference
		if name, _, _ := unstructured.NestedString(spec, "harnessRef", fieldName); name != "" {
			refs = append(refs, reference{kindHarness, name})
		}
		if name, _, _ := unstructured.NestedString(spec, "templateRef", fieldName); name != "" {
			refs = append(refs, reference{kindTemplate, name})
		}
		if tmpl, ok, _ := unstructured.NestedMap(spec, "template"); ok {
			refs = append(refs, templateReferences(tmpl)...)
		}
		for _, name := range namesIn(spec, "egressPolicyRefs") {
			refs = append(refs, reference{kindEgressPolicy, name})
		}
		return refs
	case kindTemplate:
		return templateReferences(spec)
	case kindServer:
		var refs []reference
		headers, _, _ := unstructured.NestedSlice(spec, "headersFrom")
		for _, h := range headers {
			header, _ := h.(map[string]any)
			kind, _, _ := unstructured.NestedString(header, "valueFrom", "type")
			name, _, _ := unstructured.NestedString(header, "valueFrom", fieldName)
			if name != "" {
				refs = append(refs, reference{kind, name})
			}
		}
		return refs
	}
	return nil
}

// templateReferences are the objects an agent template spec refers to: the
// RemoteMCPServers and sub-agent AgentTemplates its tools name, the
// ConfigMaps of its system prompt, output schema and prompt template, and the
// Secrets its skills and plugins are fetched with.
func templateReferences(tmpl map[string]any) []reference {
	var refs []reference
	tools, _, _ := unstructured.NestedSlice(tmpl, "tools")
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if name, _, _ := unstructured.NestedString(tool, "mcp", "server", fieldName); name != "" {
			kind, _, _ := unstructured.NestedString(tool, "mcp", "server", fieldKind)
			refs = append(refs, reference{kind, name})
		}
		if name, _, _ := unstructured.NestedString(tool, "subAgent", "templateRef", fieldName); name != "" {
			refs = append(refs, reference{kindTemplate, name})
		}
	}
	for _, field := range []string{"systemPromptFrom", "outputSchemaFrom"} {
		if name, _, _ := unstructured.NestedString(tmpl, field, fieldName); name != "" {
			refs = append(refs, reference{kindConfigMap, name})
		}
	}
	if prompt, ok, _ := unstructured.NestedMap(tmpl, "promptTemplate"); ok {
		for _, name := range namesIn(prompt, "dataSources") {
			refs = append(refs, reference{kindConfigMap, name})
		}
	}
	for _, field := range []string{"skills", "plugins"} {
		items, _, _ := unstructured.NestedSlice(tmpl, field)
		for _, it := range items {
			item, _ := it.(map[string]any)
			source, _, _ := unstructured.NestedMap(item, "source")
			for _, artifact := range source {
				a, _ := artifact.(map[string]any)
				if name, _, _ := unstructured.NestedString(a, "credentialRef", fieldName); name != "" {
					refs = append(refs, reference{kindSecret, name})
				}
			}
		}
	}
	return refs
}

// namesIn are the names of the list of references at field of obj.
func namesIn(obj map[string]any, field string) []string {
	items, _, _ := unstructured.NestedSlice(obj, field)
	var names []string
	for _, it := range items {
		item, _ := it.(map[string]any)
		if name, _ := item[fieldName].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// copyCuratedSet copies the curated Agents and AgentTemplates whose
// references are met, the objects they refer to first, and records in k the
// copies to keep. A copy whose source is no longer met for the organization
// is not kept, so pruneCurated deletes it; when a verdict is unknown, k reads
// no kind in full and no copy goes.
func (r *OrganizationReconciler) copyCuratedSet(ctx context.Context, s *curatedSet, k *curated) error {
	c := r.Config.Curated
	gvk := map[string]schema.GroupVersionKind{kindAgent: c.agent(), kindTemplate: c.template(), kindServer: c.server()}
	log := logf.FromContext(ctx)
	for _, root := range s.roots() {
		if v := s.copy(ctx, root); v.state != met {
			log.V(1).Info("curated object left out", "kind", root.kind, "name", root.name, "reason", v.reason)
		}
	}
	for ref, v := range s.copied {
		if g, ok := gvk[ref.kind]; ok && v.state == met {
			k.kept(g, ref.name)
		}
	}
	if !s.unsure() {
		for _, g := range gvk {
			k.complete[g] = true
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(s.listErr)) {
		s.errs = append(s.errs, s.listErr[kind])
	}
	return errors.Join(s.errs...)
}

// copiesAgent reports whether a curated Agent's copy is, or may be, kept in
// the organization namespace: the worker replica rule counts it before its
// copy exists.
func (s *curatedSet) copiesAgent(ctx context.Context) bool {
	if s.listErr[kindAgent] != nil {
		return true
	}
	for i := range s.agents {
		if s.check(ctx, reference{kind: kindAgent, name: s.agents[i].GetName()}).state != unmet {
			return true
		}
	}
	return false
}
