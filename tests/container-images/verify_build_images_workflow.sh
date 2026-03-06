#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build-images workflow structure, conventions, and correctness (CC-0007)
# Requirements: REQ-001 through REQ-009
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
  top_perms=$(yq_raw '.permissions' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "top-level permissions has contents: read" "$top_perms" "read"
}

# --- REQ-008: Job-level permissions scoping ---
test_job_permissions_scoping() {
  echo "Test: job-level permissions scoping (REQ-008)"

  # build-base-images and build-service-images need packages: write
  local base_perms service_perms smoke_perms
  base_perms=$(yq_raw '.jobs["build-base-images"]["permissions"]["packages"]' "$WORKFLOW" 2>/dev/null || echo "null")
  service_perms=$(yq_raw '.jobs["build-service-images"]["permissions"]["packages"]' "$WORKFLOW" 2>/dev/null || echo "null")
  smoke_perms=$(yq_raw '.jobs["smoke-test"]["permissions"]["packages"]' "$WORKFLOW" 2>/dev/null || echo "null")

  assert_eq "build-base-images has packages: write" "write" "$base_perms"
  assert_eq "build-service-images has packages: write" "write" "$service_perms"
  assert_eq "smoke-test has packages: read (least privilege)" "read" "$smoke_perms"

  # smoke-test also needs contents: read for checkout (source-refs.yaml + patch counting)
  local smoke_contents_perms
  smoke_contents_perms=$(yq_raw '.jobs["smoke-test"]["permissions"]["contents"]' "$WORKFLOW" 2>/dev/null || echo "null")
  assert_eq "smoke-test has contents: read (for checkout)" "read" "$smoke_contents_perms"
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

# --- REQ-002, REQ-003, REQ-007: Three jobs defined ---
test_three_jobs_defined() {
  echo "Test: three jobs defined (REQ-002, REQ-003, REQ-007)"

  assert_file_contains "build-base-images job defined" "$WORKFLOW" "build-base-images:"
  assert_file_contains "build-service-images job defined" "$WORKFLOW" "build-service-images:"
  assert_file_contains "smoke-test job defined" "$WORKFLOW" "smoke-test:"
}

# --- REQ-002: Base images build with multi-arch platforms ---
test_base_images_multi_arch() {
  echo "Test: base images use multi-arch platforms (REQ-002)"

  # Both build-push-action steps in build-base-images must specify platforms: linux/amd64,linux/arm64
  local platforms
  platforms=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.with.platforms) | .with.platforms' "$WORKFLOW" 2>/dev/null || true)

  if [ -z "$platforms" ]; then
    echo "  FAIL: build-base-images has no platforms values"
    FAIL=$((FAIL + 1))
    return
  fi

  local all_multiarch=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "linux/amd64,linux/arm64" ]; then
      echo "  FAIL: build-base-images platform is not multi-arch: $val"
      FAIL=$((FAIL + 1))
      all_multiarch=false
    fi
  done <<< "$platforms"

  if $all_multiarch; then
    echo "  PASS: all build-base-images steps use linux/amd64,linux/arm64"
    PASS=$((PASS + 1))
  fi
}

# --- REQ-003: Base image outputs contain digest references ---
test_base_image_digest_outputs() {
  echo "Test: base image outputs contain digest references (REQ-003)"

  local python_output venv_output
  python_output=$(yq_raw '.jobs["build-base-images"]["outputs"]["python-base-image"]' "$WORKFLOW" 2>/dev/null || true)
  venv_output=$(yq_raw '.jobs["build-base-images"]["outputs"]["venv-builder-image"]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "python-base-image output references digest" "$python_output" "outputs.digest"
  assert_contains "venv-builder-image output references digest" "$venv_output" "outputs.digest"
}

# --- REQ-003: build-service-images depends on build-base-images ---
test_service_images_depend_on_base() {
  echo "Test: build-service-images depends on build-base-images (REQ-003)"

  local needs
  needs=$(yq_raw '.jobs["build-service-images"]["needs"][]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "build-service-images needs build-base-images" "$needs" "build-base-images"
}

# --- REQ-004: Matrix includes service and release ---
test_matrix_includes_service_and_release() {
  echo "Test: matrix includes service and release (REQ-004)"

  local services releases
  services=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]["service"][]' "$WORKFLOW" 2>/dev/null || true)
  releases=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]["release"][]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "matrix includes keystone service" "$services" "keystone"
  assert_contains "matrix includes 2025.2 release" "$releases" "2025.2"
}

