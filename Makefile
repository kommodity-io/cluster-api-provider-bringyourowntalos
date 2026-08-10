CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6

.PHONY: generate
generate: ## Generate deepcopy methods and CRDs.
	$(CONTROLLER_GEN) object paths="./api/..."
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd

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
