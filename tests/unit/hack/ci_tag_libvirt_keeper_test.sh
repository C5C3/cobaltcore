#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify hack/ci-tag-libvirt-keeper.sh with a recording docker stub first on
# PATH and throwaway git repositories:
#   - a tag the registry does not have is minted once on DIGEST, read back,
#     and reported as tag=<version>-r<N> and minted=true;
#   - a tag the registry has is left where it is (minted=false);
#   - a registry that answers anything but "not found" mints nothing;
#   - a shallow clone, a history without a commit under images/libvirt and a
#     directory that is no repository fail before docker runs;
#   - a failing docker run and a version outside <upstream>-<n>ubuntu<m> fail
#     before imagetools runs;
#   - IMAGE or DIGEST unset, and a DIGEST that is no sha256 digest, fail
#     before anything runs;
#   - a failing create and a read-back that names another digest fail;
#   - a read-back that answers "not found" is retried after RETRY_DELAY, and
#     the delay triples per read;
#   - the keep pattern of hack/ghcr-prune-stale-versions.py and Renovate's
#     allowedVersions are the script's version shape plus -r<N>.
#
# Follows the project-native bash test pattern (tests/lib/assertions.sh),
# mirroring tests/unit/hack/ci_build_service_image_test.sh.
#
# Usage: bash tests/unit/hack/ci_tag_libvirt_keeper_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
KEEPER_SH="$PROJECT_ROOT/hack/ci-tag-libvirt-keeper.sh"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
DOCKER_LOG="$TMP_DIR/docker.log"
SLEEP_LOG="$TMP_DIR/sleep.log"
INSPECT_COUNT="$TMP_DIR/inspect.count"
OUTPUT_FILE="$TMP_DIR/github_output"
STDERR_FILE="$TMP_DIR/stderr"

IMAGE="ghcr.io/c5c3/libvirt"
DIGEST="sha256:$(printf '%.0sa' {1..64})"
OTHER_DIGEST="sha256:$(printf '%.0sb' {1..64})"
VERSION="10.0.0-2ubuntu8.19"

# Two commits under images/libvirt and one elsewhere: the revision is 2.
FIXTURE="$TMP_DIR/fixture"
# A history whose only commit is outside images/libvirt.
NO_LIBVIRT="$TMP_DIR/no-libvirt"
SHALLOW="$TMP_DIR/shallow"
NOT_A_REPO="$TMP_DIR/not-a-repo"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

make_stubs() {
  mkdir -p "$STUB_DIR"

  # docker: records its argv, one line per call.
  #   run                  prints STUB_VERSION, or fails with STUB_RUN_RC
  #   imagetools inspect   the first call answers per STUB_FIRST_INSPECT
  #                        (absent: not found; present: OTHER_DIGEST; error:
  #                        a 502), the next STUB_READBACK_FAILS calls with not
  #                        found, every later one with STUB_READBACK_DIGEST
  #   imagetools create    exits STUB_CREATE_RC
  cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
case "$1" in
  run)
    if [ "${STUB_RUN_RC:-0}" -ne 0 ]; then
      echo "docker: Error response from daemon: stub run failure." >&2
      exit "$STUB_RUN_RC"
    fi
    printf '%s' "$STUB_VERSION"
    exit 0
    ;;
esac
case "$2 $3" in
  "imagetools inspect")
    count=$(($(cat "$INSPECT_COUNT" 2>/dev/null || echo 0) + 1))
    echo "$count" >"$INSPECT_COUNT"
    if [ "$count" -eq 1 ]; then
      case "$STUB_FIRST_INSPECT" in
        absent)
          echo "ERROR: $4: not found" >&2
          exit 1
          ;;
        present)
          echo "\"$OTHER_DIGEST\""
          exit 0
          ;;
        error)
          echo "ERROR: unexpected status 502 Bad Gateway" >&2
          exit 1
          ;;
      esac
    fi
    if [ "$count" -le $((1 + ${STUB_READBACK_FAILS:-0})) ]; then
      echo "ERROR: $4: not found" >&2
      exit 1
    fi
    echo "\"$STUB_READBACK_DIGEST\""
    exit 0
    ;;
  "imagetools create")
    exit "${STUB_CREATE_RC:-0}"
    ;;
