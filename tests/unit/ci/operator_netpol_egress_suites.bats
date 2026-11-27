#!/usr/bin/env bats
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the operator network-policy-egress suites under
# tests/e2e/<op>-operator/network-policy-egress, which install a second release
# of an operator chart with its NetworkPolicy on and probe the rules from a pod
# the policy selects (#1554):
#   - the suites on disk are exactly the nine of keystone, horizon, glance,
#     placement, barbican, neutron, cinder, nova and ovn, and every operator
#     chart that renders a NetworkPolicy has one; c5c3's chart renders none
#   - every suite uninstalls its release in the step-level cleanup of Step 1,
#     which chainsaw runs also after a failed step, and no later step does
#   - the values file of each of the seven stand-in suites (all but keystone
#     and horizon) holds exactly the keys of its install, with no
#     networkPolicy.kubeApiServer and no barbican networkPolicy.openBao
#   - each of the seven sets its name, namespace and spec.timeouts, so a local
#     run without the shared chainsaw config gets the bounds CI has, and its
#     Step 3 requires zero restarts and greps the operator's discovery line
#   - the fixture of each of the seven holds the Namespace first, then the
#     probe, control, sink and stand-in pods of the table below with their
#     exact labels, the one image, the restricted pod spec of
#     tests/e2e/keystone/network-policy/03-probe-pods.yaml, and either sleep or
#     that file's listener program byte for byte on the table's ports;
#     horizon's control pod carries the same spec
#   - the probe step of each of the seven, run against a kubectl stub and a
#     stub probe, calls the probe in the order the proof rests on: the
#     control's open calls first, then the probe pod's dropped on sink port
#     9999, then DNS, the API server endpoint (its first IPv4 address) and the
#     Service VIP, then each stand-in, then the sink's peer-scoped ports; an
#     empty address, a NetworkPolicy in the targets namespace or a fixture pod
#     that never turns Ready stops it before the first probe call, and a
#     failing probe call ends it
#   - horizon's control pod reaches openstack-db in a step before the drop,
#     its DNS and API server probes run in a step after it, and its last step
#     checks the operator's restarts again; that probe step, run against the
#     stubs, resolves the cluster name, then opens the API server endpoint
#     and the Service VIP, and an empty address stops it before the first
#     probe call
#   - the e2e-operator run step reaches tests/e2e/<op>-operator/, and nova's
#     shard 2 list leaves the suite to shard 1
#
# The structure is read with mikefarah yq; without it those tests are
# skipped. The probe-step replay also needs jq.
#
# Usage: bin/bats tests/unit/ci/operator_netpol_egress_suites.bats

load '../../lib/bats/common'
load '../../lib/bats/stubs'

# shellcheck disable=SC2034 # read by tests/lib/ci_yaml.sh
CI_YAML="$PROJECT_ROOT/.github/workflows/ci.yaml"

# shellcheck source=tests/lib/ci_yaml.sh
source "$PROJECT_ROOT/tests/lib/ci_yaml.sh"

# The nine suites, sorted.
ALL_SUITES="barbican cinder glance horizon keystone neutron nova ovn placement"

# The seven stand-in suites, one per line:
#   op|image|stand-ins|sink ports dropped|sink ports open|discovery line
# A stand-in is <svc>:<port>[,<port>...], the labels and ports of one chart
# rule; it listens on those ports and on 9999. The sink listens on its dropped
# and open ports, ascending, and on 9999.
GW="Gateway API detected; enabling HTTPRoute watch and reconciliation"
SUITES="glance|glance|glance:9292|9292||$GW
placement|placement|placement:8778|8778||$GW
barbican|barbican|barbican:9311|9311|8200|$GW
neutron|neutron|neutron:9696|9696||$GW
cinder|cinder|cinder:8776|8776||$GW
nova|nova|nova:8774 keystone:5000,5050 placement:8778|5000 5050 8774 8778||$GW
ovn|keystone||||cert-manager detected; enabling Certificate watch and reconciliation"

