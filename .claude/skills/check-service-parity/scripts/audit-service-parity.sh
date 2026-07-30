#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-service-parity.sh — mechanical five-layer parity checks for every
# onboarded OpenStack service against the keystone reference implementation.
# Verifies, per service (services = keys of releases/<latest>/source-refs.yaml;
# c5c3 is the ControlPlane operator, not a service):
#   P1  image layer: images/<svc>/Dockerfile, tests/container-images/
#       verify_<svc>.sh, and per-release config (source-refs.yaml key,
#       extra-packages.yaml block, test-excludes/<svc>.txt) in EVERY release
#   P2  operator module: operators/<svc>/go.mod, go.work use entry, Makefile
#       OPERATORS default, operators/Dockerfile go.mod COPY line
#   P3  helm chart: crds/, values.schema.json, helm-unittest suite set at
#       parity with the keystone reference chart
#   P4  observability: dashboards/<svc>-operator.json + dashboard_test.go
#   P5  CI wiring: paths-filter, ALL_OPERATORS, FILTER_<svc>, unit-test
#       matrices, helm-validate chart loop, build/cleanup image matrices
#   P6  e2e coverage: canonical chainsaw suite set under tests/e2e/<svc>/
#       (incl. latest-release variant) plus at least one chaos suite
#   P7  deploy stack: flux HelmRelease, namespace entry, kustomization entry,
#       and the deploy-infra literal lists (kind-overlay un-suspend on the
#       flux ControlPlane path, digest-refresh target, ServiceMonitor toggle)
#   P8  docs: reference set, metrics/networkpolicy guides, vitepress nav
#   P9  ControlPlane integration: ServicesSpec field, reconcile_<svc>.go,
#       <Svc>Ready condition, c5c3 chart RBAC rule
#   P10 recurring maintenance: a database-backed service projects at least
#       one housekeeping CronJob (job.EnsureCronJob)
#   P11 API endpoint isolation: component-labelled API Service selector
#       (naming.APISelectorLabels), per-component pod-template labels
#       (naming.ComponentLabels), Job-pod exclusion on the API PDB
#       (naming.ExcludeJobPods)
#
# The keystone reference is audited too — a reference regression is its own
# [FAIL]. Deliberate thin-profile deviations (a service that legitimately
# lacks a keystone-shaped artefact) are recorded in ALLOWED_DEVIATIONS below,
# one "<svc>:<check>:<item>" token per line. Exit code 1 on any [FAIL].

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# Per-service allowlist of accepted deviations from the keystone reference.
# One "<svc>:<check>:<item>" token per line; <item> is the path or basename
# the check would otherwise flag. Example for a thin-profile service that
# ships no NetworkPolicy template of its own:
#   horizon:P3:networkpolicy_test.yaml
ALLOWED_DEVIATIONS="
"

allowed() { # allowed <svc> <check> <item>
  printf '%s\n' "${ALLOWED_DEVIATIONS}" | grep -qx "${1}:${2}:${3}"
}

# check <svc> <check-id> <item> <condition-exit-code> <fail-message>
# Downgrades an allowlisted miss to [INFO].
check() {
  local svc="$1" id="$2" item="$3" ok="$4" msg="$5"
  if [[ "${ok}" -eq 0 ]]; then
    pass "${svc}: ${msg}"
  elif allowed "${svc}" "${id}" "${item}"; then
    info "${svc}: ${msg} — allowed deviation (${id}:${item})"
  else
    fail "${svc}: ${msg}"
  fi
}

# ---------------------------------------------------------------------------
# Discovery — services are the keys of the latest release's source-refs.yaml
# ---------------------------------------------------------------------------
LATEST_RELEASE="$(ls releases | sort | tail -1)"
if [[ -z "${LATEST_RELEASE}" ]]; then
  fail "no release directories under releases/"
  exit 1
fi
LATEST_SLUG="${LATEST_RELEASE//./-}"

SERVICES="$(grep -E '^[a-z0-9_-]+:' "releases/${LATEST_RELEASE}/source-refs.yaml" \
  | cut -d: -f1 | tr '\n' ' ')"
