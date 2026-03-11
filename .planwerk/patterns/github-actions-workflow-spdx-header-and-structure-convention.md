# Pattern: GitHub Actions workflow SPDX header and structure convention

**Component**: .github/workflows/*.yaml
**Category**: configuration
**Applies-When**: Adding a new GitHub Actions workflow file to the repository; Adding or updating a GitHub Actions workflow that references external actions

## Description

All workflow YAML files use the .yaml extension and start with the SPDX license header (Copyright 2026 SAP SE or an SAP affiliate company, Apache-2.0) followed by a blank comment line and a --- YAML document separator. The trigger key is quoted as '"on"' to prevent YAML boolean interpretation. Top-level permissions are restricted to contents: read (least privilege). The concurrency group pattern is ${{ github.ref }}-${{ github.workflow }} with cancel-in-progress limited to pull_request events (so push-to-main runs are never cancelled). All jobs use runs-on: ubuntu-latest.

All action references use the full SHA hash with a trailing version comment (e.g., actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6). This prevents supply chain attacks via mutable tag retargeting and provides audit traceability. The version comment preserves human readability. This extends the existing github-actions-workflow-spdx-header-and-structure-convention pattern.

## Examples

### `.github/workflows/ci.yaml:1`

```
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
name: CI

"on":
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```
### `.github/workflows/ci.yaml:25`

```
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
```

### `.github/workflows/build-images.yaml:59`

```
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
```


