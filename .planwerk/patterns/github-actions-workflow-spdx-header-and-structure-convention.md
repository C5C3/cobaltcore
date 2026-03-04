# Pattern: GitHub Actions workflow SPDX header and structure convention

**Component**: .github/workflows/*.yaml
**Category**: configuration
**Applies-When**: Adding a new GitHub Actions workflow file to the repository

## Description

All workflow YAML files use the .yaml extension and start with the SPDX license header (Copyright 2026 SAP SE or an SAP affiliate company, Apache-2.0) followed by a blank comment line and a --- YAML document separator. The trigger key is quoted as '"on"' to prevent YAML boolean interpretation. Top-level permissions are restricted to contents: read (least privilege). The concurrency group pattern is ${{ github.ref }}-${{ github.workflow }} with cancel-in-progress limited to pull_request events (so push-to-main runs are never cancelled). All jobs use runs-on: ubuntu-latest.

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

