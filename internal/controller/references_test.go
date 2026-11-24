package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	fieldHarnessRef  = "harnessRef"
	fieldTemplateRef = "templateRef"
	fieldTools       = "tools"
	fieldTemplate    = "template"
	fieldSpec        = "spec"
	reasonTaken      = "taken"
	fieldKey         = "key"
	harness          = "kagent"
	agentName        = "assistant"
	writeServer      = "RemoteMCPServer muster"
)

func source(name string, spec map[string]any) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{fieldSpec: spec}}
	u.SetName(name)
	return u
}

func named(name string) map[string]any { return map[string]any{fieldName: name} }

// onHarness is the spec of an Agent on the Harness kagent with the template
// fields.
func onHarness(template map[string]any) map[string]any {
	return map[string]any{fieldHarnessRef: named(harness), fieldTemplate: template}
}

func mcpTool(server string) map[string]any {
	return map[string]any{"mcp": map[string]any{"server": map[string]any{fieldKind: kindServer, fieldName: server}}}
}

func subAgentTool(template string) map[string]any {
	return map[string]any{"subAgent": map[string]any{fieldName: "helper", "description": "Helps.", fieldTemplateRef: named(template)}}
}

// TestReferenceVerdicts is the reference rule of a curated Agent: met only
// when every object it refers to is a curated copy; unmet when one is not;
// unknown, and never unmet, while a read failed.
func TestReferenceVerdicts(t *testing.T) {
	listFailed := errors.New("list failed")
	for _, tc := range []struct {
		name      string
		agent     map[string]any
		templates []unstructured.Unstructured
		servers   []unstructured.Unstructured
		harnesses map[string]verdict
		listErr   map[string]error
		want      state
	}{
		{name: "an inline template on a Harness the organization has", agent: onHarness(map[string]any{}), want: met},
		{name: "an inline harness", agent: map[string]any{"harness": map[string]any{}, fieldTemplate: map[string]any{}}, want: met},
		{name: "a Harness outside spec.agentPlatform.harnesses", agent: map[string]any{fieldHarnessRef: named("claude"), fieldTemplate: map[string]any{}}, want: unmet},
		{name: "a Harness whose copy failed", agent: onHarness(map[string]any{}), harnesses: map[string]verdict{harness: {state: unknown, reason: "timeout"}}, want: unknown},
		{name: "a Harness whose name the organization holds", agent: onHarness(map[string]any{}), harnesses: map[string]verdict{harness: {state: unmet, reason: reasonTaken}}, want: unmet},
		{
			name:      "a curated templateRef",
			agent:     map[string]any{fieldHarnessRef: named(harness), fieldTemplateRef: named("triage")},
			templates: []unstructured.Unstructured{source("triage", map[string]any{})},
			want:      met,
		},
		{name: "a templateRef that is not curated", agent: map[string]any{fieldHarnessRef: named(harness), fieldTemplateRef: named("triage")}, want: unmet},
		{
			name:    "a templateRef while the templates cannot be listed",
			agent:   map[string]any{fieldHarnessRef: named(harness), fieldTemplateRef: named("triage")},
			listErr: map[string]error{kindTemplate: listFailed},
			want:    unknown,
		},
		{
			name:      "a curated templateRef whose server is not curated",
			agent:     map[string]any{fieldHarnessRef: named(harness), fieldTemplateRef: named("triage")},
			templates: []unstructured.Unstructured{source("triage", map[string]any{fieldTools: []any{mcpTool("muster")}})},
			want:      unmet,
		},
		{
			name:      "a curated sub-agent template",
			agent:     onHarness(map[string]any{fieldTools: []any{subAgentTool("helper")}}),
			templates: []unstructured.Unstructured{source("helper", map[string]any{})},
			want:      met,
		},
		{name: "a sub-agent template that is not curated", agent: onHarness(map[string]any{fieldTools: []any{subAgentTool("helper")}}), want: unmet},
		{
			name:      "sub-agent templates that refer to each other",
			agent:     onHarness(map[string]any{fieldTools: []any{subAgentTool("a")}}),
			templates: []unstructured.Unstructured{source("a", map[string]any{fieldTools: []any{subAgentTool("b")}}), source("b", map[string]any{fieldTools: []any{subAgentTool("a")}})},
			want:      unmet,
		},
		{
			name:    "a curated RemoteMCPServer",
			agent:   onHarness(map[string]any{fieldTools: []any{mcpTool("muster")}}),
			servers: []unstructured.Unstructured{source("muster", map[string]any{"url": "https://muster"})},
			want:    met,
		},
		{name: "a RemoteMCPServer that is not curated", agent: onHarness(map[string]any{fieldTools: []any{mcpTool("muster")}}), want: unmet},
		{
			name:  "a curated RemoteMCPServer with a Secret header",
			agent: onHarness(map[string]any{fieldTools: []any{mcpTool("muster")}}),
			servers: []unstructured.Unstructured{source("muster", map[string]any{"headersFrom": []any{map[string]any{
				fieldName: "Authorization", "valueFrom": map[string]any{"type": kindSecret, fieldName: "token", fieldKey: "token"},
			}}})},
			want: unmet,
		},
		{name: "a system prompt from a ConfigMap", agent: onHarness(map[string]any{"systemPromptFrom": map[string]any{fieldName: "prompt", fieldKey: "p"}}), want: unmet},
		{name: "an output schema from a ConfigMap", agent: onHarness(map[string]any{"outputSchemaFrom": map[string]any{fieldName: "schema", fieldKey: "s"}}), want: unmet},
		{name: "a prompt template data source", agent: onHarness(map[string]any{"promptTemplate": map[string]any{"dataSources": []any{named("data")}}}), want: unmet},
		{
			name: "a skill fetched with a Secret",
			agent: onHarness(map[string]any{"skills": []any{map[string]any{fieldName: "s", "source": map[string]any{
				"git": map[string]any{"url": "https://github.com/x/y", "credentialRef": map[string]any{fieldName: "git", fieldKey: "k"}},
			}}}}),
			want: unmet,
		},
		{name: "an EgressPolicy", agent: map[string]any{fieldHarnessRef: named(harness), fieldTemplate: map[string]any{}, "egressPolicyRefs": []any{named("open")}}, want: unmet},
		{
			name:    "an unmet reference beside an unknown one",
			agent:   onHarness(map[string]any{fieldTools: []any{subAgentTool("helper"), mcpTool("muster")}}),
			listErr: map[string]error{kindTemplate: listFailed},
			want:    unmet,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harnesses := tc.harnesses
			if harnesses == nil {
				harnesses = map[string]verdict{harness: {state: met}}
			}
			s := newCuratedSet(harnesses)
			s.add(kindAgent, []unstructured.Unstructured{source(agentName, tc.agent)}, nil)
			s.add(kindTemplate, tc.templates, tc.listErr[kindTemplate])
			s.add(kindServer, tc.servers, tc.listErr[kindServer])
			v := s.check(context.Background(), reference{kind: kindAgent, name: agentName})
			assert.Equal(t, tc.want, v.state, "reason: %s", v.reason)
			if tc.want != met {
				assert.NotEmpty(t, v.reason)
			}
		})
	}
}

