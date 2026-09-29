#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/lab-node-ports.sh, the inter-node TCP port check of the
# metal-stack lab:
#   1. The default port spec expands to 65 ports (16514 and 49152-49215), an
#      invalid, out-of-range or empty spec exits 2, the default image is the
#      node probe's pinned one, and an unset image without the probe manifest
#      exits 2 naming both.
#   2. Against two nodes it creates one unprivileged, host-networked listener
#      per node naming every port and one client per ordered pair on the source
#      node aimed at the destination's first InternalIP (six for three nodes),
#      prints one line per pair and exits 0 when every port is open.
#   3. A closed port, an empty client log and a port the listener could not
#      bind each exit 1 and name the port; one node, an unreadable node list, a
#      node without an InternalIP, one whose first InternalIP is IPv6 (the
#      address live migration dials) or not an address, and listeners that
#      never become Ready exit 2; a dual-stack node listing IPv4 first is
#      checked on that address.
#   4. A listener that never reports `listening` exits 2; a client that never
#      finishes is waited for the Pod timeout plus a connect timeout per port,
#      and its ports have no result.
#   5. An earlier run's Pods are deleted before any Pod is created, so their
#      logs are never read; a failed delete or create exits 2.
#   6. The rendered listener and client programs run locally (perl, timeout
#      and bash /dev/tcp) and print what the result parser reads.
#   7. The last kubectl call is the label-selected Pod delete on every path.
#
# The script runs against a recording kubectl stub first on PATH and the real
# jq, which the script requires.
#
# Usage: bash tests/unit/hack/lab_node_ports_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PORTS_SH="$PROJECT_ROOT/hack/lab-node-ports.sh"
PROBE_MANIFEST="$PROJECT_ROOT/deploy/lab/metal-stack/probe/node-probe.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

CLEANUP_CALL="kubectl delete pod -n default -l app.kubernetes.io/name=lab-node-ports --ignore-not-found --timeout=60s"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# nodes_json <name>:<ip>... — a `kubectl get nodes -o json` document; an empty
# <ip> leaves the node without an InternalIP.
nodes_json() {
  local entry items=""
  for entry in "$@"; do
    items+="${items:+,}{\"metadata\":{\"name\":\"${entry%%:*}\"},\"status\":{\"addresses\":["
    items+="{\"type\":\"Hostname\",\"address\":\"${entry%%:*}\"}"
    if [ -n "${entry#*:}" ]; then
      items+=",{\"type\":\"InternalIP\",\"address\":\"${entry#*:}\"}"
    fi
    items+="]}}"
  done
  printf '{"items":[%s]}\n' "$items"
}

