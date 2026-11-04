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
- `spec.workers.replicas`: the warm workers of the organization's WorkerPool.
- The annotation `agent-platform.giantswarm.io/display-name` holds the name people see. It is not a spec field.
- The name is at most 50 characters and holds no dot, because the organization namespace name `agents-<name>-…` must be a DNS label. The CRD refuses other names.

The status holds:

- `namespace`: the organization namespace.
- `snapshotPrefix`: `s3://<bucket>/<uid>`, where the organization's worker snapshots live. It is empty when the operator has no bucket (`snapshots.bucket`).
- `conditions`, each with `observedGeneration`. `Ready` is `True` when the namespace exists and holds every object below. It is `False` with reason `SourceMissing` while a network policy to copy is missing from `platformNamespace`, `NamespaceTerminating` while the organization namespace is terminating, `NameTaken` while a RoleBinding the operator did not write holds the name of a binding it keeps (a mirror keeps the parent binding's name), and `ReconcileFailed` when a write fails. `DefaultModelConfigured` and `Orphaned` are declared in the API; the operator does not set them yet.

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

The operator labels what it writes with `agent-platform.giantswarm.io/organization-object` and `app.kubernetes.io/managed-by: agent-platform-operator`. It never changes or deletes an object without that label.

## Development

See [docs/development.md](docs/development.md).
