#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify that every script of every chainsaw suite fits into one argument.
#
# Chainsaw runs a script operation as `bash -c <content>`, with the whole
# content as one argument. Linux caps one argument at MAX_ARG_STRLEN, 32
# pages of 4 KiB, and the terminating NUL counts. A script of 131072 bytes
# or more never starts: chainsaw reports
#
#   fork/exec /usr/bin/bash: argument list too long
#
# and the suite fails in its first second. Only the cluster-bound e2e jobs
# run the script, so without this test the failure shows after the image
# builds and the cluster bring-up. The step 1 script of
# tests/e2e/c5c3/full-controlplane-keystone reached the cap twice, which is
# why extraconfig-edit.sh and secret-store-checks.sh live beside that suite
# and are sourced by it.
#
# Usage: bash tests/unit/ci/chainsaw_script_argument_cap_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# MAX_ARG_STRLEN on the 4 KiB pages of the CI runners. The content and its
# terminating NUL have to fit, so the largest script that starts has one byte
# less.
MAX_ARG_STRLEN=131072

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# script_sizes — one "<bytes> <file>" line per script operation of the
# chainsaw files named as arguments, wherever the operation sits (try, catch,
# finally, cleanup). The size is the UTF-8 byte length of script.content,
# which is what the kernel counts; a character count would pass a script
# whose box-drawing comment rules take three bytes each.
script_sizes() {
  yq -o=json -I=0 '{"file": filename, "doc": .}' "$@" \
    | jq -r '.file as $f | .doc | .. | objects | .script? | objects
             | .content | strings | "\(utf8bytelength) \($f)"'
}

# over_cap — the lines of script_sizes whose script does not fit.
over_cap() {
  awk -v cap="$MAX_ARG_STRLEN" '$1 + 1 > cap'
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# The measurement has to count bytes and find a script outside `try`, or the
# repo-wide scan below passes vacuously.
test_measurement_counts_bytes_at_the_cap() {
  echo "Test: the measurement counts bytes and flags the first size that does not fit"

  local tmp fits over sizes
  tmp="$(mktemp -d)"
  # 131071 bytes fit with their NUL; 131072 do not. The fitting script ends in
  # one three-byte character, so a character count would read 131069. The
  # fixtures are JSON, which yq reads as YAML, and the printf builtin writes
  # them: the same cap keeps a string of this size out of yq's arguments and
  # environment.
  fits="$(printf '%*s' $(( MAX_ARG_STRLEN - 4 )) '' | tr ' ' 'x')─"
  over="$(printf '%*s' "$MAX_ARG_STRLEN" '' | tr ' ' 'x')"
  printf '{"spec":{"steps":[{"try":[{"script":{"content":"%s"}}]}]}}\n' "$fits" \
    > "$tmp/fits.yaml"
  printf '{"spec":{"steps":[{"finally":[{"script":{"content":"%s"}}]}]}}\n' "$over" \
    > "$tmp/over.yaml"

  sizes="$(script_sizes "$tmp/fits.yaml" "$tmp/over.yaml")"
  assert_eq "a script that ends in a three-byte character is measured in bytes" \
    "$(( MAX_ARG_STRLEN - 1 )) $tmp/fits.yaml" "$(sed -n 1p <<<"$sizes")"
  assert_eq "a script under finally is measured" \
    "$MAX_ARG_STRLEN $tmp/over.yaml" "$(sed -n 2p <<<"$sizes")"
  assert_eq "only the script that does not fit with its NUL is flagged" \
    "$MAX_ARG_STRLEN $tmp/over.yaml" "$(over_cap <<<"$sizes")"

  rm -rf "$tmp"
}

test_every_suite_script_fits_one_argument() {
  echo "Test: every chainsaw script fits into one argument"

  local files=() sizes offenders
  while IFS= read -r -d '' f; do
    files+=("$f")
  done < <(cd "$PROJECT_ROOT" && find tests -type f -name 'chainsaw-test.yaml' -print0)
  assert_gte "the scan found chainsaw suites (${#files[@]})" "${#files[@]}" 1
  [ "${#files[@]}" -gt 0 ] || return

  sizes="$(cd "$PROJECT_ROOT" && script_sizes "${files[@]}")"
  assert_contains "the scan measured the full-chain suite's step 1 script" \
    "$sizes" " tests/e2e/c5c3/full-controlplane-keystone/chainsaw-test.yaml"

  offenders="$(over_cap <<<"$sizes")"
  if [ -n "$offenders" ]; then
    echo "  Scripts that bash cannot be started with (bytes, file); the cap is $(( MAX_ARG_STRLEN - 1 )) bytes."
    echo "  Move a part into a file beside the suite and source it from the script,"
    echo "  as tests/e2e/c5c3/full-controlplane-keystone/extraconfig-edit.sh does:"
    printf '    %s\n' "$offenders"
  fi
  assert_eq "no chainsaw script passes the $(( MAX_ARG_STRLEN - 1 )) bytes one argument holds" \
    "" "$offenders"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: yq or jq not installed (all checks skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_measurement_counts_bytes_at_the_cap
test_every_suite_script_fits_one_argument

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