# client_log <state> [<port> <state>]... — a client log with every default
# port in <state>, except the listed overrides.
client_log() {
  local default="$1" p
  shift
  for p in 16514 $(seq 49152 49215); do
    local state="$default" i
    for (( i = 1; i < $#; i += 2 )); do
      if [ "${!i}" = "$p" ]; then
        local next=$((i + 1))
        state="${!next}"
      fi
    done
    echo "$p $state"
  done
}

# make_stub <dir>
# kubectl records its argv in $CALL_LOG and answers:
#   get nodes              → $NODES_FILE; exit 1 under KUBECTL_NODES_RC
#   delete                 → exit 1 under KUBECTL_DELETE_RC; otherwise removes
#                            <dir>/leftover, the Pods of an earlier run
#   create -f -            → stdin saved as <dir>/manifest.<n>; AlreadyExists
#                            while <dir>/leftover exists, exit 1 under
#                            KUBECTL_CREATE_RC
#   wait                   → exit $KUBECTL_WAIT_RC (default 0)
#   get pod … phase        → $KUBECTL_POD_PHASE (default Succeeded)
#   logs <listener>        → the lines of $LISTENER_LOG_FILE, then `listening`
#                            unless LISTENER_SILENT is set
#   logs <client>          → <dir>/leftover while it exists, else
#                            $CLIENT_LOG_FILE
make_stub() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
echo "kubectl $*" >>"$CALL_LOG"
case "$1" in
  get)
    case "$2" in
      nodes)
        if [ -n "${KUBECTL_NODES_RC:-}" ]; then
          echo "error: You must be logged in to the server (Unauthorized)" >&2
          exit 1
        fi
        cat "$NODES_FILE"
        ;;
      pod) echo "${KUBECTL_POD_PHASE:-Succeeded}" ;;
    esac
    ;;
  delete)
    if [ -n "${KUBECTL_DELETE_RC:-}" ]; then
      echo "Unable to connect to the server: dial tcp 10.128.0.1:443: i/o timeout" >&2
      exit 1
    fi
    rm -f "$dir/leftover"
    ;;
  create)
    if [ -f "$dir/leftover" ]; then
      echo 'Error from server (AlreadyExists): pods "lab-node-ports-listen-worker-a" already exists' >&2
      exit 1
    fi
    if [ -n "${KUBECTL_CREATE_RC:-}" ]; then
      echo 'Error from server (Forbidden): pods is forbidden: violates PodSecurity "restricted:latest"' >&2
      exit 1
    fi
    n="$(ls "$dir" | grep -c '^manifest\.')"
    cat >"$dir/manifest.$((n + 1))"
    ;;
  wait)
    if [ "${KUBECTL_WAIT_RC:-0}" != "0" ]; then
      echo "error: timed out waiting for the condition on pods/lab-node-ports-listen-worker-a" >&2
      exit "${KUBECTL_WAIT_RC}"
    fi
    ;;
  logs)
    case "$2" in
      lab-node-ports-listen-*)
        if [ -n "${LISTENER_LOG_FILE:-}" ]; then cat "$LISTENER_LOG_FILE"; fi
        if [ -z "${LISTENER_SILENT:-}" ]; then echo "listening"; fi
        ;;
      lab-node-ports-client-*)
        if [ -f "$dir/leftover" ]; then cat "$dir/leftover"; else cat "$CLIENT_LOG_FILE"; fi
        ;;
    esac
    ;;
esac
exit 0
STUB
  chmod +x "$dir/kubectl"
}

# run_check <dir> [env_var=value...]
# Runs hack/lab-node-ports.sh against the stub in <dir>/bin. Echoes combined
# output; returns the script's exit status.
run_check() {
  local dir="$1"
  shift
  (
    unset NODE_PORTS_TCP NODE_PORTS_NAMESPACE NODE_PORTS_IMAGE NODE_PORTS_NODE_SELECTOR \
      NODE_PORTS_CONNECT_TIMEOUT NODE_PORTS_POD_TIMEOUT LISTENER_LOG_FILE LISTENER_SILENT \
      KUBECTL_NODES_RC KUBECTL_DELETE_RC KUBECTL_CREATE_RC KUBECTL_WAIT_RC KUBECTL_POD_PHASE
    for assignment in "$@"; do
      export "${assignment?}"
    done
    export CALL_LOG="$dir/calls.log" NODES_FILE="$dir/nodes.json" CLIENT_LOG_FILE="$dir/client.log"
    PATH="$dir/bin:$PATH"
    export PATH
    bash "$PORTS_SH"
  ) 2>&1
}

# setup <dir> <client log state> <name>:<ip>...
setup() {
  local dir="$1" state="$2"
  shift 2
  make_stub "$dir/bin"
  : >"$dir/calls.log"
  nodes_json "$@" >"$dir/nodes.json"
  client_log "$state" >"$dir/client.log"
}

# manifests <dir> <role> — the saved manifests of <role>, one JSON per line.
manifests() {
  local f
  for f in "$1"/bin/manifest.*; do
    [ -f "$f" ] || continue
    jq -c --arg role "$2" 'select(.metadata.labels["lab-node-ports/role"] == $role)' "$f"
  done
}

assert_last_call_is_cleanup() {
  assert_eq "$1: the last kubectl call deletes the labelled Pods" \
    "$CLEANUP_CALL" "$(tail -n1 "$2/calls.log")"
}

