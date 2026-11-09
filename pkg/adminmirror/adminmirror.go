// Package adminmirror is the rule by which the operator mirrors the admins of a
// parent namespace into its organization namespaces: which parent RoleBindings
// are mirrored (IsMirroredAdmin) and which of their subjects the mirror binds
// (MirroredSubjects). A mirror is bound to the ClusterRole
// agent-platform:organizations-admin, never to the parent binding's role.
//
// The rule is a contract between this operator and agent-manager:
// create_organization's pre-write check (giantswarm/agent-manager#335)
// predicts who the operator will make an admin of a new organization, and it
// must use these functions and nothing else, or it refuses callers the
// operator would authorize. Cases is the table both repositories' tests run.
//
// The package imports only the Kubernetes API types, so a consumer pulls in
// no controller code.
package adminmirror

import (
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	// OrganizationsAdminClusterRole is the ClusterRole of every mirror, and
	// a parent binding to it is mirrored.
	OrganizationsAdminClusterRole = "agent-platform:organizations-admin"
	// ClusterAdminClusterRole is the role of rbac-operator's admin bindings in
	// a Giant Swarm Organization's namespace.
	ClusterAdminClusterRole = "cluster-admin"
	// RBACOperatorLabel, with the value RBACOperatorValue, is on every
	// RoleBinding rbac-operator writes (k8smetadata label.ManagedBy).
	RBACOperatorLabel = "giantswarm.io/managed-by"
	RBACOperatorValue = "rbac-operator"
)

// IsMirroredAdmin reports whether a parent namespace's RoleBinding makes its
// subjects admins of the parent's organizations: a binding to the ClusterRole
// agent-platform:organizations-admin, or a binding to the ClusterRole
// cluster-admin that rbac-operator wrote (RBACOperatorLabel). In a Giant Swarm
// Organization's namespace rbac-operator writes the cluster-admin binding of
// the installation's customer admin groups (write-all-customer-group) and a
// copy of every RoleBindingTemplate that selects the organization; a
// cluster-admin binding written by hand is not mirrored.
func IsMirroredAdmin(rb *rbacv1.RoleBinding) bool {
	ref := rb.RoleRef
	if ref.APIGroup != rbacv1.GroupName || ref.Kind != "ClusterRole" {
		return false
	}
	return ref.Name == OrganizationsAdminClusterRole ||
		(ref.Name == ClusterAdminClusterRole && rb.Labels[RBACOperatorLabel] == RBACOperatorValue)
}

// MirroredSubjects are the subjects a mirror binds: the Users and Groups whose
// names do not start with system:, in their order. ServiceAccounts are never
// mirrored, so a parent's automation cannot bind itself into every
// organization, and system: names are the API server's own identities. No
// subject left means no mirror.
func MirroredSubjects(in []rbacv1.Subject) []rbacv1.Subject {
	var out []rbacv1.Subject
	for _, s := range in {
		if (s.Kind != rbacv1.UserKind && s.Kind != rbacv1.GroupKind) || strings.HasPrefix(s.Name, "system:") {
			continue
		}
		out = append(out, rbacv1.Subject{Kind: s.Kind, APIGroup: rbacv1.GroupName, Name: s.Name})
	}
	return out
}
