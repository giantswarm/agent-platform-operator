# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- The `AgentPlatformOrganization` CRD (`agent-platform.giantswarm.io/v1alpha1`, namespaced): an organization in a parent namespace gets its own namespace `agents-<name>-<5 random characters>`, labelled with the organization, the parent and the organization's UID. The CRD refuses a name longer than 50 characters or with a dot.
- The organization namespace holds the platform's network policies, the agent chart's OCIRepository, the Helm ServiceAccount bound to `agent-platform-tenant-flux`, and a mirror of each `agent-platform:organizations-admin` RoleBinding of the parent (User and Group subjects only).
- The chart installs the CRD and the operator with its own RBAC: create namespaces, write RoleBindings, and `bind` only `agent-platform:organizations-admin` and `agent-platform-tenant-flux`.
- With `curated.enabled`, every organization namespace gets read-only copies of the platform's curated Harnesses (those `spec.harnesses` names, on the organization's own pool and snapshot prefix `s3://<bucket>/<uid>/<harness>`, kagent memory off), WorkerPool with its ServiceAccount, Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` and the RemoteMCPServers they use. An edit of a copy is reverted; a change of the source is copied into every organization, a `kagent.dev/` label it loses included, and a renamed WorkerPool ServiceAccount replaces the old one once the pool copy names it; a ServiceAccount name in the organization namespace that is not the operator's curated one (another writer's, or the Helm ServiceAccount) keeps the pool on its ServiceAccount and the organization not `Ready` (`NameTaken`), and while it does, an organization that should run no workers has no WorkerPool copy.
- The curated Agents run on the organization's default ModelConfig, the one annotated `agent-platform.giantswarm.io/is-default-model-config: "true"`; the condition `DefaultModelConfigured` is `False` while none or more than one is marked.
- An organization's WorkerPool runs `spec.workers.replicas` workers, and 0 while its namespace holds no ModelConfig or while it is `Orphaned`.
- Every organization namespace gets the ResourceQuota `agent-platform-organization-objects` on object counts (`objectQuota`).
- A deleted organization is erased, in order: its Flux Kustomization or HelmReleases and its Agents, every creator's sessions of its namespace through kagent's SessionService, `kagent.api.v1` or `kagent.api.v1alpha1` whichever the controller serves (on `kagent.api.v1alpha1` asked for that namespace only; otherwise, or on a kagent without the namespace filter, every namespace's are listed and the operator keeps the organization's) until Substrate holds no actor of it, its snapshot prefix twice (`erasure.snapshotDelay` apart), its namespace, then its Flux ServiceAccount. `Ready` stays `False` with reason `Deleting` and `status.erasureStep` names the step; a failing step is retried. The workers run until the sessions are gone and stop after. `curated.enabled` needs `erasure.kagent.target` and `erasure.substrate.target`.
- Commit mode (`commitMode.sourceRef.name`): every organization gets, in `erasure.fluxNamespace`, a Flux Kustomization `agents-<name>-<5>` that applies its path in git (`commitMode.path`) into its namespace as its own Flux ServiceAccount. That ServiceAccount is bound to `agent-platform:organization-flux` in the organization namespace only. The erasure waits until the Kustomization named after the organization namespace is gone before its namespace step, and the ServiceAccount of that name in its last step, also while one without the organization's UID label is terminating, and names the one it waits for in `Ready`.
- `erasure.substrate.caClusterTrustBundle.signerName` and `.matchLabels` verify ate-api with the ClusterTrustBundles of a signer, projected into the pod (needs the `ClusterTrustBundleProjection` feature); exclusive with `erasure.substrate.caConfigMap.name`.
- With `commitMode.installationKustomization.name`, an organization that the installation's Kustomization applied and that is missing from its inventory is `Orphaned`: reason `DeleteMerged` with `agent-platform.giantswarm.io/delete-pull-request`, `MissingFromGit` otherwise. Its workers stop; an organization back in the inventory is no longer `Orphaned`.
- `secretReaders`: each ClusterRole is bound, in every organization namespace, to its ServiceAccount of another namespace (Substrate's credential providers), by a RoleBinding named after the ClusterRole, and the operator gets `bind` on it. A reader removed from the list loses its bindings.

### Changed

- The operator reads the CA bundles of `--kagent-ca-file` and `--substrate-ca-file` at each new connection, so a rotated CA is trusted without a restart.
- **Breaking**: the default ModelConfig annotation is `agent-platform.giantswarm.io/is-default-model-config: "true"`, after Kubernetes' `ingressclass.kubernetes.io/is-default-class`; the release candidates' `agent-platform.giantswarm.io/default-model-config` no longer marks a default. Before upgrading, annotate the default ModelConfig of each organization namespace with the new key, or mark it again through an agent-manager on the new key.

[Unreleased]: https://github.com/giantswarm/agent-platform-operator/tree/main
