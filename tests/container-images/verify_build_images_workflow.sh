#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build-images workflow structure, conventions, and correctness (CC-0007, CC-0029, CC-0030, CC-0031, CC-0032, CC-0034)
# Requirements: REQ-001 through REQ-025
# Usage: bash tests/container-images/verify_build_images_workflow.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKFLOW="$PROJECT_ROOT/.github/workflows/build-images.yaml"

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

# --- REQ-008: SPDX header matching ci.yaml convention ---
test_spdx_header_present() {
  echo "Test: SPDX header present (REQ-008)"

  local line1 line3
  line1=$(sed -n '1p' "$WORKFLOW")
  line3=$(sed -n '3p' "$WORKFLOW")

  assert_contains "line 1 has SPDX-FileCopyrightText" "$line1" "SPDX-FileCopyrightText"
  assert_contains "line 3 has SPDX-License-Identifier" "$line3" "SPDX-License-Identifier"
}

# --- REQ-008: Trigger key quoted to prevent YAML boolean interpretation ---
test_quoted_on_key() {
  echo "Test: trigger key is quoted as '\"on\"' (REQ-008)"

  assert_file_contains "workflow has quoted on key" "$WORKFLOW" '"on"'
}

# --- REQ-008: Top-level permissions block ---
test_permissions_block() {
  echo "Test: top-level permissions block (REQ-008)"

  # Top-level permissions should have contents: read only (least privilege)
  local top_perms
  top_perms=$(yq_raw '.permissions' "$WORKFLOW" || true)

  assert_contains "top-level permissions has contents: read" "$top_perms" "read"
}

