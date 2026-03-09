#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify build-images workflow structure, conventions, and correctness (CC-0007, CC-0029, CC-0031)
# Requirements: REQ-001 through REQ-017
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

# --- REQ-002, REQ-003, REQ-004, REQ-005, REQ-007: Four jobs defined ---
test_four_jobs_defined() {
  echo "Test: four jobs defined (REQ-002, REQ-003, REQ-004, REQ-005)"

  assert_file_contains "build-base-images job defined" "$WORKFLOW" "build-base-images:"
  assert_file_contains "verify-base-images job defined" "$WORKFLOW" "verify-base-images:"
  assert_file_contains "build-service-images job defined" "$WORKFLOW" "build-service-images:"
  assert_file_contains "verify-service-images job defined" "$WORKFLOW" "verify-service-images:"
}

# --- REQ-004: verify-base-images job depends on build-base-images ---
test_verify_base_images_job() {
  echo "Test: verify-base-images job structure (REQ-004)"

  local needs
  needs=$(yq_raw '.jobs["verify-base-images"]["needs"][]' "$WORKFLOW" || true)
  assert_contains "verify-base-images needs build-base-images" "$needs" "build-base-images"

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

  # Both build-push-action steps in build-base-images must specify platforms: linux/amd64,linux/arm64
  local platforms
  platforms=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.with.platforms) | .with.platforms' "$WORKFLOW" || true)

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
  python_output=$(yq_raw '.jobs["build-base-images"]["outputs"]["python-base-image"]' "$WORKFLOW" || true)
  venv_output=$(yq_raw '.jobs["build-base-images"]["outputs"]["venv-builder-image"]' "$WORKFLOW" || true)

  assert_contains "python-base-image output references digest" "$python_output" "outputs.digest"
  assert_contains "venv-builder-image output references digest" "$venv_output" "outputs.digest"
}

# --- REQ-003, REQ-004: build-service-images depends on build-base-images and verify-base-images ---
test_service_images_depend_on_base() {
  echo "Test: build-service-images depends on build-base-images and verify-base-images (REQ-003, REQ-004)"

  local needs
  needs=$(yq_raw '.jobs["build-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "build-service-images needs build-base-images" "$needs" "build-base-images"
  assert_contains "build-service-images needs verify-base-images" "$needs" "verify-base-images"
}

# --- REQ-004: Matrix includes service and release ---
test_matrix_includes_service_and_release() {
  echo "Test: matrix includes service and release (REQ-004)"

  local services releases
  services=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]["service"][]' "$WORKFLOW" || true)
  releases=$(yq_raw '.jobs["build-service-images"]["strategy"]["matrix"]["release"][]' "$WORKFLOW" || true)

  assert_contains "matrix includes keystone service" "$services" "keystone"
  assert_contains "matrix includes 2025.2 release" "$releases" "2025.2"
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
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" || true)

  assert_contains "composite tag has version component" "$tags_step" 'VERSION'
  assert_contains "composite tag has patch count (pN)" "$tags_step" '-p${PATCH_COUNT}'
  assert_contains "composite tag has branch component" "$tags_step" '${BRANCH}'
  assert_contains "composite tag has SHA component" "$tags_step" '${SHORT_SHA}'
}

# --- REQ-005: Branch sanitization (slash-to-dash) ---
test_branch_sanitization() {
  echo "Test: branch sanitization replaces slashes with dashes (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" || true)

  # GITHUB_REF_NAME//\//-  is the bash pattern substitution for slash-to-dash
  assert_contains "branch sanitization uses slash-to-dash replacement" "$tags_step" 'GITHUB_REF_NAME//\//-'
}

# --- REQ-005: Version and SHA tag outputs emitted ---
test_version_and_sha_outputs() {
  echo "Test: version= and sha= outputs emitted (REQ-005)"

  local tags_step
  tags_step=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" || true)

  assert_contains "version output emitted" "$tags_step" 'echo "version='
  assert_contains "sha output emitted" "$tags_step" 'echo "sha='
  assert_contains "image output emitted" "$tags_step" 'echo "image='
}

