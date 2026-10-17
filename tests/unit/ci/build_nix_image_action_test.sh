#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Run the shell steps of .github/actions/build-nix-image/action.yaml the way
# the runner does (bash --noprofile --norc -eo pipefail), with stub nix,
# docker and (for nvd-feeds) curl binaries first on PATH:
#   - nvd-feeds: the release tag comes from the `latest` redirect once and
#     every asset is fetched from that tag; rewrap-nvd-feed.py (run for
#     real) checks the sha256 of the .meta and writes the NIST wrapper under
#     the file names vulnix requests; a wrong hash fails the step;
#   - vulnix: exit 0 and exit 2 with a JSON array are a completed scan and
#     write the step-summary line; exit 2 without a report (vulnix's exit on
#     an internal error) and any other status fail the step; the scan gets
#     the store paths of conf.json's store_layers, not the stream script,
#     and reads the feeds of nvd-feeds through --mirror with a fresh
#     database in RUNNER_TEMP; without feeds the step fails;
#   - load: a "Loaded image:" line tags the image as VERIFY_IMAGE_REF, its
#     absence fails the step;
#   - push: a failed skopeo run fails with its exit status, whether or not a
#     digest was written; a run that exits 0 without a sha256 digest fails,
#     also when a digest of an earlier run is left in RUNNER_TEMP; only a
#     clean run writes digest= to GITHUB_OUTPUT; an empty MANIFEST_FORMAT
#     passes skopeo no --format, 'oci' passes --format oci.
#
# verify_build_images_workflow.sh checks the step order and conditions;
# this test checks what the steps do.
#
# Usage: bash tests/unit/ci/build_nix_image_action_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ACTION="$PROJECT_ROOT/.github/actions/build-nix-image/action.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: yq or jq not installed"
  echo ""
  echo "Results: $PASS passed, $FAIL failed, 1 skipped"
  exit 0
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

STUB_DIR="$TMP_DIR/bin"
WORK="$TMP_DIR/work"
export NIX_LOG="$TMP_DIR/nix.log"
export DOCKER_LOG="$TMP_DIR/docker.log"
export CURL_LOG="$TMP_DIR/curl.log"
export STUB_CONF="$TMP_DIR/conf.json"
export STUB_ASSETS="$TMP_DIR/assets"
RUNNER_TEMP_DIR="$TMP_DIR/runner-temp"
MIRROR="$RUNNER_TEMP_DIR/nvd-mirror"
THIS_YEAR="$(date -u +%Y)"

DIGEST="sha256:$(printf '%.0sa' {1..64})"
STALE_DIGEST="sha256:$(printf '%.0sb' {1..64})"

make_stubs() {
  mkdir -p "$STUB_DIR" "$WORK"

  # nix: records its argv, one line per call.
  #   build ...                       prints STUB_CONF (the .conf attribute)
  #   develop ... --command vulnix    prints STUB_VULNIX_OUT, exits STUB_STATUS
  #   develop ... --command skopeo    writes STUB_DIGEST to the --digestfile
  #                                   path when set, exits STUB_STATUS
  cat >"$STUB_DIR/nix" <<'STUB'
#!/bin/bash
echo "nix $*" >>"$NIX_LOG"
case "$1" in
  build)
    echo "$STUB_CONF"
    exit 0
    ;;
  develop)
    shift 3
    tool="$1"
    shift
    case "$tool" in
      vulnix)
        printf '%s' "${STUB_VULNIX_OUT:-}"
        exit "${STUB_STATUS:-0}"
        ;;
      skopeo)
        digestfile=""
        while [ "$#" -gt 0 ]; do
          [ "$1" = "--digestfile" ] && digestfile="$2"
          shift
        done
        [ -n "${STUB_DIGEST:-}" ] && printf '%s' "$STUB_DIGEST" >"$digestfile"
        exit "${STUB_STATUS:-0}"
        ;;
    esac
    ;;
esac
echo "nix stub: unexpected call: $*" >&2
exit 99
STUB
  chmod +x "$STUB_DIR/nix"

  # docker: records its argv; load prints STUB_LOADED (may be empty).
  cat >"$STUB_DIR/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_LOG"
