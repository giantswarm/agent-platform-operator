# agent-platform-operator

Operator of the Agent Platform's organizations: reconciles each Organization (security.giantswarm.io) with the Agent Platform enabled into its organization namespace, curated copies and organization cleanup

## Terms

The operator writes two kinds of derived objects into an organization namespace, and each has its own word. A **curated copy** is an object that the operator keeps equal to a curated source in `platformNamespace`, apart from the rewrites for each organization (see "Curated copies"); the network policies (`networkPolicy.names`) are copies too, but not curated ones.

## The organization

An organization is an `Organization` (`security.giantswarm.io/v1alpha1`, cluster-scoped, the kind of organization-operator) whose `spec.capabilities.agentPlatform.enabled` is `true`. Its agents, MCP servers, models, credentials and members live in its **organization namespace**, which the operator creates. The operator imports the kind's Go types from `github.com/giantswarm/organization-operator/api/v1alpha1`; the agent-platform chart installs its CRD on a cluster without KaaS.

```yaml
apiVersion: security.giantswarm.io/v1alpha1
kind: Organization
metadata:
  name: payments
  annotations:
    agent-platform.giantswarm.io/display-name: Payments
spec:
  capabilities:
    agentPlatform:
      enabled: true
      harnesses: [kagent]
      workers:
        replicas: 1
```

- `spec.capabilities.agentPlatform.enabled`: the switch. An `Organization` without it gets nothing from the operator. Once it is on, it cannot be turned off for now: the agent-platform chart's admission policy refuses a write that turns it off or removes `spec.capabilities.agentPlatform`, because a switch-off is not supported yet. When the operator finds the switch off, or the block gone, on an `Organization` that carries its finalizer all the same, it changes nothing and erases nothing (the namespace, its contents, the admins binding and the finalizer stay as they are), and `AgentPlatformReady` is `False` with reason `AgentPlatformDisabled` and a message that says so. A delete of the `Organization` still erases it.
- `spec.capabilities.agentPlatform.harnesses`: the curated Harnesses the organization gets.
- `spec.capabilities.agentPlatform.workers.replicas`: the warm workers of the organization's WorkerPool, while it uses them (see "Curated copies"). Not set (also with no `workers` block) means 1, and the CRD writes that default into the object; 0 set on purpose runs no worker.
- The annotation `agent-platform.giantswarm.io/display-name` holds the name people see. It is not a spec field.
- The organization namespace is `agents-<name>`, which must be a DNS label: a name longer than 56 characters or with a dot gets no namespace, and `AgentPlatformReady` is `False` with reason `NamespaceNameInvalid`.

The operator writes its own parts of the status, in the shape organization-operator's API sets out ([giantswarm/organization-operator#673](https://github.com/giantswarm/organization-operator/pull/673)), and keeps the fields and entries of other writers: organization-operator writes `status.namespace` and the `kaas` entry of `status.namespaces`, as its own field manager. A JSON merge patch replaces a whole list, so the operator sends its status under an optimistic lock: a write of another writer since its read makes the API server refuse it, and the operator reconciles again. The operator writes:

