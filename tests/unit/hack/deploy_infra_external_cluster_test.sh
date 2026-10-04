#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the external-cluster mode of hack/deploy-infra.sh (EXTERNAL_CLUSTER=true):
#   1. EXTERNAL_CLUSTER and EXTERNAL_OVERLAY default and pass through verbatim,
#      and the derived OVERLAY_ROOT and PUBLIC_PORT follow the mode.
#   2. preflight_checks in external mode needs only kubectl and jq, still needs
#      yq under WITH_CONTROLPLANE=true, refuses each of the six kind-bound
#      opt-ins by name, accepts WITH_NFS=true only for an overlay with an
#      nfs/ kustomization (refusing an nfs/ without one, and only after the
#      base/ check, before the cluster is contacted), refuses an overlay
#      without its two kustomizations, a
#      CONTROLPLANE_NAME the overlay's rendered by-hand ControlPlane does not
#      carry (and nothing else with that name), a controlplane/ that does not
#      render, the lab overlay's ControlPlane, whose Cinder backends sit on
#      the in-cluster NFS server, without WITH_NFS=true (passing it with the
#      flag, and backends on a filer without it; refusing the server's full
#      name with a trailing dot and in mixed case as well), and a context
#      whose API server does not answer; the kind mode
#      still needs docker and kind, EXTERNAL_CLUSTER=yes included.
#   3. check_external_cluster refuses a cluster without a default StorageClass,
#      one with a node-local-dns DaemonSet, one without a Ready node, and one it
#      cannot read, and passes a cluster with a default class, a NotFound
#      DaemonSet and a Ready node, logging both. Under WITH_NFS=true it also
#      refuses a CSIDriver nfs.csi.k8s.io the HelmRelease
#      kube-system/csi-driver-nfs did not install and one it cannot read (an
#      error that says "not found" included), and passes an absent one or that
#      release's, beside a kubectl warning on stderr as well. For an overlay
#      with nfs/client-policy.yaml it then reads the node network from
#      ConfigMap kube-system/shoot-info and logs it, and refuses an absent or
#      unreadable ConfigMap, a value that is no IPv4 CIDR, and a node whose
#      InternalIP lies outside the network or is missing; without that file
#      or without WITH_NFS=true it reads no ConfigMap. Without WITH_NFS=true
#      it refuses, for the lab ControlPlane with its backends on a filer, an
#      absent CSIDriver nfs.csi.k8s.io, one without the Ephemeral lifecycle
#      mode, one it cannot read and a controlplane/ that does not render, and
#      passes one with that mode, logging its modes; under WITH_NFS=true or
#      WITH_CONTROLPLANE_CR=true, or for a ControlPlane without Cinder, it
#      reads no modes. apply_nfs_client_policy applies the template with the
#      network in place of NODE_NETWORK.
#   4. resolve_api_server_egress renders one egress rule per address on the
#      port EndpointSlice default/kubernetes publishes, /128 for IPv6, and
#      aborts on a slice without a port, without an address, or unreadable.
#   5. The script keeps one strict INFRA_ONLY gate, the un-pause patch carries
#      egressRules beside apiServerEndpointIPs and paused:false and follows
#      resolve_api_server_egress, the external Step 1 runs
#      check_external_cluster, Steps 3 and 5 apply the overlay root, Step 3
#      applies the client policy before its nfs/, behind an EXTERNAL_CLUSTER
#      and file gate, and waits for the nfs-client-modules rollout behind an
#      EXTERNAL_CLUSTER gate, the host NFS module load, the nofile cap and the
#      Keystone preload sit behind an EXTERNAL_CLUSTER gate, the CR
#      rewrite and the by-hand CR hint key on PUBLIC_PORT, the external
#      by-hand hint names the overlay's controlplane/ kustomization behind an
#      EXTERNAL_CLUSTER and file gate and its CR by CONTROLPLANE_NAME while the
#      kind hint keeps the bundled CR, and the external banners name the
#      port-forward on PUBLIC_PORT and the teardown.
#
# The script is sourced (its BASH_SOURCE guard keeps main() from running) and
# driven against a kubectl stub scripted through environment variables.
#
# Usage: bash tests/unit/hack/deploy_infra_external_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
DEPLOY_INFRA_SH="$PROJECT_ROOT/hack/deploy-infra.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_kubectl_stub <dir>
# A kubectl that records its argv in $KUBECTL_LOG and answers from the
# environment:
#   KUBECTL_VERSION_RC      exit code of `version` (default 0)
#   KUBECTL_UNREACHABLE     non-empty: every `get` fails with connection refused
#   KUBECTL_STORAGECLASSES  file answering `get storageclass -o json`
#   KUBECTL_DS_PRESENT      true: `get daemonset node-local-dns` succeeds
#   KUBECTL_DS_ERROR        non-empty: that lookup fails with this message
#   KUBECTL_NODES           file answering `get nodes -o json`
#   KUBECTL_SLICE           file answering `get endpointslice kubernetes`
#   KUBECTL_SLICE_RC        exit code of that lookup (default 0)
#   KUBECTL_KUSTOMIZE       file answering `kustomize <dir>` (default: empty)
#   KUBECTL_KUSTOMIZE_RC    exit code of that render (default 0)
#   KUBECTL_CSIDRIVER_OWNER the namespace/name labels `get csidriver` prints
#                           (default: no CSIDriver, which prints nothing under
#                           --ignore-not-found)
#   KUBECTL_CSIDRIVER_MODES the volumeLifecycleModes a `get csidriver` that asks
#                           for them prints (default: no CSIDriver)
#   KUBECTL_CSIDRIVER_ERROR non-empty: that read fails with this message
#   KUBECTL_CSIDRIVER_NOISE non-empty: that read first writes an aggregated-API
#                           error to stderr, as kubectl does while the
#                           metrics-server APIService is down
#   KUBECTL_NODE_NETWORK    data.nodeNetwork of ConfigMap kube-system/shoot-info,
#                           which `get configmap` prints (default
#                           10.128.44.0/22; empty: no such ConfigMap, which
#                           prints nothing under --ignore-not-found)
#   KUBECTL_SHOOT_INFO_ERROR non-empty: that read fails with this message
#   KUBECTL_APPLY_STDIN     file that receives the stdin of `apply -f -`
make_kubectl_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
echo "kubectl $*" >>"${KUBECTL_LOG:-/dev/null}"
case "${1:-}" in
  version)
    if [ "${KUBECTL_VERSION_RC:-0}" != "0" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit "${KUBECTL_VERSION_RC}"
    fi
    echo "Client Version: v1.35.0"
    exit 0
    ;;
  config)
    case "${2:-}" in
      current-context) echo "lab-forge" ;;
      view) echo "https://api.lab.example" ;;
    esac
    exit 0
    ;;
  kustomize)
    if [ "${KUBECTL_KUSTOMIZE_RC:-0}" != "0" ]; then
      echo "error: accumulating resources: open ${2:-}/missing.yaml: no such file or directory" >&2
      exit "${KUBECTL_KUSTOMIZE_RC}"
    fi
    cat "${KUBECTL_KUSTOMIZE:-/dev/null}"
    exit 0
    ;;
  get)
    if [ -n "${KUBECTL_UNREACHABLE:-}" ]; then
      echo "The connection to the server api.lab.example:443 was refused - did you specify the right host or port?" >&2
      exit 1
    fi
    case "${2:-}" in
      storageclass) cat "${KUBECTL_STORAGECLASSES}" ;;
      daemonset)
        if [ -n "${KUBECTL_DS_ERROR:-}" ]; then echo "${KUBECTL_DS_ERROR}" >&2; exit 1; fi
        if [ "${KUBECTL_DS_PRESENT:-false}" = "true" ]; then echo "node-local-dns   2   2   2"; exit 0; fi
        echo 'Error from server (NotFound): daemonsets.apps "node-local-dns" not found' >&2
        exit 1
        ;;
      nodes) cat "${KUBECTL_NODES}" ;;
      csidriver)
        if [ -n "${KUBECTL_CSIDRIVER_NOISE:-}" ]; then
          echo 'E1003 10:00:00.000000    4242 memcache.go:287] couldn'"'"'t get resource list for metrics.k8s.io/v1beta1: the server is currently unable to handle the request' >&2
        fi
        if [ -n "${KUBECTL_CSIDRIVER_ERROR:-}" ]; then echo "${KUBECTL_CSIDRIVER_ERROR}" >&2; exit 1; fi
        # The lifecycle modes or the namespace/name labels, whichever the
        # jsonpath read asks for.
        case "$*" in
          *volumeLifecycleModes*) value="${KUBECTL_CSIDRIVER_MODES:-}" ;;
          *) value="${KUBECTL_CSIDRIVER_OWNER:-}" ;;
        esac
        if [ -n "${value}" ]; then printf '%s' "${value}"; exit 0; fi
        [[ "$*" != *--ignore-not-found* ]] || exit 0
        echo 'Error from server (NotFound): csidrivers.storage.k8s.io "nfs.csi.k8s.io" not found' >&2
        exit 1
        ;;
      configmap)
        if [ -n "${KUBECTL_SHOOT_INFO_ERROR:-}" ]; then echo "${KUBECTL_SHOOT_INFO_ERROR}" >&2; exit 1; fi
        printf '%s' "${KUBECTL_NODE_NETWORK-10.128.44.0/22}"
        ;;
      endpointslice)
        if [ "${KUBECTL_SLICE_RC:-0}" != "0" ]; then
          echo 'Error from server (Forbidden): endpointslices.discovery.k8s.io "kubernetes" is forbidden' >&2
          exit "${KUBECTL_SLICE_RC}"
        fi
        cat "${KUBECTL_SLICE}"
        ;;
    esac
    ;;
  apply)
    if [ "$*" = "apply -f -" ] && [ -n "${KUBECTL_APPLY_STDIN:-}" ]; then cat >"${KUBECTL_APPLY_STDIN}"; fi
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# make_stub_path <dir> <cmd>...
# A private PATH: an exit-0 shim for each <cmd>, plus the two utilities the
# sourced script needs itself (dirname for SCRIPT_DIR, date for log). A
# kubectl shim is the scripted stub above.
make_stub_path() {
  local dir="$1" cmd
  shift
  mkdir -p "$dir"
  ln -s "$(command -v dirname)" "$dir/dirname"
  ln -s "$(command -v date)" "$dir/date"
  for cmd in "$@"; do
    if [ "$cmd" = "kubectl" ]; then
      make_kubectl_stub "$dir"
      continue
    fi
    printf '#!/bin/bash\nexit 0\n' >"$dir/$cmd"
    chmod +x "$dir/$cmd"
  done
}

