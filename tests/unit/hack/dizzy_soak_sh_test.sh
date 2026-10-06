#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the workstation driver of the dizzy soak, hack/dizzy-soak.sh
# (#1274):
#   1. Without a subcommand and with an unknown one it prints a usage naming
#      the four subcommands and every setting to stderr and exits 1, and
#      `make -n dizzy-soak-<subcommand>` calls it with the subcommand.
#   2. start exits 1 with a message of its own and no mutating kubectl call
#      for each of its nine preflights: kubectl, the namespace dizzy, the
#      overlay, a Ready ControlPlane, the Secret k-orc-clouds-yaml, the lab
#      fixtures (only without DIZZY_SCENARIO), the scenario file, the
#      settings and a soak that already runs, also one whose pod terminates
#      after an eviction. DIZZY_SOAK_DURATION accepts 6h, 90m, 1h30m and 0
#      and refuses 6, six, -1h, 0h and the empty string.
#   3. start deletes a finished Job, creates the password Secret only when
#      it is absent, from stdin, stops with the identity's state when K-ORC
#      does not make it Available, and otherwise applies the clouds.yaml (the
#      keys of the soak's cloud, a ' doubled, mode 600, removed after a
#      successful and a failed start), the runner, the scenario and the
#      settings, creates the Job with the dizzy image of DIZZY_VERSION, and
#      waits for its pod.
#   4. status prints none, running, complete or failed and exits 1 only for
#      none; a Job without a Complete or Failed condition is running for
#      status, start and stop alike. stop sends TERM to the runner, waits for
#      the Job and fetches the report, exiting by the Job's result, and gives
#      up after 600s; it deletes a Job whose pod never ran and exits 1, and
#      changes nothing when it cannot read the Job's pods. report copies the newest or a named run, from the soak pod while it
#      runs and from a reader pod it deletes otherwise, also when kubectl cp
#      fails, and names what the claim holds when the run is not there.
#   5. The script calls no docker, opens no port-forward and does not read
#      EXTERNAL_CLUSTER.
#   6. What kubectl prints to stderr beside a read that succeeds reaches
#      neither jq nor the password: start, status and stop work, and the
#      clouds.yaml holds the password of the Secret.
#
# The script is sourced in a subshell (its BASH_SOURCE guard keeps soak_main
# from running) with a stub kubectl and a no-op sleep first on PATH;
# SOAK_OUTPUT_DIR is pointed at a temporary directory after sourcing.
#
# Usage: bash tests/unit/hack/dizzy_soak_sh_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SOAK_SH="$PROJECT_ROOT/hack/dizzy-soak.sh"
DIZZY_SH="$PROJECT_ROOT/hack/dizzy.sh"
OVERLAY="$PROJECT_ROOT/deploy/lab/metal-stack"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# The kubectl calls that change the cluster.
MUTATING='^kubectl (apply|create|delete|patch|replace|exec|cp|run|label|annotate|wait) '

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# make_stubs <dir>
# A kubectl stub that appends its argv to $CALL_LOG, captures what it is
# given into $CAPTURE_DIR, and answers from the environment:
#   KUBECTL_VERSION_RC     non-empty: `version` fails
#   KUBECTL_NO_NAMESPACE   non-empty: the namespace dizzy is NotFound
#   KUBECTL_CP             ready (default), notready, none or absent (no CRD)
#   KUBECTL_NO_KORC_SECRET non-empty: the Secret k-orc-clouds-yaml is NotFound
#   KUBECTL_FIXTURES       available (default), unavailable or missing
#   KUBECTL_JOB            none (default), active, pending (active, its pod
#                          not Running), terminating (no active pod,
#                          FailureTarget alone), complete or failed
#   KUBECTL_STOP_RESULT    the Job's state after the runner got TERM
#                          (default complete)
#   KUBECTL_PASSWORD_SECRET non-empty: dizzy-soak-user-password exists
#   KUBECTL_PASSWORD       the password that Secret holds (default the one
#                          start created, else s3cr3t)
#   KUBECTL_WAIT_RC        non-empty: the wait for the identity times out
#   KUBECTL_CREATE_FAIL    the name whose `create --dry-run` fails
#   KUBECTL_POD_PHASE      the phase of the soak's pod (default Running; in
#                          stop's pod list of a pending Job, no pod)
#   KUBECTL_PODS_RC        non-empty: every `get pods` fails with a server
#                          error
#   KUBECTL_RUNS           what `ls -1 /reports` lists besides lost+found
#                          (default two runs)
#   KUBECTL_CP_RC          non-empty: `kubectl cp` fails
#   KUBECTL_CP_NO_VERDICT  non-empty: the copied run has no verdict.json
#   KUBECTL_STDERR_NOISE   non-empty: every call prints a discovery error to
#                          stderr, and succeeds as before
# The soak's Running pod exists while the Job is active or terminating.
make_stubs() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/kubectl" <<'STUB'
#!/bin/bash
dir="$(dirname "$0")"
cap="$CAPTURE_DIR"
echo "kubectl $*" >>"$CALL_LOG"
if [ -n "${KUBECTL_STDERR_NOISE:-}" ]; then
  echo 'E1012 08:15:00.000000   4242 memcache.go:265] couldn'"'"'t get resource list for metrics.k8s.io/v1beta1: the server is currently unable to handle the request' >&2
fi
if [ -n "${KUBECTL_PODS_RC:-}" ] && [ "$1 $2" = "get pods" ]; then
  echo 'Error from server (InternalError): an error on the server ("") has prevented the request from succeeding (get pods)' >&2
  exit 1
