# Makefile for CC-0001 Go Workspace
# Manages build, test, lint, and deployment operations for operators and common modules
#
# DEVIATION from architecture/01-project-setup.md (CC-0001):
# - Architecture doc lists 9 targets; this Makefile defines 12 (adds deploy-infra,
#   teardown-infra, install-test-deps) for completeness.
# - Stub targets use $(error) to fail explicitly with a feature reference (e.g.,
#   "S006: docker-build not yet implemented") rather than silently succeeding,
#   preventing false confidence that a target works.
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
	@go test ./internal/common/...
	@for op in $(OPERATORS); do \
		echo "Testing operators/$$op module..."; \
		go test ./operators/$$op/...; \
	done

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
# Deployment and Docker Targets
# ============================================================================

.PHONY: docker-build
docker-build:
	$(error S006: docker-build not yet implemented)

.PHONY: helm-package
helm-package:
	$(error S017: helm-package not yet implemented)

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
# TODO: Replace this stub when envtest CI infrastructure is added.
# The CI workflow omits the test-integration job; re-add it when this target runs real tests.
test-integration:
	$(error test-integration not yet implemented — envtest infrastructure pending)
