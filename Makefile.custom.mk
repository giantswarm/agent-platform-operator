##@ Development

BINARY := agent-platform-operator
CHART := helm/agent-platform-operator

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
SETUP_ENVTEST := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
ENVTEST_K8S_VERSION ?= 1.35.x

.PHONY: generate
generate: ## Regenerate the API's deepcopy code and the CRD in the chart's crds/ from api/v1alpha1.
	$(CONTROLLER_GEN) object paths=./api/...
	go run golang.org/x/tools/cmd/goimports@v0.39.0 -local github.com/giantswarm/agent-platform-operator -w api/v1alpha1/zz_generated.deepcopy.go
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=$(CHART)/crds

.PHONY: build-linux-amd64
build-linux-amd64: ## Build the linux/amd64 binary the Dockerfile expects.
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s $(LDFLAGS)" -o $(BINARY)-linux-amd64 .

.PHONY: docker-build
docker-build: build-linux-amd64 ## Build a local dev image (TAG=agent-platform-operator:dev).
	docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t $(or $(TAG),agent-platform-operator:dev) .

.PHONY: test-envtest
test-envtest: ## Run the envtest-backed tests against a real kube-apiserver (downloads one via setup-envtest; needs helm).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./... -run Envtest -count=1 -v

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART)

.PHONY: helm-template
helm-template: ## Render the chart with defaults.
	helm template agent-platform-operator $(CHART)
