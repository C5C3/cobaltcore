#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# Inventory the onboarding touch points for an OpenStack service across the
# five layers (image, operator, CI/e2e/deploy, ControlPlane, docs).
#
# Usage: inventory-touchpoints.sh <service>
#
# Prints [DONE]/[TODO] per touch point plus gotcha warnings. This is an
# inventory, not a gate: a fresh service is all-[TODO] by design, and a check
# labelled "skip if ..." is legitimately [TODO] for a service the profile rules
# it out for. Always exits 0 unless invoked incorrectly.
#
# Name greps are word-bounded (grep -w) or anchored, so a service name that is a
# substring of another word ("nova" in "Renovate") does not report [DONE].

set -euo pipefail

if [[ $# -ne 1 ]] || [[ ! "$1" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "usage: $0 <service>   (lowercase, e.g. horizon)" >&2
  exit 2
fi

SERVICE="$1"
# The Go identifier form of the service name ("glance" -> "Glance"), used by the
# layer-4 registration checks below.
SVC_KIND="$(tr '[:lower:]' '[:upper:]' <<< "${SERVICE:0:1}")${SERVICE:1}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

CI_YAML=.github/workflows/ci.yaml
BUILD_YAML=.github/workflows/build-images.yaml
C5C3_CTRL=operators/c5c3/internal/controller
C5C3_WEBHOOK=operators/c5c3/api/v1alpha1/controlplane_webhook.go

DONE_COUNT=0
TODO_COUNT=0

check() { # <description> <command...>
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  [DONE] %s\n' "${desc}"
    DONE_COUNT=$((DONE_COUNT + 1))
  else
    printf '  [TODO] %s\n' "${desc}"
    TODO_COUNT=$((TODO_COUNT + 1))
  fi
}

# list_has <file> <regex selecting the list line> — true when that line names
# SERVICE as a whole word. Used for the hand-maintained space-separated lists.
# shellcheck disable=SC2329 # invoked indirectly, as an argument of check
list_has() {
  grep -E "$2" "$1" | grep -qw -- "${SERVICE}"
}

header() { printf '\n== %s ==\n' "$1"; }

header "Layer 1: container image"
check "images/${SERVICE}/Dockerfile" test -f "images/${SERVICE}/Dockerfile"
for refs in releases/*/source-refs.yaml; do
  check "${refs}: '${SERVICE}:' key" grep -Eq "^${SERVICE}:" "${refs}"
done
for pkgs in releases/*/extra-packages.yaml; do
  check "${pkgs}: '${SERVICE}:' key" grep -Eq "^${SERVICE}:" "${pkgs}"
done
check "tests/container-images/verify_${SERVICE}.sh" test -f "tests/container-images/verify_${SERVICE}.sh"
check "build-images.yaml hadolint matrix entry (static list)" \
  grep -q "images/${SERVICE}/Dockerfile" "${BUILD_YAML}"
check "build-images.yaml ALL_SERVICES, svc_${SERVICE} filter and FILTER_svc_${SERVICE} (static lists)" \
  bash -c "grep -E '^ +ALL_SERVICES:' '${BUILD_YAML}' | grep -qw '${SERVICE}' && grep -Eq '^ +svc_${SERVICE}:' '${BUILD_YAML}' && grep -q 'FILTER_svc_${SERVICE}:' '${BUILD_YAML}'"
check "tests/container-images/verify_release_config.sh SERVICES= list" \
  list_has tests/container-images/verify_release_config.sh '^SERVICES='
check "tests/container-images/verify_deviation_comments.sh SERVICES= list (a non-python-base image gets its own function)" \
  list_has tests/container-images/verify_deviation_comments.sh '^SERVICES='
check "hack/gen-option-catalog.sh '${SERVICE})' arm (skip if the config is not oslo)" \
  grep -Eq "^[[:space:]]+${SERVICE}\)" hack/gen-option-catalog.sh
for rel_dir in releases/*/; do
  rel="${rel_dir%/}"
  rel="${rel##*/}"
  check "option catalog operators/${SERVICE}/api/v1alpha1/catalogs/${rel}.json (skip if the config is not oslo)" \
    test -f "operators/${SERVICE}/api/v1alpha1/catalogs/${rel}.json"
done

header "Layer 2: service operator"
check "operators/${SERVICE}/go.mod" test -f "operators/${SERVICE}/go.mod"
check "go.work 'use ./operators/${SERVICE}'" grep -Eq "operators/${SERVICE}([[:space:]]|$)" go.work
check "Makefile OPERATORS includes '${SERVICE}'" list_has Makefile '^OPERATORS \?='
check "operators/Dockerfile COPYs operators/${SERVICE}/go.mod (module-manifest lines)" \
  grep -q "COPY operators/${SERVICE}/go.mod" operators/Dockerfile
check "operators/${SERVICE}/helm/${SERVICE}-operator chart" \
  test -f "operators/${SERVICE}/helm/${SERVICE}-operator/Chart.yaml"
check "CRD base under operators/${SERVICE}/config/crd/bases/" \
  bash -c "ls operators/${SERVICE}/config/crd/bases/*.yaml"
check "Grafana dashboard operators/${SERVICE}/dashboards/${SERVICE}-operator.json" \
  test -f "operators/${SERVICE}/dashboards/${SERVICE}-operator.json"
check "reconcile_deployment_pin_test.go pins the rendered Deployment and Service (skip if no API Deployment)" \
  test -f "operators/${SERVICE}/internal/controller/reconcile_deployment_pin_test.go"
check "recurring-maintenance CronJob sub-reconciler, job.EnsureCronJob (skip only if the profile recorded no maintenance task)" \
  grep -rq "job.EnsureCronJob" "operators/${SERVICE}/internal/controller/"
check "placement artefact: tests/e2e/${SERVICE}/invalid-cr/*-targetclusterref-empty-name.yaml" \
  bash -c "ls tests/e2e/${SERVICE}/invalid-cr/*-targetclusterref-empty-name.yaml"

header "Layer 3: CI / e2e / deploy"
check "ci.yaml '${SERVICE}:' paths filter + FILTER_${SERVICE} env" \
  bash -c "grep -Eq '^ +${SERVICE}:$' '${CI_YAML}' && grep -q 'FILTER_${SERVICE}:' '${CI_YAML}'"
check "ci.yaml ALL_OPERATORS includes '${SERVICE}'" list_has "${CI_YAML}" '^ +ALL_OPERATORS:'
check "ci.yaml SERVICE_OPERATORS includes '${SERVICE}' (same edit as ALL_OPERATORS when it ships an image)" \
  list_has "${CI_YAML}" '^ +SERVICE_OPERATORS:'
check "ci.yaml image_${SERVICE} / tests_e2e_${SERVICE} filters + FILTER_ lines" \
  bash -c "grep -q 'FILTER_image_${SERVICE}:' '${CI_YAML}' && grep -q 'FILTER_tests_e2e_${SERVICE}:' '${CI_YAML}'"
check ".codecov.yml unit-${SERVICE} component" grep -Eq "name: unit-${SERVICE}$" .codecov.yml
check "pipeline contract test tests/unit/ci/${SERVICE}_e2e_matrix_test.sh (or the older tests/ci/verify_${SERVICE}_ci_pipeline.sh; keystone predates both)" \
  bash -c "test -f tests/unit/ci/${SERVICE}_e2e_matrix_test.sh || test -f tests/ci/verify_${SERVICE}_ci_pipeline.sh"
check "tests/e2e/${SERVICE}/ chainsaw suites" bash -c "ls tests/e2e/${SERVICE}/*/chainsaw-test.yaml"
check "Makefile verify-invalid-cr-fixtures runs tests/e2e/${SERVICE}/invalid-cr/_generate.py --check" \
  grep -q "tests/e2e/${SERVICE}/invalid-cr/_generate.py --check" Makefile
check "tests/e2e/${SERVICE}/gateway-quick-start-smoke suite" \
  test -f "tests/e2e/${SERVICE}/gateway-quick-start-smoke/chainsaw-test.yaml"
check "tests/e2e/${SERVICE}-operator/ chart-level suites (optional)" \
  bash -c "ls tests/e2e/${SERVICE}-operator/*/chainsaw-test.yaml"
check "tests/e2e-chaos scenarios exercising a ${SERVICE} CR" \
  bash -c "ls tests/e2e-chaos/*/[0-9][0-9]-${SERVICE}-cr.yaml"
for rel_dir in releases/*/; do
  rel="${rel_dir%/}"
  rel="${rel##*/}"
  check "tests/tempest/${SERVICE}-${rel//./-}/ config (skip if no tempest plugin)" \
    test -d "tests/tempest/${SERVICE}-${rel//./-}"