fi
job_state() {
  if [ -f "$dir/stopped" ]; then echo "${KUBECTL_STOP_RESULT:-complete}"; else echo "${KUBECTL_JOB:-none}"; fi
}
case "$*" in
  "version --request-timeout=2s")
    if [ -n "${KUBECTL_VERSION_RC:-}" ]; then
      echo 'The connection to the server api.lab.example:443 was refused - did you specify the right host or port?' >&2
      exit 1
    fi
    echo 'Client Version: v1.35.0'
    ;;
  "get namespace dizzy -o name")
    if [ -n "${KUBECTL_NO_NAMESPACE:-}" ]; then
      echo 'Error from server (NotFound): namespaces "dizzy" not found' >&2
      exit 1
    fi
    echo namespace/dizzy
    ;;
  "get controlplanes.c5c3.io -n openstack -o json")
    case "${KUBECTL_CP:-ready}" in
      absent) echo 'error: the server doesn'"'"'t have a resource type "controlplanes"' >&2; exit 1 ;;
      none) echo '{"items":[]}' ;;
      notready) echo '{"items":[{"metadata":{"name":"controlplane"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}' ;;
      *) echo '{"items":[{"metadata":{"name":"controlplane"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
    esac
    ;;
  "get secret k-orc-clouds-yaml -n openstack -o name")
    if [ -n "${KUBECTL_NO_KORC_SECRET:-}" ]; then
      echo 'Error from server (NotFound): secrets "k-orc-clouds-yaml" not found' >&2
      exit 1
    fi
    echo secret/k-orc-clouds-yaml
    ;;
  "get images.openstack.k-orc.cloud/hvo-cirros-kvm flavors.openstack.k-orc.cloud/hvo-smoke-test-flavor -n openstack -o json")
    case "${KUBECTL_FIXTURES:-available}" in
      missing) echo 'Error from server (NotFound): images.openstack.k-orc.cloud "hvo-cirros-kvm" not found' >&2; exit 1 ;;
      unavailable) status=False ;;
      *) status=True ;;
    esac
    printf '{"items":[{"status":{"conditions":[{"type":"Available","status":"True"}]}},{"status":{"conditions":[{"type":"Available","status":"%s"}]}}]}\n' "$status"
    ;;
  "get job dizzy-soak -n dizzy -o json --ignore-not-found")
    case "$(job_state)" in
      active | pending) echo '{"status":{"active":1,"startTime":"2026-10-12T08:15:00Z"}}' ;;
      terminating) echo '{"status":{"active":0,"terminating":1,"startTime":"2026-10-12T08:15:00Z","conditions":[{"type":"FailureTarget","status":"True"}]}}' ;;
      complete) echo '{"status":{"startTime":"2026-10-12T08:15:00Z","conditions":[{"type":"Complete","status":"True"}]}}' ;;
      failed) echo '{"status":{"startTime":"2026-10-12T08:15:00Z","conditions":[{"type":"Failed","status":"True"}]}}' ;;
    esac
    ;;
  "get secret dizzy-soak-user-password -n openstack --ignore-not-found -o name")
    if [ -n "${KUBECTL_PASSWORD_SECRET:-}" ] || [ -f "$dir/password" ]; then echo secret/dizzy-soak-user-password; fi
    ;;
  "create secret generic dizzy-soak-user-password "*)
    cat >"$dir/password"
    ;;
  "wait -f "*)
    if [ -n "${KUBECTL_WAIT_RC:-}" ]; then
      echo 'error: timed out waiting for the condition on users/dizzy-soak' >&2
      exit 1
    fi
    ;;
  "get -f "*)
    echo 'NAME                                     READY   STATUS'
    echo 'user.openstack.k-orc.cloud/dizzy-soak     False   Waiting for Project/dizzy-soak to be ready'
    ;;
  "get secret dizzy-soak-user-password -n openstack -o jsonpath={.data.password}")
    if [ -n "${KUBECTL_PASSWORD+x}" ]; then printf '%s' "$KUBECTL_PASSWORD"
    elif [ -f "$dir/password" ]; then cat "$dir/password"
    else printf 's3cr3t'; fi | base64 | tr -d '\n'
    ;;
  "create "*"--dry-run=client -o yaml")
    if [ "$1 $2" = "create secret" ]; then name="$4"; else name="$3"; fi
    if [ "$name" = "${KUBECTL_CREATE_FAIL:-}" ]; then
      echo "error: failed to create $name" >&2
      exit 1
    fi
    for arg in "$@"; do
      case "$arg" in
        --from-file=*)
          spec="${arg#--from-file=}"
          cp "${spec#*=}" "$cap/$name.${spec%%=*}"
          ls -l "${spec#*=}" | cut -c1-10 >"$cap/$name.${spec%%=*}.mode"
          echo "${spec#*=}" >"$cap/$name.${spec%%=*}.path"
          ;;
        --from-literal=*) echo "${arg#--from-literal=}" >>"$cap/$name.literals" ;;
      esac
    done
    printf 'metadata:\n  name: %s\n' "$name"
    ;;
  "apply -f -")
    cat >>"$cap/applied.yaml"
    ;;
  "create -f -")
    stdin="$(cat)"
    if grep -q '^kind: Job' <<<"$stdin"; then
      printf '%s\n' "$stdin" >"$cap/job.yaml"
    else
      printf '%s\n' "$stdin" >"$cap/reader-pod.json"
    fi
    ;;
  "get pods -n dizzy -l batch.kubernetes.io/job-name=dizzy-soak -o jsonpath={.items[0].metadata.name} {.items[0].status.phase}")
    printf 'dizzy-soak-abcde %s' "${KUBECTL_POD_PHASE:-Running}"
    ;;
  "get pods -n dizzy -l batch.kubernetes.io/job-name=dizzy-soak --field-selector=status.phase=Running -o jsonpath={.items[0].metadata.name}")
    case "$(job_state)" in active | terminating) printf 'dizzy-soak-abcde' ;; esac
    ;;
  "get pods -n dizzy -l batch.kubernetes.io/job-name=dizzy-soak -o json")
    case "$(job_state)" in
      active | terminating) phase=Running ;;
      *) phase="${KUBECTL_POD_PHASE:-}" ;;
    esac
    if [ -n "$phase" ]; then
      printf '{"items":[{"metadata":{"name":"dizzy-soak-abcde"},"status":{"phase":"%s"}}]}\n' "$phase"
    else
      echo '{"items":[]}'
    fi
    ;;
  "logs "*)
    echo 'runner: last log line'
    ;;
  "exec dizzy-soak-abcde -n dizzy -c runner -- kill -TERM 1")
    touch "$dir/stopped"
    ;;
  "exec "*" -- ls -1 /reports")
    for run in ${KUBECTL_RUNS-20261012T081500Z 20261013T081500Z}; do echo "$run"; done
    echo lost+found
    ;;
  "cp "*)
    if [ -n "${KUBECTL_CP_RC:-}" ]; then
      echo 'error: tar: /reports/x: No such file or directory' >&2
      exit 1
    fi
    dest="${!#}"
    mkdir -p "$dest"
    echo '# dizzy soak' >"$dest/report.md"
    if [ -z "${KUBECTL_CP_NO_VERDICT:-}" ]; then echo '{"verdict":"FAIL","checks":[]}' >"$dest/verdict.json"; fi
    ;;
