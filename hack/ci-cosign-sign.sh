#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-cosign-sign.sh — Sign one image or chart digest with cosign and
# verify the signature against the identity of the workflow that wrote it.
#
# Usage: hack/ci-cosign-sign.sh <repository>@sha256:<digest>
#
# The publish jobs of ci.yaml (merge-operator-images, helm-push,
# helm-push-target-cluster) call it with the digest they have just pushed.
# cosign signs keylessly through Sigstore OIDC: it exchanges the job's GitHub
# OIDC token for a Fulcio certificate whose subject is
# https://github.com/<GITHUB_WORKFLOW_REF>, stores the Sigstore bundle as an
# OCI referrer of the digest (on ghcr.io, which serves no referrers API, under
# a sha256-<digest> tag in the same package), and records it in the public
# Rekor log. The script then runs the cosign verify a consumer runs, against
# that subject and the issuer https://token.actions.githubusercontent.com,
# with cosign's stdout (the signed payloads) discarded.
#
# Both commands are retried on any failure, where hack/ci-helm-push.sh retries
# one named error. A second cosign sign of one digest adds a second bundle, and
# verify accepts either. ghcr.io can answer 404 for a digest pushed seconds
# earlier (see hack/ci-merge-manifest.sh), and the text cosign prints then is
# not on record. A sign that fails on its last attempt exits with cosign's
# status and runs no verify; a verify that fails on its last attempt exits with
# cosign's status.
#
# Required env vars:
#   GITHUB_WORKFLOW_REF — <owner>/<repo>/.github/workflows/<file>@<ref>, set by
#                         the runner; names the certificate identity
#
# Optional env vars:
#   COSIGN_ATTEMPTS — attempts per command (default: 3)
#   COSIGN_DELAY    — seconds before the first retry, tripled for each
#                     further one (default: 5, as in hack/ci-helm-push.sh)

set -euo pipefail

if [[ $# -ne 1 ]] || [[ ! "$1" =~ ^[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
  echo "usage: hack/ci-cosign-sign.sh <repository>@sha256:<digest>" >&2
  exit 2
fi
ref="$1"

if [[ -z "${GITHUB_WORKFLOW_REF:-}" ]]; then
  echo "::error::GITHUB_WORKFLOW_REF is not set; it names the certificate identity the signature is verified against" >&2
  exit 2
fi
issuer="https://token.actions.githubusercontent.com"
identity="https://github.com/${GITHUB_WORKFLOW_REF}"

attempts="${COSIGN_ATTEMPTS:-3}"
delay="${COSIGN_DELAY:-5}"
if [[ ! "${attempts}" =~ ^[1-9][0-9]*$ ]] || [[ ! "${delay}" =~ ^[0-9]+$ ]]; then
  echo "::error::COSIGN_ATTEMPTS must be a positive integer and COSIGN_DELAY a non-negative one" >&2
  exit 2
fi

# retry <name> <command...> runs the command until it exits 0 or the attempts
# are spent, with a ::warning:: before each retry, and returns the status of
# the last run.
retry() {
  local name="$1" attempt=1 pause="${delay}" status
  shift
  while :; do
    status=0
    "$@" || status=$?
    if [[ "${status}" -eq 0 ]] || [[ "${attempt}" -ge "${attempts}" ]]; then
      return "${status}"
    fi
    echo "::warning::${name} attempt ${attempt} of ${ref} failed; retrying in ${pause}s"
    sleep "${pause}"
    pause=$((pause * 3))
    attempt=$((attempt + 1))
  done
}

verify_signature() {
  cosign verify \
    --certificate-oidc-issuer "${issuer}" \
    --certificate-identity "${identity}" \
    "${ref}" >/dev/null
}

status=0
retry "cosign sign" cosign sign --yes "${ref}" || status=$?
if [[ "${status}" -ne 0 ]]; then
  echo "::error::cosign sign of ${ref} failed after ${attempts} attempts" >&2
  exit "${status}"
fi

retry "cosign verify" verify_signature || status=$?
if [[ "${status}" -ne 0 ]]; then
  echo "::error::cosign verify of ${ref} against ${identity} failed after ${attempts} attempts" >&2
  exit "${status}"
fi

echo "Signed ${ref}; verified against ${identity}"
