#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the external-cluster teardown of hack/teardown-infra.sh
# (EXTERNAL_CLUSTER=true):
#   1. EXTERNAL_CLUSTER, EXTERNAL_OVERLAY and TEARDOWN_TIMEOUT default and pass
#      through, and the default teardown still deletes the kind cluster without
#      calling kubectl.
#   2. The external teardown never calls kind or docker, deletes in the order
#      that lets every finalizer run while its controller exists (the
#      OVNCentrals directly after the ControlPlanes), passes
#      --ignore-not-found to every delete, resumes only the suspended Flux
#      objects that installed something, splits the base render into the
#      Gateway pass and the rest, deletes exactly the stack CRDs of a mixed
#      list, and names no namespace outside the stack's.
#   3. It exits 1 before any delete when the API server does not answer or yq
#      is missing, exits 1 when a wait runs out (naming the object, the
#      OVNCentral delete included, after which nothing else is deleted), when a
#      stack CRD or namespace is left, and when the base render, the CRD list
#      or the final namespace read fails, and exits 0 on a second run that
#      finds nothing.
#
# main() runs against a recording kubectl stub on a private PATH prefix and the
# real yq; the external-cluster checks are SKIP without yq.
#
# Usage: bash tests/unit/hack/teardown_infra_external_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEARDOWN_SH="$PROJECT_ROOT/hack/teardown-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

STACK_CRDS="certificates.cert-manager.io
helmreleases.helm.toolkit.fluxcd.io
keystones.keystone.openstack.c5c3.io
images.openstack.k-orc.cloud
fluxinstances.fluxcd.controlplane.io"
PLATFORM_CRDS="verticalpodautoscalers.autoscaling.k8s.io
certificates.cert.gardener.cloud
ippools.crd.projectcalico.org"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# kubectl, kind and docker stubs that append their argv to $CALL_LOG. The
# kubectl stub answers from the environment:
#   KUBECTL_VERSION_RC     exit code of `version` (default 0)
#   KUBECTL_SECOND_RUN     non-empty: the stack is gone (no Flux, c5c3 or ovn
#                          CRD, no stack CRD, deletes of CR kinds report a
#                          missing mapping)
#   KUBECTL_DELETE_RC      exit code of every waiting delete (default 0)
#   KUBECTL_OVNCENTRAL_DELETE_RC
#                          exit code of the OVNCentral delete alone (default 0)
#   KUBECTL_CRD_RC         non-empty: `get crd -o name` fails
#   KUBECTL_CRD_LEFT       a stack CRD the final report still finds
#   KUBECTL_NS_LEFT        a namespace the final report still finds
#   KUBECTL_NS_RC          non-empty: the final namespace read fails
#   KUBECTL_RENDER_RC      non-empty: `kustomize` fails
#   BASE_RENDER            file answering `kustomize` (default: base-render.yaml)
# A `delete -f -` records the kinds it received on stdin, and fails the way
# kubectl does when stdin holds no object.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  printf '%s\n' "$STACK_CRDS" "$PLATFORM_CRDS" |
    sed 's#^#customresourcedefinition.apiextensions.k8s.io/#' >"$dir/crds.txt"
  printf '%s\n' "$PLATFORM_CRDS" |
    sed 's#^#customresourcedefinition.apiextensions.k8s.io/#' >"$dir/crds-after.txt"
  cat >"$dir/helmreleases.json" <<'JSON'
{"items":[
  {"metadata":{"namespace":"c5c3-system","name":"c5c3-operator"},"spec":{"suspend":true},
   "status":{"history":[{"name":"c5c3-operator","version":1,"status":"deployed"}]}},
  {"metadata":{"namespace":"nova-system","name":"nova-operator"},"spec":{"suspend":true},"status":{}},
  {"metadata":{"namespace":"cert-manager","name":"cert-manager"},"spec":{},
   "status":{"history":[{"name":"cert-manager","version":1,"status":"deployed"}]}}
]}
JSON
  cat >"$dir/kustomizations.json" <<'JSON'
{"items":[
  {"metadata":{"namespace":"flux-system","name":"k-orc"},"spec":{"suspend":true},
   "status":{"inventory":{"entries":[{"id":"_orc-system__Namespace","v":"v1"}]}}},
  {"metadata":{"namespace":"flux-system","name":"never-applied"},"spec":{"suspend":true},"status":{}},
  {"metadata":{"namespace":"flux-system","name":"rabbitmq-cluster-operator"},"spec":{"prune":true},
   "status":{"inventory":{"entries":[{"id":"_rabbitmq-system__Namespace","v":"v1"}]}}}
]}
JSON
  cat >"$dir/base-render.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: openstack