esac
exit 0
STUB
  # shellcheck disable=SC2016 # $* and $CALL_LOG expand when the stub runs
  printf '#!/bin/bash\necho "sleep $*" >>"$CALL_LOG"\n' >"$dir/sleep"
  chmod +x "$dir/kubectl" "$dir/sleep"
}

# new_case <tmp> — A fresh stub directory, capture directory and logs.
new_case() {
  local tmp="$1"
  rm -rf "${tmp:?}/bin" "$tmp/capture" "$tmp/tmpdir" "$tmp/out"
  mkdir -p "$tmp/capture" "$tmp/tmpdir" "$tmp/out"
  make_stubs "$tmp/bin"
  export CALL_LOG="$tmp/calls.log" CAPTURE_DIR="$tmp/capture"
  : >"$CALL_LOG"
}

# run_soak <tmp> <subcommand...> [-- env_var=value...] — Source the script
# with the stubs first on PATH and the given settings, point
# SOAK_OUTPUT_DIR at <tmp>/out and run soak_main. stdout goes to
# <tmp>/stdout, stderr to <tmp>/stderr; returns soak_main's status.
run_soak() {
  local tmp="$1" args=() rc
  shift
  while [[ $# -gt 0 && "$1" != "--" ]]; do
    args+=("$1")
    shift
  done
  [[ $# -gt 0 ]] && shift
  (
    unset DIZZY_SOAK_SERVICE DIZZY_SOAK_DURATION DIZZY_SCENARIO DIZZY_ARGS DIZZY_VERSION DIZZY_AUTH_URL \
      DIZZY_CP_NAME DIZZY_CP_NAMESPACE EXTERNAL_OVERLAY DIZZY_SOAK_NAMESPACES DIZZY_SOAK_SAMPLE_INTERVAL \
      DIZZY_SOAK_MAX_ERROR_RATE DIZZY_SOAK_MAX_P95_SECONDS EXTERNAL_CLUSTER
    for assignment in "$@"; do
      export "${assignment?}"
    done
    PATH="$tmp/bin:$PATH"
    TMPDIR="$tmp/tmpdir"
    export PATH TMPDIR
    # shellcheck source=/dev/null
    source "$SOAK_SH"
    # shellcheck disable=SC2034 # read by the sourced soak_report
    SOAK_OUTPUT_DIR="$tmp/out"
    soak_main ${args[@]+"${args[@]}"}
  ) >"$tmp/stdout" 2>"$tmp/stderr"
  rc=$?
  return "$rc"
}

# mutations — The mutating kubectl calls of $CALL_LOG.
mutations() {
  grep -E "$MUTATING" "$CALL_LOG" || true
}

# errors <tmp> — The ERROR lines of a run's stdout, without the timestamp.
errors() {
  grep 'ERROR: ' "$1/stdout" | sed 's/^\[[^]]*\] //'
}

# pinned_version — The DIZZY_VERSION default of hack/dizzy.sh.
pinned_version() {
  # shellcheck disable=SC2016 # the pin line's literal text
  sed -n 's/^DIZZY_VERSION="${DIZZY_VERSION:-\(v[0-9.]*\)}"$/\1/p' "$DIZZY_SH"
}

# ---------------------------------------------------------------------------
# Test 1: usage
# ---------------------------------------------------------------------------
test_usage() {
  echo "Test: without a subcommand and with an unknown one the usage goes to stderr with exit 1"

  local tmp rc sub setting
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for sub in "" "restart"; do
    new_case "$tmp"
    if [[ -z "$sub" ]]; then run_soak "$tmp"; else run_soak "$tmp" "$sub"; fi
    rc=$?
    assert_eq "'${sub:-<none>}' exits 1" "1" "$rc"
    assert_eq "'${sub:-<none>}' prints nothing to stdout" "" "$(cat "$tmp/stdout")"
    assert_eq "'${sub:-<none>}' calls no kubectl" "" "$(cat "$CALL_LOG")"
  done
  local usage
  usage="$(cat "$tmp/stderr")"
  for sub in start status stop "report [run]"; do
    assert_contains "the usage names '$sub'" "$usage" "  $sub "
  done
  for setting in DIZZY_SOAK_SERVICE DIZZY_SOAK_DURATION DIZZY_SCENARIO DIZZY_ARGS DIZZY_VERSION DIZZY_AUTH_URL \
    DIZZY_CP_NAME DIZZY_CP_NAMESPACE EXTERNAL_OVERLAY DIZZY_SOAK_NAMESPACES DIZZY_SOAK_SAMPLE_INTERVAL \
    DIZZY_SOAK_MAX_ERROR_RATE DIZZY_SOAK_MAX_P95_SECONDS; do
    assert_contains "the usage names $setting" "$usage" "  $setting "
  done

  for sub in start status stop report; do
    assert_eq "make -n dizzy-soak-$sub calls hack/dizzy-soak.sh $sub" "hack/dizzy-soak.sh $sub" \
      "$(make -s -n -C "$PROJECT_ROOT" "dizzy-soak-$sub" 2>/dev/null)"
  done
}

# ---------------------------------------------------------------------------
# Test 2: the preflights
# ---------------------------------------------------------------------------
# preflight_fails <tmp> <description> <message> [env_var=value...] — start
# exits 1 with that ERROR line alone and makes no mutating call.
preflight_fails() {
  local tmp="$1" description="$2" message="$3" rc
  shift 3
  new_case "$tmp"
  run_soak "$tmp" start -- "$@"
  rc=$?
  assert_eq "$description: start exits 1" "1" "$rc"
  assert_eq "$description: the one error names it" "ERROR: $message" "$(errors "$tmp" | head -n 1)"
  assert_eq "$description: no mutating kubectl call" "" "$(mutations)"
}

test_preflights() {
  echo "Test: each preflight of start stops it with its own message before any change"

  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/no-soak"

  preflight_fails "$tmp" "(a) no cluster" "kubectl is not configured or no cluster is reachable" KUBECTL_VERSION_RC=1
  assert_contains "(a) kubectl's message follows" "$(cat "$tmp/stdout")" "api.lab.example:443 was refused"
  preflight_fails "$tmp" "(b) no namespace dizzy" \
    "the dizzy stack is not installed (no namespace dizzy); deploy it with WITH_DIZZY=true first" KUBECTL_NO_NAMESPACE=1
  assert_eq "(b) a NotFound adds no second line" "1" "$(grep -c '^\[' "$tmp/stdout")"
  preflight_fails "$tmp" "(c) no overlay" \
    "no soak overlay at $tmp/no-soak/dizzy-soak/kustomization.yaml (EXTERNAL_OVERLAY='$tmp/no-soak')" \
    EXTERNAL_OVERLAY="$tmp/no-soak"
  preflight_fails "$tmp" "(d) a ControlPlane not Ready" "ControlPlane openstack/controlplane is not Ready" KUBECTL_CP=notready
  preflight_fails "$tmp" "(d) no ControlPlane" "no ControlPlane found in namespace openstack" KUBECTL_CP=none
  preflight_fails "$tmp" "(d) no ControlPlane CRD" "no ControlPlane found in namespace openstack" KUBECTL_CP=absent
  assert_contains "(d) kubectl's message follows" "$(cat "$tmp/stdout")" "doesn't have a resource type \"controlplanes\""
  preflight_fails "$tmp" "(e) no K-ORC credentials" "the Secret k-orc-clouds-yaml is missing in namespace openstack" \
    KUBECTL_NO_KORC_SECRET=1
  preflight_fails "$tmp" "(f) fixtures not Available" \
    "the lab fixtures image/hvo-cirros-kvm and flavor/hvo-smoke-test-flavor are missing or not Available in openstack; apply $OVERLAY/hypervisor-fixtures or name a scenario with DIZZY_SCENARIO" \
    KUBECTL_FIXTURES=unavailable
  preflight_fails "$tmp" "(f) fixtures missing" \
    "the lab fixtures image/hvo-cirros-kvm and flavor/hvo-smoke-test-flavor are missing or not Available in openstack; apply $OVERLAY/hypervisor-fixtures or name a scenario with DIZZY_SCENARIO" \
    KUBECTL_FIXTURES=missing
  preflight_fails "$tmp" "(g) no scenario file" "scenario file not found: $tmp/gone.yaml" DIZZY_SCENARIO="$tmp/gone.yaml"
  assert_eq "(g) with DIZZY_SCENARIO set the fixtures are not read" "" "$(grep 'images.openstack.k-orc.cloud' "$CALL_LOG" || true)"
  preflight_fails "$tmp" "(h) a bad duration" "DIZZY_SOAK_DURATION must be 0 or a duration such as 6h" DIZZY_SOAK_DURATION=six
  preflight_fails "$tmp" "(i) a soak runs" "a soak is already running; stop it with make dizzy-soak-stop" KUBECTL_JOB=active
  preflight_fails "$tmp" "(i) a soak's pod terminates" "a soak is already running; stop it with make dizzy-soak-stop" \
    KUBECTL_JOB=terminating
}

test_settings() {
  echo "Test: start accepts the durations 6h, 90m, 1h30m and 0 and refuses malformed settings"

  local tmp value rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for value in 6h 90m 1h30m 0; do
    new_case "$tmp"
    run_soak "$tmp" start -- DIZZY_SOAK_DURATION="$value"
    rc=$?
    assert_eq "DIZZY_SOAK_DURATION=$value starts the soak" "0" "$rc"
    assert_contains "and reaches the runner" "$(cat "$tmp/capture/dizzy-soak-config.literals")" "DIZZY_SOAK_DURATION=$value"
  done
  for value in 6 six -1h 0h ""; do
    preflight_fails "$tmp" "DIZZY_SOAK_DURATION='$value'" "DIZZY_SOAK_DURATION must be 0 or a duration such as 6h" \
      DIZZY_SOAK_DURATION="$value"
  done
  for value in 0 abc 1.5; do
    preflight_fails "$tmp" "DIZZY_SOAK_SAMPLE_INTERVAL='$value'" \
      "DIZZY_SOAK_SAMPLE_INTERVAL must be a positive number of seconds such as 60" DIZZY_SOAK_SAMPLE_INTERVAL="$value"
  done
  for value in 101 -1 five; do
    preflight_fails "$tmp" "DIZZY_SOAK_MAX_ERROR_RATE='$value'" \
      "DIZZY_SOAK_MAX_ERROR_RATE must be empty or a percentage from 0 to 100" DIZZY_SOAK_MAX_ERROR_RATE="$value"
  done
  for value in 0 -2 2s; do
    preflight_fails "$tmp" "DIZZY_SOAK_MAX_P95_SECONDS='$value'" \
      "DIZZY_SOAK_MAX_P95_SECONDS must be empty or a positive number of seconds" DIZZY_SOAK_MAX_P95_SECONDS="$value"
  done
  preflight_fails "$tmp" "DIZZY_VERSION with a slash" \
    "DIZZY_VERSION must be an image tag of letters, digits, '.', '_' and '-'" DIZZY_VERSION="v1/evil"

  new_case "$tmp"
  run_soak "$tmp" start -- DIZZY_SOAK_MAX_ERROR_RATE=0 DIZZY_SOAK_MAX_P95_SECONDS=0.5 DIZZY_SOAK_SAMPLE_INTERVAL=30
  rc=$?
  assert_eq "an error rate of 0, a p95 of 0.5 and an interval of 30 start the soak" "0" "$rc"
}

# ---------------------------------------------------------------------------
# Test 3: what start does
# ---------------------------------------------------------------------------
test_start() {
  echo "Test: start applies the overlay and the soak's objects, creates the Job and waits for its pod"

  local tmp rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  new_case "$tmp"
  run_soak "$tmp" start
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "start exits 0" "0" "$rc"
  assert_eq "there is no Job to delete" "" "$(grep '^kubectl delete job' <<<"$calls" || true)"
  assert_contains "a reader pod left behind is deleted" "$calls" "kubectl delete pod dizzy-soak-reader -n dizzy --ignore-not-found"
  assert_contains "the absent password Secret is created from stdin" "$calls" \
    "kubectl create secret generic dizzy-soak-user-password -n openstack --from-file=password=/dev/stdin"
  local password
  password="$(cat "$tmp/bin/password")"
  if [[ "$password" =~ ^[A-Za-z0-9]{32}$ ]]; then
    echo "  PASS: the password is 32 characters of [A-Za-z0-9]"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the password '$password' is not 32 characters of [A-Za-z0-9]"
    FAIL=$((FAIL + 1))
  fi
  assert_not_contains "and never in an argument" "$calls" "$password"
  assert_contains "the overlay is applied" "$calls" "kubectl apply -k $OVERLAY/dizzy-soak"
  assert_contains "the identity is waited for" "$calls" \
    "kubectl wait -f $OVERLAY/dizzy-soak/identity.yaml --for=condition=Available --timeout=300s"
  assert_eq "the password, the four objects and the Job are created in this order" \
    "create secret generic dizzy-soak-user-password|apply -k|wait -f|create secret generic dizzy-soak-clouds|create configmap dizzy-soak-runner|create configmap dizzy-soak-scenario|create configmap dizzy-soak-config|create -f -" \
    "$(grep -oE '^kubectl (create secret generic [a-z-]+|create configmap [a-z-]+|apply -k|wait -f|create -f -)' "$CALL_LOG" |
      sed 's/^kubectl //' | paste -sd '|' -)"
  assert_eq "each object goes through kubectl apply" "4" "$(grep -c '^kubectl apply -f -$' "$CALL_LOG")"

  # The clouds.yaml.
  local clouds
  clouds="$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml")"
  assert_eq "clouds.yaml holds the soak's cloud" "$(printf '%s\n' \
    'clouds:' \
    '  dizzy-soak:' \
    '    auth:' \
    '      auth_url: http://controlplane-keystone.openstack.svc:5000/v3' \
    '      username: dizzy-soak' \
    "      password: '${password}'" \
    '      project_name: dizzy-soak' \
    '      user_domain_name: Default' \
    '      project_domain_name: Default' \
    '    identity_api_version: 3' \
    '    interface: internal' \
    '    region_name: RegionOne')" "$clouds"
  assert_eq "the file has mode 600 when kubectl reads it" "-rw-------" "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml.mode")"
  assert_eq "and is gone after start" "false" \
    "$([[ -e "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml.path")" ]] && echo true || echo false)"
  assert_eq "no temporary file is left" "" "$(ls "$tmp/tmpdir")"

  # The runner, the scenario and the settings.
  assert_eq "the runner ConfigMap holds hack/dizzy-soak-runner.sh" "$(cat "$PROJECT_ROOT/hack/dizzy-soak-runner.sh")" \
    "$(cat "$tmp/capture/dizzy-soak-runner.dizzy-soak-runner.sh")"
  assert_eq "the scenario ConfigMap holds the overlay's scenario.yaml" "$(cat "$OVERLAY/dizzy-soak/scenario.yaml")" \
    "$(cat "$tmp/capture/dizzy-soak-scenario.scenario.yaml")"
  assert_eq "the settings reach the runner with their defaults and an empty DIZZY_ARGS" "$(printf '%s\n' \
    DIZZY_SOAK_SERVICE=mix DIZZY_SOAK_DURATION=6h DIZZY_ARGS= "DIZZY_VERSION=$(pinned_version)" DIZZY_SOAK_NAMESPACES= \
    DIZZY_SOAK_SAMPLE_INTERVAL=60 DIZZY_SOAK_MAX_ERROR_RATE= DIZZY_SOAK_MAX_P95_SECONDS=)" \
    "$(cat "$tmp/capture/dizzy-soak-config.literals")"

  # The Job.
  assert_contains "the Job carries the pinned dizzy image" "$(cat "$tmp/capture/job.yaml")" \
    "image: ghcr.io/b42labs/dizzy:$(pinned_version)"
  assert_not_contains "and no token" "$(cat "$tmp/capture/job.yaml")" "dizzy:DIZZY_VERSION"
  assert_eq "otherwise it is job.yaml" "$(sed "s#dizzy:DIZZY_VERSION#dizzy:$(pinned_version)#" "$OVERLAY/dizzy-soak/job.yaml")" \
    "$(cat "$tmp/capture/job.yaml")"
  assert_contains "the follow-up commands are printed" "$(cat "$tmp/stdout")" "make dizzy-soak-stop"
  assert_contains "with the pod" "$(cat "$tmp/stdout")" "The soak runs in the pod dizzy/dizzy-soak-abcde."
}