# ---------------------------------------------------------------------------
# Test 1: the defaults
# ---------------------------------------------------------------------------
test_defaults() {
  echo "Test: default ports and image"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  out="$(bash -c 'source "$1"; expand_ports "16514 49152-49215"' _ "$PORTS_SH")"
  assert_eq "the default spec expands to 65 ports" "65" "$(wc -w <<<"$out" | tr -d ' ')"
  assert_eq "the list starts with 16514 49152 and ends with 49215" "16514 49152" "$(cut -d' ' -f1-2 <<<"$out")"
  assert_eq "the list ends with 49215" "49215" "$(tr ' ' '\n' <<<"$out" | tail -n1)"
  assert_eq "the default spec is 16514 49152-49215" "16514 49152-49215" \
    "$(unset NODE_PORTS_TCP; bash -c 'source "$1"; printf "%s" "$NODE_PORTS_TCP"' _ "$PORTS_SH")"

  out="$(bash -c 'source "$1"; expand_ports "16514 49215-49152"' _ "$PORTS_SH" 2>&1)"
  rc=$?
  assert_eq "a backwards range exits 2" "2" "$rc"
  assert_contains "and names the entry" "$out" "'49215-49152'"
  out="$(bash -c 'source "$1"; expand_ports "libvirt"' _ "$PORTS_SH" 2>&1)"
  rc=$?
  assert_eq "a non-numeric entry exits 2" "2" "$rc"
  out="$(bash -c 'source "$1"; expand_ports "0"' _ "$PORTS_SH" 2>&1)"
  rc=$?
  assert_eq "port 0 exits 2" "2" "$rc"
  assert_contains "port 0 is outside the port range" "$out" "'0' is outside 1-65535"
  out="$(bash -c 'source "$1"; expand_ports "65000-65536"' _ "$PORTS_SH" 2>&1)"
  rc=$?
  assert_eq "a range ending at 65536 exits 2" "2" "$rc"
  out="$(bash -c 'source "$1"; expand_ports ""' _ "$PORTS_SH" 2>&1)"
  rc=$?
  assert_eq "an empty spec exits 2" "2" "$rc"
  assert_contains "says it names no port" "$out" "NODE_PORTS_TCP names no port"

  local pinned
  pinned="$(grep -oE 'image: [^ ]+' "$PROBE_MANIFEST" | head -n1 | cut -d' ' -f2)"
  assert_not_empty "the probe manifest pins an image" "$pinned"
  assert_eq "the default image is the probe's pin" "$pinned" \
    "$(unset NODE_PORTS_IMAGE; bash -c 'source "$1"; resolve_image' _ "$PORTS_SH")"
  assert_eq "NODE_PORTS_IMAGE overrides the pin" "example.org/tools:1" \
    "$(NODE_PORTS_IMAGE=example.org/tools:1 bash -c 'source "$1"; resolve_image' _ "$PORTS_SH")"

  out="$(unset NODE_PORTS_IMAGE; bash -c 'source "$1"; REPO_ROOT="$2"; resolve_image' _ "$PORTS_SH" "$tmp" 2>&1)"
  rc=$?
  assert_eq "an unset image without the probe manifest exits 2" "2" "$rc"
  assert_contains "the error names both" "$out" \
    "NODE_PORTS_IMAGE is unset and deploy/lab/metal-stack/probe/node-probe.yaml is missing"
}

# ---------------------------------------------------------------------------
# Test 2: two nodes, every port open
# ---------------------------------------------------------------------------
test_two_nodes_all_open() {
  echo "Test: two nodes with every port open"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup "$tmp" open worker-a:10.128.44.10 worker-b:10.128.44.11

  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "exits 0 when every port is open" "0" "$rc"
  assert_contains "prints the a -> b line" "$out" \
    "worker-a (10.128.44.10) -> worker-b (10.128.44.11): 65/65 open"
  assert_contains "prints the b -> a line" "$out" \
    "worker-b (10.128.44.11) -> worker-a (10.128.44.10): 65/65 open"

  local listeners clients
  listeners="$(manifests "$tmp" listener)"
  clients="$(manifests "$tmp" client)"
  assert_eq "one listener per node" "2" "$(grep -c . <<<"$listeners")"
  assert_eq "one client per ordered pair" "2" "$(grep -c . <<<"$clients")"
  assert_eq "every Pod is host-networked" "true true true true" \
    "$(printf '%s\n%s\n' "$listeners" "$clients" | jq -r '.spec.hostNetwork' | tr '\n' ' ' | sed 's/ $//')"
  assert_eq "the listeners sit on their nodes" "worker-a worker-b" \
    "$(jq -r '.spec.nodeName' <<<"$listeners" | tr '\n' ' ' | sed 's/ $//')"
  assert_not_contains "no Pod is privileged" "$listeners$clients" "privileged"
  assert_eq "no container keeps a capability" "ALL" \
    "$(jq -r '.spec.containers[0].securityContext.capabilities.drop[]' <<<"$listeners" | sort -u)"
  assert_eq "the listeners run as non-root" "true" \
    "$(jq -r '.spec.securityContext.runAsNonRoot' <<<"$listeners" | sort -u)"
  assert_eq "no Pod mounts a ServiceAccount token" "false" \
    "$(printf '%s\n%s\n' "$listeners" "$clients" | jq -r '.spec.automountServiceAccountToken' | sort -u)"
  assert_eq "the listener command names all 65 ports" "65" \
    "$(jq -r '.spec.containers[0].command[4:] | length' <<<"$listeners" | sort -u)"
  assert_eq "the listener command runs perl" "perl" \
    "$(jq -r '.spec.containers[0].command[0]' <<<"$listeners" | sort -u)"

  local a_to_b
  a_to_b="$(jq -c 'select(.metadata.name == "lab-node-ports-client-worker-a-worker-b")' <<<"$clients")"
  assert_eq "the a -> b client runs on a" "worker-a" "$(jq -r '.spec.nodeName' <<<"$a_to_b")"
  assert_contains "the a -> b client connects to b's InternalIP" \
    "$(jq -r '.spec.containers[0].command[2]' <<<"$a_to_b")" "/dev/tcp/10.128.44.11/"
  assert_last_call_is_cleanup "all open" "$tmp"
}