# --- REQ-006: PR uses single-arch, load, and conditional push/platforms ---
test_pr_single_arch_load() {
  echo "Test: PR uses single-arch, load, and conditional push/platforms (REQ-006)"

  local platforms load_val push_val
  platforms=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.platforms' "$WORKFLOW" || true)
  load_val=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.load' "$WORKFLOW" || true)
  push_val=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.push' "$WORKFLOW" || true)

  assert_contains "platforms uses pull_request conditional for single-arch" "$platforms" "pull_request"
  assert_contains "platforms includes linux/amd64 for PR" "$platforms" "linux/amd64"
  assert_contains "load conditioned on pull_request" "$load_val" "pull_request"
  assert_contains "push conditioned on not pull_request" "$push_val" "pull_request"
}

# --- REQ-007: verify-service-images uses verify_keystone.sh ---
test_verify_service_images_command() {
  echo "Test: verify-service-images uses verify_keystone.sh (REQ-007)"

  # verify-service-images uses MATRIX_SERVICE env var for tag derivation
  assert_file_contains "verify-service-images uses MATRIX_SERVICE env var" "$WORKFLOW" 'MATRIX_SERVICE: \${{ matrix.service }}'
  assert_file_contains "verify-service-images runs verify_keystone.sh" "$WORKFLOW" 'verify_keystone.sh'
}

# --- REQ-007: verify-service-images depends on build-service-images ---
test_verify_service_images_depends_on_service_images() {
  echo "Test: verify-service-images depends on build-service-images (REQ-007)"

  local needs
  needs=$(yq_raw '.jobs["verify-service-images"]["needs"][]' "$WORKFLOW" || true)

  assert_contains "verify-service-images needs build-service-images" "$needs" "build-service-images"
}

# --- REQ-007: verify-service-images has its own matrix strategy for multi-service support ---
test_verify_service_images_has_matrix() {
  echo "Test: verify-service-images has its own matrix strategy (REQ-007)"

  local services
  services=$(yq_raw '.jobs["verify-service-images"]["strategy"]["matrix"]["service"][]' "$WORKFLOW" || true)

  assert_contains "verify-service-images matrix includes keystone" "$services" "keystone"
}

