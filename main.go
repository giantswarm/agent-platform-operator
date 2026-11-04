// agent-platform-operator reconciles each AgentPlatformOrganization
// (agent-platform.giantswarm.io) into its organization namespace. It serves
// no API of its own: only metrics and health probes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
	"github.com/giantswarm/agent-platform-operator/internal/grpcwire"
	"github.com/giantswarm/agent-platform-operator/internal/kagent"
	"github.com/giantswarm/agent-platform-operator/internal/snapshots"
	"github.com/giantswarm/agent-platform-operator/internal/substrate"
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
		curatedCopies           bool
		curatedWorkerPool       string
		kagentAPIVersion        string
		workerPoolAPIVersion    string
		objectQuota             string
		kustomizationAPIVersion string
		helmReleaseAPIVersion   string
		kagentSessions          grpcwire.Config
		substrateAPI            grpcwire.Config
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
	flag.BoolVar(&curatedCopies, "curated-copies", false, "Copy the curated objects of --platform-namespace into every organization namespace: the Harnesses its spec.harnesses names, the WorkerPool --curated-worker-pool with its ServiceAccount, the Agents and AgentTemplates labelled "+v1alpha1.CuratedLabel+"=true and the RemoteMCPServers they name. Needs --snapshot-bucket")
	flag.StringVar(&curatedWorkerPool, "curated-worker-pool", "kagent-default", "The curated WorkerPool in --platform-namespace every organization gets a copy of")
	flag.StringVar(&kagentAPIVersion, "kagent-api-version", "api.kagent.dev/v1alpha3", "api.kagent.dev API version of the Agents, AgentTemplates, Harnesses, ModelConfigs and RemoteMCPServers; the erasure deletes a deleted organization's Agents of it")
	flag.StringVar(&workerPoolAPIVersion, "workerpool-api-version", "ate.dev/v1alpha1", "ate.dev API version of the WorkerPool")
	flag.StringVar(&objectQuota, "object-quota", "", "Comma-separated <resource>=<count> hard limits of the ResourceQuota "+controller.ObjectQuotaName+" kept in every organization namespace, for example configmaps=500,count/agents.api.kagent.dev=200; empty keeps none")
	flag.StringVar(&kustomizationAPIVersion, "flux-kustomization-api-version", "kustomize.toolkit.fluxcd.io/v1", "kustomize.toolkit.fluxcd.io API version of the organizations' Flux Kustomizations, which the erasure deletes")
	flag.StringVar(&helmReleaseAPIVersion, "flux-helmrelease-api-version", "helm.toolkit.fluxcd.io/v2", "helm.toolkit.fluxcd.io API version of the HelmReleases the erasure deletes")
	flag.StringVar(&kagentSessions.Target, "kagent-target", "", "The kagent controller's gRPC API, grpc://host:port or grpcs://host[:port], through which the erasure deletes a deleted organization's sessions; empty skips them. Needed with --curated-copies")
	flag.StringVar(&kagentSessions.CAFile, "kagent-ca-file", "", "CA bundle of a grpcs --kagent-target; empty uses the system roots")
	flag.StringVar(&kagentSessions.TokenFile, "kagent-token-file", "/var/run/secrets/agent-platform-operator/kagent/token", "Projected ServiceAccount token of the kagent controller's machine audience, sent to --kagent-target")
	flag.StringVar(&substrateAPI.Target, "substrate-target", "", "Substrate's ate-api, grpcs://host[:port], which the erasure asks for a deleted organization's actors; empty skips the wait. Needed with --curated-copies")
	flag.StringVar(&substrateAPI.CAFile, "substrate-ca-file", "", "CA bundle of --substrate-target; empty uses the system roots")
	flag.StringVar(&substrateAPI.TokenFile, "substrate-token-file", "/var/run/secrets/agent-platform-operator/substrate/token", "Projected ServiceAccount token of an audience ate-api trusts, sent to --substrate-target")
	flag.StringVar(&cfg.Erasure.FluxNamespace, "flux-namespace", "", "Namespace of the installation's Flux source, where an organization's Flux Kustomization and Flux ServiceAccount live, labelled "+v1alpha1.OrganizationUIDLabel+"=<its UID>; the erasure deletes them there. Empty: there are none")
	flag.DurationVar(&cfg.Erasure.SnapshotDelay, "snapshot-erase-delay", 5*time.Minute, "How long after the first deletion of a deleted organization's snapshot prefix it is deleted again: at least Substrate's --actor-workflow-deadline (5m by default), so an upload that started before the last actor went has ended")
	flag.DurationVar(&cfg.Erasure.Poll, "erasure-poll-interval", 10*time.Second, "How often an erasure step that waits for something to be gone looks again")
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
	if cfg.ObjectQuota, err = parseQuota(objectQuota); err != nil {
		return err
	}
	kagentGV, err := schema.ParseGroupVersion(kagentAPIVersion)
	if err != nil {
		return fmt.Errorf("--kagent-api-version: %w", err)
	}
	cfg.Erasure.Agent = kagentGV.WithKind("Agent")
	kustomizationGV, err := schema.ParseGroupVersion(kustomizationAPIVersion)
	if err != nil {
		return fmt.Errorf("--flux-kustomization-api-version: %w", err)
	}
	cfg.Erasure.Kustomization = kustomizationGV.WithKind("Kustomization")
	helmReleaseGV, err := schema.ParseGroupVersion(helmReleaseAPIVersion)
	if err != nil {
		return fmt.Errorf("--flux-helmrelease-api-version: %w", err)
	}
	cfg.Erasure.HelmRelease = helmReleaseGV.WithKind("HelmRelease")
	if curatedCopies {
		if kagentSessions.Target == "" || substrateAPI.Target == "" {
			return errors.New("--curated-copies needs --kagent-target and --substrate-target: a deleted organization's sessions and actors are erased through them")
		}
		if cfg.SnapshotBucket == "" {
			return errors.New("--curated-copies needs --snapshot-bucket: a Harness copy's snapshot location is s3://<bucket>/<uid>/<harness>")
		}
		if curatedWorkerPool == "" {
			return errors.New("--curated-copies needs --curated-worker-pool")
		}
		cfg.Curated.WorkerPool = curatedWorkerPool
		cfg.Curated.Kagent = kagentGV
		if cfg.Curated.Substrate, err = schema.ParseGroupVersion(workerPoolAPIVersion); err != nil {
			return fmt.Errorf("--workerpool-api-version: %w", err)
		}
	}

	if kagentSessions.Target != "" {
		conn, err := grpcwire.Dial(kagentSessions)
		if err != nil {
			return fmt.Errorf("--kagent-target: %w", err)
		}
		defer conn.Close() //nolint:errcheck // closed on exit
		cfg.Erasure.Sessions = kagent.New(conn)
	}
	if substrateAPI.Target != "" {
		conn, err := grpcwire.Dial(substrateAPI)
		if err != nil {
			return fmt.Errorf("--substrate-target: %w", err)
		}
		defer conn.Close() //nolint:errcheck // closed on exit
		cfg.Erasure.Actors = substrate.New(conn)
	}
	if cfg.SnapshotBucket != "" {
		s3, err := snapshots.NewS3(context.Background())
		if err != nil {
			return err
		}
		cfg.Erasure.Snapshots = s3
	}

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
	// The network policy kind and the curated kinds are watched: a kind the
	// API server does not serve would fail only later, as a watch that never
	// syncs.
	watched := map[schema.GroupVersionKind]string{}
	if !gvk.Empty() {
		watched[gvk] = "--network-policy-flavor " + networkPolicyFlavor
	}
	if cfg.Curated.Enabled() {
		for _, k := range cfg.Curated.Kinds() {
			watched[k] = "--curated-copies"
		}
	}
	for k, by := range watched {
		_, err := mgr.GetRESTMapper().RESTMapping(k.GroupKind(), k.Version)
		switch {
		case meta.IsNoMatchError(err):
			return fmt.Errorf("%s: the API server does not serve %s %s", by, k.GroupVersion(), k.Kind)
		case err != nil:
			return fmt.Errorf("discovering %s: %w", k, err)
		}
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

// parseQuota reads <resource>=<count> pairs, comma-separated.
func parseQuota(s string) (corev1.ResourceList, error) {
	var out corev1.ResourceList
	for _, pair := range splitList(s) {
		name, count, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("--object-quota: %q is not <resource>=<count>", pair)
		}
		q, err := resource.ParseQuantity(strings.TrimSpace(count))
		if err != nil {
			return nil, fmt.Errorf("--object-quota: %s: %w", name, err)
		}
		if out == nil {
			out = corev1.ResourceList{}
		}
		out[corev1.ResourceName(strings.TrimSpace(name))] = q
	}
	return out, nil
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