---
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: openstack-gw
  namespace: openstack
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cert-manager
  namespace: cert-manager
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: jetstack
  namespace: flux-system
YAML

  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
args="$*"
case "$args" in
  "delete -f -"*)
    kinds="$(grep '^kind:' | sed 's/^kind: //' | sort | tr '\n' ' ')"
    echo "kubectl ${args} [kinds: ${kinds% }]" >>"$CALL_LOG"
    if [ -z "$kinds" ]; then
      echo "error: no objects passed to delete" >&2
      exit 1
    fi
    ;;
  *) echo "kubectl ${args}" >>"$CALL_LOG" ;;
esac
case "$args" in
  version*)
    if [ "${KUBECTL_VERSION_RC:-0}" != "0" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit "${KUBECTL_VERSION_RC}"
    fi
    echo "Client Version: v1.35.0"
    ;;
  "config current-context"*) echo "lab-forge" ;;
  "get crd controlplanes.c5c3.io"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "controlplanes.c5c3.io" not found' >&2
      exit 1
    fi
    ;;
  "get crd ovncentrals.ovn.openstack.c5c3.io"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "ovncentrals.ovn.openstack.c5c3.io" not found' >&2
      exit 1
    fi
    ;;
  "get crd -o name")
    if [ -n "${KUBECTL_CRD_RC:-}" ]; then
      echo 'error: You must be logged in to the server (Unauthorized)' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ] || grep -q 'delete customresourcedefinition' "$CALL_LOG"; then
      cat "$dir/crds-after.txt"
      if [ -n "${KUBECTL_CRD_LEFT:-}" ]; then
        echo "customresourcedefinition.apiextensions.k8s.io/${KUBECTL_CRD_LEFT}"
      fi
    else
      cat "$dir/crds.txt"
    fi
    ;;
  "get helmrelease -A -o json")
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "helmrelease"' >&2
      exit 1
    fi
    cat "$dir/helmreleases.json"
    ;;
  "get kustomization -n flux-system -o json")
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "kustomization"' >&2
      exit 1
    fi
    cat "$dir/kustomizations.json"
    ;;
  "get namespace "*)
    if [ -n "${KUBECTL_NS_RC:-}" ]; then
      echo 'Unable to connect to the server: dial tcp 10.128.0.1:443: i/o timeout' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_NS_LEFT:-}" ]; then echo "namespace/${KUBECTL_NS_LEFT}"; fi
    ;;
  "kustomize "*)
    if [ -n "${KUBECTL_RENDER_RC:-}" ]; then
      echo 'error: accumulating resources: must build at directory: not a valid directory' >&2
      exit 1
    fi
    cat "${BASE_RENDER:-$dir/base-render.yaml}"
    ;;
  delete*)
    case "$args" in
      "delete ovncentrals.ovn.openstack.c5c3.io "*)
        if [ "${KUBECTL_OVNCENTRAL_DELETE_RC:-0}" != "0" ]; then
          echo "error: timed out waiting for the condition on ovncentrals/controlplane-ovn" >&2
          exit "${KUBECTL_OVNCENTRAL_DELETE_RC}"
        fi
        ;;
    esac
    if [ "${KUBECTL_DELETE_RC:-0}" != "0" ]; then
      echo "error: timed out waiting for the condition on namespaces/openstack" >&2
      exit "${KUBECTL_DELETE_RC}"
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      case "$args" in
        "delete -k "* | "delete -f "*)
          echo 'error: resource mapping not found for name: "openstack-db" namespace: "openstack" from "STDIN": no matches for kind "MariaDB" in version "k8s.mariadb.com/v1alpha1"' >&2
          echo 'ensure CRDs are installed first' >&2
          exit 1
          ;;
        "delete fluxinstance "*)
          echo 'error: the server doesn'"'"'t have a resource type "fluxinstance"' >&2
          exit 1
          ;;
      esac
    fi
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"

  local tool
  for tool in kind docker; do
    printf '#!/bin/bash\necho "%s $*" >>"$CALL_LOG"\nexit 0\n' "$tool" >"$dir/$tool"
    chmod +x "$dir/$tool"
  done
}