if [[ -z "${SERVICES// /}" ]]; then
  fail "no service keys found in releases/${LATEST_RELEASE}/source-refs.yaml"
  exit 1
fi

RELEASES="$(ls releases | tr '\n' ' ')"

hdr "Discovered services and releases"
info "latest release: ${LATEST_RELEASE} (e2e variant slug: ${LATEST_SLUG})"
info "releases: ${RELEASES}"
info "services: ${SERVICES}"
info "reference service: keystone"

# Canonical per-service chainsaw suite set, derived from the suites every
# service is expected to carry (the keystone reference has all of them).
CANONICAL_E2E="basic-deployment scale healthcheck httproute network-policy \
deletion-cleanup pod-security-restricted invalid-cr gateway-quick-start-smoke"

# Reference helm-unittest suite set, derived live from the keystone chart.
REFERENCE_HELM_TESTS="$(find operators/keystone/helm/keystone-operator/tests \
  -maxdepth 1 -name '*_test.yaml' -exec basename {} \; 2>/dev/null | sort | tr '\n' ' ' || true)"

# ---------------------------------------------------------------------------
# P1 — image layer
# ---------------------------------------------------------------------------
hdr "P1: image layer (Dockerfile, verify script, per-release config)"
for svc in ${SERVICES}; do
  t=0; [[ -f "images/${svc}/Dockerfile" ]] || t=1
  check "${svc}" P1 "images/${svc}/Dockerfile" "${t}" "images/${svc}/Dockerfile present"

  t=0; [[ -f "tests/container-images/verify_${svc}.sh" ]] || t=1
  check "${svc}" P1 "verify_${svc}.sh" "${t}" \
    "tests/container-images/verify_${svc}.sh locks the image contract"

  for rel in ${RELEASES}; do
    t=0; grep -qE "^${svc}:" "releases/${rel}/source-refs.yaml" 2>/dev/null || t=1
    check "${svc}" P1 "releases/${rel}/source-refs" "${t}" \
      "releases/${rel}/source-refs.yaml carries the ${svc} key"

    t=0; grep -qE "^${svc}:" "releases/${rel}/extra-packages.yaml" 2>/dev/null || t=1
    check "${svc}" P1 "releases/${rel}/extra-packages" "${t}" \
      "releases/${rel}/extra-packages.yaml carries the ${svc} block"

    t=0; [[ -f "releases/${rel}/test-excludes/${svc}.txt" ]] || t=1
    check "${svc}" P1 "releases/${rel}/test-excludes" "${t}" \
      "releases/${rel}/test-excludes/${svc}.txt present"
  done
done

# ---------------------------------------------------------------------------
# P2 — operator module wiring
# ---------------------------------------------------------------------------
hdr "P2: operator module (go.work, Makefile OPERATORS, operators/Dockerfile)"
for svc in ${SERVICES}; do
  t=0; [[ -f "operators/${svc}/go.mod" ]] || t=1
  check "${svc}" P2 "operators/${svc}/go.mod" "${t}" "operators/${svc}/ is a Go module"

  t=0; grep -qE "^[[:space:]]*\./operators/${svc}$" go.work || t=1
  check "${svc}" P2 "go.work" "${t}" "go.work has a use entry for operators/${svc}"

  t=0; grep -E '^OPERATORS \?=' Makefile | grep -qw "${svc}" || t=1
  check "${svc}" P2 "Makefile-OPERATORS" "${t}" \
    "Makefile OPERATORS default includes ${svc}"

  t=0; grep -qF "operators/${svc}/go.mod" operators/Dockerfile || t=1
  check "${svc}" P2 "operators/Dockerfile" "${t}" \
    "operators/Dockerfile copies the ${svc} module manifests"
done

