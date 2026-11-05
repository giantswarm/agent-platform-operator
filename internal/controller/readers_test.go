package controller_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agent-platform-operator/internal/controller"
)

func TestParseSecretReaders(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []controller.SecretReader
		wantErr string
	}{
		{name: "empty", in: ""},
		{
			name: "two readers, spaces and empty entries ignored",
			in:   " k8s-credential-provider-secret-reader = ate-system/k8s-credential-provider ,, google-reader=ate-system/google ",
			want: []controller.SecretReader{
				{ClusterRole: "k8s-credential-provider-secret-reader", ServiceAccount: types.NamespacedName{Namespace: readerNS, Name: "k8s-credential-provider"}},
				{ClusterRole: "google-reader", ServiceAccount: types.NamespacedName{Namespace: readerNS, Name: "google"}},
			},
		},
		{
			name: "a ClusterRole name with a colon",
			in:   "substrate:reader=ate-system/reader",
			want: []controller.SecretReader{{ClusterRole: "substrate:reader", ServiceAccount: types.NamespacedName{Namespace: readerNS, Name: "reader"}}},
		},
		{name: "no ServiceAccount", in: "reader", wantErr: "is not <clusterrole>=<namespace>/<serviceaccount>"},
		{name: "no namespace", in: "reader=provider", wantErr: "is not <clusterrole>=<namespace>/<serviceaccount>"},
		{name: "no ClusterRole", in: "=ate-system/provider", wantErr: "names no ClusterRole"},
		{name: "a ClusterRole that is no RBAC name", in: "a/b=ate-system/provider", wantErr: `ClusterRole "a/b"`},
		{name: "an invalid namespace", in: "reader=ATE/provider", wantErr: `namespace "ATE"`},
		{name: "an empty ServiceAccount", in: "reader=ate-system/", wantErr: `ServiceAccount ""`},
		{name: "a ClusterRole listed twice", in: "reader=ate-system/a,reader=ate-system/b", wantErr: `ClusterRole "reader" is listed twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := controller.ParseSecretReaders(tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