# resolve <expression> [env_var=value...]
# Sources deploy-infra.sh with the given overrides and echoes <expression>.
resolve() {
  local expression="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY KIND_HOST_PORT
    for assignment in "$@"; do
      export "${assignment?}"
    done
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    eval "printf '%s' \"${expression}\""
  )
}

# run_preflight <path> [env_var=value...]
# Sources deploy-infra.sh with PATH=<path> and the given overrides and runs
# preflight_checks. Echoes combined output; returns its exit status.
run_preflight() {
  local path="$1"
  shift
  (
    unset EXTERNAL_CLUSTER EXTERNAL_OVERLAY WITH_CONTROLPLANE WITH_CONTROLPLANE_CR \
      CONTROLPLANE_NAME WITH_VPA WITH_METRICS_SERVER WITH_REGISTRY_CACHE \
      WITH_CHAOS_MESH WITH_OVN_KERNEL_MODULES WITH_NFS WITH_DIZZY
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$path"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    preflight_checks
  ) 2>&1
}

# run_fn <stub_dir> <function> [env_var=value...]
# Sources deploy-infra.sh with the kubectl stub first on the real PATH (jq is
# the real one) and runs <function>. Echoes combined output; returns its status.
run_fn() {
  local stub_dir="$1" fn="$2"
  shift 2
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$stub_dir:$PATH"
    export PATH
    # shellcheck source=/dev/null
    source "$DEPLOY_INFRA_SH"
    "$fn"
    if [ "$fn" = "resolve_api_server_egress" ]; then
      echo "IPS=${API_SERVER_ENDPOINT_IPS}"
      echo "PORT=${API_SERVER_PORT}"
      echo "RULES=${API_SERVER_EGRESS_RULES}"
    fi
  ) 2>&1
}

# line_of <fixed string> — the first line of deploy-infra.sh containing it.
line_of() {
  grep -nF -- "$1" "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1
}

# assert_before <description> <line a> <line b> — a is a line before b.
assert_before() {
  if [ -n "$2" ] && [ -n "$3" ] && [ "$2" -lt "$3" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (line '${2}' is not before line '${3}')"
    FAIL=$((FAIL + 1))
  fi
}

STORAGECLASSES_WITH_DEFAULT='{"items":[
  {"metadata":{"name":"premium","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}},
  {"metadata":{"name":"standard"}}]}'
STORAGECLASSES_WITHOUT_DEFAULT='{"items":[
  {"metadata":{"name":"premium","annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}},
  {"metadata":{"name":"standard"}}]}'
NODES_READY='{"items":[
  {"metadata":{"name":"shoot-worker-a"},"status":{"conditions":[{"type":"Ready","status":"True"}],
    "addresses":[{"type":"InternalIP","address":"10.128.44.1"},{"type":"Hostname","address":"shoot-worker-a"}]}},
  {"metadata":{"name":"shoot-worker-b"},"status":{"conditions":[{"type":"Ready","status":"True"}],
    "addresses":[{"type":"InternalIP","address":"10.128.44.3"},{"type":"InternalIP","address":"fd00::3"}]}}]}'
NODES_WITHOUT_ADDRESS='{"items":[
  {"metadata":{"name":"shoot-worker-a"},"status":{"conditions":[{"type":"Ready","status":"True"}],
    "addresses":[{"type":"InternalIP","address":"10.128.44.1"}]}},
  {"metadata":{"name":"shoot-worker-b"},"status":{"conditions":[{"type":"Ready","status":"True"}],
    "addresses":[{"type":"Hostname","address":"shoot-worker-b"}]}}]}'
