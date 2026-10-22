#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-collect-e2e-coverage.sh — Collect the operator coverage of an e2e
# leg before its kind cluster is deleted.
#
# The e2e-operator, e2e-chaos and tempest jobs deploy the coverage-instrumented
# operator images with E2E_COVERAGE=true (hack/ci-deploy-operator.sh): every
# manager writes its Go coverage counters every minute to
# COVERAGE_NODE_DIR/<op> on its kind node. This script makes the managers
# flush once more, copies the counters off the nodes, merges them into one
# profile per operator, writes the per-operator totals to the job summary and
# names the profiles and their Codecov flags in GITHUB_OUTPUT.
#
# Usage: hack/ci-collect-e2e-coverage.sh <family>
#   <family>  e2e, chaos or tempest; the profiles are named
#             <family>-<op>.out and the flags <family>-<op>, plus
#             <family>-common for internal/common
#
# Env vars:
#   GITHUB_OUTPUT        — required; GitHub Actions output file
#   GITHUB_STEP_SUMMARY  — the summary file; stdout when unset
#   KIND_CLUSTER         — the kind cluster (default: cobaltcore)
#   SCALE_DOWN           — "true" (default) scales every Deployment whose
#                          manager container sets GOCOVERDIR to 0 replicas and
#                          waits up to 60 s for its pods to go, so each
#                          manager flushes on SIGTERM; any other value copies
#                          what is on the nodes as it is
#
# Steps:
#   1. Final flush (SCALE_DOWN=true). A failed kubectl call or a wait that
#      times out prints ::warning:: and the script carries on.
#   2. Copy. docker cp copies COVERAGE_NODE_DIR of every node kind lists to
#      COVERAGE_OUT/raw/<node>/. A node without the directory prints
#      ::notice:: and is skipped.
#   3. Merge. Each operator directory, whose name must match
#      ^[a-z][a-z0-9-]*$, is gathered from every node and merged with
#      go tool covdata textfmt into COVERAGE_OUT/<family>-<op>.out. A set
#      without a covcounters.* file, a failed merge and a profile with no
#      line beyond mode: are skipped with a ::notice:: or ::warning::.
#   4. Summary. A table of statements, covered statements and coverage per
#      kept profile, counting the blocks under operators/<op>/ alone.
#
# Outputs (GITHUB_OUTPUT):
#   found  — true when at least one profile was kept, false otherwise
#   files  — the kept profiles, comma-joined, sorted by operator
#   flags  — <family>-<op> per kept profile in the same order, then
#            <family>-common; empty when nothing was found
#
# Exit codes: 1 for a family other than e2e, chaos or tempest or a missing
# GITHUB_OUTPUT; 0 in every other case, so a coverage problem never fails the
# leg.

set -euo pipefail

FAMILY="${1:-}"
case "${FAMILY}" in
  e2e | chaos | tempest) ;;
  *)
    echo "::error::family must be e2e, chaos or tempest, got '${FAMILY}'"
    exit 1
    ;;
esac
if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
  echo "::error::GITHUB_OUTPUT is required"
  exit 1
fi
KIND_CLUSTER="${KIND_CLUSTER:-cobaltcore}"
COVERAGE_NODE_DIR=/var/lib/cobaltcore-coverage
COVERAGE_OUT=_output/coverage
SCALE_DOWN="${SCALE_DOWN:-true}"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# A self-hosted runner reuses its workspace, so a profile an earlier run left
# behind must never be uploaded as this one's.
rm -rf "${COVERAGE_OUT}/raw"
rm -f "${COVERAGE_OUT}/${FAMILY}"-*.out
mkdir -p "${COVERAGE_OUT}/raw"

