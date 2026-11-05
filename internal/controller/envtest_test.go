package controller_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

const (
	chartDir         = "../../helm/agent-platform-operator"
	releaseNamespace = "agent-platform"
	platformNS       = "kagent"
	workerPolicy     = "substrate-workers-ingress"
	helmSA           = "kagent-flux"
	helmClusterRole  = "agent-platform-tenant-flux"
	snapshotBucket   = "agent-snapshots"
	appLabel         = "app"
	workerApp        = "substrate-worker"

	// The curated objects of the platform namespace.
	curatedPool      = "kagent-default"
	poolSA           = "substrate-worker"
	curatedHarness   = "kagent"
	curatedAgent     = "agent-manager-assistant"
	platformAgent    = "platform-sre"
	curatedTemplate  = "triage"
	musterServer     = "muster"
	platformModel    = "default-model-config"
	platformLocation = "s3://platform-snapshots/kagent"
	workerImage      = "gsoci.azurecr.io/giantswarm/substrate-worker:1.0.0"
	harnessImage     = "gsoci.azurecr.io/giantswarm/kagent/golang-adk@sha256:aaaa"

	// discoveryLabel is the label kagent reads to skip a RemoteMCPServer's
	// tool discovery.
	discoveryLabel = "kagent.dev/discovery"
	// promptField is an agent template's system prompt field.
	promptField = "systemPrompt"
	// agentPrompt is the curated Agent's system prompt.
	agentPrompt = "Help with agents."

	// configMapQuota is the ConfigMap count of the object quota: no other
	// case writes ConfigMaps.
	configMapQuota = 3

	// snapshotDelay is the erasure's delay between the two deletions of a
	// snapshot prefix.
	snapshotDelay = 2 * time.Second

	// Commit mode: the installation's Flux source, SOPS key and
	// Kustomization in fluxNS, and the ClusterRole an organization's Flux
	// ServiceAccount is bound to.
	fluxSource          = "management-clusters-fleet"
	sopsSecret          = "sops-keys"
	installationKS      = "flux"
	organizationFluxCR  = "agent-platform:organization-flux"
	organizationPathFmt = "./agent-platform/organizations/{parent}/{name}"

	// A ServiceAccountBinding: a credential provider's Secret reader
	// ClusterRole and its ServiceAccount in Substrate's namespace.
	bindingClusterRole = "k8s-credential-provider-secret-reader"
	bindingNS          = "ate-system"
	bindingSA          = "k8s-credential-provider"
)

var (
	ociGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository"}
	npGVK  = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
	// The versions the agent-platform chart ships: the kagent line's
	// api.kagent.dev/v1alpha3 and Substrate's ate.dev/v1alpha1.
	kagentGV    = schema.GroupVersion{Group: "api.kagent.dev", Version: "v1alpha3"}
	substrateGV = schema.GroupVersion{Group: "ate.dev", Version: "v1alpha1"}
	agentGVK    = kagentGV.WithKind("Agent")
	templateGVK = kagentGV.WithKind("AgentTemplate")
	harnessGVK  = kagentGV.WithKind("Harness")
	modelGVK    = kagentGV.WithKind("ModelConfig")
	serverGVK   = kagentGV.WithKind("RemoteMCPServer")
	poolGVK     = substrateGV.WithKind("WorkerPool")
	// The Flux kinds the erasure deletes.
	kustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	helmReleaseGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
)

// cluster is a kube-apiserver holding the chart's CRD and RBAC, with the
// reconciler running as the chart's ServiceAccount.
type cluster struct {
	// admin is a client with every right.
	admin client.Client
	// rest is the API server's admin config, to impersonate others with.
	rest *rest.Config
	// config is the reconciler's Config.
	config controller.Config
	// log records, in order, what the erasure did through the fakes of
	// kagent's session API, Substrate's actors and the snapshot bucket, and
	// what the erasure observers saw it delete.
	log      *eventLog
	sessions *fakeSessions
	actors   *fakeActors
	bucket   *fakeBucket
}

