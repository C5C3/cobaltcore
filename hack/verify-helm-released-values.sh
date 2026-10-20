#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/verify-helm-released-values.sh — Render every operator chart with the
# values.yaml of its released chart and fail when the chart rejects them.
#
# Every operator chart's values.schema.json rejects unknown keys, so a value the
# in-repo chart renamed or removed since its released chart stops the
# `helm upgrade` of every deployment that still sets it. This script renders the
# in-repo chart with the released chart's values.yaml as a user values file
# (`helm template test <chart> -f <released values.yaml>`) and fails the chart
# when the render fails, whether the schema or a template `fail` rejects it.
#
# "Released" is the highest semver tag at oci://ghcr.io/c5c3/charts/<chart>, the
# tag hack/ci-fetch-released-operator.sh and the Flux HelmReleases resolve too.
# The pull is anonymous, and only Chart.yaml and values.yaml are read from the
# pulled chart. A chart that was never published is skipped with a notice.
#
# An intended removal is waived in hack/helm-removed-values.yaml: the waived keys
# are deleted from the released values before the render. A waived key the
# released values no longer carry raises a warning, so its entry can be deleted.
#
# Usage: hack/verify-helm-released-values.sh [operator...]
#   With no operator, every chart of the operators/*/helm/*-operator glob.
#
# Optional env vars:
#   REGISTRY                 — OCI registry host (default: ghcr.io).
#   REMOVED_VALUES_FILE      — the waiver list (default: hack/helm-removed-values.yaml).
#   RELEASED_VALUES_OUT_DIR  — scratch directory, cleared first (default: _output/released-values).
#   PULL_RETRY_DELAY         — seconds between pull attempts (default: 5).
#
# Every chart is checked before the script exits: 0 when no chart failed (a
# skipped chart counts as passed), 1 otherwise. A malformed waiver list or a bad
# argument exits 1 before anything is pulled.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

REGISTRY="${REGISTRY:-ghcr.io}"
REMOVED_VALUES_FILE="${REMOVED_VALUES_FILE:-hack/helm-removed-values.yaml}"
OUT_DIR="${RELEASED_VALUES_OUT_DIR:-_output/released-values}"
PULL_RETRY_DELAY="${PULL_RETRY_DELAY:-5}"
PULL_ATTEMPTS=3

# A waiver key is a dotted path of values keys. It reaches yq through strenv,
# never through the expression text.
KEY_PATTERN='^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$'

# ---------------------------------------------------------------------------
# 1. Preflight.
# ---------------------------------------------------------------------------
if [[ ! "${PULL_RETRY_DELAY}" =~ ^[0-9]+$ ]]; then
  echo "::error::PULL_RETRY_DELAY must be a whole number of seconds (got: ${PULL_RETRY_DELAY})"
  exit 1
fi
if ! command -v helm >/dev/null 2>&1; then
  echo "::error::helm is not on PATH"
  exit 1
fi
# The yq of Debian and PyPI is a jq wrapper with another expression language.
if ! grep -q mikefarah <<<"$(yq --version 2>&1)"; then
  echo "::error::yq on PATH is not mikefarah/yq v4"
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. Resolve the charts. The waiver list is checked against the whole glob, so
#    ALL_CHARTS is kept beside the charts this run covers.
# ---------------------------------------------------------------------------
shopt -s nullglob
ALL_CHARTS=(operators/*/helm/*-operator)
shopt -u nullglob
if [[ ${#ALL_CHARTS[@]} -eq 0 ]]; then
  echo "::error::no operator chart found under operators/*/helm/"
  exit 1
fi

if [[ $# -eq 0 ]]; then
  CHARTS=("${ALL_CHARTS[@]}")
else
  CHARTS=()
  for op in "$@"; do
    chart="operators/${op}/helm/${op}-operator"
    if [[ ! -f "${chart}/Chart.yaml" ]]; then
      echo "::error::no operator chart at ${chart}"
      exit 1
    fi
    CHARTS+=("${chart}")
  done
fi

# ---------------------------------------------------------------------------
# 3. Validate the waiver list.
# ---------------------------------------------------------------------------

# is_chart_name <name> — Return 0 when <name> is the basename of a chart of the
# operators/*/helm/*-operator glob.
is_chart_name() {
  local chart
  for chart in "${ALL_CHARTS[@]}"; do
    [[ "$(basename "${chart}")" == "$1" ]] && return 0
  done
  return 1
}

# waiver_field <chart> <index> <field> — Echo one field of one waiver entry, or
# nothing when the entry is not a map or lacks the field.
waiver_field() {
  CHART="$1" IDX="$2" FIELD="$3" \
    yq '.[strenv(CHART)][env(IDX)] | select(tag == "!!map") | .[strenv(FIELD)] // ""' "${REMOVED_VALUES_FILE}"
}

# validate_waivers — Exit 1 at the first malformed part of the waiver list. An
# absent, empty or {} file holds no waivers and leaves HAVE_WAIVERS at 0.
HAVE_WAIVERS=0
validate_waivers() {
  local file="${REMOVED_VALUES_FILE}" tag names name count i key reason
  [[ -s "${file}" ]] || return 0

  if ! tag="$(yq 'tag' "${file}" 2>&1)"; then
    echo "${tag}"
    tag=""
  fi
  case "${tag}" in
    '!!null') return 0 ;;
    '!!map') ;;
    *)
      echo "::error::${file} must map chart names to lists of {key, reason} entries"
      exit 1
      ;;
  esac

  names="$(yq 'keys | .[]' "${file}")"
  [[ -n "${names}" ]] || return 0
  while IFS= read -r name; do
    if ! is_chart_name "${name}"; then
      echo "::error::${file} names unknown chart ${name}"
      exit 1
    fi
    if [[ "$(CHART="${name}" yq '.[strenv(CHART)] | tag' "${file}")" != '!!seq' ]]; then
      echo "::error::${file}: ${name} must be a list of {key, reason} entries"
      exit 1
    fi
    count="$(CHART="${name}" yq '.[strenv(CHART)] | length' "${file}")"
    for ((i = 0; i < count; i++)); do
      key="$(waiver_field "${name}" "${i}" key)"
      reason="$(waiver_field "${name}" "${i}" reason)"
      if [[ -z "${key}" || -z "${reason}" ]]; then
        echo "::error::${file}: ${name} entry ${i} needs a key and a non-empty reason"
        exit 1
      fi
      if [[ ! "${key}" =~ ${KEY_PATTERN} ]]; then
        echo "::error::${file}: ${name} key '${key}' is not a dotted values path"
        exit 1
      fi
    done
  done <<<"${names}"
  HAVE_WAIVERS=1
}

