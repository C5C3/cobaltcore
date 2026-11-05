#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-run-tempest.sh — Run Tempest API tests in CI.
#
# CI-specific Tempest execution wrapper that handles port-forwarding,
# config generation, and Docker-based test execution. This is the CI
# counterpart to hack/run-tempest.sh (which handles local execution
# including image building and more complex orchestration).
#
# Runs Tempest in two sequential stestr phases to isolate core tempest.api.*
# from keystone_tempest_plugin.* — they share state via Keystone's dynamic
# service_providers injection, which makes tempest.api.identity.v3.test_tokens.
# TokensV3Test.test_validate_token race against ServiceProvidersTest cleanup
# under parallel execution. Inside each phase, tests still run at the default
# concurrency of 4. See docs/reference/tempest-test-infrastructure.md.
#
# After both phases, any test that failed on the first run is re-run once
# serially (stestr --concurrency 1) to absorb cross-suite race flakes. Tests
# that pass on retry are rewritten in the JUnit report as flakes rather than
# failures. Tests that still fail are left as failures and the job exits 1.
#
# Required env vars:
#   (none — all have sensible defaults for the keystone test scenario)
#
# Optional env vars:
#   SERVICE       — Service under test (default: keystone)
#   CONFIG_DIR    — Directory containing tempest.conf template, include/exclude
#                   lists (default: tests/tempest/${SERVICE}-2026-1)
#   NAMESPACE     — Kubernetes namespace (default: openstack)
#   ADMIN_SECRET  — Secret name holding admin password (default: keystone-admin)
#   OUTPUT_DIR    — Test output directory (default: _output/tempest)
#   TEMPEST_IMAGE    — Tempest container image (default: c5c3/tempest:local)
#   SERVICE_K8S_NAME — K8s Service name for port-forward (default: ${SERVICE}-tempest-2026-1)
#   GLANCE_K8S_NAME  — K8s Service name of a Glance API to port-forward on 9292
#                   (default: empty). When set, the script also forwards
#                   svc/${GLANCE_K8S_NAME} 9292:9292, waits for its /healthcheck,
#                   and add-hosts its cluster-local DNS names to 127.0.0.1 so the
#                   catalog's image endpoint resolves to the forwarded port. Used
#                   by the glance leg (tempest.api.image against a real Glance)
#                   and by the cinder leg, whose volume tests create volumes
#                   from an image and upload volumes back to one.
#   BARBICAN_K8S_NAME — K8s Service name of a Barbican API to port-forward on
#                   9311 (default: empty). The barbican counterpart of
#                   GLANCE_K8S_NAME: same forward, /healthcheck poll and
#                   add-host treatment, so the catalog's key-manager endpoint
#                   resolves to the forwarded port. Used by the barbican leg
#                   (barbican_tempest_plugin.tests.api against a real Barbican).
#   NEUTRON_K8S_NAME — K8s Service name of a Neutron API to port-forward on
#                   9696 (default: empty). The neutron counterpart of
#                   GLANCE_K8S_NAME: same forward, readiness poll and add-host
#                   treatment, so the catalog's network endpoint resolves to the
#                   forwarded port. Neutron answers its root path with the
#                   version document and no /healthcheck, so that is what the
#                   poll asks for. Used by the neutron leg (tempest.api.network
#                   and neutron_tempest_plugin.api against a real Neutron).
#                   A tempest.conf that carries ${NEUTRON_PUBLIC_NETWORK_ID}
#                   (the neutron legs) makes the script resolve the one external
#                   network through this 9696 forward, with a docker run of
#                   TEMPEST_IMAGE, and render its id in place of the
#                   placeholder. Such a leg needs NEUTRON_K8S_NAME; without it
#                   the script exits 1 before any docker call.
#   CINDER_K8S_NAME — K8s Service name of a Cinder API to port-forward on
#                   8776 (default: empty). The cinder counterpart of
#                   GLANCE_K8S_NAME: same forward, /healthcheck poll and
#                   add-host treatment, so the catalog's block-storage endpoint
#                   resolves to the forwarded port. Used by the cinder leg
#                   (tempest.api.volume and cinder_tempest_plugin.api.volume
#                   against a real Cinder). That leg also sets GLANCE_K8S_NAME,
#                   for the Glance its volume tests create images from.
#   NOVA_K8S_NAME   — K8s Service name of a Nova API to port-forward on
#                   8774 (default: empty). The nova counterpart of
#                   GLANCE_K8S_NAME: same forward, readiness poll and add-host
#                   treatment, so the catalog's compute endpoint resolves to the
#                   forwarded port. Nova registers no healthcheck middleware and
#                   therefore serves no /healthcheck, so the poll asks for /,
#                   the path the operator probes. Used by the nova legs
#                   (tempest.api.compute against a real Nova with a fake
#                   compute) and by the cinder legs, whose compute-tagged volume
#                   tests attach a volume to a server.
#   NOVA_CONSOLE_K8S_NAME — K8s Service name of the noVNC console proxy
#                   (<nova>-novncproxy) to port-forward on 6080 (default:
#                   empty). test_novnc_bad_token dials the novncproxy_base_url
#                   nova hands out, and the operator renders that as the
#                   cluster-local Service URL when no gateway is set, so the
#                   name has to resolve to the forwarded port. Only the nova
#                   legs set it.
#   TEMPEST_CONCURRENCY — stestr worker count (default: 4). Must not exceed the
#                   request capacity (replicas × uwsgi.processes) of ANY target
#                   it drives — the Keystone target, and on the glance, barbican,
#                   neutron, cinder and nova legs their service target too. Every
#                   port-forward pins to a single pod, so capacity is raised via
#                   uwsgi.processes in the target CR, not via replicas.
#
# The include list is scope-split into a core (tempest.*) and a plugin
# (<service>_tempest_plugin.*) phase. Every non-comment line must land in exactly
# one phase (unknown prefixes are a hard failure), but ONE phase may be empty —
# a pure tempest.api.image include list (the glance leg) has no plugin phase and
# a pure barbican_tempest_plugin.* one (the barbican leg) has no core phase, so
# hack/tempest/run-tests.sh skips whichever phase is empty.
#
# CI-specific Tempest wrapper script.
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
SERVICE="${SERVICE:-keystone}"
CONFIG_DIR="${CONFIG_DIR:-tests/tempest/${SERVICE}-2026-1}"
NAMESPACE="${NAMESPACE:-openstack}"
ADMIN_SECRET="${ADMIN_SECRET:-keystone-admin}"
OUTPUT_DIR="${OUTPUT_DIR:-_output/tempest}"
TEMPEST_IMAGE="${TEMPEST_IMAGE:-c5c3/tempest:local}"
TEMPEST_CONCURRENCY="${TEMPEST_CONCURRENCY:-4}"