# ---------------------------------------------------------------------------
# P3 — helm chart parity
# ---------------------------------------------------------------------------
hdr "P3: helm chart (crds/, values.schema.json, unittest suite parity)"
for svc in ${SERVICES}; do
  chart="operators/${svc}/helm/${svc}-operator"
  t=0; [[ -f "${chart}/Chart.yaml" ]] || t=1
  check "${svc}" P3 "Chart.yaml" "${t}" "${chart}/Chart.yaml present"
  [[ -f "${chart}/Chart.yaml" ]] || continue

  crd_count=$( (find "${chart}/crds" -maxdepth 1 -name '*.yaml' -type f 2>/dev/null || true) | wc -l | tr -d ' ')
  t=0; [[ "${crd_count}" -gt 0 ]] || t=1
  check "${svc}" P3 "crds" "${t}" "${chart}/crds/ carries ${crd_count} CRD copy(ies)"

  t=0; [[ -f "${chart}/values.schema.json" ]] || t=1
  check "${svc}" P3 "values.schema.json" "${t}" \
    "${chart}/values.schema.json present (make gen-helm-schema)"

  for ref_test in ${REFERENCE_HELM_TESTS}; do
    t=0; [[ -f "${chart}/tests/${ref_test}" ]] || t=1
    check "${svc}" P3 "${ref_test}" "${t}" \
      "helm-unittest suite ${ref_test} at parity with the keystone chart"
  done
done

# ---------------------------------------------------------------------------
# P4 — observability
# ---------------------------------------------------------------------------
hdr "P4: observability (Grafana dashboard + drift test)"
for svc in ${SERVICES}; do
  t=0; [[ -f "operators/${svc}/dashboards/${svc}-operator.json" ]] || t=1
  check "${svc}" P4 "dashboard" "${t}" \
    "operators/${svc}/dashboards/${svc}-operator.json present"

  t=0; [[ -f "operators/${svc}/dashboards/dashboard_test.go" ]] || t=1
  check "${svc}" P4 "dashboard_test.go" "${t}" \
    "dashboard drift test pins panels to registered metrics"
done

# ---------------------------------------------------------------------------
# P5 — CI wiring
# ---------------------------------------------------------------------------
hdr "P5: CI wiring (ci.yaml, build-images.yaml, cleanup-images.yaml)"
CI=".github/workflows/ci.yaml"
for svc in ${SERVICES}; do
  t=0; grep -qF "'operators/${svc}/**'" "${CI}" || t=1
  check "${svc}" P5 "paths-filter" "${t}" \
    "ci.yaml paths-filter watches operators/${svc}/**"

  t=0; grep -E 'ALL_OPERATORS:' "${CI}" | grep -qw "${svc}" || t=1
  check "${svc}" P5 "ALL_OPERATORS" "${t}" "ci.yaml ALL_OPERATORS lists ${svc}"

  t=0; grep -q "FILTER_${svc}:" "${CI}" || t=1
  check "${svc}" P5 "FILTER" "${t}" "ci.yaml exports FILTER_${svc} to the resolver"

  missing_matrix=$(grep -E 'target: \[' "${CI}" | grep -cvw "${svc}" || true)
  t=0; [[ "${missing_matrix}" -eq 0 ]] || t=1
  check "${svc}" P5 "test-matrix" "${t}" \
    "every ci.yaml unit/integration test matrix lists ${svc}"

  t=0; grep -qF "operators/${svc}/helm/${svc}-operator" "${CI}" || t=1
  check "${svc}" P5 "helm-validate" "${t}" \
    "ci.yaml helm-validate loop covers the ${svc} chart"

  t=0; grep -qF "images/${svc}/Dockerfile" .github/workflows/build-images.yaml || t=1
  check "${svc}" P5 "build-images" "${t}" \
    "build-images.yaml lints/builds images/${svc}/Dockerfile"

  t=0; grep -qw "${svc}-operator" .github/workflows/cleanup-images.yaml || t=1
  check "${svc}" P5 "cleanup-operator" "${t}" \
    "cleanup-images.yaml covers the ${svc}-operator package"

  t=0; grep -E 'package: \[' .github/workflows/cleanup-images.yaml | grep -qw "${svc}" || t=1
  check "${svc}" P5 "cleanup-service" "${t}" \
    "cleanup-images.yaml covers the ${svc} service-image package"
done

