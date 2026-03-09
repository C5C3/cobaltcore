# Pattern: Exit code guard for command substitution under set -e

**Component**: tests/container-images/verify_*.sh
**Category**: error-handling
**Applies-When**: Writing bash test scripts with 'set -euo pipefail' that capture docker run output via command substitution

## Description

Every command substitution assignment (var=$(cmd)) where the command can fail must use '|| exit_code=$?' to prevent set -e from aborting the script before assertions run. The local declaration initializes exit_code=0, and an assert_eq check for exit code 0 is placed before any content assertions. This prevents false passes where the test script exits silently instead of recording a FAIL.

## Examples

### `tests/container-images/verify_keystone.sh:25-28`

```
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" keystone-manage --version 2>&1) || exit_code=$?

  assert_eq "keystone-manage --version exits 0" "0" "$exit_code"
```

### `tests/container-images/verify_venv_builder.sh:25-28`

```
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" uv --version 2>&1) || exit_code=$?

  assert_eq "uv --version exits 0" "0" "$exit_code"
```