# --- REQ-007: verify-service-images derives image ref independently ---
test_verify_service_images_derives_image_ref() {
  echo "Test: verify-service-images derives image ref via tags step (REQ-007)"

  local derive_step
  derive_step=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" || true)

  assert_contains "verify-service-images derives VERSION from source-refs.yaml" "$derive_step" "source-refs.yaml"
  assert_contains "verify-service-images computes image-ref output" "$derive_step" "image-ref="
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

  local base_timeout verify_base_timeout service_timeout verify_service_timeout
  base_timeout=$(yq_raw '.jobs["build-base-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  verify_base_timeout=$(yq_raw '.jobs["verify-base-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
  service_timeout=$(yq_raw '.jobs["build-service-images"]["timeout-minutes"]' "$WORKFLOW" || echo "null")
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
  echo "Test: all jobs use runs-on: ubuntu-latest (REQ-008)"

  local base_runner verify_base_runner service_runner verify_service_runner
  base_runner=$(yq_raw '.jobs["build-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  verify_base_runner=$(yq_raw '.jobs["verify-base-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  service_runner=$(yq_raw '.jobs["build-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")
  verify_service_runner=$(yq_raw '.jobs["verify-service-images"]["runs-on"]' "$WORKFLOW" || echo "null")

  assert_eq "build-base-images uses ubuntu-latest" "ubuntu-latest" "$base_runner"
  assert_eq "verify-base-images uses ubuntu-latest" "ubuntu-latest" "$verify_base_runner"
  assert_eq "build-service-images uses ubuntu-latest" "ubuntu-latest" "$service_runner"
  assert_eq "verify-service-images uses ubuntu-latest" "ubuntu-latest" "$verify_service_runner"
}

# --- REQ-002: Base images always push unconditionally ---
test_base_images_always_push() {
  echo "Test: base images always push unconditionally (REQ-002)"

  # Check that all build-push-action steps in build-base-images have push: true
  # and that push is not conditioned on event_name (unlike service images).
  # Use the raw YAML file to verify the literal "push: true" value.
  local push_values
  push_values=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.with.push) | .with.push' "$WORKFLOW" || true)

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
  reject_step_if=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Reject fork PRs") | .if' "$WORKFLOW" || true)

  assert_contains "Reject fork PRs step exists with pull_request condition" "$reject_step_if" "pull_request"
  assert_contains "Reject fork PRs step checks head repo full_name" "$reject_step_if" "github.event.pull_request.head.repo.full_name"
  assert_contains "Reject fork PRs step compares against github.repository" "$reject_step_if" "github.repository"
}

# --- CC-0007: Base images have immutable SHA tags alongside :latest ---
test_base_images_have_sha_tags() {
  echo "Test: base images have SHA tags for commit traceability (CC-0007)"

  local python_tags venv_tags
  python_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-python-base") | .with.tags' "$WORKFLOW" || true)
  venv_tags=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.id == "build-venv-builder") | .with.tags' "$WORKFLOW" || true)

  assert_contains "python-base tags include github.sha" "$python_tags" 'github.sha'
  assert_contains "venv-builder tags include github.sha" "$venv_tags" 'github.sha'
}

# --- CC-0007: Version-only tag restricted to main branch ---
test_version_tag_restricted_to_main() {
  echo "Test: version-only tag restricted to main branch (CC-0007)"

  local tags_block
  tags_block=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.id == "build-service") | .with.tags' "$WORKFLOW" || true)

  assert_contains "version tag line contains ref_name == main conditional" "$tags_block" "github.ref_name == 'main'"
}

# --- CC-0007: Matrix jobs use fail-fast: false for independent failure reporting ---
test_matrix_jobs_fail_fast_false() {
  echo "Test: matrix jobs use fail-fast: false (CC-0007)"

  local service_fail_fast verify_service_fail_fast
  service_fail_fast=$(yq_raw '.jobs["build-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")
  verify_service_fail_fast=$(yq_raw '.jobs["verify-service-images"]["strategy"]["fail-fast"]' "$WORKFLOW" || echo "null")

  assert_eq "build-service-images has fail-fast: false" "false" "$service_fail_fast"
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

# --- CC-0007: verify-service-images tag derivation has sync comment referencing build-service-images ---
test_verify_service_images_tag_derivation_sync_comment() {
  echo "Test: verify-service-images tag derivation has sync comment (CC-0007)"

  assert_file_contains "verify-service-images has sync comment referencing Derive tags step" "$WORKFLOW" "MUST stay in sync with the .Derive tags. step"
}

# --- CC-0007: verify-service-images validates yq output against null/empty ---
test_verify_service_images_null_guard() {
  echo "Test: verify-service-images validates yq output against null/empty (CC-0007)"

  local derive_step
  derive_step=$(yq_raw '.jobs["verify-service-images"]["steps"][] | select(.id == "tags") | .run' "$WORKFLOW" || true)

  assert_contains "verify-service-images derive step checks for null string" "$derive_step" '"null"'
  assert_contains "verify-service-images derive step exits on invalid ref" "$derive_step" "exit 1"
}

# --- REQ-008: Expression injection defense — run: blocks use env vars ---
test_run_blocks_use_env_vars() {
  echo "Test: run: blocks use env vars instead of direct interpolation (REQ-008)"

  assert_file_not_contains "resolve source ref run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'yq .*\${{ matrix'
  assert_file_not_contains "apply patches run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'git -C.*\${{ matrix'
  assert_file_not_contains "apply overrides run uses env vars (no direct matrix interpolation)" "$WORKFLOW" 'apply-constraint-overrides.sh \${{ matrix'
}

# --- CC-0029: SBOM permissions on build-base-images (REQ-012) ---
test_sbom_permissions_on_build_base_images() {
  echo "Test: SBOM permissions on build-base-images (CC-0029, REQ-012)"

  local id_token attestations
  id_token=$(yq_raw '.jobs["build-base-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["build-base-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "build-base-images has id-token: write" "write" "$id_token"
  assert_eq "build-base-images has attestations: write" "write" "$attestations"
}

# --- CC-0029: SBOM permissions on build-service-images (REQ-012) ---
test_sbom_permissions_on_build_service_images() {
  echo "Test: SBOM permissions on build-service-images (CC-0029, REQ-012)"

  local id_token attestations
  id_token=$(yq_raw '.jobs["build-service-images"]["permissions"]["id-token"]' "$WORKFLOW" || echo "null")
  attestations=$(yq_raw '.jobs["build-service-images"]["permissions"]["attestations"]' "$WORKFLOW" || echo "null")

  assert_eq "build-service-images has id-token: write" "write" "$id_token"
  assert_eq "build-service-images has attestations: write" "write" "$attestations"
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
}

# --- CC-0029: SBOM generation steps exist (REQ-010) ---
test_sbom_generation_steps_exist() {
  echo "Test: SBOM generation steps exist in both build jobs (CC-0029, REQ-010)"

  # build-base-images should have 2 SBOM generation steps (python-base, venv-builder)
  local base_sbom_count
  base_sbom_count=$(yq_count '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .uses' "$WORKFLOW")
  assert_eq "build-base-images has 2 SBOM generation steps" "2" "$base_sbom_count"

  # build-service-images should have 1 SBOM generation step
  local service_sbom_count
  service_sbom_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images has 1 SBOM generation step" "1" "$service_sbom_count"
}

# --- CC-0029: SBOM format is cyclonedx-json (REQ-010) ---
test_sbom_format_cyclonedx_json() {
  echo "Test: SBOM format is cyclonedx-json (CC-0029, REQ-010)"

  # All SBOM generation steps in build-base-images must use cyclonedx-json
  local base_formats
  base_formats=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.format' "$WORKFLOW" || true)

  local all_cyclonedx=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "cyclonedx-json" ]; then
      echo "  FAIL: build-base-images SBOM format is not cyclonedx-json: $val"
      FAIL=$((FAIL + 1))
      all_cyclonedx=false
    fi
  done <<< "$base_formats"

  if $all_cyclonedx && [ -n "$base_formats" ]; then
    echo "  PASS: all build-base-images SBOM steps use cyclonedx-json"
    PASS=$((PASS + 1))
  elif [ -z "$base_formats" ]; then
    echo "  FAIL: build-base-images SBOM format check found no steps"
    FAIL=$((FAIL + 1))
  fi

  # build-service-images SBOM step
  local service_format
  service_format=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with.format' "$WORKFLOW" || true)
  assert_eq "build-service-images SBOM format is cyclonedx-json" "cyclonedx-json" "$service_format"
}

# --- CC-0029: SBOM generation steps disable artifact upload (REQ-010) ---
test_sbom_no_artifact_upload() {
  echo "Test: SBOM generation steps set upload-artifact: false (CC-0029, REQ-010)"

  # All SBOM generation steps in build-base-images must set upload-artifact: false
  local base_upload_values
  base_upload_values=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with["upload-artifact"]' "$WORKFLOW" || true)

  local all_false=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "false" ]; then
      echo "  FAIL: build-base-images SBOM step does not set upload-artifact: false: $val"
      FAIL=$((FAIL + 1))
      all_false=false
    fi
  done <<< "$base_upload_values"

  if $all_false && [ -n "$base_upload_values" ]; then
    echo "  PASS: all build-base-images SBOM steps set upload-artifact: false"
    PASS=$((PASS + 1))
  elif [ -z "$base_upload_values" ]; then
    echo "  FAIL: no build-base-images SBOM steps found for upload-artifact check"
    FAIL=$((FAIL + 1))
  fi

  # build-service-images SBOM step
  local service_upload
  service_upload=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action"))) | .with["upload-artifact"]' "$WORKFLOW" || true)
  assert_eq "build-service-images SBOM step sets upload-artifact: false" "false" "$service_upload"
}

# --- CC-0029: SBOM generation references correct digest (REQ-015) ---
test_sbom_generation_references_digest() {
  echo "Test: SBOM generation steps reference correct digest (CC-0029, REQ-015)"

  # python-base SBOM references build-python-base digest
  local python_base_image
  python_base_image=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Generate SBOM for python-base") | .with.image' "$WORKFLOW" || true)
  assert_contains "python-base SBOM references build-python-base digest" "$python_base_image" "build-python-base.outputs.digest"

  # venv-builder SBOM references build-venv-builder digest
  local venv_builder_image
  venv_builder_image=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.name == "Generate SBOM for venv-builder") | .with.image' "$WORKFLOW" || true)
  assert_contains "venv-builder SBOM references build-venv-builder digest" "$venv_builder_image" "build-venv-builder.outputs.digest"

  # service SBOM references build-service digest
  local service_image
  service_image=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.name == "Generate SBOM for service image") | .with.image' "$WORKFLOW" || true)
  assert_contains "service SBOM uses tags.outputs.image" "$service_image" "steps.tags.outputs.image"
  assert_contains "service SBOM references build-service digest" "$service_image" "build-service.outputs.digest"
}

