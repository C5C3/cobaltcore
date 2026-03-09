# Pattern: Shell test script structure with PASS/FAIL counters

**Component**: tests/scripts/*, tests/container-images/*
**Category**: testing
**Applies-When**: Writing a new shell-based verification test script for infrastructure components (Dockerfiles, scripts, configs)

## Description

Shell test scripts follow a consistent structure: (1) shebang + SPDX header, (2) description comment with CC feature ID and requirement IDs, (3) `set -euo pipefail`, (4) PASS/FAIL integer counters, (5) assert_* helper functions (assert_eq, assert_contains, etc.) that increment counters and print PASS/FAIL with description, (6) named test_* functions each printing a test name, (7) sequential test execution with blank-line separators, (8) summary line `=== Results: $PASS passed, $FAIL failed ===`, (9) `exit 1` if any failures. This pattern is consistently used across 7 test files.

## Examples

### `tests/scripts/test_apply_constraint_overrides.sh:9-35`

```
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
SCRIPT_UNDER_TEST="$PROJECT_ROOT/scripts/apply-constraint-overrides.sh"

PASS=0
FAIL=0
TMPDIR_BASE=$(mktemp -d)

cleanup() {
  rm -rf "$TMPDIR_BASE"
}
trap cleanup EXIT

assert_eq() {
  local description="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected: $expected"
    echo "    actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}
```

### `tests/container-images/verify_python_base.sh:11-29`

```
set -euo pipefail

IMAGE="${1:-c5c3/python-base:3.12-noble}"

PASS=0
FAIL=0

assert_eq() {
  local description="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS: $description"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $description"
    echo "    expected: $expected"
    echo "    actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}
```

