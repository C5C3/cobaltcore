#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the deploy-stack artifacts that install and expose a standalone Nova:
#
#   - deploy/flux-system renders the nova-operator HelmRelease in its own
#     nova-system Namespace, bound to the five operators it needs and to the
#     optional image-digest ConfigMap. A sixth dependsOn entry naming a Flux
#     Kustomization (the message bus) would never become Ready, and the release
#     would wait forever.
#   - deploy/kind/base suspends that release (the kind stack installs the chart
#     itself) and carries the three Nova Gateway listeners, each on its own
#     hostname and its own certificate.
#   - deploy/kind/infrastructure ships the three matching Certificates and the
#     two kind-only database shims, one per database block of a Nova.
#   - every new file carries its SPDX header, and none of them is named by a
#     Renovate manager: they pin no third-party version.
#
# The rendering tests need kustomize and yq; each skips with a counted SKIP when
# either is missing. The header and Renovate tests run either way.
#
# Usage: bash tests/unit/deploy/nova_deploy_stack_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

FLUX_SYSTEM_DIR="$PROJECT_ROOT/deploy/flux-system"
KIND_BASE_DIR="$PROJECT_ROOT/deploy/kind/base"
KIND_INFRA_DIR="$PROJECT_ROOT/deploy/kind/infrastructure"
RENOVATE_CONFIG="$PROJECT_ROOT/renovate.json"

# Every file this work package adds, relative to the repository root.
NEW_FILES=(
  "deploy/flux-system/releases/nova-operator.yaml"
  "deploy/kind/infrastructure/nova-nip-io-tls-certificate.yaml"
  "deploy/kind/infrastructure/nova-metadata-nip-io-tls-certificate.yaml"
  "deploy/kind/infrastructure/nova-console-nip-io-tls-certificate.yaml"
  "deploy/kind/infrastructure/nova-api-db-externalsecret.yaml"
  "deploy/kind/infrastructure/nova-db-externalsecret.yaml"
  "deploy/openbao/policies/nova-api-db-dynamic.hcl"
  "deploy/openbao/policies/nova-cell-db-dynamic.hcl"
)

# The three Nova listeners, as "<listener> <hostname> <certificate secret>".
NOVA_LISTENERS=(
  "https-nova nova.127-0-0-1.nip.io nova-nip-io-tls"
  "https-nova-metadata nova-metadata.127-0-0-1.nip.io nova-metadata-nip-io-tls"
  "https-nova-console nova-console.127-0-0-1.nip.io nova-console-nip-io-tls"
)

# Read a single value out of a rendered stream. Prints the first line of the
# yq result, or the empty string when the expression matches nothing. A stream
# of many documents yields one empty line per document the select drops, and a
# collect expression turns those into empty strings, so blank lines are filtered
# out before the first line is taken.
render_value() {
  local stream="$1" expression="$2"
  printf '%s\n' "$stream" | yq -r "$expression" 2>/dev/null \
    | grep -v '^---$' | grep -v '^null$' | grep -v '^$' | head -n1
}

# Render a kustomization the way hack/deploy-infra.sh does: no
# --load-restrictor flag, because kubectl's embedded kustomize has none.
# Prints the rendered stream on success, the error output on failure.
render_dir() {
  kustomize build "$1" 2>&1
}

# tools_missing <skipped_checks>
# Reports whether kustomize or yq is off PATH and counts the skipped checks.
tools_missing() {
  local skipped="$1"
  if command -v kustomize >/dev/null 2>&1 && command -v yq >/dev/null 2>&1; then
    return 1
  fi
  echo "  SKIP: kustomize or yq not installed ($skipped checks skipped)"
  SKIP=$((SKIP + skipped))
  return 0
}

