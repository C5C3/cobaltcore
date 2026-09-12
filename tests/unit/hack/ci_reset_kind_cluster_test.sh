#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-reset-kind-cluster.sh clears a runner the way the e2e jobs need
# it cleared: the cluster about to be created always goes, every other leftover
# goes on the first sweep of a job and survives every later one, and a leftover
# that cannot be removed fails the step instead of letting the creation run into
# `node(s) already exist for a cluster with the name "cobaltcore"` again.
#
# The script is exercised against docker and kind stubs, so every assertion here
# is about the decisions it makes rather than about a live docker daemon. Two of
# them are worth spelling out:
#
#   - Containers are selected by the `io.x-k8s.kind.cluster` label kind stamps on
#     its nodes. A sweep that went by container name, or by "every container on
#     the host", would take the registry pull-through caches of
#     hack/deploy-infra.sh with it — the stub keeps one unlabelled container
#     around so that regression fails here.
#   - `kind delete cluster` is best-effort: the force-remove behind it is what
#     makes the reset dependable on a host where the previous job left a node
#     container kind itself no longer recognises.
#   - That force-remove is retried, because the docker daemon rejects a removal
#     it has not yet seen the container's exit event for. The stub can fail a
#     given number of removals with the daemon's own wording, which is what
#     separates riding out that race from papering over a real leftover.
#
# Usage: bash tests/unit/hack/ci_reset_kind_cluster_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RESET_SH="$PROJECT_ROOT/hack/ci-reset-kind-cluster.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# write_stubs <dir> <state_file> <log_file>
# Writes a docker and a kind stub into <dir>.
#
# <state_file> is the host's container inventory, one `<id> <cluster>` line per
# container; `-` as the cluster means the container carries no kind label (the
# registry-cache case). Both stubs append every invocation to <log_file>.
#
# Stub behaviour is steered by the environment of the script under test:
#   STUB_KIND                 — `absent` leaves kind off the PATH entirely
#   STUB_KIND_DELETE_EXIT     — exit code of `kind delete cluster` (default 0)
#   STUB_KIND_DELETE_EFFECTIVE — `false` keeps the node containers in place
#   STUB_DOCKER_RM_EFFECTIVE  — `false` makes `docker rm -f` a no-op
#   STUB_DOCKER_RM_FAIL_TIMES — how many `docker rm` calls fail with the
#                               missing-exit-event error before one works
write_stubs() {
  local dir="$1" state="$2" log="$3"
  mkdir -p "$dir"

  cat >"$dir/docker" <<STUB
#!/bin/bash
# Test stub docker — only the two \`ps\` shapes and the \`rm\` the reset script
# issues are recognised; anything else is a test failure rather than a silent
# exit 0. See write_stubs() in
# tests/unit/hack/ci_reset_kind_cluster_test.sh.
STATE="$state"
echo "docker \$*" >>"$log"
case "\$1" in
  ps)
    want=""
    key_only=false
    for arg in "\$@"; do
      case "\$arg" in
        label=io.x-k8s.kind.cluster=*) want="\${arg#label=io.x-k8s.kind.cluster=}" ;;
        label=io.x-k8s.kind.cluster)   key_only=true ;;
      esac
    done
    if [ -n "\$want" ]; then
      awk -v c="\$want" '\$2 == c { print \$1 }' "\$STATE"
    elif [ "\$key_only" = true ]; then
      awk '\$2 != "-" { print \$2 }' "\$STATE"
    else
      echo "[docker-stub] unexpected ps invocation: \$*" >&2
      exit 64
    fi
    ;;
  rm)
    count_file="\$STATE.rmcount"
    calls=\$(cat "\$count_file" 2>/dev/null || echo 0)
    calls=\$((calls + 1))
    echo "\$calls" >"\$count_file"
    if [ "\$calls" -le "\${STUB_DOCKER_RM_FAIL_TIMES:-0}" ]; then
      # Verbatim wording of the race this retry exists for.
      echo 'Error response from daemon: cannot remove container "cobaltcore-control-plane": could not kill container: tried to kill container, but did not receive an exit event' >&2
      exit 1
    fi
    if [ "\${STUB_DOCKER_RM_EFFECTIVE:-true}" = true ]; then
      for arg in "\$@"; do
        case "\$arg" in
          rm | -f | -v) ;;
          *)
            grep -v "^\$arg " "\$STATE" >"\$STATE.next" || true
            mv "\$STATE.next" "\$STATE"
            ;;
        esac
      done
    fi
    ;;
  *)
    echo "[docker-stub] unexpected invocation: \$*" >&2
    exit 64
    ;;
