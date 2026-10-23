#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-quick-start-controlplane.sh — Run Steps 3 to 6 of
# docs/quick-start-controlplane.md with the page's own YAML blocks, on a
# cluster that `WITH_CONTROLPLANE=true make deploy-infra` brought up.
#
# .github/workflows/flux-path.yaml runs it every night, so the page's
# `# controlplane-ovn.yaml` block and its first `# controlplane.yaml` block are
# the manifests a machine applies. An edit to either block changes what the
# night applies.
#
# Subcommands:
#   up
#       Checks that kubectl, yq and jq are on PATH, then runs the stages below
#       in order and stops at the first failure with exit 1. Before each stage
#       it writes the stage's name to ${STATE_DIR}/stage, and after the last
#       one it writes "done".
#         extract  Copies the two blocks to ${STATE_DIR}/controlplane-ovn.yaml
#                  and ${STATE_DIR}/controlplane.yaml, checks that they are an
#                  OVNCentral and the ControlPlane ${CONTROLPLANE_NAME}, both
#                  in openstack, and on KIND_HOST_PORT=443 deletes every
#                  publicEndpoint from the ControlPlane copy, as the CR's
#                  comments tell a user on port 443 to do. deploy-infra seeded
#                  the admin password for CONTROLPLANE_NAME, so a CR of
#                  another name would stall at AdminPasswordReady.
#         ovn      Applies the OVNCentral and waits OVN_TIMEOUT for Ready.
#         apply    Applies the ControlPlane.
#         mariadb  Waits for mariadb/openstack-db to exist, then for Ready for
#                  what is left of MARIADB_TIMEOUT.
#                  The c5c3-operator creates the MariaDB some seconds after
#                  the apply, and the page's `kubectl wait` fails on NotFound
#                  before that.
#         onboard  Reads the OpenBao root token from Secret
#                  shared-services/openbao-init-keys and runs
#                  ${ONBOARD_SCRIPT} openstack ${CONTROLPLANE_NAME} with the
#                  token in BAO_TOKEN. The token reaches the onboarding script
#                  only through its environment, never through an argument or
#                  any output.
#         ready    Waits READY_TIMEOUT for the ControlPlane's Ready.
#   summary <file>
#       Writes a Markdown report of a failed run to <file> and exits 0: the
#       stage ${STATE_DIR}/stage names (deploy-infra when the file is absent,
#       because `up` never started), the ControlPlane's conditions, the
#       OVNCentral's Ready condition, the HelmReleases and the Flux sources in
#       flux-system. A read that fails writes "(unavailable: <its error>)"
#       instead. It reads no Secret: the report goes into a public issue.
#
# Optional env vars (defaults in brackets):
#   QUICK_START_DOC    [<repo root>/docs/quick-start-controlplane.md]
#   ONBOARD_SCRIPT     [<repo root>/deploy/openbao/bootstrap/setup-database-tenant.sh]
#   STATE_DIR          [_output/flux-path]
#   CONTROLPLANE_NAME  [controlplane]
#   KIND_HOST_PORT     [443]
#   OVN_TIMEOUT        [600]  seconds; the page's 10 minutes
#   MARIADB_TIMEOUT    [600]  seconds; the page's 10 minutes
#   READY_TIMEOUT      [1800] seconds; twice the page's 15 minutes, because a
#                      CI node cold-starts a service about four times slower
#                      than a workstation
#
# The script stays Bash 3.2 compatible, like hack/deploy-infra.sh, because a
# local reproduction on macOS runs it too. It never sets -x.
#
# Usage:
#   WITH_CONTROLPLANE=true make deploy-infra
#   hack/ci-quick-start-controlplane.sh up
#   hack/ci-quick-start-controlplane.sh summary _output/flux-path/summary.md
#
# Exit codes:
#   0 — up reached a Ready ControlPlane, or summary wrote its file
#   1 — a stage of up failed
#   2 — usage error

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

