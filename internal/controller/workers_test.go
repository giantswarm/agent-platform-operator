package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orgv1 "github.com/giantswarm/organization-operator/api/v1alpha1"
)

// TestWorkerReplicas is the worker replica rule, one case for each row of its
// table and for "not set" against "set to 0".
func TestWorkerReplicas(t *testing.T) {
	three, zero := int32(3), int32(0)
	now := metav1.Now()
	orphaned := []metav1.Condition{{Type: orgv1.AgentPlatformOrphaned, Status: metav1.ConditionTrue}}
	// erasing is the condition of an erasure on step.
	erasing := func(step string) []metav1.Condition {
		return []metav1.Condition{{Type: orgv1.AgentPlatformErasing, Status: metav1.ConditionTrue, Reason: step}}
	}
	used := usage{models: true, inUse: true}
	for _, tc := range []struct {
		name     string
		replicas *int32
		deleted  bool
		conds    []metav1.Condition
		use      usage
		want     int64
	}{
		{name: "no ModelConfig", replicas: &three, use: usage{inUse: true}, want: 0},
		{name: "a ModelConfig, no Agent and no session", replicas: &three, use: usage{models: true}, want: 0},
		{name: "a ModelConfig and an Agent or a session", replicas: &three, use: used, want: 3},
		{name: "a ModelConfig and an Agent, replicas not set", use: used, want: 1},
		{name: "a ModelConfig and an Agent, replicas set to 0", replicas: &zero, use: used, want: 0},
		{name: "AgentPlatformOrphaned", replicas: &three, conds: orphaned, use: used, want: 0},
		{name: "being erased, sessions left", replicas: &three, deleted: true, conds: erasing(orgv1.AgentPlatformErasingSessions), want: 3},
		{name: "being erased, sessions left, replicas not set", deleted: true, conds: erasing(orgv1.AgentPlatformErasingWorkloads), want: 1},
		{name: "being erased, sessions gone", replicas: &three, deleted: true, conds: erasing(orgv1.AgentPlatformErasingSnapshots), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := &orgv1.Organization{
				Spec: orgv1.OrganizationSpec{Capabilities: orgv1.OrganizationCapabilities{AgentPlatform: &orgv1.AgentPlatformSpec{
					Enabled: true, Workers: orgv1.AgentPlatformWorkers{Replicas: tc.replicas},
				}}},
				Status: orgv1.OrganizationStatus{Conditions: tc.conds},
			}
			if tc.deleted {
				org.DeletionTimestamp = &now
			}
			assert.Equal(t, tc.want, workerReplicas(org, tc.use))
		})
	}
}