case "$1" in
  load)
    cat >/dev/null
    [ -n "${STUB_LOADED:-}" ] && echo "Loaded image: $STUB_LOADED"
    exit 0
    ;;
  tag)
    exit 0
    ;;
esac
echo "docker stub: unexpected call: $*" >&2
exit 99
STUB
  chmod +x "$STUB_DIR/docker"

  # The out-link of the build step: the stream script.
  printf '#!/bin/bash\necho image-stream\n' >"$WORK/result-image"
  chmod +x "$WORK/result-image"

  jq -n '{store_layers: [["/nix/store/aaa-rclone-1.75.1"], ["/nix/store/bbb-glibc-2.42", "/nix/store/ccc-zlib-1.3.2"]]}' >"$STUB_CONF"
}

# make_feeds — the mirror directory the nvd-feeds step leaves behind, with
# one feed for the current year; the vulnix step serves it to the stub.
make_feeds() {
  rm -rf "$MIRROR"
  mkdir -p "$MIRROR"
  printf '{"vulnerabilities":[]}' | gzip >"$MIRROR/nvdcve-2.0-$THIS_YEAR.json.gz"
}

# make_curl_stub — curl for the nvd-feeds step: a -w '%{redirect_url}' call
# prints the release URL of tag STUB_TAG; a -o download copies the asset of
# the URL's basename from STUB_ASSETS. Every call is logged.
make_curl_stub() {
  cat >"$STUB_DIR/curl" <<'STUB'
#!/bin/bash
echo "curl $*" >>"$CURL_LOG"
out=""
mode=download
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -w) [[ "$2" == *redirect_url* ]] && mode=redirect; shift ;;
    --retry) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
if [ "$mode" = redirect ]; then
  printf '%s' "https://github.com/fkie-cad/nvd-json-data-feeds/releases/download/${STUB_TAG:-}/${url##*/}"
  exit 0
fi
asset="$STUB_ASSETS/${url##*/}"
if [ ! -f "$asset" ]; then
  echo "curl stub: no asset for $url" >&2
  exit 22
fi
cp "$asset" "$out"
STUB
  chmod +x "$STUB_DIR/curl"
}

# make_assets — one CVE-<year>.json.xz and .meta pair per year the step
# fetches, in the mirror's {"cve_items": [cve, …]} wrapper.
make_assets() {
  rm -rf "$STUB_ASSETS"
  mkdir -p "$STUB_ASSETS"
  local y json
  for ((y = THIS_YEAR - 5; y <= THIS_YEAR; y++)); do
    json="{\"cve_count\":1,\"cve_items\":[{\"id\":\"CVE-$y-0001\",\"vulnStatus\":\"Analyzed\"}]}"
    printf '%s' "$json" | xz >"$STUB_ASSETS/CVE-$y.json.xz"
    printf 'lastModifiedDate:2026-10-14T13:17:13+00:00\nsize:%s\nsha256:%s\n' \
      "${#json}" "$(printf '%s' "$json" | sha256sum | cut -d' ' -f1)" >"$STUB_ASSETS/CVE-$y.meta"
  done
}

# run_step <id> [VAR=value ...] — run the step's own script in WORK and
# print its stdout and stderr; the exit status is the step's.
run_step() {
  local id="$1"
  shift
  local script="$TMP_DIR/step-$id.sh"
  yq -r ".runs.steps[] | select(.id == \"$id\") | .run" "$ACTION" >"$script"
  (cd "$WORK" && env PATH="$STUB_DIR:$PATH" "$@" \
    bash --noprofile --norc -eo pipefail "$script" 2>&1)
}

reset_logs() {
  : >"$NIX_LOG"
  : >"$DOCKER_LOG"
  : >"$CURL_LOG"
  : >"$TMP_DIR/summary"
  : >"$TMP_DIR/output"
  rm -f "$WORK/vulnix.json"
}

FINDINGS='[{"name":"glibc-2.42","derivation":"/nix/store/x-glibc-2.42.drv","affected_by":["CVE-1"]},{"name":"zlib-1.3.2","derivation":"/nix/store/y-zlib-1.3.2.drv","affected_by":["CVE-2"]}]'