QUICK_START_DOC="${QUICK_START_DOC:-${REPO_ROOT}/docs/quick-start-controlplane.md}"
ONBOARD_SCRIPT="${ONBOARD_SCRIPT:-${REPO_ROOT}/deploy/openbao/bootstrap/setup-database-tenant.sh}"
STATE_DIR="${STATE_DIR:-_output/flux-path}"
CONTROLPLANE_NAME="${CONTROLPLANE_NAME:-controlplane}"
KIND_HOST_PORT="${KIND_HOST_PORT:-443}"
OVN_TIMEOUT="${OVN_TIMEOUT:-600}"
MARIADB_TIMEOUT="${MARIADB_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"

# The namespace both of the page's CRs name.
NAMESPACE=openstack
OVN_MARKER='# controlplane-ovn.yaml'
CP_MARKER='# controlplane.yaml'

# ---------------------------------------------------------------------------
# usage — Print the usage text to stderr and exit 2.
# ---------------------------------------------------------------------------
usage() {
  echo "usage: hack/ci-quick-start-controlplane.sh up | summary <file>" >&2
  exit 2
}

# ---------------------------------------------------------------------------
# die <message> — Print the message to stderr and exit 1.
# ---------------------------------------------------------------------------
die() {
  echo "$*" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# set_stage <name> — Record the stage `up` is in and announce it.
# ---------------------------------------------------------------------------
set_stage() {
  echo "$1" >"${STATE_DIR}/stage"
  echo "==> $1"
}

# ---------------------------------------------------------------------------
# run_kubectl <stage> <args>... — Run kubectl; on failure print the stage and
# the command below kubectl's own message and exit 1.
# ---------------------------------------------------------------------------
run_kubectl() {
  local stage="$1" rc=0
  shift
  kubectl "$@" || rc=$?
  [ "${rc}" -eq 0 ] || die "ERROR: ${stage}: kubectl $* failed (exit ${rc})"
}

# ---------------------------------------------------------------------------
# extract_block <marker> <doc> — Print the first fenced yaml block of <doc>
# whose first line is <marker>, without the marker. Returns 1 when no block
# starts with it. The reader of tests/unit/deploy/metal_stack_controlplane_test.sh.
# ---------------------------------------------------------------------------
extract_block() {
  awk -v marker="$1" '
    BEGIN { in_block = 0; found = 0 }
    /^```yaml[[:space:]]*$/ {
      # Peek the next line; enter the block only when it is the marker.
      if ((getline next_line) > 0) {
        sub(/[[:space:]]+$/, "", next_line)
        if (next_line == marker) {
          in_block = 1
          found = 1
        }
      }
      next
    }
    in_block && /^```[[:space:]]*$/ { in_block = 0; exit }
    in_block { print }
    END { if (!found) exit 1 }
  ' "$2"
}

# ---------------------------------------------------------------------------
# extract_copy <marker> <copy> — Write the block <marker> heads to <copy>.
# ---------------------------------------------------------------------------
extract_copy() {
  if ! extract_block "$1" "${QUICK_START_DOC}" >"$2" || [ ! -s "$2" ]; then
    die "ERROR: ${QUICK_START_DOC} has no yaml block that starts with '$1'"
  fi
}

# ---------------------------------------------------------------------------
# read_field <copy> <field> — Print .<field> of <copy>; null when it is unset.
# ---------------------------------------------------------------------------
read_field() {
  yq ".$2" "$1" || die "ERROR: $1: yq cannot read $2"
}

# ---------------------------------------------------------------------------
# expect_field <copy> <field> <expected> — Exit 1 unless .<field> of <copy> is
# <expected>.
# ---------------------------------------------------------------------------
expect_field() {
  local value
  value="$(read_field "$1" "$2")"
  [ "${value}" = "$3" ] || die "ERROR: $1: $2 is '${value}', expected '$3'"
}

# ---------------------------------------------------------------------------
# up — Steps 3 to 6 of the page.
# ---------------------------------------------------------------------------
up() {
  local tool knob value
  for tool in kubectl yq jq; do
    command -v "${tool}" >/dev/null 2>&1 || die "ERROR: ${tool} is not on PATH"
  done
  # Checked before any arithmetic, so env text never reaches $(( )).
  for knob in OVN_TIMEOUT MARIADB_TIMEOUT READY_TIMEOUT; do
    value="${!knob}"
    case "${value}" in
      '' | *[!0-9]*) die "ERROR: ${knob} is '${value}', expected a whole number" ;;
    esac
  done

  mkdir -p "${STATE_DIR}"

  local ovn_copy="${STATE_DIR}/controlplane-ovn.yaml"
  local cp_copy="${STATE_DIR}/controlplane.yaml"
  local ovn_name

  set_stage extract
  extract_copy "${OVN_MARKER}" "${ovn_copy}"
  extract_copy "${CP_MARKER}" "${cp_copy}"
  expect_field "${ovn_copy}" kind OVNCentral
  expect_field "${ovn_copy}" metadata.namespace "${NAMESPACE}"
  ovn_name="$(read_field "${ovn_copy}" metadata.name)"
  case "${ovn_name}" in
    '' | null) die "ERROR: ${ovn_copy}: metadata.name is '${ovn_name}', expected a name" ;;
  esac
  expect_field "${cp_copy}" kind ControlPlane
  expect_field "${cp_copy}" metadata.namespace "${NAMESPACE}"
  expect_field "${cp_copy}" metadata.name "${CONTROLPLANE_NAME}"
  if [ "${KIND_HOST_PORT}" = "443" ]; then
    yq -i 'del(.spec.services[].publicEndpoint)' "${cp_copy}"
  fi

  set_stage ovn
  run_kubectl ovn apply -f "${ovn_copy}"
  run_kubectl ovn wait "ovncentral/${ovn_name}" -n "${NAMESPACE}" \
    --for=condition=Ready "--timeout=${OVN_TIMEOUT}s"

  set_stage apply
  run_kubectl apply apply -f "${cp_copy}"

  set_stage mariadb
  local start="${SECONDS}" left
  run_kubectl mariadb wait --for=create mariadb/openstack-db -n "${NAMESPACE}" \
    "--timeout=${MARIADB_TIMEOUT}s"
  left=$((MARIADB_TIMEOUT - (SECONDS - start)))
  [ "${left}" -ge 1 ] || left=1
  run_kubectl mariadb wait mariadb/openstack-db -n "${NAMESPACE}" \
    --for=condition=Ready "--timeout=${left}s"

  set_stage onboard
  local encoded token rc=0
  encoded="$(kubectl get secret openbao-init-keys -n shared-services \
    -o jsonpath='{.data.init-output}')" ||
    die "ERROR: cannot read Secret shared-services/openbao-init-keys"
  # printf is a builtin, so neither value appears in a process's argv.
  token="$(printf '%s' "${encoded}" | jq -Rr '@base64d | fromjson | .root_token' 2>/dev/null)" ||
    token=""
  case "${token}" in
    '' | null) die "ERROR: Secret shared-services/openbao-init-keys carries no root_token" ;;
  esac
  export BAO_TOKEN="${token}"
  unset encoded token
  "${ONBOARD_SCRIPT}" "${NAMESPACE}" "${CONTROLPLANE_NAME}" || rc=$?
  unset BAO_TOKEN
  [ "${rc}" -eq 0 ] ||
    die "ERROR: onboard: ${ONBOARD_SCRIPT} ${NAMESPACE} ${CONTROLPLANE_NAME} failed (exit ${rc})"

  set_stage ready
  run_kubectl ready wait "controlplane/${CONTROLPLANE_NAME}" -n "${NAMESPACE}" \
    --for=condition=Ready "--timeout=${READY_TIMEOUT}s"

  echo "done" >"${STATE_DIR}/stage"
  echo "OK: controlplane/${CONTROLPLANE_NAME} in ${NAMESPACE} is Ready"
}