# --- REQ-008: Job-level permissions scoping ---
test_job_permissions_scoping() {
  echo "Test: job-level permissions scoping (REQ-008)"

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

# --- REQ-008: Concurrency control ---
test_concurrency_control() {
  echo "Test: concurrency control (REQ-008)"

  assert_file_contains "concurrency group pattern" "$WORKFLOW" 'github.ref.*github.workflow'
}

# --- REQ-001: Push triggers include main and stable/** ---
test_push_triggers() {
  echo "Test: push triggers include main and stable/** (REQ-001)"

  assert_file_contains "push trigger includes main" "$WORKFLOW" "main"
  assert_file_contains "push trigger includes stable/**" "$WORKFLOW" "stable/\*\*"
}

# --- REQ-001: pull_request trigger present ---
test_pull_request_trigger() {
  echo "Test: pull_request trigger present (REQ-001)"

  assert_file_contains "pull_request trigger present" "$WORKFLOW" "pull_request"
}

# --- REQ-002, REQ-003, REQ-004, REQ-005, REQ-007: Seven jobs defined ---
test_five_jobs_defined() {
  echo "Test: seven jobs defined (REQ-002, REQ-003, REQ-004, REQ-005, CC-0034 REQ-001)"

  assert_file_contains "build-base-images job defined" "$WORKFLOW" "build-base-images:"
  assert_file_contains "merge-base-images job defined" "$WORKFLOW" "merge-base-images:"
  assert_file_contains "verify-base-images job defined" "$WORKFLOW" "verify-base-images:"
  assert_file_contains "build-service-images job defined" "$WORKFLOW" "build-service-images:"
  assert_file_contains "merge-service-images job defined" "$WORKFLOW" "merge-service-images:"
  assert_file_contains "test-service-images job defined" "$WORKFLOW" "test-service-images:"
  assert_file_contains "verify-service-images job defined" "$WORKFLOW" "verify-service-images:"
}

# --- REQ-004: verify-base-images job depends on build-base-images ---
test_verify_base_images_job() {
  echo "Test: verify-base-images job structure (REQ-004)"

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

# --- REQ-002: Base images build with multi-arch platforms ---
test_base_images_multi_arch() {
  echo "Test: base images use multi-arch platforms (REQ-002)"

  local matrix_platforms
  matrix_platforms=$(yq_raw '.jobs["build-base-images"]["strategy"]["matrix"]["include"][]["platform"]' "$WORKFLOW" || true)
  assert_contains "build-base-images matrix includes linux/amd64" "$matrix_platforms" "linux/amd64"
  assert_contains "build-base-images matrix includes linux/arm64" "$matrix_platforms" "linux/arm64"
}

# --- REQ-003: Base image outputs contain digest references ---
test_base_image_digest_outputs() {
  echo "Test: base image outputs contain digest references (REQ-003)"

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

# --- REQ-003, REQ-004: build-service-images depends on build-base-images and verify-base-images ---
test_service_images_depend_on_base() {
  echo "Test: build-service-images depends on build-base-images and verify-base-images (REQ-003, REQ-004)"

  local needs
  needs=$(yq_raw '.jobs["build-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "build-service-images needs merge-base-images" "$needs" "merge-base-images"
  assert_contains "build-service-images needs verify-base-images" "$needs" "verify-base-images"
  assert_contains "build-service-images needs generate-matrix" "$needs" "generate-matrix"
}

# --- REQ-004: Matrix is dynamic via fromJson ---
test_matrix_includes_service_and_release() {
  echo "Test: matrix is dynamic via fromJson(needs.generate-matrix.outputs.matrix) (REQ-004)"

  local matrix_expr
  matrix_expr=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "build-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "build-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"

  local gen_matrix_output
  gen_matrix_output=$(yq_raw '.jobs["generate-matrix"]["outputs"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "generate-matrix job exposes matrix output" "$gen_matrix_output" "matrix"
}

# --- REQ-004: Source ref resolution step exists ---
test_source_ref_resolution_step() {
  echo "Test: source ref resolution step with yq (REQ-004)"

  local source_ref_step
  source_ref_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "source-ref") | .run' "$WORKFLOW" || true)

  assert_contains "source-ref step uses yq to resolve ref" "$source_ref_step" "yq"
  assert_contains "source-ref step reads source-refs.yaml" "$source_ref_step" "source-refs.yaml"
}

# --- REQ-004: Conditional patch application step ---
test_patch_application_step() {
  echo "Test: conditional patch application with hashFiles guard (REQ-004)"

  local patch_if
  patch_if=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name == "Apply patches") | .if' "$WORKFLOW" || true)

  assert_contains "patch step uses hashFiles guard" "$patch_if" "hashFiles"
}

# --- REQ-004: Constraint overrides step ---
test_constraint_overrides_step() {
  echo "Test: constraint overrides step references apply-constraint-overrides.sh (REQ-004)"

  local overrides_run
  overrides_run=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name == "Apply constraint overrides") | .run' "$WORKFLOW" || true)

  assert_contains "constraint overrides step runs apply-constraint-overrides.sh" "$overrides_run" "apply-constraint-overrides.sh"
}

# --- REQ-004: Four build-contexts for service images ---
test_build_contexts_for_service_images() {
  echo "Test: build-contexts for service images (REQ-004)"

  local build_contexts
  build_contexts=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["build-contexts"]' "$WORKFLOW" || true)

  assert_contains "build-context includes python-base" "$build_contexts" "python-base="
  assert_contains "build-context includes venv-builder" "$build_contexts" "venv-builder="
  assert_contains "build-context includes service source" "$build_contexts" "matrix.service"
  assert_contains "build-context includes upper-constraints" "$build_contexts" "upper-constraints="
}

# --- REQ-005: Tag schema composite ---
test_tag_schema_composite() {
  echo "Test: tag schema composite (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "Derive tags uses composite action" "$tags_step" "./.github/actions/derive-service-tags"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action has VERSION component" "$action_file" 'VERSION'
  assert_file_contains "composite action has PATCH_COUNT component" "$action_file" 'PATCH_COUNT'
  assert_file_contains "composite action has BRANCH component" "$action_file" 'BRANCH'
  assert_file_contains "composite action has SHORT_SHA component" "$action_file" 'SHORT_SHA'
}

# --- REQ-005: Branch sanitization (slash-to-dash) ---
test_branch_sanitization() {
  echo "Test: branch sanitization replaces slashes with dashes (REQ-005)"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action has slash-to-dash replacement" "$action_file" 'GITHUB_REF_NAME//\\\//-'
}

# --- REQ-005: Version and SHA tag outputs emitted ---
test_version_and_sha_outputs() {
  echo "Test: version= and sha= outputs emitted (REQ-005)"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action emits version output" "$action_file" '"version='
  assert_file_contains "composite action emits sha output" "$action_file" '"sha='
  assert_file_contains "composite action emits image output" "$action_file" '"image='
}

# --- REQ-006: PR uses single-arch, load, and conditional push/platforms ---
test_pr_single_arch_load() {
  echo "Test: PR uses single-arch, load, and conditional push/platforms (REQ-006)"

  local build_matrix_run
  build_matrix_run=$(yq_raw '.jobs["generate-matrix"]["steps"][] | select(.id == "matrix") | .run' "$WORKFLOW" || true)
  assert_contains "build-matrix excludes ARM64 on pull_request" "$build_matrix_run" "pull_request"
  assert_contains "build-matrix includes linux/arm64 on push" "$build_matrix_run" "linux/arm64"

  local load_val
  load_val=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.load' "$WORKFLOW" || true)
  assert_contains "load conditioned on pull_request" "$load_val" "pull_request"
}

# --- REQ-007: verify-service-images uses verify_<service>.sh via matrix ---
test_verify_service_images_command() {
  echo "Test: verify-service-images uses verify script via matrix.service (REQ-007)"

  # verify-service-images uses MATRIX_SERVICE env var for tag derivation
  assert_file_contains "verify-service-images uses MATRIX_SERVICE env var" "$WORKFLOW" 'MATRIX_SERVICE: \${{ matrix.service }}'
  assert_file_contains "verify-service-images runs verify script via matrix.service" "$WORKFLOW" 'verify_${{ matrix.service }}.sh'
}

# --- REQ-007, CC-0034 REQ-007: verify-service-images depends on build-service-images and test-service-images ---
test_verify_service_images_depends_on_service_images() {
  echo "Test: verify-service-images depends on build-service-images and test-service-images (REQ-007, CC-0034 REQ-007)"

  local needs
  needs=$(yq_raw '.jobs["verify-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "verify-service-images needs merge-service-images" "$needs" "merge-service-images"
  assert_contains "verify-service-images needs test-service-images" "$needs" "test-service-images"
  assert_contains "verify-service-images needs generate-matrix" "$needs" "generate-matrix"
}

# --- REQ-007: verify-service-images uses dynamic matrix via fromJson ---
test_verify_service_images_has_matrix() {
  echo "Test: verify-service-images uses dynamic matrix via fromJson (REQ-007)"

  local matrix_expr
  matrix_expr=$(yq_raw '.jobs["verify-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)

  assert_contains "verify-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "verify-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"
}

# --- REQ-007: verify-service-images derives image ref independently ---
test_verify_service_images_derives_image_ref() {
  echo "Test: verify-service-images derives image ref via tags step (REQ-007)"

  local tags_uses
  tags_uses=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "verify-service-images uses derive-service-tags composite action" "$tags_uses" "./.github/actions/derive-service-tags"
}

# --- REQ-008: All actions pinned to 40-char hex SHA ---
test_actions_pinned_to_sha() {
  echo "Test: all actions pinned to SHA (REQ-008)"

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

# --- REQ-008: SHA-pinned actions have version comments ---
test_actions_have_version_comments() {
  echo "Test: SHA-pinned actions have version comments (REQ-008)"

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

# --- REQ-009: GHA caching present (cache-from and cache-to) ---
test_gha_caching_present() {
  echo "Test: GHA caching present (REQ-009)"

  assert_file_contains "cache-from: type=gha present" "$WORKFLOW" "cache-from: type=gha"
  assert_file_contains "cache-to: type=gha present" "$WORKFLOW" "cache-to: type=gha"
}

# --- REQ-008: All jobs have timeout-minutes ---
test_timeout_minutes_on_all_jobs() {
  echo "Test: all jobs have timeout-minutes (REQ-008, CC-0034 REQ-012)"

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
}

# --- REQ-008: All jobs use runs-on: ubuntu-latest ---
test_runs_on_ubuntu_latest() {
  echo "Test: all jobs use runs-on: ubuntu-latest (REQ-008, CC-0034 REQ-012)"

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
}

# --- REQ-002: Base images always push unconditionally ---
test_base_images_always_push() {
  echo "Test: base images always push unconditionally (REQ-002)"

  local outputs_values
  outputs_values=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.with.outputs) | .with.outputs' "$WORKFLOW" || true)
  assert_contains "build-base-images uses push-by-digest=true" "$outputs_values" "push-by-digest=true"
  assert_contains "build-base-images push-by-digest output includes push=true" "$outputs_values" "push=true"
}

# --- CC-0007: Fork PRs rejected in build-base-images ---
test_fork_pr_rejection_step_exists() {
  echo "Test: fork PR rejection step exists in build-base-images (CC-0007)"

  local reject_step_if
  reject_step_if=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Reject fork PRs") | .if' "$WORKFLOW" || true)

  assert_contains "Reject fork PRs step exists with pull_request condition" "$reject_step_if" "pull_request"
  assert_contains "Reject fork PRs step checks head repo full_name" "$reject_step_if" "github.event.pull_request.head.repo.full_name"
  assert_contains "Reject fork PRs step compares against github.repository" "$reject_step_if" "github.repository"
}

# --- CC-0007: Base images have immutable SHA tags alongside :latest ---
test_base_images_have_sha_tags() {
  echo "Test: base images have SHA tags for commit traceability (CC-0007)"

  # In the push-by-digest architecture, SHA tags are applied in merge-base-images via imagetools create
  local python_manifest venv_manifest
  python_manifest=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Create and push python-base manifest") | .run' "$WORKFLOW" || true)
  venv_manifest=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Create and push venv-builder manifest") | .run' "$WORKFLOW" || true)

  assert_contains "python-base tags include github.sha" "$python_manifest" 'github.sha'
  assert_contains "venv-builder tags include github.sha" "$venv_manifest" 'github.sha'
}

# --- CC-0007: Version-only tag restricted to main branch ---
test_version_tag_restricted_to_main() {
  echo "Test: version-only tag restricted to main branch (CC-0007)"

  # In the push-by-digest architecture, the version tag conditional lives in the
  # merge-service-images manifest creation step's run script (bash GITHUB_REF_NAME check)
  local manifest_run
  manifest_run=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Create and push service image manifest") | .run' "$WORKFLOW" || true)

  assert_contains "version tag line contains ref_name == main conditional" "$manifest_run" 'GITHUB_REF_NAME}" == "main"'
}

# --- CC-0007: Matrix jobs use fail-fast: false for independent failure reporting ---
test_matrix_jobs_fail_fast_false() {
  echo "Test: matrix jobs use fail-fast: false (CC-0007, CC-0034 REQ-012)"

  local service_fail_fast test_service_fail_fast verify_service_fail_fast
  service_fail_fast=$(yq_raw '.jobs["build-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")
  test_service_fail_fast=$(yq_raw '.jobs["test-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")
  verify_service_fail_fast=$(yq_raw '.jobs["verify-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")

  assert_eq "build-service-images has fail-fast: false" "false" "$service_fail_fast"
  assert_eq "test-service-images has fail-fast: false" "false" "$test_service_fail_fast"
  assert_eq "verify-service-images has fail-fast: false" "false" "$verify_service_fail_fast"
}

# --- CC-0007: Source ref resolution validates yq output against null/empty ---
test_source_ref_null_guard() {
  echo "Test: source-ref step validates yq output against null/empty (CC-0007)"

  local source_ref_run
  source_ref_run=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "source-ref") | .run' "$WORKFLOW" || true)

  assert_contains "source-ref step checks for null string" "$source_ref_run" '"null"'
  assert_contains "source-ref step checks for empty value" "$source_ref_run" '-z "$ref"'
  assert_contains "source-ref step exits on invalid ref" "$source_ref_run" "exit 1"
}

# --- CC-0007: all three tag-deriving jobs use the same composite action ---
test_verify_service_images_tag_derivation_sync_comment() {
  echo "Test: all tag-deriving jobs use derive-service-tags composite action (CC-0007)"

  local build_uses merge_uses verify_uses
  build_uses=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  merge_uses=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  verify_uses=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .uses' "$WORKFLOW" || true)
  assert_contains "build-service-images uses derive-service-tags" "$build_uses" "./.github/actions/derive-service-tags"
  assert_contains "merge-service-images uses derive-service-tags" "$merge_uses" "./.github/actions/derive-service-tags"
  assert_contains "verify-service-images uses derive-service-tags" "$verify_uses" "./.github/actions/derive-service-tags"
}

# --- CC-0007: composite action validates yq output against null/empty ---
test_verify_service_images_null_guard() {
  echo "Test: composite action validates yq output against null/empty (CC-0007)"

  local action_file=".github/actions/derive-service-tags/action.yaml"
  assert_file_contains "composite action checks for null string" "$action_file" '"null"'
  assert_file_contains "composite action exits on invalid ref" "$action_file" "exit 1"
}

# ===========================================================================================
# CC-0034: test-service-images job tests
# ===========================================================================================

# --- CC-0034 REQ-001: test-service-images job exists and has correct structure ---
test_test_service_images_job_structure() {
  echo "Test: test-service-images job structure (CC-0034 REQ-001, REQ-002, REQ-012)"

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

  # Validate absence of elevated permissions (CC-0034 REQ-012)
  local id_token attestations security_events
  id_token=$(yq_raw '.jobs["test-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["test-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")
  security_events=$(yq_raw '.jobs["test-service-images"]["permissions"]["security-events"]' "$WORKFLOW" || echo "null")

  assert_eq "test-service-images has no id-token permission" "null" "$id_token"
  assert_eq "test-service-images has no attestations permission" "null" "$attestations"
  assert_eq "test-service-images has no security-events permission" "null" "$security_events"
}

# --- CC-0034 REQ-002: test-service-images depends on build-base-images and verify-base-images ---
test_test_service_images_depends_on_base() {
  echo "Test: test-service-images depends on build-base-images and verify-base-images (CC-0034 REQ-002)"

  local needs
  needs=$(yq_raw '.jobs["test-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "test-service-images needs merge-base-images" "$needs" "merge-base-images"
  assert_contains "test-service-images needs verify-base-images" "$needs" "verify-base-images"
}

# --- CC-0034 REQ-001: test-service-images has matrix strategy matching build-service-images ---
test_test_service_images_has_matrix() {
  echo "Test: test-service-images has matrix strategy (CC-0034 REQ-001)"

  local matrix_expr fail_fast
  matrix_expr=$(yq_raw '.jobs["test-service-images"]["strategy"]["matrix"]' "$WORKFLOW" || true)
  fail_fast=$(yq_raw '.jobs["test-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")

  assert_contains "test-service-images matrix uses fromJson" "$matrix_expr" "fromJson"
  assert_contains "test-service-images matrix reads generate-matrix output" "$matrix_expr" "generate-matrix"
  assert_eq "test-service-images has fail-fast: false" "false" "$fail_fast"
}

# --- CC-0034 REQ-011: test-service-images uses venv-builder-image from build-base-images outputs ---
test_test_service_images_uses_venv_builder_output() {
  echo "Test: test-service-images uses venv-builder-image output (CC-0034 REQ-011)"

  local run_step_env
  run_step_env=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["VENV_BUILDER_IMAGE"]' "$WORKFLOW" || true)

  assert_contains "Run tests env references venv-builder-image output" "$run_step_env" "needs.merge-base-images.outputs.venv-builder-image"
}

# --- CC-0034 REQ-003: test-service-images resolves source ref from source-refs.yaml ---
test_test_service_images_source_ref_step() {
  echo "Test: test-service-images has source-ref resolution step (CC-0034 REQ-003)"

  local source_ref_run
  source_ref_run=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "source-ref") | .run' "$WORKFLOW" || true)

  assert_not_empty "test-service-images has source-ref step" "$source_ref_run"
  assert_contains "source-ref step reads source-refs.yaml" "$source_ref_run" "source-refs.yaml"
  assert_contains "source-ref step checks for null" "$source_ref_run" '"null"'
  assert_contains "source-ref step checks for empty value" "$source_ref_run" '-z "$ref"'
  assert_contains "source-ref step exits on invalid ref" "$source_ref_run" "exit 1"
}

# --- CC-0034 REQ-003: test-service-images checks out service source at correct ref ---
test_test_service_images_checkout_service_source() {
  echo "Test: test-service-images checks out service source (CC-0034 REQ-003)"

  local checkout_repo checkout_ref checkout_path
  checkout_repo=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.with.repository) | .with.repository' "$WORKFLOW" || true)
  checkout_ref=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.with.repository) | .with.ref' "$WORKFLOW" || true)
  checkout_path=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.with.repository) | .with.path' "$WORKFLOW" || true)

  assert_contains "service checkout uses openstack/ repo" "$checkout_repo" "openstack/"
  assert_contains "service checkout uses source-ref output" "$checkout_ref" "steps.source-ref.outputs.ref"
  assert_contains "service checkout path includes matrix.service" "$checkout_path" "matrix.service"
}

# --- CC-0034 REQ-004: test-service-images applies patches with hashFiles guard ---
test_test_service_images_apply_patches() {
  echo "Test: test-service-images applies patches with hashFiles guard (CC-0034 REQ-004)"

  local apply_step_if apply_step_run
  apply_step_if=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Apply patches") | .if' "$WORKFLOW" || true)
  apply_step_run=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Apply patches") | .run' "$WORKFLOW" || true)

  assert_contains "Apply patches uses hashFiles guard" "$apply_step_if" "hashFiles"
  assert_contains "Apply patches uses hashFiles with .patch pattern" "$apply_step_if" ".patch"
  assert_contains "Apply patches runs git apply" "$apply_step_run" "git -C"
}

# --- CC-0034 REQ-004: test-service-images applies constraint overrides ---
test_test_service_images_constraint_overrides() {
  echo "Test: test-service-images applies constraint overrides (CC-0034 REQ-004)"

  local override_step_run
  override_step_run=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Apply constraint overrides") | .run' "$WORKFLOW" || true)

  assert_contains "constraint overrides step runs apply-constraint-overrides.sh" "$override_step_run" "apply-constraint-overrides.sh"
}

# --- CC-0034 REQ-005: test-service-images Run tests step mounts correct volumes ---
test_test_service_images_run_tests_volumes() {
  echo "Test: test-service-images Run tests step mounts correct volumes (CC-0034 REQ-005)"

  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)

  assert_contains "Run tests mounts service source" "$run_step" "/workspace/src"
  assert_contains "Run tests mounts upper-constraints.txt" "$run_step" "upper-constraints.txt"
  assert_contains "Run tests mounts test-excludes directory" "$run_step" "test-excludes"
  assert_contains "Run tests mounts results directory" "$run_step" "/workspace/results"
}

# --- CC-0034 REQ-005: test-service-images Run tests step runs stestr ---
test_test_service_images_run_tests_stestr() {
  echo "Test: test-service-images Run tests step runs stestr (CC-0034 REQ-005)"

  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)

  assert_contains "Run tests installs test dependencies with pip" "$run_step" "pip install"
  assert_contains "Run tests installs stestr" "$run_step" "stestr"
  assert_contains "Run tests runs stestr init" "$run_step" "stestr init"
  assert_contains "Run tests runs stestr run" "$run_step" "stestr run"
}

# --- CC-0034 REQ-005: test-service-images uses exclude-list from test-excludes ---
test_test_service_images_exclude_list() {
  echo "Test: test-service-images uses exclude-list from test-excludes (CC-0034 REQ-005)"

  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)

  assert_contains "Run tests builds EXCLUDE_LIST_ARG" "$run_step" "EXCLUDE_LIST_ARG"
  assert_contains "Run tests checks for service-specific exclude file" "$run_step" "test-excludes/\${MATRIX_SERVICE}.txt"
  assert_contains "Run tests passes exclude-list to stestr" "$run_step" "--exclude-list"
}

# --- CC-0034 REQ-005: test-service-images exports subunit results ---
test_test_service_images_subunit_output() {
  echo "Test: test-service-images exports subunit test results (CC-0034 REQ-005)"

  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)

  assert_contains "Run tests exports subunit results" "$run_step" "stestr last --subunit"
  assert_contains "Run tests writes results to subunit file" "$run_step" "testresults.subunit"
}

# --- CC-0034 REQ-006: test-service-images uploads test results as artifacts ---
test_test_service_images_upload_artifacts() {
  echo "Test: test-service-images uploads test results as artifacts (CC-0034 REQ-006)"

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

# --- CC-0034 REQ-006: artifact name includes matrix.service for disambiguation ---
test_test_service_images_artifact_name() {
  echo "Test: test-service-images artifact name includes matrix.service (CC-0034 REQ-006)"

  local artifact_name
  artifact_name=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Upload test results") | .with.name' "$WORKFLOW" || true)

  assert_contains "artifact name includes matrix.service" "$artifact_name" "matrix.service"
  assert_contains "artifact name includes matrix.release" "$artifact_name" "matrix.release"
}

# --- CC-0034 REQ-010: test-service-images env vars prevent expression injection ---
test_test_service_images_env_vars() {
  echo "Test: test-service-images steps use env vars for matrix values (CC-0034 REQ-010)"

  # Source-ref step uses MATRIX_SERVICE and MATRIX_RELEASE env vars
  local source_ref_env_service source_ref_env_release
  source_ref_env_service=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "source-ref") | .env["MATRIX_SERVICE"]' "$WORKFLOW" || true)
  source_ref_env_release=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "source-ref") | .env["MATRIX_RELEASE"]' "$WORKFLOW" || true)

  assert_contains "source-ref step has MATRIX_SERVICE env" "$source_ref_env_service" "matrix.service"
  assert_contains "source-ref step has MATRIX_RELEASE env" "$source_ref_env_release" "matrix.release"

  # Apply patches step uses env vars
  local patches_env_service patches_env_release
  patches_env_service=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Apply patches") | .env["MATRIX_SERVICE"]' "$WORKFLOW" || true)
  patches_env_release=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Apply patches") | .env["MATRIX_RELEASE"]' "$WORKFLOW" || true)

  assert_contains "Apply patches step has MATRIX_SERVICE env" "$patches_env_service" "matrix.service"
  assert_contains "Apply patches step has MATRIX_RELEASE env" "$patches_env_release" "matrix.release"

  # Run tests step uses env vars
  local run_tests_env_service run_tests_env_release
  run_tests_env_service=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["MATRIX_SERVICE"]' "$WORKFLOW" || true)
  run_tests_env_release=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["MATRIX_RELEASE"]' "$WORKFLOW" || true)

  assert_contains "Run tests step has MATRIX_SERVICE env" "$run_tests_env_service" "matrix.service"
  assert_contains "Run tests step has MATRIX_RELEASE env" "$run_tests_env_release" "matrix.release"

  # INSTALL_SPEC env var references pip-extras output (CC-0034)
  local install_spec_env
  install_spec_env=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .env["INSTALL_SPEC"]' "$WORKFLOW" || true)
  assert_contains "Run tests step has INSTALL_SPEC referencing pip-extras output" "$install_spec_env" "steps.pip-extras.outputs.install_spec"

  # Resolve pip extras step reads from extra-packages.yaml and outputs install_spec (CC-0034)
  local pip_extras_run
  pip_extras_run=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.id == "pip-extras") | .run' "$WORKFLOW" || true)
  assert_contains "Resolve pip extras reads extra-packages.yaml" "$pip_extras_run" "extra-packages.yaml"
  assert_contains "Resolve pip extras outputs install_spec" "$pip_extras_run" "install_spec="
}

