package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

func TestParseObjectQuota(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    corev1.ResourceList
		wantErr string
	}{
		{name: "empty", in: ""},
		{name: "only separators", in: " , ,"},
		{
			name: "two limits with spaces around the pairs",
			in:   " configmaps=500 , count/agents.api.kagent.dev = 200,",
			want: corev1.ResourceList{
				corev1.ResourceConfigMaps:     resource.MustParse("500"),
				"count/agents.api.kagent.dev": resource.MustParse("200"),
			},
		},
		{name: "a pair without a count", in: "configmaps", wantErr: `"configmaps" is not <resource>=<count>`},
		{name: "a count that is not a quantity", in: "configmaps=many", wantErr: "configmaps: quantities must match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := controller.ParseObjectQuota(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, equality.Semantic.DeepEqual(tc.want, got), "got %v", got)
		})
	}
}
