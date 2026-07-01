# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Makefile for Go Workspace
# Manages build, test, lint, and deployment operations for operators and common modules
#
# DEVIATION from architecture/01-project-setup.md
# - Architecture doc lists 9 targets; this Makefile defines 12 (adds deploy-infra,
#   teardown-infra, install-test-deps) for completeness.
# - generate/manifests use controller-gen to produce deepcopy functions and CRD/webhook
#   manifests for each operator module that has an api/ directory.

# Default operators to build and test
OPERATORS ?= keystone c5c3

# Allow single operator override: make build OPERATOR=keystone
ifdef OPERATOR
OPERATORS := $(OPERATOR)
endif

# controller-gen generates deepcopy functions, CRD manifests, and webhook configs.
CONTROLLER_GEN ?= controller-gen

# setup-envtest downloads kubebuilder assets for envtest integration tests.
# Default resolves via GOPATH so local runs work without manually exporting GOBIN.
SETUP_ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest

# Local tool binaries are installed under bin/ (gitignored) so local dev pins
# the same versions as CI without depending on whatever sits on $PATH.
LOCALBIN ?= $(CURDIR)/bin

# Pin gofumpt version to match CI (single source of truth for local dev).
# Must be kept in sync with GOFUMPT_VERSION in .github/workflows/ci.yaml.
# GOFUMPT resolves to the bin/ copy managed by install-gofumpt at the pinned
# version, so a stale $PATH gofumpt can never silently format differently.
GOFUMPT_VERSION ?= v0.10.0
GOFUMPT ?= $(LOCALBIN)/gofumpt

# Kubernetes version for envtest binary downloads.
# Pin to a specific version for reproducible integration tests across runs.
ENVTEST_K8S_VERSION ?= 1.35

# Image tag for docker-build. Uses deferred evaluation so $(OPERATOR) is resolved
# at recipe expansion time.
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
# test-common runs unit tests for internal/common only.
# Used by CI to deduplicate common coverage into a single matrix leg.
test-common:
	@echo "Testing internal/common module..."
	@go test -coverprofile=cover-unit-common.out ./internal/common/...

.PHONY: test-operator
# test-operator runs unit tests for a single operator without common.
# Usage: make test-operator OPERATOR=keystone
test-operator:
	$(if $(OPERATOR),,$(error test-operator requires OPERATOR, e.g. make test-operator OPERATOR=keystone))
	@echo "Testing operators/$(OPERATOR) module..."
	@go test -coverprofile=cover-unit-$(OPERATOR).out ./operators/$(OPERATOR)/...

.PHONY: test-race
# test-race runs all Go tests with the race detector enabled.
# Operator code is inherently concurrent — reconcilers, watches, and informer
# caches operate across goroutines — so race detection catches data corruption
# that normal tests miss. CI passes RACE_FLAGS="-count=1" to disable test
# caching (race conditions are non-deterministic, so cached results mask races).
# OPERATOR works via the global override at lines 13–16.
# Usage: make test-race [OPERATOR=keystone] [RACE_FLAGS="-count=1"]
RACE_FLAGS ?=
test-race:
	@echo "Race-testing internal/common module..."
	@go test -race $(RACE_FLAGS) ./internal/common/...
	@for op in $(OPERATORS); do \
		echo "Race-testing operators/$$op module..."; \
		go test -race $(RACE_FLAGS) ./operators/$$op/... || exit 1; \
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
# Security Targets
# ============================================================================

.PHONY: govulncheck
# govulncheck scans all workspace modules for known Go vulnerabilities.
# Delegates to hack/ci-govulncheck.sh, which runs govulncheck per module
# (matching go.work use directives) and applies a documented allowlist for
# advisories with no fix and no real exposure. CI calls this target instead of
# hardcoding module paths.
govulncheck:
	@hack/ci-govulncheck.sh internal/common $(addprefix operators/,$(OPERATORS))

# ============================================================================
# Shell Script Targets
# ============================================================================