# The restricted pod spec every probe pod carries, as yq path=value.
RESTRICTED='.spec.automountServiceAccountToken=false
.spec.restartPolicy=Never
.spec.terminationGracePeriodSeconds=0
.spec.securityContext.runAsNonRoot=true
.spec.securityContext.runAsUser=42424
.spec.securityContext.runAsGroup=42424
.spec.securityContext.seccompProfile.type=RuntimeDefault
.spec.containers[0].imagePullPolicy=IfNotPresent
.spec.containers[0].securityContext.allowPrivilegeEscalation=false
.spec.containers[0].securityContext.readOnlyRootFilesystem=true
.spec.containers[0].securityContext.capabilities.drop[0]=ALL
.spec.containers[0].resources.requests.cpu=10m
.spec.containers[0].resources.requests.memory=64Mi
.spec.containers[0].resources.limits.memory=128Mi'

setup() {
  STUBS="$BATS_TEST_TMPDIR"
  mkdir -p "$STUBS/lib" "$STUBS/suites/run"
  default_rules
}

suite_dir() {
  echo "$PROJECT_ROOT/tests/e2e/$1-operator/network-policy-egress"
}

# pod_field <file> <pod> <yq expression>: the expression on the named Pod of
# the multi-document file.
pod_field() {
  POD="$2" yq ea '[select(.kind == "Pod" and .metadata.name == strenv(POD))] | .[0] | '"$3" "$1"
}

# --- Test 1: the suites on disk ---
@test "the nine suites exist, one per chart that renders a NetworkPolicy" {

  local found dir op
  found="$(for dir in "$PROJECT_ROOT"/tests/e2e/*-operator/network-policy-egress; do
    dir="${dir%-operator/network-policy-egress}"
    echo "${dir##*/}"
  done | LC_ALL=C sort | paste -s -d ' ' -)"
  assert_eq "the network-policy-egress suites on disk are the nine" "$ALL_SUITES" "$found"

  for dir in "$PROJECT_ROOT"/operators/*/helm/*-operator; do
    op="${dir%/helm/*}"
    op="${op##*/}"
    if [ ! -e "$dir/templates/networkpolicy.yaml" ]; then
      continue
    fi
    if [ -f "$(suite_dir "$op")/chainsaw-test.yaml" ]; then
      check_passed "the $op-operator NetworkPolicy has a suite"
    else
      soft fail "$op-operator renders a NetworkPolicy: add tests/e2e/$op-operator/network-policy-egress"
    fi
  done
  assert_eq "the c5c3-operator chart renders no NetworkPolicy" "absent" \
    "$([ -e "$PROJECT_ROOT/operators/c5c3/helm/c5c3-operator/templates/networkpolicy.yaml" ] \
      && echo present || echo absent)"
}

# --- Test 2: the release is uninstalled in the step cleanup ---
@test "every suite uninstalls its release in the cleanup of Step 1 and nowhere else" {
  # The cleanup of the nine suites are read with mikefarah yq.
  require_mikefarah_yq

  local op suite
  for op in $ALL_SUITES; do
    suite="$(suite_dir "$op")/chainsaw-test.yaml"
    soft assert_contains "the $op install is the first operation of Step 1" \
      "$(yq '.spec.steps[0].try[0].script.content' "$suite")" "helm install $op-operator-netpol"
    soft assert_contains "the $op Step 1 cleanup uninstalls the release" \
      "$(yq '[.spec.steps[0].cleanup[]?.script.content] | join("\n")' "$suite")" \
      "helm uninstall $op-operator-netpol -n $op-system"
    soft assert_eq "no later $op step runs helm uninstall" "0" \
      "$(yq '[.spec.steps[1:][] | .. | select(tag == "!!str") | select(test("helm uninstall"))] | length' "$suite")"
  done
}

