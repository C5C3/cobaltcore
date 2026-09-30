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
#      OVNCentrals directly after the ControlPlanes, the proving OpenBao
#      instance on DeletePVCs before the infrastructure overlay, a wait for
#      the stack CRs in openstack that are still being reaped before the
#      operators go, which reads every namespaced stack kind in one call, no
#      cluster-scoped or platform kind, and passes the Gateway and the objects
#      nothing reaps), passes --ignore-not-found to every delete, resumes only
#      the suspended Flux objects that installed something, splits the base
#      render into the Gateway pass and the rest, deletes exactly the stack
#      CRDs of a mixed list, and names no namespace outside the stack's.
#   3. It exits 1 before any delete when the API server does not answer or yq
#      is missing, exits 1 when a wait runs out (naming the object, the
#      OVNCentral delete included, after which nothing else is deleted), when
#      a stack CR in openstack that is being deleted or whose owner is gone
#      outlives the wait, or the read of those CRs keeps failing (naming the
#      object or kubectl's error, before any operator is removed), when the
#      CRD scope cannot be read (before any operator is removed), when the
#      proving OpenBao instance cannot be switched to DeletePVCs, when a stack
#      CRD or namespace is left, and when the base render, the CRD list of
#      step 8 or the final namespace read fails, and exits 0 on a second run
#      that finds nothing.
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
controlplanes.c5c3.io
helmreleases.helm.toolkit.fluxcd.io
keystones.keystone.openstack.c5c3.io
images.openstack.k-orc.cloud
fluxinstances.fluxcd.controlplane.io
gateways.gateway.networking.k8s.io
gatewayclasses.gateway.networking.k8s.io"
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
#                          CRD, no OpenBao instance, no stack CRD, deletes of
#                          CR kinds report a missing mapping)
#   KUBECTL_DELETE_RC      exit code of every waiting delete (default 0)
#   KUBECTL_OVNCENTRAL_DELETE_RC
#                          exit code of the OVNCentral delete alone (default 0)
#   KUBECTL_OPENBAO_PATCH_RC
#                          exit code of the OpenBao instance patch (default 0)
#   KUBECTL_CRD_RC         non-empty: `get crd -o name` fails
#   KUBECTL_CRD_SCOPE_RC   non-empty: `get crd -o custom-columns=...` fails
#   KUBECTL_CR_READ_RC     non-empty: the read of the stack CRs in openstack
#                          fails
#   KUBECTL_CR_LEFT        non-empty: that read keeps answering a Keystone of
#                          this name that is being deleted
#   KUBECTL_CR_ORPHANED    non-empty: that read keeps answering a SecretStore of
#                          this name whose ControlPlane is gone
#   KUBECTL_CLUSTER_SCOPED_CR
#                          non-empty: a read that names the cluster-scoped
#                          gatewayclasses answers a GatewayClass being deleted
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
  # The CRDs of crds.txt with their scope and kind, as the custom-columns read
  # prints them. The platform's VerticalPodAutoscaler and Certificate are
  # namespaced, like the stack's.
  cat >"$dir/crd-columns.txt" <<'COLUMNS'