# --- CC-0029: SBOM attestation steps exist (REQ-011) ---
test_sbom_attestation_steps_exist() {
  echo "Test: SBOM attestation steps exist in both build jobs (CC-0029, REQ-011)"

  # build-base-images should have 2 attestation steps
  local base_attest_count
  base_attest_count=$(yq_count '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .uses' "$WORKFLOW")
  assert_eq "build-base-images has 2 attestation steps" "2" "$base_attest_count"

  # build-service-images should have 1 attestation step
  local service_attest_count
  service_attest_count=$(yq_count '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .uses' "$WORKFLOW")
  assert_eq "build-service-images has 1 attestation step" "1" "$service_attest_count"
}

# --- CC-0029: SBOM attestation push-to-registry (REQ-016) ---
test_sbom_attestation_push_to_registry() {
  echo "Test: SBOM attestation push-to-registry is true (CC-0029, REQ-016)"

  # All attestation steps in build-base-images
  local base_push_values
  base_push_values=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)

  local all_true=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [ "$val" != "true" ]; then
      echo "  FAIL: build-base-images attestation push-to-registry is not true: $val"
      FAIL=$((FAIL + 1))
      all_true=false
    fi
  done <<< "$base_push_values"

  if $all_true && [ -n "$base_push_values" ]; then
    echo "  PASS: all build-base-images attestation steps have push-to-registry: true"
    PASS=$((PASS + 1))
  elif [ -z "$base_push_values" ]; then
    echo "  FAIL: no build-base-images attestation steps found (empty yq result)"
    FAIL=$((FAIL + 1))
  fi

  # build-service-images attestation step
  local service_push
  service_push=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("actions/attest@"))) | .with["push-to-registry"]' "$WORKFLOW" || true)
  assert_eq "build-service-images attestation push-to-registry is true" "true" "$service_push"
}