esac
echo "docker stub: unexpected call: $*" >&2
exit 99
STUB
  chmod +x "$STUB_DIR/docker"

  # sleep: records its argument, one line per call, and returns at once.
  cat >"$STUB_DIR/sleep" <<'STUB'
#!/bin/bash
echo "$*" >>"$SLEEP_LOG"
STUB
  chmod +x "$STUB_DIR/sleep"
}

# fixture_commit <dir> <path> — append a line to <path> and commit it.
fixture_commit() {
  local dir="$1" path="$2"
  mkdir -p "$(dirname "$dir/$path")"
  echo "change" >>"$dir/$path"
  git -C "$dir" add -A
  git -C "$dir" -c user.name=test -c user.email=test@example.invalid \
    -c commit.gpgsign=false commit -q -m "change $path"
}

make_repos() {
  git init -q "$FIXTURE"
  fixture_commit "$FIXTURE" images/libvirt/Dockerfile
  fixture_commit "$FIXTURE" README.md
  fixture_commit "$FIXTURE" images/libvirt/Dockerfile

  git init -q "$NO_LIBVIRT"
  fixture_commit "$NO_LIBVIRT" README.md

  # git ignores --depth for a plain local path; the file:// URL keeps it.
  git clone -q --depth 1 "file://$FIXTURE" "$SHALLOW"

  mkdir -p "$NOT_A_REPO"
}

# run_keeper [VAR=value | unset:VAR ...]
# Runs the script with the stub first on PATH and fresh logs. By default IMAGE,
# DIGEST, the fixture repository, RETRY_DELAY=0, a fresh GITHUB_OUTPUT, the
# version VERSION, an absent tag and a read-back of DIGEST; the arguments
# override that. Stores stdout in STDOUT, stderr in STDERR, the exit status in
# RC, the recorded docker calls in DOCKER_CALLS, the recorded sleep arguments
# in SLEEPS and the output file in GH_OUTPUT.
run_keeper() {
  RC=0
  rm -f "$DOCKER_LOG" "$SLEEP_LOG" "$INSPECT_COUNT" "$OUTPUT_FILE"
  : >"$OUTPUT_FILE"
  STDOUT="$(
    export IMAGE DIGEST OTHER_DIGEST DOCKER_LOG SLEEP_LOG INSPECT_COUNT
    export LIBVIRT_REPO_DIR="$FIXTURE" RETRY_DELAY=0 GITHUB_OUTPUT="$OUTPUT_FILE" \
      STUB_VERSION="$VERSION" STUB_FIRST_INSPECT=absent STUB_READBACK_DIGEST="$DIGEST"
    unset STUB_RUN_RC STUB_CREATE_RC STUB_READBACK_FAILS
    # The temporary directory is the ceiling, so a directory in it that is no
    # repository is never read as part of one further up.
    export GIT_CEILING_DIRECTORIES="$TMP_DIR"
    local assignment
    for assignment in "$@"; do
      case "$assignment" in
        unset:*) unset "${assignment#unset:}" ;;
        *) export "${assignment?}" ;;
      esac
    done
    PATH="$STUB_DIR:$PATH" bash "$KEEPER_SH" 2>"$STDERR_FILE"
  )" || RC=$?
  STDERR="$(cat "$STDERR_FILE")"
  DOCKER_CALLS=""
  if [ -f "$DOCKER_LOG" ]; then
    DOCKER_CALLS="$(cat "$DOCKER_LOG")"
  fi
  SLEEPS=""
  if [ -f "$SLEEP_LOG" ]; then
    SLEEPS="$(cat "$SLEEP_LOG")"
  fi
  GH_OUTPUT="$(cat "$OUTPUT_FILE")"
}