test_start_variants() {
  echo "Test: start's variants: DIZZY_VERSION, DIZZY_SCENARIO, DIZZY_AUTH_URL, a quote, a finished Job, a second start"

  local tmp rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_soak "$tmp" start -- DIZZY_VERSION=v9.9.9
  assert_contains "DIZZY_VERSION=v9.9.9 sets the init container's image" "$(cat "$tmp/capture/job.yaml")" \
    "image: ghcr.io/b42labs/dizzy:v9.9.9"
  assert_contains "and the runner's setting" "$(cat "$tmp/capture/dizzy-soak-config.literals")" "DIZZY_VERSION=v9.9.9"

  new_case "$tmp"
  printf 'name: other\n' >"$tmp/other.yaml"
  run_soak "$tmp" start -- DIZZY_SCENARIO="$tmp/other.yaml" DIZZY_ARGS="--set resources.servers=10"
  rc=$?
  assert_eq "a named scenario starts the soak" "0" "$rc"
  assert_eq "the scenario ConfigMap holds the named file" "name: other" "$(cat "$tmp/capture/dizzy-soak-scenario.scenario.yaml")"
  assert_eq "the fixtures are not read" "" "$(grep 'images.openstack.k-orc.cloud' "$CALL_LOG" || true)"
  assert_contains "DIZZY_ARGS reaches the runner" "$(cat "$tmp/capture/dizzy-soak-config.literals")" \
    "DIZZY_ARGS=--set resources.servers=10"

  new_case "$tmp"
  run_soak "$tmp" start -- DIZZY_AUTH_URL=http://keystone.example:5000/v3 KUBECTL_PASSWORD="it's" KUBECTL_PASSWORD_SECRET=1
  calls="$(cat "$CALL_LOG")"
  assert_contains "DIZZY_AUTH_URL wins" "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml")" \
    "      auth_url: http://keystone.example:5000/v3"
  assert_contains "a quote in the password is doubled" "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml")" \
    "      password: 'it''s'"
  assert_not_contains "a password Secret that exists is not created again" "$calls" "create secret generic dizzy-soak-user-password"

  new_case "$tmp"
  run_soak "$tmp" start -- DIZZY_CP_NAME=cp2 DIZZY_CP_NAMESPACE=openstack
  assert_contains "DIZZY_CP_NAME names the Keystone Service" "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml")" \
    "      auth_url: http://cp2-keystone.openstack.svc:5000/v3"

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_JOB=complete
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a finished Job does not stop start" "0" "$rc"
  assert_contains "it is deleted, its pods first" "$calls" \
    "kubectl delete job dizzy-soak -n dizzy --cascade=foreground --wait --timeout=300s"
  assert_contains "and the new Job is created" "$calls" "kubectl create -f -"

  # A second start: the Secret the first created exists.
  run_soak "$tmp" start -- KUBECTL_JOB=failed
  assert_eq "a second start creates the password Secret no second time" "1" \
    "$(grep -c 'create secret generic dizzy-soak-user-password' "$CALL_LOG")"
}