# --- REQ-004: Source ref resolution step exists ---
test_source_ref_resolution_step() {
  echo "Test: source ref resolution step with yq (REQ-004)"

  local source_ref_step
  source_ref_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "source-ref") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "source-ref step uses yq to resolve ref" "$source_ref_step" "yq"
  assert_contains "source-ref step reads source-refs.yaml" "$source_ref_step" "source-refs.yaml"
}

# --- REQ-004: Conditional patch application step ---
test_patch_application_step() {
  echo "Test: conditional patch application with hashFiles guard (REQ-004)"

  local patch_if
  patch_if=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name == "Apply patches") | .if' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "patch step uses hashFiles guard" "$patch_if" "hashFiles"
}

# --- REQ-004: Constraint overrides step ---
test_constraint_overrides_step() {
  echo "Test: constraint overrides step references apply-constraint-overrides.sh (REQ-004)"

  local overrides_run
  overrides_run=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name == "Apply constraint overrides") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "constraint overrides step runs apply-constraint-overrides.sh" "$overrides_run" "apply-constraint-overrides.sh"
}

# --- REQ-004: Four build-contexts for service images ---
test_build_contexts_for_service_images() {
  echo "Test: build-contexts for service images (REQ-004)"

  local build_contexts
  build_contexts=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with["build-contexts"]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "build-context includes python-base" "$build_contexts" "python-base="
  assert_contains "build-context includes venv-builder" "$build_contexts" "venv-builder="
  assert_contains "build-context includes service source" "$build_contexts" "matrix.service"
  assert_contains "build-context includes upper-constraints" "$build_contexts" "upper-constraints="
}

# --- REQ-005: Tag schema composite ---
test_tag_schema_composite() {
  echo "Test: tag schema composite (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "composite tag has version component" "$tags_step" 'VERSION'
  assert_contains "composite tag has patch count (pN)" "$tags_step" '-p${PATCH_COUNT}'
  assert_contains "composite tag has branch component" "$tags_step" '${BRANCH}'
  assert_contains "composite tag has SHA component" "$tags_step" '${SHORT_SHA}'
}

# --- REQ-005: Branch sanitization (slash-to-dash) ---
test_branch_sanitization() {
  echo "Test: branch sanitization replaces slashes with dashes (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" 2>/dev/null || true)

  # GITHUB_REF_NAME//\//-  is the bash pattern substitution for slash-to-dash
  assert_contains "branch sanitization uses slash-to-dash replacement" "$tags_step" 'GITHUB_REF_NAME//\//-'
}

# --- REQ-005: Version and SHA tag outputs emitted ---
test_version_and_sha_outputs() {
  echo "Test: version= and sha= outputs emitted (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "version output emitted" "$tags_step" 'echo "version='
  assert_contains "sha output emitted" "$tags_step" 'echo "sha='
}

# --- REQ-006: PR uses single-arch, load, and conditional push/platforms ---
test_pr_single_arch_load() {
  echo "Test: PR uses single-arch, load, and conditional push/platforms (REQ-006)"

  local platforms load_val push_val
  platforms=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.platforms' "$WORKFLOW" 2>/dev/null || true)
  load_val=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.load' "$WORKFLOW" 2>/dev/null || true)
  push_val=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.push' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "platforms uses pull_request conditional for single-arch" "$platforms" "pull_request"
  assert_contains "platforms includes linux/amd64 for PR" "$platforms" "linux/amd64"
  assert_contains "load conditioned on pull_request" "$load_val" "pull_request"
  assert_contains "push conditioned on not pull_request" "$push_val" "pull_request"
}

# --- REQ-007: Smoke test uses dynamic service-manage command ---
test_smoke_test_command() {
  echo "Test: smoke test uses dynamic service-manage command (REQ-007)"

  # PR smoke test uses MATRIX_SERVICE env var for dynamic dispatch
  assert_file_contains "PR smoke test uses MATRIX_SERVICE env var" "$WORKFLOW" 'MATRIX_SERVICE: \${{ matrix.service }}'
  assert_file_contains "smoke test uses dynamic manage command" "$WORKFLOW" '${MATRIX_SERVICE}-manage'
}