test_vulnix_completed_scans() {
  echo "Test: vulnix exit 0 and exit 2 with a report are a completed scan"

  local out rc
  reset_logs
  rc=0
  out=$(run_step vulnix ATTRIBUTE=backup-shifter-image RUNNER_TEMP="$RUNNER_TEMP_DIR" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_STATUS=0 STUB_VULNIX_OUT='[]') || rc=$?
  assert_eq "exit 0 with [] passes" "0" "$rc"
  assert_eq "the summary reports no findings" "vulnix: 0 findings in 0 derivations" \
    "$(cat "$TMP_DIR/summary")"
  assert_contains "the scan builds the conf of the attribute" "$(cat "$NIX_LOG")" \
    "nix build .#backup-shifter-image.conf --no-link --print-out-paths"
  assert_contains "vulnix scans the store paths of the image layers" "$(cat "$NIX_LOG")" \
    "vulnix --mirror http://127.0.0.1:18765/ --cache-dir $RUNNER_TEMP_DIR/vulnix --closure --json /nix/store/aaa-rclone-1.75.1 /nix/store/bbb-glibc-2.42 /nix/store/ccc-zlib-1.3.2"
  assert_not_contains "vulnix does not scan the stream script" "$(cat "$NIX_LOG")" "result-image"
  assert_contains "the mirror serves the feed directory" "$(cat "$MIRROR/http.log")" \
    "Serving HTTP on 127.0.0.1 port 18765"
  assert_eq "the mirror is stopped with the step" "" "$(pgrep -f "http.server --bind 127.0.0.1 --directory $MIRROR" || true)"

  reset_logs
  rc=0
  out=$(run_step vulnix ATTRIBUTE=backup-shifter-image RUNNER_TEMP="$RUNNER_TEMP_DIR" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_STATUS=2 STUB_VULNIX_OUT="$FINDINGS") || rc=$?
  assert_eq "exit 2 with findings passes" "0" "$rc"
  assert_eq "the summary counts the findings" "vulnix: 2 findings in 2 derivations" \
    "$(cat "$TMP_DIR/summary")"
  assert_eq "the report is uploaded as vulnix wrote it" "$FINDINGS" \
    "$(jq -c . "$WORK/vulnix.json")"
}

test_vulnix_failed_scans() {
  echo "Test: a vulnix run without a report fails the step"

  local out rc
  reset_logs
  rc=0
  out=$(run_step vulnix ATTRIBUTE=backup-shifter-image RUNNER_TEMP="$RUNNER_TEMP_DIR" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_STATUS=2 STUB_VULNIX_OUT='') || rc=$?
  assert_nonzero_exit "exit 2 without a report fails" "$rc"
  assert_contains "the error names the missing report" "$out" \
    "::error::vulnix exited with status 2 and wrote no JSON report"
  assert_eq "no summary line is written" "" "$(cat "$TMP_DIR/summary")"

  reset_logs
  rc=0
  out=$(run_step vulnix ATTRIBUTE=backup-shifter-image RUNNER_TEMP="$RUNNER_TEMP_DIR" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_STATUS=1 STUB_VULNIX_OUT='') || rc=$?
  assert_nonzero_exit "exit 1 fails" "$rc"
  assert_contains "the error names the status" "$out" "::error::vulnix exited with status 1"
  assert_eq "no summary line is written" "" "$(cat "$TMP_DIR/summary")"

  reset_logs
  rm -rf "$MIRROR"
  rc=0
  out=$(run_step vulnix ATTRIBUTE=backup-shifter-image RUNNER_TEMP="$RUNNER_TEMP_DIR" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_STATUS=0 STUB_VULNIX_OUT='[]') || rc=$?
  assert_nonzero_exit "no feeds in RUNNER_TEMP fails" "$rc"
  assert_contains "the error names the missing feeds" "$out" \
    "::error::no NVD feeds in $MIRROR; the nvd-feeds step did not run"
  assert_not_contains "vulnix does not run without feeds" "$(cat "$NIX_LOG")" "vulnix"
  make_feeds
}