NODES_NOT_READY='{"items":[
  {"metadata":{"name":"shoot-worker-a"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}'

# ---------------------------------------------------------------------------
# Test 1: the knobs and the derived variables
# ---------------------------------------------------------------------------
test_knobs_and_derived_variables() {
  echo "Test: EXTERNAL_CLUSTER, EXTERNAL_OVERLAY, OVERLAY_ROOT and PUBLIC_PORT"

  assert_eq "EXTERNAL_CLUSTER defaults to false" "false" "$(resolve '${EXTERNAL_CLUSTER}')"
  assert_eq "EXTERNAL_OVERLAY defaults to deploy/lab/metal-stack" "deploy/lab/metal-stack" \
    "$(resolve '${EXTERNAL_OVERLAY}')"
  assert_eq "EXTERNAL_CLUSTER=true is preserved" "true" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=true)"
  assert_eq "EXTERNAL_CLUSTER=yes is preserved verbatim" "yes" \
    "$(resolve '${EXTERNAL_CLUSTER}' EXTERNAL_CLUSTER=yes)"
  assert_eq "EXTERNAL_OVERLAY passes through verbatim" "deploy/lab/other" \
    "$(resolve '${EXTERNAL_OVERLAY}' EXTERNAL_OVERLAY=deploy/lab/other)"

  assert_eq "OVERLAY_ROOT is deploy/kind by default" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}')"
  assert_eq "OVERLAY_ROOT is the lab overlay under EXTERNAL_CLUSTER=true" \
    "$PROJECT_ROOT/deploy/lab/metal-stack" "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=true)"
  assert_eq "an absolute EXTERNAL_OVERLAY is used unchanged" "/srv/overlays/lab" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY=/srv/overlays/lab)"
  assert_eq "EXTERNAL_OVERLAY is ignored in kind mode" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_OVERLAY=/srv/overlays/lab)"
  assert_eq "EXTERNAL_CLUSTER=yes keeps the kind overlay" "$PROJECT_ROOT/deploy/kind" \
    "$(resolve '${OVERLAY_ROOT}' EXTERNAL_CLUSTER=yes)"

  assert_eq "PUBLIC_PORT is 443 by default" "443" "$(resolve '${PUBLIC_PORT}')"
  assert_eq "PUBLIC_PORT follows KIND_HOST_PORT in kind mode" "8443" \
    "$(resolve '${PUBLIC_PORT}' KIND_HOST_PORT=8443)"
  assert_eq "PUBLIC_PORT is 8443 in external mode whatever KIND_HOST_PORT says" "8443" \
    "$(resolve '${PUBLIC_PORT}' EXTERNAL_CLUSTER=true KIND_HOST_PORT=9443)"
}

# ---------------------------------------------------------------------------
# Test 2: preflight in external mode
# ---------------------------------------------------------------------------
test_external_preflight() {
  echo "Test: preflight_checks under EXTERNAL_CLUSTER=true"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stub_path "$tmp/kubectl-jq" kubectl jq
  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_eq "passes with only kubectl and jq on PATH" "0" "$rc"
  assert_contains "reports success" "$output" "Pre-flight checks passed."
  assert_contains "logs the kubeconfig context" "$output" "Kubeconfig context  : lab-forge"
  assert_contains "logs the API server URL" "$output" "API server          : https://api.lab.example"
  assert_not_contains "never asks for docker" "$output" "'docker' is not installed"

  make_stub_path "$tmp/jq-only" jq
  output="$(run_preflight "$tmp/jq-only" EXTERNAL_CLUSTER=true)"
  rc=$?
  assert_nonzero_exit "fails without kubectl" "$rc"
  assert_contains "names kubectl as missing" "$output" "'kubectl' is not installed"

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true)"
  rc=$?
  assert_nonzero_exit "fails without yq under WITH_CONTROLPLANE=true" "$rc"
  assert_contains "names yq as required" "$output" "requires 'yq'"

  # The reader applies the overlay's controlplane/ by hand, and Step 7 seeds the
  # paths of openstack/CONTROLPLANE_NAME alone: a rendered ControlPlane under
  # another namespace or name, none or two are refused before the cluster is
  # contacted, unless the bundled CR is applied and renamed instead. The kubectl
  # stub renders an overlay whose CR is openstack/lab (with cat); the yq that
  # reads the render is the real one.
  if command -v yq >/dev/null 2>&1; then
    make_stub_path "$tmp/kubectl-jq-yq" kubectl jq
    ln -s "$(command -v yq)" "$tmp/kubectl-jq-yq/yq"
    ln -s "$(command -v cat)" "$tmp/kubectl-jq-yq/cat"
    mkdir -p "$tmp/lab/base" "$tmp/lab/infrastructure" "$tmp/lab/controlplane"
    : >"$tmp/lab/base/kustomization.yaml"
    : >"$tmp/lab/infrastructure/kustomization.yaml"
    : >"$tmp/lab/controlplane/kustomization.yaml"
    cat >"$tmp/lab-render.yaml" <<'YAML'
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNCentral
metadata:
  name: lab-ovn
  namespace: openstack
---
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: lab
  namespace: openstack