done
check "hack/ci-generate-tempest-matrix.sh ALL_TEMPEST_SERVICES + hack/ci-resolve-changes.sh TEMPEST_ALL_SERVICES (skip if no tempest plugin)" \
  bash -c "grep -E '^ALL_TEMPEST_SERVICES=' hack/ci-generate-tempest-matrix.sh | grep -qw '${SERVICE}' && grep -E '^TEMPEST_ALL_SERVICES=' hack/ci-resolve-changes.sh | grep -qw '${SERVICE}'"
check "deploy/flux-system/releases/${SERVICE}-operator.yaml" \
  test -f "deploy/flux-system/releases/${SERVICE}-operator.yaml"
check "deploy/kind/base/kustomization.yaml suspend patch for ${SERVICE}-operator" \
  grep -q "name: ${SERVICE}-operator" deploy/kind/base/kustomization.yaml
check "hack/deploy-infra.sh un-suspends ${SERVICE}-operator (flux ControlPlane path)" \
  bash -c "grep -A1 'kubectl patch helmrelease ${SERVICE}-operator -n ${SERVICE}-system' hack/deploy-infra.sh | grep -q '\"suspend\":false'"
check "hack/refresh-operator-image-digests.sh target tuple for ${SERVICE}-operator" \
  grep -q "${SERVICE}-operator|${SERVICE}-system|" hack/refresh-operator-image-digests.sh
