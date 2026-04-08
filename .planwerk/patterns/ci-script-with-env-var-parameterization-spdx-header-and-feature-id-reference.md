# Pattern: CI script with env-var parameterization, SPDX header, and feature ID reference

**Component**: hack/ci-*.sh
**Category**: configuration
**Applies-When**: Adding a new CI helper script under hack/ that encapsulates a sequence of CI steps into a reusable, locally-runnable script

## Description

CI helper scripts follow a consistent structure: (1) #!/usr/bin/env bash shebang, (2) SPDX Apache-2.0 header, (3) script name and feature ID in a comment block, (4) required env vars documented with names and descriptions, (5) optional env vars documented with defaults, (6) REQ-nnn references, (7) set -euo pipefail, (8) SCRIPT_DIR/REPO_ROOT computation via BASH_SOURCE for path independence, (9) required vars validated with ${VAR:?message} syntax, (10) optional vars with ${VAR:-default} defaults, (11) numbered section comments (# 1. Description, # 2. Description, etc.) with horizontal rules. All scripts are executable (chmod +x) and designed to work both in CI and locally.

## Examples

### `hack/ci-build-service-image.sh:1-33`

```
#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-build-service-image.sh — Build an OpenStack service container image.
# Feature: CC-0050
#
# Required env vars:
#   OPERATOR      — OpenStack service name (e.g. keystone)
#   IMAGE_PREFIX  — Container image prefix (e.g. ghcr.io/c5c3)
#
# Optional env vars:
#   RELEASE       — Release directory name (default: 2025.2)
#
# REQ-002: Reusable service image build script.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OPERATOR="${OPERATOR:?OPERATOR is required (e.g. keystone)}"
IMAGE_PREFIX="${IMAGE_PREFIX:?IMAGE_PREFIX is required (e.g. ghcr.io/c5c3)}"
RELEASE="${RELEASE:-2025.2}"
```

### `hack/ci-deploy-operator.sh:1-31`

```
#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-deploy-operator.sh — Deploy an operator into a kind cluster.
# Feature: CC-0050
#
# Required env vars:
#   OPERATOR    — Operator name (e.g. keystone)
#   IMAGE_REPO  — Full image repository (e.g. ghcr.io/c5c3/keystone-operator)
#
# Optional env vars:
#   IMAGE_TAG   — Image tag (default: dev)
#
# REQ-003: Reusable operator deployment script.
# REQ-007: set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

OPERATOR="${OPERATOR:?OPERATOR is required (e.g. keystone)}"
IMAGE_REPO="${IMAGE_REPO:?IMAGE_REPO is required (e.g. ghcr.io/c5c3/keystone-operator)}"
IMAGE_TAG="${IMAGE_TAG:-dev}"

CHART_PATH="operators/${OPERATOR}/helm/${OPERATOR}-operator"
```