test_nvd_feeds() {
  echo "Test: the nvd-feeds step fetches one release and rewraps its feeds"

  local out rc y
  make_curl_stub
  make_assets
  reset_logs
  rc=0
  out=$(run_step nvd-feeds RUNNER_TEMP="$RUNNER_TEMP_DIR" GITHUB_ACTION_PATH="$(dirname "$ACTION")" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_TAG=v2026.10.15-000015) || rc=$?
  assert_eq "the step passes" "0" "$rc"
  assert_eq "the tag is resolved once from the latest redirect" "1" \
    "$(grep -c "releases/latest/download/" "$CURL_LOG" || true)"
  assert_eq "twelve assets are fetched from the resolved tag" "12" \
    "$(grep -c -- "-o .* https://github.com/fkie-cad/nvd-json-data-feeds/releases/download/v2026.10.15-000015/CVE-" "$CURL_LOG" || true)"
  assert_eq "the feeds cover the current year and the five before it" \
    "$(for ((y = THIS_YEAR - 5; y <= THIS_YEAR; y++)); do echo "nvdcve-2.0-$y.json.gz"; done)" \
    "$(ls "$MIRROR")"
  assert_eq "the items sit in the NIST wrapper" \
    "{\"vulnerabilities\":[{\"cve\":{\"id\":\"CVE-$THIS_YEAR-0001\",\"vulnStatus\":\"Analyzed\"}}]}" \
    "$(gzip -dc "$MIRROR/nvdcve-2.0-$THIS_YEAR.json.gz" | jq -c .)"
  assert_eq "the summary names the release and the years" \
    "nvd-feeds: v2026.10.15-000015, $((THIS_YEAR - 5)) to $THIS_YEAR" "$(cat "$TMP_DIR/summary")"

  reset_logs
  sed -i 's/^sha256:.*/sha256:0000/' "$STUB_ASSETS/CVE-$THIS_YEAR.meta"
  rc=0
  out=$(run_step nvd-feeds RUNNER_TEMP="$RUNNER_TEMP_DIR" GITHUB_ACTION_PATH="$(dirname "$ACTION")" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_TAG=v2026.10.15-000015) || rc=$?
  assert_nonzero_exit "a feed whose sha256 differs from its .meta fails" "$rc"
  assert_contains "the error names the file and both hashes" "$out" \
    "::error::$MIRROR/src/CVE-$THIS_YEAR.json.xz: sha256 "
  assert_contains "the error names the .meta hash" "$out" "does not match 0000 from"
  assert_eq "no summary line is written" "" "$(cat "$TMP_DIR/summary")"

  reset_logs
  rc=0
  out=$(run_step nvd-feeds RUNNER_TEMP="$RUNNER_TEMP_DIR" GITHUB_ACTION_PATH="$(dirname "$ACTION")" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_TAG=) || rc=$?
  assert_nonzero_exit "a redirect without a tag fails" "$rc"
  assert_contains "the error quotes the redirect" "$out" \
    "redirected to 'https://github.com/fkie-cad/nvd-json-data-feeds/releases/download//CVE-all.meta', which names no release tag"
  assert_eq "nothing is downloaded" "1" "$(wc -l <"$CURL_LOG")"

  reset_logs
  rc=0
  out=$(run_step nvd-feeds RUNNER_TEMP="$RUNNER_TEMP_DIR" GITHUB_ACTION_PATH="$(dirname "$ACTION")" \
    GITHUB_STEP_SUMMARY="$TMP_DIR/summary" STUB_TAG=v1/../v2) || rc=$?
  assert_nonzero_exit "a redirect outside the release download path fails" "$rc"
  assert_contains "the error quotes the redirect" "$out" "which names no release tag"
  assert_eq "nothing is downloaded" "1" "$(wc -l <"$CURL_LOG")"

  rm -f "$STUB_DIR/curl"
  make_feeds
}

test_load() {
  echo "Test: the load step tags the loaded image or fails"

  local out rc
  reset_logs
  rc=0
  out=$(run_step load VERIFY_IMAGE_REF=ghcr.io/c5c3/backup-shifter:pr-verify \
    STUB_LOADED=backup-shifter:latest) || rc=$?
  assert_eq "a loaded image passes" "0" "$rc"
  assert_contains "the loaded image is tagged as VERIFY_IMAGE_REF" "$(cat "$DOCKER_LOG")" \
    "docker tag backup-shifter:latest ghcr.io/c5c3/backup-shifter:pr-verify"

  reset_logs
  rc=0
  out=$(run_step load VERIFY_IMAGE_REF=ghcr.io/c5c3/backup-shifter:pr-verify \
    STUB_LOADED='') || rc=$?
  assert_nonzero_exit "no Loaded image line fails" "$rc"
  assert_contains "the error names the missing line" "$out" \
    '::error::docker load printed no "Loaded image:" line'
  assert_not_contains "nothing is tagged" "$(cat "$DOCKER_LOG")" "docker tag"
}