# Derive the service name used in k8s (e.g. keystone-tempest-2026-1).
# Allow override for release-specific CR names (e.g. keystone-tempest-2026-2).
# bare CR name; the historical "-api" suffix was dropped.
SERVICE_K8S_NAME="${SERVICE_K8S_NAME:-${SERVICE}-tempest-2026-1}"
CATALOG_SVC="${SERVICE_K8S_NAME}.${NAMESPACE}.svc.cluster.local"

# Optional service APIs port-forwarded alongside Keystone, one row per leg as
# "<display name>:<env var>:<port>:<readiness path>". Each var is empty in the
# keystone-only scenario, which skips that leg's forward, poll and add-host.
# Neutron serves no /healthcheck; its root path returns the version document
# unauthenticated, so that is its readiness signal. Nova serves none either, so
# its row polls /, the path the operator probes, and so does Placement. A leg
# fills one row per API it needs: the cinder legs take their own API, the Glance
# their volume tests create images from and the compute API on 8774. The nova
# legs take that same compute API and the noVNC console proxy on 6080. Both
# compute-stack legs take Placement on 8778: their catalog carries a placement
# endpoint on a cluster-internal name and their tempest.conf declares the
# service available, so a test that builds the placement client would otherwise
# fail on name resolution rather than skip.
GLANCE_K8S_NAME="${GLANCE_K8S_NAME:-}"
BARBICAN_K8S_NAME="${BARBICAN_K8S_NAME:-}"
NEUTRON_K8S_NAME="${NEUTRON_K8S_NAME:-}"
CINDER_K8S_NAME="${CINDER_K8S_NAME:-}"
NOVA_K8S_NAME="${NOVA_K8S_NAME:-}"
NOVA_CONSOLE_K8S_NAME="${NOVA_CONSOLE_K8S_NAME:-}"
PLACEMENT_K8S_NAME="${PLACEMENT_K8S_NAME:-}"
OPTIONAL_TARGETS=(
  "Glance:GLANCE_K8S_NAME:9292:/healthcheck"
  "Barbican:BARBICAN_K8S_NAME:9311:/healthcheck"
  "Neutron:NEUTRON_K8S_NAME:9696:/"
  "Cinder:CINDER_K8S_NAME:8776:/healthcheck"
  "Nova:NOVA_K8S_NAME:8774:/"
  "NovaConsole:NOVA_CONSOLE_K8S_NAME:6080:/vnc_lite.html"
  "Placement:PLACEMENT_K8S_NAME:8778:/"
)
# The id section 3a resolves for ${NEUTRON_PUBLIC_NETWORK_ID}. It is set by the
# lookup alone, never read from the environment, and stays empty on every leg
# whose tempest.conf does not carry the placeholder.
PUBLIC_NETWORK_ID=""

