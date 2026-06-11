#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-wait-for-image.sh — Wait for a pushed image reference to be resolvable.
# Feature: CC-0055
#
# ghcr.io is eventually consistent: a `docker buildx --push` reports success and
# returns a digest, but a read of that same digest issued seconds later can still
# return 404 ("not found"). When the next build step consumes the freshly-pushed
# image as a `--build-context` source (e.g. venv-builder building FROM
# python-base@<digest>), that 404 fails the whole build. This helper blocks until
# the reference resolves, absorbing the propagation window before the dependent
# build starts.
#
# Required env vars:
#   REF       — Full image reference to wait for (e.g. ghcr.io/c5c3/python-base@sha256:...)
#
# Optional env vars:
#   ATTEMPTS  — Maximum inspect attempts (default: 3)

set -euo pipefail

REF="${REF:?REF is required (e.g. ghcr.io/c5c3/python-base@sha256:...)}"
attempts="${ATTEMPTS:-3}"

# Same backoff as ci-merge-manifest.sh: 5s, then ×3 each retry.
delay=5
for attempt in $(seq 1 "${attempts}"); do
  if docker buildx imagetools inspect "${REF}" --format '{{json .Manifest.Digest}}' >/dev/null 2>&1; then
    echo "Resolved ${REF}"
    exit 0
  fi
  if [[ "${attempt}" -eq "${attempts}" ]]; then
    echo "::error::${REF} not resolvable after ${attempts} attempts"
    exit 1
  fi
  echo "::warning::imagetools inspect attempt ${attempt} for ${REF} failed; retrying in ${delay}s"
  sleep "${delay}"
  delay=$((delay * 3))
done