# --- Test 1: every new file carries its SPDX header ---
test_new_files_carry_spdx() {
  echo "Test: every file the nova deploy stack adds carries an SPDX header"

  local rel file first_line header
  for rel in "${NEW_FILES[@]}"; do
    file="$PROJECT_ROOT/$rel"
    if [[ ! -f "$file" ]]; then
      echo "  FAIL: $rel does not exist"
      FAIL=$((FAIL + 1))
      continue
    fi
    first_line="$(head -n1 "$file")"
    header="$(head -n3 "$file")"
    assert_contains "$rel opens with the copyright line" \
      "$first_line" "SPDX-FileCopyrightText: Copyright 2026 SAP SE"
    assert_contains "$rel declares Apache-2.0 in its header" \
      "$header" "SPDX-License-Identifier: Apache-2.0"
  done
}

# --- Test 2: no new file is named by a Renovate manager ---
# Every pinned third-party version under deploy/ is named by a customManager and
# a paired packageRule. These files pin nothing third-party: the chart is built
# from this repository and tracked by a semver range, the certificates and the
# ESO shims carry no version at all. A file that appears here means either a pin
# slipped in or a manager is matching a file it cannot bump.
test_renovate_names_none_of_the_new_files() {
  echo "Test: renovate.json names none of the new deploy-stack files"

  if [[ ! -f "$RENOVATE_CONFIG" ]]; then
    echo "  FAIL: $RENOVATE_CONFIG does not exist"
    FAIL=$((FAIL + 1))
    return
  fi

  local rel base hits
  for rel in "${NEW_FILES[@]}"; do
    base="$(basename "$rel")"
    hits="$( { grep -c "$base" "$RENOVATE_CONFIG" || true; } )"
    assert_eq "renovate.json does not name $base" "0" "${hits// /}"
  done
}

# --- Test 3: the production overlay renders the release ---
test_flux_system_renders_the_release() {
  echo "Test: kustomize build deploy/flux-system renders the nova-operator release"

  tools_missing 8 && return

  local rendered
  if ! rendered="$(render_dir "$FLUX_SYSTEM_DIR")"; then
    echo "  FAIL: kustomize build $FLUX_SYSTEM_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 8))
    return
  fi

  local hr='select(.kind == "HelmRelease" and .metadata.name == "nova-operator")'

  assert_eq "the release declares the v2 API of the Helm controller" \
    "helm.toolkit.fluxcd.io/v2" "$(render_value "$rendered" "$hr | .apiVersion")"
  assert_eq "the release lives in the operator's own namespace" \
    "nova-system" "$(render_value "$rendered" "$hr | .metadata.namespace")"

  # rabbitmq-cluster-operator is deliberately absent: it is applied by a Flux
  # Kustomization, and a HelmRelease dependsOn can only wait on HelmReleases.
  assert_eq "the release waits on exactly the five operators it needs" \
    "cert-manager external-secrets keystone-operator mariadb-operator memcached-operator" \
    "$(render_value "$rendered" "$hr | [.spec.dependsOn[].name] | sort | join(\" \")")"

  assert_eq "the release installs the nova-operator chart" \
    "nova-operator" "$(render_value "$rendered" "$hr | .spec.chart.spec.chart")"
  assert_eq "the chart range floors at the first published version" \
    ">=0.1.0 <1.0.0" "$(render_value "$rendered" "$hr | .spec.chart.spec.version")"

  # The digest ConfigMap is optional, so the Quick Start renders the release
  # unchanged when hack/refresh-operator-image-digests.sh never ran.
  assert_eq "the release reads the image digest from its ConfigMap" \
    "nova-operator-image-digest" \
    "$(render_value "$rendered" "$hr | .spec.valuesFrom[0].name")"
  assert_eq "an absent digest ConfigMap is tolerated" \
    "true" "$(render_value "$rendered" "$hr | .spec.valuesFrom[0].optional")"

  assert_eq "the operator namespace is rendered too" \
    "nova-system" \
    "$(render_value "$rendered" 'select(.kind == "Namespace" and .metadata.name == "nova-system") | .metadata.name')"
}