test_start_failures() {
  echo "Test: start stops on an identity K-ORC does not make Available and on a pod that does not run"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_WAIT_RC=1
  rc=$?
  assert_eq "a wait that times out exits 1" "1" "$rc"
  assert_eq "with the message" "ERROR: the soak identity did not become Available within 300s:" "$(errors "$tmp")"
  assert_contains "and kubectl's line" "$(cat "$tmp/stdout")" "timed out waiting for the condition on users/dizzy-soak"
  assert_contains "then the identity's state" "$(cat "$tmp/stdout")" "user.openstack.k-orc.cloud/dizzy-soak     False"
  assert_contains "from kubectl get -f" "$(cat "$CALL_LOG")" "kubectl get -f $OVERLAY/dizzy-soak/identity.yaml"
  assert_not_contains "no Job is created" "$(cat "$CALL_LOG")" "create -f -"

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_CREATE_FAIL=dizzy-soak-runner
  rc=$?
  assert_eq "a ConfigMap that cannot be applied exits 1" "1" "$rc"
  assert_eq "with its message" "ERROR: cannot apply the ConfigMap dizzy-soak-runner in dizzy (kubectl's error is above)" \
    "$(errors "$tmp")"
  assert_eq "the clouds.yaml was written" "true" "$([[ -s "$tmp/capture/dizzy-soak-clouds.clouds.yaml" ]] && echo true || echo false)"
  assert_eq "and is gone after the failed start" "" "$(ls "$tmp/tmpdir")"
  assert_not_contains "no Job is created" "$(cat "$CALL_LOG")" "create -f -"

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_POD_PHASE=Pending
  rc=$?
  assert_eq "a pod that stays Pending exits 1" "1" "$rc"
  assert_contains "after 300s" "$(errors "$tmp")" "ERROR: the soak pod did not start within 300s"
  assert_eq "polling every 5s" "61" "$(grep -c '^sleep 5$' "$CALL_LOG")"

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_POD_PHASE=Failed
  rc=$?
  assert_eq "a pod that fails exits 1" "1" "$rc"
  assert_contains "with its last log lines" "$(cat "$tmp/stdout")" "runner: last log line"
}

