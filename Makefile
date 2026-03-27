# Makefile for CC-0001 Go Workspace
# Manages build, test, lint, and deployment operations for operators and common modules
#
# DEVIATION from architecture/01-project-setup.md (CC-0001):
# - Architecture doc lists 9 targets; this Makefile defines 12 (adds deploy-infra,
#   teardown-infra, install-test-deps) for completeness.
# - generate/manifests use controller-gen to produce deepcopy functions and CRD/webhook
#   manifests for each operator module that has an api/ directory (CC-0011).

# Default operators to build and test
OPERATORS ?= keystone c5c3

# Allow single operator override: make build OPERATOR=keystone
ifdef OPERATOR
OPERATORS := $(OPERATOR)
endif

# controller-gen generates deepcopy functions, CRD manifests, and webhook configs.
CONTROLLER_GEN ?= controller-gen

# setup-envtest downloads kubebuilder assets for envtest integration tests (CC-0018, REQ-003).
# Default resolves via GOPATH so local runs work without manually exporting GOBIN.
SETUP_ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest

# Kubernetes version for envtest binary downloads (CC-0018).
# Pin to a specific version for reproducible integration tests across runs.
ENVTEST_K8S_VERSION ?= 1.35

# Image tag for docker-build. Uses deferred evaluation so $(OPERATOR) is resolved
# at recipe expansion time (CC-0018, REQ-010).
IMG ?= ghcr.io/c5c3/$(OPERATOR)-operator:latest

# ============================================================================
# Build Targets
# ============================================================================

.PHONY: build
# common has no main package, so go build produces no binary — no -o flag needed.
# Operator modules contain main packages; -o /dev/null discards the binary since
# this target verifies compilation only (production builds use docker-build).
build:
	@echo "Building internal/common module..."
	@cd internal/common && go build ./...
	@for op in $(OPERATORS); do \
		echo "Building operators/$$op module..."; \
		(cd operators/$$op && go build -o /dev/null ./...); \
	done

# ============================================================================
# Test Targets
# ============================================================================

.PHONY: test
# Test uses workspace-relative paths (go test ./internal/common/...) instead of
# cd-ing into each module. go.work resolves these paths to the correct modules,
# and workspace-relative paths produce cleaner output with full package paths.
# Build and lint cd into each module because golangci-lint requires running from
# the module root, and build follows the same pattern for consistency.
test:
	@echo "Testing internal/common module..."
	@go test -coverprofile=cover-unit-common.out ./internal/common/...
	@for op in $(OPERATORS); do \
		echo "Testing operators/$$op module..."; \
		go test -coverprofile=cover-unit-$$op.out ./operators/$$op/... || exit 1; \
	done

.PHONY: test-common
# test-common runs unit tests for internal/common only (CC-0018).
# Used by CI to deduplicate common coverage into a single matrix leg.
test-common:
	@echo "Testing internal/common module..."
	@go test -coverprofile=cover-unit-common.out ./internal/common/...

.PHONY: test-operator
# test-operator runs unit tests for a single operator without common (CC-0018).
# Usage: make test-operator OPERATOR=keystone
test-operator:
	$(if $(OPERATOR),,$(error test-operator requires OPERATOR, e.g. make test-operator OPERATOR=keystone))
	@echo "Testing operators/$(OPERATOR) module..."
	@go test -coverprofile=cover-unit-$(OPERATOR).out ./operators/$(OPERATOR)/...

# ============================================================================
# Lint Targets
# ============================================================================

.PHONY: lint
lint:
	@echo "Linting internal/common module..."
	@cd internal/common && golangci-lint run ./...
	@for op in $(OPERATORS); do \
		echo "Linting operators/$$op module..."; \
		(cd operators/$$op && golangci-lint run ./...); \
	done

# ============================================================================
# Code Generation Targets
# ============================================================================

.PHONY: generate
# generate runs controller-gen object to produce zz_generated.deepcopy.go files
# for internal/common/types and each operator that has an api/ directory (CC-0011, REQ-009).
generate: generate-common
	@for op in $(OPERATORS); do \
		if [ -d "operators/$$op/api" ]; then \
			echo "Generating deepcopy for operators/$$op..."; \
			(cd operators/$$op && $(CONTROLLER_GEN) object:headerFile=../../hack/boilerplate.go.txt paths=./api/...); \
		fi; \
	done

.PHONY: generate-common
# generate-common runs controller-gen object for internal/common/types with the
# SPDX header (CC-0011). Separated so it runs alongside operator generation.
generate-common:
	@echo "Generating deepcopy for internal/common/types..."
	@cd internal/common && $(CONTROLLER_GEN) object:headerFile=../../hack/boilerplate.go.txt paths=./types/...