certificates.cert-manager.io                 Namespaced   Certificate
controlplanes.c5c3.io                        Namespaced   ControlPlane
helmreleases.helm.toolkit.fluxcd.io          Namespaced   HelmRelease
keystones.keystone.openstack.c5c3.io         Namespaced   Keystone
images.openstack.k-orc.cloud                 Namespaced   Image
fluxinstances.fluxcd.controlplane.io         Namespaced   FluxInstance
gateways.gateway.networking.k8s.io           Namespaced   Gateway
gatewayclasses.gateway.networking.k8s.io     Cluster      GatewayClass
verticalpodautoscalers.autoscaling.k8s.io    Namespaced   VerticalPodAutoscaler
certificates.cert.gardener.cloud             Namespaced   Certificate
ippools.crd.projectcalico.org                Cluster      IPPool
COLUMNS
  grep -E '(autoscaling\.k8s\.io|cert\.gardener\.cloud|crd\.projectcalico\.org) ' \
    "$dir/crd-columns.txt" >"$dir/crd-columns-after.txt"
  # What the stack-CR read finds in openstack after steps 1 and 2 when no
  # ControlPlane was ever applied: the base overlay's Gateway, the eso-tenant
  # Certificate hack/deploy-infra.sh applies, and its CertificateRequest. Nothing
  # reaps any of them before step 3.
  cat >"$dir/openstack-crs.json" <<'JSON'
{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"name":"openstack-gw","uid":"uid-gateway"}},
{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"eso-tenant-client-tls","uid":"uid-eso-cert"}},
{"apiVersion":"cert-manager.io/v1","kind":"CertificateRequest","metadata":{"name":"eso-tenant-client-tls-1","uid":"uid-eso-cr",
 "ownerReferences":[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","name":"eso-tenant-client-tls","uid":"uid-eso-cert"}]}}
JSON
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
  "get openbaoclusters.openbao.org openbao-instance -n openstack"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'error: the server doesn'"'"'t have a resource type "openbaoclusters"' >&2
      exit 1
    fi
    ;;
  "patch openbaoclusters.openbao.org openbao-instance "*)
    if [ "${KUBECTL_OPENBAO_PATCH_RC:-0}" != "0" ]; then
      echo 'Error from server (Forbidden): openbaoclusters.openbao.org "openbao-instance" is forbidden: User "lab" cannot patch resource "openbaoclusters"' >&2
      exit "${KUBECTL_OPENBAO_PATCH_RC}"
    fi
    ;;
  "get crd ovncentrals.ovn.openstack.c5c3.io"*)
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "ovncentrals.ovn.openstack.c5c3.io" not found' >&2
      exit 1
    fi
    ;;
  "get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope,KIND:.spec.names.kind --no-headers")
    if [ -n "${KUBECTL_CRD_SCOPE_RC:-}" ]; then
      echo 'Error from server (Forbidden): customresourcedefinitions.apiextensions.k8s.io is forbidden' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_SECOND_RUN:-}" ]; then
      cat "$dir/crd-columns-after.txt"
    else
      cat "$dir/crd-columns.txt"
    fi
    ;;
  "get "*" -n openstack -o json")
    if [ -n "${KUBECTL_CR_READ_RC:-}" ]; then
      echo 'error: You must be logged in to the server (Unauthorized)' >&2
      exit 1
    fi
    items="$(cat "$dir/openstack-crs.json")"
    if [ -n "${KUBECTL_CR_LEFT:-}" ]; then
      items="${items},{\"apiVersion\":\"keystone.openstack.c5c3.io/v1alpha1\",\"kind\":\"Keystone\",
        \"metadata\":{\"name\":\"${KUBECTL_CR_LEFT}\",\"uid\":\"uid-keystone\",\"deletionTimestamp\":\"2026-09-30T19:00:00Z\"}}"
    fi
    if [ -n "${KUBECTL_CR_ORPHANED:-}" ]; then
      items="${items},{\"apiVersion\":\"external-secrets.io/v1\",\"kind\":\"SecretStore\",
        \"metadata\":{\"name\":\"${KUBECTL_CR_ORPHANED}\",\"uid\":\"uid-store\",
        \"ownerReferences\":[{\"apiVersion\":\"c5c3.io/v1alpha1\",\"kind\":\"ControlPlane\",\"name\":\"controlplane\",\"uid\":\"uid-controlplane\"}]}}"
    fi
    if [ -n "${KUBECTL_CLUSTER_SCOPED_CR:-}" ]; then
      case "$args" in
        *gatewayclasses.gateway.networking.k8s.io*)
          items="${items},{\"apiVersion\":\"gateway.networking.k8s.io/v1\",\"kind\":\"GatewayClass\",
            \"metadata\":{\"name\":\"envoy\",\"uid\":\"uid-gatewayclass\",\"deletionTimestamp\":\"2026-09-30T19:00:00Z\"}}"
          ;;
      esac
    fi
    printf '{"apiVersion":"v1","kind":"List","items":[%s]}\n' "$items"
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
      -e "s# --type merge -p {\"spec\":{\"deletionPolicy\":\"DeletePVCs\"}}##" \
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
    echo "  SKIP: yq not installed (18 checks skipped)"
    SKIP=$((SKIP + 18))
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
  # By line start: the CRD scope read's custom-columns end in `.kind`.
  assert_eq "kind is never called" "" "$(grep '^kind ' "$CALL_LOG" || true)"
  assert_not_contains "docker is never called, even with PURGE_REGISTRY_CACHE=true" \
    "$(cat "$CALL_LOG")" "docker "
  assert_contains "the context is logged" "$output" "Kubeconfig context  : lab-forge"
  assert_contains "the final report counts nothing left" "$output" \
    "Stack CRDs left: 0; stack namespaces left: 0"

  local expected
  expected="$(printf '%s\n' \
    'kubectl delete controlplane --all -n openstack' \
    'kubectl delete ovncentrals.ovn.openstack.c5c3.io --all -n openstack' \
    'kubectl patch openbaoclusters.openbao.org openbao-instance -n openstack' \
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

  # The wait for the ControlPlane's descendants: after the overlays, before the
  # base overlay removes the operators that finalize them. One read of the
  # namespaced stack kinds; neither the cluster-scoped GatewayClass nor a
  # platform kind is read. The Gateway, the eso-tenant Certificate and its
  # CertificateRequest are not being reaped, so the first read ends the wait.
  local messaging_line wait_line operators_line reads read_kinds
  messaging_line="$(grep -n 'delete -k .*deploy/kind/messaging' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  wait_line="$(grep -n -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  operators_line="$(grep -n 'patch helmrelease c5c3-operator' "$CALL_LOG" | cut -d: -f1 | head -n1)"
  assert_eq "the stack CRs in openstack are read after the message-bus overlay" "true" \
    "$([[ -n "$wait_line" && -n "$messaging_line" && "$wait_line" -gt "$messaging_line" ]] && echo true || echo false)"
  assert_eq "and before the operators are resumed and removed" "true" \
    "$([[ -n "$wait_line" && -n "$operators_line" && "$wait_line" -lt "$operators_line" ]] && echo true || echo false)"
  reads="$(grep -c -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG")"
  assert_eq "the Gateway and the objects nothing reaps end the wait on the first read" "1" "$reads"
  read_kinds="$(grep -- '^kubectl get [^ ]* -n openstack -o json$' "$CALL_LOG" | head -n1 |
    cut -d' ' -f3 | tr ',' '\n' | sort)"
  assert_eq "the one read names every namespaced stack kind and nothing else" \
    "$(grep -vx 'gatewayclasses.gateway.networking.k8s.io' <<<"$STACK_CRDS" | sort)" "$read_kinds"

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
    echo "  SKIP: yq not installed (49 checks skipped)"
    SKIP=$((SKIP + 49))
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

  # A ControlPlane descendant that outlives the wait: exit 1 before any
  # operator is resumed or removed, naming the object, so its controller is
  # still there when the operator looks at it. One is being deleted, the other
  # has lost its ControlPlane and waits for garbage collection to mark it.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_LEFT=controlplane-keystone)"
  rc=$?
  assert_eq "a stack CR being deleted in openstack exits 1" "1" "$rc"
  assert_contains "the error says the CRs were not finalized" "$output" \
    "the stack CRs in openstack were not finalized within 1s"
  assert_contains "and names the object" "$output" "Keystone/controlplane-keystone"
  assert_not_contains "and not the Gateway nothing reaps before step 3" "$output" "Gateway/openstack-gw"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_ORPHANED=openbao-tenant-store)"
  rc=$?
  assert_eq "a stack CR whose ControlPlane is gone exits 1" "1" "$rc"
  assert_contains "and names the object" "$output" "SecretStore/openbao-tenant-store"
  assert_not_contains "and not the CertificateRequest whose Certificate is there" "$output" \
    "CertificateRequest/eso-tenant-client-tls-1"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  # A read that keeps failing is not an empty namespace.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CR_READ_RC=1)"
  rc=$?
  assert_eq "a stack-CR read that keeps failing exits 1" "1" "$rc"
  assert_contains "and names kubectl's error" "$output" \
    "cannot read them: error: You must be logged in to the server (Unauthorized)"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_CRD_SCOPE_RC=1)"
  rc=$?
  assert_eq "a CRD scope that cannot be read exits 1" "1" "$rc"
  assert_contains "says the scope cannot be read" "$output" "cannot read the scope of the cluster's CRDs"
  assert_not_contains "no operator is resumed or removed afterwards" "$(cat "$CALL_LOG")" "patch helmrelease"

  # The cluster-scoped GatewayClass is never read in openstack: kubectl would
  # ignore -n and report the one step 3 deletes.
  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true TEARDOWN_TIMEOUT=1 KUBECTL_CLUSTER_SCOPED_CR=1)"
  rc=$?
  assert_eq "a GatewayClass being deleted does not hold the wait" "0" "$rc"
  assert_not_contains "the cluster-scoped CRD is not read in openstack" \
    "$(grep -- ' -n openstack -o json$' "$CALL_LOG")" "gatewayclasses"

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
  assert_contains "in step 8, after the stack namespaces are deleted" "$(cat "$CALL_LOG")" \
    "kubectl delete namespace $(stack_namespace_names)"
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
  assert_not_contains "and patches no OpenBao instance that is gone" \
    "$(cat "$CALL_LOG")" "patch openbaoclusters"

  : >"$CALL_LOG"
  output="$(run_teardown "$tmp/bin" EXTERNAL_CLUSTER=true KUBECTL_OPENBAO_PATCH_RC=1)"
  rc=$?
  assert_nonzero_exit "an OpenBao instance that cannot be switched to DeletePVCs exits non-zero" "$rc"
  assert_contains "with kubectl's error" "$output" 'openbaoclusters.openbao.org "openbao-instance" is forbidden'
  assert_not_contains "before the infrastructure overlay is deleted" "$(cat "$CALL_LOG")" "kubectl delete -k"

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