check "hack/deploy-infra.sh enable_operator_servicemonitor call for ${SERVICE}-operator" \
  grep -q "enable_operator_servicemonitor ${SERVICE}-operator ${SERVICE}-system" hack/deploy-infra.sh
check "kind Gateway listener hostname ${SERVICE}.127-0-0-1.nip.io" \
  grep -q "hostname: ${SERVICE}\.127-0-0-1\.nip\.io" deploy/kind/base/openstack-gateway.yaml
check "deploy/kind/infrastructure/${SERVICE}-nip-io-tls-certificate.yaml" \
  test -f "deploy/kind/infrastructure/${SERVICE}-nip-io-tls-certificate.yaml"
check "OpenBao DB-engine tenant leg: a SERVICE_TENANTS row for spec.services.${SERVICE} (skip if no database)" \
  grep -Eq "^  \"[a-z0-9-]+ ${SERVICE} [a-z0-9_,]+\"|provision_service_tenant ${SERVICE} " deploy/openbao/bootstrap/setup-database-tenant.sh
check "OpenBao auth role ${SERVICE}[-<block>]-db in setup-auth.sh (skip if no database)" \
  grep -Eq "role/${SERVICE}(-[a-z0-9]+)?-db\"" deploy/openbao/bootstrap/setup-auth.sh
check "deploy/openbao/policies/${SERVICE}[-<block>]-db-dynamic.hcl (skip if no database)" \
  bash -c "ls deploy/openbao/policies/ | grep -Eq '^${SERVICE}(-[a-z0-9]+)?-db-dynamic\.hcl$'"

header "Layer 4: ControlPlane (c5c3) integration"
check "ServicesSpec has Service…Spec for '${SERVICE}'" \
  grep -qi "Service${SERVICE}Spec" operators/c5c3/api/v1alpha1/controlplane_types.go
check "${C5C3_CTRL}/reconcile_${SERVICE}.go" \
  test -f "${C5C3_CTRL}/reconcile_${SERVICE}.go"
check "c5c3 helm _rbac-rules.tpl (generated from the c5c3 RBAC markers) names the ${SERVICE} API group" \
  grep -q "${SERVICE}\.openstack\.c5c3\.io" operators/c5c3/helm/c5c3-operator/templates/_rbac-rules.tpl
check "controlplane_controller.go mirrors a ${SVC_KIND}Ready condition" \
  grep -q "${SVC_KIND}Ready" "${C5C3_CTRL}/controlplane_controller.go"