# The jq programs of summary. A cell is one line with no backtick and every |
# escaped, so a condition message keeps its table row; the message goes into
# a code span, so an @name in it notifies nobody in the public issue.
# shellcheck disable=SC2016 # jq programs, not shell expansions
JQ_CELL='def cell: tostring | gsub("[\r\n]+"; " ") | gsub("`"; "\u0027") | gsub("\\|"; "\\|");
def code: if . == "" then "" else "`" + . + "`" end;'
# shellcheck disable=SC2016
JQ_CONDITIONS="${JQ_CELL}"'
(.status.conditions // []) as $c
| if ($c | length) == 0 then "(no conditions)"
  else "| type | status | reason | message |", "| --- | --- | --- | --- |",
    ($c[] | "| \(.type // "" | cell) | \(.status // "" | cell) | \(.reason // "" | cell) | \(.message // "" | cell | code) |")
  end'
# shellcheck disable=SC2016
JQ_OVN_READY="${JQ_CELL}"'
if (.items | length) == 0 then "(no OVNCentral in openstack)"
else "| name | status | reason | message |", "| --- | --- | --- | --- |",
  (.items[] | ([.status.conditions[]? | select(.type == "Ready")][0] // {}) as $r
    | "| \(.metadata.name | cell) | \($r.status // "" | cell) | \($r.reason // "" | cell) | \($r.message // "" | cell | code) |")
end'

# The file json_table and text_block send stderr to; summary creates it.
errfile=""

# ---------------------------------------------------------------------------
# unavailable <error text> — The placeholder for a read that failed.
# ---------------------------------------------------------------------------
unavailable() {
  echo "(unavailable: ${1%%$'\n'*})"
}

# ---------------------------------------------------------------------------
# json_table <jq program> <kubectl args>... — Print the table the program
# makes of `kubectl get <args> -o json`, or the placeholder.
# ---------------------------------------------------------------------------
json_table() {
  local program="$1" json table
  shift
  if ! json="$(kubectl get "$@" -o json 2>"${errfile}")"; then
    unavailable "$(cat "${errfile}")"
  elif ! table="$(printf '%s' "${json}" | jq -r "${program}" 2>"${errfile}")"; then
    unavailable "$(cat "${errfile}")"
  else
    echo "${table}"
  fi
}

# ---------------------------------------------------------------------------
# text_block <kubectl args>... — Print `kubectl get <args>` in a fenced block,
# or the placeholder.
# ---------------------------------------------------------------------------
text_block() {
  local out
  if out="$(kubectl get "$@" 2>"${errfile}")"; then
    # shellcheck disable=SC2016 # Markdown fences
    printf '```text\n%s\n```\n' "${out}"
  else
    unavailable "$(cat "${errfile}")"
  fi
}

# ---------------------------------------------------------------------------
# summary <file> — Write the report of a failed run.
# ---------------------------------------------------------------------------
summary() {
  local file="$1" stage
  mkdir -p "$(dirname "${file}")"
  errfile="$(mktemp)"
  trap 'rm -f "${errfile}"' EXIT

  if [ -f "${STATE_DIR}/stage" ]; then
    stage="\`$(cat "${STATE_DIR}/stage")\`"
  else
    # shellcheck disable=SC2016 # a Markdown code span
    stage='`deploy-infra` (the bring-up step never started: deploy-infra or an earlier step failed)'
  fi

  {
    echo "## Flux path run summary"
    echo
    echo "Stage: ${stage}"
    echo
    echo "### ControlPlane ${NAMESPACE}/${CONTROLPLANE_NAME}"
    echo
    json_table "${JQ_CONDITIONS}" "controlplane/${CONTROLPLANE_NAME}" -n "${NAMESPACE}"
    echo
    echo "### OVNCentral Ready"
    echo
    json_table "${JQ_OVN_READY}" ovncentrals -n "${NAMESPACE}"
    echo
    echo "### HelmReleases"
    echo
    text_block helmrelease -A
    echo
    echo "### Flux sources"
    echo
    text_block gitrepository,helmrepository,kustomization -n flux-system
  } >"${file}"
  echo "summary written to ${file}"
}

case "${1:-}" in
  up)
    [ "$#" -eq 1 ] || usage
    up
    ;;
  summary)
    [ "$#" -eq 2 ] || usage
    summary "$2"
    ;;
  *) usage ;;
esac