// startEnvtest starts the API server, installs the chart's CRD and applies
// its RBAC (templates/rbac.yaml), creates the platform namespace with the
// worker network policy and the Flux namespace, and runs the reconciler, in
// commit mode, as the chart's ServiceAccount. Skipped without envtest binaries: run via
// `make test-envtest`, which resolves KUBEBUILDER_ASSETS with setup-envtest
// (and needs helm).
func startEnvtest(t *testing.T) *cluster {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test-envtest`")
	}
	rendered, err := exec.Command("helm", "template", "agent-platform-operator", chartDir,
		"--namespace", releaseNamespace,
		"--show-only", "templates/rbac.yaml",
		"--set", "networkPolicy.flavor=kubernetes",
		"--set", "networkPolicy.names={"+workerPolicy+"}",
		"--set", "curated.enabled=true",
		"--set", "erasure.fluxNamespace="+fluxNS,
		"--set", "commitMode.sourceRef.name="+fluxSource,
		"--set", "commitMode.installationKustomization.name="+installationKS,
		"--set", "snapshots.bucket="+snapshotBucket,
		"--set", "erasure.kagent.target=grpc://kagent-controller.kagent.svc:8083",
		"--set", "erasure.substrate.target=grpcs://ate-api.substrate.svc",
		"--set", "erasure.substrate.audience=ate-api",
		"--set-json", `serviceAccountBindings=[{"clusterRole":"`+bindingClusterRole+`","serviceAccount":{"namespace":"`+bindingNS+`","name":"`+bindingSA+`"}}]`,
	).Output()
	require.NoError(t, err, "helm template: %s", stderrOf(err))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{chartDir + "/crds"},
		ErrorIfCRDPathMissing: true,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			anyFieldsCRD(ociGVK, "ocirepositories"),
			anyFieldsCRD(agentGVK, "agents"),
			anyFieldsCRD(templateGVK, "agenttemplates"),
			anyFieldsCRD(harnessGVK, "harnesses"),
			anyFieldsCRD(modelGVK, "modelconfigs"),
			anyFieldsCRD(serverGVK, "remotemcpservers"),
			anyFieldsCRD(poolGVK, "workerpools"),
			withStatus(anyFieldsCRD(kustomizationGVK, "kustomizations")),
			anyFieldsCRD(helmReleaseGVK, "helmreleases"),
		},
	}
	cfg, err := env.Start()
	require.NoError(t, err, "start envtest")
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()

	createNamespace(t, admin, releaseNamespace)
	operator := ""
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		require.NoError(t, admin.Create(ctx, u), "create %s %s", u.GetKind(), u.GetName())
		if u.GetKind() == "ClusterRoleBinding" {
			subjects, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			sa := subjects[0].(map[string]any)
			operator = "system:serviceaccount:" + sa["namespace"].(string) + ":" + sa["name"].(string)
		}
	}
	require.NotEmpty(t, operator, "the chart renders no ClusterRoleBinding for the operator")

	// The ClusterRoles the operator binds grant what the operator does not
	// hold itself, so only `bind` lets it write the bindings.
	for _, name := range []string{controller.OrganizationsAdminClusterRole, helmClusterRole, organizationFluxCR, bindingClusterRole} {
		require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create"}}},
		}))
	}

	createNamespace(t, admin, platformNS)
	createNamespace(t, admin, fluxNS)
	policy := newObject(npGVK, platformNS, workerPolicy, nil, map[string]any{
		"podSelector": map[string]any{"matchLabels": map[string]any{appLabel: workerApp}},
		"policyTypes": []any{"Ingress"},
	})
	require.NoError(t, admin.Create(ctx, policy))
	for _, obj := range curatedSources() {
		require.NoError(t, admin.Create(ctx, obj), "create %s %s", obj.GetKind(), obj.GetName())
	}
	// envtest runs no kube-controller-manager: stand in for its quota
	// controller, without which the quota admission refuses every object a
	// ResourceQuota counts.
	go recordQuotaUsage(ctx, admin)

	log := &eventLog{}
	sessions := &fakeSessions{log: log, byID: map[string]*fakeSession{}, pkg: pkgV1alpha1}
	actors := &fakeActors{log: log, sessions: sessions, linger: map[string]int{}}
	bucket := &fakeBucket{log: log, objects: map[string]int{}, fail: map[string]error{}}
	c := &cluster{admin: admin, rest: cfg, log: log, sessions: sessions, actors: actors, bucket: bucket, config: controller.Config{
		PlatformNamespace: platformNS,
		NetworkPolicies:   []string{workerPolicy},
		NetworkPolicyGVK:  npGVK,
		Chart: controller.ChartSource{
			URL: "oci://gsoci.azurecr.io/charts/giantswarm/agent", Semver: "2.x",
			Interval: "30m", APIVersion: ociGVK.GroupVersion().String(),
		},
		HelmServiceAccount: helmSA,
		HelmClusterRole:    helmClusterRole,
		ServiceAccountBindings: []controller.ServiceAccountBinding{
			{ClusterRole: bindingClusterRole, ServiceAccount: types.NamespacedName{Namespace: bindingNS, Name: bindingSA}},
		},
		SnapshotBucket: snapshotBucket,
		Curated:        controller.CuratedConfig{WorkerPool: curatedPool, Kagent: kagentGV, Substrate: substrateGV},
		ObjectQuota: corev1.ResourceList{
			corev1.ResourceConfigMaps: resource.MustParse(strconv.Itoa(configMapQuota)),
			// The operator's own Agent copies count too.
			"count/agents.api.kagent.dev": resource.MustParse("100"),
		},
		Erasure: controller.ErasureConfig{
			Sessions: sessions.serve(t), Actors: actors, Snapshots: bucket,
			SnapshotDelay: snapshotDelay, Poll: 100 * time.Millisecond,
			HelmRelease: helmReleaseGVK, Agent: agentGVK,
		},
		Flux: controller.FluxConfig{
			Namespace: fluxNS, Kustomization: kustomizationGVK,
			SourceKind: "GitRepository", SourceName: fluxSource, DecryptionSecret: sopsSecret,
			Path: organizationPathFmt, Interval: "10m", ClusterRole: organizationFluxCR,
			Installation: types.NamespacedName{Namespace: fluxNS, Name: installationKS},
		},
	}}

	asOperator := rest.CopyConfig(cfg)
	asOperator.Impersonate = rest.ImpersonationConfig{UserName: operator}
	mgr, err := ctrl.NewManager(asOperator, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  controller.CacheOptions(c.config, time.Hour),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)
	r := &controller.OrganizationReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Cache: mgr.GetCache(), Config: c.config}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	return c
}

// curatedSources are the platform namespace's curated objects: a Harness
// with memory on, the WorkerPool it runs on, a curated Agent reaching muster,
// a curated AgentTemplate, the muster RemoteMCPServer, and an Agent that is
// not curated.
func curatedSources() []*unstructured.Unstructured {
	obj := func(gvk schema.GroupVersionKind, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
		return newObject(gvk, platformNS, name, labels, spec)
	}
	server := nameRef(musterServer)
	server["kind"] = "RemoteMCPServer"
	tools := []any{map[string]any{"mcp": map[string]any{"server": server}}}
	return []*unstructured.Unstructured{
		obj(harnessGVK, curatedHarness, nil, map[string]any{
			"workload": map[string]any{"image": harnessImage},
			"kagent": map[string]any{
				"memory":     map[string]any{"modelConfigRef": nameRef("embeddings"), "ttlDays": int64(15)},
				"compaction": map[string]any{"tokenThreshold": int64(1000), "summarizer": map[string]any{"modelConfigRef": nameRef(platformModel)}},
			},
			"substrate": map[string]any{
				"workerPoolRef":  nameRef("platform-pool"),
				"snapshotPolicy": map[string]any{"location": platformLocation},
			},
		}),
		obj(poolGVK, curatedPool, nil, map[string]any{
			"replicas":     int64(4),
			"workerImage":  workerImage,
			"sandboxClass": "gvisor",
			"template":     map[string]any{"serviceAccountName": poolSA},
		}),
		obj(agentGVK, curatedAgent, curatedLabels, agentSpec(agentPrompt, map[string]any{"modelConfig": nameRef(platformModel), "tools": tools})),
		obj(agentGVK, platformAgent, nil, agentSpec("Platform only.", nil)),
		obj(templateGVK, curatedTemplate, curatedLabels, templateSpec("Triage.", map[string]any{"modelConfig": nameRef(platformModel), "tools": tools})),
		obj(serverGVK, musterServer, map[string]string{discoveryLabel: "disabled", "app.kubernetes.io/name": "agent"}, map[string]any{"url": "https://muster.example.com/mcp", "description": "muster"}),
	}
}

// curatedLabels make an Agent or AgentTemplate of the platform namespace
// curated.
var curatedLabels = map[string]string{v1alpha1.CuratedLabel: "true"}

// newObject is an object of gvk ns/name with labels and spec.
func newObject(gvk schema.GroupVersionKind, ns, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

// templateSpec is an agent template's spec: the system prompt and fields.
func templateSpec(prompt string, fields map[string]any) map[string]any {
	spec := map[string]any{promptField: prompt}
	for k, v := range fields {
		spec[k] = v
	}
	return spec
}

// agentSpec is the spec of an Agent on the curated Harness with an inline
// template.
func agentSpec(prompt string, fields map[string]any) map[string]any {
	return map[string]any{"harnessRef": nameRef(curatedHarness), "template": templateSpec(prompt, fields)}
}

// nameRef is a reference to an object by name.
func nameRef(name string) map[string]any { return map[string]any{nameField: name} }

// nameField is the name field of a reference.
const nameField = "name"

// recordQuotaUsage does what kube-controller-manager's quota controller does
// for a new ResourceQuota until ctx ends: it records the hard limits and the
// objects counted so far in the status. The quota admission counts every
// object it admits after that.
func recordQuotaUsage(ctx context.Context, c client.Client) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		quotas := &corev1.ResourceQuotaList{}
		if err := c.List(ctx, quotas); err != nil {
			continue
		}
		for i := range quotas.Items {
			q := &quotas.Items[i]
			if equality.Semantic.DeepEqual(q.Status.Hard, q.Spec.Hard) {
				continue
			}
			used := corev1.ResourceList{}
			for name := range q.Spec.Hard {
				// A kind the API server does not serve counts nothing.
				n, _ := countObjects(ctx, c, q.Namespace, name)
				used[name] = *resource.NewQuantity(int64(n), resource.DecimalSI)
			}
			q.Status = corev1.ResourceQuotaStatus{Hard: q.Spec.Hard, Used: used}
			_ = c.Status().Update(ctx, q)
		}
	}
}

// countObjects counts the objects of a ResourceQuota object-count resource
// (configmaps, count/<resource>.<group>) in ns.
func countObjects(ctx context.Context, c client.Client, ns string, name corev1.ResourceName) (int, error) {
	gr := schema.ParseGroupResource(strings.TrimPrefix(string(name), "count/"))
	gvk, err := c.RESTMapper().KindFor(gr.WithVersion(""))
	if err != nil {
		return 0, err
	}
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func createNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// anyFieldsCRD is a namespaced CustomResourceDefinition that takes any fields.
func anyFieldsCRD(gvk schema.GroupVersionKind, plural string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + gvk.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvk.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: gvk.Kind, ListKind: gvk.Kind + "List", Plural: plural},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvk.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

// withStatus gives crd a status subresource, as Flux's Kustomization has: a
// status write then leaves the generation as it is.
func withStatus(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinition {
	for i := range crd.Spec.Versions {
		crd.Spec.Versions[i].Subresources = &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}}
	}
	return crd
}

func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}