# --- CC-0034 REQ-011: test-service-images uses docker run with venv-builder image ---
test_test_service_images_docker_run() {
  echo "Test: test-service-images uses docker run with venv-builder image (CC-0034 REQ-011)"

  local run_step
  run_step=$(yq_raw '.jobs["test-service-images"]["steps"][] | select(.name == "Run tests") | .run' "$WORKFLOW" || true)

  assert_contains "Run tests uses docker run" "$run_step" "docker run"
  assert_contains "Run tests references VENV_BUILDER_IMAGE" "$run_step" "VENV_BUILDER_IMAGE"
  assert_contains "Run tests creates results directory" "$run_step" "mkdir -p results"
}

# --- CC-0034 REQ-012: test-service-images has CC-0034 feature comment ---
test_test_service_images_feature_comment() {
  echo "Test: test-service-images job has CC-0034 feature comment (CC-0034 REQ-012)"

  assert_file_contains "workflow has CC-0034 comment above test-service-images" "$WORKFLOW" "CC-0034"
}

# --- REQ-008: Expression injection defense — run: blocks use env vars ---
test_run_blocks_use_env_vars() {
  echo "Test: run: blocks use env vars instead of direct interpolation (REQ-008)"

  assert_file_not_contains "resolve source ref run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'yq .*\${{ matrix'
  assert_file_not_contains "apply patches run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'git -C.*\${{ matrix'
  assert_file_not_contains "apply overrides run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'apply-constraint-overrides.sh \${{ matrix'
}

