# Agent Platform organizations

The lifecycle terms shared by the organization operator, platform charts and
integration proofs.

## Language

**Organization deletion**:
An administrator's request to remove an organization, completed after its
resources have been cleaned up.
_Avoid_: Organization erasure

**Organization cleanup**:
The removal of a deleted organization's workloads, runnable sessions, actors,
snapshot objects, namespace and associated infrastructure resources.
_Avoid_: Erasure, secure wipe

**Orphaned organization**:
An organization absent from its installation's settled desired inventory.
Orphaning prevents new work; deletion requires a separate administrative action.

**Snapshot store**:
The object storage used by the runtime to persist snapshots for suspension and
resumption. Cleanup removes the deleted organization's snapshot objects from
that same store.