# ---------------------------------------------------------------------------
# P6 — e2e coverage
# ---------------------------------------------------------------------------
hdr "P6: e2e coverage (canonical suites, release variant, chaos)"
for svc in ${SERVICES}; do
  for suite in ${CANONICAL_E2E}; do
    t=0; [[ -d "tests/e2e/${svc}/${suite}" ]] || t=1
    check "${svc}" P6 "${suite}" "${t}" "tests/e2e/${svc}/${suite} suite present"
  done

  variant="basic-deployment-${LATEST_SLUG}"
  t=0; [[ -d "tests/e2e/${svc}/${variant}" ]] || t=1
  check "${svc}" P6 "${variant}" "${t}" \
    "latest-release variant tests/e2e/${svc}/${variant} present"

  # The keystone chaos suites predate multi-service naming and are unprefixed;
  # every later service prefixes its chaos suites with its own name.
  if [[ "${svc}" == "keystone" ]]; then
    t=0; [[ -d "tests/e2e-chaos/operator-pod-kill" ]] || t=1
    check "${svc}" P6 "chaos" "${t}" \
      "chaos suite tests/e2e-chaos/operator-pod-kill present (unprefixed reference set)"
  else
    chaos_count=$( (find tests/e2e-chaos -maxdepth 1 -type d -name "${svc}-*" 2>/dev/null || true) | wc -l | tr -d ' ')
    t=0; [[ "${chaos_count}" -gt 0 ]] || t=1
    check "${svc}" P6 "chaos" "${t}" \
      "at least one chaos suite tests/e2e-chaos/${svc}-* present (found ${chaos_count})"
  fi
done

# ---------------------------------------------------------------------------
# P7 — deploy stack
# ---------------------------------------------------------------------------
hdr "P7: deploy stack (flux HelmRelease, namespace, kustomization, deploy-infra wiring)"
for svc in ${SERVICES}; do
  t=0; [[ -f "deploy/flux-system/releases/${svc}-operator.yaml" ]] || t=1
  check "${svc}" P7 "flux-release" "${t}" \
    "deploy/flux-system/releases/${svc}-operator.yaml present"

  t=0; grep -qE "name: ${svc}-system" deploy/flux-system/namespaces.yaml || t=1
  check "${svc}" P7 "namespace" "${t}" \
    "deploy/flux-system/namespaces.yaml declares ${svc}-system"

  t=0; grep -qF "releases/${svc}-operator.yaml" deploy/flux-system/kustomization.yaml || t=1
  check "${svc}" P7 "kustomization" "${t}" \
    "deploy/flux-system/kustomization.yaml lists releases/${svc}-operator.yaml"

  # hack/deploy-infra.sh carries three literal per-service lists that no gate
  # cross-checks — the failure mode that left glance-operator suspended on the
  # quick-start flux path (Glance CRDs never installed, the c5c3-operator's
  # controlplane cache never synced, the ControlPlane CR stayed status-less).
  # Each check is gated on the artefact that makes it required, so a service
  # that never enters the respective mechanism is not flagged.

  # The kind base overlay suspends the self-built operator HelmReleases (dev
  # image vs published chart); the WITH_CONTROLPLANE flux branch must
  # un-suspend every one of them.
  if grep -qE "name: ${svc}-operator" deploy/kind/base/kustomization.yaml; then
    t=0
    grep -A1 "kubectl patch helmrelease ${svc}-operator -n ${svc}-system" hack/deploy-infra.sh \
      | grep -q '"suspend":false' || t=1
    check "${svc}" P7 "deploy-infra-unsuspend" "${t}" \
      "hack/deploy-infra.sh un-suspends the ${svc}-operator HelmRelease on the flux ControlPlane path"
  fi

  # A HelmRelease consuming a <svc>-operator-image-digest ConfigMap needs the
  # matching OPERATOR_DIGEST_TARGETS tuple, or its :latest image is never
  # digest-pinned and a merged feature does not roll out.
  if grep -q "${svc}-operator-image-digest" "deploy/flux-system/releases/${svc}-operator.yaml" 2>/dev/null; then
    t=0; grep -qF "\"${svc}-operator|${svc}-system|" hack/refresh-operator-image-digests.sh || t=1
    check "${svc}" P7 "digest-target" "${t}" \
      "hack/refresh-operator-image-digests.sh OPERATOR_DIGEST_TARGETS covers ${svc}-operator"
  fi

  # A chart exposing metrics.serviceMonitor needs the WITH_PROMETHEUS toggle
  # call site, or the operator ships without a scrape target on that path.
  if grep -q 'serviceMonitor' "operators/${svc}/helm/${svc}-operator/values.yaml" 2>/dev/null; then
    t=0; grep -q "enable_operator_servicemonitor ${svc}-operator ${svc}-system" hack/deploy-infra.sh || t=1
    check "${svc}" P7 "servicemonitor-toggle" "${t}" \
      "hack/deploy-infra.sh calls enable_operator_servicemonitor for ${svc}-operator (WITH_PROMETHEUS)"
  fi
