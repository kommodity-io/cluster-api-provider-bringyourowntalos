CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3
GOLANGCI_LINT ?= go tool golangci-lint

.PHONY: generate
generate: ## Generate deepcopy methods and CRDs.
	$(CONTROLLER_GEN) object paths="./api/..."
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd
	@# CAPI contract label: CRD version v1alpha1 implements the v1beta1 contract.
	for f in config/crd/infrastructure.cluster.x-k8s.io_*.yaml; do \
		yq -i '.metadata.labels."cluster.x-k8s.io/v1beta1" = "v1alpha1"' "$$f"; \
	done

.PHONY: build
build: ## Build all packages.
	go build ./...

.PHONY: test
test: ## Run unit tests.
	go test ./...

.PHONY: release-assets
release-assets: generate ## Build the infrastructure-components.yaml release asset consumed by Kommodity.
	mkdir -p dist
	rm -f dist/infrastructure-components.yaml
	for f in config/crd/*.yaml; do \
		cat "$$f" >> dist/infrastructure-components.yaml; \
	done

.PHONY: lint
lint: ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint with auto-fix.
	$(GOLANGCI_LINT) run --fix