YAML
    export KUBECTL_LOG="$tmp/kubectl.log" KUBECTL_KUSTOMIZE="$tmp/lab-render.yaml"
    : >"$KUBECTL_LOG"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      EXTERNAL_OVERLAY="$tmp/lab")"
    rc=$?
    assert_nonzero_exit "a CONTROLPLANE_NAME the overlay's ControlPlane does not carry is refused" "$rc"
    assert_contains "the refusal names the overlay's CR and the override" "$output" \
      "controlplane renders ControlPlane 'openstack/lab', but Step 7 seeds openstack/controlplane; keep the CR in the openstack namespace and set CONTROLPLANE_NAME to its name."
    assert_eq "and comes before the cluster is contacted, after the overlay's render" \
      "kubectl kustomize $tmp/lab/controlplane" "$(cat "$KUBECTL_LOG")"
    unset KUBECTL_LOG
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=lab EXTERNAL_OVERLAY="$tmp/lab")"
    rc=$?
    assert_eq "the name the overlay's ControlPlane carries passes" "0" "$rc"
    sed 's/^  namespace: openstack$/  namespace: lab2/' "$tmp/lab-render.yaml" >"$tmp/lab2-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=lab EXTERNAL_OVERLAY="$tmp/lab" KUBECTL_KUSTOMIZE="$tmp/lab2-render.yaml")"
    rc=$?
    assert_nonzero_exit "a ControlPlane outside the openstack namespace is refused" "$rc"
    assert_contains "the refusal names the CR's namespace" "$output" \
      "renders ControlPlane 'lab2/lab', but Step 7 seeds openstack/lab;"
    sed '/^  namespace: openstack$/d' "$tmp/lab-render.yaml" >"$tmp/no-ns-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=lab EXTERNAL_OVERLAY="$tmp/lab" KUBECTL_KUSTOMIZE="$tmp/no-ns-render.yaml")"
    rc=$?
    assert_nonzero_exit "a ControlPlane without a namespace is refused" "$rc"
    assert_contains "the refusal shows the empty namespace" "$output" "renders ControlPlane '/lab'"
    sed -n '1,/^---$/p' "$tmp/lab-render.yaml" | sed '$d' >"$tmp/ovn-only-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      EXTERNAL_OVERLAY="$tmp/lab" KUBECTL_KUSTOMIZE="$tmp/ovn-only-render.yaml")"
    rc=$?
    assert_nonzero_exit "a controlplane/ without a ControlPlane is refused" "$rc"
    assert_contains "the refusal says none was rendered" "$output" \
      "controlplane must render exactly one ControlPlane (got: none); Step 7 seeds only openstack/controlplane."
    { cat "$tmp/lab-render.yaml"; echo "---"; cat "$tmp/lab2-render.yaml"; } >"$tmp/two-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=lab EXTERNAL_OVERLAY="$tmp/lab" KUBECTL_KUSTOMIZE="$tmp/two-render.yaml")"
    rc=$?
    assert_nonzero_exit "a controlplane/ with two ControlPlanes is refused" "$rc"
    assert_contains "the refusal lists both on one line" "$output" \
      "must render exactly one ControlPlane (got: openstack/lab, lab2/lab);"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      WITH_CONTROLPLANE_CR=true CONTROLPLANE_NAME=cp1 EXTERNAL_OVERLAY="$tmp/lab")"
    rc=$?
    assert_eq "an override passes when the bundled CR is applied and renamed" "0" "$rc"
    mkdir -p "$tmp/no-controlplane/base" "$tmp/no-controlplane/infrastructure"
    : >"$tmp/no-controlplane/base/kustomization.yaml"
    : >"$tmp/no-controlplane/infrastructure/kustomization.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=cp1 EXTERNAL_OVERLAY="$tmp/no-controlplane")"
    rc=$?
    assert_eq "an override passes for an overlay without controlplane/" "0" "$rc"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      CONTROLPLANE_NAME=lab EXTERNAL_OVERLAY="$tmp/lab" KUBECTL_KUSTOMIZE_RC=1)"
    rc=$?
    assert_nonzero_exit "an overlay whose controlplane/ does not render is refused" "$rc"
    assert_contains "the refusal names the directory" "$output" "cannot render $tmp/lab/controlplane"
    unset KUBECTL_KUSTOMIZE

    # The lab overlay's ControlPlane puts Cinder's volume and backup backends on
    # the NFS server only WITH_NFS=true deploys: refused without the flag, before
    # the cluster is contacted, and passed with it. The stub renders the
    # overlay's two manifests as its kustomization lists them.
    {
      cat "$PROJECT_ROOT/deploy/lab/metal-stack/controlplane/controlplane-lab.yaml"
      echo "---"
      cat "$PROJECT_ROOT/deploy/lab/metal-stack/controlplane/ovncentral.yaml"
    } >"$tmp/metal-stack-render.yaml"
    export KUBECTL_LOG="$tmp/kubectl.log"
    : >"$KUBECTL_LOG"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      KUBECTL_KUSTOMIZE="$tmp/metal-stack-render.yaml")"
    rc=$?
    assert_nonzero_exit "the lab overlay is refused without WITH_NFS=true" "$rc"
    assert_contains "the refusal names both NFS backends and the flag" "$output" \
      "controlplane puts the Cinder backends nfs1 nfsbk on the in-cluster NFS server nfs-server.openstack, which only WITH_NFS=true deploys; rerun with WITH_NFS=true."
    assert_not_contains "and comes before the cluster is contacted" "$(cat "$KUBECTL_LOG")" "kubectl version"
    unset KUBECTL_LOG
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      WITH_NFS=true KUBECTL_KUSTOMIZE="$tmp/metal-stack-render.yaml")"
    rc=$?
    assert_eq "the lab overlay passes with WITH_NFS=true" "0" "$rc"
    # A backend on an NFS server the overlay does not deploy, a filer, needs no
    # WITH_NFS=true; WITH_NFS=true would even be refused for an overlay without
    # nfs/. The backup backend names the in-cluster server by its shorter
    # namespace form, which still resolves to it.
    sed -e '/^      backends:$/,/^      backupBackend:$/s/nfs-server[.]openstack[.]svc[.]cluster[.]local/filer.example.com/' \
      -e 's/nfs-server[.]openstack[.]svc[.]cluster[.]local/nfs-server.openstack/' \
      "$tmp/metal-stack-render.yaml" >"$tmp/filer-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      KUBECTL_KUSTOMIZE="$tmp/filer-render.yaml")"
    rc=$?
    assert_nonzero_exit "a backup backend on nfs-server.openstack is refused without WITH_NFS=true" "$rc"
    assert_contains "the refusal names only that backend" "$output" \
      "controlplane puts the Cinder backends nfsbk on the in-cluster NFS server"
    sed 's/nfs-server[.]openstack$/filer.example.com/' "$tmp/filer-render.yaml" >"$tmp/filer-only-render.yaml"
    output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
      KUBECTL_KUSTOMIZE="$tmp/filer-only-render.yaml")"
    rc=$?
    assert_eq "Cinder on a filer passes without WITH_NFS=true" "0" "$rc"
    # The CRD admits any case and the full name with the root's trailing dot,
    # and both resolve to the in-cluster server from csi-nfs-node as well.
    local server
    for server in nfs-server.openstack.svc.cluster.local. NFS-Server.openstack; do
      sed "s/nfs-server[.]openstack[.]svc[.]cluster[.]local\$/${server}/" \
        "$tmp/metal-stack-render.yaml" >"$tmp/spelled-render.yaml"
      assert_contains "the render puts the backends on ${server}" \
        "$(cat "$tmp/spelled-render.yaml")" "server: ${server}"
      output="$(run_preflight "$tmp/kubectl-jq-yq" EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true \
        KUBECTL_KUSTOMIZE="$tmp/spelled-render.yaml")"
      rc=$?
      assert_nonzero_exit "backends on ${server} are refused without WITH_NFS=true" "$rc"
      assert_contains "the refusal names both" "$output" \
        "controlplane puts the Cinder backends nfs1 nfsbk on the in-cluster NFS server"
    done
  else
    echo "  SKIP: yq not installed (29 checks skipped)"
    SKIP=$((SKIP + 29))
  fi

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true KUBECTL_VERSION_RC=1)"
  rc=$?
  assert_nonzero_exit "fails when the API server does not answer" "$rc"
  assert_contains "quotes kubectl's error" "$output" "api.lab.example:443 was refused"

  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY=/nonexistent)"
  rc=$?
  assert_nonzero_exit "fails on an overlay without its kustomizations" "$rc"
  assert_contains "names EXTERNAL_OVERLAY" "$output" "EXTERNAL_OVERLAY='/nonexistent' has no base/ and infrastructure/ kustomization"

  mkdir -p "$tmp/half-overlay/base"
  : >"$tmp/half-overlay/base/kustomization.yaml"
  output="$(run_preflight "$tmp/kubectl-jq" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/half-overlay")"
  rc=$?
  assert_nonzero_exit "fails on an overlay with base/ alone" "$rc"
  assert_contains "names EXTERNAL_OVERLAY for the half overlay" "$output" "EXTERNAL_OVERLAY='$tmp/half-overlay'"
}

# ---------------------------------------------------------------------------
# Test 3: the refused kind-bound opt-ins, each named in its message
# ---------------------------------------------------------------------------
test_refused_flags() {
  echo "Test: preflight_checks under EXTERNAL_CLUSTER=true refuses each kind-bound opt-in by name"

  local tmp output rc flag
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub_path "$tmp/bin" kubectl jq
  export KUBECTL_LOG="$tmp/kubectl.log"

  # WITH_VPA=true folds into WITH_METRICS_SERVER=true at the top of the script,
  # so it has to be checked first to be named at all.
  for flag in WITH_VPA WITH_METRICS_SERVER WITH_REGISTRY_CACHE WITH_CHAOS_MESH \
    WITH_OVN_KERNEL_MODULES WITH_DIZZY; do
    : >"$KUBECTL_LOG"
    output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true "${flag}=true")"
    rc=$?
    assert_nonzero_exit "${flag}=true is refused" "$rc"
    assert_contains "the refusal names ${flag}" "$output" \
      "ERROR: EXTERNAL_CLUSTER=true does not support ${flag}=true:"
    assert_eq "${flag}=true is refused before the cluster is contacted" "" "$(cat "$KUBECTL_LOG")"
  done
  unset KUBECTL_LOG

  # The opt-ins that are plain manifests stay allowed.
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_PROMETHEUS=true WITH_MESSAGING=true INFRA_ONLY=true)"
  rc=$?
  assert_eq "WITH_PROMETHEUS, WITH_MESSAGING and INFRA_ONLY stay allowed" "0" "$rc"
}