# ---------------------------------------------------------------------------
# Test 3: three nodes
# ---------------------------------------------------------------------------
test_three_nodes() {
  echo "Test: three nodes make six ordered pairs"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup "$tmp" open worker-a:10.128.44.10 worker-b:10.128.44.11 worker-c:10.128.44.12

  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "exits 0" "0" "$rc"
  assert_eq "three listeners" "3" "$(manifests "$tmp" listener | grep -c .)"
  assert_eq "six clients" "6" "$(manifests "$tmp" client | grep -c .)"
  assert_eq "six result lines" "6" "$(grep -c ': 65/65 open$' <<<"$out")"
}

# ---------------------------------------------------------------------------
# Test 4: a closed port, an empty log and an unbound port fail the run
# ---------------------------------------------------------------------------
test_failing_ports() {
  echo "Test: a closed port, an empty client log and an unbound port exit 1"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup "$tmp" open worker-a:10.128.44.10 worker-b:10.128.44.11

  client_log open 16514 closed >"$tmp/client.log"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "a closed port exits 1" "1" "$rc"
  assert_contains "the line names the closed port" "$out" \
    "worker-a (10.128.44.10) -> worker-b (10.128.44.11): 64/65 open, closed: 16514"
  assert_last_call_is_cleanup "closed port" "$tmp"

  : >"$tmp/calls.log"
  : >"$tmp/client.log"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "an empty client log exits 1" "1" "$rc"
  assert_contains "every port counts as no result" "$out" "0/65 open, no result: 16514 49152"

  : >"$tmp/calls.log"
  client_log open 49160 closed >"$tmp/client.log"
  printf 'bind failed 49160\n' >"$tmp/listener.log"
  out="$(run_check "$tmp" LISTENER_LOG_FILE="$tmp/listener.log")"
  rc=$?
  assert_eq "a port the listener could not bind exits 1" "1" "$rc"
  assert_contains "it is reported as unbound, not closed" "$out" "64/65 open, listener bind failed: 49160"
}

