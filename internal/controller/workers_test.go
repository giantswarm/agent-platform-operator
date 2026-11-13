package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-platform-operator/api/v1alpha1"
)

// TestWorkerReplicas is the worker replica rule, one case for each row of its
// table and for "not set" against "set to 0".
func TestWorkerReplicas(t *testing.T) {
	three, zero := int32(3), int32(0)
	now := metav1.Now()
	orphaned := []metav1.Condition{{Type: v1alpha1.ConditionOrphaned, Status: metav1.ConditionTrue}}
	used := usage{models: true, inUse: true}
	for _, tc := range []struct {
		name     string
		replicas *int32
		deleted  bool
		step     string
		conds    []metav1.Condition
		use      usage
		want     int64
	}{
		{name: "no ModelConfig", replicas: &three, use: usage{inUse: true}, want: 0},
		{name: "a ModelConfig, no Agent and no session", replicas: &three, use: usage{models: true}, want: 0},
		{name: "a ModelConfig and an Agent or a session", replicas: &three, use: used, want: 3},
		{name: "a ModelConfig and an Agent, replicas not set", use: used, want: 1},
		{name: "a ModelConfig and an Agent, replicas set to 0", replicas: &zero, use: used, want: 0},
		{name: "Orphaned", replicas: &three, conds: orphaned, use: used, want: 0},
		{name: "being erased, sessions left", replicas: &three, deleted: true, step: v1alpha1.ErasureSessions, want: 3},
		{name: "being erased, sessions left, replicas not set", deleted: true, step: v1alpha1.ErasureWorkloads, want: 1},
		{name: "being erased, sessions gone", replicas: &three, deleted: true, step: v1alpha1.ErasureSnapshots, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := &v1alpha1.AgentPlatformOrganization{
				Spec:   v1alpha1.AgentPlatformOrganizationSpec{Workers: v1alpha1.Workers{Replicas: tc.replicas}},
				Status: v1alpha1.AgentPlatformOrganizationStatus{ErasureStep: tc.step, Conditions: tc.conds},
			}
			if tc.deleted {
				org.DeletionTimestamp = &now
			}
			assert.Equal(t, tc.want, workerReplicas(org, tc.use))
		})
	}
}
