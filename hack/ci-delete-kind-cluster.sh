#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-delete-kind-cluster.sh — Tear down the kind cluster(s) of an e2e job.
#
# The last step of every e2e job, so that the cluster goes while the job still
# owns the runner. Before it existed the teardown was left to the post step of
# helm/kind-action, whose `kind delete cluster` is one attempt with no retry and
# whose failure is the job's failure. Run 34714006750 is what that costs: every
# suite of e2e-infra passed, the post step then lost the docker race
#
#   Error response from daemon: cannot remove container
#   "cobaltcore-control-plane": could not kill container: tried to kill
#   container, but did not receive an exit event
#
# and a job with nothing wrong in it was reported red. The runner's own
# job-completed hook removed the cluster seconds later, which is the second half
# of the point: nothing depended on that deletion succeeding right then.
#
# So this script inverts both halves. It retries — it reuses the sweep of
# hack/ci-reset-kind-cluster.sh, whose force-remove rides out that race — and it
# never fails the job: a cluster that survives is annotated as a warning and
# left to the reset that fronts the next job on this runner, which is the same
# leftover that reset has always been there to clear.
#
# Scope is `all`: a job owns its runner for its duration, so at the end of it
# every kind cluster on the host is this job's own. That also covers the jobs
# that create two (e2e-multicluster) without threading either name in here.
#
# Required env vars:
#   (none)
#
# Optional env vars:
#   KIND_TEARDOWN_SCRIPT  — sweep to run (default: hack/ci-reset-kind-cluster.sh)
#   anything the sweep reads — KIND_RM_ATTEMPTS and KIND_RM_RETRY_DELAY in
#   particular — is passed through untouched.
#
# Usage:
#   hack/ci-delete-kind-cluster.sh
#
# Exits 0 always, so a teardown problem cannot turn a green test run red.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

KIND_TEARDOWN_SCRIPT="${KIND_TEARDOWN_SCRIPT:-${SCRIPT_DIR}/ci-reset-kind-cluster.sh}"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

main() {
  if [[ ! -f "${KIND_TEARDOWN_SCRIPT}" ]]; then
    echo "::warning::kind teardown skipped: '${KIND_TEARDOWN_SCRIPT}' does not exist."
    return 0
  fi

  log "Tearing down every kind cluster left on this runner."

  # KIND_RESET_SCOPE=all also bypasses the once-per-job marker the sweep writes,
  # so a job that already swept on its way in still tears down everything here.
  if KIND_RESET_SCOPE=all bash "${KIND_TEARDOWN_SCRIPT}"; then
    log "Teardown complete."
    return 0
  fi

  echo "::warning::a kind cluster survived the teardown of this job — the next e2e job on this runner clears it before it creates its own."
  return 0
}

main "$@"
