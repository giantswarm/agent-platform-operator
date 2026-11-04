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
| snapshots.region | string | `""` | AWS region of the bucket, the AWS_REGION of the operator; empty takes the one the pod's environment sets, if any. |
| curated.enabled | bool | `false` | Copy the curated objects of `platformNamespace`, read-only, into every organization namespace: the Harnesses an organization's `spec.harnesses` names (pointed at its own pool and snapshot prefix, kagent memory off), the WorkerPool `workerPool` with its ServiceAccount, the Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` (their template's `modelConfig` set to the organization's default ModelConfig) and the RemoteMCPServers they name. Needs `snapshots.bucket`, `erasure.kagent.target`, `erasure.substrate.target` and an API server that serves `kagentApiVersion` and `workerPoolApiVersion`. |
| curated.workerPool | string | `"kagent-default"` | The curated WorkerPool in `platformNamespace`. |
| curated.kagentApiVersion | string | `"api.kagent.dev/v1alpha3"` | api.kagent.dev API version of the Agents, AgentTemplates, Harnesses, ModelConfigs and RemoteMCPServers. |
| curated.workerPoolApiVersion | string | `"ate.dev/v1alpha1"` | ate.dev API version of the WorkerPool. |
| objectQuota | object | object counts of the kinds an organization writes | Hard limits of the ResourceQuota `agent-platform-organization-objects` kept in every organization namespace, as `<resource>: <count>`: object counts, so one organization cannot flood the API server. A key set to `null` drops its limit; no key keeps no ResourceQuota. |
| serviceAccount.annotations | object | `{}` | Annotations of the operator's ServiceAccount, for example `eks.amazonaws.com/role-arn`: the IRSA role the erasure deletes snapshots under the organizations' prefixes of `snapshots.bucket` with. |
| erasure.fluxNamespace | string | `""` | The namespace of the installation's Flux source, where an organization's Flux Kustomization and Flux ServiceAccount live, labelled agent-platform.giantswarm.io/organization-uid with its UID: the erasure deletes them there. The same as the agent-platform chart's agentPlatformOrganizations.fluxNamespace. Empty: there are none. |
| erasure.snapshotDelay | string | `"5m"` | How long after the first deletion of a deleted organization's snapshot prefix it is deleted again. Keep it at least Substrate's actor workflow deadline, which ate-api sets to 5m by default, so an upload that started before the organization's last actor went has ended. |
| erasure.pollInterval | string | `"10s"` | How often an erasure step that waits for something to be gone looks again. |
| erasure.kustomizationApiVersion | string | `"kustomize.toolkit.fluxcd.io/v1"` | kustomize.toolkit.fluxcd.io API version of the organizations' Flux Kustomizations. |
| erasure.helmReleaseApiVersion | string | `"helm.toolkit.fluxcd.io/v2"` | helm.toolkit.fluxcd.io API version of the organizations' HelmReleases. |
| erasure.kagent.target | string | `""` | The kagent controller's gRPC API (`grpc://host:port` or `grpcs://host[:port]`), through which the erasure deletes a deleted organization's sessions, as the operator's ServiceAccount. Empty skips them; required with `curated.enabled`. |
| erasure.kagent.audience | string | `"kagent"` | The audience of the ServiceAccount token sent to kagent: the controller's machine audience. |
| erasure.substrate.target | string | `""` | Substrate's ate-api (`grpcs://host[:port]`), which the erasure asks until a deleted organization has no actor left. Empty skips the wait; required with `curated.enabled`. |
| erasure.substrate.audience | string | `""` | The audience of the ServiceAccount token sent to ate-api: one its Kubernetes JWT provider accepts. |
| erasure.substrate.caConfigMap.name | string | `""` | ConfigMap holding the CA bundle ate-api's serving certificate is verified with; empty uses the system roots. |
| erasure.substrate.caConfigMap.key | string | `"ca.crt"` | Key of the CA bundle in the ConfigMap. |
| syncPeriod | string | `"10m"` | How often every organization is reconciled without a change. A change to an organization, its namespace, its objects there, a parent's organizations-admin RoleBinding, a copied network policy, a curated object or a ModelConfig in its namespace reconciles it at once. |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| podSecurityContext | object | `{"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |

## CI values

`ci/ci-values.yaml` holds the values the CI render (`helm template` in app-build-suite) needs beyond the defaults, such as a `required` value without a default.