# --- Test 4: the kind base suspends the release and exposes the three hostnames ---
test_kind_base_suspends_and_declares_the_nova_listeners() {
  echo "Test: kustomize build deploy/kind/base suspends the release and declares each Nova listener once"

  tools_missing 13 && return

  local rendered
  if ! rendered="$(render_dir "$KIND_BASE_DIR")"; then
    echo "  FAIL: kustomize build $KIND_BASE_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 13))
    return
  fi

  local hr='select(.kind == "HelmRelease" and .metadata.name == "nova-operator")'
  assert_eq "the kind overlay suspends the release" \
    "true" "$(render_value "$rendered" "$hr | .spec.suspend")"

  local gw='select(.kind == "Gateway" and .metadata.name == "openstack-gw")'

  local entry name hostname cert listener
  for entry in "${NOVA_LISTENERS[@]}"; do
    read -r name hostname cert <<<"$entry"
    listener="$gw | .spec.listeners[] | select(.name == \"$name\")"

    # Listener names must be unique within a Gateway: a duplicate gets the whole
    # Gateway rejected on apply.
    assert_eq "the $name listener is declared once" \
      "1" "$(render_value "$rendered" "$gw | .spec.listeners | map(select(.name == \"$name\")) | length")"
    assert_eq "the $name listener answers on its own hostname" \
      "$hostname" "$(render_value "$rendered" "$listener | .hostname")"
    assert_eq "the $name listener terminates with its own certificate" \
      "$cert" "$(render_value "$rendered" "$listener | .tls.certificateRefs[0].name")"
    # Same-namespace routes only: an HTTPRoute from another namespace must not
    # be able to claim a Nova hostname.
    assert_eq "the $name listener admits routes from its own namespace only" \
      "Same" "$(render_value "$rendered" "$listener | .allowedRoutes.namespaces.from")"
  done
}

# --- Test 5: the kind infrastructure overlay renders the certificates and shims ---
test_kind_infrastructure_renders_certificates_and_shims() {
  echo "Test: kustomize build deploy/kind/infrastructure renders the nova certificates and shims"

  tools_missing 15 && return

  local rendered
  if ! rendered="$(render_dir "$KIND_INFRA_DIR")"; then
    echo "  FAIL: kustomize build $KIND_INFRA_DIR failed:"
    echo "$rendered" | head -20
    FAIL=$((FAIL + 15))
    return
  fi

  local entry name hostname cert crt
  for entry in "${NOVA_LISTENERS[@]}"; do
    read -r name hostname cert <<<"$entry"
    crt="select(.kind == \"Certificate\" and .metadata.name == \"$cert\")"

    # The Gateway references the Secret by the Certificate's name, so the two
    # have to agree.
    assert_eq "$cert writes the Secret its listener references" \
      "$cert" "$(render_value "$rendered" "$crt | .spec.secretName")"
    assert_eq "$cert covers a single hostname" \
      "1" "$(render_value "$rendered" "$crt | .spec.dnsNames | length")"
    assert_eq "$cert covers the hostname of its listener" \
      "$hostname" "$(render_value "$rendered" "$crt | .spec.dnsNames[0]")"
  done

  # One shim per database block of a Nova: apiDatabase.secretRef selects
  # nova-api-db, database.secretRef selects nova-db.
  local shim remote es
  for shim in "nova-api-db openstack/nova/openstack/standalone/api-db" \
              "nova-db openstack/nova/openstack/standalone/db"; do
    read -r name remote <<<"$shim"
    es="select(.kind == \"ExternalSecret\" and .metadata.name == \"$name\")"

    assert_eq "$name materializes the Secret the CR selects" \
      "$name" "$(render_value "$rendered" "$es | .spec.target.name")"
    assert_eq "$name reads its own standalone OpenBao path" \
      "$remote" "$(render_value "$rendered" "$es | [.spec.data[].remoteRef.key] | unique | join(\",\")")"
    # A brownfield Nova reads the SQL user from the Secret, so username is not
    # optional here.
    assert_eq "$name carries both the user and the password" \
      "username,password" "$(render_value "$rendered" "$es | [.spec.data[].secretKey] | join(\",\")")"
  done
}

# --- Run ---
test_new_files_carry_spdx
test_renovate_names_none_of_the_new_files
test_flux_system_renders_the_release
test_kind_base_suspends_and_declares_the_nova_listeners
test_kind_infrastructure_renders_certificates_and_shims

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