# ---------------------------------------------------------------------------
# Test 5: cluster errors exit 2
# ---------------------------------------------------------------------------
test_cluster_errors() {
  echo "Test: one node, an unreadable node list, a bad InternalIP and unready listeners exit 2"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  setup "$tmp" open worker-a:10.128.44.10
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "one node exits 2" "2" "$rc"
  assert_contains "says it needs two" "$out" "the check needs at least two nodes, found 1"
  assert_eq "no Pod is created" "" "$(manifests "$tmp" listener)"
  assert_last_call_is_cleanup "one node" "$tmp"

  : >"$tmp/calls.log"
  nodes_json worker-a:10.128.44.10 worker-b: >"$tmp/nodes.json"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "a node without an InternalIP exits 2" "2" "$rc"
  assert_contains "names the node" "$out" "node worker-b reports no InternalIP"
  assert_last_call_is_cleanup "no InternalIP" "$tmp"

  : >"$tmp/calls.log"
  out="$(run_check "$tmp" KUBECTL_NODES_RC=1)"
  rc=$?
  assert_eq "an unreadable node list exits 2" "2" "$rc"
  assert_contains "says the nodes cannot be listed" "$out" "cannot list the nodes"

  # A node writes its own status, and the address is spliced into the client's
  # shell command.
  : >"$tmp/calls.log"
  jq -n '{items: [
    {metadata: {name: "worker-a"}, status: {addresses: [{type: "InternalIP", address: "10.128.44.10"}]}},
    {metadata: {name: "worker-b"}, status: {addresses: [{type: "InternalIP", address: "10.128.44.11\"; touch /tmp/owned; \""}]}}
  ]}' >"$tmp/nodes.json"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "an InternalIP that is not an address exits 2" "2" "$rc"
  assert_contains "names the node" "$out" "node worker-b reports an InternalIP that is not an address"
  assert_eq "no Pod is created for it" "" "$(manifests "$tmp" client)"
  assert_last_call_is_cleanup "not an address" "$tmp"

  # The listener binds 0.0.0.0, so an IPv6 address would report every port
  # closed.
  : >"$tmp/calls.log"
  nodes_json worker-a:fd00::a worker-b:fd00::b >"$tmp/nodes.json"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "IPv6-only nodes exit 2" "2" "$rc"
  assert_contains "names the node and the listener's family" "$out" \
    "node worker-a's first InternalIP is IPv6; live migration targets it (status.hostIP) and the listener binds IPv4 only"
  assert_eq "no Pod is created for them" "" "$(manifests "$tmp" listener)"
  assert_last_call_is_cleanup "IPv6 only" "$tmp"

  # Live migration dials status.hostIP, the node's first InternalIP of either
  # family: an IPv4 address behind it would pass on a path migration never
  # takes.
  : >"$tmp/calls.log"
  jq -n '{items: [
    {metadata: {name: "worker-a"}, status: {addresses: [{type: "InternalIP", address: "fd00::a"}, {type: "InternalIP", address: "10.128.44.10"}]}},
    {metadata: {name: "worker-b"}, status: {addresses: [{type: "InternalIP", address: "fd00::b"}, {type: "InternalIP", address: "10.128.44.11"}]}}
  ]}' >"$tmp/nodes.json"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "dual-stack nodes listing IPv6 first exit 2" "2" "$rc"
  assert_contains "names the node and the address migration dials" "$out" \
    "node worker-a's first InternalIP is IPv6; live migration targets it (status.hostIP)"
  assert_eq "no Pod is created for them" "" "$(manifests "$tmp" listener)"
  assert_last_call_is_cleanup "dual-stack IPv6 first" "$tmp"

  : >"$tmp/calls.log"
  jq -n '{items: [
    {metadata: {name: "worker-a"}, status: {addresses: [{type: "InternalIP", address: "10.128.44.10"}, {type: "InternalIP", address: "fd00::a"}]}},
    {metadata: {name: "worker-b"}, status: {addresses: [{type: "InternalIP", address: "10.128.44.11"}, {type: "InternalIP", address: "fd00::b"}]}}
  ]}' >"$tmp/nodes.json"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "dual-stack nodes listing IPv4 first exit 0" "0" "$rc"
  assert_contains "and are checked on their IPv4 InternalIPs" "$out" \
    "worker-a (10.128.44.10) -> worker-b (10.128.44.11): 65/65 open"
  assert_contains "the client connects to the IPv4 address" \
    "$(manifests "$tmp" client | jq -r 'select(.metadata.name == "lab-node-ports-client-worker-a-worker-b") | .spec.containers[0].command[2]')" \
    "/dev/tcp/10.128.44.11/"

  : >"$tmp/calls.log"
  rm -f "$tmp"/bin/manifest.*
  nodes_json worker-a:10.128.44.10 worker-b:10.128.44.11 >"$tmp/nodes.json"
  out="$(run_check "$tmp" KUBECTL_WAIT_RC=1)"
  rc=$?
  assert_eq "listeners that never become Ready exit 2" "2" "$rc"
  assert_contains "quotes the wait's error" "$out" "timed out waiting for the condition"
  assert_last_call_is_cleanup "unready listeners" "$tmp"

  : >"$tmp/calls.log"
  out="$(run_check "$tmp" NODE_PORTS_NODE_SELECTOR=lab/role=hypervisor)"
  assert_contains "NODE_PORTS_NODE_SELECTOR is passed to the node list" \
    "$(cat "$tmp/calls.log")" "kubectl get nodes -l lab/role=hypervisor -o json"

  out="$(run_check "$tmp" NODE_PORTS_CONNECT_TIMEOUT=0)"
  rc=$?
  assert_eq "a zero connect timeout exits 2" "2" "$rc"
}