# ---------------------------------------------------------------------------
# 1. Final flush
# ---------------------------------------------------------------------------
# The managers flush every minute; scaling them down sends SIGTERM, on which
# each flushes the counters of its last minute. All Deployments are scaled
# before the first wait, so their pods terminate side by side.
if [[ "${SCALE_DOWN}" == "true" ]]; then
  deployments=""
  if ! json=$(kubectl get deployments --all-namespaces -o json); then
    echo "::warning::cannot list the Deployments; the managers get no final flush"
  elif ! deployments=$(jq -r '
      .items[]
      | select(any(.spec.template.spec.containers[];
          .name == "manager" and any(.env[]?; .name == "GOCOVERDIR")))
      | [.metadata.namespace, .metadata.name,
         (.spec.selector.matchLabels // {} | to_entries
          | map("\(.key)=\(.value)") | join(","))]
      | @tsv' <<<"${json}"); then
    echo "::warning::cannot read the Deployment list; the managers get no final flush"
    deployments=""
  fi
  while IFS=$'\t' read -r ns name _; do
    [[ -n "${name}" ]] || continue
    if ! kubectl scale deployment "${name}" -n "${ns}" --replicas=0; then
      echo "::warning::cannot scale ${ns}/${name} to 0; its manager gets no final flush"
    fi
  done <<<"${deployments}"
  while IFS=$'\t' read -r ns name selector; do
    [[ -n "${name}" ]] || continue
    # kubectl wait fails with "no matching resources found" when the pods are
    # already gone, which is what it waits for.
    if ! out=$(kubectl wait pod -n "${ns}" -l "${selector}" --for=delete --timeout=60s 2>&1) &&
      [[ "${out}" != *"no matching resources found"* ]]; then
      echo "::warning::the pods of ${ns}/${name} are not gone after 60s; copying the counters anyway: ${out}"
    fi
  done <<<"${deployments}"
fi

# ---------------------------------------------------------------------------
# 2. Copy
# ---------------------------------------------------------------------------
nodes=$(kind get nodes --name "${KIND_CLUSTER}" 2>/dev/null || true)
for node in ${nodes}; do
  mkdir -p "${COVERAGE_OUT}/raw/${node}"
  if ! docker cp "${node}:${COVERAGE_NODE_DIR}/." "${COVERAGE_OUT}/raw/${node}/" 2>/dev/null; then
    echo "::notice::${node} has no ${COVERAGE_NODE_DIR}; skipped"
    rm -rf "${COVERAGE_OUT:?}/raw/${node}"
  fi
done

# ---------------------------------------------------------------------------
# 3. Merge
# ---------------------------------------------------------------------------
operators=()
while IFS= read -r op; do
  [[ -n "${op}" ]] || continue
  if [[ ! "${op}" =~ ^[a-z][a-z0-9-]*$ ]]; then
    echo "::warning::skipping the coverage directory '${op}': not an operator name"
    continue
  fi
  operators+=("${op}")
done < <(find "${COVERAGE_OUT}/raw" -mindepth 2 -maxdepth 2 -type d | sed 's|.*/||' | LC_ALL=C sort -u)

kept=()
files=()
flags=()
for op in ${operators[@]+"${operators[@]}"}; do
  dirs=()
  counters=false
  for dir in "${COVERAGE_OUT}"/raw/*/"${op}"; do
    [[ -d "${dir}" ]] || continue
    dirs+=("${dir}")
    if compgen -G "${dir}/covcounters.*" >/dev/null; then
      counters=true
    fi
  done
  if [[ "${counters}" != "true" ]]; then
    echo "::notice::${op}: no coverage counters"
    continue
  fi
  profile="${COVERAGE_OUT}/${FAMILY}-${op}.out"
  if ! go tool covdata textfmt -i="$(IFS=,; echo "${dirs[*]}")" -o "${profile}"; then
    echo "::warning::${op}: go tool covdata failed"
    rm -f "${profile}"
    continue
  fi
  # covdata exits 0 on input it finds nothing applicable in and writes the
  # mode: line alone.
  if ! grep -qv '^mode:' "${profile}" 2>/dev/null; then
    echo "::notice::${op}: the merged profile holds no block"
    rm -f "${profile}"
    continue
  fi
  kept+=("${op}")
  files+=("${profile}")
  flags+=("${FAMILY}-${op}")
done

# ---------------------------------------------------------------------------
# 4. Summary and outputs
# ---------------------------------------------------------------------------
{
  echo "### Operator coverage (${FAMILY})"
  echo ""
  if [[ ${#kept[@]} -eq 0 ]]; then
    echo "No operator coverage counters were collected."
  else
    echo "| Operator | Statements | Covered | Coverage |"
    echo "| --- | --- | --- | --- |"
    for op in "${kept[@]}"; do
      # Field 2 is the block's statement count and field 3 its hit count.
      # LC_ALL=C keeps the decimal point a point under a German locale.
      LC_ALL=C awk -v op="${op}" -v prefix="github.com/c5c3/cobaltcore/operators/${op}/" '
        index($1, prefix) == 1 { s += $2; if ($3 > 0) c += $2 }
        END { printf "| %s | %d | %d | %.1f%% |\n", op, s, c, s ? c * 100 / s : 0 }
      ' "${COVERAGE_OUT}/${FAMILY}-${op}.out"
    done
  fi
} >>"${SUMMARY}"

found=false
if [[ ${#kept[@]} -gt 0 ]]; then
  found=true
  flags+=("${FAMILY}-common")
fi
{
  echo "found=${found}"
  echo "files=$(IFS=,; echo "${files[*]}")"
  echo "flags=$(IFS=,; echo "${flags[*]}")"
} >>"${GITHUB_OUTPUT}"
