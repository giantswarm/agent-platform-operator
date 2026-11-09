package adminmirror

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Case is a parent namespace's RoleBinding and what the rule makes of it.
type Case struct {
	// Binding is the parent binding, without a namespace: a test sets the
	// parent's. Its name is unique in Cases and a valid RoleBinding name.
	Binding rbacv1.RoleBinding
	// IsMirroredAdmin is what IsMirroredAdmin returns for Binding.
	IsMirroredAdmin bool
	// Mirrored are the subjects of the mirror in the organization namespace,
	// bound to OrganizationsAdminClusterRole; empty means no mirror.
	Mirrored []rbacv1.Subject
}

// Cases is the table of the rule that the operator's tests run and
// agent-manager's tests run too (giantswarm/agent-manager#335). Each call
// returns new objects.
func Cases() []Case {
	user := func(n string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: n}
	}
	group := func(n string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: n}
	}
	sa := func(ns, n string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: n}
	}
	clusterRole := func(n string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: n}
	}
	binding := func(name string, labels map[string]string, ref rbacv1.RoleRef, subjects ...rbacv1.Subject) rbacv1.RoleBinding {
		return rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, RoleRef: ref, Subjects: subjects}
	}
	rbacOperator := func() map[string]string { return map[string]string{RBACOperatorLabel: RBACOperatorValue} }

	return []Case{
		{
			// Written by an organizations-admin of the parent.
			Binding: binding("platform-team", nil, clusterRole(OrganizationsAdminClusterRole),
				user("alice@example.com"), group("acme-platform"), sa("org-acme", "automation"),
				group("system:authenticated"), user("system:serviceaccount:org-acme:automation")),
			IsMirroredAdmin: true,
			Mirrored:        []rbacv1.Subject{user("alice@example.com"), group("acme-platform")},
		},
		{
			// rbac-operator's namespaceauth: the installation's customer admin groups.
			Binding: binding("write-all-customer-group", rbacOperator(), clusterRole(ClusterAdminClusterRole),
				group("customer-admins"), group("customer-sre"), sa("default", "automation")),
			IsMirroredAdmin: true,
			Mirrored:        []rbacv1.Subject{group("customer-admins"), group("customer-sre")},
		},
		{
			// rbac-operator's copy of a per-organization admin RoleBindingTemplate.
			Binding:         binding("acme-admins", rbacOperator(), clusterRole(ClusterAdminClusterRole), user("bob@example.com")),
			IsMirroredAdmin: true,
			Mirrored:        []rbacv1.Subject{user("bob@example.com")},
		},
		{
			// rbac-operator's write-all-customer-sa: ServiceAccounts only, no mirror.
			Binding:         binding("write-all-customer-sa", rbacOperator(), clusterRole(ClusterAdminClusterRole), sa("org-acme", "automation"), sa("default", "automation")),
			IsMirroredAdmin: true,
		},
		{
			// A cluster-admin binding written by hand.
			Binding: binding("hand-written-admins", nil, clusterRole(ClusterAdminClusterRole), user("dave@example.com")),
		},
		{
			// A cluster-admin binding another tool manages.
			Binding: binding("flux-admins", map[string]string{RBACOperatorLabel: "flux"}, clusterRole(ClusterAdminClusterRole), user("erin@example.com")),
		},
		{
			// rbac-operator's binding to another role.
			Binding: binding("customer-viewers", rbacOperator(), clusterRole("view"), group("customer-viewers")),
		},
		{
			// A Role, not the ClusterRole, named cluster-admin.
			Binding: binding("role-cluster-admin", rbacOperator(),
				rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ClusterAdminClusterRole}, user("frank@example.com")),
		},
		{
			// A Role, not the ClusterRole, named organizations-admin.
			Binding: binding("role-organizations-admin", nil,
				rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: OrganizationsAdminClusterRole}, user("grace@example.com")),
		},
	}
}