# --- REQ-007: smoke-test depends on build-service-images ---
test_smoke_test_depends_on_service_images() {
  echo "Test: smoke-test depends on build-service-images (REQ-007)"

  local needs
  needs=$(yq_raw '.jobs["smoke-test"]["needs"][]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "smoke-test needs build-service-images" "$needs" "build-service-images"
}

# --- REQ-007: smoke-test has its own matrix strategy for multi-service support ---
test_smoke_test_has_matrix() {
  echo "Test: smoke-test has its own matrix strategy (REQ-007)"

  local services
  services=$(yq_raw '.jobs["smoke-test"]["strategy"]["matrix"]["service"][]' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "smoke-test matrix includes keystone" "$services" "keystone"
}

# --- REQ-007: smoke-test derives image ref independently ---
test_smoke_test_derives_image_ref() {
  echo "Test: smoke-test derives image ref via tags step (REQ-007)"

  local derive_step
  derive_step=$(yq_raw '.jobs["smoke-test"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "smoke-test derives VERSION from source-refs.yaml" "$derive_step" "source-refs.yaml"
  assert_contains "smoke-test computes image-ref output" "$derive_step" "image-ref="
}

# --- REQ-008: All actions pinned to 40-char hex SHA ---
test_actions_pinned_to_sha() {
  echo "Test: all actions pinned to SHA (REQ-008)"

  local all_pinned=true

  while IFS= read -r line; do
    [ -z "$line" ] && continue
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
  echo "Test: all jobs have timeout-minutes (REQ-008)"

  local base_timeout service_timeout smoke_timeout
  base_timeout=$(yq_raw '.jobs["build-base-images"]["timeout-minutes"]' "$WORKFLOW" 2>/dev/null || echo "null")
  service_timeout=$(yq_raw '.jobs["build-service-images"]["timeout-minutes"]' "$WORKFLOW" 2>/dev/null || echo "null")
  smoke_timeout=$(yq_raw '.jobs["smoke-test"]["timeout-minutes"]' "$WORKFLOW" 2>/dev/null || echo "null")

  if [ "$base_timeout" != "null" ] && [ -n "$base_timeout" ]; then
    echo "  PASS: build-base-images has timeout-minutes: $base_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-base-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$service_timeout" != "null" ] && [ -n "$service_timeout" ]; then
    echo "  PASS: build-service-images has timeout-minutes: $service_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: build-service-images missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi

  if [ "$smoke_timeout" != "null" ] && [ -n "$smoke_timeout" ]; then
    echo "  PASS: smoke-test has timeout-minutes: $smoke_timeout"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: smoke-test missing timeout-minutes"
    FAIL=$((FAIL + 1))
  fi
}

# --- REQ-008: All jobs use runs-on: ubuntu-latest ---
test_runs_on_ubuntu_latest() {
  echo "Test: all jobs use runs-on: ubuntu-latest (REQ-008)"

  local base_runner service_runner smoke_runner
  base_runner=$(yq_raw '.jobs["build-base-images"]["runs-on"]' "$WORKFLOW" 2>/dev/null || echo "null")
  service_runner=$(yq_raw '.jobs["build-service-images"]["runs-on"]' "$WORKFLOW" 2>/dev/null || echo "null")
  smoke_runner=$(yq_raw '.jobs["smoke-test"]["runs-on"]' "$WORKFLOW" 2>/dev/null || echo "null")

  assert_eq "build-base-images uses ubuntu-latest" "ubuntu-latest" "$base_runner"
  assert_eq "build-service-images uses ubuntu-latest" "ubuntu-latest" "$service_runner"
  assert_eq "smoke-test uses ubuntu-latest" "ubuntu-latest" "$smoke_runner"
}

# --- REQ-002: Base images always push unconditionally ---
test_base_images_always_push() {
  echo "Test: base images always push unconditionally (REQ-002)"

  # Check that all build-push-action steps in build-base-images have push: true
  # and that push is not conditioned on event_name (unlike service images).
  # Use the raw YAML file to verify the literal "push: true" value.
  local push_values
  push_values=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.with.push) | .with.push' "$WORKFLOW" 2>/dev/null || true)

  if [ -z "$push_values" ]; then
    echo "  FAIL: build-base-images has no push values"
    FAIL=$((FAIL + 1))
    return
  fi

  local all_true=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "true" ]; then
      echo "  FAIL: build-base-images push is not unconditionally true: $val"
      FAIL=$((FAIL + 1))
      all_true=false
    fi
  done <<< "$push_values"

  if $all_true; then
    echo "  PASS: build-base-images push is unconditionally true"
    PASS=$((PASS + 1))
  fi

  # Verify push is not conditioned on event_name (would be a GHA expression string, not boolean)
  if echo "$push_values" | grep -q 'event_name'; then
    echo "  FAIL: build-base-images push is conditioned on event_name"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: build-base-images push is not conditioned on event_name"
    PASS=$((PASS + 1))
  fi
}

