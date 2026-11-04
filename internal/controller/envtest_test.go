package controller_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
)

var (
	ociGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository"}
	npGVK  = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
)

// cluster is a kube-apiserver holding the chart's CRD and RBAC, with the
// reconciler running as the chart's ServiceAccount.
type cluster struct {
	// admin is a client with every right.
	admin client.Client
	// config is the reconciler's Config.
	config controller.Config
}

// startEnvtest starts the API server, installs the chart's CRD and applies
// its RBAC (templates/rbac.yaml), creates the platform namespace with the
// worker network policy, and runs the reconciler as the chart's
// ServiceAccount. Skipped without envtest binaries: run via
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
	).Output()
	require.NoError(t, err, "helm template: %s", stderrOf(err))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{chartDir + "/crds"},
		ErrorIfCRDPathMissing: true,
		CRDs:                  []*apiextensionsv1.CustomResourceDefinition{anyFieldsCRD(ociGVK, "ocirepositories")},
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
	for _, name := range []string{controller.OrganizationsAdminClusterRole, helmClusterRole} {
		require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}},
		}))
	}

	createNamespace(t, admin, platformNS)
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": workerPolicy, "namespace": platformNS},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{appLabel: workerApp}},
			"policyTypes": []any{"Ingress"},
		},
	}}
	require.NoError(t, admin.Create(ctx, policy))

	c := &cluster{admin: admin, config: controller.Config{
		PlatformNamespace: platformNS,
		NetworkPolicies:   []string{workerPolicy},
		NetworkPolicyGVK:  npGVK,
		Chart: controller.ChartSource{
			URL: "oci://gsoci.azurecr.io/charts/giantswarm/agent", Semver: "2.x",
			Interval: "30m", APIVersion: ociGVK.GroupVersion().String(),
		},
		HelmServiceAccount: helmSA,
		HelmClusterRole:    helmClusterRole,
		SnapshotBucket:     snapshotBucket,
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
	r := &controller.OrganizationReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Config: c.config}
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

func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}
