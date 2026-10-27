# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Makefile for Go Workspace
# Manages build, test, lint, and deployment operations for operators and common modules
#
# generate/manifests use controller-gen to produce deepcopy functions and CRD/webhook/RBAC
# manifests for each operator module that has an api/ directory.

# Default operators to build and test
OPERATORS ?= keystone c5c3 horizon glance placement barbican ovn neutron cinder nova

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
GOFUMPT_VERSION ?= v0.12.0
GOFUMPT ?= $(LOCALBIN)/gofumpt

# Kubernetes version for envtest binary downloads.
# Pin to a specific version for reproducible integration tests across runs.
ENVTEST_K8S_VERSION ?= 1.37

# Image tag for image. Uses deferred evaluation so $(OPERATOR) is resolved
# at recipe expansion time.
IMG ?= ghcr.io/c5c3/$(OPERATOR)-operator:latest

# ============================================================================
# Build Targets
# ============================================================================

.PHONY: build
# common has no main package, so go build produces no binary — no -o flag needed.
# Operator modules contain main packages; -o /dev/null discards the binary since
# this target verifies compilation only (production builds use make image).
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
# Fuzz Targets
# ============================================================================

# fuzz runs every Go fuzz target (a func Fuzz* in a *_fuzz_test.go file under
# internal/ or operators/) for FUZZ_TIME each. Without -fuzz the same targets
# run in seed-only mode under test, test-common, test-operator and test-race;
# this target is the long local session. go test accepts one -fuzz target and
# one package per invocation, so the loop runs them one at a time and stops at
# the first failure. A crasher is written to <package>/testdata/fuzz/<Fuzz
# name>/ by go test; fix the cause and commit that file as a regression seed.
# Finding no fuzz target, or a directory grep cannot search, fails the target.
# So does a target go test -list does not print: go test -fuzz exits 0 without
# fuzzing when a build constraint excludes the file or no target matches. The
# target then prints the go test -list output, which carries a compile error or
# a panic in TestMain. Like go test, the recipe takes a Fuzz name followed by a
# lower-case letter (Fuzzy, FuzzerSeeds) for a helper, not a target.
# Usage: make fuzz [FUZZ_TIME=10m]
FUZZ_TIME ?= 30s
.PHONY: fuzz
fuzz:
	@re='^func \(Fuzz\([A-Z0-9_][A-Za-z0-9_]*\)\{0,1\}\)('; \
	files="$$(grep -rl --include='*_fuzz_test.go' "$$re" internal operators)"; \
	if [ $$? -gt 1 ]; then echo 'fuzz: grep could not search internal/ and operators/' >&2; exit 1; fi; \
	[ -n "$$files" ] || { echo 'fuzz: no func Fuzz* found in a *_fuzz_test.go file under internal/ or operators/' >&2; exit 1; }; \
	for file in $$(printf '%s\n' "$$files" | sort); do \
		dir=$$(dirname "$$file"); \
		for fn in $$(sed -n "s/$$re.*/\1/p" "$$file"); do \
			listed=$$(go test -list="^$$fn\$$" "./$$dir" 2>&1); \
			printf '%s\n' "$$listed" | grep -qx "$$fn" || { printf '%s\n' "$$listed" >&2; echo "fuzz: go test does not list $$fn in $$dir (a build constraint excludes it, it does not compile, or its test binary failed)" >&2; exit 1; }; \
			echo "Fuzzing $$fn in $$dir for $(FUZZ_TIME)..."; \
			go test -run='^$$' -fuzz="^$$fn\$$" -fuzztime=$(FUZZ_TIME) "./$$dir" || exit 1; \
		done; \
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
# Covers hack/ utility scripts, operator rotation scripts embedded via ConfigMaps,
# the chaos workload harness (tests/e2e-chaos/lib/ and the probe-lib.sh a suite
# sources), the library of the lab failover suites (tests/lab-failover/lib/)
# and the patch checks the hvo and kna manager builds run
# (images/*/patch-checks.sh).
shellcheck:
	@echo "Linting hack/*.sh..."
	@shellcheck --severity=warning hack/*.sh
	@echo "Linting operator rotation scripts..."
	@shellcheck --severity=warning operators/*/internal/controller/scripts/*.sh
	@echo "Linting the chaos workload harness and probe libraries..."
	@shellcheck --severity=warning tests/e2e-chaos/lib/*.sh tests/e2e-chaos/*/probe-lib.sh
	@echo "Linting the lab failover library..."
	@shellcheck --severity=warning tests/lab-failover/lib/*.sh
	@echo "Linting the shared e2e suite scripts..."
	@shellcheck --severity=warning tests/e2e/lib/*.sh
	@echo "Linting the image patch checks..."
	@shellcheck --severity=warning images/*/patch-checks.sh

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
	@broken="$$(find -L tests/unit -type l)" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }; \
	broken="$$(printf '%s\n' "$$broken" | LC_ALL=C sort)"; \
	if [ -n "$$broken" ]; then \
		echo "$$broken" | sed 's|^|test-shell: |; s|$$| is a symlink whose target does not exist, so nothing behind it would run|' >&2; \
		exit 1; \
	fi; \
	stray="$$(find -L tests/unit ! -type d -name '*.sh' ! -name '*_test.sh')" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }; \
	stray="$$(printf '%s\n' "$$stray" | LC_ALL=C sort)"; \
	if [ -n "$$stray" ]; then \
		echo "$$stray" | sed 's|^|test-shell: |; s|$$| does not end in _test.sh and would never run; rename it, or move a helper under tests/lib/|' >&2; \
		exit 1; \
	fi; \
	tests="$$(find -L tests/unit ! -type d -name '*_test.sh')" || { echo 'test-shell: find could not walk tests/unit' >&2; exit 1; }; \
	tests="$$(printf '%s\n' "$$tests" | LC_ALL=C sort)"; \
	[ -n "$$tests" ] || { echo 'test-shell: no *_test.sh found under tests/unit' >&2; exit 1; }; \
	status=0; \
	for t in $$tests; do \
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

.PHONY: manifests-common
# manifests-common runs controller-gen crd over the envtest-only CELHost kind of
# internal/common/types/celhost, whose CRD installs the CEL rules of the shared
# types into the CRD-only envtest of internal/common. It is a prerequisite of
# manifests, so verify-codegen regenerates and diffs the CRD; verify-crd-sync
# iterates operators only and ignores it, because no chart ships it.
manifests-common:
	@echo "Generating the CELHost CRD for internal/common/types/celhost..."
	@cd internal/common && $(CONTROLLER_GEN) crd paths=./types/celhost/... output:crd:artifacts:config=types/celhost/crd

