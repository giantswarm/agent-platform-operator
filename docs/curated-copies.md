# The reference rule of the curated copies

A curated Agent or AgentTemplate is copied into an organization namespace only
when every object it refers to is a curated copy in that namespace too. When
one reference is not met, that object is not copied, and a copy made before
goes. No copy with a dangling reference exists.

## The references

| Reference | Met when |
|---|---|
| An Agent's `harnessRef` | the Harness is in the organization's `spec.harnesses` and its copy is the operator's |
| An Agent's `templateRef`, a tool's `subAgent.templateRef` | the AgentTemplate in `platformNamespace` carries `agent-platform.giantswarm.io/curated: "true"`, and its own references are met |
| A tool's `mcp.server` | the RemoteMCPServer in `platformNamespace` carries the curated label, and its own references are met |
| `systemPromptFrom`, `outputSchemaFrom`, `promptTemplate.dataSources` (ConfigMaps) | never: the operator copies no ConfigMap |
| A skill's or plugin's `credentialRef` (Secret), a RemoteMCPServer's `headersFrom` | never: the operator copies no Secret or ConfigMap |
| An Agent's `egressPolicyRefs` | never: the operator copies no EgressPolicy |

An Agent with an inline `harness` refers to no Harness: the operator rewrites
its pool and snapshot location and removes its model references, as for a
Harness copy.

## Why

The organization's Flux ServiceAccount (`agent-platform:organization-flux`)
writes Agents, AgentTemplates and RemoteMCPServers in its namespace. A dangling
reference in a read-only curated copy would bind to an object of that name that
the organization wrote, so a curated Agent would run the organization's
template or tools. A Harness outside `spec.harnesses` is the organization's
choice, not a fault: the Agents on it are left out, and the organization stays
`Ready`.

An object of the organization that holds the name of a curated copy leaves the
objects that refer to it out as well, and the organization is `Ready=False`
with reason `NameTaken`. The operator does not watch the organization's own
objects: once that object is gone, the copies come at the next reconcile,
within the sync period (`syncPeriod`).

## When things change

- The organization adds the Harness to `spec.harnesses`, or the platform labels
  the missing object: the copy is made at the next reconcile.
- A copy goes in order: the Agents first, then the AgentTemplates, then the
  RemoteMCPServers and Harnesses. A kind goes only once no copy of an earlier
  kind is left to go, so a copy never outlives an object it refers to.
- A copy that is being deleted (a finalizer holds it) is no copy to refer to:
  what refers to it is written only once it is gone and copied again.
- While the operator cannot read a curated source or write a copy (an API
  error, not "not found"), it writes no copy that depends on it and removes no
  copy of an Agent, AgentTemplate, RemoteMCPServer or Harness on that
  reconcile.

## What a person sees

The operator logs a left-out object at debug level (`curated object left out`,
with the reason) and logs each copy it deletes. It writes no status for a
left-out object: how a member sees one is giantswarm/giantswarm#39272.

ConfigMaps are not copied yet: copying one needs the operator's ClusterRole to
write ConfigMaps, which waits until the agent-platform chart's admission policy
narrows those writes to organization namespaces.
