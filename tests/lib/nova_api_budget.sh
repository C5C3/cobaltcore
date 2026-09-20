#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check that a tempest Nova CR gives its uWSGI workers room, for the
# tests that pin a compute-stack leg (tests/unit/ci/nova_e2e_matrix_test.sh,
# tests/unit/ci/cinder_e2e_matrix_test.sh).
#
# What a nova-api pod needs to hold its workers is one piece of knowledge, and
# both compute-stack legs deploy the same CR. Kept here so a change to it — a
# different worker count, a re-measured footprint — is edited once instead of
# once per leg, where missing one leaves that leg's CR unpinned.
#
# spec.api.uwsgi.processes raises the worker count above the webhook default,
# and every uWSGI worker is a process carrying its own Python interpreter. With
# spec.api.deployment.resources left unset the defaulting webhook fills in the
# shared 512Mi limit (internal/common/types/workload.go), which four nova-api
# workers do not fit: in run 35500414345 the kernel OOM-killed them at roughly
# 165 MiB resident each, the CR never reported Ready, and all four tempest legs
# spent their full 900s wait before failing with nothing to say why. The floor
# below is tied to the process count, so raising one without the other fails
# here instead of on a runner.

# Minimum memory, in MiB, a Nova CR must grant per uWSGI worker.
NOVA_API_MIB_PER_WORKER=256

# nova_api_mib <quantity>
# Echo a Kubernetes memory quantity in MiB. A suffix this reader does not know
# echoes nothing, so the caller's assertion fails rather than comparing against
# an empty string.
nova_api_mib() {
  case "$1" in
  *Gi) echo $((${1%Gi} * 1024)) ;;
  *Mi) echo "${1%Mi}" ;;
  *) echo "" ;;
  esac
}

# assert_nova_api_fits_its_workers <nova-cr.yaml>
# Assert the CR sizes spec.api.deployment.resources for the worker count it
# asks for. Needs yq, and the caller's PASS/FAIL counters.
assert_nova_api_fits_its_workers() {
  local cr="$1" name procs request limit limit_mib
  name="$(basename "$(dirname "$cr")")/$(basename "$cr")"

  procs=$(yq -r '.spec.api.uwsgi.processes // ""' "$cr")
  assert_not_empty "$name raises the uWSGI process count" "$procs"
  [ -n "$procs" ] || return 0

  # Both are asserted: with only a limit the webhook still defaults the request,
  # and a request it chose is not one this leg decided.
  request=$(yq -r '.spec.api.deployment.resources.requests.memory // ""' "$cr")
  limit=$(yq -r '.spec.api.deployment.resources.limits.memory // ""' "$cr")
  assert_not_empty "$name sets a memory request of its own" "$request"
  assert_not_empty "$name sets a memory limit of its own" "$limit"
  [ -n "$limit" ] || return 0

  limit_mib=$(nova_api_mib "$limit")
  assert_not_empty "$name states that limit in Mi or Gi" "$limit_mib"
  [ -n "$limit_mib" ] || return 0

  assert_gte "$name grants ${NOVA_API_MIB_PER_WORKER}MiB per uWSGI worker" \
    "$limit_mib" "$((procs * NOVA_API_MIB_PER_WORKER))"
}