.PHONY: manifests
# manifests runs controller-gen crd and webhook to produce CRD YAML and webhook
# configuration for each operator that has an api/ directory, plus the RBAC
# ClusterRole assembled from the +kubebuilder:rbac markers across the module.
# Output is written to operators/<op>/config/crd/bases/, operators/<op>/config/webhook/
# and operators/<op>/config/rbac/role.yaml (the source of the chart's RBAC rules,
# see sync-helm-rbac).
manifests: manifests-common
	@for op in $(OPERATORS); do \
		if [ -d "operators/$$op/api" ]; then \
			echo "Generating CRD, webhook and RBAC manifests for operators/$$op..."; \
			mkdir -p operators/$$op/config/crd/bases operators/$$op/config/webhook operators/$$op/config/rbac; \
			(cd operators/$$op && $(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases); \
			(cd operators/$$op && $(CONTROLLER_GEN) webhook paths=./api/... output:webhook:artifacts:config=config/webhook); \
			(cd operators/$$op && $(CONTROLLER_GEN) rbac:roleName=$$op-operator paths=./... output:rbac:artifacts:config=config/rbac); \
		fi; \
	done

# ============================================================================
# CRD Sync Targets
# ============================================================================

# sync-crds prepends a cross-reference comment header to Helm CRD copies.
# verify-crd-sync strips comment lines (^#) before comparing, since the
# source CRDs generated by controller-gen contain no comment lines.

# TARGET_CLUSTER_CRDS are the c5c3-operator CRDs of the order kinds. Each gets a
# third copy in the target-cluster-access chart: an order on a target cluster
# lives there, so that cluster has to serve the kind.
TARGET_CLUSTER_CRDS := c5c3.io_keystoneusers.yaml c5c3.io_keystoneprojects.yaml c5c3.io_keystoneroleassignments.yaml c5c3.io_keystonecatalogentries.yaml c5c3.io_keystoneapplicationcredentials.yaml c5c3.io_mariadbdatabases.yaml c5c3.io_rabbitmqvhosts.yaml

.PHONY: sync-crds
# sync-crds copies generated CRD manifests from config/crd/bases/ to the Helm
# chart crds/ directory, prepending a cross-reference comment header, and the
# TARGET_CLUSTER_CRDS on into the target-cluster-access chart.
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
	@echo "Syncing the order CRDs into the target-cluster-access chart..."
	@for f in $(TARGET_CLUSTER_CRDS); do \
		cp "operators/c5c3/helm/c5c3-operator/crds/$$f" deploy/target-cluster/target-cluster-access/crds/; \
	done

.PHONY: verify-crd-sync
# verify-crd-sync checks that CRD files in helm chart crds/ directories match
# the source files in config/crd/bases/ (ignoring the cross-reference header);
# each TARGET_CLUSTER_CRDS copy in the target-cluster-access chart must equal the
# c5c3-operator chart copy byte for byte.
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
	for f in $(TARGET_CLUSTER_CRDS); do \
		if ! cmp -s "operators/c5c3/helm/c5c3-operator/crds/$$f" "deploy/target-cluster/target-cluster-access/crds/$$f"; then \
			echo "FAIL: deploy/target-cluster/target-cluster-access/crds/$$f differs from operators/c5c3/helm/c5c3-operator/crds/$$f"; \
			diff -u "operators/c5c3/helm/c5c3-operator/crds/$$f" "deploy/target-cluster/target-cluster-access/crds/$$f" || true; \
			fail=1; \
		fi; \
	done; \
	if [ "$$fail" -eq 1 ]; then \
		echo "CRD sync check failed. Run 'make sync-crds' to update."; \
		exit 1; \
	fi; \
	echo "CRD sync check passed."

.PHONY: tidy
# tidy runs go mod tidy in every workspace member. Each module declares its own
# pins, so tidy has to be per-module: it ignores go.work by design. Run it after
# adding or dropping an import, then commit the go.mod/go.sum it rewrites.
tidy:
	@echo "Tidying internal/common module..."
	@cd internal/common && go mod tidy
	@for op in $(OPERATORS); do \
		echo "Tidying operators/$$op module..."; \
		(cd operators/$$op && go mod tidy) || exit 1; \
	done

.PHONY: verify-go-tidy
# verify-go-tidy fails if any module's go.mod/go.sum differs from what go mod
# tidy would write (run in CI; mirrors verify-crd-sync). Without it an untidy
# module is invisible: the workspace build resolves fine while go.mod keeps a
# requirement nothing imports and go.sum keeps hashes for superseded versions,
# so the pins humans read stop matching the ones the module actually needs.
# `go mod tidy -diff` reports the delta without writing, so this is read-only.
verify-go-tidy:
	@fail=0; \
	for mod in internal/common $(addprefix operators/,$(OPERATORS)); do \
		if ! (cd $$mod && go mod tidy -diff > /dev/null 2>&1); then \
			echo "FAIL: $$mod is not tidy"; \
			(cd $$mod && go mod tidy -diff 2>&1 | head -40) || true; \
			fail=1; \
		fi; \
	done; \
	if [ "$$fail" -eq 1 ]; then \
		echo "Go tidy check failed. Run 'make tidy' and commit the result."; \
		exit 1; \
	fi; \
	echo "Go tidy check passed."

.PHONY: sync-helm-rbac
# sync-helm-rbac regenerates every operator chart's templates/_rbac-rules.tpl
# and templates/_webhook-rbac-rules.tpl from the <op>-operator and <op>-webhook
# ClusterRoles in operators/<op>/config/rbac/role.yaml (itself generated from
# the +kubebuilder:rbac markers by manifests). The chart RBAC rules are never
# edited by hand: change the markers, then run this.
sync-helm-rbac: manifests
	python3 hack/gen-helm-rbac-rules.py

.PHONY: verify-helm-rbac
# verify-helm-rbac fails if any committed templates/_rbac-rules.tpl or
# templates/_webhook-rbac-rules.tpl has drifted from the committed
# config/rbac/role.yaml, or if <op>-webhook grants a read <op>-operator does not
# (run in CI; mirrors verify-crd-sync).
# It reads the committed role.yaml, so it needs no controller-gen.
verify-helm-rbac:
	python3 hack/gen-helm-rbac-rules.py --check

.PHONY: gen-helm-schema
# gen-helm-schema regenerates every operator chart's values.schema.json from the
# single shared source in hack/gen-helm-values-schema.py. Edit the shared schema
# there (or a chart's values.schema.extras.json), then run this target so the
# charts stay in sync.
gen-helm-schema:
	python3 hack/gen-helm-values-schema.py

.PHONY: verify-helm-schema
# verify-helm-schema fails if any committed values.schema.json has drifted
# from the shared generator source (run in CI; mirrors verify-crd-sync).
verify-helm-schema:
	python3 hack/gen-helm-values-schema.py --check

.PHONY: gen-chaos-matrix
# gen-chaos-matrix regenerates the component x role matrix and the suite table
# of docs/reference/testing/chaos-e2e-tests.md from the `# Chaos matrix:`,
# `# Chaos scenario:` and `# Chaos budget:` lines of the chaos suite headers.
# Run it after a header change.
gen-chaos-matrix:
	hack/gen-chaos-matrix.sh

.PHONY: verify-chaos-matrix
# verify-chaos-matrix fails if the generated tables of the chaos page have
# drifted from the suite headers.
verify-chaos-matrix:
	hack/gen-chaos-matrix.sh --check

.PHONY: verify-helm-released-values
# verify-helm-released-values renders every operator chart with the values.yaml
# of its released chart (the highest tag under oci://ghcr.io/c5c3/charts) and
# fails when the chart rejects them. Waive an intended removal in
# hack/helm-removed-values.yaml. Needs network access to ghcr.io, no login.
# Usage: make verify-helm-released-values [OPERATOR=keystone]
verify-helm-released-values:
	hack/verify-helm-released-values.sh $(OPERATOR)

# ============================================================================
# Deployment and Docker Targets
# ============================================================================

.PHONY: image
# image builds the operator image, the flake attribute $(OPERATOR)-operator-image,
# loads it into Docker and tags it $(IMG). Needs Nix with flakes
# (docs/reference/ci-cd/nix-image-build.md). A failed nix build ends the
# target with nix's error, before docker runs.
# Usage: make image OPERATOR=keystone [IMG=custom:tag]
image:
	$(if $(OPERATOR),,$(error image requires OPERATOR, e.g. make image OPERATOR=keystone))
	out=$$(nix build --no-link --print-out-paths .#$(OPERATOR)-operator-image) && "$$out" | docker load
	docker tag $(OPERATOR)-operator:latest $(IMG)

# HELM_LIBRARY_CONSUMERS lists every chart that depends on the operator-library
# library chart: one operator chart per operator, plus the library's own test
# consumer under operators/shared/helm/, which runs the shared template suites.
HELM_LIBRARY_CONSUMERS = $(foreach op,$(OPERATORS),operators/$(op)/helm/$(op)-operator) operators/shared/helm/operator-library-testbed

.PHONY: helm-deps
# helm-deps vendors the operator-library library subchart into each consumer
# chart's charts/ directory (helm dependency build reads the committed
# Chart.lock). Run before helm lint/template/unittest locally; CI helm-validate
# runs it too. --skip-refresh: the dependency is local, so no chart-repository
# refresh is needed (and it avoids failing on a stale repo cache).
helm-deps:
	@for chart in $(HELM_LIBRARY_CONSUMERS); do \
		echo "Building Helm dependencies for $$chart..."; \
		helm dependency build --skip-refresh "$$chart"; \
	done

.PHONY: helm-bump-library
# helm-bump-library releases a new operator-library version to every consumer
# chart in one step: it rewrites the library's Chart.yaml version and the
# dependency pin in each consumer's Chart.yaml, then regenerates each Chart.lock
# (helm dependency update against the local file:// source, so no repository
# refresh). Bump the library whenever one of its templates changes; the
# consumer charts' own versions are bumped by hand, as before.
# Usage: make helm-bump-library VERSION=0.8.0
helm-bump-library:
	$(if $(VERSION),,$(error helm-bump-library requires VERSION, e.g. make helm-bump-library VERSION=0.8.0))
	@sed -i.bak -E 's/^version: .*/version: $(VERSION)/' operators/shared/helm/operator-library/Chart.yaml && rm -f operators/shared/helm/operator-library/Chart.yaml.bak
	@for chart in $(HELM_LIBRARY_CONSUMERS); do \
		echo "Pinning operator-library $(VERSION) in $$chart..."; \
		sed -i.bak -E '/- name: operator-library$$/{n;s/version: .*/version: $(VERSION)/;}' "$$chart/Chart.yaml" && rm -f "$$chart/Chart.yaml.bak"; \
		helm dependency update --skip-refresh "$$chart" >/dev/null; \
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

.PHONY: helm-package-target-cluster
# helm-package-target-cluster packages the target-cluster-access chart, the RBAC
# grant set a target cluster applies before it is registered.
# Usage: make helm-package-target-cluster [CHART_VERSION=1.2.3]
# No dependency build: the chart has no subchart.
helm-package-target-cluster:
	helm package deploy/target-cluster/target-cluster-access/ $(if $(CHART_VERSION),--version $(CHART_VERSION))

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
	@python3 tests/e2e/keystone/invalid-identitybackend-cr/_generate.py --check
	@python3 tests/e2e/horizon/invalid-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystoneservice-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystoneuser-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystoneproject-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystoneroleassignment-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystonecatalogentry-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-keystoneapplicationcredential-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-mariadbdatabase-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-rabbitmqvhost-cr/_generate.py --check
	@python3 tests/e2e/c5c3/invalid-sizingprofile-cr/_generate.py --check
	@python3 tests/e2e/glance/invalid-cr/_generate.py --check
	@python3 tests/e2e/glance/invalid-glancebackend-cr/_generate.py --check
	@python3 tests/e2e/placement/invalid-cr/_generate.py --check
	@python3 tests/e2e/barbican/invalid-cr/_generate.py --check
	@python3 tests/e2e/barbican/invalid-barbicansecretstore-cr/_generate.py --check
	@python3 tests/e2e/ovn/invalid-cr/_generate.py --check
	@python3 tests/e2e/ovn/invalid-ovnchassis-cr/_generate.py --check
	@python3 tests/e2e/neutron/invalid-cr/_generate.py --check
	@python3 tests/e2e/neutron/invalid-neutronmetadataagent-cr/_generate.py --check
	@python3 tests/e2e/cinder/invalid-cr/_generate.py --check
	@python3 tests/e2e/cinder/invalid-cinderbackend-cr/_generate.py --check
	@python3 tests/e2e/cinder/invalid-cinderbackupbackend-cr/_generate.py --check
	@python3 tests/e2e/nova/invalid-cr/_generate.py --check
	@python3 tests/e2e/nova/invalid-novacompute-cr/_generate.py --check
	@echo "Running invalid-CR fixture unit tests..."
	@python3 tests/e2e/keystone/invalid-cr/test_generate.py
	@python3 tests/e2e/keystone/invalid-identitybackend-cr/test_generate.py
	@python3 tests/e2e/horizon/invalid-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystoneservice-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystoneuser-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystoneproject-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystoneroleassignment-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystonecatalogentry-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-keystoneapplicationcredential-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-mariadbdatabase-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-rabbitmqvhost-cr/test_generate.py
	@python3 tests/e2e/c5c3/invalid-sizingprofile-cr/test_generate.py
	@python3 tests/e2e/glance/invalid-cr/test_generate.py
	@python3 tests/e2e/glance/invalid-glancebackend-cr/test_generate.py
	@python3 tests/e2e/placement/invalid-cr/test_generate.py
	@python3 tests/e2e/barbican/invalid-cr/test_generate.py
	@python3 tests/e2e/barbican/invalid-barbicansecretstore-cr/test_generate.py
	@python3 tests/e2e/ovn/invalid-cr/test_generate.py
	@python3 tests/e2e/ovn/invalid-ovnchassis-cr/test_generate.py
	@python3 tests/e2e/neutron/invalid-cr/test_generate.py
	@python3 tests/e2e/neutron/invalid-neutronmetadataagent-cr/test_generate.py
	@python3 tests/e2e/cinder/invalid-cr/test_generate.py
	@python3 tests/e2e/cinder/invalid-cinderbackend-cr/test_generate.py
	@python3 tests/e2e/cinder/invalid-cinderbackupbackend-cr/test_generate.py
	@python3 tests/e2e/nova/invalid-cr/test_generate.py
	@python3 tests/e2e/nova/invalid-novacompute-cr/test_generate.py

.PHONY: gen-option-catalogs
# gen-option-catalogs regenerates the per-release option catalogs the
# extraConfig validation checks against. For every releases/<release>/ directory
# it runs hack/gen-option-catalog.sh for keystone, glance, placement, barbican,
# neutron, and cinder, extracting each catalog from the matching shipped
# service image via that image's own oslo-config-generator. Requires docker and
# network access: it pulls the service image when absent and fetches the
# upstream generator config. It is intentionally wired into no aggregate target
# and no CI job.
gen-option-catalogs:
	@for release in $(notdir $(patsubst %/,%,$(wildcard releases/*/))); do \
		for service in keystone glance placement barbican neutron cinder nova; do \
			echo "Generating $$service $$release option catalog..."; \
			hack/gen-option-catalog.sh $$service $$release || exit 1; \
		done; \
	done

.PHONY: verify-option-catalogs
# verify-option-catalogs asserts the committed option catalogs stay byte-stable
# against a fresh extraction. It runs the same loop as gen-option-catalogs with
# --check, so a drifted or missing catalog fails the target with a diff. Like
# gen-option-catalogs it requires docker and network access and is wired into no
# aggregate target and no CI job.
verify-option-catalogs:
	@for release in $(notdir $(patsubst %/,%,$(wildcard releases/*/))); do \
		for service in keystone glance placement barbican neutron cinder nova; do \
			echo "Checking $$service $$release option catalog..."; \
			hack/gen-option-catalog.sh --check $$service $$release || exit 1; \
		done; \
	done

.PHONY: nix-lock
# nix-lock regenerates every lock under nix/locks/ from the release pins,
# writes the noVNC hash into images/nova/novnc.json and the operator vendor
# hashes into operators/<op>/vendor.json (hack/nix-lock.sh --all), with the uv
# of the lock-tools dev shell. Needs Nix with flakes and network access to
# GitHub, PyPI and the Go module proxy. Commit what it changes.
nix-lock:
	nix develop .#lock-tools --command bash hack/nix-lock.sh --all

.PHONY: nix-lock-check
# nix-lock-check runs nix-lock and fails when the locks it writes differ from
# the committed ones, the check of the nix-lock-check job in build-images.yaml.
nix-lock-check:
	nix develop .#lock-tools --command bash hack/nix-lock.sh --check

.PHONY: nix-lock-check-operators
# nix-lock-check-operators fails when an operators/<op>/vendor.json no longer
# matches the go.mod and go.sum files it derives from, the check of the
# verify-codegen job in ci.yaml. It runs no nix and needs no tool of the
# lock-tools shell, only jq and sha256sum or shasum.
nix-lock-check-operators:
	bash hack/nix-lock.sh --check-operators

.PHONY: check-feature-ids
# check-feature-ids fails if any internal feature / requirement ID (CC-NNNN or
# REQ-NNN) appears anywhere in the tracked source tree — code, tests, CI,
# scripts, the Makefile, and the published docs. Source describes behaviour,
# not internal tracking IDs. Mirrors the always-on CI gate and needs no cluster
# or toolchain beyond git and grep.
check-feature-ids:
	@bash scripts/check-no-feature-ids.sh

.PHONY: check-review-markers
# check-review-markers fails if any file carries more unresolved reviewer
# markers ("Reviewer: please …" questions committed instead of answered) than
# its entry in scripts/review-markers-baseline.txt records. Existing markers
# are tolerated; new ones may not land, and counts may only shrink. After
# resolving a marker, tighten the baseline:
#   scripts/check-no-review-markers.sh --update-baseline
check-review-markers:
	@bash scripts/check-no-review-markers.sh

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
# Like the CI job, it needs both operators deployed with
# WITH_PROMETHEUS=true hack/ci-deploy-operator.sh: keystone into its default
# keystone-system, horizon with NAMESPACE=horizon-system.
e2e-prometheus:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get ns monitoring >/dev/null 2>&1 || { echo 'kube-prometheus-stack is not installed; run `WITH_PROMETHEUS=true make deploy-infra` first' >&2; exit 1; }
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/prometheus-stack/ tests/e2e/horizon/prometheus-stack/

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
#
# The ten suites run as separate invocations, as they do in CI: the shared
# chainsaw config sets failFast, so a red registration suite would otherwise
# abort the full-chain suite, and running them in sequence also keeps their
# identity planes from converging on one node at the same time.
e2e-controlplane:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `WITH_CONTROLPLANE=true make deploy-infra` (and deploy K-ORC + the operators) first' >&2; exit 1; }
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/c5c3/full-controlplane-keystone/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-keystone-service tests/e2e/c5c3/keystone-service-foreign-namespace/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-keystone-user tests/e2e/c5c3/keystone-user/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-keystone-service-own-namespace tests/e2e/c5c3/keystone-service/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-controlplane-healthcheck tests/e2e/c5c3/healthcheck/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-controlplane-pod-security-restricted tests/e2e/c5c3/pod-security-restricted/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-admin-password-scoping tests/e2e/c5c3/admin-password-scoping/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-db-credential-scoping tests/e2e/c5c3/db-credential-scoping/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-multi-controlplane tests/e2e/c5c3/multi-controlplane/
	chainsaw test --config tests/e2e/chainsaw-config.yaml --report-name chainsaw-report-deletion-orchestration tests/e2e/c5c3/deletion-orchestration/

.PHONY: e2e-external-keystone
# e2e-external-keystone runs the External-mode ControlPlane suite against a plain
# Keystone the operator does NOT own (a brownfield-adoption stand-in). It brings
# up its own operator-free Keystone fixture, so it needs the same ControlPlane
# stack as e2e-controlplane (c5c3-operator + K-ORC + keystone-operator + OpenBao +
# ESO); the two cluster-reachability and stack-installed preflights are kept
# separate for the same distinct-failure-mode reason as e2e-controlplane. This
# target satisfies CI-to-Makefile parity so developers can reproduce the
# e2e-external-keystone CI job locally. Set E2E_REQUIRE_CONTROLPLANE_STACK=true to
# make the suite's presence guard fail loudly (as the CI job does) rather than SKIP.
e2e-external-keystone:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external make deploy-infra` (and deploy K-ORC + the operators) first' >&2; exit 1; }
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/c5c3/external-keystone/

.PHONY: e2e-controlplane-sso
# e2e-controlplane-sso runs the federated ControlPlane chain: attaching a
# KeystoneIdentityBackend is the only action taken, and the operator projects the
# websso choices onto Horizon and the trusted dashboard origin onto Keystone.
# The suite lives OUTSIDE tests/e2e/ so the per-CR e2e-operator matrix and
# `make e2e` do not sweep it up (see tests/e2e-controlplane-sso/chainsaw-test.yaml).
#
# It needs the Horizon CRD on top of what e2e-controlplane requires, so the
# preflights are kept separate: a missing horizon-operator must not read as a
# missing ControlPlane stack — see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-controlplane-sso CI job locally.
#
# The suite creates a ControlPlane named controlplane-sso, so the OpenBao
# bootstrap must have seeded that CR's admin-password and Horizon SECRET_KEY
# paths: deploy the stack with CONTROLPLANE_NAME=controlplane-sso. Run it against
# a FRESH cluster — chainsaw's cleanup deletes the ControlPlane but neither the
# MariaDB PVC nor anything Keystone persisted, so a second run boots against the
# first run's database.
e2e-controlplane-sso:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external CONTROLPLANE_NAME=controlplane-sso make deploy-infra` (and deploy K-ORC + the operators) first' >&2; exit 1; }
	@kubectl get crd horizons.horizon.openstack.c5c3.io >/dev/null 2>&1 || { echo 'the horizon-operator is not installed; run `OPERATOR=horizon IMAGE_REPO=ghcr.io/c5c3/horizon-operator NAMESPACE=horizon-system hack/ci-deploy-operator.sh` first' >&2; exit 1; }
	E2E_REQUIRE_CONTROLPLANE_STACK=true chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e-controlplane-sso/

.PHONY: e2e-autoscaling
# e2e-autoscaling runs the API autoscaler suite on a full ControlPlane: token
# load scales Keystone from one pod to its HPA maximum and back, the
# PodDisruptionBudget admits an eviction at the minimum, and every
# database-backed API is checked against its SQL connection cap at its maximum.
# The suite lives OUTSIDE tests/e2e/ so the per-CR e2e-operator matrix and
# `make e2e` do not sweep it up (see tests/e2e-autoscaling/chainsaw-test.yaml).
#
# It needs metrics-server and the VerticalPodAutoscaler CRD on top of the
# ControlPlane stack, so the four preflights are kept separate: a missing
# metrics API or VPA CRD must not read as a missing ControlPlane stack — see
# review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-autoscaling CI job locally.
#
# The suite creates a ControlPlane named cp-autoscaling, so the OpenBao
# bootstrap must have seeded that CR's admin-password path: deploy the stack
# with the command the remediation lines name, then K-ORC and the ten
# operators, as the CI job does.
E2E_AUTOSCALING_INFRA := WITH_METRICS_SERVER=true WITH_VPA=true WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external CONTROLPLANE_NAME=cp-autoscaling WITH_NFS=true WITH_MESSAGING=true
e2e-autoscaling:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable; run `$(E2E_AUTOSCALING_INFRA) make deploy-infra` first' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `$(E2E_AUTOSCALING_INFRA) make deploy-infra` (and deploy K-ORC + the operators) first' >&2; exit 1; }
	@kubectl get --raw /apis/metrics.k8s.io/v1beta1 >/dev/null 2>&1 || { echo 'metrics-server is not serving the resource-metrics API; run `$(E2E_AUTOSCALING_INFRA) make deploy-infra` first' >&2; exit 1; }
	@kubectl get crd verticalpodautoscalers.autoscaling.k8s.io >/dev/null 2>&1 || { echo 'the VerticalPodAutoscaler CRD is not installed; run `$(E2E_AUTOSCALING_INFRA) make deploy-infra` first (WITH_VPA=true deploys it)' >&2; exit 1; }
	@mkdir -p _output/reports
	chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e-autoscaling/

.PHONY: e2e-operator-upgrade
# e2e-operator-upgrade runs one operator's helm-upgrade-in-place suite,
# tests/e2e-operator-upgrade/<op>-helm-upgrade/ for OPERATOR (default
# keystone): it fetches the chart and image the suite's baseline.yaml pins from
# GHCR, installs them as the baseline, and the suite then upgrades the release
# to the in-repo chart+CRDs and asserts the deployed CR is undisturbed by the
# upgrade (each suite's header lists what it asserts). Because the suite
# manages the operator release itself, it lives
# OUTSIDE tests/e2e/ and must run against a cluster with the infra stack
# deployed but NO <op>-operator release yet (this target installs the
# baseline). The infra opt-ins, the sibling operators and the sibling CRDs the
# operator's suite needs are the caller's job, as the reusable workflow
# .github/workflows/e2e-operator-upgrade.yaml prepares them: the table in
# hack/ci-operator-upgrade-leg.sh lists them per operator. The suite upgrades
# to ghcr.io/c5c3/<op>-operator:dev, so that image must be loaded into kind
# first. The two preflights are kept separate so a kubectl/cluster
# reachability failure is not conflated with a pre-existing-operator failure —
# see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-operator-upgrade CI job locally. Requires
# `helm registry login ghcr.io` for the baseline chart/image pull; KIND_CLUSTER
# (default cobaltcore) selects the kind cluster to load the baseline image into.
UPGRADE_OPERATOR = $(or $(OPERATOR),keystone)
# The fetch script writes the image tag and the chart directory it fetched
# here, as it writes them to GITHUB_OUTPUT in CI, and the deploy reads them back.
UPGRADE_FETCHED = _output/operator-upgrade/$(UPGRADE_OPERATOR).fetched
UPGRADE_GRANTS = BARBICAN_SECRET_STORE_GRANTS=openstack=openbao-instance-provisioner
e2e-operator-upgrade:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@if helm status $(UPGRADE_OPERATOR)-operator -n $(UPGRADE_OPERATOR)-system >/dev/null 2>&1; then \
		echo 'a $(UPGRADE_OPERATOR)-operator release already exists in $(UPGRADE_OPERATOR)-system; this suite installs the released operator itself and must run against a cluster without a pre-deployed $(UPGRADE_OPERATOR)-operator (teardown the release or use a fresh kind cluster)' >&2; \
		exit 1; \
	fi
	@rm -f $(UPGRADE_FETCHED)
	GITHUB_OUTPUT=$(CURDIR)/$(UPGRADE_FETCHED) OPERATOR=$(UPGRADE_OPERATOR) hack/ci-fetch-released-operator.sh
	OPERATOR=$(UPGRADE_OPERATOR) IMAGE_REPO=ghcr.io/c5c3/$(UPGRADE_OPERATOR)-operator \
		IMAGE_TAG="$$(sed -n 's/^image_tag=//p' $(UPGRADE_FETCHED))" \
		CHART_DIR="$$(sed -n 's/^chart_dir=//p' $(UPGRADE_FETCHED))" NAMESPACE=$(UPGRADE_OPERATOR)-system \
		$(UPGRADE_GRANTS) hack/ci-deploy-operator.sh
	@mkdir -p _output/reports
	$(UPGRADE_GRANTS) chainsaw test --config tests/e2e-operator-upgrade/chainsaw-config.yaml \
		tests/e2e-operator-upgrade/$(UPGRADE_OPERATOR)-helm-upgrade/

.PHONY: e2e-multicluster
# e2e-multicluster runs the placed-services suite across two kind clusters: the
# management cluster this target's kubectl context points at (brought up by
# hack/deploy-mgmt-cluster.sh, running keystone-operator and barbican-operator)
# and the target cluster registered on it (brought up by
# `INFRA_ONLY=true CLUSTER_NAME=cobaltcore-target hack/deploy-infra.sh`, running the
# infrastructure). The suite lives OUTSIDE tests/e2e/ because every suite there
# runs against one cluster and `make e2e` sweeps that tree.
#
# The suite also runs an OVN chassis and a metadata agent on the management
# cluster, so bring the target up with WITH_OVN_KERNEL_MODULES=true (the host
# kernel serves both kind clusters) and `kind load` the ovn and neutron:2026.1
# images into the management cluster as well, as the CI job does.
#
# The two preflights are kept separate so a kubectl/cluster reachability failure
# is not conflated with a target cluster that was never registered — see review
# pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-multicluster CI job locally.
#
# The registration Secret carries the credentials the OPERATORS use, minted from
# the target-cluster-access chart's ServiceAccount.
# _output/cobaltcore-target.kubeconfig is chainsaw's own credential for asserting on
# the target and is not what the operators read; write it with
# `kind get kubeconfig --name cobaltcore-target > _output/cobaltcore-target.kubeconfig`.
# See docs/reference/target-clusters.md for the registration Secret's contract.
e2e-multicluster:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable; point the context at the management cluster (`kubectl config use-context kind-cobaltcore-mgmt`, created by hack/deploy-mgmt-cluster.sh)' >&2; exit 1; }
	@kubectl get secret cobaltcore-target -n c5c3-clusters >/dev/null 2>&1 || { echo 'no target cluster is registered; install deploy/target-cluster/target-cluster-access on the target and create the registration Secret `cobaltcore-target` in c5c3-clusters (see docs/guides/deploy-to-a-target-cluster.md)' >&2; exit 1; }
	@test -f _output/cobaltcore-target.kubeconfig || { echo 'chainsaw has no kubeconfig for the target cluster; run `kind get kubeconfig --name cobaltcore-target > _output/cobaltcore-target.kubeconfig`' >&2; exit 1; }
	chainsaw test --config tests/e2e-multicluster/chainsaw-config.yaml tests/e2e-multicluster/

.PHONY: e2e-orders-multicluster
# e2e-orders-multicluster runs the two-cluster orders suite: a service owner's
# seven orders applied on a registered target cluster and delivered there by the
# c5c3-operator on the management cluster this target's kubectl context points
# at. The management cluster carries the lean stack the e2e-orders-multicluster
# job deploys: `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external make
# deploy-infra`, the keystone, horizon, glance and placement CRDs, K-ORC
# (hack/ci-deploy-korc.sh), and the keystone- and c5c3-operator. The target is a
# bare kind cluster with deploy/target-cluster/target-cluster-access installed
# (namespaces={openstack}, assignedNamespaces={orders-tenant,orders-outsider})
# and the two tenant namespaces created; its registration Secret carries
# namespaces=openstack and assignedNamespaces=orders-tenant,orders-outsider. The
# suite lives OUTSIDE tests/e2e/ for the reason e2e-multicluster's does.
#
# The four preflights are kept separate so each missing piece is named on its
# own — see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-orders-multicluster CI job locally. Set
# E2E_REQUIRE_CONTROLPLANE_STACK=true to make the suite's presence guard fail
# loudly (as the CI job does) rather than SKIP.
e2e-orders-multicluster:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable; point the context at the management cluster' >&2; exit 1; }
	@kubectl get crd controlplanes.c5c3.io >/dev/null 2>&1 || { echo 'the c5c3 ControlPlane stack is not installed; run `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external make deploy-infra` (and deploy K-ORC + the keystone- and c5c3-operator) first' >&2; exit 1; }
	@kubectl get secret cobaltcore-target -n c5c3-clusters >/dev/null 2>&1 || { echo 'no target cluster is registered; install deploy/target-cluster/target-cluster-access on the target with assignedNamespaces={orders-tenant,orders-outsider} and create the registration Secret `cobaltcore-target` in c5c3-clusters with the namespaces and assignedNamespaces keys (see docs/quick-start-orders.md)' >&2; exit 1; }
	@test -f _output/cobaltcore-target.kubeconfig || { echo 'chainsaw has no kubeconfig for the target cluster; run `kind get kubeconfig --name cobaltcore-target > _output/cobaltcore-target.kubeconfig`' >&2; exit 1; }
	chainsaw test --config tests/e2e-orders-multicluster/chainsaw-config.yaml tests/e2e-orders-multicluster/

.PHONY: e2e-ovn-overlay
# e2e-ovn-overlay runs the Geneve datapath suite across the two workers of a
# multi-node kind cluster (hack/kind-config-multinode.yaml): it programs one
# logical switch port per worker, pings from a network namespace on the first
# to the port on the second, and asserts the Southbound database bound the port
# and registered two Geneve tunnel endpoints. The suite lives OUTSIDE tests/e2e/
# because every suite there runs against the single-node hack/kind-config.yaml
# cluster that `make e2e` sweeps, where both ports would land on one node and no
# packet would ever enter a tunnel.
#
# The two preflights are kept separate so a kubectl/cluster reachability failure
# is not conflated with a single-node cluster. See review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the e2e-ovn-overlay CI job locally.
#
# The suite labels and unlabels the workers itself, so the only thing it asks of
# the cluster is that two schedulable nodes exist and the ovn-operator is
# deployed (`OPERATOR=ovn IMAGE_REPO=ghcr.io/c5c3/ovn-operator NAMESPACE=ovn-system
# hack/ci-deploy-operator.sh`). The chassis pods need the openvswitch and geneve
# modules on the host, which `WITH_OVN_KERNEL_MODULES=true` loads at deploy time.
e2e-ovn-overlay:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@[ "$$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o name | wc -l | tr -d ' ')" -ge 2 ] || { echo 'the overlay suite needs a multi-node cluster; run KIND_CONFIG=hack/kind-config-multinode.yaml make deploy-infra first' >&2; exit 1; }
	chainsaw test --config tests/e2e-ovn-overlay/chainsaw-config.yaml tests/e2e-ovn-overlay/

.PHONY: e2e-nova-libvirt
# e2e-nova-libvirt runs a NovaCompute pool on Nova's libvirt driver against a
# libvirtd on the one kind node and boots a server there under QEMU's TCG
# emulation (virtType: qemu), from the stack's 1 MiB zero image. It asserts
# that libvirt runs the domain and that nova-compute built its disk as a qcow2
# overlay, and that deleting the server removes both. The suite lives OUTSIDE
# tests/e2e/ because the blocking nova leg of e2e-operator sweeps every
# directory under tests/e2e/nova/, while this suite runs in the non-blocking
# e2e-nova-libvirt CI job of its own.
#
# The three preflights are kept separate so each failure names its own cause:
# no reachable cluster, the operators missing, a cluster that is not one amd64
# node. The third exists because images/libvirt/qemu.nix builds the
# x86_64-softmmu target only: on an arm64 node Nova asks for an aarch64 guest
# that image cannot run.
#
# The suite labels and unlabels the node and starts libvirtd itself. It asks
# of the cluster the stack's infrastructure (`WITH_OVN_KERNEL_MODULES=true
# WITH_MESSAGING=true make deploy-infra`) and the six operators the CI job
# deploys with hack/ci-deploy-operator.sh: keystone, placement, glance, ovn,
# neutron and nova, each into <op>-system.
e2e-nova-libvirt:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd novacomputes.nova.openstack.c5c3.io >/dev/null 2>&1 || { echo 'the libvirt suite needs the nova-operator and its five siblings (keystone, placement, glance, ovn, neutron); deploy each with hack/ci-deploy-operator.sh first' >&2; exit 1; }
	@archs="$$(kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.architecture}')"; [ "$$archs" = "amd64" ] || { echo "the libvirt suite boots an x86_64 guest on one amd64 node; this cluster's nodes are: $$archs" >&2; exit 1; }
	chainsaw test --config tests/e2e-nova-libvirt/chainsaw-config.yaml tests/e2e-nova-libvirt/

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

.PHONY: dizzy-keystone dizzy-glance
# dizzy-keystone and dizzy-glance run dizzy's chaos churn soak (a 5-minute
# small profile by default) against the quick-start ControlPlane, exporting
# metrics to the dizzy VictoriaMetrics; watch them at
# https://dizzy.127-0-0-1.nip.io. On the metal-stack lab
# (EXTERNAL_CLUSTER=true), open the Gateway port-forward on 8443 and
# `kubectl -n dizzy port-forward svc/dizzy-victoria-metrics-server 8428:8428`
# first; Grafana is then at https://dizzy.127-0-0-1.nip.io:8443.
#
# Variables:
#   DIZZY_SCENARIO      alternate scenario file.
#   DIZZY_ARGS          extra dizzy flags, space-separated (e.g. --duration 30m).
#   DIZZY_VERSION       pin override; the pin itself lives only in hack/dizzy.sh
#                       so Renovate has exactly one string to bump.
#   DIZZY_SECRET        ControlPlane admin Secret name override.
#   DIZZY_CP_NAMESPACE  ControlPlane admin Secret namespace override.
#   EXTERNAL_CLUSTER    true: reach Keystone and VictoriaMetrics through those
#                       two port-forwards, without docker.
#
# The three preflights are kept separate so the failure modes stay
# distinguishable — the kubectl/cluster-reachability failure is not conflated
# with the dizzy-stack-not-installed failure nor with the missing
# ControlPlane-admin-Secret failure — see review pattern
# .planwerk/review_patterns/distinguish-collapsed-failure-modes-in-preflight-checks.md
dizzy-keystone dizzy-glance: dizzy-%:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get ns dizzy >/dev/null 2>&1 || { echo 'the dizzy stack is not installed; run `WITH_DIZZY=true make deploy-infra` first' >&2; exit 1; }
	@kubectl get secret "$${DIZZY_SECRET:-controlplane-keystone-admin-credentials}" -n "$${DIZZY_CP_NAMESPACE:-openstack}" >/dev/null 2>&1 || { echo 'no ControlPlane admin Secret found; deploy the quick-start ControlPlane and wait for Ready first' >&2; exit 1; }
	hack/dizzy.sh chaos $*

.PHONY: dizzy-keystone-pressure dizzy-keystone-compare
# dizzy-keystone-pressure runs dizzy's closed-loop load on Keystone's token
# path, `dizzy keystone pressure`, against the quick-start ControlPlane with a
# profile of tests/load/keystone-pressure/ and keeps the run record in
# _output/dizzy/pressure/<label>/<profile>/. Run it once per build under two
# labels, e.g. DIZZY_PRESSURE_LABEL=base and DIZZY_PRESSURE_LABEL=patched, and
# compare the two with dizzy-keystone-compare. The run exports to the dizzy
# VictoriaMetrics when the dizzy stack is installed and needs it otherwise
# not. On the metal-stack lab (EXTERNAL_CLUSTER=true) it goes through the
# Gateway port-forward, whose latency is larger than some of the deltas the
# profiles look for; keystone-pressure-ab measures inside the cluster. See
# docs/reference/testing/keystone-pressure-testing.md.
#
# Variables:
#   DIZZY_PRESSURE_PROFILE  profile name (default warm-cache).
#   DIZZY_PRESSURE_LABEL    directory of the run record (default run).
#   DIZZY_BASE, DIZZY_OTHER the two records dizzy-keystone-compare compares,
#                           each a run-*.json or a directory whose newest
#                           run-*.json is taken; DIZZY_BASE is the build
#                           without the change.
#   DIZZY_SCENARIO, DIZZY_ARGS, DIZZY_VERSION, DIZZY_SECRET,
#   DIZZY_CP_NAMESPACE, EXTERNAL_CLUSTER  as for dizzy-keystone.
dizzy-keystone-pressure:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get secret "$${DIZZY_SECRET:-controlplane-keystone-admin-credentials}" -n "$${DIZZY_CP_NAMESPACE:-openstack}" >/dev/null 2>&1 || { echo 'no ControlPlane admin Secret found; deploy the quick-start ControlPlane and wait for Ready first' >&2; exit 1; }
	hack/dizzy.sh pressure "$${DIZZY_PRESSURE_PROFILE:-warm-cache}"

dizzy-keystone-compare:
	@[ -n "$${DIZZY_BASE:-}" ] && [ -n "$${DIZZY_OTHER:-}" ] || { echo 'name the two run records: make dizzy-keystone-compare DIZZY_BASE=<record or directory> DIZZY_OTHER=<record or directory>' >&2; exit 1; }
	hack/dizzy.sh compare "$${DIZZY_BASE}" "$${DIZZY_OTHER}"

.PHONY: keystone-pressure-ab
# keystone-pressure-ab compares two Keystone service images under dizzy's
# token-path load on the kind stack: hack/keystone-pressure-ab.sh deploys a
# Keystone on each image side by side in openstack, each with a database and
# a Memcached of its own, runs every profile against both from a dizzy pod in
# the cluster, and compares the two run records per profile. Results and
# summary.md land in _output/keystone-pressure/. A latency delta never fails
# it; a failed deployment, run or comparison does.
#
# This target satisfies the CI-to-Makefile parity expected by
# .planwerk/review_patterns/maintain-ci-to-makefile-parity-for-new-jobs.md so
# developers can reproduce the keystone-pressure CI job locally. It asks of
# the cluster the stack of `make deploy-infra` and the keystone-operator
# (`OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator
# NAMESPACE=keystone-system hack/ci-deploy-operator.sh`), and of the
# workstation the two images in the local docker, which it loads into kind.
#
# Variables (see hack/keystone-pressure-ab.sh for all of them):
#   KEYSTONE_PRESSURE_BASE_IMAGE     image of the build without the change (required).
#   KEYSTONE_PRESSURE_PATCHED_IMAGE  image of the build with it (required).
#   KEYSTONE_PRESSURE_RELEASE        release of both images (default the newest).
#   KEYSTONE_PRESSURE_PROFILES       profiles (default the four Keystone-only ones).
#   KEYSTONE_PRESSURE_DURATION       dizzy --duration of every run (default each profile's).
#   KEYSTONE_PRESSURE_PROFILE_DURATIONS  <profile>=<duration> pairs overriding it.
keystone-pressure-ab:
	@kubectl version --request-timeout=2s >/dev/null 2>&1 || { echo 'kubectl is not configured or no cluster is reachable' >&2; exit 1; }
	@kubectl get crd keystones.keystone.openstack.c5c3.io >/dev/null 2>&1 || { echo 'the keystone-operator is not installed; run `OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator NAMESPACE=keystone-system hack/ci-deploy-operator.sh` first' >&2; exit 1; }
	hack/keystone-pressure-ab.sh

.PHONY: dizzy-soak-start dizzy-soak-status dizzy-soak-stop dizzy-soak-report
# dizzy-soak-start, -status, -stop and -report drive the long-running dizzy
# soak of the metal-stack lab: a Job in the namespace dizzy that runs
# `dizzy mix chaos` in the cluster of the current kubeconfig context, in the
# OpenStack project dizzy-soak, for 6h by default or until
# dizzy-soak-stop, and writes a report with a PASS or FAIL verdict to the
# claim dizzy-soak-reports. dizzy-soak-report copies the newest run to
# _output/dizzy/soak/<run>/; `hack/dizzy-soak.sh report <run>` copies
# another. See docs/reference/testing/dizzy-chaos-testing.md#in-cluster-soak.
#
# Variables:
#   DIZZY_SOAK_SERVICE          dizzy service, as in dizzy <service> chaos (default mix).
#   DIZZY_SOAK_DURATION         6h by default; 0 runs until stopped.
#   DIZZY_SCENARIO              scenario file (default the overlay's).
#   DIZZY_ARGS                  extra dizzy flags, space-separated.
#   DIZZY_VERSION               pin override; the pin lives in hack/dizzy.sh.
#   DIZZY_AUTH_URL              in-cluster Keystone URL override.
#   DIZZY_CP_NAME               ControlPlane name (default controlplane).
#   DIZZY_CP_NAMESPACE          ControlPlane namespace (default openstack).
#   EXTERNAL_OVERLAY            overlay root (default deploy/lab/metal-stack).
#   DIZZY_SOAK_NAMESPACES       platform namespaces to sample.
#   DIZZY_SOAK_SAMPLE_INTERVAL  seconds between samples (default 60).
#   DIZZY_SOAK_MAX_ERROR_RATE   error-rate threshold in percent (default none).
#   DIZZY_SOAK_MAX_P95_SECONDS  p95 latency threshold (default none).
#   DIZZY_SOAK_MAX_OPERATOR_HEAP_GROWTH_PERCENT  operator heap growth bound in percent (default 10).
#   DIZZY_SOAK_MAX_OPERATOR_GOROUTINE_GROWTH     operator goroutine growth bound (default 10).
dizzy-soak-start dizzy-soak-status dizzy-soak-stop dizzy-soak-report: dizzy-soak-%:
	hack/dizzy-soak.sh $*

.PHONY: lab-failover-preflight lab-failover-up lab-failover-run lab-failover-down
# lab-failover-preflight, -up, -run and -down drive the failover suites of
# tests/lab-failover/ on the metal-stack lab, against a Standard-profile
# ControlPlane openstack-ha/cp-ha beside the lab's own. preflight only reads;
# up, run and down need the go-ahead on the shared lab. up brings the plane
# up, run executes the suites, down deletes the plane and the namespace; run
# down before make teardown-infra. The outputs go to
# _output/lab-failover/<run>/. See
# docs/reference/infrastructure/infrastructure-manifests.md#lab-failover-runs.
#
# Variables:
#   LAB_FAILOVER_CONTEXT          kube context of the lab (required).
#   LAB_FAILOVER_GO_AHEAD         the lab's shoot name; required by up, run and down.
#   LAB_FAILOVER_MIN_FREE_MEMORY  floor of free memory requests (default 38Gi).
#   LAB_FAILOVER_RUN              run id (default: set by up, read by run and down).
#   LAB_FAILOVER_SCENARIOS        suites of run, space-separated (default all eleven).
lab-failover-preflight lab-failover-up lab-failover-run lab-failover-down: lab-failover-%:
	hack/lab-failover.sh $*

.PHONY: refresh-operator-digests
# refresh-operator-digests re-resolves the digest behind the self-built
# operators' :latest images (keystone/c5c3/horizon), refreshes the
# per-operator image-digest ConfigMaps on the current cluster, and requests a
# Flux reconcile so a freshly merged operator image rolls out without a
# redeploy. Only effective on stacks deployed with WITH_CONTROLPLANE=true
# CONTROLPLANE_OPERATORS=flux (elsewhere the operator HelmReleases are
# suspended and the reconcile annotation is inert). Requires docker buildx and
# a kubectl context pointing at the cluster.
refresh-operator-digests:
	hack/refresh-operator-image-digests.sh

.PHONY: perf-benchmark
# perf-benchmark measures reconcile-loop latency against a running kind stack.
# It applies 1/5/25 Keystone CRs (CR_COUNTS overrides), waits for readiness, and
# reports p50/p95/p99 for the per-sub-reconciler and end-to-end reconcile
# histograms. Set GATE_P95_SECONDS to fail when the steady-state end-to-end p95
# exceeds a target (the D3 SLO as a gate), and RESULT_JSON to also write the
# quantiles to that JSON file. The nightly Performance workflow
# (.github/workflows/performance.yaml) runs it with CR_COUNTS="1 3" and
# RESULT_JSON; the 25-CR default stays a local or dedicated-cluster run. See
# docs/reference/testing/reconcile-performance-benchmark.md.
perf-benchmark:
	hack/perf-reconcile-benchmark.sh

.PHONY: perf-token-baseline
# perf-token-baseline measures Keystone token issue and validate latency
# against a running kind stack with the keystone operator deployed. It applies
# one Keystone CR, runs the token stream of the chaos workload harness against
# it, and writes the p50/p99 of each op with its sample and failure counts to
# RESULT_JSON. Exits 1 when an op has fewer than MIN_SAMPLES ok records or any
# failed record. The nightly Performance workflow runs it. See
# docs/reference/testing/reconcile-performance-benchmark.md.
#
# Variables:
#   NAMESPACE             namespace of the CR and the workload pod (default openstack).
#   CR_NAME               Keystone CR name (default keystone-perf-token).
#   DURATION_SECONDS      how long the token stream runs (default 600).
#   MIN_SAMPLES           ok records each op needs (default 300).
#   READY_TIMEOUT         kubectl wait and delete timeout (default 600s).
#   OPERATOR_NAMESPACE    namespace of the operator Deployment (default keystone-system).
#   OPERATOR_DEPLOY       operator Deployment name (default keystone-operator).
#   DB_CLUSTER_REF        managed MariaDB cluster name (default openstack-db).
#   CACHE_CLUSTER_REF     managed memcached cluster name (default openstack-memcached).
#   IMAGE_REPOSITORY      keystone image repository (default ghcr.io/c5c3/keystone).
#   IMAGE_TAG             keystone image tag, recorded as meta.release (default 2026.1).
#   RESULT_JSON           the JSON file written (default _output/perf/token.json).
#   CHAOS_WORKLOAD_IMAGE  workload pod image (default ghcr.io/c5c3/tempest:2026.1).
perf-token-baseline:
	hack/perf-token-baseline.sh

.PHONY: audit-rbac-usage
# audit-rbac-usage diffs the RBAC requests an operator's ServiceAccount made on
# a local kind cluster against the ClusterRole its kubebuilder markers render.
# The cluster must come from hack/kind-config-audit.yaml, which writes the
# kube-apiserver audit log the diff reads; run the operator and its suites
# first. The report goes to stdout and OUT_DIR. See
# docs/reference/testing/rbac-usage-audit.md.
#
# Variables:
#   OPERATOR            operator to audit (required), e.g. keystone.
#   KIND_CLUSTER        kind cluster whose node holds the log (default cobaltcore).
#   OPERATOR_NAMESPACE  namespace of the operator's ServiceAccount (default <operator>-system).
#   OUT_DIR             report directory (default _output/rbac-usage).
audit-rbac-usage:
	OPERATOR="$(OPERATOR)" hack/ci-rbac-usage-report.sh

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
# The 30m go-test timeout must stay below the CI job wall (ci.yaml
# test-integration timeout-minutes) so a hung suite panics with a goroutine
# dump instead of being killed silently by the runner. The c5c3 controller
# envtest suite alone runs ~22m.
test-integration:
	@KUBEBUILDER_ASSETS=$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path) && export KUBEBUILDER_ASSETS && \
	echo "KUBEBUILDER_ASSETS=$$KUBEBUILDER_ASSETS" && \
	for op in $(OPERATORS); do \
		echo "Integration-testing operators/$$op module..."; \
		go test -tags=integration -timeout=30m -coverprofile=cover-integration-$$op.out ./operators/$$op/... || exit 1; \
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