done

# ---------------------------------------------------------------------------
# P8 — documentation
# ---------------------------------------------------------------------------
hdr "P8: documentation (reference set, guides, vitepress nav)"
for svc in ${SERVICES}; do
  t=0; [[ -f "docs/reference/${svc}/index.md" ]] || t=1
  check "${svc}" P8 "reference-index" "${t}" "docs/reference/${svc}/index.md present"

  t=0; [[ -f "docs/reference/${svc}/${svc}-crd.md" ]] || t=1
  check "${svc}" P8 "reference-crd" "${t}" "docs/reference/${svc}/${svc}-crd.md present"

  t=0; [[ -f "docs/reference/${svc}/${svc}-reconciler.md" ]] || t=1
  check "${svc}" P8 "reference-reconciler" "${t}" \
    "docs/reference/${svc}/${svc}-reconciler.md present"

  t=0; [[ -f "docs/guides/${svc}/enable-${svc}-operator-metrics.md" ]] || t=1
  check "${svc}" P8 "guide-metrics" "${t}" \
    "docs/guides/${svc}/enable-${svc}-operator-metrics.md present"

  t=0; [[ -f "docs/guides/${svc}/enable-${svc}-operator-networkpolicy.md" ]] || t=1
  check "${svc}" P8 "guide-networkpolicy" "${t}" \
    "docs/guides/${svc}/enable-${svc}-operator-networkpolicy.md present"

  t=0; grep -qF "/reference/${svc}/" docs/.vitepress/config.ts || t=1
  check "${svc}" P8 "vitepress-nav" "${t}" \
    "docs/.vitepress/config.ts navigates to /reference/${svc}/"
done

# ---------------------------------------------------------------------------
# P9 — ControlPlane (c5c3) integration
# ---------------------------------------------------------------------------
hdr "P9: ControlPlane integration (ServicesSpec, reconciler, condition, RBAC)"
TYPES="operators/c5c3/api/v1alpha1/controlplane_types.go"
CTRL="operators/c5c3/internal/controller/controlplane_controller.go"
HELPERS="operators/c5c3/helm/c5c3-operator/templates/_helpers.tpl"
for svc in ${SERVICES}; do
  t=0; grep -qi "Service${svc}Spec" "${TYPES}" || t=1
  check "${svc}" P9 "ServicesSpec" "${t}" \
    "ControlPlane ServicesSpec models services.${svc}"

  t=0; [[ -f "operators/c5c3/internal/controller/reconcile_${svc}.go" ]] || t=1
  check "${svc}" P9 "reconciler" "${t}" \
    "operators/c5c3/internal/controller/reconcile_${svc}.go projects the child"

  t=0; grep -qi "${svc}Ready" "${CTRL}" || t=1
  check "${svc}" P9 "condition" "${t}" \
    "controlplane_controller.go mirrors a ${svc}Ready condition"

  t=0; grep -qi "${svc}\.openstack\.c5c3\.io" "${HELPERS}" || t=1
  check "${svc}" P9 "rbac" "${t}" \
    "c5c3 chart RBAC helper grants the ${svc}.openstack.c5c3.io group"
done