# calls_of <pattern> — the number of recorded docker calls containing the
# fixed string <pattern>.
calls_of() {
  printf '%s\n' "$DOCKER_CALLS" | grep -cF -- "$1" || true
}

make_stubs
make_repos

# ---------------------------------------------------------------------------
# Test 1: an absent tag is minted once on DIGEST
# ---------------------------------------------------------------------------
test_absent_tag_is_minted() {
  echo "Test: a tag the registry does not have is minted once on DIGEST and read back"

  run_keeper

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "imagetools create runs once" "1" "$(calls_of "imagetools create")"
  assert_contains "the tag is <version>-r2 on DIGEST" "$DOCKER_CALLS" \
    "docker buildx imagetools create -t ${IMAGE}:${VERSION}-r2 ${IMAGE}@${DIGEST}"
  assert_contains "the version is read from the linux/amd64 variant of DIGEST" "$DOCKER_CALLS" \
    "docker run --rm --platform linux/amd64 ${IMAGE}@${DIGEST} dpkg-query -W -f=\${Version} libvirt-daemon-system"
  assert_eq "the tag is inspected before and after the create" "2" "$(calls_of "imagetools inspect ${IMAGE}:${VERSION}-r2")"
  assert_eq "GITHUB_OUTPUT holds the tag and minted=true" \
    "$(printf 'tag=%s\nminted=true' "${VERSION}-r2")" "$GH_OUTPUT"
  assert_contains "stdout carries the outputs too" "$STDOUT" "minted=true"
  assert_eq "nothing is printed on stderr" "" "$STDERR"
}