# --- Test 3: the values files, names and timeouts ---
@test "each stand-in suite installs exactly its values and sets its own timeouts" {
  # The values files and timeouts are read with mikefarah yq.
  require_mikefarah_yq

  local op suite values
  while IFS='|' read -r op _; do
    suite="$(suite_dir "$op")/chainsaw-test.yaml"
    values="$(suite_dir "$op")/00-install-operator.yaml"
    soft assert_eq "the $op values file holds exactly the install keys" \
      "{\"image\":{\"repository\":\"ghcr.io/c5c3/$op-operator\",\"tag\":\"dev\",\"pullPolicy\":\"Never\"},\"replicas\":1,\"controller\":{\"defaultImagePullPolicy\":\"IfNotPresent\"},\"webhook\":{\"enabled\":false},\"networkPolicy\":{\"enabled\":true}}" \
      "$(yq -o json -I0 '.' "$values")"
    soft assert_eq "the $op suite is named $op-operator-network-policy-egress" \
      "$op-operator-network-policy-egress" "$(yq '.metadata.name' "$suite")"
    soft assert_eq "the $op suite runs in $op-system" "$op-system" "$(yq '.spec.namespace' "$suite")"
    soft assert_eq "the $op suite sets its timeouts" \
      '{"apply":"30s","assert":"2m","cleanup":"3m","delete":"1m","error":"30s","exec":"2m"}' \
      "$(yq -o json -I0 '.spec.timeouts' "$suite")"
  done <<<"$SUITES"
}

# --- Test 4: Step 3 ---
@test "each stand-in suite requires no restart and the discovery line in Step 3" {
  # Step 3 of the stand-in suites are read with mikefarah yq.
  require_mikefarah_yq

  local op discovery step3
  while IFS='|' read -r op _ _ _ _ discovery; do
    step3="$(yq '.spec.steps[2].try[0].script.content' "$(suite_dir "$op")/chainsaw-test.yaml")"
    soft assert_contains "the $op Step 3 reads the restart counts" "$step3" \
      "{.status.containerStatuses[*].restartCount}"
    soft assert_contains "the $op Step 3 fails on a restart" "$step3" \
      'ERROR: ${pod} restarted ${n} times under the policy'
    soft assert_contains "the $op Step 3 greps the discovery line" "$step3" "grep -qF '$discovery'"
  done <<<"$SUITES"
}

# check_pod <file> <pod> <namespace> <labels> <image> <command>: the pod's
# namespace, exact labels (sorted key=value, comma-joined), image, restricted
# spec and command. <command> is "sleep" or the ports of the listener.
check_pod() {
  local fx="$1" pod="$2" ns="$3" labels="$4" image="$5" command="$6" path value actual got
  assert_eq "$pod runs in $ns" "$ns" "$(pod_field "$fx" "$pod" '.metadata.namespace')"
  assert_eq "$pod carries exactly its labels" "$labels" \
    "$(pod_field "$fx" "$pod" '.metadata.labels | to_entries | map(.key + "=" + .value) | sort | join(",")')"
  assert_eq "$pod runs one container of $image" "1 $image" \
    "$(pod_field "$fx" "$pod" '(.spec.containers | length) + " " + .spec.containers[0].image')"
  # One yq call prints the values of the RESTRICTED paths in table order.
  got="$(pod_field "$fx" "$pod" "[$(cut -d= -f1 <<<"$RESTRICTED" | paste -s -d, -)] | .[]")"
  while IFS='=' read -r path value; do
    IFS= read -r actual <&3
    soft assert_eq "$pod sets $path" "$value" "$actual"
  done <<<"$RESTRICTED" 3<<<"$got"
  if [ "$command" = sleep ]; then
    assert_eq "$pod sleeps" "sleep infinity" \
      "$(pod_field "$fx" "$pod" '.spec.containers[0].command | join(" ")')"
    return
  fi
  assert_eq "$pod runs python3 -c" "python3 -c" \
    "$(pod_field "$fx" "$pod" '.spec.containers[0].command | .[0:2] | join(" ")')"
  assert_eq "$pod runs the reference listener byte for byte" "$LISTENER" \
    "$(pod_field "$fx" "$pod" '.spec.containers[0].command[2]')"
  assert_eq "$pod listens on $command" "$command" \
    "$(pod_field "$fx" "$pod" '.spec.containers[0].command | .[3:] | join(" ")')"
}