# --- CC-0029: SBOM permissions on merge-base-images (REQ-012) ---
test_sbom_permissions_on_build_base_images() {
  echo "Test: SBOM permissions on merge-base-images (CC-0029, REQ-012)"

  local id_token attestations
  id_token=$(yq_raw '.jobs["merge-base-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["merge-base-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "merge-base-images has id-token: write" "write" "$id_token"
  assert_eq "merge-base-images has attestations: write" "write" "$attestations"
}

# --- CC-0029: SBOM permissions on merge-service-images (REQ-012) ---
test_sbom_permissions_on_build_service_images() {
  echo "Test: SBOM permissions on merge-service-images (CC-0029, REQ-012)"

  local id_token attestations
  id_token=$(yq_raw '.jobs["merge-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["merge-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "merge-service-images has id-token: write" "write" "$id_token"
  assert_eq "merge-service-images has attestations: write" "write" "$attestations"
}

# --- CC-0029: Verify jobs do NOT have SBOM permissions (REQ-012) ---
test_verify_jobs_no_sbom_permissions() {
  echo "Test: verify jobs do not have SBOM permissions (CC-0029, REQ-012)"

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

# --- CC-0029: SBOM generation steps exist (REQ-010) ---
test_sbom_generation_steps_exist() {
  echo "Test: SBOM generation steps exist in both merge jobs (CC-0029, REQ-010)"

  # merge-base-images should have 2 SBOM generation steps (python-base, venv-builder)
  local base_sbom_count
  base_sbom_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 2 SBOM generation steps" "2" "$base_sbom_count"

  # merge-service-images should have 1 SBOM generation step
  local service_sbom_count
  service_sbom_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images has 1 SBOM generation step" "1" "$service_sbom_count"
}

# --- CC-0029: SBOM format is cyclonedx-json (REQ-010) ---
test_sbom_format_cyclonedx_json() {
  echo "Test: SBOM format is cyclonedx-json (CC-0029, REQ-010)"

  # All SBOM generation steps in merge-base-images must use cyclonedx-json
  local base_formats
  base_formats=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.format' "$WORKFLOW" || true)

  local all_cyclonedx=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "cyclonedx-json" ]; then
      echo "  FAIL: merge-base-images SBOM format is not cyclonedx-json: $val"
      FAIL=$((FAIL + 1))
      all_cyclonedx=false
    fi
  done <<< "$base_formats"

  if $all_cyclonedx && [ -n "$base_formats" ]; then
    echo "  PASS: all merge-base-images SBOM steps use cyclonedx-json"
    PASS=$((PASS + 1))
  elif [ -z "$base_formats" ]; then
    echo "  FAIL: merge-base-images SBOM format check found no steps"
    FAIL=$((FAIL + 1))
  fi

  # merge-service-images SBOM step
  local service_format
  service_format=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.format' "$WORKFLOW" || true)
  assert_eq "merge-service-images SBOM format is cyclonedx-json" "cyclonedx-json" "$service_format"
}

# --- CC-0029: SBOM generation steps disable artifact upload (REQ-010) ---
test_sbom_no_artifact_upload() {
  echo "Test: SBOM generation steps set upload-artifact: false (CC-0029, REQ-010)"

  # All SBOM generation steps in merge-base-images must set upload-artifact: false
  local base_upload_values
  base_upload_values=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with["upload-artifact"]' "$WORKFLOW" || true)

  local all_false=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "false" ]; then
      echo "  FAIL: merge-base-images SBOM step does not set upload-artifact: false: $val"
      FAIL=$((FAIL + 1))
      all_false=false
    fi
  done <<< "$base_upload_values"

  if $all_false && [ -n "$base_upload_values" ]; then
    echo "  PASS: all merge-base-images SBOM steps set upload-artifact: false"
    PASS=$((PASS + 1))
  elif [ -z "$base_upload_values" ]; then
    echo "  FAIL: no merge-base-images SBOM steps found for upload-artifact check"
    FAIL=$((FAIL + 1))
  fi

  # merge-service-images SBOM step
  local service_upload
  service_upload=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with["upload-artifact"]' "$WORKFLOW" || true)
  assert_eq "merge-service-images SBOM step sets upload-artifact: false" "false" "$service_upload"
}

# --- CC-0029: SBOM generation references correct digest (REQ-015) ---
test_sbom_generation_references_digest() {
  echo "Test: SBOM generation steps reference correct digest (CC-0029, REQ-015)"

  # python-base SBOM references merge-python-base digest
  local python_base_image
  python_base_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Generate SBOM for python-base") | .with.image' "$WORKFLOW" || true)
  assert_contains "python-base SBOM references merge-python-base digest" "$python_base_image" "merge-python-base.outputs.digest"

  # venv-builder SBOM references merge-venv-builder digest
  local venv_builder_image
  venv_builder_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Generate SBOM for venv-builder") | .with.image' "$WORKFLOW" || true)
  assert_contains "venv-builder SBOM references merge-venv-builder digest" "$venv_builder_image" "merge-venv-builder.outputs.digest"

  # service SBOM references merge-service digest
  local service_image
  service_image=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Generate SBOM for service image") | .with.image' "$WORKFLOW" || true)
  assert_contains "service SBOM uses tags.outputs.image" "$service_image" "steps.tags.outputs.image"
  assert_contains "service SBOM references merge-service digest" "$service_image" "merge-service.outputs.digest"
}

# --- CC-0029: SBOM attestation steps exist (REQ-011) ---
test_sbom_attestation_steps_exist() {
  echo "Test: SBOM attestation steps exist in both merge jobs (CC-0029, REQ-011)"

  # merge-base-images should have 2 attestation steps
  local base_attest_count
  base_attest_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 2 attestation steps" "2" "$base_attest_count"

  # merge-service-images should have 1 attestation step
  local service_attest_count
  service_attest_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images has 1 attestation step" "1" "$service_attest_count"
}

# --- CC-0029: SBOM attestation push-to-registry (REQ-016) ---
test_sbom_attestation_push_to_registry() {
  echo "Test: SBOM attestation push-to-registry is true (CC-0029, REQ-016)"

  # All attestation steps in merge-base-images
  local base_push_values
  base_push_values=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)

  local all_true=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "true" ]; then
      echo "  FAIL: merge-base-images attestation push-to-registry is not true: $val"
      FAIL=$((FAIL + 1))
      all_true=false
    fi
  done <<< "$base_push_values"

  if $all_true && [ -n "$base_push_values" ]; then
    echo "  PASS: all merge-base-images attestation steps have push-to-registry: true"
    PASS=$((PASS + 1))
  elif [ -z "$base_push_values" ]; then
    echo "  FAIL: no merge-base-images attestation steps found (empty yq result)"
    FAIL=$((FAIL + 1))
  fi

  # merge-service-images attestation step
  local service_push
  service_push=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)
  assert_eq "merge-service-images attestation push-to-registry is true" "true" "$service_push"
}