# ---------------------------------------------------------------------------
# Test 4: status, stop and report
# ---------------------------------------------------------------------------
test_status() {
  echo "Test: status prints none, running, complete or failed and exits 1 only for none"

  local tmp rc state expected
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  for state in none active terminating complete failed; do
    new_case "$tmp"
    run_soak "$tmp" status -- KUBECTL_JOB="$state"
    rc=$?
    case "$state" in
      none) expected="soak: none 1" ;;
      active | terminating) expected="soak: running 0" ;;
      *) expected="soak: $state 0" ;;
    esac
    assert_eq "a Job $state" "$expected" "$(head -n 1 "$tmp/stdout") $rc"
    assert_eq "status makes no mutating call" "" "$(mutations)"
  done
  new_case "$tmp"
  run_soak "$tmp" status -- KUBECTL_JOB=active
  assert_eq "it names the pod, its phase, the start and the last log lines" "$(printf '%s\n' \
    'soak: running' 'pod: dizzy-soak-abcde (Running)' 'started: 2026-10-12T08:15:00Z' 'last log lines:' 'runner: last log line')" \
    "$(cat "$tmp/stdout")"
  assert_contains "ten of them, from the runner" "$(cat "$CALL_LOG")" "kubectl logs dizzy-soak-abcde -n dizzy -c runner --tail=10"
}