# --- CC-0007: Fork PRs rejected in build-base-images ---
test_fork_pr_rejection_step_exists() {
  echo "Test: fork PR rejection step exists in build-base-images (CC-0007)"

  local reject_step_if
  reject_step_if=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Reject fork PRs") | .if' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "Reject fork PRs step exists with pull_request condition" "$reject_step_if" "pull_request"
  assert_contains "Reject fork PRs step checks head repo full_name" "$reject_step_if" "github.event.pull_request.head.repo.full_name"
  assert_contains "Reject fork PRs step compares against github.repository" "$reject_step_if" "github.repository"
}

# --- CC-0007: Base images have immutable SHA tags alongside :latest ---
test_base_images_have_sha_tags() {
  echo "Test: base images have SHA tags for commit traceability (CC-0007)"

  local python_tags venv_tags
  python_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with.tags' "$WORKFLOW" 2>/dev/null || true)
  venv_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with.tags' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "python-base tags include github.sha" "$python_tags" 'github.sha'
  assert_contains "venv-builder tags include github.sha" "$venv_tags" 'github.sha'
}

# --- CC-0007: Version-only tag restricted to main branch ---
test_version_tag_restricted_to_main() {
  echo "Test: version-only tag restricted to main branch (CC-0007)"

  local tags_block
  tags_block=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.tags' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "version tag line contains ref_name == main conditional" "$tags_block" "github.ref_name == 'main'"
}

# --- CC-0007: Matrix jobs use fail-fast: false for independent failure reporting ---
test_matrix_jobs_fail_fast_false() {
  echo "Test: matrix jobs use fail-fast: false (CC-0007)"

  local service_fail_fast smoke_fail_fast
  service_fail_fast=$(yq_raw '.jobs["build-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" 2>/dev/null || echo "null")
  smoke_fail_fast=$(yq_raw '.jobs["smoke-test"]["strategy"]["fail-fast"]' "$WORKFLOW" 2>/dev/null || echo "null")

  assert_eq "build-service-images has fail-fast: false" "false" "$service_fail_fast"
  assert_eq "smoke-test has fail-fast: false" "false" "$smoke_fail_fast"
}

# --- CC-0007: Source ref resolution validates yq output against null/empty ---
test_source_ref_null_guard() {
  echo "Test: source-ref step validates yq output against null/empty (CC-0007)"

  local source_ref_run
  source_ref_run=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "source-ref") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "source-ref step checks for null string" "$source_ref_run" '"null"'
  assert_contains "source-ref step checks for empty value" "$source_ref_run" '-z "$ref"'
  assert_contains "source-ref step exits on invalid ref" "$source_ref_run" "exit 1"
}

# --- CC-0007: smoke-test tag derivation has sync comment referencing build-service-images ---
test_smoke_test_tag_derivation_sync_comment() {
  echo "Test: smoke-test tag derivation has sync comment (CC-0007)"

  assert_file_contains "smoke-test has sync comment referencing Derive tags step" "$WORKFLOW" "MUST stay in sync with the .Derive tags. step"
}

# --- CC-0007: smoke-test validates yq output against null/empty ---
test_smoke_test_null_guard() {
  echo "Test: smoke-test validates yq output against null/empty (CC-0007)"

  local derive_step
  derive_step=$(yq_raw '.jobs["smoke-test"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" 2>/dev/null || true)

  assert_contains "smoke-test derive step checks for null string" "$derive_step" '"null"'
  assert_contains "smoke-test derive step exits on invalid ref" "$derive_step" "exit 1"
}

# --- REQ-008: Expression injection defense — run: blocks use env vars ---
test_run_blocks_use_env_vars() {
  echo "Test: run: blocks use env vars instead of direct interpolation (REQ-008)"

  assert_file_not_contains "resolve source ref run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'yq .*\${{ matrix'
  assert_file_not_contains "apply patches run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'git -C.*\${{ matrix'
  assert_file_not_contains "apply overrides run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'apply-constraint-overrides.sh \${{ matrix'
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
test_three_jobs_defined
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
test_smoke_test_command
echo ""
test_smoke_test_depends_on_service_images
echo ""
test_smoke_test_has_matrix
echo ""
test_smoke_test_derives_image_ref
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
test_smoke_test_tag_derivation_sync_comment
echo ""
test_smoke_test_null_guard
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
