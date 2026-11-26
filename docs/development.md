# Developing on agent-platform-operator

- `make test` runs the unit tests. The envtest-backed tests skip without `KUBEBUILDER_ASSETS`.
- `make test-envtest` runs the envtest-backed tests against a real kube-apiserver. It downloads the binaries with setup-envtest and needs `helm`: the tests install the `Organization` CRD of the organization-operator module in `go.mod`, apply the chart's RBAC and run the reconciler as the chart's ServiceAccount.
- `make helm-lint` lints the chart.
