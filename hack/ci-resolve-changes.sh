#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-changes.sh — Resolve effective CI changes for matrix generation.
# Feature: CC-0050
#
# Reads paths-filter outputs (passed as FILTER_* env vars) and determines which
# operators changed. Emits outputs to GITHUB_OUTPUT for downstream jobs.
#
# On tag pushes the full release pipeline runs for all operators regardless of
# which files were touched.
#
# Required env vars:
#   ALL_OPERATORS    — Space-separated list of known operators (e.g. "keystone")
#   GITHUB_OUTPUT    — GitHub Actions output file (set automatically by Actions)
#   GITHUB_REF       — Git ref (set automatically by Actions)
#
# Per-operator filter outputs must be set as FILTER_<operator> env vars:
#   FILTER_keystone           — paths-filter output for keystone paths
#   FILTER_c5c3               — paths-filter output for c5c3 paths (CC-0110)
#   FILTER_docs               — paths-filter output for docs paths
#   FILTER_helm               — paths-filter output for helm paths
#   FILTER_e2e_infra          — paths-filter output for e2e-infra paths
#   FILTER_e2e_chaos          — paths-filter output for e2e-chaos paths (CC-0054)
#   FILTER_e2e_prometheus     — paths-filter output for e2e-prometheus paths (CC-0100)
#   FILTER_tests_e2e_operator — paths-filter output for tests/e2e/** (CC-0054 follow-up)
#   FILTER_tests_tempest      — paths-filter output for tests/tempest/** (CC-0054 follow-up)
#   FILTER_go_common          — paths-filter output for go_common paths
#
# To add a new operator (e.g. glance):
#   1. Add a filter block in ci.yaml (glance: ...)
#   2. Add the operator name to ALL_OPERATORS in the ci.yaml step env block
#   3. Add a matching FILTER_glance env var in the ci.yaml step env block
#
# REQ-001: Extracted from ci.yaml to reduce workflow file size (CC-0050, review #2 comment 3).
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

if [[ -z "${ALL_OPERATORS:-}" ]]; then
  echo "::error::ALL_OPERATORS must be set (space-separated list of operator names)"
  exit 1
fi

ops=()
go_changed=false
any_e2e_tests=false

# CC-0054 follow-up: Any change to an E2E test definition (infrastructure,
# operator, chaos, tempest) should trigger the full E2E suite so that
# refactoring test infra is validated end-to-end. This is independent of
# go_changed so lint/unit/format/vuln stay skipped on pure test-only edits.
if [[ "${FILTER_tests_e2e_operator:-false}" == "true" || \
      "${FILTER_tests_tempest:-false}" == "true" || \
      "${FILTER_e2e_infra:-false}" == "true" || \
      "${FILTER_e2e_chaos:-false}" == "true" ]]; then
  any_e2e_tests=true
fi

if [[ "${GITHUB_REF}" == refs/tags/v* ]]; then
  # Tag push: run the full release pipeline for all known operators.
  go_changed=true
  read -ra ops <<< "$ALL_OPERATORS"
  echo "docs=true"      >> "$GITHUB_OUTPUT"
  echo "helm=true"      >> "$GITHUB_OUTPUT"
  echo "e2e-infra=true" >> "$GITHUB_OUTPUT"
else
  echo "docs=${FILTER_docs:-false}" >> "$GITHUB_OUTPUT"
  echo "helm=${FILTER_helm:-false}" >> "$GITHUB_OUTPUT"
  # e2e-infra runs when its own paths change or any E2E test definition changes.
  if [[ "${FILTER_e2e_infra:-false}" == "true" || "$any_e2e_tests" == "true" ]]; then
    echo "e2e-infra=true"  >> "$GITHUB_OUTPUT"
  else
    echo "e2e-infra=false" >> "$GITHUB_OUTPUT"
  fi

  if [[ "${FILTER_go_common:-false}" == "true" ]]; then
    # Shared-code change → all operators are potentially affected.
    go_changed=true
    read -ra ops <<< "$ALL_OPERATORS"
  else
    # Operator-specific change → include only changed operators.
    for op in $ALL_OPERATORS; do
      filter_var="FILTER_${op}"
      # Indirect expansion with default so an operator listed in ALL_OPERATORS
      # before its FILTER_<op> env var is wired up doesn't trip `set -u`.
      if [[ "${!filter_var:-false}" == "true" ]]; then
        go_changed=true
        ops+=("$op")
      fi
    done
  fi

  # E2E test-only changes: go_changed stays false, but all operators must
  # run their E2E suite so the edited test definitions are exercised.
  if [[ "$any_e2e_tests" == "true" && ${#ops[@]} -eq 0 ]]; then
    read -ra ops <<< "$ALL_OPERATORS"
  fi
fi

echo "go=${go_changed}" >> "$GITHUB_OUTPUT"

# CC-0054: Chaos E2E tests run when chaos test definitions change, when any
# Go code changes (so chaos validates the current operator code), or when any
# other E2E test definition changes (so refactoring shared test infra runs
# the full E2E suite end-to-end).
if [[ "$go_changed" == "true" || "${FILTER_e2e_chaos:-false}" == "true" || "$any_e2e_tests" == "true" ]]; then
  echo "e2e-chaos=true" >> "$GITHUB_OUTPUT"
else
  echo "e2e-chaos=false" >> "$GITHUB_OUTPUT"
fi

# CC-0100, REQ-010: Prometheus stack E2E tests run when prometheus paths change
# (kind overlay, suite, deploy/composite/operator wiring), when any Go code
# changes (so the suite re-validates the current operator metrics surface), or
# when any other E2E test definition changes (so refactoring shared test infra
# runs the full E2E suite end-to-end).
if [[ "$go_changed" == "true" || "${FILTER_e2e_prometheus:-false}" == "true" || "$any_e2e_tests" == "true" ]]; then
  echo "e2e-prometheus=true" >> "$GITHUB_OUTPUT"
else
  echo "e2e-prometheus=false" >> "$GITHUB_OUTPUT"
fi

# Emit operator matrix — single codepath for both tag and non-tag.
if [[ ${#ops[@]} -eq 0 ]]; then
  echo "has-e2e-operators=false"                   >> "$GITHUB_OUTPUT"
  # Always emit valid JSON so fromJson() never fails in downstream jobs.
  echo 'e2e-operators={"operator":["__none__"]}'   >> "$GITHUB_OUTPUT"
else
  matrix=$(printf '%s\n' "${ops[@]}" | jq -Rnc '[inputs] | {operator: .}')
  echo "has-e2e-operators=true"                    >> "$GITHUB_OUTPUT"
  echo "e2e-operators=${matrix}"                   >> "$GITHUB_OUTPUT"
fi