.PHONY: shellcheck
# shellcheck lints all shell scripts with shellcheck --severity=warning.
# Covers hack/ utility scripts and operator rotation scripts embedded via ConfigMaps.
shellcheck:
	@echo "Linting hack/*.sh..."
	@shellcheck --severity=warning hack/*.sh
	@echo "Linting operator rotation scripts..."
	@shellcheck --severity=warning operators/*/internal/controller/scripts/*.sh

.PHONY: chainsaw-lint
# chainsaw-lint validates every Chainsaw test (tests/**/chainsaw-test.yaml) and
# Chainsaw configuration (tests/**/chainsaw-config.yaml) against the Chainsaw
# schema. Catches structural issues (typos, removed fields, schema drift after
# a chainsaw version bump) before the heavy cluster-bound e2e-operator and
# e2e-chaos jobs run. No cluster required — chainsaw must be on PATH (install
# via `make install-test-deps`). xargs propagates any per-file failure as a
# non-zero exit (123) so make fails the target as a whole; -print0/-0 keeps
# paths with spaces safe; -r/--no-run-if-empty turns an empty find result
# into a clean no-op instead of a `flag needs an argument` error if the
# tests/ tree is ever restructured (GNU xargs only, matches the Linux-only
# CI runner).
chainsaw-lint:
	@command -v chainsaw >/dev/null 2>&1 || { echo 'chainsaw is not installed; run `make install-test-deps` first' >&2; exit 1; }
	@echo "Linting Chainsaw test definitions..."
	@find tests -type f -name 'chainsaw-test.yaml' -print0 | xargs -0 -r -n1 chainsaw lint test -f
	@echo "Linting Chainsaw configurations..."
	@find tests -type f -name 'chainsaw-config.yaml' -print0 | xargs -0 -r -n1 chainsaw lint configuration -f