# push_case <status> <digest skopeo writes> [VAR=value ...] — run the push
# step with a digest of an earlier run left in RUNNER_TEMP.
push_case() {
  local status="$1" digest="$2"
  shift 2
  reset_logs
  mkdir -p "$TMP_DIR/runner-temp"
  printf '%s' "$STALE_DIGEST" >"$TMP_DIR/runner-temp/digest"
  run_step push IMAGE_NAME=ghcr.io/c5c3/backup-shifter \
    RUNNER_TEMP="$TMP_DIR/runner-temp" GITHUB_OUTPUT="$TMP_DIR/output" \
    STUB_STATUS="$status" STUB_DIGEST="$digest" "$@"
}

test_push() {
  echo "Test: the push step exports only the digest of a clean skopeo run"

  local out rc
  rc=0
  out=$(push_case 0 "$DIGEST") || rc=$?
  assert_eq "a clean push passes" "0" "$rc"
  assert_eq "the digest goes to GITHUB_OUTPUT" "digest=$DIGEST" "$(cat "$TMP_DIR/output")"
  assert_contains "skopeo pushes the stream's tar by digest" "$(cat "$NIX_LOG")" \
    "skopeo copy --retry-times 3 --digestfile $TMP_DIR/runner-temp/digest docker-archive:$TMP_DIR/runner-temp/image.tar docker://ghcr.io/c5c3/backup-shifter@@unknown-digest@@"

  rc=0
  out=$(push_case 1 "") || rc=$?
  assert_nonzero_exit "a failed push without a digest fails" "$rc"
  assert_contains "the error names skopeo's status" "$out" "::error::skopeo exited with status 1"
  assert_eq "no digest is exported" "" "$(cat "$TMP_DIR/output")"

  rc=0
  out=$(push_case 1 "$DIGEST") || rc=$?
  assert_nonzero_exit "a failed push with a digest fails" "$rc"
  assert_contains "the error names skopeo's status" "$out" "::error::skopeo exited with status 1"
  assert_eq "no digest is exported" "" "$(cat "$TMP_DIR/output")"

  rc=0
  out=$(push_case 0 "") || rc=$?
  assert_nonzero_exit "a clean exit without a digest fails" "$rc"
  assert_contains "the error names the missing digest" "$out" \
    "::error::skopeo exited 0 but wrote no digest"
  assert_eq "the digest of the earlier run is not exported" "" "$(cat "$TMP_DIR/output")"

  rc=0
  out=$(push_case 0 "sha256:xyz") || rc=$?
  assert_nonzero_exit "a clean exit with a malformed digest fails" "$rc"
  assert_eq "no digest is exported" "" "$(cat "$TMP_DIR/output")"

  rc=0
  out=$(push_case 0 "$DIGEST" MANIFEST_FORMAT=oci) || rc=$?
  assert_eq "an OCI push passes" "0" "$rc"
  assert_contains "skopeo writes an OCI manifest" "$(cat "$NIX_LOG")" \
    "skopeo copy --retry-times 3 --format oci --digestfile $TMP_DIR/runner-temp/digest docker-archive:"
  assert_eq "the digest of the OCI push goes to GITHUB_OUTPUT" "digest=$DIGEST" "$(cat "$TMP_DIR/output")"

  rc=0
  out=$(push_case 0 "$DIGEST" MANIFEST_FORMAT=) || rc=$?
  assert_not_contains "an empty manifest-format passes no --format" "$(cat "$NIX_LOG")" "--format"
}

make_stubs
make_feeds

test_nvd_feeds
echo ""
test_vulnix_completed_scans
echo ""
test_vulnix_failed_scans
echo ""
test_load
echo ""
test_push

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
