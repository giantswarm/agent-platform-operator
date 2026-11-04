# agent-platform-operator

Operator of the Agent Platform's organizations: reconciles each AgentPlatformOrganization (agent-platform.giantswarm.io) into its organization namespace, curated copies and erasure

## The AgentPlatformOrganization

An `AgentPlatformOrganization` (`agent-platform.giantswarm.io/v1alpha1`, namespaced) is a share of the installation's Agent Platform for one team. It lives in a **parent namespace**: on a Giant Swarm installation the Giant Swarm Organization's `org-<name>`. Its agents, MCP servers, models, credentials and members live in its **organization namespace**, which the operator creates.

```yaml
apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: AgentPlatformOrganization
metadata:
  name: payments
  namespace: org-acme
  annotations:
    agent-platform.giantswarm.io/display-name: Payments
spec:
  harnesses: [kagent]
  workers:
    replicas: 1
```

- `spec.harnesses`: the curated Harnesses the organization gets.
- `spec.workers.replicas`: the warm workers of the organization's WorkerPool, while it can use them (see "Curated copies").
- The annotation `agent-platform.giantswarm.io/display-name` holds the name people see. It is not a spec field.
- The name is at most 50 characters and holds no dot, because the organization namespace name `agents-<name>-…` must be a DNS label. The CRD refuses other names.

The status holds:

- `namespace`: the organization namespace.
- `snapshotPrefix`: `s3://<bucket>/<uid>`, where the organization's worker snapshots live. It is empty when the operator has no bucket (`snapshots.bucket`).
- `conditions`, each with `observedGeneration`. `Ready` is `True` when the namespace exists and holds every object below. It is `False` with reason `SourceMissing` while a network policy to copy is missing from `platformNamespace`, `NamespaceTerminating` while the organization namespace is terminating, `NameTaken` while an object the operator did not write holds the name of a RoleBinding, a curated copy or the ResourceQuota it keeps (a mirror keeps the parent binding's name), and `ReconcileFailed` when a write fails. `SourceMissing` also covers a curated object to copy.
  - `DefaultModelConfigured` (set with `curated.enabled`) is `True` while exactly one ModelConfig in the organization namespace carries the annotation `agent-platform.giantswarm.io/default-model-config: "true"` (giantswarm-platform-manager#789). It is `False` with reason `NoDefaultModelConfig` when none does and `SeveralDefaultModelConfigs` when more than one does: the curated Agents then have no model.
  - `Orphaned` (set with `commitMode.installationKustomization.name`) is `True` while the organization carries the ownership labels of the installation's Kustomization (`kustomize.toolkit.fluxcd.io/name` and `/namespace`) but is missing from that Kustomization's `status.inventory`: its file is gone from git. The reason is `DeleteMerged` when the organization carries `agent-platform.giantswarm.io/delete-pull-request` (agent-manager's delete pull request merged), and `MissingFromGit` otherwise (a git mistake). It is `False` with reason `InInventory` while the organization is in the inventory, and absent on an organization without those labels (applied without git). While the operator cannot read the inventory, the condition stays as it was. The worker replica rule reads it.

## The organization namespace

The operator creates the namespace with `generateName: agents-<name>-`, so the API server adds 5 random characters (`agents-payments-x7k2q`). The namespace carries these labels:

| Label | Value |
| --- | --- |
| `agent-platform.giantswarm.io/organization` | the AgentPlatformOrganization's name |
| `agent-platform.giantswarm.io/parent` | the parent namespace |
| `agent-platform.giantswarm.io/organization-uid` | the AgentPlatformOrganization's UID |

The operator finds the namespace by the `organization-uid` label, never by its name. When the status is lost, or a create stopped before the status was written, the next reconcile finds the same namespace and creates no second one. The namespace has no ownerReference to the AgentPlatformOrganization.

In the namespace the operator keeps, from the first reconcile on:

- the network policies `networkPolicy.names`, copied from `platformNamespace`;
- the agent chart's OCIRepository (`agentChart.*`), named after the chart (`agent`);
- the Helm ServiceAccount `helmReleaseServiceAccount`, which the organization's HelmReleases run as, with a RoleBinding to the ClusterRole `agent-platform-tenant-flux`;
- a mirror of every RoleBinding in the parent namespace to the ClusterRole `agent-platform:organizations-admin`: the same name and roleRef, with the `User` and `Group` subjects only. The operator skips ServiceAccount subjects and subjects whose name starts with `system:`. The mirror goes when the parent binding goes.

- the ResourceQuota `agent-platform-organization-objects` on object counts (`objectQuota`), so one organization cannot flood the API server with objects;
- in commit mode, the RoleBinding `organization-flux` that binds the organization's Flux ServiceAccount to `commitMode.clusterRole` (`agent-platform:organization-flux`).

## Commit mode

With `commitMode.sourceRef.name` (it needs `erasure.fluxNamespace`), an organization's contents (agents, models, connectors, egress, members) come from git. The operator keeps, for each organization, beside the installation's Flux source in `erasure.fluxNamespace`:

- a ServiceAccount, the organization's Flux ServiceAccount, bound only in the organization namespace, by the RoleBinding `organization-flux`;
- a Kustomization that applies the organization's path (`commitMode.path`, `{parent}` and `{name}` replaced) from the source `commitMode.sourceRef`, decrypted with the SOPS key `commitMode.decryptionSecret`, into the organization namespace (`targetNamespace`) as that ServiceAccount (`serviceAccountName`, which Flux impersonates). A merged file that writes outside the organization namespace is refused. The Kustomization prunes (`prune: true`), waits on deletion until what it applied is gone (`deletionPolicy: WaitForTermination`) and is never suspended (`suspend: false`). An edit of it is reverted.

Both are named after the organization namespace (`agents-payments-x7k2q`), so every organization Kustomization starts with `agents-`. Both carry `agent-platform.giantswarm.io/organization-uid: <uid>` and `agent-platform.giantswarm.io/organization-object: flux`. The erasure deletes the Kustomization first and the ServiceAccount last. Keep `commitMode.path` outside what the installation's Kustomization applies, or the installation's identity applies the organization's files too. No controller-wide Flux multi-tenancy flag is needed.

## Curated copies

With `curated.enabled` (it needs `snapshots.bucket`), the operator also keeps read-only copies of the curated objects of `platformNamespace` in every organization namespace:

- the Harnesses `spec.harnesses` names, with `substrate.workerPoolRef` set to the organization's pool, `substrate.snapshotPolicy.location` set to `s3://<bucket>/<uid>/<harness>` `kagent.memory` removed, and no compaction summarizer model (compaction uses the agent's own model);
- the WorkerPool `curated.workerPool` with the ServiceAccount its workers run as, and the placement labels `kagent.dev/worker-pool-name` and `kagent.dev/worker-pool-namespace`;
- the Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` (and an AgentTemplate a curated Agent's `templateRef` names), with the template's `modelConfig` set to the organization's default ModelConfig, or removed while there is none;
- the RemoteMCPServers the tools of those templates name: the muster server the curated Agents reach their tools through.

A copy carries its source's `kagent.dev/` labels (`kagent.dev/discovery: disabled` on the muster server), and no other label of the source.

**The worker replica rule**: the WorkerPool copy runs `spec.workers.replicas`, except 0 replicas while the organization namespace holds no ModelConfig (of any name) and while the organization is `Orphaned`. A deleted organization runs `spec.workers.replicas` until its sessions are gone, whatever else holds, since kagent needs a worker to suspend an actor, and 0 after (see "Erasure").

A copy follows its source: a change in `platformNamespace`, a move of the default mark, or a change of `spec.harnesses` re-copies it into every organization it concerns. A copy whose source is no longer curated for the organization goes. An edit of a copy is reverted. The copies carry `agent-platform.giantswarm.io/organization-object: curated`, which the admission policy that refuses edits to them selects, and `agent-platform.giantswarm.io/copied-from: <platformNamespace>/<name>`.

The operator labels what it writes with `agent-platform.giantswarm.io/organization-object` and `app.kubernetes.io/managed-by: agent-platform-operator`. It never changes or deletes an object without that label, except what the erasure deletes in a deleted organization's namespace.

## Erasure

Every AgentPlatformOrganization carries the finalizer `agent-platform.giantswarm.io/erasure`. When it is deleted, by itself or with its parent namespace, the operator erases it in this order, and removes the finalizer only after the last step:

1. **Workloads**: it deletes the organization's Flux Kustomization and waits until it is gone, then deletes the HelmReleases left in the organization namespace (all of them where there is no Kustomization) and its Agents (the curated copies, and Agents written outside git), and waits until they are gone.
2. **Sessions**: it lists every creator's sessions of the organization's Agents through the kagent controller (`erasure.kagent.target`) and deletes each, until none is left, then waits until Substrate's ate-api (`erasure.substrate.target`) holds no actor in the organization's atespace (the namespace's name).
3. **Snapshots**: it deletes every object under `status.snapshotPrefix`, and again after `erasure.snapshotDelay` (default 5m, Substrate's default actor workflow deadline), for an upload that started before the last actor went.
4. **Namespace**: it deletes the organization namespace and waits until it is gone.
5. **OutsideObjects**: it deletes the organization's Flux ServiceAccount beside the installation's Flux source, and waits until it is gone.
6. It removes the finalizer.

While it erases, `Ready` is `False` with reason `Deleting`; `status.erasureStep` and the condition's message name the step, and the message names a failure. A failing step is retried with backoff; a step that waits looks again every `erasure.pollInterval`. An organization created again with the same parent and name gets a new UID, so a new namespace and snapshot prefix.

The erasure finds the organization's Flux Kustomization and Flux ServiceAccount in the namespace of the installation's Flux source (`erasure.fluxNamespace`, the agent-platform chart's `agentPlatformOrganizations.fluxNamespace`) by the label `agent-platform.giantswarm.io/organization-uid: <uid>`, the label the agent-platform chart's admission policy holds the operator's writes there to. It looks nowhere else: the label in another namespace names nothing the operator created. Without `erasure.fluxNamespace` there are none.

The erasure keeps its progress in `status.erasureStep` and `status.snapshotsErasedAt`: install the chart so that its CRD is upgraded with it (Flux `upgrade.crds: CreateReplace`), or the API server drops those fields.

kagent's session service deletes another creator's session only for its own control plane today, so the Sessions step fails on such a session, with kagent's answer in the condition, until kagent authorizes the delete for a caller granted `delete` on `sessions.api.kagent.dev` cluster-wide ([giantswarm/kagent-extensions#27](https://github.com/giantswarm/kagent-extensions/issues/27)). The erasure of an organization with a person's session works once that change ships in the kagent line.

The operator calls kagent and Substrate as its own ServiceAccount, with a projected token of each server's audience (`erasure.kagent.audience`, `erasure.substrate.audience`): kagent must admit it as a machine caller with cluster-wide `list` and `delete` on `sessions.api.kagent.dev`, and ate-api must trust the token. It deletes snapshots with the AWS SDK's default credentials: on a management cluster, the IRSA role in `serviceAccount.annotations`. The agent-platform chart renders that role and the kagent grant.

## Development

See [docs/development.md](docs/development.md).