- the `agentPlatform` entry of `status.namespaces`: `{name: <organization namespace>, capability: agentPlatform}`.
- `status.capabilities.agentPlatform.snapshotPrefix`: `s3://<bucket>/<uid>`, where the organization's worker snapshots live. It is empty when the operator has no bucket (`snapshots.bucket`).
- `status.capabilities.agentPlatform.lastAppliedRevision`: the last source revision the Kustomization that applied the organization applied it from (e.g. `<branch>@sha1:<sha>` for a GitRepository), that Kustomization's `status.lastAppliedRevision` while the organization is in its settled inventory. It is kept while `AgentPlatformOrphaned` is `True`, so a removed file can be found in that revision. It is empty when no such revision was recorded: on an organization applied without Flux, and on one already missing from the inventory when the operator first ran with this field.
- its conditions in `status.conditions`, each with `observedGeneration`; their types start with `AgentPlatform`. `AgentPlatformReady` is `True` when the namespace exists and holds every object below. It is `False` with reason `SourceMissing` while a network policy to copy is missing from `platformNamespace`, `NamespaceTerminating` while the organization namespace is terminating, `NameTaken` while an object the operator did not write holds the name of the organization namespace, a RoleBinding, a curated copy or the ResourceQuota it keeps, and `ReconcileFailed` when a write fails. `SourceMissing` also covers a curated object to copy.
  - `AgentPlatformDefaultModelConfigured` (set with `curated.enabled`) is `True` while exactly one ModelConfig in the organization namespace carries the annotation `agent-platform.giantswarm.io/is-default-model-config: "true"` (giantswarm-platform-manager#789). It is `False` with reason `NoDefaultModelConfig` when none does and `SeveralDefaultModelConfigs` when more than one does: the curated Agents then have no model.
  - `AgentPlatformOrphaned` (set with `commitMode.installationKustomization.name`) is `True` while the organization carries the ownership labels of a Flux Kustomization (`kustomize.toolkit.fluxcd.io/name` and `/namespace`) but is missing from that Kustomization's `status.inventory`: its file is gone from git. That Kustomization is the installation's or any other, in any namespace: the operator reads the one the labels name, and watches every Kustomization of the cluster, keeping only the `Organization` entries of each inventory. The reason is `DeleteMerged` when the organization carries `agent-platform.giantswarm.io/delete-pull-request` (agent-manager's delete pull request merged), and `MissingFromGit` otherwise (a git mistake). The operator reads the annotation whoever set it: agent-manager sets it for an organization whose file is in agent-manager's own path in git; for an organization that another Kustomization applies, nothing sets it yet, so it is set by hand to the URL of the pull request that removed the organization's file. It is `False` with reason `InInventory` while the organization is in the inventory, and absent on an organization without those labels (applied without git). While the operator cannot read the inventory, and while that Kustomization is not settled (not `Ready` for its current generation, or `Reconciling`: Flux labels an object before it records it in the inventory), the condition stays as it was. The operator is not ready (its readiness probe fails, naming the flag) while `--installation-kustomization` names a Kustomization that it cannot read (one that does not exist, or no right to read it), and becomes ready once Flux creates it. Apart from that check, the setting is the switch of the condition: the inventory read is the one of the Kustomization that the labels name. A Kustomization or Flux ServiceAccount named after the organization namespace that lost the organization's UID label is not taken over (`NameTaken`), and cleanup stops at it in its Workloads step, before the namespace goes, naming it and both labels to restore (`organization-uid` and `organization-object: flux`), until they are back or someone deletes it. The worker replica rule reads it.
  - `AgentPlatformErasing` and `AgentPlatformSnapshotsErased` record the progress of organization cleanup (see "Organization cleanup").

## The organization namespace

The operator creates the namespace `agents-<name>`: an `Organization` name is unique in the cluster, so the namespace name is too. The namespace carries these labels:

| Label | Value |
| --- | --- |
| `agent-platform.giantswarm.io/organization` | the Organization's name |
| `agent-platform.giantswarm.io/organization-uid` | the Organization's UID |

The operator writes only a namespace that carries the organization's UID. A namespace `agents-<name>` that another writer created, or that an earlier `Organization` of the same name left while it terminates, is not taken over: the organization is `AgentPlatformReady=False NameTaken` until it is gone. The namespace has no ownerReference to the `Organization`.

The right to manage `Organization` objects is bound by the installation: it binds `agent-platform:organizations-admin` for the whole cluster. The operator writes no binding for it.

In the namespace the operator keeps, from the first reconcile on:

- the network policies `networkPolicy.names`, copied from `platformNamespace`;
- the agent chart's OCIRepository (`agentChart.*`), named after the chart (`agent`);
- the Helm ServiceAccount `helmReleaseServiceAccount`, which the organization's HelmReleases run as, with a RoleBinding to the ClusterRole `agent-platform-tenant-flux`;
- the RoleBinding `organization-admins`, which binds the ClusterRole `agent-platform:admin` to the Users and Groups of the organization's `spec.admins`, in their order. It follows the list while the Agent Platform is on, and an edit of it is reverted. With no admins the operator empties its binding and deletes it (emptied first, so a finalizer that holds the binding keeps no access), the organization has no such binding, and that is no error: the platform team manages the organization. A RoleBinding `organization-admins` that the operator did not write is left as it is, and the organization is `AgentPlatformReady=False NameTaken` while it has admins. The operator writes no binding in other namespaces from the list (no `cluster-admin`);
- the ResourceQuota `agent-platform-organization-objects` on object counts (`objectQuota`), so one organization cannot flood the API server with objects;
- in commit mode, the RoleBinding `organization-flux` that binds the organization's Flux ServiceAccount to `commitMode.clusterRole` (`agent-platform:organization-flux`).

## Commit mode

With `commitMode.sourceRef.name` (it needs `erasure.fluxNamespace`), an organization's contents (agents, models, connectors, egress, members) come from git. The operator keeps, for each organization, beside the installation's Flux source in `erasure.fluxNamespace`:

- a ServiceAccount, the organization's Flux ServiceAccount, bound only in the organization namespace, by the RoleBinding `organization-flux`;
- a Kustomization that applies the organization's path (`commitMode.path`, `{name}` replaced) from the source `commitMode.sourceRef`, decrypted with the SOPS key `commitMode.decryptionSecret`, into the organization namespace (`targetNamespace`) as that ServiceAccount (`serviceAccountName`, which Flux impersonates). A merged file that writes outside the organization namespace is refused. The Kustomization prunes (`prune: true`), waits on deletion until what it applied is gone (`deletionPolicy: WaitForTermination`) and is never suspended (`suspend: false`). An edit of it is reverted.

Both are named after the organization namespace (`agents-payments`), so every organization Kustomization starts with `agents-`. Both carry `agent-platform.giantswarm.io/organization-uid: <uid>` and `agent-platform.giantswarm.io/organization-object: flux`. The operator deletes the Kustomization first and the ServiceAccount last. Keep `commitMode.path` outside what the installation's Kustomization applies, or the installation's identity applies the organization's files too. No controller-wide Flux multi-tenancy flag is needed.

Every organization's Kustomization decrypts with the installation's SOPS key, as every other manager writing to the installation's repository does. A file merged into an organization's path can therefore carry ciphertext copied from elsewhere in the repository, which Flux decrypts into the organization namespace. A `kustomization.yaml` in an organization's path can also list a file of another path (`../`), which the same key decrypts. A Flux source or a SOPS key per organization, which closes this, is tracked in giantswarm/agent-platform-operator#15. Until then the control is platform review of every merge into an organization's path, through CODEOWNERS of the installation repository that an organization's admin cannot change, never that organization's admin alone: agent-manager#292 (slice 11, the repository layout) owns it. A CI check in the installation repositories that refuses `..` in an organization's `kustomization.yaml` is giantswarm/giantswarm#39013. The agent-platform chart's ClusterRole `agent-platform:organization-flux` says the same.

## Curated copies

With `curated.enabled` (it needs `snapshots.bucket`), the operator also keeps read-only copies of the curated objects of `platformNamespace` in every organization namespace:

- the Harnesses `spec.capabilities.agentPlatform.harnesses` names, with `substrate.workerPoolRef` set to the organization's pool, `substrate.snapshotPolicy.location` set to `s3://<bucket>/<uid>/<harness>` `kagent.memory` removed, and no compaction summarizer model (compaction uses the agent's own model);
- the WorkerPool `curated.workerPool` with the ServiceAccount its workers run as, and the placement labels `kagent.dev/worker-pool-name` and `kagent.dev/worker-pool-namespace`;
- the Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` whose references are met (see "The reference rule" below), with the template's `modelConfig` set to the organization's default ModelConfig, or removed while there is none;
- the RemoteMCPServers labelled `agent-platform.giantswarm.io/curated: "true"` that the tools of those Agents and templates name: the muster server the curated Agents reach their tools through.

**The reference rule**: a curated Agent or AgentTemplate is copied only when every object it refers to is a curated copy in the organization namespace too: its Harness must be in `spec.capabilities.agentPlatform.harnesses`, and every AgentTemplate and RemoteMCPServer it refers to must carry the curated label. Otherwise it is left out, and a copy made before goes. [docs/curated-copies.md](docs/curated-copies.md) has the full rule and why.

A copy carries its source's `kagent.dev/` labels (`kagent.dev/discovery: disabled` on the muster server), and no other label of the source.

**The worker replica rule**: the WorkerPool copy runs the warm workers that the organization uses, so an agent's first start can build its golden snapshot without a person setting a number:

| State of the organization | Warm workers |
|---|---|
| No ModelConfig (of any name) in the organization namespace, or no Agent and no session | 0 |
| A ModelConfig and one Agent or more (a curated copy counts) | `spec.capabilities.agentPlatform.workers.replicas`, 1 when it is not set |
| A ModelConfig and a session, but no Agent | `spec.capabilities.agentPlatform.workers.replicas`, 1 when it is not set |
| `AgentPlatformOrphaned` is `True` | 0 |
| Deleted | `spec.capabilities.agentPlatform.workers.replicas` until its sessions are gone, whatever else holds, then 0 (see "Organization cleanup") |

Any Agent of the organization namespace counts, not only one from git: its creation and its deletion reconcile the organization. A session keeps the workers after the last Agent goes, because kagent needs a worker to suspend a session's actor. The operator lists the sessions through kagent (`erasure.kagent.target`, as cleanup does) only while the namespace holds a ModelConfig and no Agent, and only up to the first one; kagent keeps a session of an Agent that is gone until someone deletes it, so such a session keeps the workers (`spec.capabilities.agentPlatform.workers.replicas`). Nothing tells the operator that a session went: the pool goes to 0 at the next reconcile, within the sync period (`syncPeriod`, 10 minutes by default). While the operator cannot read the Agents or the sessions, the pool runs `spec.capabilities.agentPlatform.workers.replicas` and the organization is `AgentPlatformReady=False` with the error. There is no hold: when an Agent that never had a session is the last to go, the pool goes to 0 at once, and the next Agent starts a worker again.

A copy follows its source: a change in `platformNamespace`, a move of the default mark, or a change of `spec.capabilities.agentPlatform.harnesses` re-copies it into every organization it concerns. A copy whose source is no longer curated for the organization goes. An edit of a copy is reverted. The copies carry `agent-platform.giantswarm.io/organization-object: curated`, which the admission policy that refuses edits to them selects, and `agent-platform.giantswarm.io/copied-from: <platformNamespace>/<name>`.

The operator labels what it writes with `agent-platform.giantswarm.io/organization-object` and `app.kubernetes.io/managed-by: agent-platform-operator`. It never changes or deletes an object without that label, except what cleanup deletes in a deleted organization's namespace.

<a id="erasure"></a>

## Organization cleanup

Organization deletion requests removal; organization cleanup removes the resources it leaves behind. See the [glossary](GLOSSARY.md) for the lifecycle terms. Cleanup does not promise a secure wipe of storage media or removal of retained session metadata.

Every `Organization` with the Agent Platform enabled carries the finalizer `agent-platform.giantswarm.io/erasure`, also when the operator finds the switch off. When it is deleted, the operator cleans up in this order, and removes the finalizer only after the last step:

1. **Workloads**: it deletes the organization's Flux Kustomization and waits until it is gone, then deletes the HelmReleases left in the organization namespace (all of them where there is no Kustomization) and its Agents (the curated copies, and Agents written outside git), and waits until they are gone.
2. **Sessions**: it lists every creator's sessions of the organization namespace's Agents, Agents already gone included, through the kagent controller (`erasure.kagent.target`), on `kagent.api.v1alpha1` asking for that namespace only (on `kagent.api.v1`, or on a kagent without the namespace filter, every namespace's sessions are listed and the operator keeps the organization's), and deletes each, until none is left, then waits until Substrate's ate-api (`erasure.substrate.target`) holds no actor in the organization's atespace (the namespace's name).
3. **Snapshots**: it deletes every object under `status.capabilities.agentPlatform.snapshotPrefix`, and again after `erasure.snapshotDelay` (default 5m, Substrate's default actor workflow deadline), for an upload that started before the last actor went.
4. **Namespace**: it deletes the organization namespace and waits until it is gone.
5. **OutsideObjects**: it deletes the organization's Flux ServiceAccount beside the installation's Flux source, and waits until it is gone.
6. It removes the finalizer.

While cleanup runs, `AgentPlatformErasing` is `True` with the step as its reason (`Workloads`, `Sessions`, `Snapshots`, `Namespace` or `OutsideObjects`), and `AgentPlatformReady` is `False` with reason `Deleting`; the message of both names the step and a failure. A failing step is retried with backoff; a step that waits looks again every `erasure.pollInterval`. An organization created again with the same name gets a new UID, so a new snapshot prefix and Flux objects; its namespace has the same name, created again once the old one is gone.

The operator finds the organization's Flux Kustomization and Flux ServiceAccount in the namespace of the installation's Flux source (`erasure.fluxNamespace`, the agent-platform chart's `agentPlatformOrganizations.fluxNamespace`) by the label `agent-platform.giantswarm.io/organization-uid: <uid>`, the label the agent-platform chart's admission policy holds the operator's writes there to. It looks nowhere else: the label in another namespace names nothing the operator created. Without `erasure.fluxNamespace` there are none.

The operator keeps its progress in its conditions: the step is the reason of `AgentPlatformErasing`, and the time of the first deletion of the snapshot prefix is the `lastTransitionTime` of `AgentPlatformSnapshotsErased`, which turns `True` then (to the second, rounded up); the second deletion waits `erasure.snapshotDelay` from it. The prefix it deletes is the one `status.capabilities.agentPlatform.snapshotPrefix` recorded: the `Organization` CRD must hold that field, or the API server drops it.

kagent must authorize cross-creator session deletion for a caller granted `delete` on `sessions.api.kagent.dev` cluster-wide ([giantswarm/kagent-extensions#27](https://github.com/giantswarm/kagent-extensions/issues/27)). Without it, cleanup stops at the Sessions step and reports kagent's error in the condition.

Existing interface names retain `erasure` for compatibility: Helm values under `erasure`, the finalizer, status fields and the `--snapshot-erase-delay` and `--erasure-poll-interval` flags configure or report organization cleanup.

The operator calls kagent's SessionService, `kagent.api.v1` or `kagent.api.v1alpha1` whichever the controller serves, and Substrate as its own ServiceAccount, with a projected token of each server's audience (`erasure.kagent.audience`, `erasure.substrate.audience`): kagent must admit it as a machine caller with cluster-wide `list` and `delete` on `sessions.api.kagent.dev`, and ate-api must trust the token. It verifies ate-api's serving certificate with `erasure.substrate.caClusterTrustBundle` (the ClusterTrustBundles of a signer, projected into the pod) or `erasure.substrate.caConfigMap`, reading the bundle at each new connection, so a rotated CA is trusted without a restart. It deletes snapshots with the AWS SDK's default credentials: on a management cluster, the IRSA role in `serviceAccount.annotations`. The agent-platform chart renders that role and the kagent grant.

## Development

See [docs/development.md](docs/development.md).