# --- CC-0029: SBOM/attestation steps have PR-skip guard (REQ-013) ---
test_sbom_steps_pr_skip_guard() {
  echo "Test: SBOM/attestation steps have PR-skip guard (CC-0029, REQ-013)"

  # All SBOM steps in merge-base-images must have PR guard
  local base_sbom_ifs
  base_sbom_ifs=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action|actions/attest"))) | .if' "$WORKFLOW" || true)

  local all_guarded=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"github.event_name != 'pull_request'"* ]]; then
      echo "  FAIL: merge-base-images SBOM/attestation step missing PR guard: $val"
      FAIL=$((FAIL + 1))
      all_guarded=false
    fi
  done <<< "$base_sbom_ifs"

  if $all_guarded && [ -n "$base_sbom_ifs" ]; then
    echo "  PASS: all merge-base-images SBOM/attestation steps have PR-skip guard"
    PASS=$((PASS + 1))
  elif [ -z "$base_sbom_ifs" ]; then
    echo "  FAIL: merge-base-images SBOM/attestation steps not found or missing PR guard"
    FAIL=$((FAIL + 1))
  else
    echo "  FAIL: some merge-base-images SBOM/attestation steps missing PR-skip guard (see above)"
  fi

  # merge-service-images has job-level PR guard
  local merge_service_job_if
  merge_service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "merge-service-images job has PR-skip guard" "$merge_service_job_if" "event_name != 'pull_request'"
}

# --- CC-0035: build provenance attestation steps exist (SLSA Level 2+) ---
test_build_provenance_steps_exist() {
  echo "Test: build provenance attestation steps exist in both build jobs (CC-0035)"

  local base_prov_count
  base_prov_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 2 build-provenance attestation steps" "2" "$base_prov_count"

  local service_prov_count
  service_prov_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images has 1 build-provenance attestation step" "1" "$service_prov_count"
}

# --- CC-0035: build provenance steps have PR-skip guard ---
test_build_provenance_steps_pr_skip_guard() {
  echo "Test: build provenance steps have PR-skip guard (CC-0035)"

  local base_prov_ifs
  base_prov_ifs=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .if' "$WORKFLOW" || true)

  local all_guarded=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"github.event_name != 'pull_request'"* ]]; then
      echo "  FAIL: merge-base-images provenance step missing PR guard: $val"
      FAIL=$((FAIL + 1))
      all_guarded=false
    fi
  done <<< "$base_prov_ifs"

  if $all_guarded && [ -n "$base_prov_ifs" ]; then
    echo "  PASS: all merge-base-images provenance steps have PR-skip guard"
    PASS=$((PASS + 1))
  elif [ -z "$base_prov_ifs" ]; then
    echo "  FAIL: merge-base-images provenance steps not found or missing PR guard"
    FAIL=$((FAIL + 1))
  else
    echo "  FAIL: some merge-base-images provenance steps missing PR-skip guard (see above)"
    FAIL=$((FAIL + 1))
  fi

  # merge-service-images uses a job-level if guard instead of per-step guards
  local service_job_if
  service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  if [[ "$service_job_if" == *"github.event_name != 'pull_request'"* ]]; then
    echo "  PASS: merge-service-images has job-level PR-skip guard"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: merge-service-images missing job-level PR-skip guard: $service_job_if"
    FAIL=$((FAIL + 1))
  fi
}

# --- CC-0035: build provenance push-to-registry is true ---
test_build_provenance_push_to_registry() {
  echo "Test: build provenance push-to-registry is true (CC-0035)"

  local base_push_values
  base_push_values=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)

  local all_true=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != "true" ]]; then
      echo "  FAIL: merge-base-images provenance step push-to-registry is not true: $val"
      FAIL=$((FAIL + 1))
      all_true=false
    fi
  done <<< "$base_push_values"

  if $all_true && [ -n "$base_push_values" ]; then
    echo "  PASS: all merge-base-images provenance steps have push-to-registry: true"
    PASS=$((PASS + 1))
  elif [ -z "$base_push_values" ]; then
    echo "  FAIL: no merge-base-images provenance steps found (empty yq result)"
    FAIL=$((FAIL + 1))
  fi

  local service_push
  service_push=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest-build-provenance@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)
  assert_eq "merge-service-images provenance push-to-registry is true" "true" "$service_push"
}

# --- CC-0031: metadata-action steps exist in build-base-images (REQ-002) ---
test_metadata_action_steps_exist_in_build_base_images() {
  echo "Test: metadata-action steps exist in build-base-images (CC-0031, REQ-002)"

  local meta_count
  meta_count=$(yq_count '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("docker/metadata-action"))) | .uses' "$WORKFLOW")
  assert_eq "build-base-images has 2 docker/metadata-action steps" "2" "$meta_count"

  local python_base_id venv_builder_id
  python_base_id=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .id' "$WORKFLOW" || true)
  venv_builder_id=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .id' "$WORKFLOW" || true)

  assert_eq "meta-python-base step exists" "meta-python-base" "$python_base_id"
  assert_eq "meta-venv-builder step exists" "meta-venv-builder" "$venv_builder_id"
}

# --- CC-0031: metadata-action step exists in build-service-images (REQ-002) ---
test_metadata_action_step_exists_in_build_service_images() {
  echo "Test: metadata-action step exists in build-service-images (CC-0031, REQ-002)"

  local meta_count
  meta_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("docker/metadata-action"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images has 1 docker/metadata-action step" "1" "$meta_count"

  local keystone_id
  keystone_id=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .id' "$WORKFLOW" || true)
  assert_eq "meta-service step exists" "meta-service" "$keystone_id"
}

# --- CC-0031: keystone metadata uses raw version strategy (REQ-003) ---
test_service_metadata_uses_raw_version_strategy() {
  echo "Test: service metadata uses raw version strategy (CC-0031, REQ-003)"

  local tags_input
  tags_input=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .with.tags' "$WORKFLOW" || true)

  assert_contains "meta-service tags input contains type=raw" "$tags_input" "type=raw"
  assert_contains "meta-service tags input references source-ref output" "$tags_input" "steps.source-ref.outputs.ref"
}

# --- CC-0031: base metadata steps have no tags override (REQ-004) ---
test_base_metadata_steps_have_no_tags_override() {
  echo "Test: base metadata steps have no tags override (CC-0031, REQ-004)"

  local python_base_tags venv_builder_tags
  python_base_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .with.tags' "$WORKFLOW" || echo "null")
  venv_builder_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .with.tags' "$WORKFLOW" || echo "null")

  assert_eq "meta-python-base has no tags input" "null" "$python_base_tags"
  assert_eq "meta-venv-builder has no tags input" "null" "$venv_builder_tags"
}

# --- CC-0031: python-base build-push-action has labels input (REQ-005) ---
test_python_base_build_push_has_labels_input() {
  echo "Test: python-base build-push-action has labels input (CC-0031, REQ-005)"

  local labels
  labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with.labels' "$WORKFLOW" || true)

  assert_contains "build-python-base labels references meta-python-base" "$labels" "steps.meta-python-base.outputs.labels"
}

# --- CC-0031: venv-builder build-push-action has labels input (REQ-005) ---
test_venv_builder_build_push_has_labels_input() {
  echo "Test: venv-builder build-push-action has labels input (CC-0031, REQ-005)"

  local labels
  labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with.labels' "$WORKFLOW" || true)

  assert_contains "build-venv-builder labels references meta-venv-builder" "$labels" "steps.meta-venv-builder.outputs.labels"
}

# --- CC-0031: service build-push-action has labels input (REQ-005) ---
test_service_build_push_has_labels_input() {
  echo "Test: service build-push-action has labels input (CC-0031, REQ-005)"

  local labels
  labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "build-service labels references meta-service" "$labels" "steps.meta-service.outputs.labels"
}

# --- CC-0034: OCI base image labels present in build steps (REQ-002) ---
test_oci_base_labels_in_build_steps() {
  echo "Test: OCI base image labels present in all three build steps (CC-0034)"

  local python_labels venv_labels service_labels
  python_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with.labels' "$WORKFLOW" || true)
  venv_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with.labels' "$WORKFLOW" || true)
  service_labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "build-python-base labels include org.opencontainers.image.base.name" "$python_labels" "org.opencontainers.image.base.name"
  assert_contains "build-python-base labels include org.opencontainers.image.base.digest" "$python_labels" "org.opencontainers.image.base.digest"
  assert_contains "build-venv-builder labels include org.opencontainers.image.base.name" "$venv_labels" "org.opencontainers.image.base.name"
  assert_contains "build-venv-builder labels include org.opencontainers.image.base.digest" "$venv_labels" "org.opencontainers.image.base.digest"
  assert_contains "build-service labels include org.opencontainers.image.base.name" "$service_labels" "org.opencontainers.image.base.name"
  assert_contains "build-service labels include org.opencontainers.image.base.digest" "$service_labels" "org.opencontainers.image.base.digest"
}

# --- CC-0031: metadata-action labels include OCI title (REQ-005) ---
test_metadata_action_labels_include_oci_title() {
  echo "Test: metadata-action labels include OCI title (CC-0031, REQ-005)"

  local python_labels venv_labels keystone_labels
  python_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .with.labels' "$WORKFLOW" || true)
  venv_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .with.labels' "$WORKFLOW" || true)
  keystone_labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "meta-python-base labels include OCI title" "$python_labels" "org.opencontainers.image.title"
  assert_contains "meta-venv-builder labels include OCI title" "$venv_labels" "org.opencontainers.image.title"
  assert_contains "meta-service labels include OCI title" "$keystone_labels" "org.opencontainers.image.title"
}

# --- CC-0031: metadata-action labels include OCI description (REQ-005) ---
test_metadata_action_labels_include_oci_description() {
  echo "Test: metadata-action labels include OCI description (CC-0031, REQ-005)"

  local python_labels venv_labels keystone_labels
  python_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .with.labels' "$WORKFLOW" || true)
  venv_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .with.labels' "$WORKFLOW" || true)
  keystone_labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "meta-python-base labels include OCI description" "$python_labels" "org.opencontainers.image.description"
  assert_contains "meta-venv-builder labels include OCI description" "$venv_labels" "org.opencontainers.image.description"
  assert_contains "meta-service labels include OCI description" "$keystone_labels" "org.opencontainers.image.description"
}

# --- CC-0031: metadata-action labels include OCI licenses (REQ-005) ---
test_metadata_action_labels_include_oci_licenses() {
  echo "Test: metadata-action labels include OCI licenses (CC-0031, REQ-005)"

  local python_labels venv_labels keystone_labels
  python_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .with.labels' "$WORKFLOW" || true)
  venv_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .with.labels' "$WORKFLOW" || true)
  keystone_labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "meta-python-base labels include Apache-2.0 license" "$python_labels" "org.opencontainers.image.licenses=Apache-2.0"
  assert_contains "meta-venv-builder labels include Apache-2.0 license" "$venv_labels" "org.opencontainers.image.licenses=Apache-2.0"
  assert_contains "meta-service labels include Apache-2.0 license" "$keystone_labels" "org.opencontainers.image.licenses=Apache-2.0"
}

# --- CC-0031: metadata-action labels include OCI vendor (REQ-005) ---
test_metadata_action_labels_include_oci_vendor() {
  echo "Test: metadata-action labels include OCI vendor (CC-0031, REQ-005)"

  local python_labels venv_labels keystone_labels
  python_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-python-base") | .with.labels' "$WORKFLOW" || true)
  venv_labels=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "meta-venv-builder") | .with.labels' "$WORKFLOW" || true)
  keystone_labels=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "meta-service") | .with.labels' "$WORKFLOW" || true)

  assert_contains "meta-python-base labels include vendor" "$python_labels" "org.opencontainers.image.vendor"
  assert_contains "meta-venv-builder labels include vendor" "$venv_labels" "org.opencontainers.image.vendor"
  assert_contains "meta-service labels include vendor" "$keystone_labels" "org.opencontainers.image.vendor"
}