test_stop() {
  echo "Test: stop sends TERM to the runner, waits for the Job, fetches the report and exits by the Job"

  local tmp rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_soak "$tmp" stop
  rc=$?
  assert_eq "without a Job stop exits 1" "1" "$rc"
  assert_eq "with its message" "ERROR: no soak is running" "$(errors "$tmp")"
  assert_eq "and makes no mutating call" "" "$(mutations)"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=complete
  assert_eq "a finished Job is not running" "ERROR: no soak is running" "$(errors "$tmp")"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=active KUBECTL_STOP_RESULT=complete
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "a soak that ends Complete exits 0" "0" "$rc"
  assert_contains "TERM goes to PID 1 of the runner" "$calls" "kubectl exec dizzy-soak-abcde -n dizzy -c runner -- kill -TERM 1"
  assert_eq "it polls every 5s" "sleep 5" "$(grep '^sleep ' "$CALL_LOG" | sort -u)"
  assert_contains "then fetches the report through a reader" "$calls" "kubectl cp -n dizzy -c reader dizzy-soak-reader:/reports/20261013T081500Z $tmp/out/20261013T081500Z"
  assert_eq "the report is local" "true" "$([[ -f "$tmp/out/20261013T081500Z/report.md" ]] && echo true || echo false)"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=terminating KUBECTL_STOP_RESULT=failed
  rc=$?
  assert_contains "a soak whose pod terminates is stopped too" "$(cat "$CALL_LOG")" \
    "kubectl exec dizzy-soak-abcde -n dizzy -c runner -- kill -TERM 1"
  assert_eq "and stop exits by its result" "1" "$rc"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=pending KUBECTL_POD_PHASE=Pending
  rc=$?
  assert_eq "a soak whose pod never ran is stopped by deleting its Job" \
    "kubectl delete job dizzy-soak -n dizzy --cascade=foreground --wait --timeout=300s" "$(mutations)"
  assert_eq "then stop exits 1" "1" "$rc"
  assert_eq "and names the next start" \
    "ERROR: the soak's pod never ran, so there is no report; make dizzy-soak-start starts a soak again" \
    "$(errors "$tmp")"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=pending KUBECTL_POD_PHASE=Succeeded
  assert_eq "a Job whose pod has run is not deleted" "" "$(mutations)"
  assert_eq "it is not running" "ERROR: no soak is running" "$(errors "$tmp")"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=pending
  assert_eq "a Job whose pod was never created is deleted" \
    "kubectl delete job dizzy-soak -n dizzy --cascade=foreground --wait --timeout=300s" "$(mutations)"

  local state
  for state in active pending; do
    new_case "$tmp"
    run_soak "$tmp" stop -- KUBECTL_JOB="$state" KUBECTL_POD_PHASE=Pending KUBECTL_PODS_RC=1
    rc=$?
    assert_eq "a $state soak whose pods cannot be read exits 1" "1" "$rc"
    assert_eq "and stop makes no mutating call" "" "$(mutations)"
    assert_eq "with its message" "ERROR: cannot read the pods of the Job dizzy/dizzy-soak" "$(errors "$tmp")"
    assert_contains "and kubectl's error" "$(cat "$tmp/stdout")" "Error from server (InternalError)"
  done

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=active KUBECTL_STOP_RESULT=failed
  rc=$?
  assert_eq "a soak that ends Failed exits 1" "1" "$rc"
  assert_contains "after fetching the report" "$(cat "$CALL_LOG")" "kubectl cp "

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=active KUBECTL_STOP_RESULT=active
  rc=$?
  assert_eq "a soak that does not finish exits 1" "1" "$rc"
  assert_eq "after 600s" "ERROR: the soak did not finish within 600s; see make dizzy-soak-status" "$(errors "$tmp")"
  assert_eq "which is 120 polls" "120" "$(grep -c '^sleep 5$' "$CALL_LOG")"
  assert_not_contains "and fetches nothing" "$(cat "$CALL_LOG")" "kubectl cp "

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_JOB=active KUBECTL_CP_RC=1
  rc=$?
  assert_eq "a report that cannot be fetched exits 1" "1" "$rc"
  assert_contains "and says so" "$(errors "$tmp")" "ERROR: the soak finished complete, but its report could not be fetched"
}