check "desired${SVC_KIND}Registration builder in builtin_registrations.go (skip if the service has neither a catalog entry nor a service user)" \
  grep -q "func desired${SVC_KIND}Registration" "${C5C3_CTRL}/builtin_registrations.go"
check "reconcile_${SERVICE}.go projects the registration (reconcileBuiltinRegistration) and folds it into ${SVC_KIND}Ready (foldBuiltinRegistrationReady)" \
  bash -c "grep -q 'reconcileBuiltinRegistration(ctx, cp, desired${SVC_KIND}Registration(cp)' ${C5C3_CTRL}/reconcile_${SERVICE}.go && grep -q 'foldBuiltinRegistrationReady(cp, child, conditionType${SVC_KIND}Ready)' ${C5C3_CTRL}/reconcile_${SERVICE}.go"
check "projectedBuiltinRegistrations entry in reconcile_serviceaccounts.go (ServiceAccountsReady aggregation)" \
  grep -q "desired${SVC_KIND}Registration(cp)" "${C5C3_CTRL}/reconcile_serviceaccounts.go"
check "${SERVICE}CatalogURL public-URL helper in reconcile_catalog.go" \
  grep -q "func ${SERVICE}CatalogURL" "${C5C3_CTRL}/reconcile_catalog.go"
check "reconcile_${SERVICE}_dbcredentials.go (skip if no database; keystone's glue lives in reconcile_dbcredentials.go)" \
  test -f "${C5C3_CTRL}/reconcile_${SERVICE}_dbcredentials.go"
check "${SERVICE}MessagingTarget for reconcileServiceMessaging (skip if no message bus)" \
  grep -q "func ${SERVICE}MessagingTarget" "${C5C3_CTRL}/reconcile_${SERVICE}.go"
check "validate${SVC_KIND}ChildName bounds the projected child name in controlplane_webhook.go (skip if the service CRD bounds no metadata.name)" \
  grep -q "func validate${SVC_KIND}ChildName" "${C5C3_WEBHOOK}"
check "envtest full chain covers '${SERVICE}' (integration_test.go)" \
  grep -qiw "${SERVICE}" "${C5C3_CTRL}/integration_test.go"
check "full-ControlPlane e2e suite drives '${SERVICE}'" \
  grep -qiw "${SERVICE}" tests/e2e/c5c3/full-controlplane-keystone/chainsaw-test.yaml

header "Layer 5: documentation"
check "docs/reference/${SERVICE}/ pages" bash -c "ls docs/reference/${SERVICE}/*.md"
check "VitePress sidebar references reference/${SERVICE}/" \
  grep -q "reference/${SERVICE}/" docs/.vitepress/config.ts
check "tests/unit/docs/${SERVICE}_crd_naming_convention_test.sh" \
  test -f "tests/unit/docs/${SERVICE}_crd_naming_convention_test.sh"
check "docs/reference/testing/${SERVICE}-e2e-tests.md (optional)" \
  test -f "docs/reference/testing/${SERVICE}-e2e-tests.md"
check "per-service guides under docs/guides/${SERVICE}/" \
  bash -c "ls docs/guides/${SERVICE}/*.md"
check "quick start integrates '${SERVICE}' (docs/quick-start-controlplane.md)" \
  grep -qiw "${SERVICE}" docs/quick-start-controlplane.md

header "Gotcha warnings"
for uc in releases/*/upper-constraints.txt; do
  if grep -Eq "^${SERVICE}===" "${uc}"; then
    pin="$(grep -E "^${SERVICE}===" "${uc}")"
    printf '  [WARN] %s pins %s — source install must match the pin or strip it via overrides/<release>/constraints.txt\n' \
      "${uc}" "${pin}"
  fi
done
if [[ -d "operators/${SERVICE}" ]] && ! grep -qw -- "${SERVICE}" <<< "$(grep -E '^ +ALL_OPERATORS:' "${CI_YAML}")"; then
  printf '  [WARN] operators/%s exists but ci.yaml ALL_OPERATORS does not name it — its tests and e2e leg never run in CI\n' \
    "${SERVICE}"
fi

printf '\nSummary: %d done, %d todo (inventory only — todo is expected for a fresh service)\n' \
  "${DONE_COUNT}" "${TODO_COUNT}"
exit 0
