package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	v2Line        = "2.x"
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
			name:       "an interval that alone differs is written",
			existing:   chartCopy(own, chartSpec(map[string]any{fieldInterval: "5m"})),
			want:       chartSpec(nil),
			wantLabels: own,
			writes:     1,
		},
		{
			name:       "a missing own label alone is written, the other labels kept",
			existing:   chartCopy(map[string]string{ObjectLabel: ScopeNamespace, "example.com/note": "kept"}, chartSpec(nil)),
			want:       chartSpec(nil),
			wantLabels: withNote,
			writes:     1,
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

var testHelmReleaseGVK = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}

// chartRelease is a HelmRelease in chartNS whose chart is the OCIRepository
// source.
func chartRelease(name, source string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", fieldName: source},
	}}}
	u.SetGroupVersionKind(testHelmReleaseGVK)
	u.SetNamespace(chartNS)
	u.SetName(name)
	return u
}

// deployed is a release that last deployed chart version.
func deployed(hr *unstructured.Unstructured, version string) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"chartVersion": version, "status": "deployed"}}, "status", "history")
	return hr
}

// migrateReport is agent-manager migrate's report in chartNS, marked ready
// for major, naming releases, when major is not empty.
func migrateReport(major string, releases ...string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: chartNS, Name: DefaultMigrationReport}}
	if major != "" {
		cm.Annotations = map[string]string{MigrationReadyAnnotation: major, MigrationReadyReleasesAnnotation: strings.Join(releases, ",")}
	}
	return cm
}

// gateScheme knows ConfigMaps and the HelmRelease as unstructured.
func gateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	scheme.AddKnownTypeWithName(testHelmReleaseGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(testHelmReleaseGVK.GroupVersion().WithKind("HelmReleaseList"), &unstructured.UnstructuredList{})
	return scheme
}

