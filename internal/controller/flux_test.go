package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// installations is a reader that holds the installation Kustomization or
// not.
type installations struct{ exists bool }

func (i *installations) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	if !i.exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: "kustomize.toolkit.fluxcd.io", Resource: "kustomizations"}, key.Name)
	}
	return nil
}

func (i *installations) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

// The operator may come up before Flux creates the installation
// Kustomization: it is not ready, names the flag, and becomes ready once the
// Kustomization exists.
func TestInstallationCheck(t *testing.T) {
	f := FluxConfig{
		Kustomization: schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
		Installation:  types.NamespacedName{Namespace: "flux-giantswarm", Name: "flux"},
	}
	reader := &installations{}
	check := InstallationCheck(reader, f)
	require.NotNil(t, check)
	req := httptest.NewRequest("GET", "/readyz", nil)

	err := check(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--installation-kustomization flux-giantswarm/flux")

	reader.exists = true
	assert.NoError(t, check(req))

	assert.Nil(t, InstallationCheck(reader, FluxConfig{}), "a check without an installation Kustomization")
}
