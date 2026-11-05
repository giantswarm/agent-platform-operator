package controller_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

func TestParseServiceAccountBindings(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []controller.ServiceAccountBinding
		wantErr string
	}{
		{name: "empty", in: ""},
		{name: "an empty list", in: "[]", want: []controller.ServiceAccountBinding{}},
		{
			name: "two bindings",
			in:   `[{"clusterRole":"k8s-credential-provider-secret-reader","serviceAccount":{"namespace":"ate-system","name":"k8s-credential-provider"}},{"clusterRole":"google-reader","serviceAccount":{"namespace":"ate-system","name":"google"}}]`,
			want: []controller.ServiceAccountBinding{
				{ClusterRole: "k8s-credential-provider-secret-reader", ServiceAccount: types.NamespacedName{Namespace: bindingNS, Name: "k8s-credential-provider"}},
				{ClusterRole: "google-reader", ServiceAccount: types.NamespacedName{Namespace: bindingNS, Name: "google"}},
			},
		},
		{
			name: "a ClusterRole name with a colon, a comma and an equals sign",
			in:   `[{"clusterRole":"substrate:reader,a=b","serviceAccount":{"namespace":"ate-system","name":"reader"}}]`,
			want: []controller.ServiceAccountBinding{{ClusterRole: "substrate:reader,a=b", ServiceAccount: types.NamespacedName{Namespace: bindingNS, Name: "reader"}}},
		},
		{name: "not JSON", in: "reader=ate-system/provider", wantErr: "not a JSON list"},
		{name: "an unknown field", in: `[{"clusterRole":"reader","serviceAccount":{"namespace":"ate-system","name":"p"},"role":"x"}]`, wantErr: `unknown field "role"`},
		{name: "data after the list", in: `[] []`, wantErr: "data after the list"},
		{name: "no ClusterRole", in: `[{"serviceAccount":{"namespace":"ate-system","name":"p"}}]`, wantErr: "binds no ClusterRole"},
		{name: "a ClusterRole that is no RBAC name", in: `[{"clusterRole":"a/b","serviceAccount":{"namespace":"ate-system","name":"p"}}]`, wantErr: `ClusterRole "a/b"`},
		{name: "an invalid namespace", in: `[{"clusterRole":"reader","serviceAccount":{"namespace":"ATE","name":"p"}}]`, wantErr: `namespace "ATE"`},
		{name: "no ServiceAccount name", in: `[{"clusterRole":"reader","serviceAccount":{"namespace":"ate-system"}}]`, wantErr: `ServiceAccount ""`},
		{
			name:    "a ClusterRole listed twice",
			in:      `[{"clusterRole":"reader","serviceAccount":{"namespace":"ate-system","name":"a"}},{"clusterRole":"reader","serviceAccount":{"namespace":"ate-system","name":"b"}}]`,
			wantErr: `ClusterRole "reader" is listed twice`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := controller.ParseServiceAccountBindings(tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