test_report() {
  echo "Test: report copies a run from the soak pod or a reader pod and names what the claim holds"

  local tmp rc calls
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_soak "$tmp" report -- KUBECTL_JOB=active KUBECTL_CP_NO_VERDICT=1
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "report from a running soak exits 0" "0" "$rc"
  assert_contains "it lists the runs in the soak pod" "$calls" "kubectl exec dizzy-soak-abcde -n dizzy -c runner -- ls -1 /reports"
  assert_contains "and copies the newest" "$calls" \
    "kubectl cp -n dizzy -c runner dizzy-soak-abcde:/reports/20261013T081500Z $tmp/out/20261013T081500Z"
  assert_not_contains "without a reader pod" "$calls" "dizzy-soak-reader"
  assert_contains "a run still going has no verdict yet" "$(cat "$tmp/stdout")" "the run is still going"

  new_case "$tmp"
  run_soak "$tmp" report -- KUBECTL_JOB=complete
  rc=$?
  calls="$(cat "$CALL_LOG")"
  assert_eq "report after the soak exits 0" "0" "$rc"
  assert_contains "it creates the reader pod" "$calls" "kubectl create -f -"
  assert_contains "copies the newest run from it" "$calls" \
    "kubectl cp -n dizzy -c reader dizzy-soak-reader:/reports/20261013T081500Z $tmp/out/20261013T081500Z"
  assert_eq "and deletes it afterwards" "kubectl delete pod dizzy-soak-reader -n dizzy --ignore-not-found" "$(tail -n 1 "$CALL_LOG")"
  assert_contains "it prints the report and the verdict" "$(cat "$tmp/stdout")" "Report: $tmp/out/20261013T081500Z/report.md"
  assert_contains "the verdict" "$(cat "$tmp/stdout")" "Verdict: FAIL"
  local pod="$tmp/capture/reader-pod.json"
  assert_eq "the reader runs the runner image of job.yaml" \
    "$(sed -n 's/^[[:space:]]*image: \(docker\.io\/alpine\/k8s:.*\)$/\1/p' "$OVERLAY/dizzy-soak/job.yaml")" \
    "$(jq -r '.spec.containers[0].image' "$pod")"
  assert_eq "it sleeps 3600s" "sleep 3600" "$(jq -r '.spec.containers[0].command | join(" ")' "$pod")"
  assert_eq "it mounts the claim read-only" "dizzy-soak-reports true true" \
    "$(jq -r '"\(.spec.volumes[0].persistentVolumeClaim.claimName) \(.spec.volumes[0].persistentVolumeClaim.readOnly) \(.spec.containers[0].volumeMounts[0].readOnly)"' "$pod")"
  assert_eq "without a ServiceAccount token" "false" "$(jq -r '.spec.automountServiceAccountToken' "$pod")"

  new_case "$tmp"
  run_soak "$tmp" report 20261012T081500Z -- KUBECTL_JOB=complete
  assert_contains "a named run is copied" "$(cat "$CALL_LOG")" "dizzy-soak-reader:/reports/20261012T081500Z $tmp/out/20261012T081500Z"

  new_case "$tmp"
  run_soak "$tmp" report -- KUBECTL_JOB=complete KUBECTL_CP_RC=1
  rc=$?
  assert_eq "a kubectl cp that fails exits 1" "1" "$rc"
  assert_eq "the reader is still deleted" "kubectl delete pod dizzy-soak-reader -n dizzy --ignore-not-found" "$(tail -n 1 "$CALL_LOG")"

  new_case "$tmp"
  run_soak "$tmp" report -- KUBECTL_RUNS=""
  rc=$?
  assert_eq "an empty claim exits 1" "1" "$rc"
  assert_eq "with its message" "ERROR: no soak report on the claim dizzy-soak-reports" "$(errors "$tmp")"
  assert_eq "and deletes the reader" "kubectl delete pod dizzy-soak-reader -n dizzy --ignore-not-found" "$(tail -n 1 "$CALL_LOG")"

  local run
  for run in 20990101T000000Z ../x; do
    new_case "$tmp"
    run_soak "$tmp" report "$run"
    rc=$?
    assert_eq "the run '$run' exits 1" "1" "$rc"
    assert_eq "naming the run and the claim" "ERROR: no soak report $run on the claim dizzy-soak-reports; the claim holds:" "$(errors "$tmp")"
    assert_contains "and listing its runs" "$(cat "$tmp/stdout")" "         20261012T081500Z"
    assert_not_contains "and copying nothing" "$(cat "$CALL_LOG")" "kubectl cp "
  done
}

# ---------------------------------------------------------------------------
# Test 5: what the script never does
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016 # the script's literal text, not expanded here
test_no_workstation_plumbing() {
  echo "Test: the script calls no docker, opens no port-forward and does not read EXTERNAL_CLUSTER"

  local code
  code="$(grep -vE '^[[:space:]]*#' "$SOAK_SH")"
  assert_not_contains "no docker" "$code" "docker"
  assert_not_contains "no port-forward" "$code" "port-forward"
  assert_not_contains "no EXTERNAL_CLUSTER" "$code" "EXTERNAL_CLUSTER"
  assert_file_contains_fixed "it sources hack/dizzy.sh for the pin" "$SOAK_SH" 'source "${SOAK_SCRIPT_DIR}/dizzy.sh"'
}

# ---------------------------------------------------------------------------
# Test 6: kubectl's stderr beside a read that succeeds
# ---------------------------------------------------------------------------
test_kubectl_stderr_noise() {
  echo "Test: what kubectl prints to stderr beside a successful read reaches neither jq nor the password"

  local tmp rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  new_case "$tmp"
  run_soak "$tmp" start -- KUBECTL_STDERR_NOISE=1
  rc=$?
  assert_eq "start exits 0" "0" "$rc"
  assert_contains "the clouds.yaml holds the password of the Secret" "$(cat "$tmp/capture/dizzy-soak-clouds.clouds.yaml")" \
    "      password: '$(cat "$tmp/bin/password")'"
  assert_contains "kubectl's message still reaches the terminal" "$(cat "$tmp/stderr")" "memcache.go:265]"
  assert_eq "no temporary file is left" "" "$(ls "$tmp/tmpdir")"

  new_case "$tmp"
  run_soak "$tmp" status -- KUBECTL_STDERR_NOISE=1 KUBECTL_JOB=active
  rc=$?
  assert_eq "status reads the running soak" "soak: running 0" "$(head -n 1 "$tmp/stdout") $rc"

  new_case "$tmp"
  run_soak "$tmp" stop -- KUBECTL_STDERR_NOISE=1 KUBECTL_JOB=active
  rc=$?
  assert_eq "stop stops it" "0" "$rc"
  assert_contains "with TERM to the runner" "$(cat "$CALL_LOG")" "kubectl exec dizzy-soak-abcde -n dizzy -c runner -- kill -TERM 1"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (every check of this file skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_usage
test_preflights
test_settings
test_start
test_start_variants
test_start_failures
test_status
test_stop
test_report
test_no_workstation_plumbing
test_kubectl_stderr_noise

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
