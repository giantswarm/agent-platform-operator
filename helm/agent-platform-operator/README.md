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
| serviceAccountBindings | list | `[]` | ClusterRoles bound to a ServiceAccount of another namespace in every organization namespace, such as Substrate's credential providers' Secret readers, which read the Secret an actor's credential names there. Each ClusterRole is bound by a RoleBinding named after it (listed once, not the Helm or Flux binding's name), and the operator gets `bind` on it; a ClusterRole removed from the list loses its bindings. The agent-platform chart's admission policy holds which ServiceAccounts the operator binds there. |
| snapshots.bucket | string | `""` | Bucket of the organizations' worker snapshots: an organization's `status.snapshotPrefix` is `s3://<bucket>/<uid>`. Empty leaves it empty. |
| snapshots.region | string | `""` | AWS region of the bucket, the AWS_REGION of the operator; empty takes the one the pod's environment sets, if any. |
| snapshots.endpoint | string | `""` | S3 endpoint URL; empty uses the AWS endpoint. |
| snapshots.usePathStyle | bool | `false` | Address buckets in the URL path instead of the hostname. |
| snapshots.credentials.existingSecret | string | `""` | Secret in the operator namespace containing the S3 credential pair. Empty uses the default AWS credential chain. |
| snapshots.credentials.accessKeyIdKey | string | `"accessKeyId"` | Key of the access key ID in the Secret. |
| snapshots.credentials.secretAccessKeyKey | string | `"secretAccessKey"` | Key of the secret access key in the Secret. |
| snapshots.credentials.sessionTokenKey | string | `""` | Optional key of a session token in the Secret. |
| curated.enabled | bool | `false` | Copy the curated objects of `platformNamespace`, read-only, into every organization namespace: the Harnesses an organization's `spec.harnesses` names (pointed at its own pool and snapshot prefix, kagent memory off), the WorkerPool `workerPool` with its ServiceAccount, the Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` (their template's `modelConfig` set to the organization's default ModelConfig) and the RemoteMCPServers they name. Needs `snapshots.bucket`, `erasure.kagent.target`, `erasure.substrate.target` and an API server that serves `kagentApiVersion` and `workerPoolApiVersion`. |
| curated.workerPool | string | `"kagent-default"` | The curated WorkerPool in `platformNamespace`. |
| curated.kagentApiVersion | string | `"api.kagent.dev/v1alpha3"` | api.kagent.dev API version of the Agents, AgentTemplates, Harnesses, ModelConfigs and RemoteMCPServers. |
| curated.workerPoolApiVersion | string | `"ate.dev/v1alpha1"` | ate.dev API version of the WorkerPool. |
| objectQuota | object | object counts of the kinds an organization writes | Hard limits of the ResourceQuota `agent-platform-organization-objects` kept in every organization namespace, as `<resource>: <count>`: object counts, so one organization cannot flood the API server. A key set to `null` drops its limit, also a key the chart does not set; no key keeps no ResourceQuota. A key without `/` must be a standard quota resource (`pods`, `services`, `secrets`, `configmaps`, ...): count other kinds as `count/<resource>.<group>` (`count/serviceaccounts` for the core group), or the API server refuses the ResourceQuota. The chart refuses such a key with "at '/objectQuota/<key>': got string, want null". |
| serviceAccount.annotations | object | `{}` | Annotations of the operator's ServiceAccount, for example `eks.amazonaws.com/role-arn`: the IRSA role used to delete snapshots under the organizations' prefixes of `snapshots.bucket`. |
| erasure.fluxNamespace | string | `""` | The namespace of the installation's Flux source, where an organization's Flux Kustomization and Flux ServiceAccount live, labelled agent-platform.giantswarm.io/organization-uid with its UID: the erasure deletes them there. The same as the agent-platform chart's agentPlatformOrganizations.fluxNamespace. Empty: there are none. |
| erasure.snapshotDelay | string | `"5m"` | How long after the first deletion of a deleted organization's snapshot prefix it is deleted again. Keep it at least Substrate's actor workflow deadline, which ate-api sets to 5m by default, so an upload that started before the organization's last actor went has ended. |
| erasure.pollInterval | string | `"10s"` | How often a cleanup step that waits for something to be gone looks again. |
| erasure.kustomizationApiVersion | string | `"kustomize.toolkit.fluxcd.io/v1"` | kustomize.toolkit.fluxcd.io API version of the organizations' Flux Kustomizations and of the installation's. |
| erasure.helmReleaseApiVersion | string | `"helm.toolkit.fluxcd.io/v2"` | helm.toolkit.fluxcd.io API version of the organizations' HelmReleases. |
| erasure.kagent.target | string | `""` | The kagent controller's gRPC API (`grpc://host:port` or `grpcs://host[:port]`), through which cleanup deletes a deleted organization's sessions, as the operator's ServiceAccount. Empty skips them; required with `curated.enabled`. |
| erasure.kagent.audience | string | `"kagent"` | The audience of the ServiceAccount token sent to kagent: the controller's machine audience. |
| erasure.substrate.target | string | `""` | Substrate's ate-api (`grpcs://host[:port]`), which the operator asks until a deleted organization has no actor left. Empty skips the wait; required with `curated.enabled`. |
| erasure.substrate.audience | string | `""` | The audience of the ServiceAccount token sent to ate-api: one its Kubernetes JWT provider accepts. |
| erasure.substrate.caClusterTrustBundle.signerName | string | `""` | Signer of the ClusterTrustBundles ate-api's serving certificate is verified with, projected into the pod (Substrate's `servicedns.podcert.ate.dev/identity`). The kubelet updates the projection when a bundle changes and the operator reads it at each new connection, so a rotated CA is trusted without a restart. Needs the `ClusterTrustBundleProjection` feature. Set: `caConfigMap.name` must be empty. |
| erasure.substrate.caClusterTrustBundle.matchLabels | object | `{}` | Labels a ClusterTrustBundle of `signerName` must carry to be projected (Substrate's live bundles carry `podcert.ate.dev/canarying: live`). Empty: every ClusterTrustBundle of `signerName`. |
| erasure.substrate.caConfigMap.name | string | `""` | ConfigMap holding the CA bundle ate-api's serving certificate is verified with, for a cluster without the ClusterTrustBundle projection. Empty, with `caClusterTrustBundle.signerName` empty too: the system roots. |
| erasure.substrate.caConfigMap.key | string | `"ca.crt"` | Key of the CA bundle in the ConfigMap. |
| commitMode.sourceRef.kind | string | `"GitRepository"` | Kind of the installation's Flux source in `erasure.fluxNamespace`: `GitRepository` or `OCIRepository`. |
| commitMode.sourceRef.name | string | `""` | The installation's Flux source. Set: commit mode, every organization gets, in `erasure.fluxNamespace`, a Kustomization that applies its path from this source into its namespace, and the Flux ServiceAccount that Kustomization runs as, both named after the organization namespace, bound in the organization namespace only to `clusterRole`. Empty: none. Needs `erasure.fluxNamespace`. |
| commitMode.decryptionSecret | string | `"sops-keys"` | The installation's SOPS key Secret in `erasure.fluxNamespace`, which the organizations' Kustomizations decrypt with; empty decrypts nothing. |
| commitMode.path | string | `"./agent-platform/organizations/{parent}/{name}"` | An organization's path in the source: `{parent}` and `{name}` are replaced by its parent namespace and its name. An installation puts it under its own agent-platform configuration path, outside what the installation's Kustomization applies. |
| commitMode.interval | string | `"10m"` | `spec.interval` of the organizations' Kustomizations. |
| commitMode.clusterRole | string | `"agent-platform:organization-flux"` | The ClusterRole an organization's Flux ServiceAccount is bound to in its organization namespace (rendered by the agent-platform chart). |
| commitMode.installationKustomization.name | string | `""` | The installation's Kustomization that applies the AgentPlatformOrganizations (with `deletionPolicy: Orphan`). An organization it applied is `Orphaned` while it is missing from its inventory, with reason `DeleteMerged` when it carries `agent-platform.giantswarm.io/delete-pull-request`, `MissingFromGit` otherwise. Empty: no `Orphaned` condition. |
| commitMode.installationKustomization.namespace | string | `""` | Its namespace; empty is `erasure.fluxNamespace`. |
| syncPeriod | string | `"10m"` | How often every organization is reconciled without a change. A change to an organization, its namespace, its objects there, a parent's organizations-admin RoleBinding, a copied network policy, a curated object or a ModelConfig in its namespace reconciles it at once. |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| podSecurityContext | object | `{"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |

## CI values

`ci/ci-values.yaml` holds the values the CI render (`helm template` in app-build-suite) needs beyond the defaults, such as a `required` value without a default.
