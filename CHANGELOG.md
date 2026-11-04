# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- The `AgentPlatformOrganization` CRD (`agent-platform.giantswarm.io/v1alpha1`, namespaced): an organization in a parent namespace gets its own namespace `agents-<name>-<5 random characters>`, labelled with the organization, the parent and the organization's UID. The CRD refuses a name longer than 50 characters or with a dot.
- The organization namespace holds the platform's network policies, the agent chart's OCIRepository, the Helm ServiceAccount bound to `agent-platform-tenant-flux`, and a mirror of each `agent-platform:organizations-admin` RoleBinding of the parent (User and Group subjects only).
- The chart installs the CRD and the operator with its own RBAC: create namespaces, write RoleBindings, and `bind` only `agent-platform:organizations-admin` and `agent-platform-tenant-flux`.
- With `curated.enabled`, every organization namespace gets read-only copies of the platform's curated Harnesses (those `spec.harnesses` names, on the organization's own pool and snapshot prefix `s3://<bucket>/<uid>/<harness>`, kagent memory off), WorkerPool with its ServiceAccount, Agents and AgentTemplates labelled `agent-platform.giantswarm.io/curated: "true"` and the RemoteMCPServers they use. An edit of a copy is reverted; a change of the source is copied into every organization.
- The curated Agents run on the organization's default ModelConfig, the one annotated `agent-platform.giantswarm.io/default-model-config: "true"`; the condition `DefaultModelConfigured` is `False` while none or more than one is marked.
- An organization's WorkerPool runs `spec.workers.replicas` workers, and 0 while its namespace holds no ModelConfig or while it is `Orphaned`.
- Every organization namespace gets the ResourceQuota `agent-platform-organization-objects` on object counts (`objectQuota`).
- A deleted organization is erased, in order: its Flux Kustomization or HelmReleases and its Agents, every creator's sessions through kagent until Substrate holds no actor of it, its snapshot prefix twice (`erasure.snapshotDelay` apart), its namespace, then its Flux ServiceAccount. `Ready` stays `False` with reason `Deleting` and `status.erasureStep` names the step; a failing step is retried. The workers run until the sessions are gone and stop after. `curated.enabled` needs `erasure.kagent.target` and `erasure.substrate.target`.


[Unreleased]: https://github.com/giantswarm/agent-platform-operator/tree/main