.PHONY: test-shell
# test-shell runs every shell-script unit test under tests/unit/.
# Each test is a self-contained bash script that uses tests/lib/assertions.sh.
test-shell:
	@echo "Running shell unit tests..."
	@status=0; \
	for t in tests/unit/hack/*_test.sh tests/unit/deploy/*_test.sh tests/unit/renovate/*_test.sh tests/unit/docs/*_test.sh; do \
		[ -f "$$t" ] || continue; \
		echo "=== $$t ==="; \
		bash "$$t" || status=1; \
	done; \
	exit $$status

# ============================================================================
# Format Targets
# ============================================================================

.PHONY: fmt
# fmt formats all tracked Go files with gofumpt.
# Only formats git-tracked files to skip generated, vendored, or tooling code.
fmt: install-gofumpt
	@echo "Formatting Go files with gofumpt..."
	@git ls-files '*.go' | xargs $(GOFUMPT) -w

.PHONY: format-check
# format-check verifies all tracked Go files conform to gofumpt formatting.
# Mirrors the CI format-check job for local pre-commit validation.
format-check: install-gofumpt
	@unformatted=$$(git ls-files '*.go' | xargs $(GOFUMPT) -l); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not formatted with gofumpt:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi; \
	echo "Format check passed."

.PHONY: install-gofumpt
# install-gofumpt installs gofumpt at the pinned version into bin/.
# Re-installs when the binary is missing or its version drifts from the pin so
# fmt/format-check always run the exact version the CI format-check job uses.
install-gofumpt:
	@if ! test -x '$(GOFUMPT)' || ! '$(GOFUMPT)' --version | grep -qF '$(GOFUMPT_VERSION)'; then \
		echo "Installing gofumpt $(GOFUMPT_VERSION) into $(LOCALBIN)..."; \
		GOBIN='$(LOCALBIN)' go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION); \
	fi

# ============================================================================
# Code Generation Targets
# ============================================================================

.PHONY: generate
# generate runs controller-gen object to produce zz_generated.deepcopy.go files
# for internal/common/types and each operator that has an api/ directory.
generate: generate-common
	@for op in $(OPERATORS); do \
		if [ -d "operators/$$op/api" ]; then \
			echo "Generating deepcopy for operators/$$op..."; \
			(cd operators/$$op && $(CONTROLLER_GEN) object:headerFile=../../hack/boilerplate.go.txt paths=./api/...); \
		fi; \
	done

.PHONY: generate-common
# generate-common runs controller-gen object for internal/common/types with the
# SPDX header. Separated so it runs alongside operator generation.
generate-common:
	@echo "Generating deepcopy for internal/common/types..."
	@cd internal/common && $(CONTROLLER_GEN) object:headerFile=../../hack/boilerplate.go.txt paths=./types/...

.PHONY: manifests
# manifests runs controller-gen crd and webhook to produce CRD YAML and webhook
# configuration for each operator that has an api/ directory.
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
# CRD Sync Targets
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
					"# Run 'make verify-crd-sync' to check for drift, or 'make sync-crds' to update." \
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

.PHONY: gen-helm-schema
# gen-helm-schema regenerates both operator charts' values.schema.json from the
# single shared source in hack/gen-helm-values-schema.py. Edit the shared schema
# there, then run this target so keystone-operator and c5c3-operator stay in sync.
gen-helm-schema:
	python3 hack/gen-helm-values-schema.py

.PHONY: verify-helm-schema
# verify-helm-schema fails if either committed values.schema.json has drifted
# from the shared generator source (run in CI; mirrors verify-crd-sync).
verify-helm-schema:
	python3 hack/gen-helm-values-schema.py --check

# ============================================================================
# Deployment and Docker Targets
# ============================================================================

.PHONY: docker-build
# docker-build builds the operator Docker image from operators/$(OPERATOR)/Dockerfile.
# Build context is the repository root (required by go.work).
# Usage: make docker-build OPERATOR=keystone [IMG=custom:tag]
# Optional: DOCKER_CACHE_FROM=type=gha,scope=... DOCKER_CACHE_TO=type=gha,mode=max,scope=...
DOCKER_CACHE_FROM ?=
DOCKER_CACHE_TO ?=
docker-build:
	$(if $(OPERATOR),,$(error docker-build requires OPERATOR, e.g. make docker-build OPERATOR=keystone))
	docker build -t $(IMG) -f operators/$(OPERATOR)/Dockerfile \
		$(if $(DOCKER_CACHE_FROM),--cache-from $(DOCKER_CACHE_FROM)) \
		$(if $(DOCKER_CACHE_TO),--cache-to $(DOCKER_CACHE_TO)) \
		.

.PHONY: helm-deps
# helm-deps vendors the operator-library library subchart into each operator
# chart's charts/ directory (helm dependency build reads the committed
# Chart.lock). Run before helm lint/template/unittest locally; CI helm-validate
# runs it too. --skip-refresh: the dependency is local, so no chart-repository
# refresh is needed (and it avoids failing on a stale repo cache).
helm-deps:
	@for op in $(OPERATORS); do \
		echo "Building Helm dependencies for operators/$$op..."; \
		helm dependency build --skip-refresh operators/$$op/helm/$$op-operator/; \
	done

.PHONY: helm-package
# helm-package packages the operator Helm chart.
# Usage: make helm-package OPERATOR=keystone [CHART_VERSION=1.2.3]
# Vendors the operator-library subchart first so the packaged tarball is
# self-contained (helm package fails on an unresolved dependency).
helm-package:
	$(if $(OPERATOR),,$(error helm-package requires OPERATOR, e.g. make helm-package OPERATOR=keystone))
	helm dependency build --skip-refresh operators/$(OPERATOR)/helm/$(OPERATOR)-operator/
	helm package operators/$(OPERATOR)/helm/$(OPERATOR)-operator/ $(if $(CHART_VERSION),--version $(CHART_VERSION))

# ============================================================================
# Testing and Infrastructure Targets
# ============================================================================

.PHONY: verify-invalid-cr-fixtures
# verify-invalid-cr-fixtures asserts that the invalid-CR fixtures stay
# in lockstep with their canonical scaffold and with chainsaw-test.yaml. It
# runs `_generate.py --check` (drift mode: zero exit only when every on-disk
# fixture matches the scaffold) and the test_generate.py unit suite (asserts
# FIXTURES count, uniqueness, and that every Fixture.filename is referenced
# by chainsaw-test.yaml). Both checks are sub-second and require no cluster.
verify-invalid-cr-fixtures:
	@echo "Checking invalid-CR fixture drift..."
	@python3 tests/e2e/keystone/invalid-cr/_generate.py --check
	@echo "Running invalid-CR fixture unit tests..."
	@python3 tests/e2e/keystone/invalid-cr/test_generate.py

.PHONY: check-feature-ids
# check-feature-ids fails if any internal feature / requirement ID (CC-NNNN or
# REQ-NNN) appears anywhere in the tracked source tree — code, tests, CI,
# scripts, the Makefile, and the published docs. Source describes behaviour,
# not internal tracking IDs. Mirrors the always-on CI gate and needs no cluster
# or toolchain beyond git and grep.
check-feature-ids:
	@bash scripts/check-no-feature-ids.sh

.PHONY: e2e
# chainsaw auto-discovers chainsaw-test.yaml recursively, so new suites
# under tests/e2e/**/ (e.g. keystone/gateway-quick-start, infrastructure/gateway-quick-start-smoke)
# are picked up without Makefile changes.
e2e:
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/

.PHONY: e2e-chaos
# e2e-chaos runs Chaos Mesh pod-kill E2E tests against a deployed kind cluster.
# Chaos Mesh is opt-in in the kind Quick Start; fail fast with a
# clear remediation hint when the namespace is missing instead of letting chainsaw
# attempt the suite against a cluster that lacks the dependency. The two preflights
# are kept separate so the kubectl/cluster-reachability failure is not conflated with
# the chaos-mesh-not-installed failure — see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
e2e-chaos:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get ns chaos-mesh >/dev/null 2>&1 || { echo 'chaos-mesh is not installed; run `WITH_CHAOS_MESH=true make deploy-infra` first' >&2; exit 1; }
	chainsaw test --config tests/e2e-chaos/chainsaw-config.yaml tests/e2e-chaos/

