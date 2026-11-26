#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Publish stable charts and point each stable operator tag at its signed RC index.
set -euo pipefail
stable=$1
commit=$2
images=$3
shift 3
registry=ghcr.io/c5c3
issuer=https://token.actions.githubusercontent.com
identity="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/.github/workflows/ci.yaml@refs/heads/release/${stable%.*}"

output=$(mktemp)
trap 'rm -f "$output"' EXIT
for archive in "$@"; do
  name=$(basename "$archive" "-$stable.tgz")
  if [[ "$name" != target-cluster-access ]]; then
    image="$registry/$name"
    digest=$(jq -er --arg name "$name" '.[$name]' "$images")
    ref="$image@$digest"
    cosign verify --certificate-oidc-issuer "$issuer" --certificate-identity "$identity" "$ref" >/dev/null
    revision=$(docker buildx imagetools inspect "$ref" --raw | jq -er '.annotations["org.opencontainers.image.revision"]')
    [[ "$revision" == "$commit" ]] || { echo "::error::$ref belongs to $revision"; exit 1; }
    docker buildx imagetools create --prefer-index=false --tag "$image:$stable" "$ref"
    promoted=$(docker buildx imagetools inspect "$image:$stable" --format '{{.Manifest.Digest}}')
    [[ "$promoted" == "$digest" ]] || { echo "::error::$image:$stable differs from $ref"; exit 1; }
  fi
  : > "$output"
  GITHUB_OUTPUT="$output" hack/ci-helm-push.sh "$archive" "oci://$registry/charts"
  hack/ci-cosign-sign.sh "$(sed -n 's/^ref=//p' "$output")"
done