# --- Test 5: the fixtures ---
@test "each fixture holds the probe, control, sink and stand-in pods of the table" {
  # The fixtures are read with mikefarah yq.
  require_mikefarah_yq

  local op img standins drop open fx rel targets image sink standin svc ports pods
  LISTENER="$(pod_field "$PROJECT_ROOT/tests/e2e/keystone/network-policy/03-probe-pods.yaml" \
    keystone-netpol-probe-target '.spec.containers[0].command[2]')"
  assert_contains "the reference listener binds each port it is given" "$LISTENER" \
    'srv.bind(("0.0.0.0", int(port)))'

  while IFS='|' read -r op img standins drop open _; do
    fx="$(suite_dir "$op")/01-probe-pods.yaml"
    rel="$op-operator-netpol"
    targets="$rel-targets"
    image="ghcr.io/c5c3/$img:2026.1"
    soft assert_eq "the $op fixture starts with the Namespace $targets" "Namespace/$targets" \
      "$(yq 'select(document_index == 0) | .kind + "/" + .metadata.name' "$fx")"
    soft assert_eq "the $op fixture creates no other Namespace" "$targets" \
      "$(yq ea '[select(.kind == "Namespace")] | map(.metadata.name) | join(" ")' "$fx")"
    pods="$op-system/$rel-probe $op-system/$rel-control $targets/$rel-sink"
    for standin in $standins; do
      pods="$pods $targets/$rel-${standin%%:*}-api"
    done
    soft assert_eq "the $op fixture holds the pods of the table in order" "$pods" \
      "$(yq ea '[select(.kind == "Pod") | .metadata.namespace + "/" + .metadata.name] | join(" ")' "$fx")"
    soft assert_eq "the $op fixture holds nothing but the Namespace and the pods" "" \
      "$(yq ea '[select(.kind != "Namespace" and .kind != "Pod")] | map(.kind) | join(" ")' "$fx")"

    soft check_pod "$fx" "$rel-probe" "$op-system" \
      "app.kubernetes.io/instance=$rel,app.kubernetes.io/name=$op-operator" "$image" sleep
    soft check_pod "$fx" "$rel-control" "$op-system" \
      "app.kubernetes.io/instance=$rel,app.kubernetes.io/name=netpol-probe-client" "$image" sleep
    sink="$(tr ' ' '\n' <<<"$drop $open" | sed '/^$/d' | sort -n | paste -s -d ' ' -)"
    soft check_pod "$fx" "$rel-sink" "$targets" \
      "app.kubernetes.io/instance=$rel,app.kubernetes.io/name=netpol-probe-sink" "$image" \
      "${sink:+$sink }9999"
    for standin in $standins; do
      svc="${standin%%:*}"
      ports="$(tr ',' ' ' <<<"${standin#*:}")"
      soft check_pod "$fx" "$rel-$svc-api" "$targets" \
        "app.kubernetes.io/managed-by=$svc-operator,app.kubernetes.io/name=$svc" "$image" "$ports 9999"
    done
  done <<<"$SUITES"

  check_pod "$PROJECT_ROOT/tests/e2e/horizon-operator/network-policy-egress/03-control-pod.yaml" \
    horizon-operator-netpol-control horizon-system \
    "app.kubernetes.io/instance=horizon-operator-netpol,app.kubernetes.io/name=netpol-probe-client" \
    "ghcr.io/c5c3/horizon:2026.1" sleep
}

# The kubectl stub of the probe-step replay answers the reads of the step by
# the rules of default_rules and exits 99 on anything else:
#   wait ...                         exits 0
#   get networkpolicy -n <ns> ...    prints nothing
#   get pod <name> ... {.status.podIP}
#                                    prints the IP of <name> from pod-ips
#   get endpointslice kubernetes ... prints an IPv6 address followed by
#                                    172.18.0.2, port 6443
#   get service kubernetes ...       prints 10.96.0.1 or port 443
# A test changes an answer with a rule it declares before run_probe_step,
# which declares the defaults again after the run. The stub probe appends its
# arguments to probe.log and exits 1 at the call whose number
# STUB_PROBE_FAIL_AT names.
default_rules() {
  # shellcheck disable=SC2016 # $* expands in the stub
  stub_on kubectl '*' --exec 'echo "unexpected kubectl call: $*" >&2; exit 99'
  stub_on kubectl 'wait *'
  stub_on kubectl 'get networkpolicy -n * -o name'
  stub_on kubectl 'get pod * -o jsonpath={.status.podIP}' \
    --exec "awk -v pod=\"\$3\" '\$1 == pod { printf \"%s\", \$2 }' $(printf %q "$STUBS/pod-ips")"
  stub_on kubectl 'get endpointslice kubernetes -n default -o json' \
    --out '{"endpoints":[{"addresses":["fd00::2","172.18.0.2"]}],"ports":[{"port":6443}]}'
  stub_on kubectl 'get service kubernetes -n default -o jsonpath={.spec.clusterIP}' --out-raw 10.96.0.1
  stub_on kubectl 'get service kubernetes -n default -o jsonpath={.spec.ports\[0\].port}' --out-raw 443
  stub_script assert-networkpolicy-enforced.sh <<'STUB'
stubs="$(cd "$(dirname "$0")/.." && pwd)"
printf '%s\n' "$*" >>"$stubs/probe.log"
if [ "${STUB_PROBE_FAIL_AT:-0}" -eq "$(wc -l <"$stubs/probe.log")" ]; then
  echo "FAIL: stub probe call ${STUB_PROBE_FAIL_AT}"
  exit 1
fi
STUB
  # The step runs the probe as ../../lib/assert-networkpolicy-enforced.sh.
  ln -sf "$STUB_DIR/bin/assert-networkpolicy-enforced.sh" "$STUBS/lib/assert-networkpolicy-enforced.sh"
}

# run_probe_step [VAR=value...]: run $STUBS/step.sh with sh from a directory
# two levels below $STUBS, as chainsaw runs it from the suite directory, with
# the stubs first on PATH and only the named stub settings set. Leaves the
# output in $output and the exit code in $status, and declares the default
# kubectl rules again.
run_probe_step() {
  rm -f "$STUBS/probe.log"
  run probe_step "$@"
  rm -rf "$STUB_DIR"
  default_rules
}

# probe_step [VAR=value...]: the run of run_probe_step.
probe_step() {
  cd "$STUBS/suites/run" && env -u STUB_PROBE_FAIL_AT PATH="$STUB_DIR/bin:$PATH" "$@" sh "$STUBS/step.sh"
}

# --- Test 6: the probe step ---
@test "each stand-in suite's probe step calls the probe in the order the proof rests on" {
  require_mikefarah_yq
  require_tool jq

  local op standins drop open ns rel targets sink n standin svc ports var expected
  while IFS='|' read -r op _ standins drop open _; do
    ns="$op-system"
    rel="$op-operator-netpol"
    targets="$rel-targets"
    yq '[.spec.steps[].try[] | select(has("script")) | .script.content
      | select(test("PROBE=../../lib/assert-networkpolicy-enforced.sh"))] | .[0]' \
      "$(suite_dir "$op")/chainsaw-test.yaml" >"$STUBS/step.sh"
    soft assert_contains "the $op probe step waits for its fixture pods" "$(cat "$STUBS/step.sh")" \
      "kubectl wait --for=condition=Ready --timeout=120s"

    # Every pod gets an IP of its own; the stand-ins follow the sink.
    printf '%s 10.244.0.10\n' "$rel-sink" >"$STUBS/pod-ips"
    n=10
    for standin in $standins; do
      n=$((n + 1))
      printf '%s 10.244.0.%s\n' "$rel-${standin%%:*}-api" "$n" >>"$STUBS/pod-ips"
    done
    sink="$(tr ' ' '\n' <<<"$drop $open" | sed '/^$/d' | sort -n | paste -s -d ' ' -)"

    expected="$ns $rel-control open 10.244.0.10 ${sink:+$sink }9999"
    n=10
    for standin in $standins; do
      n=$((n + 1))
      expected="$expected"$'\n'"$ns $rel-control open 10.244.0.$n $(tr ',' ' ' <<<"${standin#*:}") 9999"
    done
    expected="$expected"$'\n'"$ns $rel-probe dropped 10.244.0.10 9999"
    expected="$expected"$'\n'"$ns $rel-probe resolves kubernetes.default.svc.cluster.local."
    expected="$expected"$'\n'"$ns $rel-probe open 172.18.0.2 6443"
    expected="$expected"$'\n'"$ns $rel-probe open 10.96.0.1 443"
    n=10
    for standin in $standins; do
      n=$((n + 1))
      ports="$(tr ',' ' ' <<<"${standin#*:}")"
      expected="$expected"$'\n'"$ns $rel-probe open 10.244.0.$n $ports"
      expected="$expected"$'\n'"$ns $rel-probe dropped 10.244.0.$n 9999"
    done
    if [ -n "$drop" ]; then
      expected="$expected"$'\n'"$ns $rel-probe dropped 10.244.0.10 $drop"
    fi
    if [ -n "$open" ]; then
      expected="$expected"$'\n'"$ns $rel-probe open 10.244.0.10 $open"
    fi

    run_probe_step
    soft assert_eq "the $op probe step passes against the stubs" "0" "$status"
    if [ "$status" -ne 0 ]; then
      echo "$output"
    fi
    soft assert_eq "the $op probe step calls the probe in order" "$expected" "$(cat "$STUBS/probe.log" 2>/dev/null)"
    soft assert_eq "the $op probe pod's first call is dropped on sink port 9999" \
      "$ns $rel-probe dropped 10.244.0.10 9999" "$(grep -m 1 " $rel-probe " "$STUBS/probe.log" 2>/dev/null)"

    stub_on kubectl 'get service kubernetes -n default -o jsonpath={.spec.clusterIP}'
    run_probe_step
    soft assert_eq "the $op step stops on an empty Service VIP" "1" "$status"
    soft assert_contains "the $op step names the empty VIP" "$output" "ERROR: VIP is empty"
    soft assert_eq "the $op step calls no probe without a VIP" "" "$(cat "$STUBS/probe.log" 2>/dev/null)"

    stub_on kubectl 'get endpointslice kubernetes -n default -o json' \
      --out '{"endpoints":[{"addresses":["fd00::2"]}],"ports":[{"port":6443}]}'
    run_probe_step
    soft assert_eq "the $op step stops on an EndpointSlice without an IPv4 address" "1" "$status"
    soft assert_contains "the $op step names the empty API address" "$output" "ERROR: API_IP is empty"
    soft assert_eq "the $op step calls no probe without an API address" "" "$(cat "$STUBS/probe.log" 2>/dev/null)"

    stub_on kubectl 'get networkpolicy -n * -o name' --out networkpolicy.networking.k8s.io/deny-all
    run_probe_step
    soft assert_eq "the $op step stops on a NetworkPolicy in $targets" "1" "$status"
    soft assert_contains "the $op step names the targets namespace" "$output" \
      "ERROR: $targets holds a NetworkPolicy"
    soft assert_eq "the $op step calls no probe beside a targets policy" "" "$(cat "$STUBS/probe.log" 2>/dev/null)"

    stub_on kubectl 'wait *' --rc 1 --err 'error: timed out waiting for the condition on pods/stub'
    run_probe_step
    soft assert_nonzero_exit "the $op step fails when a fixture pod never turns Ready" "$status"
    soft assert_contains "the $op step passes kubectl's timeout on" "$output" \
      "timed out waiting for the condition"
    soft assert_eq "the $op step calls no probe before its pods are Ready" "" "$(cat "$STUBS/probe.log" 2>/dev/null)"

    run_probe_step STUB_PROBE_FAIL_AT=1
    soft assert_eq "the $op step ends at the first failing probe call" "1 1" \
      "$status $(wc -l <"$STUBS/probe.log" | tr -d ' ')"
    # Keep the variable names of the stand-ins in the script readable: one
    # per stand-in, named after its service.
    for standin in $standins; do
      svc="${standin%%:*}"
      var="$(tr '[:lower:]' '[:upper:]' <<<"$svc")_API"
      soft assert_contains "the $op step reads the $svc stand-in into $var" "$(cat "$STUBS/step.sh")" \
        "require $var \"\${$var}\""
    done
  done <<<"$SUITES"
}

# steps_with <suite> <text>: the indexes of the steps whose try scripts hold
# <text>, one per line.
steps_with() {
  TEXT="$2" yq '[.spec.steps[] | [.try[]? | select(has("script")) | .script.content] | join("\n")]
    | to_entries | .[] | select(.value | contains(strenv(TEXT))) | .key' "$1"
}

# is_before <a> <b>: "true" when both step indexes are set and <a> is lower.
is_before() {
  if [ -n "$1" ] && [ -n "$2" ] && [ "$1" -lt "$2" ]; then
    echo true
  else
    echo false
  fi
}

# --- Test 7: the horizon probe steps ---
@test "horizon reaches openstack-db before the drop and probes DNS, the API server and restarts after it" {
  require_mikefarah_yq
  require_tool jq

  local suite ctl drop dns restart last ns=horizon-system pod=horizon-operator-netpol-probe expected
  suite="$(suite_dir horizon)/chainsaw-test.yaml"
  ctl="$(steps_with "$suite" 'horizon-operator-netpol-control open openstack-db.openstack.svc 3306' | head -n 1)"
  drop="$(steps_with "$suite" 'the policy never dropped TCP 3306' | head -n 1)"
  dns="$(steps_with "$suite" "$pod resolves kubernetes.default.svc.cluster.local." | head -n 1)"
  restart="$(steps_with "$suite" 'ERROR: ${pod} restarted ${n} times under the policy' | tail -n 1)"
  last="$(($(yq '.spec.steps | length' "$suite") - 1))"
  assert_eq "horizon's control pod reaches openstack-db in a step before the drop" "true" \
    "$(is_before "$ctl" "$drop")"
  assert_eq "horizon probes DNS and the API server in a step after the drop" "true" \
    "$(is_before "$drop" "$dns")"
  assert_eq "horizon checks the operator's restarts again in its last step" "$last" "$restart"

  yq '[.spec.steps[].try[] | select(has("script")) | .script.content
    | select(test("horizon-operator-netpol-probe resolves"))] | .[0]' "$suite" >"$STUBS/step.sh"
  expected="$ns $pod resolves kubernetes.default.svc.cluster.local."
  expected="$expected"$'\n'"$ns $pod open 172.18.0.2 6443"
  expected="$expected"$'\n'"$ns $pod open 10.96.0.1 443"

  run_probe_step
  assert_eq "the horizon DNS and API server step passes against the stubs" "0" "$status"
  if [ "$status" -ne 0 ]; then
    echo "$output"
  fi
  assert_eq "the horizon DNS and API server step calls the probe in order" "$expected" \
    "$(cat "$STUBS/probe.log" 2>/dev/null)"

  stub_on kubectl 'get service kubernetes -n default -o jsonpath={.spec.clusterIP}'
  run_probe_step
  assert_eq "the horizon step stops on an empty Service VIP" "1" "$status"
  assert_contains "the horizon step names the empty VIP" "$output" "ERROR: VIP is empty"
  assert_eq "the horizon step calls no probe without a VIP" "" "$(cat "$STUBS/probe.log" 2>/dev/null)"

  stub_on kubectl 'get endpointslice kubernetes -n default -o json' \
    --out '{"endpoints":[{"addresses":["fd00::2"]}],"ports":[{"port":6443}]}'
  run_probe_step
  assert_eq "the horizon step stops on an EndpointSlice without an IPv4 address" "1" "$status"
  assert_contains "the horizon step names the empty API address" "$output" "ERROR: API_IP is empty"
  assert_eq "the horizon step calls no probe without an API address" "" \
    "$(cat "$STUBS/probe.log" 2>/dev/null)"
}

# --- Test 8: the run step ---
@test "the e2e-operator run step reaches the suites, nova's in shard 1" {

  local run shard_two
  run="$(job_step e2e-operator "Run E2E tests")"
  assert_contains "the run step adds tests/e2e/<op>-operator/" "$run" \
    'test_dirs+=("tests/e2e/${OPERATOR}-operator/")'
  assert_contains "the nova shards split tests/e2e/nova-operator/ too" "$run" \
    'for dir in tests/e2e/nova/*/ tests/e2e/nova-operator/*/; do'
  shard_two="$(sed -n 's/^ *shard_two="\(.*\)"$/\1/p' <<<"$run")"
  assert_not_empty "the run step names the shard 2 suites" "$shard_two"
  assert_not_contains "nova's network-policy-egress runs in shard 1" " $shard_two " \
    " network-policy-egress "
}
