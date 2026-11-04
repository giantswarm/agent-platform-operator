// agent-platform-operator reconciles each AgentPlatformOrganization
// (agent-platform.giantswarm.io) into its organization namespace. It serves
// no API of its own: only metrics and health probes.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

// leaderElectionID names the Lease the replicas elect the leader with.
const leaderElectionID = "agent-platform-operator"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfg                     controller.Config
		networkPolicyFlavor     string
		networkPolicies         string
		syncPeriod              time.Duration
		metricsAddr, probeAddr  string
		leaderElect             bool
		leaderElectionNamespace string
	)
	flag.StringVar(&cfg.PlatformNamespace, "platform-namespace", "kagent", "The platform namespace the network policies are copied from")
	flag.StringVar(&networkPolicyFlavor, "network-policy-flavor", "", "Kind of the network policies copied into an organization namespace: kubernetes (NetworkPolicy) or cilium (CiliumNetworkPolicy); empty copies none")
	flag.StringVar(&networkPolicies, "network-policies", "", "Comma-separated names of the network policies copied from --platform-namespace into every organization namespace")
	flag.StringVar(&cfg.Chart.URL, "agent-chart-oci-url", "oci://gsoci.azurecr.io/charts/giantswarm/agent", "OCI URL of the agent chart; the OCIRepository in every organization namespace is named after its last segment. Empty keeps no OCIRepository")
	flag.StringVar(&cfg.Chart.Semver, "agent-chart-semver", "2.x", "Semver range the OCIRepository tracks")
	flag.StringVar(&cfg.Chart.SemverFilter, "agent-chart-semver-filter", "", "Regular expression the agent chart's tags must match before the range is evaluated, as the OCIRepository's ref.semverFilter; empty filters nothing")
	flag.StringVar(&cfg.Chart.Interval, "ocirepository-interval", "30m", "OCIRepository.spec.interval of the agent chart's source")
	flag.StringVar(&cfg.Chart.APIVersion, "flux-ocirepository-api-version", "source.toolkit.fluxcd.io/v1", "source.toolkit.fluxcd.io API version of the OCIRepository")
	flag.StringVar(&cfg.HelmServiceAccount, "helmrelease-service-account", "kagent-flux", "The ServiceAccount the organization's HelmReleases run as, kept in every organization namespace; empty keeps none")
	flag.StringVar(&cfg.HelmClusterRole, "helm-cluster-role", "agent-platform-tenant-flux", "ClusterRole bound to --helmrelease-service-account in every organization namespace")
	flag.StringVar(&cfg.SnapshotBucket, "snapshot-bucket", "", "Bucket of the organizations' worker snapshots: status.snapshotPrefix is s3://<bucket>/<uid>; empty leaves it empty")
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Minute, "How often every organization is reconciled without a change")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to; 0 disables it")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probes bind to")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Elect a leader among the replicas, so one reconciles")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "", "Namespace of the leader Lease; empty is the pod's namespace, so a run outside a cluster must set it")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	gvk, err := networkPolicyGVK(networkPolicyFlavor)
	if err != nil {
		return err
	}
	cfg.NetworkPolicyGVK = gvk
	cfg.NetworkPolicies = splitList(networkPolicies)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Cache:                         controller.CacheOptions(cfg, syncPeriod),
		Metrics:                       metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElect,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       leaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	r := &controller.OrganizationReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Config: cfg}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// networkPolicyGVK is the kind of network policy a flavor copies.
func networkPolicyGVK(flavor string) (schema.GroupVersionKind, error) {
	switch flavor {
	case "":
		return schema.GroupVersionKind{}, nil
	case "kubernetes":
		return schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, nil
	case "cilium":
		return schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}, nil
	}
	return schema.GroupVersionKind{}, fmt.Errorf("--network-policy-flavor %q: kubernetes, cilium or empty", flavor)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