# ---------------------------------------------------------------------------
# 1. Prepare output directories
# ---------------------------------------------------------------------------
mkdir -p "${OUTPUT_DIR}" "${OUTPUT_DIR}/config"
chmod o+w "${OUTPUT_DIR}"

# ---------------------------------------------------------------------------
# 2. Extract admin password from Kubernetes secret
# ---------------------------------------------------------------------------
ADMIN_PASSWORD_B64=$(kubectl get secret "${ADMIN_SECRET}" -n "${NAMESPACE}" \
  -o jsonpath='{.data.password}' 2>/dev/null) || {
  echo "::error::Secret '${ADMIN_SECRET}' not found in namespace '${NAMESPACE}'"
  exit 1
}
if [[ -z "${ADMIN_PASSWORD_B64}" ]]; then
  echo "::error::Secret '${ADMIN_SECRET}' in namespace '${NAMESPACE}' is missing the 'password' key"
  exit 1
fi
ADMIN_PASSWORD=$(echo "${ADMIN_PASSWORD_B64}" | base64 -d)

# ---------------------------------------------------------------------------
# 3. Set up port-forward and wait for readiness
# ---------------------------------------------------------------------------
# kubectl pins a forward to one pod and exits for good when that pod goes away,
# so every forward's output goes to a log the results artifact retains rather
# than to /dev/null: a forward that dropped mid-run turns every later call into
# a connection error, and the reason is only in that log. Keystone is the one
# every leg opens and every test needs, since each one issues a token first.
kubectl port-forward "svc/${SERVICE_K8S_NAME}" -n "${NAMESPACE}" 5000:5000 \
  >"${OUTPUT_DIR}/port-forward-Keystone.log" 2>&1 &