# --- CC-0031: static OCI labels in python-base Dockerfile (REQ-001) ---
test_dockerfile_static_labels_python_base() {
  echo "Test: python-base Dockerfile has static OCI labels (CC-0031, REQ-001)"

  local dockerfile="$PROJECT_ROOT/images/python-base/Dockerfile"

  assert_file_contains "python-base has org.opencontainers.image.title" "$dockerfile" 'org.opencontainers.image.title='
  assert_file_contains "python-base has org.opencontainers.image.description" "$dockerfile" 'org.opencontainers.image.description='
  assert_file_contains "python-base has org.opencontainers.image.licenses" "$dockerfile" 'org.opencontainers.image.licenses='
  assert_file_contains "python-base has org.opencontainers.image.vendor" "$dockerfile" 'org.opencontainers.image.vendor='
}

# --- CC-0031: static OCI labels in venv-builder Dockerfile (REQ-001) ---
test_dockerfile_static_labels_venv_builder() {
  echo "Test: venv-builder Dockerfile has static OCI labels (CC-0031, REQ-001)"

  local dockerfile="$PROJECT_ROOT/images/venv-builder/Dockerfile"

  assert_file_contains "venv-builder has org.opencontainers.image.title" "$dockerfile" 'org.opencontainers.image.title='
  assert_file_contains "venv-builder has org.opencontainers.image.description" "$dockerfile" 'org.opencontainers.image.description='
  assert_file_contains "venv-builder has org.opencontainers.image.licenses" "$dockerfile" 'org.opencontainers.image.licenses='
  assert_file_contains "venv-builder has org.opencontainers.image.vendor" "$dockerfile" 'org.opencontainers.image.vendor='
}

# --- CC-0031: static OCI labels in keystone Dockerfile runtime stage (REQ-001) ---
test_dockerfile_static_labels_keystone() {
  echo "Test: keystone Dockerfile has static OCI labels in runtime stage (CC-0031, REQ-001)"

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

# --- CC-0030: cosign-installer step in merge-base-images (REQ-018) ---
test_cosign_installer_in_build_base_images() {
  echo "Test: cosign-installer step exists in merge-base-images (CC-0030, REQ-018)"

  local installer_count
  installer_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("sigstore/cosign-installer"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 1 cosign-installer step" "1" "$installer_count"
}

# --- CC-0030: cosign-installer step in merge-service-images (REQ-018) ---
test_cosign_installer_in_build_service_images() {
  echo "Test: cosign-installer step exists in merge-service-images (CC-0030, REQ-018)"

  local installer_count
  installer_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("sigstore/cosign-installer"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images has 1 cosign-installer step" "1" "$installer_count"
}

# --- CC-0030: cosign sign steps exist in both merge jobs (REQ-019, REQ-020) ---
test_cosign_sign_steps_count() {
  echo "Test: cosign sign steps exist in both merge jobs (CC-0030, REQ-019, REQ-020)"

  # merge-base-images should have 2 sign steps (python-base and venv-builder)
  local base_sign_count
  base_sign_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.run and (.run | test("cosign sign"))) | .run' "$WORKFLOW")
  assert_eq "merge-base-images has 2 cosign sign steps" "2" "$base_sign_count"

  # merge-service-images should have 1 sign step
  local service_sign_count
  service_sign_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.run and (.run | test("cosign sign"))) | .run' "$WORKFLOW")
  assert_eq "merge-service-images has 1 cosign sign step" "1" "$service_sign_count"
}

# --- CC-0030: cosign sign steps have PR-skip guard (REQ-021) ---
test_cosign_sign_steps_pr_guard() {
  echo "Test: cosign sign steps have PR-skip guard (CC-0030, REQ-021)"

  # All cosign sign steps in merge-base-images must have PR guard
  local base_sign_ifs
  base_sign_ifs=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.run and (.run | test("cosign sign"))) | .if' "$WORKFLOW" || true)

  local all_guarded=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"github.event_name != 'pull_request'"* ]]; then
      echo "  FAIL: merge-base-images cosign sign step missing PR guard: $val"
      FAIL=$((FAIL + 1))
      all_guarded=false
    fi
  done <<< "$base_sign_ifs"

  if $all_guarded && [ -n "$base_sign_ifs" ]; then
    echo "  PASS: all merge-base-images cosign sign steps have PR-skip guard"
    PASS=$((PASS + 1))
  elif [ -z "$base_sign_ifs" ]; then
    echo "  FAIL: merge-base-images cosign sign steps not found or missing PR guard"
    FAIL=$((FAIL + 1))
  fi

  # merge-service-images has job-level PR guard
  local merge_service_job_if
  merge_service_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "merge-service-images job-level if excludes pull_request" "$merge_service_job_if" "event_name != 'pull_request'"
}

# --- CC-0030: cosign sign steps reference digest (REQ-022) ---
test_cosign_sign_steps_reference_digest() {
  echo "Test: cosign sign steps reference correct digest (CC-0030, REQ-022)"

  # python-base sign references merge-python-base digest
  local python_base_run
  python_base_run=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Sign python-base") | .run' "$WORKFLOW" || true)
  assert_contains "python-base sign references merge-python-base digest" "$python_base_run" "merge-python-base.outputs.digest"

  # venv-builder sign references merge-venv-builder digest
  local venv_builder_run
  venv_builder_run=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name == "Sign venv-builder") | .run' "$WORKFLOW" || true)
  assert_contains "venv-builder sign references merge-venv-builder digest" "$venv_builder_run" "merge-venv-builder.outputs.digest"

  # service sign references merge-service digest
  local service_run
  service_run=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name == "Sign service image") | .run' "$WORKFLOW" || true)
  assert_contains "service sign uses tags.outputs.image" "$service_run" "steps.tags.outputs.image"
  assert_contains "service sign references merge-service digest" "$service_run" "merge-service.outputs.digest"
}

# --- CC-0030: cosign sign uses --yes flag (REQ-022) ---
test_cosign_sign_uses_yes_flag() {
  echo "Test: cosign sign steps use --yes flag (CC-0030, REQ-022)"

  # All cosign sign run commands in merge-base-images must contain --yes
  local base_runs
  base_runs=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.run and (.run | test("cosign sign"))) | .run' "$WORKFLOW" || true)

  local all_yes=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"--yes"* ]]; then
      echo "  FAIL: merge-base-images cosign sign step missing --yes flag: $val"
      FAIL=$((FAIL + 1))
      all_yes=false
    fi
  done <<< "$base_runs"

  if $all_yes && [ -n "$base_runs" ]; then
    echo "  PASS: all merge-base-images cosign sign steps use --yes flag"
    PASS=$((PASS + 1))
  elif [ -z "$base_runs" ]; then
    echo "  FAIL: no merge-base-images cosign sign steps found"
    FAIL=$((FAIL + 1))
  fi

  # service sign must contain --yes
  local service_run
  service_run=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.run and (.run | test("cosign sign"))) | .run' "$WORKFLOW" || true)
  assert_contains "merge-service-images cosign sign uses --yes flag" "$service_run" "--yes"
}

