package controller

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MigrationReadyAnnotation is what agent-manager's migrate sets on its report
// ConfigMap in an organization namespace (ChartSource.MigrationReport) to the
// agent chart major every release there is on the values of (agent-manager's
// migrate.OrganizationReadyAnnotation: the two constants are one contract).
// The operator moves the namespace's chart source across a major only while
// it names the new major.
const MigrationReadyAnnotation = "agent-platform.giantswarm.io/agent-chart-ready-major"

// DefaultMigrationReport is agent-manager migrate's report ConfigMap.
const DefaultMigrationReport = "agent-manager-migrate-report"

// DefaultMigrationPoll is how often an organization whose chart source is
// held at its major looks for migrate's mark again.
const DefaultMigrationPoll = time.Minute

const reasonChartSourceHeld = "ChartSourceHeld"

// majorMove is the major a move of an existing chart source from ref to the
// configured range crosses into: the configured range's highest major when it
// is above every major the source's range admits, else 0 (within a major, or
// back down, nothing waits). A source range that admits no release version,
// or has none (a tag or digest the operator never writes), is read as below
// any major, so the move waits too.
func (c ChartSource) majorMove(ref map[string]any) uint64 {
	to := rangeMajors(c.Semver)
	if len(to) == 0 {
		return 0
	}
	target := to[len(to)-1]
	from, _ := ref[fieldSemver].(string)
	if have := rangeMajors(from); len(have) > 0 && have[len(have)-1] >= target {
		return 0
	}
	return target
}

// chartSourceHeld is the cause of Ready=False while a chart source waits at
// its major; it reconciles the organization again after poll.
func chartSourceHeld(ns, name string, major uint64, why string, poll time.Duration) *notReadyError {
	return &notReadyError{reason: reasonChartSourceHeld, requeueAfter: poll, message: fmt.Sprintf(
		"the agent chart source %s/%s stays on its range until agent-manager's migrate marks the namespace ready for chart %d.x (%s): %s", ns, name, major, MigrationReadyAnnotation, why)}
}

// migrationReady reports whether the chart source name in ns may move to
// major: migrate's report there carries MigrationReadyAnnotation with that
// major, or no HelmRelease of the namespace renders from the source (nothing
// whose values could still be the old major's). why says what holds it.
func (r *OrganizationReconciler) migrationReady(ctx context.Context, ns, name string, major uint64) (ready bool, why string, err error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	report := r.Config.Chart.MigrationReport
	if report == "" {
		report = DefaultMigrationReport
	}
	cm := &corev1.ConfigMap{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: report}, cm); {
	case err == nil:
		if got := cm.Annotations[MigrationReadyAnnotation]; got == strconv.FormatUint(major, 10) {
			return true, "", nil
		} else if got != "" {
			why = fmt.Sprintf("its report %s marks it ready for %s.x only", report, got)
		} else {
			why = fmt.Sprintf("its report %s names releases not on %d.x values yet", report, major)
		}
	case apierrors.IsNotFound(err):
		why = fmt.Sprintf("no migrate run has written its report %s there yet", report)
	default:
		return false, "", fmt.Errorf("get the migrate report %s/%s: %w", ns, report, err)
	}
	gvk := r.Config.Erasure.HelmRelease
	if gvk.Empty() {
		return false, why, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := reader.List(ctx, list, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return true, "", nil // Flux's HelmRelease is not served: no release to wait for
		}
		return false, "", fmt.Errorf("list the HelmReleases of %s: %w", ns, err)
	}
	var using []string
	for i := range list.Items {
		ref, _, _ := unstructured.NestedStringMap(list.Items[i].Object, "spec", "chartRef")
		if ref["kind"] == "OCIRepository" && ref[fieldName] == name && (ref[fieldNamespace] == "" || ref[fieldNamespace] == ns) {
			using = append(using, list.Items[i].GetName())
		}
	}
	if len(using) == 0 {
		return true, "", nil
	}
	slices.Sort(using)
	return false, fmt.Sprintf("%s; HelmReleases on it: %v", why, using), nil
}

// versionLiteral is a version a range names (2, 2.6, 2.6.1, v3.0.0-rc.1).
var versionLiteral = regexp.MustCompile(`v?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?`)

// rangeMajors are the release majors a semver range admits, sorted; none when
// it does not parse. Read as agent-manager's migrate reads a target range
// (internal/migrate majorsOf) and the agent-platform chart's guard
// (agent-platform.agentChart.majors): every version the range names, with the
// next patch and minor, and per major 0-99 the first and a late version of
// every minor below 200 and a late minor, each counting only when the range
// admits it, so every alternative of a `||` range counts.
func rangeMajors(rng string) []uint64 {
	c, err := semver.NewConstraint(rng)
	if err != nil {
		return nil
	}
	var majors []uint64
	add := func(m uint64) {
		if !slices.Contains(majors, m) {
			majors = append(majors, m)
		}
	}
	for _, lit := range versionLiteral.FindAllStringSubmatch(rng, -1) {
		parts := make([]uint64, 3)
		ok := true
		for i, s := range lit[1:] {
			if s == "" {
				continue
			}
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				ok = false
				break
			}
			parts[i] = n
		}
		if !ok {
			continue
		}
		for _, v := range []*semver.Version{
			semver.New(parts[0], parts[1], parts[2], "", ""),
			semver.New(parts[0], parts[1], parts[2]+1, "", ""),
			semver.New(parts[0], parts[1]+1, 0, "", ""),
		} {
			if c.Check(v) {
				add(parts[0])
			}
		}
	}
	const late = 999999
	for m := range uint64(100) {
		if slices.Contains(majors, m) {
			continue
		}
		if c.Check(semver.New(m, late, late, "", "")) {
			add(m)
			continue
		}
		for n := range uint64(200) {
			if c.Check(semver.New(m, n, 0, "", "")) || c.Check(semver.New(m, n, late, "", "")) {
				add(m)
				break
			}
		}
	}
	slices.Sort(majors)
	return majors
}