# ---------------------------------------------------------------------------
# Test 2: an existing tag is not moved
# ---------------------------------------------------------------------------
test_present_tag_is_not_moved() {
  echo "Test: a tag the registry has stays on the digest it names"

  run_keeper STUB_FIRST_INSPECT=present

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "imagetools create never runs" "0" "$(calls_of "imagetools create")"
  assert_contains "the log names the digest the tag keeps" "$STDOUT" \
    "${IMAGE}:${VERSION}-r2 already names ${OTHER_DIGEST}; not moved"
  assert_eq "GITHUB_OUTPUT holds the tag and minted=false" \
    "$(printf 'tag=%s\nminted=false' "${VERSION}-r2")" "$GH_OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 3: an unreadable registry mints nothing
# ---------------------------------------------------------------------------
test_registry_error_mints_nothing() {
  echo "Test: an inspect that fails with a 502 leaves the tag alone and fails"

  run_keeper STUB_FIRST_INSPECT=error

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the tag and buildx's message" "$STDERR" \
    "::error::cannot tell whether ${IMAGE}:${VERSION}-r2 exists: ERROR: unexpected status 502 Bad Gateway"
  assert_eq "imagetools create never runs" "0" "$(calls_of "imagetools create")"
  assert_eq "GITHUB_OUTPUT stays empty" "" "$GH_OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 4: a shallow clone fails before docker runs
# ---------------------------------------------------------------------------
test_shallow_clone_fails() {
  echo "Test: a shallow clone fails with the fetch-depth hint before docker runs"

  run_keeper LIBVIRT_REPO_DIR="$SHALLOW"

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error asks for the full history" "$STDERR" \
    "::error::hack/ci-tag-libvirt-keeper.sh needs the full history to count the commits under images/libvirt (actions/checkout with fetch-depth: 0)"
  assert_eq "docker never runs" "" "$DOCKER_CALLS"
}

# ---------------------------------------------------------------------------
# Test 5: a history without the image fails
# ---------------------------------------------------------------------------
test_no_libvirt_commit_fails() {
  echo "Test: a history with no commit under images/libvirt fails"

  run_keeper LIBVIRT_REPO_DIR="$NO_LIBVIRT"

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the directory" "$STDERR" \
    "::error::no commit touches images/libvirt in ${NO_LIBVIRT}"
  assert_eq "docker never runs" "" "$DOCKER_CALLS"
}

# ---------------------------------------------------------------------------
# Test 6: a directory that is no repository fails with git's message
# ---------------------------------------------------------------------------
test_not_a_repository_fails() {
  echo "Test: a LIBVIRT_REPO_DIR that is no git repository fails before docker runs"

  run_keeper LIBVIRT_REPO_DIR="$NOT_A_REPO"

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the directory" "$STDERR" \
    "::error::cannot read the git history in ${NOT_A_REPO}: "
  assert_contains "and carries git's message" "$STDERR" "not a git repository"
  assert_eq "the error is one line" "1" "$(grep -c . <<<"$STDERR")"
  assert_eq "docker never runs" "" "$DOCKER_CALLS"
}

# ---------------------------------------------------------------------------
# Test 7: a failing docker run fails
# ---------------------------------------------------------------------------
test_failing_run_fails() {
  echo "Test: a docker run that exits 125 fails before imagetools runs"

  run_keeper STUB_RUN_RC=125

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the image" "$STDERR" \
    "::error::cannot read the libvirt-daemon-system version from ${IMAGE}@${DIGEST}"
  assert_eq "imagetools never runs" "0" "$(calls_of "imagetools")"
}

# ---------------------------------------------------------------------------
# Test 8: a version outside the keeper shape fails
# ---------------------------------------------------------------------------
test_unexpected_version_fails() {
  echo "Test: an empty version, an epoch and a suffix fail before imagetools runs"

  local version
  for version in "" "1:10.0.0-2ubuntu8.19" "10.0.0-2ubuntu8.19+esm1"; do
    run_keeper STUB_VERSION="$version"
    assert_eq "'$version' exits 1" "1" "$RC"
    assert_contains "'$version' is named as not <upstream>-<n>ubuntu<m>" "$STDERR" \
      "::error::libvirt-daemon-system version '${version}' is not <upstream>-<n>ubuntu<m>"
    assert_eq "'$version' never reaches imagetools" "0" "$(calls_of "imagetools")"
  done
}

# ---------------------------------------------------------------------------
# Test 9: IMAGE and DIGEST are checked before anything runs
# ---------------------------------------------------------------------------
test_missing_or_malformed_inputs_fail() {
  echo "Test: IMAGE unset, DIGEST unset and DIGEST=latest fail before docker runs"

  run_keeper unset:IMAGE
  assert_eq "IMAGE unset exits 1" "1" "$RC"
  assert_contains "the message names IMAGE" "$STDERR" "IMAGE is required"
  assert_eq "docker never runs without IMAGE" "" "$DOCKER_CALLS"

  run_keeper unset:DIGEST
  assert_eq "DIGEST unset exits 1" "1" "$RC"
  assert_contains "the message names DIGEST" "$STDERR" "DIGEST is required"
  assert_eq "docker never runs without DIGEST" "" "$DOCKER_CALLS"

  run_keeper DIGEST=latest
  assert_eq "DIGEST=latest exits 1" "1" "$RC"
  assert_contains "the message names DIGEST and the value" "$STDERR" \
    "::error::DIGEST must be sha256:<64 hex>, got 'latest'"
  assert_eq "docker never runs with DIGEST=latest" "" "$DOCKER_CALLS"
}

# ---------------------------------------------------------------------------
# Test 10: a failing create fails
# ---------------------------------------------------------------------------
test_failing_create_fails() {
  echo "Test: an imagetools create that exits 1 fails"

  run_keeper STUB_CREATE_RC=1

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names the digest and the tag" "$STDERR" \
    "::error::tagging ${IMAGE}@${DIGEST} as ${VERSION}-r2 failed"
  assert_eq "GITHUB_OUTPUT stays empty" "" "$GH_OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 11: a read-back of another digest fails after three reads
# ---------------------------------------------------------------------------
test_wrong_readback_fails() {
  echo "Test: a tag that reads back as another digest fails after three reads"

  run_keeper STUB_READBACK_DIGEST="$OTHER_DIGEST" RETRY_DELAY=5

  assert_eq "the script exits 1" "1" "$RC"
  assert_contains "the error names both digests" "$STDERR" \
    "::error::${IMAGE}:${VERSION}-r2 names '${OTHER_DIGEST}', not ${DIGEST}"
  assert_eq "the tag is inspected once before and three times after the create" "4" \
    "$(calls_of "imagetools inspect")"
  assert_contains "the retry is announced" "$STDOUT" \
    "::warning::${IMAGE}:${VERSION}-r2 names '${OTHER_DIGEST}' on read 1/3; retrying in 5s"
  assert_eq "the waits between the reads are RETRY_DELAY, then three times that" \
    "$(printf '5\n15')" "$SLEEPS"
  assert_eq "GITHUB_OUTPUT stays empty" "" "$GH_OUTPUT"
}

# ---------------------------------------------------------------------------
# Test 12: a read-back that answers not found is retried
# ---------------------------------------------------------------------------
test_readback_not_found_is_retried() {
  echo "Test: a read-back that answers not found once is retried, and the tag is reported minted"

  run_keeper STUB_READBACK_FAILS=1 RETRY_DELAY=5

  assert_eq "the script exits 0" "0" "$RC"
  assert_eq "the tag is inspected once before and twice after the create" "3" \
    "$(calls_of "imagetools inspect")"
  assert_contains "the retry is announced" "$STDOUT" "on read 1/3; retrying in 5s"
  assert_eq "the script waits RETRY_DELAY once" "5" "$SLEEPS"
  assert_eq "GITHUB_OUTPUT holds the tag and minted=true" \
    "$(printf 'tag=%s\nminted=true' "${VERSION}-r2")" "$GH_OUTPUT"
  assert_eq "nothing is printed on stderr" "" "$STDERR"
}

# ---------------------------------------------------------------------------
# Test 13: the prune and Renovate accept exactly the tags the script mints
# ---------------------------------------------------------------------------
# A tag the script mints but the prune does not keep loses the digest the lab
# pins once latest moves on; a tag Renovate does not allow is never proposed.
test_keeper_shape_lockstep() {
  echo "Test: the prune keep pattern and Renovate's allowedVersions are the script's shape plus -r<N>"

  local shape want prune renovate
  shape="$(sed -n "s/^version_shape='\(.*\)'$/\1/p" "$KEEPER_SH")"
  want="${shape%\$}-r[0-9]+\$"
  prune="$(python3 - "$PROJECT_ROOT/hack/ghcr-prune-stale-versions.py" <<'PYEOF'
import importlib.util
import sys

spec = importlib.util.spec_from_file_location("ghcr_prune", sys.argv[1])
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)
print("\n".join(p for p in prune.DEFAULT_KEEP_PATTERNS if "ubuntu" in p))
PYEOF
  )"
  renovate="$(jq -r '.packageRules[]
    | select((.matchPackageNames // []) | index("ghcr.io/c5c3/libvirt"))
    | .allowedVersions // empty' "$PROJECT_ROOT/renovate.json" | sed 's|^/||; s|/$||')"

  assert_not_empty "the script defines version_shape" "$shape"
  assert_eq "the prune keep pattern is the script's shape plus -r<N>" "$want" "$prune"
  assert_eq "Renovate's allowedVersions is the script's shape plus -r<N>" "$want" "$renovate"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_absent_tag_is_minted
test_present_tag_is_not_moved
test_registry_error_mints_nothing
test_shallow_clone_fails
test_no_libvirt_commit_fails
test_not_a_repository_fails
test_failing_run_fails
test_unexpected_version_fails
test_missing_or_malformed_inputs_fail
test_failing_create_fails
test_wrong_readback_fails
test_readback_not_found_is_retried
test_keeper_shape_lockstep

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