# --- CC-0029: SBOM/attestation steps have PR-skip guard (REQ-013) ---
test_sbom_steps_pr_skip_guard() {
  echo "Test: SBOM/attestation steps have PR-skip guard (CC-0029, REQ-013)"

  # All SBOM steps in build-base-images must have PR guard
  local base_sbom_ifs
  base_sbom_ifs=$(yq_raw '.jobs["build-base-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action|actions/attest"))) | .if' "$WORKFLOW" || true)

  local all_guarded=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"github.event_name != 'pull_request'"* ]]; then
      echo "  FAIL: build-base-images SBOM/attestation step missing PR guard: $val"
      FAIL=$((FAIL + 1))
      all_guarded=false
    fi
  done <<< "$base_sbom_ifs"

  if $all_guarded && [ -n "$base_sbom_ifs" ]; then
    echo "  PASS: all build-base-images SBOM/attestation steps have PR-skip guard"
    PASS=$((PASS + 1))
  elif [ -z "$base_sbom_ifs" ]; then
    echo "  FAIL: build-base-images SBOM/attestation steps not found or missing PR guard"
    FAIL=$((FAIL + 1))
  else
    echo "  FAIL: some build-base-images SBOM/attestation steps missing PR-skip guard (see above)"
  fi

  # All SBOM steps in build-service-images must have PR guard
  local service_sbom_ifs
  service_sbom_ifs=$(yq_raw '.jobs["build-service-images"]["steps"][] | select(.uses and (.uses | test("anchore/sbom-action|actions/attest"))) | .if' "$WORKFLOW" || true)

  local service_all_guarded=true
  while IFS= read -r val; do
    [ -z "$val" ] && continue
    if [[ "$val" != *"github.event_name != 'pull_request'"* ]]; then
      echo "  FAIL: build-service-images SBOM/attestation step missing PR guard: $val"
      FAIL=$((FAIL + 1))
      service_all_guarded=false
    fi
  done <<< "$service_sbom_ifs"

  if $service_all_guarded && [ -n "$service_sbom_ifs" ]; then
    echo "  PASS: all build-service-images SBOM/attestation steps have PR-skip guard"
    PASS=$((PASS + 1))
  elif [ -z "$service_sbom_ifs" ]; then
    echo "  FAIL: build-service-images SBOM/attestation steps not found or missing PR guard"
    FAIL=$((FAIL + 1))
  else
    echo "  FAIL: some build-service-images SBOM/attestation steps missing PR-skip guard (see above)"
  fi
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
test_four_jobs_defined
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
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
