# Developing on agent-platform-operator

- `make generate` regenerates the API's deepcopy code and the CRD in `helm/agent-platform-operator/crds/` from `api/v1alpha1`. Run it after a change to the API types and commit the result.
- `make test` runs the unit tests. The envtest-backed tests skip without `KUBEBUILDER_ASSETS`.
- `make test-envtest` runs the envtest-backed tests against a real kube-apiserver. It downloads the binaries with setup-envtest and needs `helm`: the tests install the chart's CRD, apply the chart's RBAC and run the reconciler as the chart's ServiceAccount.
- `make helm-lint` lints the chart.
