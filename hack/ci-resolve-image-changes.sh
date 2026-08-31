#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-resolve-image-changes.sh — Resolve which container images a pull
# request builds.
#
# Reads the paths-filter outputs of the build-images workflow (passed as
# FILTER_* env vars) and decides which services enter the build matrix and
# which of the four release-independent images are built. A service is built
# when its own sources changed; an image is built when its own sources changed.
#
# Three inputs override that and build everything, because everything is built
# from them: any event that is not a pull request, a change to the workflow's
# own plumbing (the workflow file, the composite actions, the build scripts),
# and a change to the base images or the release configuration. The base class
# also builds the Tempest image, which is built FROM python-base and
# venv-builder and reads releases/<release>/. The OVN, federation proxy and
# backup shifter images build FROM ubuntu:noble and read neither, so they come
# from their own filters only.
#
# Required env vars:
#   EVENT_NAME   — github.event_name
#   ALL_SERVICES — Space-separated list of every service in the build matrix
#                  (the union of the keys in releases/*/source-refs.yaml)
#
# Optional env vars:
#   FILTER_svc_<service> — One per name in ALL_SERVICES
#   FILTER_base          — Base images, release configs, build scripts, overrides
#   FILTER_tempest       — Tempest image sources
#   FILTER_ovn           — OVN image sources
#   FILTER_proxy         — Keystone federation proxy image sources
#   FILTER_shifter       — Backup shifter image sources
#   FILTER_plumbing      — The workflow, its composite actions and hack scripts
#   Anything but the literal "true" is false, including the empty string the
#   skipped filter step yields on push and workflow_dispatch.
#   GITHUB_OUTPUT        — GitHub Actions output file (default /dev/null)
#
# Outputs written to GITHUB_OUTPUT and printed to stdout:
#   services      — "all", or a space-separated subset of ALL_SERVICES in
#                   ALL_SERVICES order, or the empty string
#   has-services  — true when services is "all" or non-empty
#   build-tempest — true or false
#   build-ovn     — true or false
#   build-proxy   — true or false
#   build-shifter — true or false
#
# To add a new service: add a svc_<service> paths filter, its FILTER_svc_<service>
# env line, and the name to ALL_SERVICES, all in the changes job of
# .github/workflows/build-images.yaml.
# tests/unit/ci/build_images_services_lockstep_test.sh fails when a step is missed.

set -euo pipefail

# ---------------------------------------------------------------------------
# Guards and defaults
# ---------------------------------------------------------------------------
if [[ -z "${EVENT_NAME:-}" ]]; then
  echo "::error::EVENT_NAME must be set (the GitHub event name)"
  exit 1
fi
if [[ -z "${ALL_SERVICES:-}" ]]; then
  echo "::error::ALL_SERVICES must be set (space-separated list of service names)"
  exit 1
fi

GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# filter_on <name> — true when FILTER_<name> is exactly "true".
filter_on() {
  local var="FILTER_$1"
  [[ "${!var:-false}" == "true" ]]
}

# emit <name> <value> — writes one output line, and echoes it so the script is
# readable when run by hand.
emit() {
  echo "$1=$2" >>"$GITHUB_OUTPUT"
  echo "$1=$2"
}

# yesno <command...> — normalises an exit status into true/false.
yesno() { if "$@"; then echo true; else echo false; fi; }

# ---------------------------------------------------------------------------
# Resolve
# ---------------------------------------------------------------------------
build_everything=false
if [[ "${EVENT_NAME}" != "pull_request" ]] || filter_on plumbing; then
  build_everything=true
fi

if [[ "${build_everything}" == "true" ]]; then
  services="all"
  build_tempest=true
  build_ovn=true
  build_proxy=true
  build_shifter=true
else
  build_tempest=$(yesno filter_on tempest)
  build_ovn=$(yesno filter_on ovn)
  build_proxy=$(yesno filter_on proxy)
  build_shifter=$(yesno filter_on shifter)

  services=""
  if filter_on base; then
    services="all"
    build_tempest=true
  else
    for service in ${ALL_SERVICES}; do
      if filter_on "svc_${service}"; then
        services="${services:+${services} }${service}"
      fi
    done
  fi
fi

has_services=$(yesno test -n "${services}")

# ---------------------------------------------------------------------------
# Emit
# ---------------------------------------------------------------------------
emit services "${services}"
emit has-services "${has_services}"
emit build-tempest "${build_tempest}"
emit build-ovn "${build_ovn}"
emit build-proxy "${build_proxy}"
emit build-shifter "${build_shifter}"
