# Makefile for CC-0001 Go Workspace
# Manages build, test, lint, and deployment operations for operators and common modules
#
# DEVIATION from architecture/01-project-setup.md (CC-0001):
# - Architecture doc lists 9 targets; this Makefile defines 11 (adds deploy-infra,
#   install-test-deps) for completeness.
# - Stub targets use $(error) to fail explicitly with a feature reference (e.g.,
#   "S006: docker-build not yet implemented") rather than silently succeeding,
#   preventing false confidence that a target works.
# - generate/manifests are no-ops (echo) rather than errors because they are valid
#   targets that simply have no generators registered yet — controller-gen will be
#   added when CRD types exist (CC-0011).

# Default operators to build and test
OPERATORS ?= keystone c5c3

# Allow single operator override: make build OPERATOR=keystone
ifdef OPERATOR
OPERATORS := $(OPERATOR)
endif

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
generate:
	@echo "controller-gen not yet configured"

.PHONY: manifests
manifests:
	@echo "controller-gen not yet configured"

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
	$(error S002: e2e not yet implemented)

.PHONY: deploy-infra
deploy-infra:
	$(error S008: deploy-infra not yet implemented)

.PHONY: install-test-deps
install-test-deps:
	$(error S002: install-test-deps not yet implemented)

.PHONY: test-integration
test-integration:
	$(error S002: test-integration not yet implemented)
