#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build-images workflow structure, conventions, and correctness
# Requirements: through
# Usage: bash tests/container-images/verify_build_images_workflow.sh
#
# note: the build-images workflow was refactored to delegate step-level
# logic to composite actions under .github/actions/ and helper scripts under
# hack/. Many assertions below therefore traverse those composite-action YAML
# files (supply-chain-attest, merge-manifest-and-attest, build-push-image,
# checkout-service-source, derive-service-tags, setup-docker-registry) and the
# hack/ci-run-unit-tests.sh + hack/ci-merge-manifest.sh helpers, instead of
# inspecting the workflow itself. The workflow is still verified for job
# topology, permissions, concurrency, and correct composite-action wiring.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKFLOW="$PROJECT_ROOT/.github/workflows/build-images.yaml"

# Composite actions and hack scripts that now host step-level logic.
ACTION_SUPPLY_CHAIN="$PROJECT_ROOT/.github/actions/supply-chain-attest/action.yaml"
ACTION_MERGE_MANIFEST="$PROJECT_ROOT/.github/actions/merge-manifest-and-attest/action.yaml"
ACTION_BUILD_PUSH="$PROJECT_ROOT/.github/actions/build-push-image/action.yaml"
ACTION_CHECKOUT_SOURCE="$PROJECT_ROOT/.github/actions/checkout-service-source/action.yaml"
ACTION_DERIVE_TAGS="$PROJECT_ROOT/.github/actions/derive-service-tags/action.yaml"
ACTION_SETUP_REGISTRY="$PROJECT_ROOT/.github/actions/setup-docker-registry/action.yaml"
HACK_RUN_UNIT_TESTS="$PROJECT_ROOT/hack/ci-run-unit-tests.sh"
HACK_MERGE_MANIFEST="$PROJECT_ROOT/hack/ci-merge-manifest.sh"
HACK_GEN_MATRIX="$PROJECT_ROOT/hack/ci-generate-build-matrix.sh"

PASS=0
FAIL=0

# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# Helper: query yq with raw output (strips quotes from strings)
yq_raw() {
  yq -r "$@"
}

# Helper: run a yq query and count matching lines.
# Fails loudly on yq parse errors instead of silently returning 0.
yq_count() {
  local output
  output=$(yq -r "$@") || { echo "ERROR: yq failed for query: $1" >&2; return 1; }
  echo "$output" | grep -c . || echo "0"
}

# --- SPDX header matching ci.yaml convention ---
test_spdx_header_present() {
  echo "Test: SPDX header present"

  local line1 line3
  line1=$(sed -n '1p' "$WORKFLOW")
  line3=$(sed -n '3p' "$WORKFLOW")

  assert_contains "line 1 has SPDX-FileCopyrightText" "$line1" "SPDX-FileCopyrightText"
  assert_contains "line 3 has SPDX-License-Identifier" "$line3" "SPDX-License-Identifier"
}

# --- Trigger key quoted to prevent YAML boolean interpretation ---
test_quoted_on_key() {
  echo "Test: trigger key is quoted as '\"on\"'"

  assert_file_contains "workflow has quoted on key" "$WORKFLOW" '"on"'
}

# --- Top-level permissions block ---
test_permissions_block() {
  echo "Test: top-level permissions block"

  # Top-level permissions should have contents: read only (least privilege)
  local top_perms
  top_perms=$(yq_raw '.permissions' "$WORKFLOW" || true)

  assert_contains "top-level permissions has contents: read" "$top_perms" "read"
}