# ---------------------------------------------------------------------------
# Test 6: a silent listener and a client that never finishes
# ---------------------------------------------------------------------------
test_timeouts() {
  echo "Test: a silent listener exits 2, a client that never finishes has no result"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup "$tmp" open worker-a:10.128.44.10 worker-b:10.128.44.11

  out="$(run_check "$tmp" LISTENER_SILENT=1 NODE_PORTS_POD_TIMEOUT=1)"
  rc=$?
  assert_eq "a listener that never reports 'listening' exits 2" "2" "$rc"
  assert_contains "names the listener" "$out" \
    "listener lab-node-ports-listen-worker-a never reported 'listening'"
  assert_eq "no client is created" "" "$(manifests "$tmp" client)"
  assert_last_call_is_cleanup "silent listener" "$tmp"

  # Two ports at one second each on top of the one-second Pod timeout: a client
  # that drops every packet needs the whole scan before it can finish.
  : >"$tmp/calls.log"
  : >"$tmp/client.log"
  out="$(run_check "$tmp" KUBECTL_POD_PHASE=Pending NODE_PORTS_TCP="16514 16515" \
    NODE_PORTS_POD_TIMEOUT=1 NODE_PORTS_CONNECT_TIMEOUT=1)"
  rc=$?
  assert_eq "a client that never finishes exits 1" "1" "$rc"
  assert_contains "the client wait is the Pod timeout plus a connect timeout per port" "$out" \
    "WARNING: Pod lab-node-ports-client-worker-a-worker-b did not finish within 3s (phase 'Pending')."
  assert_contains "its ports have no result" "$out" "0/2 open, no result: 16514 16515"
  assert_last_call_is_cleanup "unfinished client" "$tmp"
}

# ---------------------------------------------------------------------------
# Test 7: the Pods of an earlier run
# ---------------------------------------------------------------------------
test_earlier_run() {
  echo "Test: an earlier run's Pods are deleted first and never read"

  local tmp out rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  setup "$tmp" open worker-a:10.128.44.10 worker-b:10.128.44.11

  # The earlier run found every port open; the network has changed since.
  client_log open >"$tmp/bin/leftover"
  client_log open 16514 closed >"$tmp/client.log"
  out="$(run_check "$tmp")"
  rc=$?
  assert_eq "this run's closed port fails the run" "1" "$rc"
  assert_contains "the line reports this run's result" "$out" \
    "worker-a (10.128.44.10) -> worker-b (10.128.44.11): 64/65 open, closed: 16514"
  assert_eq "the first kubectl delete waits for the earlier Pods, before any create" \
    "kubectl delete pod -n default -l app.kubernetes.io/name=lab-node-ports --ignore-not-found --wait --timeout=120s" \
    "$(grep -E '^kubectl (delete|create) ' "$tmp/calls.log" | head -n1)"
  assert_last_call_is_cleanup "earlier run" "$tmp"

  : >"$tmp/calls.log"
  rm -f "$tmp"/bin/manifest.*
  out="$(run_check "$tmp" KUBECTL_DELETE_RC=1)"
  rc=$?
  assert_eq "an earlier run's Pods that cannot be deleted exit 2" "2" "$rc"
  assert_contains "says so" "$out" "cannot remove the Pods of an earlier run"
  assert_eq "no Pod is created" "" "$(manifests "$tmp" listener)"

  : >"$tmp/calls.log"
  out="$(run_check "$tmp" KUBECTL_CREATE_RC=1)"
  rc=$?
  assert_eq "Pods that cannot be created exit 2" "2" "$rc"
  assert_contains "names the listener create" "$out" "cannot create the listener Pods"
  assert_last_call_is_cleanup "create refused" "$tmp"
}

# ---------------------------------------------------------------------------
# Test 8: the rendered programs run
# ---------------------------------------------------------------------------

