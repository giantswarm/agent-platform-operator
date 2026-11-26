package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	chartNS       = "agents-payments-abcde"
	chartRange    = "2.7.x"
	chartInterval = "30m"
	fieldProvider = "provider"
	fieldTimeout  = "timeout"
	providerValue = "generic"
)

// chart is the operator's configured agent chart source: what the platform
// release moved it to.
var chart = ChartSource{
	URL:          "oci://registry.example.com/charts/giantswarm/agent",
	Semver:       chartRange,
	SemverFilter: "",
	Interval:     chartInterval,
	APIVersion:   "source.toolkit.fluxcd.io/v1",
}

// chartCopy is an OCIRepository named after the chart in chartNS with labels
// and spec.
func chartCopy(labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(chart.APIVersion, "OCIRepository"))
	u.SetNamespace(chartNS)
	u.SetName(chart.Name())
	u.SetLabels(labels)
	return u
}

// chartSpec is the spec chart derives, with the fields of other.
func chartSpec(other map[string]any) map[string]any {
	spec := map[string]any{fieldInterval: chartInterval, fieldURL: chart.URL, fieldRef: map[string]any{fieldSemver: chartRange}}
	for k, v := range other {
		spec[k] = v
	}
	return spec
}

// oldSpec is the spec an older operator release wrote: another registry,
// range and filter, plus fields the operator does not derive (another
// writer's, or the API server's defaults).
func oldSpec() map[string]any {
	return map[string]any{
		fieldInterval: "10m",
		fieldURL:      "oci://gsoci.azurecr.io/charts/giantswarm/agent",
		fieldRef:      map[string]any{fieldSemver: "2.6.x", "semverFilter": "-dev", "tag": "2.6.0"},
		"insecure":    true,
		fieldProvider: providerValue,
	}
}

// The chart source of an organization follows the operator's configuration
// on every reconcile: the fields it derives (url, interval, ref) are written,
// every other field and label stays, an unchanged copy is not written again,
// and an object of that name the operator did not write is left alone.
func TestEnsureChartSource(t *testing.T) {
	own := ownLabels(ScopeNamespace)
	defaulted := map[string]any{fieldProvider: providerValue, fieldTimeout: "60s"}
	withNote := map[string]string{ObjectLabel: ScopeNamespace, managedByLabel: managedByValue, "example.com/note": "kept"}
	tests := []struct {
		name     string
		existing *unstructured.Unstructured
		// want is the spec after the reconcile; wantLabels its labels.
		want       map[string]any
		wantLabels map[string]string
		// writes is how many creates and updates the reconcile makes.
		writes int
	}{
		{
			name:       "a missing source is created",
			want:       chartSpec(nil),
			wantLabels: own,
			writes:     1,
		},
		{
			name:       "an older source moves to the configured url, range and interval, keeping the fields and labels it does not derive",
			existing:   chartCopy(withNote, oldSpec()),
			want:       chartSpec(map[string]any{"insecure": true, fieldProvider: providerValue}),
			wantLabels: withNote,
			writes:     1,
		},
		{
			name:     "a source at the configured spec, with defaulted fields, is not written",
			existing: chartCopy(own, chartSpec(defaulted)),
			want:     chartSpec(defaulted),
			// Unchanged: no write, no hot loop.
			wantLabels: own,
			writes:     0,
		},
		{
			name:       "a source of that name the operator did not write is left alone",
			existing:   chartCopy(map[string]string{"app.kubernetes.io/managed-by": "someone-else"}, oldSpec()),
			want:       oldSpec(),
			wantLabels: map[string]string{"app.kubernetes.io/managed-by": "someone-else"},
			writes:     0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(runtime.NewScheme())
			if tc.existing != nil {
				b = b.WithObjects(tc.existing)
			}
			writes := 0
			c := b.WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				},
			}).Build()
			r := &OrganizationReconciler{Client: c, Config: Config{Chart: chart}}

			require.NoError(t, r.ensureChartSource(t.Context(), chartNS))
			assert.Equal(t, tc.writes, writes)

			got := chartCopy(nil, nil)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
			assert.Equal(t, tc.want, got.Object["spec"])
			assert.Equal(t, tc.wantLabels, got.GetLabels())

			// A second reconcile of what the first left writes nothing.
			writes = 0
			require.NoError(t, r.ensureChartSource(t.Context(), chartNS))
			assert.Zero(t, writes, "the second reconcile wrote the source again")
		})
	}
}

// A configured semverFilter is written into ref, and dropped again once the
// configuration has none.
func TestEnsureChartSourceSemverFilter(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(chartCopy(ownLabels(ScopeNamespace), oldSpec())).Build()
	filtered := chart
	filtered.SemverFilter = ".*-rc.*"
	r := &OrganizationReconciler{Client: c, Config: Config{Chart: filtered}}
	require.NoError(t, r.ensureChartSource(t.Context(), chartNS))

	got := chartCopy(nil, nil)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
	ref, _, _ := unstructured.NestedMap(got.Object, "spec", fieldRef)
	assert.Equal(t, map[string]any{fieldSemver: chartRange, "semverFilter": ".*-rc.*"}, ref)

	r.Config.Chart = chart
	require.NoError(t, r.ensureChartSource(t.Context(), chartNS))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
	ref, _, _ = unstructured.NestedMap(got.Object, "spec", fieldRef)
	assert.Equal(t, map[string]any{fieldSemver: chartRange}, ref)
}