# --- Job-level permissions scoping ---
test_job_permissions_scoping() {
  echo "Test: job-level permissions scoping"

  # build-base-images and build-service-images need packages: write
  local base_perms service_perms verify_service_perms
  base_perms=$(yq_raw '.jobs["build-base-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")
  service_perms=$(yq_raw '.jobs["build-service-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")
  verify_service_perms=$(yq_raw '.jobs["verify-service-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")

  assert_eq "build-base-images has packages: write" "write" "$base_perms"
  assert_eq "build-service-images has packages: write" "write" "$service_perms"
  assert_eq "verify-service-images has packages: read (least privilege)" "read" "$verify_service_perms"

  local merge_base_perms merge_service_perms
  merge_base_perms=$(yq_raw '.jobs["merge-base-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")
  merge_service_perms=$(yq_raw '.jobs["merge-service-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")

  assert_eq "merge-base-images has packages: write" "write" "$merge_base_perms"
  assert_eq "merge-service-images has packages: write" "write" "$merge_service_perms"

  # verify-service-images also needs contents: read for checkout (source-refs.yaml + patch counting)
  local verify_service_contents_perms
  verify_service_contents_perms=$(yq_raw '.jobs["verify-service-images"]["permissions"]["contents"]' "$WORKFLOW" || echo "null")
  assert_eq "verify-service-images has contents: read (for checkout)" "read" "$verify_service_contents_perms"
}

# --- Concurrency control ---
test_concurrency_control() {
  echo "Test: concurrency control"

  assert_file_contains "concurrency group pattern" "$WORKFLOW" 'github.ref.*github.workflow'
}

# --- Push triggers include main and stable/** ---
test_push_triggers() {
  echo "Test: push triggers include main and stable/**"

  assert_file_contains "push trigger includes main" "$WORKFLOW" "main"
  assert_file_contains "push trigger includes stable/**" "$WORKFLOW" "stable/\*\*"
}

# --- pull_request trigger present ---
test_pull_request_trigger() {
  echo "Test: pull_request trigger present"

  assert_file_contains "pull_request trigger present" "$WORKFLOW" "pull_request"
}

# --- Seven jobs defined ---
test_five_jobs_defined() {
  echo "Test: seven jobs defined"

  assert_file_contains "build-base-images job defined" "$WORKFLOW" "build-base-images:"
  assert_file_contains "merge-base-images job defined" "$WORKFLOW" "merge-base-images:"
  assert_file_contains "verify-base-images job defined" "$WORKFLOW" "verify-base-images:"
  assert_file_contains "build-service-images job defined" "$WORKFLOW" "build-service-images:"
  assert_file_contains "merge-service-images job defined" "$WORKFLOW" "merge-service-images:"
  assert_file_contains "test-service-images job defined" "$WORKFLOW" "test-service-images:"
  assert_file_contains "verify-service-images job defined" "$WORKFLOW" "verify-service-images:"
  assert_file_contains "build-keystone-federation-proxy job defined" "$WORKFLOW" "build-keystone-federation-proxy:"
  assert_file_contains "merge-keystone-federation-proxy-image job defined" "$WORKFLOW" "merge-keystone-federation-proxy-image:"
}

# --- verify-base-images job depends on build-base-images ---
test_verify_base_images_job() {
  echo "Test: verify-base-images job structure"

  local needs
  needs=$(yq_raw '.jobs["verify-base-images"]["needs"][]' "$WORKFLOW" || true)
  assert_contains "verify-base-images needs merge-base-images" "$needs" "merge-base-images"

  local timeout
  timeout=$(yq_raw '.jobs["verify-base-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  if [ "$timeout" != "null" ] && [ -n "$timeout" ]; then
    echo "  PASS: verify-base-images has timeout-minutes: $timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: verify-base-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  local runner
  runner=$(yq_raw '.jobs["verify-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  assert_eq "verify-base-images uses ubuntu-latest" "ubuntu-latest" "$runner"

  local pkg_perms
  pkg_perms=$(yq_raw '.jobs["verify-base-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")
  assert_eq "verify-base-images has packages: read" "read" "$pkg_perms"

  local contents_perms
  contents_perms=$(yq_raw '.jobs["verify-base-images"]["permissions"]["contents"]' "$WORKFLOW" || echo "null")
  assert_eq "verify-base-images has contents: read (for checkout)" "read" "$contents_perms"
}

# --- Base images build with multi-arch platforms ---
test_base_images_multi_arch() {
  echo "Test: base images use multi-arch platforms"

  local matrix_platforms
  matrix_platforms=$(yq_raw '.jobs["build-base-images"]["strategy"]["matrix"]["include"][]["platform"]' "$WORKFLOW" || true)
  assert_contains "build-base-images matrix includes linux/amd64" "$matrix_platforms" "linux/amd64"
  assert_contains "build-base-images matrix includes linux/arm64" "$matrix_platforms" "linux/arm64"
}

# --- Base image outputs contain digest references ---
test_base_image_digest_outputs() {
  echo "Test: base image outputs contain digest references"

  local python_output venv_output python_name_output python_digest_output
  python_output=$(yq_raw '.jobs["merge-base-images"]["outputs"]["python-base-image"]' "$WORKFLOW" || true)
  venv_output=$(yq_raw '.jobs["merge-base-images"]["outputs"]["venv-builder-image"]' "$WORKFLOW" || true)
  python_name_output=$(yq_raw '.jobs["merge-base-images"]["outputs"]["python-base-name"]' "$WORKFLOW" || true)
  python_digest_output=$(yq_raw '.jobs["merge-base-images"]["outputs"]["python-base-digest"]' "$WORKFLOW" || true)

  assert_contains "python-base-image output references digest" "$python_output" "outputs.digest"
  assert_contains "venv-builder-image output references digest" "$venv_output" "outputs.digest"
  assert_contains "python-base-name output is non-empty" "$python_name_output" "python-base"
  assert_contains "python-base-digest output references merge-python-base digest" "$python_digest_output" "merge-python-base.outputs.digest"
}

# --- keystone-federation-proxy build/merge job structure ---
test_keystone_federation_proxy_jobs() {
  echo "Test: keystone-federation-proxy job structure"

  local needs
  needs=$(yq_raw '.jobs["build-keystone-federation-proxy"]["needs"][]' "$WORKFLOW" || true)
  assert_contains "build-keystone-federation-proxy needs lint-dockerfiles" "$needs" "lint-dockerfiles"
  assert_contains "build-keystone-federation-proxy needs prepare" "$needs" "prepare"

  # Release-independent: no release axis, a static multi-arch include matrix.
  local matrix_platforms
  matrix_platforms=$(yq_raw '.jobs["build-keystone-federation-proxy"]["strategy"]["matrix"]["include"][]["platform"]' "$WORKFLOW" || true)
  assert_contains "build-keystone-federation-proxy matrix includes linux/amd64" "$matrix_platforms" "linux/amd64"
  assert_contains "build-keystone-federation-proxy matrix includes linux/arm64" "$matrix_platforms" "linux/arm64"

  # PR-inline verification wiring (the tempest pattern).
  local verify_script
  verify_script=$(yq_raw '.jobs["build-keystone-federation-proxy"]["steps"][] | select(.id == "build-keystone-federation-proxy") | .with["verify-script"]' "$WORKFLOW" || echo "null")
  assert_eq "build step wires the verify script" \
    "tests/container-images/verify_keystone_federation_proxy.sh" "$verify_script"

  # The lint matrix covers the new Dockerfile.
  local lint_matrix
  lint_matrix=$(yq_raw '.jobs["lint-dockerfiles"]["strategy"]["matrix"]["dockerfile"][]' "$WORKFLOW" || true)
  assert_contains "lint-dockerfiles covers the federation-proxy Dockerfile" \
    "$lint_matrix" "images/keystone-federation-proxy/Dockerfile"

  # Merge job: PR-skipped, needs the build, tags :latest + :<sha>.
  local merge_if
  merge_if=$(yq_raw '.jobs["merge-keystone-federation-proxy-image"]["if"]' "$WORKFLOW" || echo "null")
  assert_contains "merge job skipped on PRs" "$merge_if" "github.event_name != 'pull_request'"

  local merge_needs
  merge_needs=$(yq_raw '.jobs["merge-keystone-federation-proxy-image"]["needs"][]' "$WORKFLOW" || true)
  assert_contains "merge job needs the build job" "$merge_needs" "build-keystone-federation-proxy"

  local merge_tags
  merge_tags=$(yq_raw '.jobs["merge-keystone-federation-proxy-image"]["steps"][] | select(.id == "merge-keystone-federation-proxy") | .with["tags"]' "$WORKFLOW" || echo "null")
  assert_contains "merge tags include :latest" "$merge_tags" "keystone-federation-proxy:latest"
  assert_contains "merge tags include the commit SHA" "$merge_tags" 'keystone-federation-proxy:${{ github.sha }}'
}

# --- build-service-images depends on build-base-images and verify-base-images ---
test_service_images_depend_on_base() {
  echo "Test: build-service-images depends on build-base-images and verify-base-images"

  local needs
  needs=$(yq_raw '.jobs["build-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "build-service-images needs merge-base-images" "$needs" "merge-base-images"
  assert_contains "build-service-images needs verify-base-images" "$needs" "verify-base-images"
  assert_contains "build-service-images needs generate-matrix" "$needs" "generate-matrix"
}

# --- Matrix is dynamic via fromJson ---
test_matrix_includes_service_and_release() {
  echo "Test: matrix is dynamic via fromJson(needs.generate-matrix.outputs.matrix)"

  local matrix_expr
  matrix_expr=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "build-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "build-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"

  local gen_matrix_output
  gen_matrix_output=$(yq_raw '.jobs["generate-matrix"]["outputs"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "generate-matrix job exposes matrix output" "$gen_matrix_output" "matrix"
}

# --- Source ref resolution step exists (now in checkout-service-source composite) ---
test_source_ref_resolution_step() {
  echo "Test: source ref resolution step with yq"

  # build-service-images delegates source checkout to the
  # checkout-service-source composite action, which performs yq resolution
  # against releases/<release>/source-refs.yaml.
  local uses
  uses=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "checkout-source") | .uses' "$WORKFLOW" || true)
  assert_contains "build-service-images uses checkout-service-source composite" "$uses" "./.github/actions/checkout-service-source"

  local source_ref_step
  source_ref_step=$(yq_raw '.runs.steps[] | select(.id == "source-ref") | .run' "$ACTION_CHECKOUT_SOURCE" || true)
  assert_contains "checkout-service-source resolves ref with yq" "$source_ref_step" "yq"
  assert_contains "checkout-service-source reads source-refs.yaml" "$source_ref_step" "source-refs.yaml"
}

# --- Conditional patch application step (now in checkout-service-source composite) ---
test_patch_application_step() {
  echo "Test: conditional patch application with hashFiles guard"

  # patch application moved into checkout-service-source composite.
  # The composite uses a shell-level guard (compgen -G "*.patch") instead of
  # the workflow-level hashFiles() expression.
  local patch_run
  patch_run=$(yq_raw '.runs.steps[] | select(.name == "Apply patches") | .run' "$ACTION_CHECKOUT_SOURCE" || true)
  assert_contains "Apply patches step guards on .patch files" "$patch_run" '*.patch'
  assert_contains "Apply patches step uses compgen guard" "$patch_run" "compgen -G"
  assert_contains "Apply patches step runs git apply" "$patch_run" "git -C"
}

# --- Constraint overrides step (now in checkout-service-source composite) ---
test_constraint_overrides_step() {
  echo "Test: constraint overrides step references apply-constraint-overrides.sh"

  local overrides_run
  overrides_run=$(yq_raw '.runs.steps[] | select(.name == "Apply constraint overrides") | .run' "$ACTION_CHECKOUT_SOURCE" || true)
  assert_contains "checkout-service-source runs apply-constraint-overrides.sh" "$overrides_run" "apply-constraint-overrides.sh"
}

# --- Four build-contexts for service images ---
test_build_contexts_for_service_images() {
  echo "Test: build-contexts for service images"

  local build_contexts
  build_contexts=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["build-contexts"]' "$WORKFLOW" || true)

  assert_contains "build-context includes python-base" "$build_contexts" "python-base="
  assert_contains "build-context includes venv-builder" "$build_contexts" "venv-builder="
  assert_contains "build-context includes service source" "$build_contexts" "matrix.service"
  assert_contains "build-context includes upper-constraints" "$build_contexts" "upper-constraints="
}

# --- Tag schema composite ---
test_tag_schema_composite() {
  echo "Test: tag schema composite"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "Derive tags uses composite action" "$tags_step" "./.github/actions/derive-service-tags"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action has VERSION component" "$action_file" 'VERSION'
  assert_file_contains "composite action has PATCH_COUNT component" "$action_file" 'PATCH_COUNT'
  assert_file_contains "composite action has BRANCH component" "$action_file" 'BRANCH'
  assert_file_contains "composite action has SHORT_SHA component" "$action_file" 'SHORT_SHA'
}

# --- Branch sanitization (slash-to-dash) ---
test_branch_sanitization() {
  echo "Test: branch sanitization replaces slashes with dashes"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action has slash-to-dash replacement" "$action_file" 'GITHUB_REF_NAME//\\\//-'
}

# --- Version and SHA tag outputs emitted ---
test_version_and_sha_outputs() {
  echo "Test: version= and sha= outputs emitted"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action emits version output" "$action_file" '"version='
  assert_file_contains "composite action emits sha output" "$action_file" '"sha='
  assert_file_contains "composite action emits image output" "$action_file" '"image='
}

# --- PR uses single-arch, load, and conditional push/platforms ---
test_pr_single_arch_load() {
  echo "Test: PR uses single-arch, load, and conditional push/platforms"

  # build-matrix generation moved into hack/ci-generate-build-matrix.sh,
  # invoked from the generate-matrix job.
  local matrix_uses
  matrix_uses=$(yq_raw '.jobs["generate-matrix"]["steps"][] | select(.id == "matrix") | .run' "$WORKFLOW" || true)
  assert_contains "generate-matrix runs hack/ci-generate-build-matrix.sh" "$matrix_uses" "hack/ci-generate-build-matrix.sh"

  assert_file_contains "build-matrix script branches on pull_request" "$HACK_GEN_MATRIX" "pull_request"
  assert_file_contains "build-matrix script references linux/arm64" "$HACK_GEN_MATRIX" "linux/arm64"

  # PR single-arch "load" branch lives in build-push-image composite;
  # workflow passes push-by-digest=false on PR to activate the load path.
  assert_file_contains "build-push-image toggles load on push-by-digest" "$ACTION_BUILD_PUSH" "push-by-digest == 'false'"
  local pbd
  pbd=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["push-by-digest"]' "$WORKFLOW" || true)
  assert_contains "build-service push-by-digest conditioned on pull_request" "$pbd" "pull_request"
}

# --- verify-service-images uses verify_<service>.sh via matrix ---
test_verify_service_images_command() {
  echo "Test: verify-service-images uses verify script via matrix.service"

  # verify-service-images uses SERVICE_NAME env var to run the verify
  # script, avoiding direct matrix interpolation inside a run: block.
  local service_env run_block
  service_env=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.name == "Pull and verify service image") | .env["SERVICE_NAME"]' "$WORKFLOW" || true)
  run_block=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.name == "Pull and verify service image") | .run' "$WORKFLOW" || true)

  assert_contains "verify-service-images sets SERVICE_NAME env from matrix.service" "$service_env" "matrix.service"
  assert_contains "verify-service-images runs verify_\${SERVICE_NAME}.sh" "$run_block" 'verify_${SERVICE_NAME}.sh'
}

# --- verify-service-images depends on build-service-images and test-service-images ---
test_verify_service_images_depends_on_service_images() {
  echo "Test: verify-service-images depends on build-service-images and test-service-images"

  local needs
  needs=$(yq_raw '.jobs["verify-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "verify-service-images needs merge-service-images" "$needs" "merge-service-images"
  assert_contains "verify-service-images needs test-service-images" "$needs" "test-service-images"
  assert_contains "verify-service-images needs generate-matrix" "$needs" "generate-matrix"
}

# --- verify-service-images uses dynamic matrix via fromJson ---
test_verify_service_images_has_matrix() {
  echo "Test: verify-service-images uses dynamic matrix via fromJson"

  local matrix_expr
  matrix_expr=$(yq_raw '.jobs["verify-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "verify-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "verify-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"
}

# --- verify-service-images derives image ref independently ---
test_verify_service_images_derives_image_ref() {
  echo "Test: verify-service-images derives image ref via tags step"

  local tags_uses
  tags_uses=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "verify-service-images uses derive-service-tags composite action" "$tags_uses" "./.github/actions/derive-service-tags"
}

# --- All actions pinned to 40-char hex SHA ---
test_actions_pinned_to_sha() {
  echo "Test: all actions pinned to SHA"

  local all_pinned=true

  while IFS= read -r line; do
    [ -z "$line" ] && continue
    # Local composite actions (uses: ./) do not require SHA pinning
    echo "$line" | grep -qE 'uses:[[:space:]]+\./' && continue
    if ! echo "$line" | grep -qE '@[0-9a-f]{40}'; then
      echo "  FAIL: action not pinned to SHA: $line"
      FAIL=$((FAIL + 1))
      all_pinned=false
    fi
  done <<< "$(grep 'uses:' "$WORKFLOW")"

  if $all_pinned; then
    echo "  PASS: all actions pinned to 40-char SHA"
    PASS=$((PASS + 1))
  fi
}

# --- SHA-pinned actions have version comments ---
test_actions_have_version_comments() {
  echo "Test: SHA-pinned actions have version comments"

  local all_commented=true

  while IFS= read -r line; do
    [ -z "$line" ] && continue
    # Local composite actions (uses: ./) do not require version comments
    echo "$line" | grep -qE 'uses:[[:space:]]+\./' && continue
    if ! echo "$line" | grep -qE '# v[0-9]'; then
      echo "  FAIL: action missing version comment: $line"
      FAIL=$((FAIL + 1))
      all_commented=false
    fi
  done <<< "$(grep 'uses:' "$WORKFLOW")"

  if $all_commented; then
    echo "  PASS: all SHA-pinned actions have version comments"
    PASS=$((PASS + 1))
  fi
}

# --- GHA caching present (cache-from and cache-to) ---
test_gha_caching_present() {
  echo "Test: GHA caching present"

  # cache-from / cache-to moved into build-push-image composite, which
  # derives the scope from the cache-scope input.
  assert_file_contains "cache-from: type=gha present" "$ACTION_BUILD_PUSH" "cache-from: type=gha"
  assert_file_contains "cache-to: type=gha present" "$ACTION_BUILD_PUSH" "cache-to: type=gha"

  # Verify workflow actually passes a cache-scope to each build-push-image call.
  local base_scopes service_scope
  base_scopes=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/build-push-image"))) | .with["cache-scope"]' "$WORKFLOW" || true)
  service_scope=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["cache-scope"]' "$WORKFLOW" || true)
  assert_contains "build-base-images passes cache-scope for python-base" "$base_scopes" "python-base"
  assert_contains "build-base-images passes cache-scope for venv-builder" "$base_scopes" "venv-builder"
  assert_contains "build-service passes cache-scope with matrix.service" "$service_scope" "matrix.service"
}

# --- All jobs have timeout-minutes ---
test_timeout_minutes_on_all_jobs() {
  echo "Test: all jobs have timeout-minutes"

  local base_timeout verify_base_timeout service_timeout test_service_timeout verify_service_timeout
  base_timeout=$(yq_raw '.jobs["build-base-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  verify_base_timeout=$(yq_raw '.jobs["verify-base-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  service_timeout=$(yq_raw '.jobs["build-service-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  test_service_timeout=$(yq_raw '.jobs["test-service-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  verify_service_timeout=$(yq_raw '.jobs["verify-service-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")

  if [ "$base_timeout" != "null" ] && [ -n "$base_timeout" ]; then
    echo "  PASS: build-base-images has timeout-minutes: $base_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-base-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$verify_base_timeout" != "null" ] && [ -n "$verify_base_timeout" ]; then
    echo "  PASS: verify-base-images has timeout-minutes: $verify_base_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: verify-base-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$service_timeout" != "null" ] && [ -n "$service_timeout" ]; then
    echo "  PASS: build-service-images has timeout-minutes: $service_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-service-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$test_service_timeout" != "null" ] && [ -n "$test_service_timeout" ]; then
    echo "  PASS: test-service-images has timeout-minutes: $test_service_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: test-service-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$verify_service_timeout" != "null" ] && [ -n "$verify_service_timeout" ]; then
    echo "  PASS: verify-service-images has timeout-minutes: $verify_service_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: verify-service-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  local fedproxy_timeout fedproxy_merge_timeout
  fedproxy_timeout=$(yq_raw '.jobs["build-keystone-federation-proxy"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  fedproxy_merge_timeout=$(yq_raw '.jobs["merge-keystone-federation-proxy-image"]["timeout-minutes"]' "$WORKFLOW" || echo "null")

  if [ "$fedproxy_timeout" != "null" ] && [ -n "$fedproxy_timeout" ]; then
    echo "  PASS: build-keystone-federation-proxy has timeout-minutes: $fedproxy_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-keystone-federation-proxy missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$fedproxy_merge_timeout" != "null" ] && [ -n "$fedproxy_merge_timeout" ]; then
    echo "  PASS: merge-keystone-federation-proxy-image has timeout-minutes: $fedproxy_merge_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: merge-keystone-federation-proxy-image missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi
}

# --- All jobs use runs-on: ubuntu-latest ---
test_runs_on_ubuntu_latest() {
  echo "Test: all jobs use runs-on: ubuntu-latest"

  local verify_base_runner merge_base_runner merge_service_runner test_service_runner verify_service_runner
  verify_base_runner=$(yq_raw '.jobs["verify-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  merge_base_runner=$(yq_raw '.jobs["merge-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  merge_service_runner=$(yq_raw '.jobs["merge-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  test_service_runner=$(yq_raw '.jobs["test-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  verify_service_runner=$(yq_raw '.jobs["verify-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")

  assert_eq "verify-base-images uses ubuntu-latest" "ubuntu-latest" "$verify_base_runner"
  assert_eq "merge-base-images uses ubuntu-latest" "ubuntu-latest" "$merge_base_runner"
  assert_eq "merge-service-images uses ubuntu-latest" "ubuntu-latest" "$merge_service_runner"
  assert_eq "test-service-images uses ubuntu-latest" "ubuntu-latest" "$test_service_runner"
  assert_eq "verify-service-images uses ubuntu-latest" "ubuntu-latest" "$verify_service_runner"

  local base_runner service_runner
  base_runner=$(yq_raw '.jobs["build-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  service_runner=$(yq_raw '.jobs["build-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")

  assert_contains "build-base-images uses matrix runner expression" "$base_runner" "matrix.runner"
  assert_contains "build-service-images uses matrix runner expression" "$service_runner" "matrix.runner"

  local fedproxy_runner fedproxy_merge_runner
  fedproxy_runner=$(yq_raw '.jobs["build-keystone-federation-proxy"]["runs-on"]' "$WORKFLOW" || echo "null")
  fedproxy_merge_runner=$(yq_raw '.jobs["merge-keystone-federation-proxy-image"]["runs-on"]' "$WORKFLOW" || echo "null")

  assert_contains "build-keystone-federation-proxy uses matrix runner expression" "$fedproxy_runner" "matrix.runner"
  assert_eq "merge-keystone-federation-proxy-image uses ubuntu-latest" "ubuntu-latest" "$fedproxy_merge_runner"
}

# --- Base images always push unconditionally ---
test_base_images_always_push() {
  echo "Test: base images always push unconditionally"

  # The actual docker/build-push-action outputs= line lives inside the
  # build-push-image composite, gated on the push-by-digest input. Base image
  # build calls do NOT pass push-by-digest, so they inherit the default 'true'.
  assert_file_contains "build-push-image emits push-by-digest=true output" "$ACTION_BUILD_PUSH" "push-by-digest=true"
  assert_file_contains "build-push-image emits push=true output" "$ACTION_BUILD_PUSH" "push=true"

  local python_pbd venv_pbd
  python_pbd=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with["push-by-digest"]' "$WORKFLOW" || echo "null")
  venv_pbd=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["push-by-digest"]' "$WORKFLOW" || echo "null")
  # null = default (true) = unconditional push.
  assert_eq "python-base build does not override push-by-digest" "null" "$python_pbd"
  assert_eq "venv-builder build does not override push-by-digest" "null" "$venv_pbd"
}

# --- Fork PRs rejected in build-base-images ---
test_fork_pr_rejection_step_exists() {
  echo "Test: fork PR rejection step exists in build-base-images"

  # step name is "Reject fork PRs".
  local reject_step_if
  reject_step_if=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Reject fork PRs") | .if' "$WORKFLOW" || true)

  assert_contains "Reject fork PRs step exists with pull_request condition" "$reject_step_if" "pull_request"
  assert_contains "Reject fork PRs step checks head repo full_name" "$reject_step_if" "github.event.pull_request.head.repo.full_name"
  assert_contains "Reject fork PRs step compares against github.repository" "$reject_step_if" "github.repository"
}

# --- Base images have immutable SHA tags alongside :latest ---
test_base_images_have_sha_tags() {
  echo "Test: base images have SHA tags for commit traceability"

  # merge-base-images now calls merge-manifest-and-attest and passes
  # a space-separated `tags:` input that includes :latest and :${{ github.sha }}.
  local python_tags venv_tags
  python_tags=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with.tags' "$WORKFLOW" || true)
  venv_tags=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with.tags' "$WORKFLOW" || true)

  assert_contains "python-base tags include github.sha" "$python_tags" 'github.sha'
  assert_contains "venv-builder tags include github.sha" "$venv_tags" 'github.sha'
}

# --- Version-only tag restricted to main branch ---
test_version_tag_restricted_to_main() {
  echo "Test: version-only tag restricted to main branch"

  # the version/release tag append-on-main bash guard lives in the
  # workflow-level "Build service image tags" run script within merge-service-images.
  local service_tags_run
  service_tags_run=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "service-tags") | .run' "$WORKFLOW" || true)
  assert_contains "service tags script contains ref_name == main conditional" "$service_tags_run" 'GITHUB_REF_NAME}" == "main"'
  assert_contains "service tags script appends VERSION_TAG on main" "$service_tags_run" "VERSION_TAG"
}

# --- Matrix jobs use fail-fast: false for independent failure reporting ---
test_matrix_jobs_fail_fast_false() {
  echo "Test: matrix jobs use fail-fast: false"

  local service_fail_fast test_service_fail_fast verify_service_fail_fast
  service_fail_fast=$(yq_raw '.jobs["build-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")
  test_service_fail_fast=$(yq_raw '.jobs["test-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")
  verify_service_fail_fast=$(yq_raw '.jobs["verify-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")

  assert_eq "build-service-images has fail-fast: false" "false" "$service_fail_fast"
  assert_eq "test-service-images has fail-fast: false" "false" "$test_service_fail_fast"
  assert_eq "verify-service-images has fail-fast: false" "false" "$verify_service_fail_fast"
}

# --- Source ref resolution validates yq output against null/empty ---
test_source_ref_null_guard() {
  echo "Test: source-ref step validates yq output against null/empty"

  # resolution + null/empty guard now live in the checkout-service-source composite.
  local source_ref_run
  source_ref_run=$(yq_raw '.runs.steps[] | select(.id == "source-ref") | .run' "$ACTION_CHECKOUT_SOURCE" || true)

  assert_contains "source-ref step checks for null string" "$source_ref_run" '"null"'
  assert_contains "source-ref step checks for empty value" "$source_ref_run" '-z "$ref"'
  assert_contains "source-ref step exits on invalid ref" "$source_ref_run" "exit 1"
}

# --- all three tag-deriving jobs use the same composite action ---
test_verify_service_images_tag_derivation_sync_comment() {
  echo "Test: all tag-deriving jobs use derive-service-tags composite action"

  local build_uses merge_uses verify_uses
  build_uses=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  merge_uses=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  verify_uses=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "build-service-images uses derive-service-tags" "$build_uses" "./.github/actions/derive-service-tags"
  assert_contains "merge-service-images uses derive-service-tags" "$merge_uses" "./.github/actions/derive-service-tags"
  assert_contains "verify-service-images uses derive-service-tags" "$verify_uses" "./.github/actions/derive-service-tags"
}

# --- composite action validates yq output against null/empty ---
test_verify_service_images_null_guard() {
  echo "Test: composite action validates yq output against null/empty"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action checks for null string" "$action_file" '"null"'
  assert_file_contains "composite action exits on invalid ref" "$action_file" "exit 1"
}

# ===========================================================================================
# test-service-images job tests
# ===========================================================================================

# --- test-service-images job exists and has correct structure ---
test_test_service_images_job_structure() {
  echo "Test: test-service-images job structure"

  local runner
  runner=$(yq_raw '.jobs["test-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  assert_eq "test-service-images uses ubuntu-latest" "ubuntu-latest" "$runner"

  local timeout
  timeout=$(yq_raw '.jobs["test-service-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  assert_eq "test-service-images has timeout-minutes: 60" "60" "$timeout"

  local contents_perms
  contents_perms=$(yq_raw '.jobs["test-service-images"]["permissions"]["contents"]' "$WORKFLOW" || echo "null")
  assert_eq "test-service-images has contents: read" "read" "$contents_perms"

  local packages_perms
  packages_perms=$(yq_raw '.jobs["test-service-images"]["permissions"]["packages"]' "$WORKFLOW" || echo "null")
  assert_eq "test-service-images has packages: read" "read" "$packages_perms"

  # Validate absence of elevated permissions
  local id_token attestations security_events
  id_token=$(yq_raw '.jobs["test-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["test-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")
  security_events=$(yq_raw '.jobs["test-service-images"]["permissions"]["security-events"]' "$WORKFLOW" || echo "null")

  assert_eq "test-service-images has no id-token permission" "null" "$id_token"
  assert_eq "test-service-images has no attestations permission" "null" "$attestations"
  assert_eq "test-service-images has no security-events permission" "null" "$security_events"
}

# --- test-service-images depends on build-base-images and verify-base-images ---
test_test_service_images_depends_on_base() {
  echo "Test: test-service-images depends on build-base-images and verify-base-images"

  local needs
  needs=$(yq_raw '.jobs["test-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "test-service-images needs merge-base-images" "$needs" "merge-base-images"
  assert_contains "test-service-images needs verify-base-images" "$needs" "verify-base-images"
}

# --- test-service-images has matrix strategy matching build-service-images ---
test_test_service_images_has_matrix() {
  echo "Test: test-service-images has matrix strategy"

  local matrix_expr fail_fast
  matrix_expr=$(yq_raw '.jobs["test-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)
  fail_fast=$(yq_raw '.jobs["test-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")

  assert_contains "test-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "test-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"
  assert_eq "test-service-images has fail-fast: false" "false" "$fail_fast"
}

# --- test-service-images uses venv-builder-image from build-base-images outputs ---
test_test_service_images_uses_venv_builder_output() {
  echo "Test: test-service-images uses venv-builder-image output"

  local run_step_env
  run_step_env=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["VENV_BUILDER_IMAGE"]' "$WORKFLOW" || true)

  assert_contains "Run tests env references venv-builder-image output" "$run_step_env" "needs.merge-base-images.outputs.venv-builder-image"
}

# --- test-service-images resolves source ref from source-refs.yaml ---
test_test_service_images_source_ref_step() {
  echo "Test: test-service-images has source-ref resolution step"

  # source ref resolution + null/empty guard live in the
  # checkout-service-source composite, which test-service-images consumes.
  local uses
  uses=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "checkout-source") | .uses' "$WORKFLOW" || true)
  assert_contains "test-service-images uses checkout-service-source composite" "$uses" "./.github/actions/checkout-service-source"

  local source_ref_run
  source_ref_run=$(yq_raw '.runs.steps[] | select(.id == "source-ref") | .run' "$ACTION_CHECKOUT_SOURCE" || true)

  assert_not_empty "checkout-service-source has source-ref step" "$source_ref_run"
  assert_contains "source-ref step reads source-refs.yaml" "$source_ref_run" "source-refs.yaml"
  assert_contains "source-ref step checks for null" "$source_ref_run" '"null"'
  assert_contains "source-ref step checks for empty value" "$source_ref_run" '-z "$ref"'
  assert_contains "source-ref step exits on invalid ref" "$source_ref_run" "exit 1"
}

# --- test-service-images checks out service source at correct ref ---
test_test_service_images_checkout_service_source() {
  echo "Test: test-service-images checks out service source"

  # the actions/checkout step now lives in the checkout-service-source composite.
  local checkout_repo checkout_ref checkout_path
  checkout_repo=$(yq_raw '.runs.steps[] | select(.with.repository) | .with.repository' "$ACTION_CHECKOUT_SOURCE" || true)
  checkout_ref=$(yq_raw '.runs.steps[] | select(.with.repository) | .with.ref' "$ACTION_CHECKOUT_SOURCE" || true)
  checkout_path=$(yq_raw '.runs.steps[] | select(.with.repository) | .with.path' "$ACTION_CHECKOUT_SOURCE" || true)

  assert_contains "service checkout uses openstack/ repo" "$checkout_repo" "openstack/"
  assert_contains "service checkout uses resolved source-ref output" "$checkout_ref" "steps.source-ref.outputs.source-ref"
  assert_contains "service checkout path includes service input" "$checkout_path" "inputs.service"
}

# --- test-service-images applies patches ---
test_test_service_images_apply_patches() {
  echo "Test: test-service-images applies patches"

  # patch application moved into checkout-service-source composite,
  # which uses a shell-level compgen guard instead of a workflow hashFiles() expression.
  local apply_step_run
  apply_step_run=$(yq_raw '.runs.steps[] | select(.name == "Apply patches") | .run' "$ACTION_CHECKOUT_SOURCE" || true)

  assert_contains "Apply patches uses compgen guard on *.patch" "$apply_step_run" "compgen -G"
  assert_contains "Apply patches references *.patch files" "$apply_step_run" '*.patch'
  assert_contains "Apply patches runs git apply" "$apply_step_run" "git -C"
}

# --- test-service-images applies constraint overrides ---
test_test_service_images_constraint_overrides() {
  echo "Test: test-service-images applies constraint overrides"

  local override_step_run
  override_step_run=$(yq_raw '.runs.steps[] | select(.name == "Apply constraint overrides") | .run' "$ACTION_CHECKOUT_SOURCE" || true)

  assert_contains "constraint overrides step runs apply-constraint-overrides.sh" "$override_step_run" "apply-constraint-overrides.sh"
}

# --- test-service-images Run tests step mounts correct volumes ---
test_test_service_images_run_tests_volumes() {
  echo "Test: test-service-images Run tests step mounts correct volumes"

  # the docker run + volume mount sequence lives in hack/ci-run-unit-tests.sh.
  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)
  assert_contains "Run tests step invokes hack/ci-run-unit-tests.sh" "$run_step" "hack/ci-run-unit-tests.sh"

  assert_file_contains "ci-run-unit-tests.sh mounts service source" "$HACK_RUN_UNIT_TESTS" "/workspace/src"
  assert_file_contains "ci-run-unit-tests.sh mounts upper-constraints.txt" "$HACK_RUN_UNIT_TESTS" "upper-constraints.txt"
  assert_file_contains "ci-run-unit-tests.sh mounts test-excludes directory" "$HACK_RUN_UNIT_TESTS" "test-excludes"
  assert_file_contains "ci-run-unit-tests.sh mounts results directory" "$HACK_RUN_UNIT_TESTS" "/workspace/results"
}

# --- test-service-images Run tests step runs stestr ---
test_test_service_images_run_tests_stestr() {
  echo "Test: test-service-images Run tests step runs stestr"

  assert_file_contains "ci-run-unit-tests.sh installs test dependencies with pip" "$HACK_RUN_UNIT_TESTS" "pip install"
  assert_file_contains "ci-run-unit-tests.sh installs stestr" "$HACK_RUN_UNIT_TESTS" "stestr"
  assert_file_contains "ci-run-unit-tests.sh runs stestr init" "$HACK_RUN_UNIT_TESTS" "stestr init"
  assert_file_contains "ci-run-unit-tests.sh runs stestr run" "$HACK_RUN_UNIT_TESTS" "stestr run"
}

# --- test-service-images uses exclude-list from test-excludes ---
test_test_service_images_exclude_list() {
  echo "Test: test-service-images uses exclude-list from test-excludes"

  assert_file_contains "ci-run-unit-tests.sh builds EXCLUDE_LIST_ARG" "$HACK_RUN_UNIT_TESTS" "EXCLUDE_LIST_ARG"
  assert_file_contains "ci-run-unit-tests.sh checks for service-specific exclude file" "$HACK_RUN_UNIT_TESTS" 'test-excludes/${SERVICE_NAME}.txt'
  assert_file_contains "ci-run-unit-tests.sh passes exclude-list to stestr" "$HACK_RUN_UNIT_TESTS" "exclude-list"
}

# --- test-service-images exports subunit results ---
test_test_service_images_subunit_output() {
  echo "Test: test-service-images exports subunit test results"

  assert_file_contains "ci-run-unit-tests.sh exports subunit results" "$HACK_RUN_UNIT_TESTS" "stestr last --subunit"
  assert_file_contains "ci-run-unit-tests.sh writes results to subunit file" "$HACK_RUN_UNIT_TESTS" "testresults.subunit"
}

# --- test-service-images uploads test results as artifacts ---
test_test_service_images_upload_artifacts() {
  echo "Test: test-service-images uploads test results as artifacts"

  local upload_step_name upload_step_if upload_step_path upload_step_retention
  upload_step_name=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .name' "$WORKFLOW" || true)
  upload_step_if=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .if' "$WORKFLOW" || true)
  upload_step_path=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .with.path' "$WORKFLOW" || true)
  upload_step_retention=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .with["retention-days"]' "$WORKFLOW" || echo "null")

  assert_not_empty "Upload test results step exists" "$upload_step_name"
  assert_eq "Upload test results runs always" "always()" "$upload_step_if"
  assert_contains "Upload test results includes subunit file" "$upload_step_path" "testresults.subunit"
  assert_eq "Upload test results has 30-day retention" "30" "$upload_step_retention"
}

# --- artifact name includes matrix.service for disambiguation ---
test_test_service_images_artifact_name() {
  echo "Test: test-service-images artifact name includes matrix.service"

  local artifact_name
  artifact_name=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .with.name' "$WORKFLOW" || true)

  assert_contains "artifact name includes matrix.service" "$artifact_name" "matrix.service"
  assert_contains "artifact name includes matrix.release" "$artifact_name" "matrix.release"
}

# --- test-service-images env vars prevent expression injection ---
test_test_service_images_env_vars() {
  echo "Test: test-service-images steps use env vars for matrix values"

  # checkout-service-source composite action receives service/release
  # as inputs (not through workflow-level env vars). The composite then
  # populates MATRIX_SERVICE / MATRIX_RELEASE env vars on its internal steps
  # from those inputs, which keeps matrix values out of run: block interpolation.
  local checkout_with_service checkout_with_release
  checkout_with_service=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "checkout-source") | .with.service' "$WORKFLOW" || true)
  checkout_with_release=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "checkout-source") | .with.release' "$WORKFLOW" || true)
  assert_contains "checkout-source receives matrix.service" "$checkout_with_service" "matrix.service"
  assert_contains "checkout-source receives matrix.release" "$checkout_with_release" "matrix.release"

  # Inside the composite, steps are parameterized with env vars derived from inputs.
  local composite_source_env composite_patches_env
  composite_source_env=$(yq_raw '.runs.steps[] | select(.id == "source-ref") | .env["MATRIX_SERVICE"]' "$ACTION_CHECKOUT_SOURCE" || true)
  composite_patches_env=$(yq_raw '.runs.steps[] | select(.name == "Apply patches") | .env["MATRIX_SERVICE"]' "$ACTION_CHECKOUT_SOURCE" || true)
  assert_contains "composite source-ref step exposes MATRIX_SERVICE env" "$composite_source_env" "inputs.service"
  assert_contains "composite apply-patches step exposes MATRIX_SERVICE env" "$composite_patches_env" "inputs.service"

  # Run tests step uses env vars to inject matrix values into the shell script.
  local run_tests_env_service run_tests_env_release
  run_tests_env_service=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["SERVICE_NAME"]' "$WORKFLOW" || true)
  run_tests_env_release=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["RELEASE"]' "$WORKFLOW" || true)

  assert_contains "Run tests step has SERVICE_NAME env from matrix.service" "$run_tests_env_service" "matrix.service"
  assert_contains "Run tests step has RELEASE env from matrix.release" "$run_tests_env_release" "matrix.release"

  # INSTALL_SPEC env var references pip-extras output
  local install_spec_env
  install_spec_env=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["INSTALL_SPEC"]' "$WORKFLOW" || true)
  assert_contains "Run tests step has INSTALL_SPEC referencing pip-extras output" "$install_spec_env" "steps.pip-extras.outputs.install_spec"

  # Resolve pip extras step reads from extra-packages.yaml and outputs install_spec
  local pip_extras_run
  pip_extras_run=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "pip-extras") | .run' "$WORKFLOW" || true)
  assert_contains "Resolve pip extras reads extra-packages.yaml" "$pip_extras_run" "extra-packages.yaml"
  assert_contains "Resolve pip extras outputs install_spec" "$pip_extras_run" "install_spec="
}

# --- test-service-images uses docker run with venv-builder image ---
test_test_service_images_docker_run() {
  echo "Test: test-service-images uses docker run with venv-builder image"

  # docker run + venv-builder invocation lives in hack/ci-run-unit-tests.sh.
  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)
  assert_contains "Run tests step invokes hack/ci-run-unit-tests.sh" "$run_step" "hack/ci-run-unit-tests.sh"

  assert_file_contains "ci-run-unit-tests.sh uses docker run" "$HACK_RUN_UNIT_TESTS" "docker run"
  assert_file_contains "ci-run-unit-tests.sh references VENV_BUILDER_IMAGE" "$HACK_RUN_UNIT_TESTS" "VENV_BUILDER_IMAGE"
  assert_file_contains "ci-run-unit-tests.sh creates results directory" "$HACK_RUN_UNIT_TESTS" "mkdir -p"
}

# --- Expression injection defense — run: blocks use env vars ---
test_run_blocks_use_env_vars() {
  echo "Test: run: blocks use env vars instead of direct interpolation"

  assert_file_not_contains "resolve source ref run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'yq .*\${{ matrix'
  assert_file_not_contains "apply patches run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'git -C.*\${{ matrix'
  assert_file_not_contains "apply overrides run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'apply-constraint-overrides.sh \${{ matrix'
}

# --- SBOM permissions on merge-base-images ---
test_sbom_permissions_on_build_base_images() {
  echo "Test: SBOM permissions on merge-base-images"

  local id_token attestations
  id_token=$(yq_raw '.jobs["merge-base-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["merge-base-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "merge-base-images has id-token: write" "write" "$id_token"
  assert_eq "merge-base-images has attestations: write" "write" "$attestations"
}

# --- SBOM permissions on merge-service-images ---
test_sbom_permissions_on_build_service_images() {
  echo "Test: SBOM permissions on merge-service-images"

  local id_token attestations
  id_token=$(yq_raw '.jobs["merge-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["merge-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "merge-service-images has id-token: write" "write" "$id_token"
  assert_eq "merge-service-images has attestations: write" "write" "$attestations"
}

# --- Verify jobs do NOT have SBOM permissions ---
test_verify_jobs_no_sbom_permissions() {
  echo "Test: verify jobs do not have SBOM permissions"

  local verify_base_id_token verify_base_attestations
  verify_base_id_token=$(yq_raw '.jobs["verify-base-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  verify_base_attestations=$(yq_raw '.jobs["verify-base-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "verify-base-images has no id-token permission" "null" "$verify_base_id_token"
  assert_eq "verify-base-images has no attestations permission" "null" "$verify_base_attestations"

  local verify_service_id_token verify_service_attestations
  verify_service_id_token=$(yq_raw '.jobs["verify-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  verify_service_attestations=$(yq_raw '.jobs["verify-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "verify-service-images has no id-token permission" "null" "$verify_service_id_token"
  assert_eq "verify-service-images has no attestations permission" "null" "$verify_service_attestations"

  local test_service_id_token test_service_attestations
  test_service_id_token=$(yq_raw '.jobs["test-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  test_service_attestations=$(yq_raw '.jobs["test-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "test-service-images has no id-token permission" "null" "$test_service_id_token"
  assert_eq "test-service-images has no attestations permission" "null" "$test_service_attestations"
}

# --- SBOM generation steps exist ---
test_sbom_generation_steps_exist() {
  echo "Test: SBOM generation wired through supply-chain-attest composite"

  # SBOM generation (anchore/sbom-action) now lives in the
  # supply-chain-attest composite. The workflow exercises that composite
  # once per image via merge-manifest-and-attest.
  local sbom_count
  sbom_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("anchore/sbom-action"))) | .uses' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 1 anchore/sbom-action step" "1" "$sbom_count"

  # merge-base-images invokes merge-manifest-and-attest twice (python-base + venv-builder).
  local base_merge_count
  base_merge_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/merge-manifest-and-attest"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images invokes merge-manifest-and-attest twice" "2" "$base_merge_count"

  # merge-service-images invokes merge-manifest-and-attest once.
  local service_merge_count
  service_merge_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/merge-manifest-and-attest"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images invokes merge-manifest-and-attest once" "1" "$service_merge_count"
}

# --- SBOM format is cyclonedx-json ---
test_sbom_format_cyclonedx_json() {
  echo "Test: SBOM format is cyclonedx-json"

  local fmt
  fmt=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.format' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "supply-chain-attest SBOM format is cyclonedx-json" "cyclonedx-json" "$fmt"
}

# --- SBOM generation steps disable artifact upload ---
test_sbom_no_artifact_upload() {
  echo "Test: SBOM generation sets upload-artifact: false"

  local upload
  upload=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with["upload-artifact"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "supply-chain-attest SBOM step sets upload-artifact: false" "false" "$upload"
}

# --- SBOM generation references correct digest ---
test_sbom_generation_references_digest() {
  echo "Test: SBOM generation references correct digest"

  # supply-chain-attest receives image-digest as an input and
  # interpolates it into anchore/sbom-action's `image` field. Verify the
  # primitive, then check each workflow call site passes the right digest.
  local sbom_image
  sbom_image=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.image' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest composes image from inputs" "$sbom_image" "inputs.image-name"
  assert_contains "supply-chain-attest composes image from digest" "$sbom_image" "inputs.image-digest"

  # merge-manifest-and-attest forwards steps.merge.outputs.digest to supply-chain-attest.
  local forwarded_digest
  forwarded_digest=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("./.github/actions/supply-chain-attest"))) | .with["image-digest"]' "$ACTION_MERGE_MANIFEST" || true)
  assert_contains "merge-manifest-and-attest forwards merge digest" "$forwarded_digest" "steps.merge.outputs.digest"

  # The workflow labels the python-base / venv-builder / service call sites with the right image input.
  local python_image venv_image service_image
  python_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with.image' "$WORKFLOW" || true)
  venv_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with.image' "$WORKFLOW" || true)
  service_image=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Merge and attest service image") | .with.image' "$WORKFLOW" || true)
  assert_contains "python-base merge call passes python-base image" "$python_image" "python-base"
  assert_contains "venv-builder merge call passes venv-builder image" "$venv_image" "venv-builder"
  assert_contains "service merge call passes tags.outputs.image" "$service_image" "steps.tags.outputs.image"
}

# --- SBOM attestation steps exist ---
test_sbom_attestation_steps_exist() {
  echo "Test: SBOM attestation step exists in supply-chain-attest"

  # one actions/attest@ step lives in supply-chain-attest composite;
  # it runs once per supply-chain-attest invocation.
  local attest_count
  attest_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("actions/attest@"))) | .uses' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 1 actions/attest step" "1" "$attest_count"
}

# --- SBOM attestation push-to-registry ---
test_sbom_attestation_push_to_registry() {
  echo "Test: SBOM attestation push-to-registry is true"

  # actions/attest@ lives in supply-chain-attest composite. Verify
  # that its push-to-registry is hard-coded to true.
  local push
  push=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("actions/attest@"))) | .with["push-to-registry"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "supply-chain-attest actions/attest push-to-registry is true" "true" "$push"
}

# --- SBOM/attestation steps have PR-skip guard ---
test_sbom_steps_pr_skip_guard() {
  echo "Test: SBOM/attestation steps have PR-skip guard"

  # supply-chain-attest SBOM generation + actions/attest + provenance
  # + cosign sign are all gated on scan-mode == 'sbom'. On PR the workflow
  # passes scan-mode=image (for merge-base-images) or skips entirely
  # (merge-service-images has a job-level PR guard), so the SBOM primitives
  # never run on PR.
  local sbom_if attest_if
  sbom_if=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("anchore/sbom-action"))) | .if' "$ACTION_SUPPLY_CHAIN" || true)
  attest_if=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("actions/attest@"))) | .if' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest SBOM step guarded by scan-mode sbom" "$sbom_if" "scan-mode == 'sbom'"
  assert_contains "supply-chain-attest attest step guarded by scan-mode sbom" "$attest_if" "scan-mode == 'sbom'"

  # merge-base-images passes scan-mode=image on PR for python-base and venv-builder.
  local python_scan_mode venv_scan_mode
  python_scan_mode=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with["scan-mode"]' "$WORKFLOW" || true)
  venv_scan_mode=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with["scan-mode"]' "$WORKFLOW" || true)
  assert_contains "python-base merge picks scan-mode from event_name" "$python_scan_mode" "pull_request"
  assert_contains "venv-builder merge picks scan-mode from event_name" "$venv_scan_mode" "pull_request"

  # merge-service-images has job-level PR guard.
  local merge_service_job_if
  merge_service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "merge-service-images job has PR-skip guard" "$merge_service_job_if" "event_name != 'pull_request'"
}

# --- build provenance attestation steps exist (SLSA Level 2+) ---
test_build_provenance_steps_exist() {
  echo "Test: build provenance attestation exists in supply-chain-attest composite"

  # single attest-build-provenance step in supply-chain-attest composite,
  # invoked once per image via merge-manifest-and-attest.
  local prov_count
  prov_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .uses' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 1 attest-build-provenance step" "1" "$prov_count"
}

# --- build provenance steps have PR-skip guard ---
test_build_provenance_steps_pr_skip_guard() {
  echo "Test: build provenance guarded by scan-mode sbom"

  local prov_if
  prov_if=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .if' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest provenance step guarded by scan-mode sbom" "$prov_if" "scan-mode == 'sbom'"

  # merge-service-images uses a job-level if guard instead of per-step guards.
  local service_job_if
  service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "merge-service-images has job-level PR-skip guard" "$service_job_if" "event_name != 'pull_request'"
}

# --- build provenance push-to-registry is true ---
test_build_provenance_push_to_registry() {
  echo "Test: build provenance push-to-registry is true"

  local push
  push=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .with["push-to-registry"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "supply-chain-attest provenance push-to-registry is true" "true" "$push"
}

# --- metadata-action step lives inside build-push-image composite ---
test_metadata_action_steps_exist_in_build_base_images() {
  echo "Test: metadata-action wired through build-push-image"

  # there is one docker/metadata-action step in the build-push-image
  # composite, exercised once per image build (python-base + venv-builder
  # + service + tempest).
  local meta_count
  meta_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("docker/metadata-action"))) | .uses' "$ACTION_BUILD_PUSH")
  assert_eq "build-push-image has 1 docker/metadata-action step" "1" "$meta_count"

  local meta_id
  meta_id=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("docker/metadata-action"))) | .id' "$ACTION_BUILD_PUSH" || true)
  assert_eq "build-push-image metadata step id is 'meta'" "meta" "$meta_id"

  # Verify build-base-images calls build-push-image twice.
  local base_build_count
  base_build_count=$(yq_count '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/build-push-image"))) | .uses' "$WORKFLOW")
  assert_eq "build-base-images invokes build-push-image twice" "2" "$base_build_count"
}

# --- metadata-action step invoked for each service build ---
test_metadata_action_step_exists_in_build_service_images() {
  echo "Test: build-service-images invokes build-push-image"

  local service_build_count
  service_build_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/build-push-image"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images invokes build-push-image once" "1" "$service_build_count"
}

# --- service metadata uses raw version strategy ---
test_service_metadata_uses_raw_version_strategy() {
  echo "Test: service metadata uses raw version strategy"

  # the metadata-tags input is passed by the caller (build-service-images)
  # into build-push-image, which forwards it to docker/metadata-action.
  local tags_input
  tags_input=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["metadata-tags"]' "$WORKFLOW" || true)
  assert_contains "build-service metadata-tags uses type=raw" "$tags_input" "type=raw"
  assert_contains "build-service metadata-tags references checkout-source source-ref output" "$tags_input" "steps.checkout-source.outputs.source-ref"

  # build-push-image forwards metadata-tags input into docker/metadata-action.
  local composite_tags
  composite_tags=$(yq_raw '.runs.steps[] | select(.id == "meta") | .with.tags' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image meta step forwards metadata-tags input" "$composite_tags" "inputs.metadata-tags"
}

# --- base metadata calls pass no metadata-tags (empty string default) ---
test_base_metadata_steps_have_no_tags_override() {
  echo "Test: base build-push-image calls pass empty metadata-tags"

  local python_meta_tags venv_meta_tags
  python_meta_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with["metadata-tags"]' "$WORKFLOW" || echo "null")
  venv_meta_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["metadata-tags"]' "$WORKFLOW" || echo "null")
  assert_eq "build-python-base does not override metadata-tags" "null" "$python_meta_tags"
  assert_eq "build-venv-builder does not override metadata-tags" "null" "$venv_meta_tags"
}

# --- python-base build-push-action forwards generated labels ---
test_python_base_build_push_has_labels_input() {
  echo "Test: build-push-image forwards meta.outputs.labels to docker/build-push-action"

  local labels
  labels=$(yq_raw '.runs.steps[] | select(.id == "build") | .with.labels' "$ACTION_BUILD_PUSH" || true)
  assert_contains "docker/build-push-action labels references steps.meta.outputs.labels" "$labels" "steps.meta.outputs.labels"
}

# --- venv-builder build-push labels input (already covered by previous) ---
test_venv_builder_build_push_has_labels_input() {
  echo "Test: build-push-image passes labels for venv-builder build"

  # Each build-push-image invocation receives labels-title / labels-description
  # inputs which are injected into meta.with.labels template.
  local title
  title=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["labels-title"]' "$WORKFLOW" || true)
  assert_eq "build-venv-builder passes labels-title=venv-builder" "venv-builder" "$title"
}

# --- service build-push-action has labels input ---
test_service_build_push_has_labels_input() {
  echo "Test: build-push-image passes labels for service build"

  local title
  title=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["labels-title"]' "$WORKFLOW" || true)
  assert_contains "build-service passes labels-title from matrix.service" "$title" "matrix.service"
}

# --- OCI base image labels present via extra-labels input ---
test_oci_base_labels_in_build_steps() {
  echo "Test: OCI base image labels present via extra-labels"

  # build-push-image exposes an extra-labels input, used to inject
  # org.opencontainers.image.base.{name,digest} per call site.
  local python_extra venv_extra service_extra
  python_extra=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with["extra-labels"]' "$WORKFLOW" || true)
  venv_extra=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["extra-labels"]' "$WORKFLOW" || true)
  service_extra=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["extra-labels"]' "$WORKFLOW" || true)

  assert_contains "build-python-base extra-labels include base.name" "$python_extra" "org.opencontainers.image.base.name"
  assert_contains "build-python-base extra-labels include base.digest" "$python_extra" "org.opencontainers.image.base.digest"
  assert_contains "build-venv-builder extra-labels include base.name" "$venv_extra" "org.opencontainers.image.base.name"
  assert_contains "build-venv-builder extra-labels include base.digest" "$venv_extra" "org.opencontainers.image.base.digest"
  assert_contains "build-service extra-labels include base.name" "$service_extra" "org.opencontainers.image.base.name"
  assert_contains "build-service extra-labels include base.digest" "$service_extra" "org.opencontainers.image.base.digest"

  # build-push-image forwards extra-labels into docker/build-push-action labels input.
  assert_file_contains "build-push-image forwards extra-labels" "$ACTION_BUILD_PUSH" "inputs.extra-labels"
}

# --- metadata-action labels include OCI title ---
test_metadata_action_labels_include_oci_title() {
  echo "Test: metadata-action labels include OCI title"

  local labels
  labels=$(yq_raw '.runs.steps[] | select(.id == "meta") | .with.labels' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image meta labels include OCI title" "$labels" "org.opencontainers.image.title"

  # Each caller must pass a labels-title input so metadata-action can render it.
  local python venv service
  python=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with["labels-title"]' "$WORKFLOW" || true)
  venv=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["labels-title"]' "$WORKFLOW" || true)
  service=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["labels-title"]' "$WORKFLOW" || true)
  assert_not_empty "build-python-base passes labels-title" "$python"
  assert_not_empty "build-venv-builder passes labels-title" "$venv"
  assert_not_empty "build-service passes labels-title" "$service"
}

# --- metadata-action labels include OCI description ---
test_metadata_action_labels_include_oci_description() {
  echo "Test: metadata-action labels include OCI description"

  local labels
  labels=$(yq_raw '.runs.steps[] | select(.id == "meta") | .with.labels' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image meta labels include OCI description" "$labels" "org.opencontainers.image.description"

  local python venv service
  python=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with["labels-description"]' "$WORKFLOW" || true)
  venv=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with["labels-description"]' "$WORKFLOW" || true)
  service=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["labels-description"]' "$WORKFLOW" || true)
  assert_not_empty "build-python-base passes labels-description" "$python"
  assert_not_empty "build-venv-builder passes labels-description" "$venv"
  assert_not_empty "build-service passes labels-description" "$service"
}

# --- metadata-action labels include OCI licenses ---
test_metadata_action_labels_include_oci_licenses() {
  echo "Test: metadata-action labels include OCI licenses"

  local labels
  labels=$(yq_raw '.runs.steps[] | select(.id == "meta") | .with.labels' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image meta labels include Apache-2.0 license" "$labels" "org.opencontainers.image.licenses=Apache-2.0"
}

# --- metadata-action labels include OCI vendor ---
test_metadata_action_labels_include_oci_vendor() {
  echo "Test: metadata-action labels include OCI vendor"

  local labels
  labels=$(yq_raw '.runs.steps[] | select(.id == "meta") | .with.labels' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image meta labels include vendor" "$labels" "org.opencontainers.image.vendor"
}

# --- static OCI labels in python-base Dockerfile ---
test_dockerfile_static_labels_python_base() {
  echo "Test: python-base Dockerfile has static OCI labels"

  local dockerfile="$PROJECT_ROOT/images/python-base/Dockerfile"

  assert_file_contains "python-base has org.opencontainers.image.title" "$dockerfile" 'org.opencontainers.image.title='
  assert_file_contains "python-base has org.opencontainers.image.description" "$dockerfile" 'org.opencontainers.image.description='
  assert_file_contains "python-base has org.opencontainers.image.licenses" "$dockerfile" 'org.opencontainers.image.licenses='
  assert_file_contains "python-base has org.opencontainers.image.vendor" "$dockerfile" 'org.opencontainers.image.vendor='
}

# --- static OCI labels in venv-builder Dockerfile ---
test_dockerfile_static_labels_venv_builder() {
  echo "Test: venv-builder Dockerfile has static OCI labels"

  local dockerfile="$PROJECT_ROOT/images/venv-builder/Dockerfile"

  assert_file_contains "venv-builder has org.opencontainers.image.title" "$dockerfile" 'org.opencontainers.image.title='
  assert_file_contains "venv-builder has org.opencontainers.image.description" "$dockerfile" 'org.opencontainers.image.description='
  assert_file_contains "venv-builder has org.opencontainers.image.licenses" "$dockerfile" 'org.opencontainers.image.licenses='
  assert_file_contains "venv-builder has org.opencontainers.image.vendor" "$dockerfile" 'org.opencontainers.image.vendor='
}

# --- static OCI labels in keystone Dockerfile runtime stage ---
test_dockerfile_static_labels_keystone() {
  echo "Test: keystone Dockerfile has static OCI labels in runtime stage"

  local dockerfile="$PROJECT_ROOT/images/keystone/Dockerfile"

  # Extract only the runtime stage (Stage 2: from 'FROM python-base' to end of file)
  # to verify labels are in the correct stage, not the build stage.
  local runtime_stage
  runtime_stage=$(sed -n '/^FROM python-base/,$ p' "$dockerfile")

  assert_contains "keystone runtime stage has org.opencontainers.image.title" "$runtime_stage" 'org.opencontainers.image.title='
  assert_contains "keystone runtime stage has org.opencontainers.image.description" "$runtime_stage" 'org.opencontainers.image.description='
  assert_contains "keystone runtime stage has org.opencontainers.image.licenses" "$runtime_stage" 'org.opencontainers.image.licenses='
  assert_contains "keystone runtime stage has org.opencontainers.image.vendor" "$runtime_stage" 'org.opencontainers.image.vendor='
}

# --- cosign-installer step in setup-docker-registry composite ---
test_cosign_installer_in_build_base_images() {
  echo "Test: cosign-installer lives in setup-docker-registry composite"

  # cosign installer moved into the setup-docker-registry composite,
  # gated on the install-cosign input. merge-base-images omits install-cosign,
  # inheriting the default 'true'.
  local installer_count
  installer_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("sigstore/cosign-installer"))) | .uses' "$ACTION_SETUP_REGISTRY")
  assert_eq "setup-docker-registry has 1 cosign-installer step" "1" "$installer_count"

  local merge_base_install
  merge_base_install=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/setup-docker-registry"))) | .with["install-cosign"]' "$WORKFLOW" || echo "null")
  assert_eq "merge-base-images does not override install-cosign (default true)" "null" "$merge_base_install"
}

# --- cosign-installer gate for merge-service-images ---
test_cosign_installer_in_build_service_images() {
  echo "Test: merge-service-images installs cosign via setup-docker-registry"

  local merge_service_install
  merge_service_install=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("./.github/actions/setup-docker-registry"))) | .with["install-cosign"]' "$WORKFLOW" || echo "null")
  assert_eq "merge-service-images does not override install-cosign (default true)" "null" "$merge_service_install"
}

# --- cosign sign step lives in supply-chain-attest ---
test_cosign_sign_steps_count() {
  echo "Test: cosign sign step lives in supply-chain-attest composite"

  local sign_count
  sign_count=$(yq_count '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .run' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 1 cosign sign step" "1" "$sign_count"
}

# --- cosign sign gated by scan-mode sbom (skipped on PR) ---
test_cosign_sign_steps_pr_guard() {
  echo "Test: cosign sign guarded by scan-mode sbom"

  local sign_if
  sign_if=$(yq_raw '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .if' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest cosign sign step guarded by scan-mode sbom" "$sign_if" "scan-mode == 'sbom'"

  # merge-service-images has job-level PR guard.
  local merge_service_job_if
  merge_service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "merge-service-images job-level if excludes pull_request" "$merge_service_job_if" "event_name != 'pull_request'"
}

# --- cosign sign references composite image + digest inputs ---
test_cosign_sign_steps_reference_digest() {
  echo "Test: cosign sign references image + digest inputs"

  # supply-chain-attest signs "${IMAGE_NAME}@${IMAGE_DIGEST}" where
  # both env vars come from composite inputs.
  local sign_env_name sign_env_digest sign_run
  sign_env_name=$(yq_raw '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .env["IMAGE_NAME"]' "$ACTION_SUPPLY_CHAIN" || true)
  sign_env_digest=$(yq_raw '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .env["IMAGE_DIGEST"]' "$ACTION_SUPPLY_CHAIN" || true)
  sign_run=$(yq_raw '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .run' "$ACTION_SUPPLY_CHAIN" || true)

  assert_contains "cosign sign env IMAGE_NAME comes from composite input" "$sign_env_name" "inputs.image-name"
  assert_contains "cosign sign env IMAGE_DIGEST comes from composite input" "$sign_env_digest" "inputs.image-digest"
  assert_contains "cosign sign references image+digest reference" "$sign_run" 'IMAGE_NAME}@${IMAGE_DIGEST'
}

# --- cosign sign uses --yes flag ---
test_cosign_sign_uses_yes_flag() {
  echo "Test: cosign sign uses --yes flag"

  local sign_run
  sign_run=$(yq_raw '.runs.steps[] | select(.run and (.run | test("cosign sign"))) | .run' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest cosign sign uses --yes flag" "$sign_run" "--yes"
}

# =====================================================================
# Grype vulnerability scanning verification tests
# =====================================================================

# --- Grype scan primitive lives in supply-chain-attest ---
test_grype_scan_steps_in_build_base_images() {
  echo "Test: Grype scan primitives live in supply-chain-attest"

  # supply-chain-attest has two Grype scan steps, one for sbom mode
  # and one for image mode. Both merge-base-images and merge-service-images
  # reach them via merge-manifest-and-attest.
  local scan_count
  scan_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 2 Grype scan steps (sbom + image)" "2" "$scan_count"
}

# --- Grype scans also invoked for PR service builds via build-push-image ---
test_grype_scan_step_in_build_service_images() {
  echo "Test: build-push-image invokes supply-chain-attest for PR service scans"

  # build-push-image composite invokes supply-chain-attest in
  # scan-mode=image for PR builds of service images and tempest.
  local pr_scan_count
  pr_scan_count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("./.github/actions/supply-chain-attest"))) | .uses' "$ACTION_BUILD_PUSH")
  assert_eq "build-push-image invokes supply-chain-attest (PR scan branch)" "1" "$pr_scan_count"

  # The workflow passes a grype-category for build-service-images PR builds.
  local service_grype_cat
  service_grype_cat=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["grype-category"]' "$WORKFLOW" || true)
  assert_contains "build-service passes grype-category with matrix.service" "$service_grype_cat" "matrix.service"
}

# --- anchore/scan-action is SHA-pinned ---
test_grype_scan_action_sha_pinned() {
  echo "Test: anchore/scan-action is SHA-pinned"

  # anchore/scan-action pinning lives in the supply-chain-attest composite.
  local uses_values
  uses_values=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$ACTION_SUPPLY_CHAIN" || true)

  local all_pinned=true
  local found=false
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    found=true
    if [[ ! "$val" =~ anchore/scan-action@[0-9a-f]{40} ]]; then
      echo "  FAIL: anchore/scan-action not SHA-pinned: $val"
      FAIL=$((FAIL + 1))
      all_pinned=false
    fi
  done <<< "$uses_values"

  if $all_pinned && $found; then
    echo "  PASS: all anchore/scan-action uses are SHA-pinned"
    PASS=$((PASS + 1))
  elif ! $found; then
    echo "  FAIL: no anchore/scan-action uses found"
    FAIL=$((FAIL + 1))
  fi

  # Validate inline version comment.
  assert_file_contains "anchore/scan-action pin has # v7 version comment" "$ACTION_SUPPLY_CHAIN" "anchore/scan-action@[0-9a-f]\{40\}[[:space:]]*# v7"
}

# --- Grype scan covers both push (sbom) and PR (image) contexts ---
test_grype_scan_steps_cover_both_contexts() {
  echo "Test: Grype scan covers sbom (push) and image (PR) scan modes"

  # supply-chain-attest selects between grype-sbom and grype-image
  # via scan-mode. Verify both step ids exist and are gated on scan-mode.
  local sbom_if image_if
  sbom_if=$(yq_raw '.runs.steps[] | select(.id == "grype-sbom") | .if' "$ACTION_SUPPLY_CHAIN" || true)
  image_if=$(yq_raw '.runs.steps[] | select(.id == "grype-image") | .if' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "grype-sbom step guarded by scan-mode sbom" "$sbom_if" "scan-mode == 'sbom'"
  assert_contains "grype-image step guarded by scan-mode image" "$image_if" "scan-mode == 'image'"

  # merge-base-images selects scan-mode based on event_name.
  local python_mode venv_mode
  python_mode=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with["scan-mode"]' "$WORKFLOW" || true)
  venv_mode=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with["scan-mode"]' "$WORKFLOW" || true)
  assert_contains "python-base merge toggles scan-mode on event_name" "$python_mode" "event_name"
  assert_contains "venv-builder merge toggles scan-mode on event_name" "$venv_mode" "event_name"

  # merge-service-images is PR-gated at the job level; build-push-image
  # handles service-image PR scans in image mode.
  local service_merge_job_if
  service_merge_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "service SBOM scan has push-only guard (job-level)" "$service_merge_job_if" "!= 'pull_request'"

  local build_push_scan_if build_push_scan_mode
  build_push_scan_if=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("./.github/actions/supply-chain-attest"))) | .if' "$ACTION_BUILD_PUSH" || true)
  build_push_scan_mode=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("./.github/actions/supply-chain-attest"))) | .with["scan-mode"]' "$ACTION_BUILD_PUSH" || true)
  assert_contains "build-push-image supply-chain call guarded on push-by-digest=false (PR)" "$build_push_scan_if" "push-by-digest == 'false'"
  assert_eq "build-push-image supply-chain call uses scan-mode=image" "image" "$build_push_scan_mode"
}

# --- Grype scan SBOM input is wired through supply-chain-attest ---
test_grype_sbom_input_wiring() {
  echo "Test: Grype scan SBOM input wiring"

  # grype-sbom receives the SBOM file via composite input sbom-output-file.
  local sbom_input
  sbom_input=$(yq_raw '.runs.steps[] | select(.id == "grype-sbom") | .with.sbom' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "grype-sbom reads sbom-output-file input" "$sbom_input" "inputs.sbom-output-file"

  # Each call site provides the right per-image filename.
  local python_sbom venv_sbom service_sbom
  python_sbom=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with["sbom-output-file"]' "$WORKFLOW" || true)
  venv_sbom=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with["sbom-output-file"]' "$WORKFLOW" || true)
  service_sbom=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Merge and attest service image") | .with["sbom-output-file"]' "$WORKFLOW" || true)

  assert_contains "python-base call passes sbom-python-base.cyclonedx.json" "$python_sbom" "sbom-python-base.cyclonedx.json"
  assert_contains "venv-builder call passes sbom-venv-builder.cyclonedx.json" "$venv_sbom" "sbom-venv-builder.cyclonedx.json"
  assert_contains "service call passes sbom-<service>.cyclonedx.json" "$service_sbom" "matrix.service"
  assert_contains "service call SBOM filename is cyclonedx.json" "$service_sbom" "cyclonedx.json"

  # merge-service-images is PR-gated at the job level.
  local service_merge_if
  service_merge_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "service Grype sbom input has push-only conditional (job-level)" "$service_merge_if" "event_name != 'pull_request'"
}

# --- Grype scan image input wiring for PR context ---
test_grype_image_input_wiring() {
  echo "Test: Grype scan image input wiring for PR context"

  # grype-image step reads its image ref from the composite input image-ref-for-scan.
  local image_input
  image_input=$(yq_raw '.runs.steps[] | select(.id == "grype-image") | .with.image' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "grype-image reads image-ref-for-scan input" "$image_input" "inputs.image-ref-for-scan"

  # merge-manifest-and-attest composite derives the scan ref internally
  # from the freshly-merged digest, because a composite's callers cannot
  # self-reference steps.<id>.outputs within their own `with:` block.
  local composite_ref
  composite_ref=$(yq_raw '.runs.steps[] | select(.name == "Supply chain attest") | .with["image-ref-for-scan"]' "$ACTION_MERGE_MANIFEST" || true)
  assert_contains "merge composite derives scan ref from inputs.image" "$composite_ref" "inputs.image"
  assert_contains "merge composite derives scan ref from merged digest" "$composite_ref" "steps.merge.outputs.digest"

  # build-service-images forwards composite tag for PR image scans.
  local service_ref
  service_ref=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["image-ref-for-scan"]' "$WORKFLOW" || true)
  assert_contains "build-service passes composite tag as image-ref-for-scan" "$service_ref" "tags.outputs.composite"
}

# --- Grype scan severity threshold is high ---
test_grype_severity_threshold() {
  echo "Test: Grype scan severity-cutoff is high"

  local sbom_sev image_sev
  sbom_sev=$(yq_raw '.runs.steps[] | select(.id == "grype-sbom") | .with["severity-cutoff"]' "$ACTION_SUPPLY_CHAIN" || true)
  image_sev=$(yq_raw '.runs.steps[] | select(.id == "grype-image") | .with["severity-cutoff"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "grype-sbom severity-cutoff is high" "high" "$sbom_sev"
  assert_eq "grype-image severity-cutoff is high" "high" "$image_sev"
}

# --- Grype scan fail-build is false ---
test_grype_fail_build_false() {
  echo "Test: Grype scan fail-build is false"

  local sbom_fail image_fail
  sbom_fail=$(yq_raw '.runs.steps[] | select(.id == "grype-sbom") | .with["fail-build"]' "$ACTION_SUPPLY_CHAIN" || true)
  image_fail=$(yq_raw '.runs.steps[] | select(.id == "grype-image") | .with["fail-build"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "grype-sbom fail-build is false" "false" "$sbom_fail"
  assert_eq "grype-image fail-build is false" "false" "$image_fail"
}

# --- SARIF upload step lives in supply-chain-attest composite ---
test_sarif_upload_steps_exist() {
  echo "Test: SARIF upload step lives in supply-chain-attest"

  local count
  count=$(yq_count '.runs.steps[] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$ACTION_SUPPLY_CHAIN")
  assert_eq "supply-chain-attest has 1 SARIF upload step" "1" "$count"
}

# --- SARIF upload category is provided by the caller ---
test_sarif_upload_categories() {
  echo "Test: SARIF upload categories match image names"

  # category is forwarded from the composite input grype-category.
  local category
  category=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .with.category' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest upload-sarif category forwards input" "$category" "inputs.grype-category"

  # Each merge call site sets the right category.
  local python_cat venv_cat service_cat service_pr_cat
  python_cat=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest python-base") | .with["grype-category"]' "$WORKFLOW" || true)
  venv_cat=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Merge and attest venv-builder") | .with["grype-category"]' "$WORKFLOW" || true)
  service_cat=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Merge and attest service image") | .with["grype-category"]' "$WORKFLOW" || true)
  service_pr_cat=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["grype-category"]' "$WORKFLOW" || true)

  assert_eq "python-base SARIF category is grype-python-base" "grype-python-base" "$python_cat"
  assert_eq "venv-builder SARIF category is grype-venv-builder" "grype-venv-builder" "$venv_cat"
  assert_contains "service merge SARIF category references matrix.service" "$service_cat" "matrix.service"
  assert_contains "service PR SARIF category references matrix.service" "$service_pr_cat" "matrix.service"
}

# --- SARIF upload has if: always() with output guard ---
test_sarif_upload_always_condition() {
  echo "Test: SARIF upload has if: always() with output guard"

  local if_expr
  if_expr=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .if' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "supply-chain-attest SARIF upload has always() condition" "$if_expr" "always()"
  assert_contains "supply-chain-attest SARIF upload has output guard" "$if_expr" "outputs.sarif"
}

# --- upload-sarif action is SHA-pinned ---
test_sarif_upload_action_sha_pinned() {
  echo "Test: upload-sarif action is SHA-pinned"

  local uses
  uses=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$ACTION_SUPPLY_CHAIN" || true)

  if [ -z "$uses" ]; then
    echo "  FAIL: no upload-sarif action uses found"
    FAIL=$((FAIL + 1))
  elif [[ ! "$uses" =~ codeql-action/upload-sarif@[0-9a-f]{40} ]]; then
    echo "  FAIL: upload-sarif action not SHA-pinned: $uses"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: upload-sarif action is SHA-pinned"
    PASS=$((PASS + 1))
  fi

  # Validate inline version comment.
  assert_file_contains "upload-sarif pin has # v4 version comment" "$ACTION_SUPPLY_CHAIN" "codeql-action/upload-sarif@[0-9a-f]\{40\}[[:space:]]*# v4"
}

# --- security-events permission on merge-base-images ---
test_security_events_permission_build_base_images() {
  echo "Test: merge-base-images has security-events: write permission"

  local perm
  perm=$(yq_raw '.jobs["merge-base-images"]["permissions"]["security-events"]' "$WORKFLOW" || true)
  assert_eq "merge-base-images security-events permission is write" "write" "$perm"
}

# --- security-events permission on merge-service-images ---
test_security_events_permission_build_service_images() {
  echo "Test: merge-service-images has security-events: write permission"

  local perm
  perm=$(yq_raw '.jobs["merge-service-images"]["permissions"]["security-events"]' "$WORKFLOW" || true)
  assert_eq "merge-service-images security-events permission is write" "write" "$perm"
}

# --- verify jobs do NOT have security-events permission ---
test_verify_jobs_no_security_events_permission() {
  echo "Test: verify jobs do not have security-events permission"

  local verify_base_perm
  verify_base_perm=$(yq_raw '.jobs["verify-base-images"]["permissions"]["security-events"] // "null"' "$WORKFLOW" || true)
  assert_eq "verify-base-images has no security-events permission" "null" "$verify_base_perm"

  local verify_service_perm
  verify_service_perm=$(yq_raw '.jobs["verify-service-images"]["permissions"]["security-events"] // "null"' "$WORKFLOW" || true)
  assert_eq "verify-service-images has no security-events permission" "null" "$verify_service_perm"

  local test_service_perm
  test_service_perm=$(yq_raw '.jobs["test-service-images"]["permissions"]["security-events"] // "null"' "$WORKFLOW" || true)
  assert_eq "test-service-images has no security-events permission" "null" "$test_service_perm"
}

# --- Grype scan output format is sarif ---
test_grype_output_format_sarif() {
  echo "Test: Grype scan output-format is sarif"

  local sbom_format image_format
  sbom_format=$(yq_raw '.runs.steps[] | select(.id == "grype-sbom") | .with["output-format"]' "$ACTION_SUPPLY_CHAIN" || true)
  image_format=$(yq_raw '.runs.steps[] | select(.id == "grype-image") | .with["output-format"]' "$ACTION_SUPPLY_CHAIN" || true)
  assert_eq "grype-sbom output-format is sarif" "sarif" "$sbom_format"
  assert_eq "grype-image output-format is sarif" "sarif" "$image_format"
}

# --- SARIF upload references Grype step output ---
test_sarif_upload_references_grype_output() {
  echo "Test: SARIF upload sarif_file references Grype step output"

  # the SARIF upload step in supply-chain-attest picks whichever of
  # grype-sbom.outputs.sarif or grype-image.outputs.sarif is populated.
  local sarif_file
  sarif_file=$(yq_raw '.runs.steps[] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .with.sarif_file' "$ACTION_SUPPLY_CHAIN" || true)
  assert_contains "SARIF upload references grype-sbom output" "$sarif_file" "grype-sbom.outputs.sarif"
  assert_contains "SARIF upload references grype-image output" "$sarif_file" "grype-image.outputs.sarif"
}

# --- Run all tests ---
echo "=== Build images workflow verification tests ==="
echo ""
test_spdx_header_present
echo ""
test_quoted_on_key
echo ""
test_permissions_block
echo ""
test_job_permissions_scoping
echo ""
test_concurrency_control
echo ""
test_push_triggers
echo ""
test_pull_request_trigger
echo ""
test_five_jobs_defined
echo ""
test_verify_base_images_job
echo ""
test_base_images_multi_arch
echo ""
test_base_image_digest_outputs
echo ""
test_keystone_federation_proxy_jobs
echo ""
test_service_images_depend_on_base
echo ""
test_matrix_includes_service_and_release
echo ""
test_source_ref_resolution_step
echo ""
test_patch_application_step
echo ""
test_constraint_overrides_step
echo ""
test_build_contexts_for_service_images
echo ""
test_tag_schema_composite
echo ""
test_branch_sanitization
echo ""
test_version_and_sha_outputs
echo ""
test_pr_single_arch_load
echo ""
test_verify_service_images_command
echo ""
test_verify_service_images_depends_on_service_images
echo ""
test_verify_service_images_has_matrix
echo ""
test_verify_service_images_derives_image_ref
echo ""
test_actions_pinned_to_sha
echo ""
test_actions_have_version_comments
echo ""
test_gha_caching_present
echo ""
test_timeout_minutes_on_all_jobs
echo ""
test_runs_on_ubuntu_latest
echo ""
test_base_images_always_push
echo ""
test_run_blocks_use_env_vars
echo ""
test_fork_pr_rejection_step_exists
echo ""
test_base_images_have_sha_tags
echo ""
test_version_tag_restricted_to_main
echo ""
test_matrix_jobs_fail_fast_false
echo ""
test_source_ref_null_guard
echo ""
test_verify_service_images_tag_derivation_sync_comment
echo ""
test_verify_service_images_null_guard
echo ""
test_test_service_images_job_structure
echo ""
test_test_service_images_depends_on_base
echo ""
test_test_service_images_has_matrix
echo ""
test_test_service_images_uses_venv_builder_output
echo ""
test_test_service_images_source_ref_step
echo ""
test_test_service_images_checkout_service_source
echo ""
test_test_service_images_apply_patches
echo ""
test_test_service_images_constraint_overrides
echo ""
test_test_service_images_run_tests_volumes
echo ""
test_test_service_images_run_tests_stestr
echo ""
test_test_service_images_exclude_list
echo ""
test_test_service_images_subunit_output
echo ""
test_test_service_images_upload_artifacts
echo ""
test_test_service_images_artifact_name
echo ""
test_test_service_images_env_vars
echo ""
test_test_service_images_docker_run
echo ""
test_sbom_permissions_on_build_base_images
echo ""
test_sbom_permissions_on_build_service_images
echo ""
test_verify_jobs_no_sbom_permissions
echo ""
test_sbom_generation_steps_exist
echo ""
test_sbom_format_cyclonedx_json
echo ""
test_sbom_no_artifact_upload
echo ""
test_sbom_generation_references_digest
echo ""
test_sbom_attestation_steps_exist
echo ""
test_sbom_attestation_push_to_registry
echo ""
test_sbom_steps_pr_skip_guard
echo ""
test_build_provenance_steps_exist
echo ""
test_build_provenance_steps_pr_skip_guard
echo ""
test_build_provenance_push_to_registry
echo ""
test_metadata_action_steps_exist_in_build_base_images
echo ""
test_metadata_action_step_exists_in_build_service_images
echo ""
test_service_metadata_uses_raw_version_strategy
echo ""
test_base_metadata_steps_have_no_tags_override
echo ""
test_python_base_build_push_has_labels_input
echo ""
test_venv_builder_build_push_has_labels_input
echo ""
test_service_build_push_has_labels_input
echo ""
test_oci_base_labels_in_build_steps
echo ""
test_metadata_action_labels_include_oci_title
echo ""
test_metadata_action_labels_include_oci_description
echo ""
test_metadata_action_labels_include_oci_licenses
echo ""
test_metadata_action_labels_include_oci_vendor
echo ""
test_dockerfile_static_labels_python_base
echo ""
test_dockerfile_static_labels_venv_builder
echo ""
test_dockerfile_static_labels_keystone
echo ""
test_cosign_installer_in_build_base_images
echo ""
test_cosign_installer_in_build_service_images
echo ""
test_cosign_sign_steps_count
echo ""
test_cosign_sign_steps_pr_guard
echo ""
test_cosign_sign_steps_reference_digest
echo ""
test_cosign_sign_uses_yes_flag
echo ""
test_grype_scan_steps_in_build_base_images
echo ""
test_grype_scan_step_in_build_service_images
echo ""
test_grype_scan_action_sha_pinned
echo ""
test_grype_scan_steps_cover_both_contexts
echo ""
test_grype_sbom_input_wiring
echo ""
test_grype_image_input_wiring
echo ""
test_grype_severity_threshold
echo ""
test_grype_fail_build_false
echo ""
test_sarif_upload_steps_exist
echo ""
test_sarif_upload_categories
echo ""
test_sarif_upload_always_condition
echo ""
test_sarif_upload_action_sha_pinned
echo ""
test_security_events_permission_build_base_images
echo ""
test_security_events_permission_build_service_images
echo ""
test_verify_jobs_no_security_events_permission
echo ""
test_grype_output_format_sarif
echo ""
test_sarif_upload_references_grype_output
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