# ---------------------------------------------------------------------------
# Test 3b: WITH_NFS=true needs the overlay's nfs/ kustomization
# ---------------------------------------------------------------------------
test_nfs_overlay_preflight() {
  echo "Test: preflight_checks under EXTERNAL_CLUSTER=true accepts WITH_NFS=true only with an nfs/ kustomization"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_stub_path "$tmp/bin" kubectl jq
  export KUBECTL_LOG="$tmp/kubectl.log"

  : >"$KUBECTL_LOG"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_NFS=true)"
  rc=$?
  assert_eq "WITH_NFS=true passes with the default overlay, which has nfs/" "0" "$rc"
  assert_contains "and reaches the end of preflight" "$output" "Pre-flight checks passed."

  # An overlay with the two kustomizations Steps 3 and 5 apply and no nfs/.
  mkdir -p "$tmp/no-nfs/base" "$tmp/no-nfs/infrastructure"
  : >"$tmp/no-nfs/base/kustomization.yaml"
  : >"$tmp/no-nfs/infrastructure/kustomization.yaml"
  local refusal="EXTERNAL_CLUSTER=true WITH_NFS=true needs $tmp/no-nfs/nfs/kustomization.yaml, which does not exist"

  : >"$KUBECTL_LOG"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_NFS=true EXTERNAL_OVERLAY="$tmp/no-nfs")"
  rc=$?
  assert_nonzero_exit "WITH_NFS=true is refused for an overlay without nfs/" "$rc"
  assert_contains "the refusal names the missing kustomization" "$output" "$refusal"
  assert_eq "and comes before the cluster is contacted" "" "$(cat "$KUBECTL_LOG")"

  # The check tests the file, not the directory.
  mkdir -p "$tmp/no-nfs/nfs"
  : >"$KUBECTL_LOG"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_NFS=true EXTERNAL_OVERLAY="$tmp/no-nfs")"
  rc=$?
  assert_nonzero_exit "an nfs/ directory without a kustomization is refused too" "$rc"
  assert_contains "with the same message" "$output" "$refusal"
  assert_eq "before the cluster is contacted" "" "$(cat "$KUBECTL_LOG")"

  # Only the value true turns the stack on.
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true EXTERNAL_OVERLAY="$tmp/no-nfs")"
  rc=$?
  assert_eq "the overlay without an nfs/ kustomization passes without WITH_NFS" "0" "$rc"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_NFS=yes EXTERNAL_OVERLAY="$tmp/no-nfs")"
  rc=$?
  assert_eq "and with WITH_NFS=yes" "0" "$rc"
  assert_not_contains "WITH_NFS=yes is not refused" "$output" "WITH_NFS=true needs"

  # The overlay check comes first: an overlay with neither base/ nor nfs/ is
  # reported for its base/.
  mkdir -p "$tmp/empty"
  output="$(run_preflight "$tmp/bin" EXTERNAL_CLUSTER=true WITH_NFS=true EXTERNAL_OVERLAY="$tmp/empty")"
  rc=$?
  assert_nonzero_exit "an overlay with neither base/ nor nfs/ is refused" "$rc"
  assert_contains "for its missing base/ and infrastructure/" "$output" \
    "EXTERNAL_OVERLAY='$tmp/empty' has no base/ and infrastructure/ kustomization"
  assert_not_contains "and not for its nfs/" "$output" "WITH_NFS=true needs"
  unset KUBECTL_LOG
}

# ---------------------------------------------------------------------------
# Test 4: the kind mode still needs docker and kind
# ---------------------------------------------------------------------------
test_kind_preflight_unchanged() {
  echo "Test: preflight_checks in kind mode still requires docker and kind"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  make_stub_path "$tmp/no-docker" kind kubectl jq
  output="$(run_preflight "$tmp/no-docker")"
  rc=$?
  assert_nonzero_exit "the kind mode fails without docker" "$rc"
  assert_contains "names docker as missing" "$output" "'docker' is not installed"

  make_stub_path "$tmp/no-kind" docker kubectl jq
  output="$(run_preflight "$tmp/no-kind" EXTERNAL_CLUSTER=yes)"
  rc=$?
  assert_nonzero_exit "EXTERNAL_CLUSTER=yes keeps the kind mode and fails without kind" "$rc"
  assert_contains "names kind as missing" "$output" "'kind' is not installed"
}