esac
exit 0
STUB
  chmod +x "$dir/docker"

  cat >"$dir/kind" <<STUB
#!/bin/bash
# Test stub kind — recognises \`delete cluster --name <name>\`.
STATE="$state"
echo "kind \$*" >>"$log"
if [ "\$1" = delete ] && [ "\$2" = cluster ] && [ "\$3" = --name ]; then
  if [ "\${STUB_KIND_DELETE_EFFECTIVE:-true}" = true ]; then
    awk -v c="\$4" '\$2 != c' "\$STATE" >"\$STATE.next"
    mv "\$STATE.next" "\$STATE"
  fi
  exit "\${STUB_KIND_DELETE_EXIT:-0}"
fi
echo "[kind-stub] unexpected invocation: \$*" >&2
exit 64
STUB
  chmod +x "$dir/kind"
}

# run_reset <tmp> <cluster> [env assignments…]
# Runs the reset script with PATH limited to the stubs (plus the system
# directories the script's own tools come from), and RUNNER_TEMP pointed inside
# <tmp> so the once-per-job marker never touches the real runner temp.
# Echoes combined stdout/stderr; returns the script's exit code.
run_reset() {
  local tmp="$1" cluster="$2"
  shift 2
  (
    export PATH="$tmp/bin:/usr/bin:/bin"
    export RUNNER_TEMP="$tmp/runner-temp"
    if [ "${STUB_KIND:-present}" = absent ]; then
      rm -f "$tmp/bin/kind"
    fi
    # The retry spacing is real seconds on a runner and dead time here; a case
    # that cares about it passes its own KIND_RM_RETRY_DELAY after this one.
    env KIND_RM_RETRY_DELAY=0 "$@" bash "$RESET_SH" "$cluster"
  ) 2>&1
}

# new_case <state lines…> — Sets up a fresh temp dir with the given inventory.
# Echoes the temp dir; the caller reads "$tmp/docker.log" and "$tmp/state".
new_case() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/runner-temp"
  printf '%s\n' "$@" >"$tmp/state"
  write_stubs "$tmp/bin" "$tmp/state" "$tmp/log"
  : >"$tmp/log"
  echo "$tmp"
}

# ---------------------------------------------------------------------------
# Test A: a clean runner is left alone
# ---------------------------------------------------------------------------
test_clean_runner() {
  echo "Test: a runner with no kind cluster needs no deletion"

  local tmp
  tmp="$(new_case "cache1 -")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore)"
  exit_code=$?

  assert_eq "exits 0 on a clean runner" "0" "$exit_code"
  assert_contains "says there is nothing to reset" \
    "$output" "No leftover kind cluster on this runner"
  assert_file_not_contains "does not remove any container" "$tmp/log" "docker rm"
  assert_file_not_contains "does not call kind delete" "$tmp/log" "kind delete"
}

