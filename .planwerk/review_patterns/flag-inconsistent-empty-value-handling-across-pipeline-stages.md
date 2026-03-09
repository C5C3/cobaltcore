# Review Pattern: Flag inconsistent empty-value handling across pipeline stages

**Review-Area**: validation
**Detection-Hint**: When a CI/workflow step validates inputs that are passed to a downstream consumer (e.g., Dockerfile), compare how each stage handles empty or missing values. If the downstream already has conditional guards for empty inputs (e.g., `[ -n "$VAR" ] && ... || true`), the upstream should not hard-fail on empty values. Also look for inconsistency within the same step: if one field (pip_packages) tolerates empty while a sibling field (pip_extras) hard-fails, that asymmetry needs justification.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Compare validation strictness between the CI resolution step and the Dockerfile (or any downstream consumer). Check whether all sibling fields in the same config block are validated with the same policy. If one allows empty and another doesn't, ask why.

## Why it matters

Over-strict validation at the CI level blocks legitimate future configurations (e.g., a service with no Python extras) and forces workarounds like inventing fake values. It creates a hidden coupling where adding a new service requires modifying the workflow, not just the config file.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: The step hard-fails with `::error::` if `pip_extras` resolves to an empty string. This means any future service that genuinely has no Python extras cannot be added to the matrix without either inventing a fake extra or modifying this workflow. The same concern applies to the `apt_packages` guard. For `pip_packages` the step correctly treats an empty list as valid (no error). The same permissive treatment should apply to `pip_extras` and `apt_packages`.
- **What was missed**: Compare validation strictness between the CI resolution step and the Dockerfile (or any downstream consumer). Check whether all sibling fields in the same config block are validated with the same policy. If one allows empty and another doesn't, ask why.
- **Fix**: Removed the mandatory non-empty guards (`::error::` + `exit 1`) for both `pip_extras` and `apt_packages`, letting empty values flow through to the Dockerfile which already handles them via conditional guards.