// TestReferenceCopies is the order and the outcome of the writes: the
// objects an Agent refers to are written first, and the Agent only when
// they were; a name another writer holds leaves it out, a failed write
// leaves its state unknown, so no copy goes.
func TestReferenceCopies(t *testing.T) {
	agent := map[string]any{fieldHarnessRef: named(harness), fieldTemplateRef: named("triage")}
	triage := source("triage", map[string]any{fieldTools: []any{mcpTool("muster")}})
	muster := source("muster", map[string]any{})
	for _, tc := range []struct {
		name       string
		fail       map[string]error
		wantWrites []string
		wantAgent  state
		wantUnsure bool
	}{
		{name: "every write passes", wantWrites: []string{writeServer, "AgentTemplate triage", "Agent assistant"}, wantAgent: met},
		{
			name:       "the template's name is taken",
			fail:       map[string]error{"triage": &notReadyError{reason: reasonNameTaken, message: reasonTaken}},
			wantWrites: []string{writeServer, "AgentTemplate triage"},
			wantAgent:  unmet,
		},
		{
			name:       "the server's write fails",
			fail:       map[string]error{"muster": errors.New("timeout")},
			wantWrites: []string{writeServer},
			wantAgent:  unknown,
			wantUnsure: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCuratedSet(map[string]verdict{harness: {state: met}})
			s.add(kindAgent, []unstructured.Unstructured{source(agentName, agent)}, nil)
			s.add(kindTemplate, []unstructured.Unstructured{triage}, nil)
			s.add(kindServer, []unstructured.Unstructured{muster}, nil)
			var writes []string
			s.write = func(_ context.Context, kind string, src *unstructured.Unstructured) error {
				writes = append(writes, kind+" "+src.GetName())
				return tc.fail[src.GetName()]
			}
			got := s.copy(context.Background(), reference{kind: kindAgent, name: agentName})
			assert.Equal(t, tc.wantAgent, got.state, "reason: %s", got.reason)
			assert.Equal(t, tc.wantWrites, writes)
			assert.Equal(t, tc.wantUnsure, s.unsure())
		})
	}

	t.Run("an Agent left out is not written, nor what it refers to", func(t *testing.T) {
		s := newCuratedSet(map[string]verdict{})
		s.add(kindAgent, []unstructured.Unstructured{source(agentName, agent)}, nil)
		s.add(kindTemplate, []unstructured.Unstructured{triage}, nil)
		s.add(kindServer, []unstructured.Unstructured{muster}, nil)
		s.write = func(context.Context, string, *unstructured.Unstructured) error {
			t.Error("a write for an Agent whose Harness the organization does not have")
			return nil
		}
		assert.Equal(t, unmet, s.copy(context.Background(), reference{kind: kindAgent, name: agentName}).state)
		assert.False(t, s.copiesAgent(context.Background()))
		assert.False(t, s.unsure())
	})
}