# --- CC-0030: id-token permission comment references CC-0030 (REQ-018) ---
test_cosign_id_token_permission_comment() {
  echo "Test: id-token permission comment references CC-0030 (CC-0030, REQ-018)"

  # The id-token: write line itself (not other comments) should reference CC-0030
  assert_file_contains "id-token permission references CC-0030" "$WORKFLOW" "id-token:.*CC-0030"
}

# =====================================================================
# CC-0032: Grype vulnerability scanning verification tests
# =====================================================================

# --- CC-0032: Grype scan steps exist in merge-base-images (REQ-001) ---
test_grype_scan_steps_in_build_base_images() {
  echo "Test: Grype scan steps exist in merge-base-images (CC-0032, REQ-001)"

  local scan_count
  scan_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 4 Grype scan steps" "4" "$scan_count"
}

# --- CC-0032: Grype scan step exists in build-service-images and merge-service-images (REQ-001) ---
test_grype_scan_step_in_build_service_images() {
  echo "Test: Grype scan step exists in build-service-images and merge-service-images (CC-0032, REQ-001)"

  local scan_count
  scan_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images has 1 Grype scan step" "1" "$scan_count"

  local merge_scan_count
  merge_scan_count=$(yq_count '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW")
  assert_eq "merge-service-images has 1 Grype scan step" "1" "$merge_scan_count"
}

# --- CC-0032: anchore/scan-action is SHA-pinned (REQ-010) ---
test_grype_scan_action_sha_pinned() {
  echo "Test: anchore/scan-action is SHA-pinned (CC-0032, REQ-010)"

  # Collect all anchore/scan-action uses from both merge jobs and build-service-images
  local uses_values
  uses_values=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW" || true)
  uses_values+=$'\n'
  uses_values+=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW" || true)
  uses_values+=$'\n'
  uses_values+=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/scan-action"))) | .uses' "$WORKFLOW" || true)

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

  # Validate inline version comment (CC-0032, REQ-010)
  assert_file_contains "anchore/scan-action pin has # v7 version comment" "$WORKFLOW" "anchore/scan-action@[0-9a-f]\{40\}[[:space:]]*# v7"
}

# --- CC-0032: Grype scan covers both push and PR contexts (REQ-004) ---
test_grype_scan_steps_cover_both_contexts() {
  echo "Test: Grype scan steps cover both push and PR contexts (CC-0032, REQ-004)"

  # Each image must have both a push-context (SBOM) and a PR-context (image) scan step.
  # This ensures scanning always runs regardless of event type, while keeping
  # sbom and image as mutually exclusive inputs per anchore/scan-action docs.

  # merge-base-images: verify push (SBOM) and PR (image) steps exist for python-base
  local python_sbom_if
  python_sbom_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .if' "$WORKFLOW" || true)
  assert_contains "python-base SBOM scan has push-only guard" "$python_sbom_if" "!= 'pull_request'"

  local python_image_if
  python_image_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-image") | .if' "$WORKFLOW" || true)
  assert_contains "python-base image scan has PR-only guard" "$python_image_if" "== 'pull_request'"

  # merge-base-images: verify push (SBOM) and PR (image) steps exist for venv-builder
  local venv_sbom_if
  venv_sbom_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .if' "$WORKFLOW" || true)
  assert_contains "venv-builder SBOM scan has push-only guard" "$venv_sbom_if" "!= 'pull_request'"

  local venv_image_if
  venv_image_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-image") | .if' "$WORKFLOW" || true)
  assert_contains "venv-builder image scan has PR-only guard" "$venv_image_if" "== 'pull_request'"

  # merge-service-images uses a job-level if guard; the SBOM scan step needs no additional step guard
  local service_merge_job_if
  service_merge_job_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "service SBOM scan has push-only guard (job-level)" "$service_merge_job_if" "!= 'pull_request'"

  # build-service-images: verify PR (image) step exists for service
  local service_image_if
  service_image_if=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "grype-service-image") | .if' "$WORKFLOW" || true)
  assert_contains "service image scan has PR-only guard" "$service_image_if" "== 'pull_request'"
}

# --- CC-0032: Grype scan SBOM input references correct files (REQ-002) ---
test_grype_sbom_input_wiring() {
  echo "Test: Grype scan SBOM input references correct SBOM files (CC-0032, REQ-002)"

  # python-base SBOM scan must reference sbom-python-base.cyclonedx.json
  local python_sbom
  python_sbom=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .with.sbom' "$WORKFLOW" || true)
  assert_contains "python-base Grype scan references sbom-python-base.cyclonedx.json" "$python_sbom" "sbom-python-base.cyclonedx.json"

  # python-base SBOM scan must have push-only conditional guard
  local python_sbom_if
  python_sbom_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .if' "$WORKFLOW" || true)
  assert_contains "python-base Grype sbom input has push-only conditional" "$python_sbom_if" "event_name != 'pull_request'"

  # venv-builder SBOM scan must reference sbom-venv-builder.cyclonedx.json
  local venv_sbom
  venv_sbom=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .with.sbom' "$WORKFLOW" || true)
  assert_contains "venv-builder Grype scan references sbom-venv-builder.cyclonedx.json" "$venv_sbom" "sbom-venv-builder.cyclonedx.json"

  # venv-builder SBOM scan must have push-only conditional guard
  local venv_sbom_if
  venv_sbom_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .if' "$WORKFLOW" || true)
  assert_contains "venv-builder Grype sbom input has push-only conditional" "$venv_sbom_if" "event_name != 'pull_request'"

  # service SBOM scan must reference sbom-{service}.cyclonedx.json
  local service_sbom
  service_sbom=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "grype-service-sbom") | .with.sbom' "$WORKFLOW" || true)
  assert_contains "service Grype scan references matrix.service SBOM filename" "$service_sbom" "matrix.service"
  assert_contains "service Grype scan SBOM input is cyclonedx.json format" "$service_sbom" "cyclonedx.json"

  # merge-service-images uses a job-level if guard; check job-level guard instead of step-level
  local service_merge_if
  service_merge_if=$(yq_raw '.jobs["merge-service-images"]["if"]' "$WORKFLOW" || true)
  assert_contains "service Grype sbom input has push-only conditional (job-level)" "$service_merge_if" "event_name != 'pull_request'"
}

# --- CC-0032: Grype scan image input wiring for PR context (REQ-003) ---
test_grype_image_input_wiring() {
  echo "Test: Grype scan image input wiring for PR context (CC-0032, REQ-003)"

  # python-base image scan must reference python-base image
  local python_image
  python_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-image") | .with.image' "$WORKFLOW" || true)
  assert_contains "python-base Grype scan image input references python-base" "$python_image" "python-base"
  assert_contains "python-base Grype scan image input references merge-python-base digest" "$python_image" "merge-python-base.outputs.digest"

  # venv-builder image scan must reference venv-builder image
  local venv_image
  venv_image=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-image") | .with.image' "$WORKFLOW" || true)
  assert_contains "venv-builder Grype scan image input references venv-builder" "$venv_image" "venv-builder"
  assert_contains "venv-builder Grype scan image input references merge-venv-builder digest" "$venv_image" "merge-venv-builder.outputs.digest"

  # service image scan must reference composite tag
  local service_image
  service_image=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "grype-service-image") | .with.image' "$WORKFLOW" || true)
  assert_contains "service Grype scan image input references composite tag" "$service_image" "tags.outputs.composite"
}

# --- CC-0032: Grype scan severity threshold is high (REQ-005) ---
test_grype_severity_threshold() {
  echo "Test: Grype scan severity-cutoff is high (CC-0032, REQ-005)"

  # All Grype scan steps (both SBOM and image variants) must have severity-cutoff: high
  local python_sbom_severity
  python_sbom_severity=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "python-base (sbom) Grype severity-cutoff is high" "high" "$python_sbom_severity"

  local python_image_severity
  python_image_severity=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-image") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "python-base (image) Grype severity-cutoff is high" "high" "$python_image_severity"

  local venv_sbom_severity
  venv_sbom_severity=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (sbom) Grype severity-cutoff is high" "high" "$venv_sbom_severity"

  local venv_image_severity
  venv_image_severity=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-image") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (image) Grype severity-cutoff is high" "high" "$venv_image_severity"

  local service_sbom_severity
  service_sbom_severity=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "grype-service-sbom") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "service (sbom) Grype severity-cutoff is high" "high" "$service_sbom_severity"

  local service_image_severity
  service_image_severity=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "grype-service-image") | .with["severity-cutoff"]' "$WORKFLOW" || true)
  assert_eq "service (image) Grype severity-cutoff is high" "high" "$service_image_severity"
}

# --- CC-0032: Grype scan fail-build is false (REQ-005) ---
test_grype_fail_build_false() {
  echo "Test: Grype scan fail-build is false (CC-0032, REQ-005)"

  # All Grype scan steps must have fail-build: false (non-blocking scan, to be activated later)
  local python_sbom_fail
  python_sbom_fail=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "python-base (sbom) Grype fail-build is false" "false" "$python_sbom_fail"

  local python_image_fail
  python_image_fail=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-image") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "python-base (image) Grype fail-build is false" "false" "$python_image_fail"

  local venv_sbom_fail
  venv_sbom_fail=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (sbom) Grype fail-build is false" "false" "$venv_sbom_fail"

  local venv_image_fail
  venv_image_fail=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-image") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (image) Grype fail-build is false" "false" "$venv_image_fail"

  local service_sbom_fail
  service_sbom_fail=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "grype-service-sbom") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "service (sbom) Grype fail-build is false" "false" "$service_sbom_fail"

  local service_image_fail
  service_image_fail=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "grype-service-image") | .with["fail-build"]' "$WORKFLOW" || true)
  assert_eq "service (image) Grype fail-build is false" "false" "$service_image_fail"
}