# ---------------------------------------------------------------------------
# P10 — recurring maintenance
# ---------------------------------------------------------------------------
# Every soft-deleting service needs periodic housekeeping (glance:
# glance-manage db purge; keystone: keystone-manage trust_flush). Package
# deployments run it from a cron entry on the controller node; here nothing
# runs it unless the operator projects a CronJob, and the omission is silent —
# the deployment stays Ready while the backlog grows. Presence of
# job.EnsureCronJob is a smoke signal only: whether the projected CronJob
# covers the tasks the service actually needs is the hand cross-reference in
# the skill's step 2.
hdr "P10: recurring maintenance (housekeeping CronJob per database-backed service)"
for svc in ${SERVICES}; do
  # Skip services that model no database: they carry no soft-delete backlog,
  # so their absence is a profile fact, not a deviation worth a token.
  if ! grep -rq "DatabaseSpec" "operators/${svc}/api/" 2>/dev/null; then
    info "${svc}: models no database — no housekeeping CronJob expected"
    continue
  fi

  t=0; grep -rq "job.EnsureCronJob" "operators/${svc}/internal/controller/" 2>/dev/null || t=1
  check "${svc}" P10 "maintenance-cronjob" "${t}" \
    "operator projects at least one recurring-maintenance CronJob (job.EnsureCronJob)"
done

# ---------------------------------------------------------------------------
# P11 — API endpoint isolation
# ---------------------------------------------------------------------------
# A maintenance pod (CronJob/Job) whose template carries bare commonLabels
# satisfies a name+instance Service selector and becomes an API Service
# endpoint: the numeric targetPort admits a pod without the container port,
# and a Job pod has no readiness probe, so it counts as ready from its first
# moment — a share of in-cluster API traffic is answered with ECONNREFUSED
# for the pod's whole runtime. The fixed shape selects the API Service by
# component (naming.APISelectorLabels), labels every pod template per
# component (naming.ComponentLabels), and keys the API PodDisruptionBudget on
# the absence of batch.kubernetes.io/job-name (naming.ExcludeJobPods) rather
# than the component label. These greps are smoke signals: whether EVERY
# Job/CronJob pod template actually sets a non-api component value is the
# hand cross-reference in the skill's step 2.
hdr "P11: API endpoint isolation (component-labelled Service selector, PDB Job-pod exclusion)"
for svc in ${SERVICES}; do
  t=0; grep -rq "naming.APISelectorLabels" "operators/${svc}/internal/controller/" 2>/dev/null || t=1
  check "${svc}" P11 "api-service-selector" "${t}" \
    "API Service selector narrows by component (naming.APISelectorLabels)"

  t=0; grep -rq "naming.ComponentLabels" "operators/${svc}/internal/controller/" 2>/dev/null || t=1
  check "${svc}" P11 "component-pod-labels" "${t}" \
    "pod templates are labelled per component (naming.ComponentLabels)"

  t=0; grep -rq "naming.ExcludeJobPods" "operators/${svc}/internal/controller/" 2>/dev/null || t=1
  check "${svc}" P11 "pdb-excludes-job-pods" "${t}" \
    "API PodDisruptionBudget excludes Job pods (naming.ExcludeJobPods)"
done

# ---------------------------------------------------------------------------
# Inventory — one line per service and layer
# ---------------------------------------------------------------------------
hdr "Inventory"
for svc in ${SERVICES}; do
  helm_tests=$( (find "operators/${svc}/helm/${svc}-operator/tests" -maxdepth 1 -name '*_test.yaml' 2>/dev/null || true) | wc -l | tr -d ' ')
  e2e_suites=$( (find "tests/e2e/${svc}" -maxdepth 1 -mindepth 1 -type d 2>/dev/null || true) | wc -l | tr -d ' ')
  if [[ "${svc}" == "keystone" ]]; then
    chaos_suites=$( (find tests/e2e-chaos -maxdepth 1 -mindepth 1 -type d 2>/dev/null || true) | wc -l | tr -d ' ')
  else
    chaos_suites=$( (find tests/e2e-chaos -maxdepth 1 -type d -name "${svc}-*" 2>/dev/null || true) | wc -l | tr -d ' ')
  fi
  info "${svc}: helm-tests=${helm_tests} e2e-suites=${e2e_suites} chaos-suites=${chaos_suites}"
done

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no service-parity findings"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} service-parity finding(s)"
  exit 1
fi
