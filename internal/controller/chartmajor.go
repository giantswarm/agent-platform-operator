package controller

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// MigrationReadyReleasesAnnotation is set beside MigrationReadyAnnotation to
// the comma-separated names of the HelmReleases the run found on target
// values (agent-manager's migrate.OrganizationReadyReleasesAnnotation). A
// release on the source that it does not name was created after the run read
// the namespace: the mark does not cover it, and migrate must run again.
const MigrationReadyReleasesAnnotation = "agent-platform.giantswarm.io/agent-chart-ready-releases"

// DefaultMigrationPoll is how often an organization whose chart source is
// held at its major looks for migrate's mark again.
const DefaultMigrationPoll = time.Minute

const (
	reasonChartSourceHeld   = "ChartSourceHeld"
	reasonChartRangeInvalid = "ChartRangeInvalid"
)

// targetMajor is the one release major the configured range admits; a range
// of none (only pre-releases, or not a range) or of several (an open range
// such as >=2.6.0) is refused: the operator could not tell which major's
// values the organization's releases need, and migrate marks one major only.
func (c ChartSource) targetMajor(ns string) (uint64, *notReadyError) {
	switch to := rangeMajors(c.Semver); len(to) {
	case 1:
		return to[0], nil
	case 0:
		return 0, &notReadyError{reason: reasonChartRangeInvalid, message: fmt.Sprintf(
			"the agent chart range %q admits no release version, so the chart source in %s is neither created nor moved: configure a range of one major (agentChart.semver, e.g. \">=3.0.0 <4.0.0\")", c.Semver, ns)}
	default:
		return 0, &notReadyError{reason: reasonChartRangeInvalid, message: fmt.Sprintf(
			"the agent chart range %q admits majors %v, so the chart source in %s is neither created nor moved: agent-manager's migrate readies an organization for one major; bound the range below the next major (agentChart.semver, e.g. \">=3.0.0 <4.0.0\")", c.Semver, to, ns)}
	}
}

// crossesMajor reports whether a source on ref moving to target crosses into
// a higher major: every major ref's range admits is below target. A range
// that admits no release version, or none (a tag or digest the operator never
// writes), is read as below any major, so the move waits too.
func crossesMajor(ref map[string]any, target uint64) bool {
	from, _ := ref[fieldSemver].(string)
	have := rangeMajors(from)
	return len(have) == 0 || have[len(have)-1] < target
}

// chartSourceHeld is the cause of Ready=False while a chart source waits at
// its major (or, missing, waits to be created); it reconciles the
// organization again after poll.
func chartSourceHeld(ns, name string, major uint64, why string, poll time.Duration) *notReadyError {
	return &notReadyError{reason: reasonChartSourceHeld, requeueAfter: poll, message: fmt.Sprintf(
		"the agent chart source %s/%s is not moved to chart %d.x until agent-manager's migrate marks the namespace ready for it (%s): %s; run migrate again once its report's releases are fixed (kubectl create job --from=cronjob/<release>-agent-manager-migrate)", ns, name, major, MigrationReadyAnnotation, why)}
}

// releasesOn are the HelmReleases of ns whose chart is the OCIRepository
// name; served is false when the cluster serves no HelmRelease (or none is
// configured).
func (r *OrganizationReconciler) releasesOn(ctx context.Context, reader client.Reader, ns, name string) (rels []unstructured.Unstructured, served bool, err error) {
	gvk := r.Config.Erasure.HelmRelease
	if gvk.Empty() {
		return nil, false, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := reader.List(ctx, list, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list the HelmReleases of %s: %w", ns, err)
	}
	for i := range list.Items {
		ref, _, _ := unstructured.NestedStringMap(list.Items[i].Object, "spec", "chartRef")
		if ref["kind"] == "OCIRepository" && ref[fieldName] == name && (ref[fieldNamespace] == "" || ref[fieldNamespace] == ns) {
			rels = append(rels, list.Items[i])
		}
	}
	return rels, true, nil
}

// deployedMajor is the chart major a HelmRelease last deployed
// (status.history[0].chartVersion); ok is false when it never deployed one.
func deployedMajor(hr unstructured.Unstructured) (uint64, bool) {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return 0, false
	}
	entry, _ := history[0].(map[string]any)
	version, _ := entry["chartVersion"].(string)
	v, err := semver.NewVersion(version)
	if err != nil {
		return 0, false
	}
	return v.Major(), true
}

// migrationReady reports whether the chart source name in ns may move to (or,
// missing, be created on) major: no HelmRelease of the namespace renders from
// the source, or migrate's report there carries MigrationReadyAnnotation with
// that major and names every such release in MigrationReadyReleasesAnnotation.
// For a missing source (recreated) only releases whose deployed chart is of a
// lower major count: within a major nothing waits. why says what holds it.
func (r *OrganizationReconciler) migrationReady(ctx context.Context, ns, name string, major uint64, creating bool) (ready bool, why string, err error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	rels, served, err := r.releasesOn(ctx, reader, ns, name)
	if err != nil {
		return false, "", err
	}
	var using []string
	for _, hr := range rels {
		if m, ok := deployedMajor(hr); creating && (!ok || m >= major) {
			continue
		}
		using = append(using, hr.GetName())
	}
	slices.Sort(using)
	if served && len(using) == 0 {
		return true, "", nil
	}
	if creating && !served {
		return true, "", nil // nothing to tell a recreated source's releases by
	}
	report := r.Config.Chart.MigrationReport
	if report == "" {
		report = DefaultMigrationReport
	}
	cm := &corev1.ConfigMap{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: report}, cm); {
	case err == nil:
		got := cm.Annotations[MigrationReadyAnnotation]
		switch {
		case got == strconv.FormatUint(major, 10):
			marked := strings.Split(cm.Annotations[MigrationReadyReleasesAnnotation], ",")
			var after []string
			for _, name := range using {
				if !slices.Contains(marked, name) {
					after = append(after, name)
				}
			}
			if len(after) == 0 {
				return true, "", nil
			}
			why = fmt.Sprintf("its report %s marks it ready, but HelmReleases %v on the source were created after that run", report, after)
		case got != "":
			why = fmt.Sprintf("its report %s marks it ready for %s.x only", report, got)
		default:
			why = fmt.Sprintf("its report %s names releases not on %d.x values yet", report, major)
		}
	case apierrors.IsNotFound(err):
		why = fmt.Sprintf("no migrate run has written its report %s there yet", report)
	default:
		return false, "", fmt.Errorf("get the migrate report %s/%s: %w", ns, report, err)
	}
	if len(using) > 0 {
		why = fmt.Sprintf("%s; HelmReleases on it: %v", why, using)
	}
	return false, why, nil
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