// A move of an organization's chart source to a higher major waits for
// agent-manager migrate's mark on its report in the namespace, unless no
// HelmRelease there uses the source (D-GO-03). Held, the source keeps its
// ref while url and interval still follow, and the organization is not Ready
// (ChartSourceHeld) with a requeue after MigrationPoll.
func TestEnsureChartSourceWaitsForMigrateAcrossAMajor(t *testing.T) {
	const v3 = ">=3.0.0 <4.0.0"
	v2Ref := map[string]any{fieldSemver: v2Line}
	tests := []struct {
		name     string
		semver   string
		from     map[string]any
		objects  []client.Object
		moved    bool
		mentions string
	}{
		{name: "a release on the source and no report: held", semver: v3, from: v2Ref,
			objects: []client.Object{chartRelease("triage", chart.Name())}, mentions: "no migrate run has written its report"},
		{name: "a report without the mark: held", semver: v3, from: v2Ref,
			objects: []client.Object{chartRelease("triage", chart.Name()), migrateReport("")}, mentions: "names releases not on 3.x values yet"},
		{name: "a report marked for the old major: held", semver: v3, from: v2Ref,
			objects: []client.Object{chartRelease("triage", chart.Name()), migrateReport("2")}, mentions: "ready for 2.x only"},
		{name: "a report marked for the new major: moved", semver: v3, from: v2Ref,
			objects: []client.Object{chartRelease("triage", chart.Name()), migrateReport("3", "triage")}, moved: true},
		{name: "a release created after migrate's run: held", semver: v3, from: v2Ref,
			objects:  []client.Object{chartRelease("triage", chart.Name()), chartRelease("late", chart.Name()), migrateReport("3", "triage")},
			mentions: "HelmReleases [late] on the source were created after that run"},
		{name: "a release of another kind with the source's name does not count", semver: v3, from: v2Ref,
			objects: []client.Object{func() client.Object {
				hr := chartRelease("chart", chart.Name())
				_ = unstructured.SetNestedField(hr.Object, "HelmChart", "spec", "chartRef", "kind")
				return hr
			}()}, moved: true},
		{name: "no release on the source: moved without a report", semver: v3, from: v2Ref,
			objects: []client.Object{chartRelease("dashboards", "dashboards")}, moved: true},
		{name: "a move within a major never waits", semver: chartRange, from: map[string]any{fieldSemver: "2.6.x"},
			objects: []client.Object{chartRelease("triage", chart.Name())}, moved: true},
		{name: "a move back down never waits", semver: v2Line, from: map[string]any{fieldSemver: v3},
			objects: []client.Object{chartRelease("triage", chart.Name())}, moved: true},
		{name: "an alternative of the old range on the new major: no move across", semver: v3, from: map[string]any{fieldSemver: "2.6.0 || 3.0.1"},
			objects: []client.Object{chartRelease("triage", chart.Name())}, moved: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := gateScheme(t)
			spec := chartSpec(map[string]any{fieldURL: "oci://old.example.com/charts/giantswarm/agent", fieldRef: tc.from})
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(tc.objects, chartCopy(ownLabels(ScopeNamespace), spec))...).Build()
			cfg := chart
			cfg.Semver = tc.semver
			cfg.MigrationPoll = 7 * time.Second
			r := &OrganizationReconciler{Client: c, Config: Config{Chart: cfg, Erasure: ErasureConfig{HelmRelease: testHelmReleaseGVK}}}

			err := r.ensureChartSource(t.Context(), chartNS)
			got := chartCopy(nil, nil)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
			url, _, _ := unstructured.NestedString(got.Object, "spec", fieldURL)
			assert.Equal(t, chart.URL, url, "the url follows either way")
			ref, _, _ := unstructured.NestedMap(got.Object, "spec", fieldRef)
			if tc.moved {
				require.NoError(t, err)
				assert.Equal(t, map[string]any{fieldSemver: tc.semver}, ref)
				return
			}
			assert.Equal(t, tc.from, ref, "the ref stays on the old major")
			var nr *notReadyError
			require.ErrorAs(t, err, &nr)
			assert.Equal(t, reasonChartSourceHeld, nr.reason)
			assert.Contains(t, nr.message, tc.mentions)
			assert.Contains(t, nr.message, MigrationReadyAnnotation)
			assert.Equal(t, 7*time.Second, requeueAfter(errors.Join(errors.New("another failure"), err)))
			ready, retry := readyCondition(err, 1)
			assert.NoError(t, retry, "a held source is no failure to retry")
			assert.Equal(t, reasonChartSourceHeld, ready.Reason)

			// A second reconcile writes nothing more.
			rv := got.GetResourceVersion()
			require.Error(t, r.ensureChartSource(t.Context(), chartNS))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
			assert.Equal(t, rv, got.GetResourceVersion())
		})
	}
}

// The majors a range admits, read as agent-manager's migrate and the
// agent-platform chart read them (D-GO-08).
func TestRangeMajors(t *testing.T) {
	for rng, want := range map[string][]uint64{
		"2.x":                     {2},
		">=3.0.0 <4.0.0":          {3},
		"3.0.1 || 2.6.0":          {2, 3},
		">=2.6.1 <2.6.9":          {2},
		"= 2.6.1":                 {2},
		"~2.250":                  {2},
		"3.0.0-rc.6":              nil,
		"not a range":             nil,
		">=2.6.0 <=3.0.0-rc.1":    {2},
		"2.6.0 || >=3.0.1 <3.0.5": {2, 3},
	} {
		assert.Equal(t, want, rangeMajors(rng), rng)
	}
}

// requeueAfter is the shortest a cause of Ready=False asks for; a failure or
// a cause without one asks for none.
func TestRequeueAfter(t *testing.T) {
	assert.Zero(t, requeueAfter(nil))
	assert.Zero(t, requeueAfter(errors.Join(errors.New("failed"), &notReadyError{reason: reasonNameTaken})))
	assert.Equal(t, time.Second, requeueAfter(errors.Join(&notReadyError{requeueAfter: time.Minute}, &notReadyError{requeueAfter: time.Second})))
}