// TestVerdictOf pins that only a cause of Ready=False itself leaves a copy
// out: one joined with a failure is unknown, so no copy goes.
func TestVerdictOf(t *testing.T) {
	taken := &notReadyError{reason: reasonNameTaken, message: reasonTaken}
	assert.Equal(t, met, verdictOf(nil, "x").state)
	assert.Equal(t, unmet, verdictOf(taken, "x").state)
	assert.Equal(t, unknown, verdictOf(errors.Join(taken, errors.New("timeout")), "x").state)
	assert.Equal(t, unknown, verdictOf(fmt.Errorf("wrapped: %w", taken), "x").state)
}

// TestCuratedCopyNameTakenSinceTheRead pins that a curated copy whose name an
// object of another writer took between the read and the create is NameTaken,
// not written and not taken for the copy.
func TestCuratedCopyNameTakenSinceTheRead(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "api.kagent.dev", Version: "v1alpha3", Kind: kindTemplate}
	theirs := &unstructured.Unstructured{Object: map[string]any{fieldSpec: map[string]any{"systemPrompt": "Theirs."}}}
	theirs.SetGroupVersionKind(gvk)
	theirs.SetNamespace("agents-acme-x1")
	theirs.SetName("triage")
	store := fake.NewClientBuilder().WithObjects(theirs).Build()
	// The read misses it, as one made just before the create would.
	missed := interceptor.NewClient(store, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: "agenttemplates"}, key.Name)
	}})
	r := &OrganizationReconciler{Client: missed, APIReader: store}

	src := &unstructured.Unstructured{Object: map[string]any{fieldSpec: map[string]any{"systemPrompt": "Curated."}}}
	src.SetGroupVersionKind(gvk)
	src.SetNamespace("kagent")
	src.SetName("triage")
	err := r.ensureCurated(t.Context(), "agents-acme-x1", src, nil)
	var nr *notReadyError
	if assert.ErrorAs(t, err, &nr) {
		assert.Equal(t, reasonNameTaken, nr.reason)
	}
	assert.Equal(t, unmet, verdictOf(err, "x").state)
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(theirs), got))
	assert.Equal(t, "Theirs.", got.Object[fieldSpec].(map[string]any)["systemPrompt"])
}