# run_teardown <stub_dir> [env_var=value...]
# Runs hack/teardown-infra.sh with the stubs first on PATH and the given
# overrides. Echoes combined output; returns the script's exit status.
run_teardown() {
  local stub_dir="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY TEARDOWN_TIMEOUT PURGE_REGISTRY_CACHE
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    bash "$TEARDOWN_SH"
  ) 2>&1
}

# resolve <expression> [env_var=value...]
# Sources teardown-infra.sh with the given overrides and echoes <expression>.
resolve() {
  local expression="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY TEARDOWN_TIMEOUT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$TEARDOWN_SH"
    eval "printf '%s' \"${expression}\""
  )
}

# mutations <call log>
# The recorded kubectl calls that change the cluster, plus the render, with the
# delete flags every call carries stripped, one per line.
mutations() {
  grep -E '^kubectl (delete|patch|kustomize) ' "$1" |
    sed -e 's/ --ignore-not-found --wait --timeout=[0-9]*s//' \
      -e "s# --type merge -p {\"spec\":{\"suspend\":false}}##" \
      -e "s#${PROJECT_ROOT}/##g"
}

have_yq() {
  command -v yq >/dev/null 2>&1
}

# stack_namespace_names — the Namespaces of deploy/flux-system/namespaces.yaml,
# space-separated, then the two the kind base declares.
stack_namespace_names() {
  yq -N -r 'select(.kind == "Namespace") | .metadata.name' \
    "$PROJECT_ROOT/deploy/flux-system/namespaces.yaml" | tr '\n' ' '
  printf '%s' 'envoy-gateway-system headlamp-system'
}

# ---------------------------------------------------------------------------
# Test 1: the knobs
# ---------------------------------------------------------------------------
test_knobs() {
  echo "Test: EXTERNAL_CLUSTER, EXTERNAL_OVERLAY and TEARDOWN_TIMEOUT"

  assert_eq "EXTERNAL_CLUSTER defaults to false" "false" "$(resolve '${EXTERNAL_CLUSTER}')"
  assert_eq "EXTERNAL_CLUSTER=yes is preserved verbatim" "yes" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=yes)"
  assert_eq "TEARDOWN_TIMEOUT defaults to 600" "600" "$(resolve '${TEARDOWN_TIMEOUT}')"
  assert_eq "OVERLAY_ROOT resolves the default overlay against the repository" \
    "$PROJECT_ROOT/deploy/lab/metal-stack" "$(resolve '${OVERLAY_ROOT}')"
  assert_eq "an absolute EXTERNAL_OVERLAY is used unchanged" "/srv/overlays/lab" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_OVERLAY=/srv/overlays/lab)"
}

# ---------------------------------------------------------------------------
# Test 2: the default teardown is the kind one
# ---------------------------------------------------------------------------
test_default_deletes_the_kind_cluster() {
  echo "Test: the default teardown deletes the kind cluster and never calls kubectl"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=yes)"
  rc=$?
  assert_eq "the kind teardown exits 0" "0" "$rc"
  assert_contains "kind delete cluster is called" "$(cat "$CALL_LOG")" "kind delete cluster --name cobaltcore"
  assert_not_contains "kubectl is never called" "$(cat "$CALL_LOG")" "kubectl"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 3: the external teardown, in order