# ---------------------------------------------------------------------------
# Test 5: check_external_cluster
# ---------------------------------------------------------------------------
test_check_external_cluster() {
  echo "Test: check_external_cluster"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"
  printf '%s\n' "$STORAGECLASSES_WITH_DEFAULT" >"$tmp/sc-default.json"
  printf '%s\n' "$STORAGECLASSES_WITHOUT_DEFAULT" >"$tmp/sc-none.json"
  printf '%s\n' "$NODES_READY" >"$tmp/nodes-ready.json"
  printf '%s\n' "$NODES_NOT_READY" >"$tmp/nodes-not-ready.json"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "passes a cluster with a default class, no node-local-dns and Ready nodes" "0" "$rc"
  assert_contains "logs the default class" "$output" "Default StorageClass: premium"
  assert_contains "logs the Ready node names" "$output" "shoot-worker-a shoot-worker-b"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-none.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster without a default StorageClass" "$rc"
  assert_contains "names the missing default class" "$output" "no default StorageClass"
  assert_contains "names the overlay's volumes" "$output" "The lab overlay"

  output="$(run_fn "$tmp/bin" check_external_cluster KUBECTL_DS_PRESENT=true \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster with a node-local-dns DaemonSet" "$rc"
  assert_contains "names spec.network.dnsEndpointIPs" "$output" "spec.network.dnsEndpointIPs"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_DS_ERROR="Unable to connect to the server: net/http: TLS handshake timeout" \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses when the DaemonSet lookup fails with anything but not found" "$rc"
  assert_contains "quotes the lookup's error" "$output" "TLS handshake timeout"

  output="$(run_fn "$tmp/bin" check_external_cluster KUBECTL_UNREACHABLE=1)"
  rc=$?
  assert_nonzero_exit "refuses a cluster it cannot read" "$rc"
  assert_contains "reports the failed StorageClass read" "$output" "cannot list the cluster's StorageClasses"

  output="$(run_fn "$tmp/bin" check_external_cluster \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-not-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster without a Ready node" "$rc"
  assert_contains "names the missing Ready node" "$output" "no Ready node"

  # WITH_NFS=true: a CSIDriver nfs.csi.k8s.io must be absent or the one the
  # HelmRelease kube-system/csi-driver-nfs installed.
  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "WITH_NFS=true passes a cluster without the CSIDriver" "0" "$rc"

  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true \
    KUBECTL_CSIDRIVER_OWNER=kube-system/csi-driver-nfs \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "and one whose CSIDriver an earlier run's HelmRelease installed" "0" "$rc"

  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true KUBECTL_CSIDRIVER_OWNER=/ \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a CSIDriver the platform installed with helm" "$rc"
  assert_contains "names the CSIDriver and the HelmRelease it expected" "$output" \
    "ERROR: CSIDriver/nfs.csi.k8s.io exists and was not installed by the HelmRelease"
  assert_contains "quotes the labels it read" "$output" "namespace/name labels: '/'"
  assert_contains "and says why it refuses" "$output" "WITH_NFS=true would replace or"

  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true \
    KUBECTL_CSIDRIVER_OWNER=flux-system/platform-nfs \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses one another HelmRelease installed" "$rc"
  assert_contains "naming that release's labels" "$output" "labels: 'flux-system/platform-nfs'"

  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true \
    KUBECTL_CSIDRIVER_ERROR="Error from server (Forbidden): csidrivers.storage.k8s.io \"nfs.csi.k8s.io\" is forbidden" \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses when the CSIDriver lookup fails with anything but not found" "$rc"
  assert_contains "quotes that lookup's error" "$output" "csidrivers.storage.k8s.io \"nfs.csi.k8s.io\" is forbidden"
  assert_contains "and says it cannot tell whether the CSIDriver exists" "$output" \
    "ERROR: cannot determine whether CSIDriver/nfs.csi.k8s.io exists"

  # A failure whose message happens to say "not found" is still a failure.
  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true \
    KUBECTL_CSIDRIVER_ERROR="Unable to connect to the server: getting credentials: exec: executable kubectl-oidc_login not found" \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses when a credential plugin is not found, which is no absent CSIDriver" "$rc"
  assert_contains "and quotes that error" "$output" "executable kubectl-oidc_login not found"

  # kubectl warns on stderr and still exits 0 while an aggregated API is down;
  # the warning is no label.
  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true KUBECTL_CSIDRIVER_NOISE=1 \
    KUBECTL_CSIDRIVER_OWNER=kube-system/csi-driver-nfs \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "passes the HelmRelease's CSIDriver beside a kubectl warning" "0" "$rc"
  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=true KUBECTL_CSIDRIVER_NOISE=1 \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "and an absent CSIDriver beside one" "0" "$rc"

  output="$(run_fn "$tmp/bin" check_external_cluster WITH_NFS=false KUBECTL_CSIDRIVER_OWNER=/ \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "without WITH_NFS=true the platform's CSIDriver is no concern" "0" "$rc"

  # The node network of the overlay's nfs/client-policy.yaml. The checks above
  # ran in kind mode, whose overlay root ships no such file.
  printf '%s\n' "$NODES_WITHOUT_ADDRESS" >"$tmp/nodes-no-address.json"
  export KUBECTL_LOG="$tmp/kubectl.log"
  local shoot_info_read='kubectl get configmap shoot-info -n kube-system'

  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "the lab overlay under WITH_NFS=true passes nodes inside the node network" "0" "$rc"
  assert_contains "and logs the network" "$output" "NFS client network  : 10.128.44.0/22"
  assert_contains "read from ConfigMap kube-system/shoot-info" "$(cat "$KUBECTL_LOG")" \
    "$shoot_info_read --ignore-not-found -o jsonpath={.data.nodeNetwork}"

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_NODE_NETWORK= \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a cluster without ConfigMap kube-system/shoot-info" "$rc"
  assert_contains "says it names no node network" "$output" \
    "ERROR: ConfigMap kube-system/shoot-info names no IPv4 node network"
  assert_contains "quotes the empty value and names the template" "$output" \
    "(data.nodeNetwork: ''). $PROJECT_ROOT/deploy/lab/metal-stack/nfs/client-policy.yaml"

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_NODE_NETWORK=fd00:10::/64 \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses an IPv6 node network" "$rc"
  assert_contains "quoting it" "$output" "(data.nodeNetwork: 'fd00:10::/64')"

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_NODE_NETWORK=10.128.44.0/33 \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a prefix length above 32" "$rc"

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_SHOOT_INFO_ERROR='Error from server (Forbidden): configmaps "shoot-info" is forbidden' \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses when the ConfigMap read fails" "$rc"
  assert_contains "says it cannot read the ConfigMap" "$output" \
    "ERROR: cannot read ConfigMap kube-system/shoot-info"
  assert_contains "and quotes the read's error" "$output" 'configmaps "shoot-info" is forbidden'

  # 10.128.44.0/31 holds .0 and .1: shoot-worker-b's 10.128.44.3 is outside.
  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_NODE_NETWORK=10.128.44.0/31 \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_nonzero_exit "refuses a node whose InternalIP lies outside the node network" "$rc"
  assert_contains "names that node and its address" "$output" \
    "kube-system/shoot-info: shoot-worker-b (10.128.44.3)."
  assert_not_contains "and not the node inside" "$output" "shoot-worker-a ("
  assert_contains "and says what the policy would do to it" "$output" \
    "server, so these nodes could not mount a share."

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_NODE_NETWORK=10.128.44.2/31 \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  assert_contains "the comparison is on the network bits, not the text" "$output" \
    "kube-system/shoot-info: shoot-worker-a (10.128.44.1)."

  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-no-address.json")"
  rc=$?
  assert_nonzero_exit "refuses a node without an IPv4 InternalIP" "$rc"
  assert_contains "naming it" "$output" "shoot-worker-b (no IPv4 InternalIP)."

  # An overlay whose nfs/ ships no client-policy.yaml, and a deploy without
  # WITH_NFS=true: no ConfigMap is read.
  mkdir -p "$tmp/no-policy/nfs"
  : >"$tmp/no-policy/nfs/kustomization.yaml"
  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=true \
    EXTERNAL_OVERLAY="$tmp/no-policy" KUBECTL_NODE_NETWORK= \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "an overlay without nfs/client-policy.yaml needs no node network" "0" "$rc"
  assert_not_contains "and reads no ConfigMap" "$(cat "$KUBECTL_LOG")" "$shoot_info_read"

  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" check_external_cluster EXTERNAL_CLUSTER=true WITH_NFS=false \
    KUBECTL_NODE_NETWORK= \
    KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")"
  rc=$?
  assert_eq "without WITH_NFS=true the lab overlay needs no node network" "0" "$rc"
  assert_not_contains "and reads no ConfigMap either" "$(cat "$KUBECTL_LOG")" "$shoot_info_read"

  # Without WITH_NFS=true nothing installs nfs.csi.k8s.io, through which the
  # cinder pods of the overlay's by-hand ControlPlane mount every export as an
  # inline volume: the cluster has to bring that CSIDriver, with the Ephemeral
  # lifecycle mode. The stub renders the lab ControlPlane with both backends on
  # a filer, which preflight lets through.
  if command -v yq >/dev/null 2>&1; then
    local cp_lab="$PROJECT_ROOT/deploy/lab/metal-stack/controlplane/controlplane-lab.yaml"
    local modes_read='kubectl get csidriver nfs.csi.k8s.io --ignore-not-found -o jsonpath={.spec.volumeLifecycleModes}'
    sed 's/nfs-server[.]openstack[.]svc[.]cluster[.]local$/filer.example.com/' "$cp_lab" >"$tmp/filer-render.yaml"
    assert_not_contains "the render puts no backend on the in-cluster server" \
      "$(cat "$tmp/filer-render.yaml")" "server: nfs-server"
    local filer_cluster=(EXTERNAL_CLUSTER=true WITH_CONTROLPLANE=true KUBECTL_KUSTOMIZE="$tmp/filer-render.yaml"
      KUBECTL_STORAGECLASSES="$tmp/sc-default.json" KUBECTL_NODES="$tmp/nodes-ready.json")

    output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}")"
    rc=$?
    assert_nonzero_exit "refuses Cinder on a filer without a CSIDriver nfs.csi.k8s.io" "$rc"
    assert_contains "names the NFS backends" "$output" \
      "controlplane puts the Cinder backends nfs1 nfsbk on NFS,"
    assert_contains "and the driver and mode it needs" "$output" \
      "has no CSIDriver/nfs.csi.k8s.io with the Ephemeral lifecycle mode"
    assert_contains "and how to get one" "$output" "rerun with WITH_NFS=true, which installs it."

    output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}" \
      KUBECTL_CSIDRIVER_MODES='["Persistent"]')"
    rc=$?
    assert_nonzero_exit "refuses a CSIDriver without the Ephemeral mode" "$rc"
    assert_contains "quoting its modes" "$output" "(volumeLifecycleModes: '[\"Persistent\"]')"

    output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}" \
      KUBECTL_CSIDRIVER_MODES='["Persistent","Ephemeral"]')"
    rc=$?
    assert_eq "passes a CSIDriver with the Ephemeral mode" "0" "$rc"
    assert_contains "and logs its modes" "$output" \
      'NFS CSI driver      : nfs.csi.k8s.io ["Persistent","Ephemeral"]'

    output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}" \
      KUBECTL_CSIDRIVER_ERROR="Error from server (Forbidden): csidrivers.storage.k8s.io \"nfs.csi.k8s.io\" is forbidden")"
    rc=$?
    assert_nonzero_exit "refuses when the CSIDriver read fails" "$rc"
    assert_contains "and says it cannot read it" "$output" "ERROR: cannot read CSIDriver/nfs.csi.k8s.io"

    output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}" KUBECTL_KUSTOMIZE_RC=1)"
    rc=$?
    assert_nonzero_exit "refuses a controlplane/ that does not render" "$rc"
    assert_contains "naming the directory" "$output" \
      "cannot render $PROJECT_ROOT/deploy/lab/metal-stack/controlplane"

    # No driver to look for: WITH_NFS=true installs it, WITH_CONTROLPLANE_CR=true
    # applies the bundled CR in place of the overlay's, and a ControlPlane
    # without Cinder mounts nothing.
    yq 'del(.spec.services.cinder)' "$cp_lab" >"$tmp/no-cinder-render.yaml"
    local no_read
    for no_read in WITH_NFS=true WITH_CONTROLPLANE_CR=true KUBECTL_KUSTOMIZE="$tmp/no-cinder-render.yaml"; do
      : >"$KUBECTL_LOG"
      output="$(run_fn "$tmp/bin" check_external_cluster "${filer_cluster[@]}" "$no_read")"
      rc=$?
      assert_eq "${no_read##*/} passes without a CSIDriver" "0" "$rc"
      assert_not_contains "and reads no lifecycle modes" "$(cat "$KUBECTL_LOG")" "$modes_read"
    done
  else
    echo "  SKIP: yq not installed (19 checks skipped)"
    SKIP=$((SKIP + 19))
  fi

  # apply_nfs_client_policy: the template with the network in place of its
  # placeholder, and nothing else changed.
  local template="$PROJECT_ROOT/deploy/lab/metal-stack/nfs/client-policy.yaml"
  : >"$KUBECTL_LOG"
  output="$(run_fn "$tmp/bin" apply_nfs_client_policy EXTERNAL_CLUSTER=true \
    NFS_NODE_NETWORK=10.128.44.0/22 KUBECTL_APPLY_STDIN="$tmp/applied.yaml")"
  rc=$?
  assert_eq "apply_nfs_client_policy succeeds" "0" "$rc"
  assert_contains "it pipes the policy into kubectl apply" "$(cat "$KUBECTL_LOG")" "kubectl apply -f -"
  assert_eq "the applied policy differs from the template in the cidr line alone" \
    "$(printf '%s\n' '<             cidr: NODE_NETWORK' '>             cidr: 10.128.44.0/22')" \
    "$(diff "$template" "$tmp/applied.yaml" | grep '^[<>]')"
  assert_contains "and the log names the template and the network" "$output" \
    "NFS client policy $template applied for 10.128.44.0/22."
  unset KUBECTL_LOG
}

