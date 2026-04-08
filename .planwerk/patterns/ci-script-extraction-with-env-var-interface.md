# Pattern: CI script extraction with env-var interface

**Component**: hack/ci-*.sh, .github/workflows/ci.yaml
**Category**: service-structure
**Applies-When**: Extracting inline shell blocks from GitHub Actions workflow steps into standalone hack/ci-*.sh scripts

## Description

Inline workflow scripts are extracted into hack/ci-*.sh files. The workflow step passes GitHub Actions context (step outputs, built-in vars) as environment variables via the step's env: block. The script reads these via ${VAR:-default} with safe defaults. Required env vars are validated at script entry with ::error:: annotation and exit 1. The script writes outputs to GITHUB_OUTPUT. This keeps the workflow declarative (env mapping) and the script testable (env vars can be set locally).

## Examples

### `.github/workflows/ci.yaml:94-102`

```
      - name: Resolve effective changes
        id: result
        env:
          ALL_OPERATORS: keystone
          FILTER_keystone: ${{ steps.filter.outputs.keystone }}
          FILTER_docs: ${{ steps.filter.outputs.docs }}
          FILTER_helm: ${{ steps.filter.outputs.helm }}
          FILTER_e2e_infra: ${{ steps.filter.outputs.e2e_infra }}
          FILTER_go_common: ${{ steps.filter.outputs.go_common }}
        run: hack/ci-resolve-changes.sh
```

### `hack/ci-resolve-changes.sh:37-40`

```
if [[ -z "${ALL_OPERATORS:-}" ]]; then
  echo "::error::ALL_OPERATORS must be set (space-separated list of operator names)"
  exit 1
fi
```

