#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that every container image an operator renders comes with its pull
# policy.
#
# Each `Image:` field of a composite literal in the operators' controllers and
# in the shared workload builders (internal/common/deployment, job, database)
# has to be followed, on the next line, by an `ImagePullPolicy:` field resolved
# from the same ImageSpec (EffectivePullPolicy). A container without one falls
# back to the API server default, IfNotPresent for every tag but latest, and a
# node then keeps starting pods from the build it cached under a re-pushed
# tag. An `Image:` field that does not start its line (a one-line literal)
# fails too, so the next-line rule cannot be sidestepped.
#
# Usage: bash tests/unit/ci/image_pull_policy_sites_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# Print path:line: reason for every offending Image: field. Reads the file list
# (NUL-separated) from stdin.
offending_sites() {
  local file
  while IFS= read -r -d '' file; do
    awk -v f="$file" '
      pending {
        if ($0 !~ /^[[:space:]]+ImagePullPolicy:/) {
          print f ":" pending ": Image: is not followed by ImagePullPolicy:"
        }
        pending = 0
      }
      /^[[:space:]]+Image:[[:space:]]/ { pending = NR; next }
      /[ ,{]Image:/ { print f ":" NR ": Image: does not start its line" }
      END {
        if (pending) {
          print f ":" pending ": Image: is not followed by ImagePullPolicy:"
        }
      }
    ' "$file"
  done
}

# Print the scanned files, NUL-separated: every non-test Go file of the
# operators' controller packages and of the three shared builder packages.
scanned_files() {
  (cd "$PROJECT_ROOT" \
    && find operators/*/internal/controller internal/common/deployment \
      internal/common/job internal/common/database \
      -name '*.go' ! -name '*_test.go' -print0 | sort -z)
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# The detector has to catch a missing policy, a one-line literal and an Image:
# on the last line, and let a paired field and a ProxyImage: through, or the
# repo-wide scan below passes vacuously.
test_detector() {
  echo "Test: the detector flags unpaired and inline Image: fields"

  local tmp
  tmp="$(mktemp)"
  printf '%s\n' \
    '	c := corev1.Container{' \
    '		Image:           ref,' \
    '		ImagePullPolicy: pullPolicy,' \
    '		Name:            "ok",' \
    '	}' \
    '	d := corev1.Container{' \
    '		Image: ref,' \
    '		Name:  "missing",' \
    '	}' \
    '	e := corev1.Container{Name: "inline", Image: ref}' \
    '	f := projection{' \
    '		ProxyImage: image,' \
    '	}' \
    '		Image: ref,' \
    >"$tmp"

  local hits
  hits="$(printf '%s\0' "$tmp" | offending_sites | sed "s|^$tmp:||" | cut -d: -f1 | tr '\n' ' ')"
  rm -f "$tmp"

  assert_eq "flags lines 7, 10 and 14" "7 10 14 " "$hits"
}

test_every_site_carries_a_pull_policy() {
  echo "Test: every Image: field is followed by ImagePullPolicy:"

  local files sites found
  files="$(scanned_files | tr '\0' '\n' | grep -c . || true)"
  sites="$(scanned_files | (cd "$PROJECT_ROOT" && xargs -0 cat) | grep -cE '^[[:space:]]+Image:[[:space:]]' || true)"
  assert_gte "the scan reads the controller and builder sources" "$files" 1
  assert_gte "the scan finds Image: fields" "$sites" 1

  found="$(scanned_files | (cd "$PROJECT_ROOT" && offending_sites))"
  if [[ -z "$found" ]]; then
    echo "  PASS: all ${sites} Image: fields carry an ImagePullPolicy:"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: Image: fields without a pull policy (add ImagePullPolicy: <ImageSpec>.EffectivePullPolicy() on the next line)"
    while IFS= read -r line; do
      echo "    $line"
    done <<<"$found"
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_detector
test_every_site_carries_a_pull_policy

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