// A configured range of no release major or of several creates and moves
// nothing and says so (ChartRangeInvalid), never waiting silently for a mark
// migrate cannot write (D-GO-29).
func TestEnsureChartSourceRefusesARangeOfNoneOrSeveralMajors(t *testing.T) {
	for rng, mentions := range map[string]string{
		"3.0.0-rc.1":     "admits no release version",
		"not a range":    "admits no release version",
		">=2.6.0":        "admits majors [2 3 4",
		"2.6.0 || 3.0.1": "admits majors [2 3]",
	} {
		t.Run(rng, func(t *testing.T) {
			from := map[string]any{fieldSemver: v2Line}
			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(chartCopy(ownLabels(ScopeNamespace), chartSpec(map[string]any{fieldRef: from}))).Build()
			cfg := chart
			cfg.Semver = rng
			r := &OrganizationReconciler{Client: c, Config: Config{Chart: cfg, Erasure: ErasureConfig{HelmRelease: testHelmReleaseGVK}}}
			err := r.ensureChartSource(t.Context(), chartNS)
			var nr *notReadyError
			require.ErrorAs(t, err, &nr)
			assert.Equal(t, reasonChartRangeInvalid, nr.reason)
			assert.Contains(t, nr.message, mentions)
			got := chartCopy(nil, nil)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), got))
			ref, _, _ := unstructured.NestedMap(got.Object, "spec", fieldRef)
			assert.Equal(t, from, ref, "the ref stays")

			// Missing, it is not created on such a range.
			c = fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
			r.Client = c
			require.ErrorAs(t, r.ensureChartSource(t.Context(), chartNS), &nr)
			assert.Equal(t, reasonChartRangeInvalid, nr.reason)
			assert.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(got), chartCopy(nil, nil)))
		})
	}
}

// A recreated source (missing while releases use it) waits for migrate's
// mark when a release there last deployed a lower major (D-GO-29).
func TestEnsureChartSourceRecreatedWaitsForMigrate(t *testing.T) {
	const v3 = ">=3.0.0 <4.0.0"
	for name, tc := range map[string]struct {
		semver  string
		objects []client.Object
		created bool
	}{
		"a release on 2.x, no mark: not created": {v3, []client.Object{deployed(chartRelease("triage", chart.Name()), "2.6.1")}, false},
		"a release on 2.x, marked: created":      {v3, []client.Object{deployed(chartRelease("triage", chart.Name()), "2.6.1"), migrateReport("3", "triage")}, true},
		"a release already on 3.x: created":      {v3, []client.Object{deployed(chartRelease("triage", chart.Name()), "3.0.0")}, true},
		"a release never deployed: created":      {v3, []client.Object{chartRelease("triage", chart.Name())}, true},
		"within a major nothing waits":           {chartRange, []client.Object{deployed(chartRelease("triage", chart.Name()), "2.6.1")}, true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(tc.objects...).Build()
			cfg := chart
			cfg.Semver = tc.semver
			r := &OrganizationReconciler{Client: c, Config: Config{Chart: cfg, Erasure: ErasureConfig{HelmRelease: testHelmReleaseGVK}}}
			err := r.ensureChartSource(t.Context(), chartNS)
			getErr := c.Get(t.Context(), client.ObjectKeyFromObject(chartCopy(nil, nil)), chartCopy(nil, nil))
			if tc.created {
				require.NoError(t, err)
				require.NoError(t, getErr)
				return
			}
			var nr *notReadyError
			require.ErrorAs(t, err, &nr)
			assert.Equal(t, reasonChartSourceHeld, nr.reason)
			assert.Contains(t, nr.message, "HelmReleases on it: [triage]")
			assert.Error(t, getErr, "not created")
		})
	}
}
