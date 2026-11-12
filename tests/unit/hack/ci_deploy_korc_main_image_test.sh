#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# A grouped Renovate update may race an upstream image build. The immutable
# image's provenance must match every platform before any Kubernetes API write.
set -uo pipefail
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PASS=0
FAIL=0
# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
commit="0123456789abcdef0123456789abcdef01234567"
digest="sha256:$(printf 'a%.0s' {1..64})"
cat >"$tmp/source.yaml" <<EOF
spec:
  ref:
    commit: ${commit}
EOF
cat >"$tmp/release.yaml" <<EOF
spec:
  images:
    - name: controller
      newTag: branch-main
      digest: ${digest}
EOF

cat >"$tmp/bin/git" <<'SH'
#!/bin/bash
if [ "$1" = clone ]; then
  for destination in "$@"; do :; done
  mkdir -p "$destination"
fi
SH
cat >"$tmp/bin/kubectl" <<'SH'
#!/bin/bash
printf '%s\n' "$*" >>"$TEST_KUBECTL_LOG"
SH
cat >"$tmp/bin/docker" <<'SH'
#!/bin/bash
printf '%s\n' "$*" >>"$TEST_DOCKER_LOG"
[ "${TEST_DOCKER_FAIL:-false}" != true ] || exit 1
cat "$TEST_PROVENANCE"
SH
chmod +x "$tmp/bin/git" "$tmp/bin/kubectl" "$tmp/bin/docker"

run_pin() {
  : >"$tmp/kubectl.log"
  : >"$tmp/docker.log"
  PATH="$tmp/bin:$PATH" TEST_PROVENANCE="$tmp/provenance.json" \
    TEST_KUBECTL_LOG="$tmp/kubectl.log" TEST_DOCKER_LOG="$tmp/docker.log" \
    KORC_VERIFY_ONLY="${1:-false}" \
    KORC_SOURCE="$tmp/source.yaml" KORC_RELEASE="$tmp/release.yaml" \
    bash "$PROJECT_ROOT/hack/ci-deploy-korc.sh" 2>&1
}

jq -n --arg commit "$commit" '{"linux/amd64":{SLSA:{runDetails:{metadata:{buildkit_metadata:{vcs:{revision:$commit}}}}}},"linux/arm64":{SLSA:{runDetails:{metadata:{buildkit_metadata:{vcs:{revision:$commit}}}}}}}' >"$tmp/provenance.json"
output="$(run_pin)"; rc=$?
assert_eq "matching provenance on every platform deploys" 0 "$rc"
assert_contains "the installer applies the pinned base" "$(cat "$tmp/kubectl.log")" 'apply --server-side -k'
assert_contains "provenance is inspected by immutable digest" "$(cat "$tmp/docker.log")" "quay.io/orc/openstack-resource-controller@${digest}"
assert_not_contains "provenance does not query the moving tag" "$(cat "$tmp/docker.log")" ':branch-main'

sed "s/${commit}/${commit%?}8/" "$tmp/source.yaml" >"$tmp/source-bump.yaml"
cp "$tmp/source.yaml" "$tmp/source-original.yaml"
mv "$tmp/source-bump.yaml" "$tmp/source.yaml"
output="$(run_pin)"; rc=$?
assert_nonzero_exit "a source-only Renovate bump fails provenance verification" "$rc"
assert_eq "a source-only bump causes no Kubernetes API calls" '' "$(cat "$tmp/kubectl.log")"
mv "$tmp/source-original.yaml" "$tmp/source.yaml"

jq '."linux/arm64".SLSA.runDetails.metadata.buildkit_metadata.vcs.revision = "other-commit"' "$tmp/provenance.json" >"$tmp/bad.json"
mv "$tmp/bad.json" "$tmp/provenance.json"
output="$(run_pin)"; rc=$?
assert_nonzero_exit "one mismatched platform blocks deployment" "$rc"
assert_contains "the failure identifies source/image drift" "$output" 'image provenance does not match the pinned commit'
assert_eq "a mismatch causes no Kubernetes API calls" '' "$(cat "$tmp/kubectl.log")"

for json in '{}' 'null' '{"linux/amd64":{}}' 'invalid-json'; do
  printf '%s\n' "$json" >"$tmp/provenance.json"
  output="$(run_pin)"; rc=$?
  assert_nonzero_exit "absent or malformed provenance fails closed: $json" "$rc"
  assert_eq "absent provenance causes no Kubernetes API calls" '' "$(cat "$tmp/kubectl.log")"
done

output="$(TEST_DOCKER_FAIL=true run_pin)"; rc=$?
assert_nonzero_exit "registry inspection failure blocks deployment" "$rc"
assert_eq "registry failure causes no Kubernetes API calls" '' "$(cat "$tmp/kubectl.log")"

output="$(run_pin true)"; rc=$?
assert_eq "verify-only checks local pins offline" 0 "$rc"
assert_eq "verify-only never inspects the registry" '' "$(cat "$tmp/docker.log")"

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