validate_waivers

# ---------------------------------------------------------------------------
# 4. Check every chart.
# ---------------------------------------------------------------------------

# check_chart <chart_dir> — Pull the released chart, strip the waived keys from
# its values.yaml and render <chart_dir> with them. Returns 0 when the chart
# passed or was skipped, 1 when it failed. It runs as an `if` condition, where
# set -e does not apply, so the vendoring, the pull and the render each report
# their own failure.
check_chart() {
  local chart="$1" name ref attempt out pulled=0 released version values count i key reason
  name="$(basename "${chart}")"
  ref="oci://${REGISTRY}/c5c3/charts/${name}"

  # The operator-library subchart is gitignored, and its templates change without
  # a version bump, so it is vendored afresh on every run, as make helm-deps does.
  if ! helm dependency build --skip-refresh "${chart}"; then
    echo "::error::failed to vendor the operator-library subchart into ${chart}"
    return 1
  fi

  # helm pull --untar refuses a directory an earlier attempt left behind.
  for ((attempt = 1; attempt <= PULL_ATTEMPTS; attempt++)); do
    rm -rf "${OUT_DIR:?}/${name}"
    if out="$(helm pull "${ref}" --untar --untardir "${OUT_DIR}/${name}" 2>&1)"; then
      pulled=1
      break
    fi
    if grep -q 'name unknown' <<<"${out}"; then
      echo "::notice::${name} has no released chart at ${ref}; nothing to compare"
      return 0
    fi
    if ((attempt < PULL_ATTEMPTS)); then
      echo "::warning::pull of ${ref} failed (attempt ${attempt}/${PULL_ATTEMPTS}): $(tail -n 1 <<<"${out}"); retrying in ${PULL_RETRY_DELAY}s"
      sleep "${PULL_RETRY_DELAY}"
    fi
  done
  if ((pulled == 0)); then
    echo "::error::failed to pull ${ref} after ${PULL_ATTEMPTS} attempts: $(tail -n 1 <<<"${out}")"
    return 1
  fi

  released="${OUT_DIR}/${name}/${name}"
  if [[ ! -f "${released}/Chart.yaml" ]]; then
    echo "::error::pulled ${name} has no Chart.yaml under ${released}"
    return 1
  fi
  version="$(yq '.version' "${released}/Chart.yaml")"
  if [[ ! -f "${released}/values.yaml" ]]; then
    echo "::notice::released ${name} ${version} ships no values.yaml; nothing to compare"
    return 0
  fi

  values="${OUT_DIR}/${name}.values.yaml"
  cp "${released}/values.yaml" "${values}"
  if ((HAVE_WAIVERS)); then
    count="$(CHART="${name}" yq '.[strenv(CHART)] // [] | length' "${REMOVED_VALUES_FILE}")"
    for ((i = 0; i < count; i++)); do
      key="$(waiver_field "${name}" "${i}" key)"
      reason="$(waiver_field "${name}" "${i}" reason)"
      if grep -qxF -- "${key}" <<<"$(yq '.. | path | join(".")' "${values}")"; then
        KEY="${key}" yq -i 'delpaths([strenv(KEY) | split(".")])' "${values}"
        echo "::notice::${name}: ${key} waived (${reason})"
      else
        echo "::warning::${name}: ${key} is not in the values of released ${name} ${version}; delete its entry from ${REMOVED_VALUES_FILE}"
      fi
    done
  fi

  if out="$(helm template test "${chart}" -f "${values}" 2>&1)"; then
    echo "${name} ${version}: OK"
    return 0
  fi
  echo "${out}"
  echo "::error::${name} rejects the values of released ${name} ${version}. If this pull request removes or renames a key the released values set, keep it or waive it in ${REMOVED_VALUES_FILE}; if it narrows a key or adds a template check the released values fail, keep the old contract; if the rejected key is one main added after this run's base, rebase onto main instead of adding a waiver"
  return 1
}

rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}"

failed=0
for chart in "${CHARTS[@]}"; do
  if ! check_chart "${chart}"; then
    failed=$((failed + 1))
  fi
done

if ((failed > 0)); then
  echo "::error::${failed} of ${#CHARTS[@]} operator charts failed the released-values render"
  exit 1
fi
