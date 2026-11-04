# agent-platform-operator

Reconciles each AgentPlatformOrganization (agent-platform.giantswarm.io) into its organization namespace.

**Homepage:** <https://github.com/giantswarm/agent-platform-operator>

## Source Code

* <https://github.com/giantswarm/agent-platform-operator>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| image.name | string | `"giantswarm/agent-platform-operator"` | Image repository, below `registry.domain`. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| registry.domain | string | `"gsoci.azurecr.io"` | Image registry. |
| platformNamespace | string | `"kagent"` | The platform namespace (kagent's) the network policies are copied from. |
| networkPolicy.flavor | string | `""` | Kind of the network policies copied into every organization namespace: `kubernetes` (NetworkPolicy) or `cilium` (CiliumNetworkPolicy). Empty copies none. |
| networkPolicy.names | list | `[]` | Names of the network policies in `platformNamespace` copied into every organization namespace (the platform's worker policy, `substrate-workers` with Cilium, `substrate-workers-ingress` with Kubernetes network policies). |
| agentChart.ociUrl | string | `"oci://gsoci.azurecr.io/charts/giantswarm/agent"` | OCI URL of the agent chart. The OCIRepository kept in every organization namespace is named after its last segment (`agent`); the organization's HelmReleases reference it by that name. Empty keeps none. |
| agentChart.semver | string | `"2.x"` | Semver range the OCIRepository tracks. |
| agentChart.semverFilter | string | `""` | Regular expression the chart's tags must match before the range is evaluated (the OCIRepository's `ref.semverFilter`); empty filters nothing. |
| agentChart.interval | string | `"30m"` | OCIRepository.spec.interval. |
| agentChart.ociRepositoryApiVersion | string | `"source.toolkit.fluxcd.io/v1"` | source.toolkit.fluxcd.io API version of the OCIRepository. |
| helmReleaseServiceAccount | string | `"kagent-flux"` | The ServiceAccount the organization's HelmReleases run as (`spec.serviceAccountName`, which helm-controller impersonates), kept in every organization namespace and bound there to the ClusterRole `agent-platform-tenant-flux`. The same name as agent-manager's `flux.helmReleaseServiceAccount`. Empty keeps none. |
| snapshots.bucket | string | `""` | Bucket of the organizations' worker snapshots: an organization's `status.snapshotPrefix` is `s3://<bucket>/<uid>`. Empty leaves it empty. |
| syncPeriod | string | `"10m"` | How often every organization is reconciled without a change. A change to an organization, its namespace, its objects there, a parent's organizations-admin RoleBinding or a copied network policy reconciles it at once. |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| podSecurityContext | object | `{"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |

## CI values

`ci/ci-values.yaml` holds the values the CI render (`helm template` in app-build-suite) needs beyond the defaults, such as a `required` value without a default.