# command_of <dir> <role> — the container command of the first <role> manifest,
# one argument per NUL-terminated record.
command_of() {
  manifests "$1" "$2" | head -n1 | jq -j '.spec.containers[0].command[] | . + "\u0000"'
}

# wait_for_line <file> <line> — up to ten seconds for <line> to appear in <file>.
wait_for_line() {
  local i
  for (( i = 0; i < 100; i++ )); do
    if grep -qx -- "$2" "$1" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

test_programs_run() {
  echo "Test: the rendered listener and client run and print what the parser reads"

  if ! command -v perl >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
    echo "  SKIP: perl or timeout not installed (9 checks skipped)"
    SKIP=$((SKIP + 9))
    return
  fi

  local tmp out rc p1 p2 holder="" listener="" arg lcmd=() ccmd=()
  tmp="$(mktemp -d)"
  trap 'kill ${holder:-} ${listener:-} 2>/dev/null; rm -rf "$tmp"' RETURN

  # Two ports free right now; p2 is then held by another socket, so the
  # listener's bind of it fails the way a port an outgoing connection holds does.
  read -r p1 p2 < <(perl -MIO::Socket::INET -e '
    my @s = map { IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => 0, Listen => 1) or die "bind: $!" } 1 .. 2;
    print join(" ", map { $_->sockport } @s), "\n"')
  perl -MIO::Socket::INET -e '
    my $s = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => $ARGV[0], Proto => "tcp", Listen => 1)
      or die "hold: $!";
    $| = 1; print "held\n"; sleep 60' "$p2" >"$tmp/holder.log" 2>&1 &
  holder=$!
  wait_for_line "$tmp/holder.log" held

  setup "$tmp" open worker-a:127.0.0.1 worker-b:127.0.0.1
  run_check "$tmp" NODE_PORTS_TCP="$p1 $p2" >/dev/null
  while IFS= read -r -d '' arg; do lcmd+=("$arg"); done < <(command_of "$tmp" listener)
  while IFS= read -r -d '' arg; do ccmd+=("$arg"); done < <(command_of "$tmp" client)

  "${lcmd[@]}" >"$tmp/listener.log" 2>&1 &
  listener=$!
  wait_for_line "$tmp/listener.log" listening
  assert_eq "the listener prints 'listening' once every bind was tried" "listening" \
    "$(grep -x listening "$tmp/listener.log")"
  assert_eq "the listener names the port it could not bind" "bind failed $p2" \
    "$(grep -x "bind failed $p2" "$tmp/listener.log")"
  assert_eq "the listener binds the free port" "" "$(grep -x "bind failed $p1" "$tmp/listener.log")"

  "${ccmd[@]}" >"$tmp/client.log" 2>&1
  assert_eq "the client prints '<port> open' for a listening port" "$p1 open" \
    "$(grep -x "$p1 open" "$tmp/client.log")"
  out="$(run_check "$tmp" NODE_PORTS_TCP="$p1 $p2" LISTENER_LOG_FILE="$tmp/listener.log")"
  rc=$?
  assert_eq "the parser reads the programs' output as all open" "0" "$rc"
  assert_contains "and reports both ports" "$out" "worker-a (127.0.0.1) -> worker-b (127.0.0.1): 2/2 open"

  kill "$listener" "$holder" 2>/dev/null
  wait "$listener" "$holder" 2>/dev/null
  listener=""
  holder=""
  "${ccmd[@]}" >"$tmp/client.log" 2>&1
  assert_eq "the client prints '<port> closed' once nothing listens" "$p1 closed" \
    "$(grep -x "$p1 closed" "$tmp/client.log")"
  out="$(run_check "$tmp" NODE_PORTS_TCP="$p1 $p2" LISTENER_LOG_FILE="$tmp/listener.log")"
  rc=$?
  assert_eq "the parser fails the run on the closed ports" "1" "$rc"
  assert_contains "and tells the closed port from the unbound one" "$out" \
    "0/2 open, closed: $p1, listener bind failed: $p2"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (hack/lab-node-ports.sh requires it)"
  echo ""
  echo "Results: $PASS passed, $FAIL failed, 1 skipped"
  exit 0
fi

test_defaults
test_two_nodes_all_open
test_three_nodes
test_failing_ports
test_cluster_errors
test_timeouts
test_earlier_run
test_programs_run

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
