# Pattern: Composite action with SPDX header, feature ID, and SHA-pinned external actions

**Component**: .github/actions/*/action.yaml
**Category**: configuration
**Applies-When**: Adding a new reusable composite GitHub Action to the repository that encapsulates a sequence of CI steps

## Description

All composite actions follow a consistent structure: (1) SPDX Apache-2.0 header, (2) # CC-NNNN comment with feature ID, (3) name and description fields, (4) inputs with required/default specification, (5) optional outputs, (6) runs: using: composite with steps. All external uses: references are SHA-pinned with version comments (@sha # vN). All run: steps have explicit shell: bash. Inputs passed to shell commands use env: block indirection to prevent expression injection.

## Examples

### `.github/actions/checkout-service-source/action.yaml:1-44`

```
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CC-0055: Composite action encapsulating the duplicated source checkout
# sequence shared by build-service-images and test-service-images jobs.

name: Checkout service source
description: >
  Resolve the upstream source ref for an OpenStack service, check out the
  repository, apply any patches, and apply constraint overrides.

inputs:
  service:
    description: OpenStack service name (e.g. keystone)
    required: true
  release:
    description: Release directory name (e.g. 2025.2)
    required: true

outputs:
  source-ref:
    description: The resolved version string from source-refs.yaml
    value: ${{ steps.source-ref.outputs.source-ref }}

runs:
  using: composite
  steps:
    - name: Install yq
      uses: mikefarah/yq@b534aa9ee5d38001fba3cd8fe254a037e4847b37 # v4.45.4

    - name: Resolve source ref
      id: source-ref
      shell: bash
      env:
        MATRIX_SERVICE: ${{ inputs.service }}
        MATRIX_RELEASE: ${{ inputs.release }}
      run: |
        ref=$(yq ".\"${MATRIX_SERVICE}\"" "releases/${MATRIX_RELEASE}/source-refs.yaml")
        if [ -z "$ref" ] || [ "$ref" = "null" ]; then
          echo "::error::No source-ref found for '${MATRIX_SERVICE}' in releases/${MATRIX_RELEASE}/source-refs.yaml"
          exit 1
        fi
        echo "source-ref=$ref" >> "$GITHUB_OUTPUT"
```

### `.github/actions/setup-docker-registry/action.yaml:1-44`

```
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# CC-0055: Composite action encapsulating the Docker buildx + registry login +
# cosign installation sequence shared across build-images workflow jobs.

name: Setup Docker registry
description: >
  Configure Docker Buildx, authenticate to a container registry, and
  optionally install cosign for image signing.

inputs:
  registry:
    description: Container registry URL
    required: false
    default: ghcr.io
  username:
    description: Registry username
    required: true
  password:
    description: Registry password/token
    required: true
  install-cosign:
    description: Whether to install cosign
    required: false
    default: 'true'

runs:
  using: composite
  steps:
    - name: Set up Docker Buildx
      uses: docker/setup-buildx-action@4d04d5d9486b7bd6fa91e7baf45bbb4f8b9deedd # v4

    - name: Log in to container registry
      uses: docker/login-action@b45d80f862d83dbcd57f89517bcf500b2ab88fb2 # v4
      with:
        registry: ${{ inputs.registry }}
        username: ${{ inputs.username }}
        password: ${{ inputs.password }}

    - name: Install cosign
      if: inputs.install-cosign != 'false'
      uses: sigstore/cosign-installer@faadad0cce49287aee09b3a48701e75088a2c6ad # v4
```

