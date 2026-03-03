# Pattern: GitHub Actions workflow structure conventions

**Component**: .github/workflows
**Category**: configuration
**Applies-When**: Creating any new GitHub Actions workflow file

## Description

Workflow files use .yaml extension, quote the 'on' key as '"on"' to prevent YAML boolean interpretation, declare top-level 'permissions: contents: read' for least privilege, and include a concurrency group '${{ github.ref }}-${{ github.workflow }}' with cancel-in-progress: true. Runner is ubuntu-latest.

## Examples

### `.github/workflows/ci.yaml:7`

```
"on":
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: true
```