# ---------------------------------------------------------------------------
test_external_teardown_order() {
  echo "Test: the external teardown removes the stack in finalizer order"

  if ! have_yq; then
    echo "  SKIP: yq not installed (14 checks skipped)"
    SKIP=$((SKIP + 14))
    return
  fi

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true PURGE_REGISTRY_CACHE=true)"
  rc=$?
  assert_eq "the external teardown exits 0" "0" "$rc"
  assert_not_contains "kind is never called" "$(cat "$CALL_LOG")" "kind "
  assert_not_contains "docker is never called, even with PURGE_REGISTRY_CACHE=true" \
    "$(cat "$CALL_LOG")" "docker "
  assert_contains "the context is logged" "$output" "Kubeconfig context  : lab-forge"
  assert_contains "the final report counts nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0"

  local expected
  expected="$(printf '%s\n' \
    'kubectl delete controlplane --all -n openstack' \
    'kubectl delete ovncentrals.ovn.openstack.c5c3.io --all -n openstack' \
    'kubectl delete -k deploy/lab/metal-stack/infrastructure' \
    'kubectl delete -k deploy/kind/messaging' \
    'kubectl patch helmrelease c5c3-operator -n c5c3-system' \
    'kubectl patch kustomization k-orc -n flux-system' \
    'kubectl kustomize deploy/lab/metal-stack/base' \
    'kubectl delete -f - [kinds: Gateway GatewayClass]' \
    'kubectl delete -f - [kinds: HelmRelease HelmRepository]' \
    'kubectl delete -f deploy/kind/prometheus/release.yaml' \
    'kubectl delete pvc --all -n shared-services' \
    'kubectl delete pvc --all -n openstack' \
    'kubectl delete fluxinstance flux -n flux-system' \
    'kubectl delete namespace flux-system' \
    'kubectl delete clusterrolebinding flux-operator-cluster-admin' \
    'kubectl delete clusterrole flux-operator-edit flux-operator-view flux-web-admin flux-web-user' \
    "kubectl delete namespace $(stack_namespace_names)")"
  local actual
  actual="$(mutations "$CALL_LOG" | grep -v 'customresourcedefinition')"
  assert_eq "the deletes run in finalizer order (patches only for installed, suspended objects)" \
    "$expected" "$actual"

  local deletes without_flag
  deletes="$(grep -E '^kubectl delete ' "$CALL_LOG")"
  without_flag="$(grep -v -e '--ignore-not-found' <<<"$deletes" || true)"
  assert_eq "every delete carries --ignore-not-found" "" "$without_flag"
  assert_eq "every delete waits, bounded by TEARDOWN_TIMEOUT" "" \
    "$(grep -v -e '--wait --timeout=600s' <<<"$deletes" || true)"

  local crd_line crd_args
  crd_line="$(grep -E '^kubectl delete customresourcedefinition' "$CALL_LOG")"
  assert_eq "the CRDs are deleted last" "$crd_line" "$(tail -n1 <<<"$deletes")"
  crd_args="$(tr ' ' '\n' <<<"$crd_line" | grep '^customresourcedefinition' |
    sed 's#^customresourcedefinition.apiextensions.k8s.io/##' | sort)"
  assert_eq "exactly the stack CRDs are deleted" "$(sort <<<"$STACK_CRDS")" "$crd_args"
  local platform
  for platform in $PLATFORM_CRDS; do
    assert_not_contains "the platform CRD ${platform} is never deleted" "$crd_line" "$platform"
  done

  # Every namespace the teardown names: none outside the stack's.
  local named
  named="$(grep -E '^kubectl delete (namespace|ns) ' "$CALL_LOG" |
    sed -e 's/^kubectl delete [a-z]* //' -e 's/ --ignore-not-found.*//' | tr ' ' '\n' |
    grep -vxF -f <({ echo flux-system; stack_namespace_names | tr ' ' '\n'; }) || true)"
  assert_eq "no namespace outside the stack's is deleted" "" "$named"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 4: the script names no platform namespace
# ---------------------------------------------------------------------------
test_script_names_no_platform_namespace() {
  echo "Test: teardown-infra.sh names no namespace outside the stack's"

  local deletes named
  deletes="$(grep -E '(delete|delete_and_wait "[^"]*") (namespace|ns) ' "$TEARDOWN_SH")"
  assert_eq "the script deletes namespaces by name on two lines" "2" "$(grep -c . <<<"$deletes")"
  # The one list it deletes is read from stack_namespaces; the run in Test 3
  # checks what that list holds.
  named="$(sed -E 's/.* (namespace|ns) //' <<<"$deletes" | tr ' ' '\n' |
    grep -vxF -e flux-system -e '"${namespaces_to_delete[@]}"' || true)"
  assert_eq "every namespace delete in the script names flux-system or the stack_namespaces list" "" "$named"
  assert_file_contains_fixed "the deleted list is read from stack_namespaces" \
    "$TEARDOWN_SH" 'namespace_list="$(stack_namespaces)"'
  local ns
  for ns in kube-system firewall metallb-system default; do
    assert_file_not_contains "the script never names namespace ${ns} for deletion" \
      "$TEARDOWN_SH" "namespace ${ns}"
  done
}