PF_PID=$!
OPTIONAL_PF_PIDS=()
cleanup() {
  kill "${PF_PID}" 2>/dev/null || true
  local pid
  if [[ ${#OPTIONAL_PF_PIDS[@]} -gt 0 ]]; then
    for pid in "${OPTIONAL_PF_PIDS[@]}"; do
      kill "${pid}" 2>/dev/null || true
    done
  fi
}
trap cleanup EXIT

# Forward every configured optional API alongside Keystone, each into a log of
# its own for the reason above.
OPTIONAL_PF_NAMES=()
for target in "${OPTIONAL_TARGETS[@]}"; do
  IFS=: read -r name var port path <<<"${target}"
  [[ -n "${!var}" ]] || continue
  kubectl port-forward "svc/${!var}" -n "${NAMESPACE}" "${port}:${port}" \
    >"${OUTPUT_DIR}/port-forward-${name}.log" 2>&1 &
  OPTIONAL_PF_PIDS+=("$!")
  OPTIONAL_PF_NAMES+=("${name}:${port}")
done

ready=false
for _ in $(seq 1 10); do
  if curl -sf http://localhost:5000/ >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "${ready}" != "true" ]]; then
  echo "::error::${SERVICE} API at http://localhost:5000 did not become reachable after 10 attempts"
  exit 1
fi

for target in "${OPTIONAL_TARGETS[@]}"; do
  IFS=: read -r name var port path <<<"${target}"
  [[ -n "${!var}" ]] || continue
  target_ready=false
  for _ in $(seq 1 10); do
    if curl -sf "http://localhost:${port}${path}" >/dev/null 2>&1; then
      target_ready=true
      break
    fi
    sleep 1
  done
  if [[ "${target_ready}" != "true" ]]; then
    echo "::error::${name} API at http://localhost:${port} did not become reachable after 10 attempts"
    exit 1
  fi
done

# Point the catalog's service DNS names at the forwarded ports inside the
# container. The keystone names are always present; the glance, barbican,
# neutron, cinder, nova and placement names are added only when the matching
# target is configured so their image, key-manager, network, block-storage,
# compute and placement endpoints resolve to the forwarded 9292, 9311, 9696,
# 8776, 8774 and 8778. The console proxy name is added the same way, for the
# novncproxy_base_url the compute API hands out on 6080. That one is in no
# catalog.
ADD_HOST_ARGS=(
  --add-host "${CATALOG_SVC}:127.0.0.1"
  --add-host "${SERVICE_K8S_NAME}.${NAMESPACE}.svc:127.0.0.1"
)
for target in "${OPTIONAL_TARGETS[@]}"; do
  IFS=: read -r name var port path <<<"${target}"
  [[ -n "${!var}" ]] || continue
  ADD_HOST_ARGS+=(--add-host "${!var}.${NAMESPACE}.svc.cluster.local:127.0.0.1")
  ADD_HOST_ARGS+=(--add-host "${!var}.${NAMESPACE}.svc:127.0.0.1")
done

# ---------------------------------------------------------------------------
# 3a. Resolve the external network
# ---------------------------------------------------------------------------
# A tempest.conf that carries ${NEUTRON_PUBLIC_NETWORK_ID} (the neutron legs)
# gets the id of the flat external network its 05-external-network-job.yaml
# created. Neutron assigns network ids itself and rejects one a client picks,
# so the template cannot pin the id the way the nova legs pin their image ids.
# The lookup asks for the one external network rather than for a name, so the
# name lives in the Job alone. It runs before any suite starts, while that
# network is the only external one: the suites create and delete their own
# later.
#
# The tempest image's openstack CLI runs on the host network, where the 5000
# and 9696 forwards listen, with the add-hosts above so the catalog's network
# endpoint resolves to the 9696 forward. The password reaches the container
# through its environment and never appears on the docker command line. Stderr
# goes to a log of its own, which the results artifact keeps, so a Python
# warning cannot land among the ids. Every other leg skips this section and
# makes no docker call here.
PUBLIC_NETWORK_LOG="${OUTPUT_DIR}/public-network-lookup.log"
UUID_RE='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
if grep -qF "\${NEUTRON_PUBLIC_NETWORK_ID}" "${CONFIG_DIR}/tempest.conf"; then
  if [[ -z "${NEUTRON_K8S_NAME}" ]]; then
    echo "::error::${CONFIG_DIR}/tempest.conf names \${NEUTRON_PUBLIC_NETWORK_ID}, but NEUTRON_K8S_NAME is empty; the external network is looked up through the Neutron port-forward"
    exit 1
  fi

  LOOKUP_RC=0
  LOOKUP_OUT=$(OS_PASSWORD="${ADMIN_PASSWORD}" docker run --rm \
    --network host \
    --cap-drop=ALL \
    --security-opt no-new-privileges:true \
    "${ADD_HOST_ARGS[@]}" \
    -e OS_AUTH_URL=http://localhost:5000/v3 \
    -e OS_IDENTITY_API_VERSION=3 \
    -e OS_USERNAME=admin \
    -e OS_USER_DOMAIN_NAME=Default \
    -e OS_PROJECT_NAME=admin \
    -e OS_PROJECT_DOMAIN_NAME=Default \
    -e OS_REGION_NAME=RegionOne \
    -e OS_PASSWORD \
    "${TEMPEST_IMAGE}" \
    openstack network list --external --format value -c ID \
    2>"${PUBLIC_NETWORK_LOG}") || LOOKUP_RC=$?
  if [[ "${LOOKUP_RC}" -ne 0 ]]; then
    echo "::error::Looking up the external network failed (exit ${LOOKUP_RC}); see ${PUBLIC_NETWORK_LOG}"
    tail -n 1 "${PUBLIC_NETWORK_LOG}"
    exit 1
  fi

  mapfile -t PUBLIC_NETWORK_IDS < <(grep . <<<"${LOOKUP_OUT}")
  if [[ ${#PUBLIC_NETWORK_IDS[@]} -eq 0 ]]; then
    echo "::error::No external network exists; the neutron leg's 05-external-network-job.yaml creates one before this step"
    exit 1
  fi
  if [[ ${#PUBLIC_NETWORK_IDS[@]} -gt 1 ]]; then
    echo "::error::Expected one external network, found ${#PUBLIC_NETWORK_IDS[@]}: ${PUBLIC_NETWORK_IDS[*]}"
    exit 1
  fi
  if [[ ! "${PUBLIC_NETWORK_IDS[0]}" =~ ${UUID_RE} ]]; then
    echo "::error::The external network lookup returned '${PUBLIC_NETWORK_IDS[0]}', not a network id"
    exit 1
  fi
  PUBLIC_NETWORK_ID="${PUBLIC_NETWORK_IDS[0]}"
  echo "External network for Tempest: ${PUBLIC_NETWORK_ID}"
fi

# ---------------------------------------------------------------------------
# 4. Generate Tempest config from template
# ---------------------------------------------------------------------------
# Escape sed metacharacters in the password to prevent substitution failures
# if ADMIN_PASSWORD contains \, &, |, or / (review #2 comment 4).
# Escape backslashes first in their own pass, then the remaining metacharacters
# with a class that has no backslash, so an arbitrary password is rendered
# literally (a single [&/\|] class makes \ ambiguous in GNU sed).
ADMIN_PASSWORD_ESCAPED=$(printf '%s\n' "${ADMIN_PASSWORD}" \
  | sed -e 's/\\/\\\\/g' -e 's/[&|/]/\\&/g')
# Replace both FQDN and short DNS forms of the service URL (review #2 comment 6).
sed -e "s|${SERVICE_K8S_NAME}\\.${NAMESPACE}\\.svc\\.cluster\\.local:5000|localhost:5000|" \
    -e "s|${SERVICE_K8S_NAME}\\.${NAMESPACE}\\.svc:5000|localhost:5000|" \
    -e "s|\${KEYSTONE_ADMIN_PASSWORD}|${ADMIN_PASSWORD_ESCAPED}|" \
    -e "s|\${NEUTRON_PUBLIC_NETWORK_ID}|${PUBLIC_NETWORK_ID}|" \
    "${CONFIG_DIR}/tempest.conf" > "${OUTPUT_DIR}/config/tempest.conf"

[[ -f "${CONFIG_DIR}/exclude-tests.txt" ]] && cp "${CONFIG_DIR}/exclude-tests.txt" "${OUTPUT_DIR}/config/"

# Stage the retry helpers and shared in-container runner next to tempest.conf
# so they are available under /etc/tempest/ inside the container.
install -m 0755 "${SCRIPT_DIR}/tempest/extract-failed.py" \
  "${OUTPUT_DIR}/config/extract-failed.py"
install -m 0755 "${SCRIPT_DIR}/tempest/merge-retry-junit.py" \
  "${OUTPUT_DIR}/config/merge-retry-junit.py"
install -m 0755 "${SCRIPT_DIR}/tempest/run-tests.sh" \
  "${OUTPUT_DIR}/config/run-tests.sh"

# ---------------------------------------------------------------------------
# 5. Scope-split the include list into two phase files.
# ---------------------------------------------------------------------------
# Run core tempest.api.* and keystone_tempest_plugin.* in two sequential stestr
# invocations so they never share workers. Keystone injects the current list of
# federation service providers into every token issue/validate response, so
# ServiceProvidersTest cleanups racing against TokensV3Test.test_validate_token
# make the latter flaky under parallel execution. Upstream's keystone-tempest
# gate avoids this by only running keystone_tempest_plugin.* in its job; we run
# both suites and therefore isolate them by time.
PHASES_DIR="${OUTPUT_DIR}/config/phases"
mkdir -p "${PHASES_DIR}"

if [[ ! -f "${CONFIG_DIR}/include-tests.txt" ]]; then
  echo "::error::${CONFIG_DIR}/include-tests.txt is required for scope-split execution"
  exit 1
fi

grep -E '^tempest\.' "${CONFIG_DIR}/include-tests.txt" \
  > "${PHASES_DIR}/phase-1-core.txt" || true
grep -E '^[a-z0-9_]+_tempest_plugin\.' "${CONFIG_DIR}/include-tests.txt" \
  > "${PHASES_DIR}/phase-2-plugin.txt" || true

# Invariant: every non-comment, non-empty line in include-tests.txt must land
# in exactly one phase file. Unknown prefixes would otherwise be silently
# dropped and leave tests unrun.
TOTAL_PATTERNS=$(grep -cE '^[^#[:space:]]' \
  "${CONFIG_DIR}/include-tests.txt" || true)
PHASE1_COUNT=$(wc -l < "${PHASES_DIR}/phase-1-core.txt" | tr -d ' ')
PHASE2_COUNT=$(wc -l < "${PHASES_DIR}/phase-2-plugin.txt" | tr -d ' ')
COVERED=$((PHASE1_COUNT + PHASE2_COUNT))

if [[ "${COVERED}" -ne "${TOTAL_PATTERNS}" ]]; then
  echo "::error::Scope-split does not cover include-tests.txt: ${TOTAL_PATTERNS} patterns, ${COVERED} covered (phase1=${PHASE1_COUNT}, phase2=${PHASE2_COUNT}). Every non-comment line must match '^tempest\\.' or '^[a-z0-9_]+_tempest_plugin\\.'."
  exit 1
fi
# One phase may be empty — a pure tempest.api.image include list (the glance
# leg) has no plugin phase and a pure barbican_tempest_plugin.* one (the
# barbican leg) has no core phase, and run-tests.sh skips an empty phase — but
# both empty means the include list selects nothing runnable.
if [[ "${PHASE1_COUNT}" -eq 0 && "${PHASE2_COUNT}" -eq 0 ]]; then
  echo "::error::Scope-split produced two empty phases (phase1=${PHASE1_COUNT}, phase2=${PHASE2_COUNT}). include-tests.txt selects no runnable patterns."
  exit 1
fi

# ---------------------------------------------------------------------------
# 6. Run Tempest in container
# ---------------------------------------------------------------------------
# Resolve workspace root for volume mounts. In GitHub Actions this is
# GITHUB_WORKSPACE; locally fall back to the git repo root.
WORKSPACE_ROOT="${GITHUB_WORKSPACE:-$(git rev-parse --show-toplevel)}"

TEMPEST_RC=0
docker run --rm \
  --network host \
  "${ADD_HOST_ARGS[@]}" \
  -v "${WORKSPACE_ROOT}/${OUTPUT_DIR}/config:/etc/tempest:ro" \
  -v "${WORKSPACE_ROOT}/${OUTPUT_DIR}:/output" \
  -e "TEMPEST_CONCURRENCY=${TEMPEST_CONCURRENCY}" \
  -e "TEMPEST_GROUP_START=::group::" \
  -e "TEMPEST_GROUP_END=::endgroup::" \
  -e "TEMPEST_ERROR_PREFIX=::error::" \
  "${TEMPEST_IMAGE}" \
  bash /etc/tempest/run-tests.sh || TEMPEST_RC=$?

# ---------------------------------------------------------------------------
# 7. Report any forward that did not survive the run
# ---------------------------------------------------------------------------
# A forward whose pod was restarted or evicted exits, and every call the suite
# made through it afterwards failed with a connection error that looks like a
# broken service. Name it here so the job log says which API went away instead
# of leaving the reader to infer it from a wall of connection errors. Keystone
# comes first: every test issues a token through it, so a 5000 that went away
# fails the rest of the suite, and the keystone legs forward nothing else.
if ! kill -0 "${PF_PID}" 2>/dev/null; then
  echo "::error::The Keystone port-forward on 5000 died during the run; failures against localhost:5000 below are not service failures. See ${OUTPUT_DIR}/port-forward-Keystone.log"
fi
# The index guard is for bash 3.2, where ${!array[@]} on an empty array is an
# unbound variable under `set -u` — the keystone legs forward nothing optional.
if [[ ${#OPTIONAL_PF_PIDS[@]} -gt 0 ]]; then
  for i in "${!OPTIONAL_PF_PIDS[@]}"; do
    kill -0 "${OPTIONAL_PF_PIDS[$i]}" 2>/dev/null && continue
    IFS=: read -r name port <<<"${OPTIONAL_PF_NAMES[$i]}"
    echo "::error::The ${name} port-forward on ${port} died during the run; failures against localhost:${port} below are not service failures. See ${OUTPUT_DIR}/port-forward-${name}.log"
  done
fi

exit "${TEMPEST_RC}"
