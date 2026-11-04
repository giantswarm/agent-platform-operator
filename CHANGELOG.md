# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- The `AgentPlatformOrganization` CRD (`agent-platform.giantswarm.io/v1alpha1`, namespaced): an organization in a parent namespace gets its own namespace `agents-<name>-<5 random characters>`, labelled with the organization, the parent and the organization's UID. The CRD refuses a name longer than 50 characters.
- The organization namespace holds the platform's network policies, the agent chart's OCIRepository, the Helm ServiceAccount bound to `agent-platform-tenant-flux`, and a mirror of each `agent-platform:organizations-admin` RoleBinding of the parent (User and Group subjects only).
- The chart installs the CRD and the operator with its own RBAC: create namespaces, write RoleBindings, and `bind` only `agent-platform:organizations-admin`, `agent-platform:organization-flux` and `agent-platform-tenant-flux`.


[Unreleased]: https://github.com/giantswarm/agent-platform-operator/tree/main