.PHONY: manifests
# manifests runs controller-gen crd and webhook to produce CRD YAML and webhook
# configuration for each operator that has an api/ directory (CC-0011, REQ-009).
# Output is written to operators/<op>/config/crd/bases/ and operators/<op>/config/webhook/.
manifests:
	@for op in $(OPERATORS); do \
		if [ -d "operators/$$op/api" ]; then \
			echo "Generating CRD and webhook manifests for operators/$$op..."; \
			mkdir -p operators/$$op/config/crd/bases operators/$$op/config/webhook; \
			(cd operators/$$op && $(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases); \
			(cd operators/$$op && $(CONTROLLER_GEN) webhook paths=./api/... output:webhook:artifacts:config=config/webhook); \
		fi; \
	done

# ============================================================================
# CRD Sync Targets (CC-0017)
# ============================================================================

# sync-crds prepends a cross-reference comment header to Helm CRD copies.
# verify-crd-sync strips comment lines (^#) before comparing, since the
# source CRDs generated by controller-gen contain no comment lines.

.PHONY: sync-crds
# sync-crds copies generated CRD manifests from config/crd/bases/ to the Helm
# chart crds/ directory, prepending a cross-reference comment header.
sync-crds: manifests
	@for op in $(OPERATORS); do \
		if [ -d "operators/$$op/helm/$$op-operator/crds" ]; then \
			echo "Syncing CRDs for operators/$$op..."; \
			for crd in operators/$$op/config/crd/bases/*.yaml; do \
				[ -f "$$crd" ] || continue; \
				dest="operators/$$op/helm/$$op-operator/crds/$$(basename $$crd)"; \
				printf '%s\n%s\n%s\n' \
					"# SOURCE: operators/$$op/config/crd/bases/$$(basename $$crd)" \
					"# This file must be kept in sync with the source CRD generated by controller-gen." \
					"# Run 'make verify-crd-sync' to check for drift, or 'make sync-crds' to update (CC-0017)." \
					> "$$dest"; \
				cat "$$crd" >> "$$dest"; \
			done; \
		fi; \
	done

.PHONY: verify-crd-sync
# verify-crd-sync checks that CRD files in helm chart crds/ directories match
# the source files in config/crd/bases/ (ignoring the cross-reference header).
verify-crd-sync:
	@fail=0; \
	tmp=$$(mktemp); \
	trap 'rm -f "$$tmp"' EXIT; \
	for op in $(OPERATORS); do \
		if [ -d "operators/$$op/helm/$$op-operator/crds" ]; then \
			for crd in operators/$$op/config/crd/bases/*.yaml; do \
				[ -f "$$crd" ] || continue; \
				helm_crd="operators/$$op/helm/$$op-operator/crds/$$(basename $$crd)"; \
				if [ ! -f "$$helm_crd" ]; then \
					echo "FAIL: $$helm_crd missing (source: $$crd)"; \
					fail=1; \
				else \
					grep -v '^#' "$$helm_crd" > "$$tmp"; \
					if ! diff -q "$$crd" "$$tmp" > /dev/null 2>&1; then \
						echo "FAIL: $$helm_crd differs from $$crd"; \
						diff -u "$$crd" "$$tmp" || true; \
						fail=1; \
					fi; \
				fi; \
			done; \
		fi; \
	done; \
	if [ "$$fail" -eq 1 ]; then \
		echo "CRD sync check failed. Run 'make sync-crds' to update."; \
		exit 1; \
	fi; \
	echo "CRD sync check passed."

# ============================================================================
# Deployment and Docker Targets
# ============================================================================

.PHONY: docker-build
# docker-build builds the operator Docker image from operators/$(OPERATOR)/Dockerfile.
# Build context is the repository root (required by go.work) (CC-0018, REQ-010).
# Usage: make docker-build OPERATOR=keystone [IMG=custom:tag]
docker-build:
	$(if $(OPERATOR),,$(error docker-build requires OPERATOR, e.g. make docker-build OPERATOR=keystone))
	docker build -t $(IMG) -f operators/$(OPERATOR)/Dockerfile .

.PHONY: helm-package
# helm-package packages the operator Helm chart (CC-0018, REQ-011).
# Usage: make helm-package OPERATOR=keystone [CHART_VERSION=1.2.3]
helm-package:
	$(if $(OPERATOR),,$(error helm-package requires OPERATOR, e.g. make helm-package OPERATOR=keystone))
	helm package operators/$(OPERATOR)/helm/$(OPERATOR)-operator/ $(if $(CHART_VERSION),--version $(CHART_VERSION))

# ============================================================================
# Testing and Infrastructure Targets
# ============================================================================

.PHONY: e2e
e2e:
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/

.PHONY: deploy-infra
deploy-infra:
	hack/deploy-infra.sh

.PHONY: teardown-infra
teardown-infra:
	hack/teardown-infra.sh

.PHONY: install-test-deps
install-test-deps:
	hack/install-test-deps.sh

.PHONY: test-integration
# test-integration runs envtest-based integration tests per operator (CC-0018, REQ-003).
# Requires setup-envtest (go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest).
# Usage: make test-integration [OPERATOR=keystone]
test-integration:
	@KUBEBUILDER_ASSETS=$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path) && export KUBEBUILDER_ASSETS && \
	echo "KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS" && \
	for op in $(OPERATORS); do \
		echo "Integration-testing operators/$$op module..."; \
		go test -tags=integration -coverprofile=cover-integration-$$op.out ./operators/$$op/... || exit 1; \
	done

.PHONY: test-integration-common
# test-integration-common runs envtest-based integration tests for internal/common (CC-0018).
# Needed to meet the codecov 80% target for internal/common/**.
# Usage: make test-integration-common
test-integration-common:
	@KUBEBUILDER_ASSETS=$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path) && export KUBEBUILDER_ASSETS && \
	echo "KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS" && \
	echo "Integration-testing internal/common module..." && \
	go test -tags=integration -coverprofile=cover-integration-common.out ./internal/common/...