# --- CC-0032: SARIF upload steps exist in both build jobs (REQ-006) ---
test_sarif_upload_steps_exist() {
  echo "Test: SARIF upload steps exist in both build jobs (CC-0032, REQ-006)"

  local base_sarif_count
  base_sarif_count=$(yq_count '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$WORKFLOW")
  assert_eq "merge-base-images has 2 SARIF upload steps" "2" "$base_sarif_count"

  local service_sarif_count
  service_sarif_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images has 1 SARIF upload step" "1" "$service_sarif_count"
}

# --- CC-0032: SARIF upload categories are correct (REQ-006) ---
test_sarif_upload_categories() {
  echo "Test: SARIF upload categories match image names (CC-0032, REQ-006)"

  # python-base SARIF upload category
  local python_category
  python_category=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for python-base"))) | .with.category' "$WORKFLOW" || true)
  assert_eq "python-base SARIF category is grype-python-base" "grype-python-base" "$python_category"

  # venv-builder SARIF upload category
  local venv_category
  venv_category=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for venv-builder"))) | .with.category' "$WORKFLOW" || true)
  assert_eq "venv-builder SARIF category is grype-venv-builder" "grype-venv-builder" "$venv_category"

  # service SARIF upload category must reference matrix.service
  local service_category
  service_category=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for service"))) | .with.category' "$WORKFLOW" || true)
  assert_contains "service SARIF category references matrix.service" "$service_category" "matrix.service"
}

# --- CC-0032: SARIF upload steps have if: always() (REQ-006) ---
test_sarif_upload_always_condition() {
  echo "Test: SARIF upload steps have if: always() with output guard (CC-0032, REQ-006)"

  # Check each SARIF upload step in merge-base-images individually
  local python_if
  python_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for python-base"))) | .if' "$WORKFLOW" || true)
  assert_contains "python-base SARIF upload has always() condition" "$python_if" "always()"
  assert_contains "python-base SARIF upload has output guard" "$python_if" "outputs.sarif"

  local venv_if
  venv_if=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for venv-builder"))) | .if' "$WORKFLOW" || true)
  assert_contains "venv-builder SARIF upload has always() condition" "$venv_if" "always()"
  assert_contains "venv-builder SARIF upload has output guard" "$venv_if" "outputs.sarif"

  local service_if
  service_if=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .if' "$WORKFLOW" || true)
  assert_contains "build-service-images SARIF upload has always() condition" "$service_if" "always()"
  assert_contains "build-service-images SARIF upload has output guard" "$service_if" "outputs.sarif"
}

# --- CC-0032: upload-sarif action is SHA-pinned (REQ-010) ---
test_sarif_upload_action_sha_pinned() {
  echo "Test: upload-sarif action is SHA-pinned (CC-0032, REQ-010)"

  # Collect all upload-sarif uses from merge-base-images, build-service-images, and merge-service-images
  local uses_values
  uses_values=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$WORKFLOW" || true)
  uses_values+=$'\n'
  uses_values+=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$WORKFLOW" || true)
  uses_values+=$'\n'
  uses_values+=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.uses and (.uses | test("codeql-action/upload-sarif"))) | .uses' "$WORKFLOW" || true)

  local all_pinned=true
  local found=false
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    found=true
    if [[ ! "$val" =~ codeql-action/upload-sarif@[0-9a-f]{40} ]]; then
      echo "  FAIL: upload-sarif action not SHA-pinned: $val"
      FAIL=$((FAIL + 1))
      all_pinned=false
    fi
  done <<< "$uses_values"

  if $all_pinned && $found; then
    echo "  PASS: all upload-sarif action uses are SHA-pinned"
    PASS=$((PASS + 1))
  elif ! $found; then
    echo "  FAIL: no upload-sarif action uses found"
    FAIL=$((FAIL + 1))
  fi

  # Validate inline version comment (CC-0032, REQ-010)
  assert_file_contains "upload-sarif pin has # v4 version comment" "$WORKFLOW" "codeql-action/upload-sarif@[0-9a-f]\{40\}[[:space:]]*# v4"
}

# --- CC-0032: security-events permission on merge-base-images (REQ-007) ---
test_security_events_permission_build_base_images() {
  echo "Test: merge-base-images has security-events: write permission (CC-0032, REQ-007)"

  local perm
  perm=$(yq_raw '.jobs["merge-base-images"]["permissions"]["security-events"]' "$WORKFLOW" || true)
  assert_eq "merge-base-images security-events permission is write" "write" "$perm"
}

# --- CC-0032: security-events permission on merge-service-images (REQ-007) ---
test_security_events_permission_build_service_images() {
  echo "Test: merge-service-images has security-events: write permission (CC-0032, REQ-007)"

  local perm
  perm=$(yq_raw '.jobs["merge-service-images"]["permissions"]["security-events"]' "$WORKFLOW" || true)
  assert_eq "merge-service-images security-events permission is write" "write" "$perm"
}

# --- CC-0032: verify jobs do NOT have security-events permission (REQ-009) ---
test_verify_jobs_no_security_events_permission() {
  echo "Test: verify jobs do not have security-events permission (CC-0032, REQ-009)"

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

# --- CC-0032: security-events permission comment references CC-0032 (REQ-007) ---
test_security_events_permission_comment() {
  echo "Test: security-events permission comment references CC-0032 (CC-0032, REQ-007)"

  assert_file_contains "security-events permission references CC-0032" "$WORKFLOW" "security-events:.*CC-0032"
}

# --- CC-0032: Grype scan output format is sarif (REQ-006) ---
test_grype_output_format_sarif() {
  echo "Test: Grype scan output-format is sarif (CC-0032, REQ-006)"

  local python_sbom_format
  python_sbom_format=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-sbom") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "python-base (sbom) Grype output-format is sarif" "sarif" "$python_sbom_format"

  local python_image_format
  python_image_format=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-python-base-image") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "python-base (image) Grype output-format is sarif" "sarif" "$python_image_format"

  local venv_sbom_format
  venv_sbom_format=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-sbom") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (sbom) Grype output-format is sarif" "sarif" "$venv_sbom_format"

  local venv_image_format
  venv_image_format=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.id == "grype-venv-builder-image") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "venv-builder (image) Grype output-format is sarif" "sarif" "$venv_image_format"

  local service_sbom_format
  service_sbom_format=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.id == "grype-service-sbom") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "service (sbom) Grype output-format is sarif" "sarif" "$service_sbom_format"

  local service_image_format
  service_image_format=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "grype-service-image") | .with["output-format"]' "$WORKFLOW" || true)
  assert_eq "service (image) Grype output-format is sarif" "sarif" "$service_image_format"
}

# --- CC-0032: SARIF upload references Grype step output (REQ-006) ---
test_sarif_upload_references_grype_output() {
  echo "Test: SARIF upload sarif_file references Grype step output (CC-0032, REQ-006)"

  # SARIF upload uses || expression to pick output from whichever scan step ran
  local python_sarif_file
  python_sarif_file=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for python-base"))) | .with.sarif_file' "$WORKFLOW" || true)
  assert_contains "python-base SARIF upload references grype-python-base-sbom output" "$python_sarif_file" "grype-python-base-sbom.outputs.sarif"
  assert_contains "python-base SARIF upload references grype-python-base-image output" "$python_sarif_file" "grype-python-base-image.outputs.sarif"

  local venv_sarif_file
  venv_sarif_file=$(yq_raw '.jobs["merge-base-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for venv-builder"))) | .with.sarif_file' "$WORKFLOW" || true)
  assert_contains "venv-builder SARIF upload references grype-venv-builder-sbom output" "$venv_sarif_file" "grype-venv-builder-sbom.outputs.sarif"
  assert_contains "venv-builder SARIF upload references grype-venv-builder-image output" "$venv_sarif_file" "grype-venv-builder-image.outputs.sarif"

  local service_build_sarif_file
  service_build_sarif_file=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for service"))) | .with.sarif_file' "$WORKFLOW" || true)
  assert_contains "service SARIF upload (build-service-images) references grype-service-image output" "$service_build_sarif_file" "grype-service-image.outputs.sarif"

  local service_merge_sarif_file
  service_merge_sarif_file=$(yq_raw '.jobs["merge-service-images"]["steps"][] | select(.name and (.name | test("Upload SARIF for service"))) | .with.sarif_file' "$WORKFLOW" || true)
  assert_contains "service SARIF upload (merge-service-images) references grype-service-sbom output" "$service_merge_sarif_file" "grype-service-sbom.outputs.sarif"
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
test_test_service_images_feature_comment
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
test_cosign_id_token_permission_comment
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
test_security_events_permission_comment
echo ""
test_grype_output_format_sarif
echo ""
test_sarif_upload_references_grype_output
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
