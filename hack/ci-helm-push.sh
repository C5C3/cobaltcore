#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-helm-push.sh — Push one packaged Helm chart to an OCI registry,
# retrying the one ghcr.io failure that a second push repairs.
#
# Usage: hack/ci-helm-push.sh <chart.tgz> <oci://registry/path>
#
# helm push (v4.3.1, the HELM_VERSION pin in ci.yaml) uploads the chart
# manifest by digest and then tags it: oras-go's ExtendedCopy fetches the
# digest it has just uploaded and uploads it again under the chart version.
# ghcr.io now and then answers that fetch with 404, and helm exits with
#
#   Error: failed to perform "Tag" on destination: sha256:<digest>: not found
#
# The manifest is in the registry all the same; only the tag is missing, so
# the chart version keeps pointing at the previous push. helm stamps the
# archive's mtime as the manifest's creation time, so a second push of the
# same archive builds the same manifest, skips the blobs and manifest the
# registry already serves, and tags again. Every other error fails on the
# first attempt.
#
# Optional env vars:
#   HELM_PUSH_ATTEMPTS — attempts in total (default: 3)
#   HELM_PUSH_DELAY    — seconds before the first retry, tripled for each
#                        further one (default: 5, as in hack/ci-merge-manifest.sh)

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <chart.tgz> <oci://registry/path>" >&2
  exit 2
fi
chart="$1"
remote="$2"

if [[ ! -f "${chart}" ]]; then
  echo "::error::chart archive ${chart} does not exist" >&2
  exit 1
fi

attempts="${HELM_PUSH_ATTEMPTS:-3}"
delay="${HELM_PUSH_DELAY:-5}"
if [[ ! "${attempts}" =~ ^[1-9][0-9]*$ ]] || [[ ! "${delay}" =~ ^[0-9]+$ ]]; then
  echo "::error::HELM_PUSH_ATTEMPTS must be a positive integer and HELM_PUSH_DELAY a non-negative one" >&2
  exit 2
fi

tag_not_found='^Error: failed to perform "Tag" on destination: sha256:[0-9a-f]{64}: not found$'

attempt=1
while :; do
  status=0
  output="$(helm push "${chart}" "${remote}" 2>&1)" || status=$?
  printf '%s\n' "${output}"
  if [[ "${status}" -eq 0 ]]; then
    exit 0
  fi
  if ! grep -Eq "${tag_not_found}" <<<"${output}"; then
    exit "${status}"
  fi
  if [[ "${attempt}" -ge "${attempts}" ]]; then
    echo "::error::helm push of ${chart} uploaded the manifest but could not tag it after ${attempts} attempts" >&2
    exit "${status}"
  fi
  echo "::warning::helm push attempt ${attempt} of ${chart} uploaded the manifest, but the registry did not serve it back for tagging; retrying in ${delay}s"
  sleep "${delay}"
  delay=$((delay * 3))
  attempt=$((attempt + 1))
done