.PHONY: e2e-prometheus
# e2e-prometheus runs the kube-prometheus-stack chainsaw suite against a deployed
# kind cluster. The Prometheus + Grafana stack is opt-in in the
# kind Quick Start; fail fast with a clear remediation hint when
# the monitoring namespace is missing instead of letting chainsaw attempt the suite
# against a cluster that lacks the dependency. The two preflights are kept separate
# so the kubectl/cluster-reachability failure is not conflated with the
# prometheus-not-installed failure — see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This Makefile target also satisfies the CI-to-Makefile parity expected
# by .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-prometheus CI job locally without reading YAML.
e2e-prometheus:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get ns monitoring >/dev/null 2>&1 || { echo 'kube-prometheus-stack is not installed; run `WITH_PROMETHEUS=true make deploy-infra` first' >&2; exit 1; }
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/prometheus-stack/

.PHONY: e2e-controlplane
# e2e-controlplane runs the full ControlPlane -> Keystone chain Chainsaw suite
# against a deployed kind cluster. The full stack (c5c3-operator + K-ORC +
# keystone-operator, provisioning MariaDB/Memcached in managed mode) is opt-in;
# fail fast with a clear remediation hint when the ControlPlane CRD is missing
# instead of letting chainsaw attempt the suite against a cluster that lacks it.
# The two preflights are kept separate so the kubectl/cluster-reachability
# failure is not conflated with the stack-not-installed failure — see review
# pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This Makefile target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-controlplane CI job locally. Set
# E2E_REQUIRE_CONTROLPLANE_STACK=true to make the suite's presence guard fail
# loudly (as the CI job does) rather than SKIP.
e2e-controlplane:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `WITH_CONTROLPLANE=true make deploy-infra` (and deploy K-ORC + the operators) first' >&2; exit 1; }
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/c5c3/full-controlplane-keystone/

.PHONY: tempest-test
# tempest-test runs Tempest API tests against a deployed OpenStack service.
# Requires a running kind cluster with the service deployed.
# Usage: make tempest-test SERVICE=keystone
tempest-test:
	$(if $(SERVICE),,$(error tempest-test requires SERVICE, e.g. make tempest-test SERVICE=keystone))
	SERVICE=$(SERVICE) hack/run-tempest.sh

.PHONY: stage-prometheus-dashboard
# stage-prometheus-dashboard copies the canonical Keystone Operator Grafana
# dashboard JSON into deploy/kind/prometheus/ so that local kustomize flows
# (kustomize build, kubectl apply -k, chainsaw lint) can render the
# configMapGenerator without a parent-dir traversal.
#
# The destination file is git-ignored — the canonical source is
# operators/keystone/dashboards/keystone-operator.json. `make deploy-infra`
# (with WITH_PROMETHEUS=true) performs the same copy automatically; this
# target exists for developers who want to validate the overlay without
# running the full deploy script. See
# docs/reference/infrastructure/infrastructure-manifests.md for the
# overlay's staging contract.
stage-prometheus-dashboard:
	cp -f operators/keystone/dashboards/keystone-operator.json deploy/kind/prometheus/keystone-operator.json
	@echo "Staged dashboard JSON into deploy/kind/prometheus/."

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
# test-integration runs envtest-based integration tests per operator.
# Requires setup-envtest (go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest).
# Usage: make test-integration [OPERATOR=keystone]
test-integration:
	@KUBEBUILDER_ASSETS=$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path) && export KUBEBUILDER_ASSETS && \
	echo "KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS" && \
	for op in $(OPERATORS); do \
		echo "Integration-testing operators/$$op module..."; \
		go test -tags=integration -timeout=20m -coverprofile=cover-integration-$$op.out ./operators/$$op/... || exit 1; \
	done

.PHONY: test-integration-common
# test-integration-common runs envtest-based integration tests for internal/common.
# Needed to meet the codecov 80% target for internal/common/**.
# Usage: make test-integration-common
test-integration-common:
	@KUBEBUILDER_ASSETS=$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path) && export KUBEBUILDER_ASSETS && \
	echo "KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS" && \
	echo "Integration-testing internal/common module..." && \
	go test -tags=integration -timeout=20m -coverprofile=cover-integration-common.out ./internal/common/...
