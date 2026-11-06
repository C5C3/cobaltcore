#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Replay the install scripts of the nine operator network-policy-egress suites
# (keystone, horizon, glance, placement, barbican, neutron, cinder, nova and
# ovn) from Chainsaw's working directory, without a cluster. Helm checks the
# real chart and values paths. An EndpointSlice without an address or without
# a port stops the script before helm runs.
# Usage: bash tests/unit/ci/operator_networkpolicy_paths_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

if ! command -v yq >/dev/null 2>&1 || ! yq --version 2>&1 | grep -q mikefarah \
  || ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: mikefarah yq and jq are required"
  exit 0
fi

STUBS="$(mktemp -d "${TMPDIR:-/tmp}/operator-netpol-paths.XXXXXX")"
trap 'rm -rf "$STUBS"' EXIT
mkdir "$STUBS/bin"
ln -s "$PROJECT_ROOT" "$STUBS/workspace with spaces"

cat >"$STUBS/bin/kubectl" <<'STUB'
#!/bin/bash
set -eu
[ "$*" = "get endpointslice kubernetes -n default -o json" ]
slice='{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[{"port":6443}]}'
printf '%s\n' "${STUB_SLICE:-$slice}"
STUB

cat >"$STUBS/bin/helm" <<'STUB'
#!/bin/bash
set -eu
[ "$1" = install ]
[ -f "$3/Chart.yaml" ]
printf '%s\n' "$3" >"$PATH_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = --values ]; then
    [ -f "$2" ]
    printf '%s\n' "$2" >>"$PATH_LOG"
    exit 0
  fi
  shift
done
echo "ERROR: Helm install has no values file" >&2
exit 1
STUB
chmod +x "$STUBS/bin/kubectl" "$STUBS/bin/helm"

for operator in keystone horizon glance placement barbican neutron cinder nova ovn; do
  suite="$PROJECT_ROOT/tests/e2e/$operator-operator/network-policy-egress"
  yq -r '.spec.steps[0].try[0].script.content' "$suite/chainsaw-test.yaml" \
    >"$STUBS/install.sh"
  assert_not_empty "$operator install script is extracted" "$(cat "$STUBS/install.sh")"
  for mode in local-root local-suite ci spaces; do
    root="$PROJECT_ROOT"
    if [ "$mode" = spaces ]; then
      root="$STUBS/workspace with spaces"
    fi
    rc=0
    (
      export PATH="$STUBS/bin:$PATH" PATH_LOG="$STUBS/paths.log"
      unset GITHUB_WORKSPACE
      case "$mode" in
        local-root) cd "$PROJECT_ROOT" || exit 1 ;;
        local-suite) cd "$suite" || exit 1 ;;
        ci) export GITHUB_WORKSPACE="$root"; cd "$suite" || exit 1 ;;
        spaces) export GITHUB_WORKSPACE="$root"; cd "$STUBS" || exit 1 ;;
      esac
      bash "$STUBS/install.sh"
    ) >"$STUBS/output" 2>&1 || rc=$?
    assert_eq "$operator $mode install resolves existing paths" "0" "$rc"
    if [ "$rc" -ne 0 ]; then
      cat "$STUBS/output"
      continue
    fi
    assert_eq "$operator $mode passes chart as one absolute argument" \
      "$root/operators/$operator/helm/$operator-operator/" \
      "$(sed -n '1p' "$STUBS/paths.log")"
    assert_eq "$operator $mode passes values as one absolute argument" \
      "$root/tests/e2e/$operator-operator/network-policy-egress/00-install-operator.yaml" \
      "$(sed -n '2p' "$STUBS/paths.log")"
  done

  rc=0
  (
    unset GITHUB_WORKSPACE
    cd "$STUBS" || exit 1
    PATH="$STUBS/bin:$PATH" bash "$STUBS/install.sh"
  ) >"$STUBS/output" 2>&1 || rc=$?
  assert_nonzero_exit "$operator fails outside a checkout without a workspace" "$rc"
  assert_contains "$operator reports the missing repository" \
    "$(cat "$STUBS/output")" "not a git repository"

  rm -f "$STUBS/paths.log"
  rc=0
  (
    export PATH="$STUBS/bin:$PATH" PATH_LOG="$STUBS/paths.log"
    export STUB_SLICE='{"endpoints":[],"ports":[{"port":6443}]}'
    cd "$suite" || exit 1
    bash "$STUBS/install.sh"
  ) >"$STUBS/output" 2>&1 || rc=$?
  assert_eq "$operator stops on an EndpointSlice without an address" "1" "$rc"
  assert_contains "$operator names the empty EndpointSlice" "$(cat "$STUBS/output")" \
    "ERROR: EndpointSlice default/kubernetes carries no address or no port"
  assert_eq "$operator calls no helm without an address" "absent" \
    "$([ -e "$STUBS/paths.log" ] && echo present || echo absent)"

  rm -f "$STUBS/paths.log"
  rc=0
  (
    export PATH="$STUBS/bin:$PATH" PATH_LOG="$STUBS/paths.log"
    export STUB_SLICE='{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[]}'
    cd "$suite" || exit 1
    bash "$STUBS/install.sh"
  ) >"$STUBS/output" 2>&1 || rc=$?
  assert_eq "$operator stops on an EndpointSlice without a port" "1" "$rc"
  assert_contains "$operator names the portless EndpointSlice" "$(cat "$STUBS/output")" \
    "ERROR: EndpointSlice default/kubernetes carries no address or no port"
  assert_eq "$operator calls no helm without a port" "absent" \
    "$([ -e "$STUBS/paths.log" ] && echo present || echo absent)"
done

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