# ---------------------------------------------------------------------------
# Test B: the leftover that breaks the creation is deleted
# ---------------------------------------------------------------------------
test_deletes_leftover_target() {
  echo "Test: deletes the leftover cluster the job is about to create"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "reports the leftover" \
    "$output" "Leftover kind cluster(s) on this runner: cobaltcore"
  assert_file_contains "asks kind to delete the cluster" \
    "$tmp/log" "kind delete cluster --name cobaltcore"
  assert_eq "no node container is left behind" "" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test C: the force-remove behind a failed `kind delete`
# ---------------------------------------------------------------------------
test_force_removes_after_kind_delete_failure() {
  echo "Test: force-removes the node containers when kind delete fails"

  local tmp
  tmp="$(new_case "n1 cobaltcore" "n2 cobaltcore" "cache1 -")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore \
    STUB_KIND_DELETE_EXIT=1 STUB_KIND_DELETE_EFFECTIVE=false)"
  exit_code=$?

  assert_eq "exits 0 — the cluster is gone either way" "0" "$exit_code"
  assert_contains "warns that kind delete failed" \
    "$output" "WARNING: 'kind delete cluster --name cobaltcore' failed"
  assert_file_contains "force-removes both node containers" "$tmp/log" "docker rm -f -v n1 n2"
  assert_eq "the unlabelled container survives the sweep" "cache1 -" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test D: kind missing from PATH (the sweep runs before it is installed)
# ---------------------------------------------------------------------------
test_works_without_kind_on_path() {
  echo "Test: removes the leftover with docker alone when kind is not installed"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(STUB_KIND=absent run_reset "$tmp" cobaltcore)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "says why it goes straight to docker" \
    "$output" "kind is not on PATH yet"
  assert_file_contains "removes the node container" "$tmp/log" "docker rm -f -v n1"
  assert_eq "the cluster is gone" "" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test E: a leftover that survives deletion fails the step
# ---------------------------------------------------------------------------
test_fails_when_leftover_survives() {
  echo "Test: fails when a node container survives both deletion paths"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore \
    STUB_KIND_DELETE_EXIT=1 STUB_KIND_DELETE_EFFECTIVE=false \
    STUB_DOCKER_RM_EFFECTIVE=false)"
  exit_code=$?

  assert_nonzero_exit "exits non-zero" "$exit_code"
  assert_contains "annotates the failure for the job log" \
    "$output" "::error::kind cluster 'cobaltcore' still has node containers"
  assert_eq "gives up after KIND_RM_ATTEMPTS removals, not before" \
    "3" "$(grep -c '^docker rm' "$tmp/log")"
  assert_eq "no marker is written when the sweep failed" \
    "0" "$(find "$tmp/runner-temp" -name 'cobaltcore-kind-reset-*.done' | wc -l | tr -d ' ')"
}

# ---------------------------------------------------------------------------
# Test E2: the missing-exit-event race is ridden out rather than reported
# ---------------------------------------------------------------------------
test_retries_through_missing_exit_event() {
  echo "Test: retries the force-remove the docker daemon rejected"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  # The shape of run 34714006750: kind cannot delete the cluster because the
  # daemon refuses to remove a container whose exit event has not arrived yet.
  # It arrives before the second attempt, which is the whole reason this is a
  # retry and not a failure.
  local output exit_code
  output="$(run_reset "$tmp" cobaltcore \
    STUB_KIND_DELETE_EXIT=1 STUB_KIND_DELETE_EFFECTIVE=false \
    STUB_DOCKER_RM_FAIL_TIMES=1)"
  exit_code=$?

  assert_eq "exits 0 — the second attempt got the container" "0" "$exit_code"
  assert_contains "keeps the daemon's own wording in the log" \
    "$output" "did not receive an exit event"
  assert_contains "says it is retrying" "$output" "retrying the removal (attempt 2/3)"
  assert_not_contains "does not report a surviving leftover" \
    "$output" "::error::kind cluster"
  assert_eq "two removals were enough" "2" "$(grep -c '^docker rm' "$tmp/log")"
  assert_eq "the cluster is gone" "" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test E3: KIND_RM_ATTEMPTS bounds the retry
# ---------------------------------------------------------------------------
test_rm_attempts_is_honoured() {
  echo "Test: KIND_RM_ATTEMPTS bounds how often the removal is retried"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore \
    STUB_KIND_DELETE_EXIT=1 STUB_KIND_DELETE_EFFECTIVE=false \
    STUB_DOCKER_RM_EFFECTIVE=false KIND_RM_ATTEMPTS=1)"
  exit_code=$?

  assert_nonzero_exit "still fails when the leftover survives" "$exit_code"
  assert_eq "one attempt means one removal" "1" "$(grep -c '^docker rm' "$tmp/log")"

  output="$(run_reset "$tmp" cobaltcore KIND_RM_ATTEMPTS=0)"
  exit_code=$?

  assert_eq "a non-positive attempt count is a usage error" "2" "$exit_code"
  assert_contains "explains what KIND_RM_ATTEMPTS accepts" \
    "$output" "::error::KIND_RM_ATTEMPTS must be a positive integer"
}

# ---------------------------------------------------------------------------
# Test F: the first sweep of a job takes every cluster, later ones do not
# ---------------------------------------------------------------------------
test_sweeps_foreign_clusters_once_per_job() {
  echo "Test: sweeps foreign leftovers once per job, then only its own cluster"

  local tmp
  tmp="$(new_case "n1 cobaltcore-mgmt" "n2 cobaltcore-target")"
  trap 'rm -rf "$tmp"' RETURN

  # First run of the job: nothing on this host can belong to a job running
  # alongside it, and either leftover would hold the host ports the new cluster
  # needs, so both go.
  local output exit_code
  output="$(run_reset "$tmp" cobaltcore)"
  exit_code=$?

  assert_eq "first sweep exits 0" "0" "$exit_code"
  assert_contains "first sweep works at scope all" "$output" "(scope: all)"
  assert_eq "both foreign leftovers are gone" "" "$(cat "$tmp/state")"
  assert_eq "the marker is written" \
    "1" "$(find "$tmp/runner-temp" -name 'cobaltcore-kind-reset-*.done' | wc -l | tr -d ' ')"

  # Second run in the same job: the cluster created in between must survive, so
  # only the name this run is about to create is removed.
  printf '%s\n' "n3 cobaltcore" "n4 cobaltcore-target" >"$tmp/state"
  : >"$tmp/log"

  output="$(run_reset "$tmp" cobaltcore)"
  exit_code=$?

  assert_eq "second sweep exits 0" "0" "$exit_code"
  assert_contains "second sweep narrows to the one cluster" "$output" "(scope: cluster)"
  assert_contains "says which cluster it keeps" "$output" "Keeping 'cobaltcore-target'"
  assert_eq "the cluster created in between survives" \
    "n4 cobaltcore-target" "$(cat "$tmp/state")"

  # The marker is scoped to the job that wrote it: the next job on this runner
  # sweeps in full again, even where the runner keeps RUNNER_TEMP around.
  printf '%s\n' "n5 cobaltcore-target" >"$tmp/state"
  : >"$tmp/log"

  output="$(run_reset "$tmp" cobaltcore GITHUB_RUN_ID=999 GITHUB_RUN_ATTEMPT=1)"
  exit_code=$?

  assert_eq "the next job's sweep exits 0" "0" "$exit_code"
  assert_contains "another job sweeps in full again" "$output" "(scope: all)"
  assert_eq "the leftover of the previous job is gone" "" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test G: KIND_RESET_SCOPE overrides the automatic choice
# ---------------------------------------------------------------------------
test_scope_override() {
  echo "Test: KIND_RESET_SCOPE overrides the automatic scope"

  local tmp
  tmp="$(new_case "n1 cobaltcore" "n2 other")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(run_reset "$tmp" cobaltcore KIND_RESET_SCOPE=cluster)"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "honours scope=cluster on a first run" "$output" "(scope: cluster)"
  assert_eq "the foreign cluster is untouched" "n2 other" "$(cat "$tmp/state")"

  output="$(run_reset "$tmp" cobaltcore KIND_RESET_SCOPE=bogus)"
  exit_code=$?

  assert_eq "an unknown scope is a usage error" "2" "$exit_code"
  assert_contains "explains the accepted values" \
    "$output" "::error::KIND_RESET_SCOPE must be all, cluster or auto"
}

# ---------------------------------------------------------------------------
# Test G2: an explicit scope=all needs no cluster name
# ---------------------------------------------------------------------------
test_scope_all_without_cluster_name() {
  echo "Test: KIND_RESET_SCOPE=all sweeps without being given a cluster name"

  local tmp
  tmp="$(new_case "n1 cobaltcore" "n2 cobaltcore-target" "cache1 -")"
  trap 'rm -rf "$tmp"' RETURN

  # How hack/ci-delete-kind-cluster.sh calls this at the end of a job: there is
  # no one cluster to single out, every one on the host belongs to the job that
  # is finishing.
  local output exit_code
  output="$(
    (
      export PATH="$tmp/bin:/usr/bin:/bin"
      export RUNNER_TEMP="$tmp/runner-temp"
      env -u CLUSTER_NAME -u KIND_CLUSTER KIND_RESET_SCOPE=all KIND_RM_RETRY_DELAY=0 \
        bash "$RESET_SH"
    ) 2>&1
  )"
  exit_code=$?

  assert_eq "exits 0" "0" "$exit_code"
  assert_contains "works at scope all" "$output" "(scope: all)"
  assert_eq "both clusters are gone, the unlabelled container is not" \
    "cache1 -" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Test H: usage errors
# ---------------------------------------------------------------------------
test_requires_cluster_name() {
  echo "Test: refuses to run without a cluster name"

  local tmp
  tmp="$(new_case "n1 cobaltcore")"
  trap 'rm -rf "$tmp"' RETURN

  local output exit_code
  output="$(
    (
      export PATH="$tmp/bin:/usr/bin:/bin"
      export RUNNER_TEMP="$tmp/runner-temp"
      env -u CLUSTER_NAME -u KIND_CLUSTER bash "$RESET_SH"
    ) 2>&1
  )"
  exit_code=$?

  assert_eq "exits 2 on a usage error" "2" "$exit_code"
  assert_contains "names the ways a cluster can be passed" \
    "$output" "::error::hack/ci-reset-kind-cluster.sh needs a cluster name"
  assert_eq "touches nothing" "n1 cobaltcore" "$(cat "$tmp/state")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_clean_runner
test_deletes_leftover_target
test_force_removes_after_kind_delete_failure
test_works_without_kind_on_path
test_fails_when_leftover_survives
test_retries_through_missing_exit_event
test_rm_attempts_is_honoured
test_sweeps_foreign_clusters_once_per_job
test_scope_override
test_scope_all_without_cluster_name
test_requires_cluster_name

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
