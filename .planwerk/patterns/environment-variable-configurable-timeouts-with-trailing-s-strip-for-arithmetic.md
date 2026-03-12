# Pattern: Environment-variable-configurable timeouts with trailing-s strip for arithmetic

**Component**: hack/deploy-infra.sh
**Category**: configuration
**Applies-When**: Writing a shell script wait function that accepts a human-readable timeout string (e.g., '600s') from an environment variable and needs to use it in both kubectl --timeout and bash arithmetic comparisons

## Description

Wait helper functions accept a timeout string with trailing 's' suffix (e.g., '600s') from environment variables, strip the suffix for arithmetic comparisons (`timeout_secs="${timeout%s}"`), and use the original value for kubectl --timeout which accepts the 's' suffix natively. This avoids maintaining two separate timeout variables and follows the convention established by kubectl's timeout format.

## Examples

### `hack/deploy-infra.sh:40-42`

```
wait_for_helmreleases() {
  local timeout="${HELM_RELEASE_TIMEOUT}"
  # Strip trailing 's' for arithmetic if present.
  local timeout_secs="${timeout%s}"
```

### `hack/deploy-infra.sh:101-103`

```
wait_for_externalsecrets() {
  local timeout="${EXTERNAL_SECRET_TIMEOUT}"
  local timeout_secs="${timeout%s}"
```