# ---------------------------------------------------------------------------
# Test 5: the abort paths and the second run
# ---------------------------------------------------------------------------
test_external_teardown_failures() {
  echo "Test: the external teardown aborts on an unreachable cluster, a timeout or leftovers"

  if ! have_yq; then
    echo "  SKIP: yq not installed (27 checks skipped)"
    SKIP=$((SKIP + 27))
    return
  fi

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_VERSION_RC=1)"
  rc=$?
  assert_nonzero_exit "an unreachable API server exits non-zero" "$rc"
  assert_contains "quotes kubectl's error" "$output" "api.lab.example:443 was refused"
  assert_not_contains "nothing is deleted" "$(cat "$CALL_LOG")" "kubectl delete"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_DELETE_RC=1)"
  rc=$?
  assert_eq "a wait that runs out exits 1" "1" "$rc"
  assert_contains "the error says the delete did not finish" "$output" "did not finish within 600s"
  assert_contains "the error names the object still present" "$output" "namespaces/openstack"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NS_LEFT=openstack)"
  rc=$?
  assert_eq "a namespace left behind exits 1" "1" "$rc"
  assert_contains "the report counts it" "$output" "stack namespaces left: 1"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_LEFT=certificates.cert-manager.io)"
  rc=$?
  assert_eq "a stack CRD left behind exits 1" "1" "$rc"
  assert_contains "the report counts it" "$output" "Stack CRDs left: 1; stack namespaces left: 0"
  assert_contains "and says objects were left" "$output" "the teardown left stack objects behind"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_RC=1)"
  rc=$?
  assert_eq "a CRD list that cannot be read exits 1" "1" "$rc"
  assert_contains "says the CRDs cannot be listed" "$output" "cannot list the cluster's CRDs"
  assert_not_contains "and deletes no CRD blind" "$(cat "$CALL_LOG")" "delete customresourcedefinition"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_RENDER_RC=1)"
  rc=$?
  assert_eq "a base overlay that does not render exits 1" "1" "$rc"
  assert_contains "says the overlay cannot be rendered" "$output" "cannot render"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_NS_RC=1)"
  rc=$?
  assert_eq "a final namespace read that fails exits 1" "1" "$rc"
  assert_contains "says the namespaces cannot be read" "$output" "cannot read the stack namespaces"

  : >"$CALL_LOG"
  yq 'select(.kind != "Gateway" and .kind != "GatewayClass")' "$tmp/bin/base-render.yaml" \
    >"$tmp/no-gateway.yaml"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true BASE_RENDER="$tmp/no-gateway.yaml")"
  rc=$?
  assert_eq "an overlay without a Gateway still tears down (the empty pass is absence)" "0" "$rc"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_SECOND_RUN=1)"
  rc=$?
  assert_eq "a second run that finds nothing exits 0" "0" "$rc"
  assert_contains "and reports nothing left" "$output" "Stack CRDs left: 0; stack namespaces left: 0"
  assert_not_contains "and deletes no ControlPlane without the c5c3 CRD" \
    "$(cat "$CALL_LOG")" "delete controlplane"
  assert_not_contains "and deletes no OVNCentral without the ovn CRD" \
    "$(cat "$CALL_LOG")" "delete ovncentrals"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_OVNCENTRAL_DELETE_RC=1)"
  rc=$?
  assert_eq "an OVNCentral delete that runs out exits 1" "1" "$rc"
  assert_contains "the error names the OVNCentral step" "$output" \
    "deleting the OVNCentrals in openstack failed or did not finish within 600s"
  assert_contains "the error names the OVNCentral still present" "$output" \
    "error: timed out waiting for the condition on ovncentrals/controlplane-ovn"
  assert_not_contains "and no later step deletes anything" "$(cat "$CALL_LOG")" "kubectl delete -k"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Test 6: yq is required
# ---------------------------------------------------------------------------
test_requires_yq() {
  echo "Test: the external teardown requires yq"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stubs "$tmp/bin"
  ln -s "$(command -v dirname)" "$tmp/bin/dirname"
  ln -s "$(command -v date)" "$tmp/bin/date"
  export CALL_LOG="$tmp/calls.log"
  : >"$CALL_LOG"

  output="$(
    unset TEARDOWN_TIMEOUT
    EXTERNAL_CLUSTER=true PATH="$tmp/bin" "$BASH" "$TEARDOWN_SH" 2>&1
  )"
  rc=$?
  assert_nonzero_exit "the external teardown exits non-zero without yq" "$rc"
  assert_contains "names yq as missing" "$output" "'yq' is not installed"
  assert_not_contains "nothing is deleted" "$(cat "$CALL_LOG")" "kubectl delete"
  unset CALL_LOG
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs
test_default_deletes_the_kind_cluster
test_external_teardown_order
test_script_names_no_platform_namespace
test_external_teardown_failures
test_requires_yq

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