# ---------------------------------------------------------------------------
# Test 6: resolve_api_server_egress
# ---------------------------------------------------------------------------
test_resolve_api_server_egress() {
  echo "Test: resolve_api_server_egress renders the egress rules on the slice's port"

  local tmp output rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  make_kubectl_stub "$tmp/bin"

  printf '%s\n' '{"endpoints":[{"addresses":["240.248.220.69"]}],"ports":[{"name":"https","port":443,"protocol":"TCP"}]}' \
    >"$tmp/gardener.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/gardener.json")"
  rc=$?
  assert_eq "resolves a Gardener-shaped slice" "0" "$rc"
  assert_contains "keeps the address list" "$output" 'IPS=["240.248.220.69"]'
  assert_contains "reads the published port" "$output" "PORT=443"
  assert_contains "renders one /32 rule on that port" "$output" \
    'RULES=[{"to":[{"ipBlock":{"cidr":"240.248.220.69/32"}}],"ports":[{"protocol":"TCP","port":443}]}]'

  printf '%s\n' '{"endpoints":[{"addresses":["fd00::1"]}],"ports":[{"name":"https","port":6443,"protocol":"TCP"}]}' \
    >"$tmp/ipv6.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/ipv6.json")"
  assert_contains "an IPv6 address gets a /128 block" "$output" '"cidr":"fd00::1/128"'

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}]}' >"$tmp/no-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/no-port.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice without a port" "$rc"
  assert_contains "says the slice carries no port" "$output" \
    "EndpointSlice default/kubernetes carries no port"

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[{"name":"https","protocol":"TCP"}]}' \
    >"$tmp/unnumbered-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/unnumbered-port.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice whose only port carries no number" "$rc"
  assert_contains "says that slice carries no port" "$output" \
    "EndpointSlice default/kubernetes carries no port"

  printf '%s\n' '{"endpoints":[{"addresses":["172.18.0.2"]}],"ports":[{"name":"none"},{"name":"https","port":443,"protocol":"TCP"}]}' \
    >"$tmp/second-port.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/second-port.json")"
  rc=$?
  assert_eq "skips a port entry without a number" "0" "$rc"
  assert_contains "and takes the first numbered port" "$output" "PORT=443"

  printf '%s\n' '{"endpoints":[],"ports":[{"name":"https","port":6443,"protocol":"TCP"}]}' >"$tmp/empty.json"
  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE="$tmp/empty.json")"
  rc=$?
  assert_nonzero_exit "aborts on a slice without an address" "$rc"
  assert_contains "says there is no API server address" "$output" \
    "no API server address in EndpointSlice default/kubernetes"

  output="$(run_fn "$tmp/bin" resolve_api_server_egress KUBECTL_SLICE_RC=1)"
  rc=$?
  assert_nonzero_exit "aborts on an unreadable slice" "$rc"
  assert_contains "prints kubectl's error" "$output" "is forbidden"
  assert_contains "with the no-address explanation" "$output" \
    "no API server address in EndpointSlice default/kubernetes"
}

