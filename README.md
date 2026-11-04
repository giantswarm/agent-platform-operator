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
  - `Orphaned` is declared in the API; the operator does not set it yet. The worker replica rule reads it.

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

- the ResourceQuota `agent-platform-organization-objects` on object counts (`objectQuota`), so one organization cannot flood the API server with objects.

## Curated copies

With `curated.enabled` (it needs `snapshots.bucket`), the operator also keeps read-only copies of the curated objects of `platformNamespace` in every organization namespace:

- the Harnesses `spec.harnesses` names, with `substrate.workerPoolRef` set to the organization's pool, `substrate.snapshotPolicy.location` set to `s3://<bucket>/<uid>/<harness>` `kagent.memory` removed, and no compaction summarizer model (compaction uses the agent's own model);
- the WorkerPool `curated.workerPool` with the ServiceAccount its workers run as, and the placement labels `kagent.dev/worker-pool-name` and `kagent.dev/worker-pool-namespace`;
- the Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` (and an AgentTemplate a curated Agent's `templateRef` names), with the template's `modelConfig` set to the organization's default ModelConfig, or removed while there is none;
- the RemoteMCPServers the tools of those templates name: the muster server the curated Agents reach their tools through.

A copy carries its source's `kagent.dev/` labels (`kagent.dev/discovery: disabled` on the muster server), and no other label of the source.

**The worker replica rule**: the WorkerPool copy runs `spec.workers.replicas`, except 0 replicas while the organization namespace holds no ModelConfig (of any name) and while the organization is `Orphaned`.

A copy follows its source: a change in `platformNamespace`, a move of the default mark, or a change of `spec.harnesses` re-copies it into every organization it concerns. A copy whose source is no longer curated for the organization goes. An edit of a copy is reverted. The copies carry `agent-platform.giantswarm.io/organization-object: curated`, which the admission policy that refuses edits to them selects, and `agent-platform.giantswarm.io/copied-from: <platformNamespace>/<name>`.

The operator labels what it writes with `agent-platform.giantswarm.io/organization-object` and `app.kubernetes.io/managed-by: agent-platform-operator`. It never changes or deletes an object without that label.

## Development

See [docs/development.md](docs/development.md).