# ---------------------------------------------------------------------------
# Test 7: the gates and lines the mode adds to main()
# ---------------------------------------------------------------------------
test_main_gates() {
  echo "Test: main() gates the kind-only steps and names the port-forward"

  local gate_count
  gate_count="$(grep -cE '"\$\{INFRA_ONLY\}" == "true"' "$DEPLOY_INFRA_SH" || true)"
  assert_eq "deploy-infra.sh still has exactly 1 strict INFRA_ONLY==true gate" "1" "$gate_count"

  # Backslashes are stripped so the check holds for either quoting style.
  local unpause_patch
  unpause_patch="$(grep -A2 'kubectl patch openbaocluster openbao-instance' "$DEPLOY_INFRA_SH" |
    tr -d '\\' | tr '\n' ' ')"
  assert_contains "the un-pause patch carries egressRules" "$unpause_patch" '"egressRules":'
  assert_contains "the un-pause patch carries apiServerEndpointIPs" "$unpause_patch" '"apiServerEndpointIPs":'
  assert_contains "the un-pause patch clears spec.paused" "$unpause_patch" '"paused":false'
  assert_before "resolve_api_server_egress runs before the un-pause patch" \
    "$(grep -nE '^[[:space:]]+resolve_api_server_egress$' "$DEPLOY_INFRA_SH" | head -1 | cut -d: -f1)" \
    "$(line_of 'kubectl patch openbaocluster openbao-instance')"

  assert_contains "the external Step 1 branch runs check_external_cluster" \
    "$(grep -A3 -F 'Step 1/8: External cluster (EXTERNAL_CLUSTER=true)' "$DEPLOY_INFRA_SH")" \
    '    check_external_cluster'
  assert_file_contains_fixed "Step 3 applies the overlay root's base" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${OVERLAY_ROOT}/base"'
  assert_file_not_contains "Step 3 no longer hardcodes the kind base" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${REPO_ROOT}/deploy/kind/base"'
  assert_file_contains_fixed "the WITH_CONTROLPLANE render of Step 5 reads the overlay root" \
    "$DEPLOY_INFRA_SH" 'kubectl kustomize "${OVERLAY_ROOT}/infrastructure"'

  # WITH_NFS=true: the overlay's nfs/ in place of deploy/kind/nfs, no modprobe
  # on this machine, and a rollout gate on the client modules of every node.
  assert_file_contains_fixed "Step 3 applies the overlay root's nfs/" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k "${OVERLAY_ROOT}/nfs"'
  assert_eq "the host NFS module load is the else branch of the EXTERNAL_CLUSTER gate, the skip log its then branch" \
    "$(printf '%s\n' \
      'if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then' \
      'log "Skipping the host-side NFS kernel modules (EXTERNAL_CLUSTER=true; the pods of ${OVERLAY_ROOT}/nfs load them on the nodes)."' \
      'else' \
      'load_nfs_kernel_modules')" \
    "$(grep -B3 -E '^[[:space:]]+load_nfs_kernel_modules$' "$DEPLOY_INFRA_SH" | sed 's/^[[:space:]]*//')"
  assert_before "the client policy is applied before the overlay's nfs/" \
    "$(line_of '      apply_nfs_client_policy')" "$(line_of 'kubectl apply -k "${OVERLAY_ROOT}/nfs"')"
  assert_eq "behind an EXTERNAL_CLUSTER gate and a gate on the template" \
    'if [[ "${EXTERNAL_CLUSTER}" == "true" && -f "${OVERLAY_ROOT}/nfs/client-policy.yaml" ]]; then' \
    "$(grep -B1 -E '^      apply_nfs_client_policy$' "$DEPLOY_INFRA_SH" | head -n1 | sed 's/^[[:space:]]*//')"
  assert_eq "check_external_cluster resolves the node network behind the same template gate" \
    'if [[ -f "${OVERLAY_ROOT}/nfs/client-policy.yaml" ]]; then' \
    "$(grep -B1 -E '^      resolve_nfs_node_network "\$\{nodes_json\}"$' "$DEPLOY_INFRA_SH" | head -n1 | sed 's/^[[:space:]]*//')"
  local ds_wait='kubectl rollout status daemonset/nfs-client-modules -n openstack --timeout="${POD_TIMEOUT}s"'
  assert_before "the nfs-client-modules rollout wait follows the overlay's nfs/ apply" \
    "$(line_of 'kubectl apply -k "${OVERLAY_ROOT}/nfs"')" "$(line_of "$ds_wait")"
  assert_before "and the nfs-server rollout wait" \
    "$(line_of 'kubectl rollout status deployment/nfs-server -n openstack')" "$(line_of "$ds_wait")"
  assert_contains "the nfs-client-modules wait sits behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B1 -F "$ds_wait" "$DEPLOY_INFRA_SH")" 'if [[ "${EXTERNAL_CLUSTER}" == "true" ]]; then'
  local ds_failure
  ds_failure="$(grep -A3 -F "$ds_wait" "$DEPLOY_INFRA_SH")"
  assert_contains "a failed nfs-client-modules rollout names the DaemonSet and the log command" "$ds_failure" \
    "ERROR: DaemonSet openstack/nfs-client-modules did not roll out, so not every node has the nfs and nfsv4 modules. Read 'kubectl logs -n openstack -l app.kubernetes.io/name=nfs-client-modules -c load --prefix --tail=-1'."
  assert_contains "and exits 1" "$ds_failure" "exit 1"

  assert_contains "the Keystone preload sits behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B3 -F 'docker pull "ghcr.io/c5c3/keystone:' "$DEPLOY_INFRA_SH")" \
    'if [[ "${EXTERNAL_CLUSTER}" != "true" ]]; then'
  assert_contains "the nofile cap sits behind an EXTERNAL_CLUSTER gate" \
    "$(grep -B1 -E '^    cap_node_nofile$' "$DEPLOY_INFRA_SH")" \
    'if [[ "${EXTERNAL_CLUSTER}" != "true" ]]; then'
  assert_before "the external Step 1 branch precedes the SKIP_KIND_CREATE branch" \
    "$(line_of 'Step 1/8: External cluster (EXTERNAL_CLUSTER=true)')" \
    "$(line_of '"${SKIP_KIND_CREATE:-false}" == "true"')"

  assert_file_contains_fixed "the CR rewrite reads strenv(PUBLIC_PORT)" \
    "$DEPLOY_INFRA_SH" 'strenv(PUBLIC_PORT)'
  assert_file_not_contains "the CR rewrite no longer reads strenv(KIND_HOST_PORT)" \
    "$DEPLOY_INFRA_SH" 'strenv(KIND_HOST_PORT)'
  assert_file_contains_fixed "the CR rewrite is gated on PUBLIC_PORT" \
    "$DEPLOY_INFRA_SH" 'if [[ "${PUBLIC_PORT}" != "443" ]]; then'
  local hint
  hint="$(grep -B5 -F 'Or re-run with WITH_CONTROLPLANE_CR=true' "$DEPLOY_INFRA_SH")"
  assert_contains "the by-hand CR hint is gated on PUBLIC_PORT" "$hint" \
    'if [[ "${PUBLIC_PORT}" != "443" ]]; then'
  assert_contains "the by-hand CR hint names the URL on the public port" "$hint" \
    'https://keystone.127-0-0-1.nip.io:${PUBLIC_PORT}/v3'
  assert_not_contains "the by-hand CR hint no longer keys on KIND_HOST_PORT" "$hint" "KIND_HOST_PORT"

  assert_file_contains_fixed "the external by-hand hint applies the overlay's controlplane kustomization" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -k ${OVERLAY_ROOT}/controlplane'
  local overlay_gate
  # The gate is the ending's own if/elif, at four spaces; the admission probe
  # nested under it, at six, is not the gate.
  overlay_gate="$(grep -B30 -F 'kubectl apply -k ${OVERLAY_ROOT}/controlplane' "$DEPLOY_INFRA_SH" |
    grep -E '^    (if|elif) \[\[' | tail -n1)"
  assert_contains "the overlay hint is gated on EXTERNAL_CLUSTER" "$overlay_gate" \
    '"${EXTERNAL_CLUSTER}" == "true"'
  assert_contains "the overlay hint is gated on the overlay's controlplane kustomization" \
    "$overlay_gate" '-f "${OVERLAY_ROOT}/controlplane/kustomization.yaml"'
  assert_file_contains_fixed "the overlay hint names the CR by CONTROLPLANE_NAME" \
    "$DEPLOY_INFRA_SH" "(the CR is named '\${CONTROLPLANE_NAME}')"
  assert_file_contains_fixed "the kind by-hand hint still names the bundled CR" \
    "$DEPLOY_INFRA_SH" 'kubectl apply -f deploy/kind/controlplane/controlplane.yaml'

  local banner
  banner="$(awk '/Infrastructure deployment complete!/,/^}/' "$DEPLOY_INFRA_SH")"
  assert_contains "the completion banner names the port-forward" "$banner" "port-forward"
  assert_contains "the completion banner forwards PUBLIC_PORT to 443" "$banner" '${PUBLIC_PORT}:443'
  assert_not_contains "the completion banner repeats no literal 8443" "$banner" "8443"
  assert_contains "the completion banner resolves the Envoy Service by its Gateway label" \
    "$banner" "gateway.envoyproxy.io/owning-gateway-name=openstack-gw"
  assert_contains "the completion banner names the external teardown" \
    "$banner" "EXTERNAL_CLUSTER=true make teardown-infra"
  assert_file_contains_fixed "the run banner reports the cluster mode" \
    "$DEPLOY_INFRA_SH" 'Cluster mode        : external (EXTERNAL_CLUSTER=true; overlay ${OVERLAY_ROOT}; port-forward on ${PUBLIC_PORT})'
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_knobs_and_derived_variables
test_external_preflight
test_refused_flags
test_nfs_overlay_preflight
test_kind_preflight_unchanged
test_check_external_cluster
test_resolve_api_server_egress
test_main_gates

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
