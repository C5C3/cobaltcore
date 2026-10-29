#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the fault catalogue of the random chaos runs (#1395),
# tests/chaos-random/catalogue.yaml, against itself and against the
# `# Chaos random:` lines of the chaos suite headers.
#
# A suite is a directory directly under tests/e2e-chaos/ with a
# chainsaw-test.yaml; its header is the leading run of comment and blank
# lines, as hack/gen-chaos-matrix.sh reads it. check_catalogue prints one
# problem per line, naming <file>:<line>, and nothing for a consistent tree:
#   - a suite has no `# Chaos random:` line, has a `none` line beside another
#     `# Chaos random:` line, or names an entry the catalogue lacks;
#   - an entry is named by no suite and has no `source:`;
#   - a name is not unique or does not match
#     ^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$;
#   - `kind` is neither PodChaos nor NetworkChaos;
#   - `environments` is empty or holds a value other than ci or lab;
#   - `duration` or `recovery` is not <n>s with n >= 1;
#   - `spec` is missing or carries a `duration`;
#   - a NetworkChaos entry selects, in its selector or its target, one of the
#     hostNetwork workloads by app.kubernetes.io/name: libvirt, novacompute,
#     ovnchassis, neutronmetadataagent, migration-port-reservation (NetworkChaos
#     and hostNetwork in the Lab Chaos Mesh section of
#     docs/reference/infrastructure/infrastructure-manifests.md).
# The fixture tests prove each problem on a copy of the repository's headers
# and catalogue before the last test runs the check on the repository.
#
# Requires yq (mikefarah) and jq; without either the file skips.
#
# Usage: bash tests/unit/ci/chaos_random_catalogue_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
SUITES_DIR="$PROJECT_ROOT/tests/e2e-chaos"
CATALOGUE="$PROJECT_ROOT/tests/chaos-random/catalogue.yaml"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

NAME_RE='^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$'
HOST_NETWORK_NAMES='["libvirt","novacompute","ovnchassis","neutronmetadataagent","migration-port-reservation"]'

# suite_lines <suites dir> — One line per suite: its file, a tab, and either
# "end <n>" (the line after its header) or "random <n> <value>" for each
# `# Chaos random:` line of its header, in order.
# shellcheck disable=SC2016 # an awk program, not shell
SUITE_AWK='
FNR == 1 { if (file != "") print file "\tend " header_end; file = FILENAME; header_end = 0; in_header = 1 }
in_header && $0 !~ /^#/ && $0 !~ /^[ \t]*$/ { in_header = 0; header_end = FNR }
in_header && index($0, "# Chaos random:") == 1 {
  value = substr($0, length("# Chaos random:") + 1)
  gsub(/^[ \t]+|[ \t]+$/, "", value)
  print FILENAME "\trandom " FNR " " value
}
END { if (file != "") print file "\tend " header_end }
'

# check_catalogue <suites dir> <catalogue> — Print one problem per line.
check_catalogue() {
  local suites_dir="$1" catalogue="$2" entries files
  shopt -s nullglob
  files=("$suites_dir"/*/chainsaw-test.yaml)
  shopt -u nullglob
  # --header-preprocess=false: yq otherwise strips the leading comment block
  # before it parses, and every line number falls short by its length.
  if ! entries="$(yq --header-preprocess=false -o=json -I0 '.[] | {"line": line, "name": .name, "kind": .kind,
      "environments": .environments, "duration": .duration, "recovery": .recovery,
      "source": .source, "spec": .spec,
      "lines": {"name": (.name | key | line), "kind": (.kind | key | line),
        "environments": (.environments | key | line), "duration": (.duration | key | line),
        "recovery": (.recovery | key | line), "spec": (.spec | key | line),
        "spec_duration": (.spec.duration | key | line)}}' "$catalogue" 2>&1)"; then
    echo "$catalogue: not a YAML list: $entries"
    return
  fi

  local suites=""
  if [[ "${#files[@]}" -gt 0 ]]; then
    suites="$(awk "$SUITE_AWK" "${files[@]}")"
  fi

  # The suites: a random line, no none beside another, every entry known. A
  # suite's random lines come first, its end line last.
  local file kind rest line value names_in_catalogue count=0 none_line=""
  names_in_catalogue="$(jq -r '.name // empty' <<<"$entries")"
  while IFS=$'\t' read -r file rest; do
    [[ -n "$file" ]] || continue
    kind="${rest%% *}"
    rest="${rest#* }"
    if [[ "$kind" == end ]]; then
      if [[ "$count" -eq 0 ]]; then
        echo "$file:$rest: the header ends here without a \"# Chaos random:\" line"
      elif [[ -n "$none_line" && "$count" -gt 1 ]]; then
        echo "$file:$none_line: a \"none\" line beside another \"# Chaos random:\" line"
      fi
      count=0
      none_line=""
      continue
    fi
    line="${rest%% *}"
    value="${rest#* }"
    count=$((count + 1))
    if [[ "$value" == none || "$value" == "none "* ]]; then
      [[ -n "$none_line" ]] || none_line="$line"
      continue
    fi
    if ! grep -qxF -- "$value" <<<"$names_in_catalogue"; then
      echo "$file:$line: no catalogue entry \"$value\" in $catalogue"
    fi
  done <<<"$suites"

  # The catalogue: names, kinds, environments, durations, spec, hostNetwork.
  local named
  named="$(awk -F '\t' '$2 ~ /^random / { sub(/^random [0-9]+ /, "", $2); print $2 }' <<<"$suites" | sort -u)"
  jq -r --arg file "$catalogue" --arg re "$NAME_RE" --argjson host "$HOST_NETWORK_NAMES" \
    --arg named "$named" '
    def at(field): "\($file):\(if .lines[field] > 0 then .lines[field] else .line end)";
    def seconds: type == "string" and test("^[0-9]+s$") and (rtrimstr("s") | tonumber) >= 1;
    def selected_names: [(.spec.selector.labelSelectors["app.kubernetes.io/name"]? // empty),
                         (.spec.target.selector.labelSelectors["app.kubernetes.io/name"]? // empty)];
    ($named | split("\n") | map(select(. != ""))) as $named_list
    | [inputs] as $all
    | ($all | map(.name)) as $names
    | $all | to_entries[] | .key as $i | .value
    | (.name // "") as $n
    | (if ($n | type) != "string" or ($n | test($re) | not) then "\(at("name")): name \"\($n)\" does not match \($re)" else empty end),
      (if ($names[:$i] | index($n)) != null then "\(at("name")): a second entry named \($n)" else empty end),
      (if .kind != "PodChaos" and .kind != "NetworkChaos" then "\(at("kind")): entry \($n): kind \"\(.kind // "")\" is neither PodChaos nor NetworkChaos" else empty end),
      (if (.environments | type) != "array" or (.environments | length) == 0 then "\(at("environments")): entry \($n): environments is empty"
       elif any(.environments[]; . != "ci" and . != "lab") then "\(at("environments")): entry \($n): environments holds a value other than ci and lab: \(.environments | join(", "))"
       else empty end),
      (if (.duration | seconds | not) then "\(at("duration")): entry \($n): duration \"\(.duration // "")\" is not <n>s with n >= 1" else empty end),
      (if (.recovery | seconds | not) then "\(at("recovery")): entry \($n): recovery \"\(.recovery // "")\" is not <n>s with n >= 1" else empty end),
      (if (.spec | type) != "object" then "\(at("spec")): entry \($n) has no spec"
       elif .spec.duration != null then "\(at("spec_duration")): entry \($n): spec carries a duration; the template deadline is the entry'"'"'s duration"
       else empty end),
      (if .kind == "NetworkChaos" and any(selected_names[]; . as $s | $host | index($s) != null) then
         "\(at("spec")): entry \($n): a NetworkChaos selects the hostNetwork workload \([selected_names[] | . as $s | select($host | index($s) != null)] | join(", "))"
       else empty end),
      (if ($named_list | index($n)) == null and .source == null then "\(at("name")): entry \($n) is named by no suite and has no source:" else empty end)
  ' -n <<<"$entries"
}

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

# fixture <root> — Copies of the repository's suite files and catalogue.
fixture() {
  local root="$1" f
  rm -rf "$root"
  mkdir -p "$root/suites"
  for f in "$SUITES_DIR"/*/chainsaw-test.yaml; do
    mkdir -p "$root/suites/$(basename "$(dirname "$f")")"
    cp "$f" "$root/suites/$(basename "$(dirname "$f")")/"
  done
  cp "$CATALOGUE" "$root/catalogue.yaml"
}

# line_of <file> <fixed text> — The number of the first line that is the text.
line_of() {
  grep -nxF -- "$2" "$1" | head -n 1 | cut -d: -f1
}

# expect_problem <description> <root> <want> — check_catalogue on the fixture
# prints the problem.
expect_problem() {
  local out
  out="$(check_catalogue "$2/suites" "$2/catalogue.yaml")"
  assert_contains "$1" "$out" "$3"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_the_suites() {
  echo "Test: every suite names catalogue entries, or one none line"
  local root="$TMP_DIR/suites" f n

  fixture "$root"
  f="$root/suites/mariadb-pod-kill/chainsaw-test.yaml"
  sed -i.bak '/^# Chaos random:/d' "$f"
  n="$(awk '!/^#/ && !/^[ \t]*$/ { print NR; exit }' "$f")"
  expect_problem "a suite without a # Chaos random: line" "$root" \
    "$f:$n: the header ends here without a \"# Chaos random:\" line"

  fixture "$root"
  f="$root/suites/mariadb-pod-kill/chainsaw-test.yaml"
  sed -i.bak 's/^# Chaos random: mariadb-pod-kill$/# Chaos random: mariadb-pod-explode/' "$f"
  n="$(line_of "$f" "# Chaos random: mariadb-pod-explode")"
  expect_problem "an unknown entry" "$root" "$f:$n: no catalogue entry \"mariadb-pod-explode\" in $root/catalogue.yaml"

  fixture "$root"
  f="$root/suites/mariadb-operator-cascading-failure/chainsaw-test.yaml"
  sed -i.bak 's/^# Chaos random: mariadb-operator-pod-kill$/# Chaos random: none (no reason)/' "$f"
  n="$(line_of "$f" "# Chaos random: none (no reason)")"
  expect_problem "a none line beside an entry line" "$root" \
    "$f:$n: a \"none\" line beside another \"# Chaos random:\" line"

  fixture "$root"
  f="$root/suites/eso-down/chainsaw-test.yaml"
  awk '{ print } /^# Chaos random: none/ { print "# Chaos random: none (twice)" }' "$f" >"$f.tmp" && mv "$f.tmp" "$f"
  n="$(grep -n '^# Chaos random: none' "$f" | head -n 1 | cut -d: -f1)"
  expect_problem "two none lines" "$root" "$f:$n: a \"none\" line beside another \"# Chaos random:\" line"
}

test_the_entries() {
  echo "Test: every entry is well formed, unique and named by a suite or a source"
  local root="$TMP_DIR/entries" c n

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '. += [{"name": "orphan-pod-kill", "kind": "PodChaos", "environments": ["ci"], "duration": "30s",
    "recovery": "60s", "spec": {"action": "pod-kill", "mode": "one", "selector": {"namespaces": ["x"]}}}]' "$c"
  n="$(line_of "$c" "- name: orphan-pod-kill")"
  expect_problem "an unreferenced entry without source:" "$root" \
    "$c:$n: entry orphan-pod-kill is named by no suite and has no source:"
  yq -i '(.[] | select(.name == "orphan-pod-kill")).source = "lab F9"' "$c"
  assert_not_contains "and with a source: it is fine" "$(check_catalogue "$root/suites" "$c")" "orphan-pod-kill"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "mariadb-pod-kill")).spec.duration = "60s"' "$c"
  n="$(grep -n '^    duration: 60s$' "$c" | head -n 1 | cut -d: -f1)"
  expect_problem "a spec.duration" "$root" \
    "$c:$n: entry mariadb-pod-kill: spec carries a duration; the template deadline is the entry's duration"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "mariadb-pod-kill")).kind = "StressChaos"' "$c"
  n="$(grep -n '^  kind: StressChaos$' "$c" | cut -d: -f1)"
  expect_problem "an unknown kind" "$root" \
    "$c:$n: entry mariadb-pod-kill: kind \"StressChaos\" is neither PodChaos nor NetworkChaos"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "keystone-mariadb-partition")).spec.target.selector.labelSelectors =
    {"app.kubernetes.io/name": "libvirt"}' "$c"
  n="$(awk '/^- name: keystone-mariadb-partition$/ { on = 1 } on && /^  spec:$/ { print NR; exit }' "$c")"
  expect_problem "a NetworkChaos entry selecting app.kubernetes.io/name: libvirt" "$root" \
    "$c:$n: entry keystone-mariadb-partition: a NetworkChaos selects the hostNetwork workload libvirt"

  fixture "$root"
  assert_not_contains "a PodChaos of a hostNetwork pod is fine" \
    "$(check_catalogue "$root/suites" "$root/catalogue.yaml")" "libvirt-pod-kill"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "eso-pod-kill")).name = "mariadb-pod-kill"' "$c"
  n="$(grep -n '^- name: mariadb-pod-kill$' "$c" | tail -n 1 | cut -d: -f1)"
  expect_problem "a name that is not unique" "$root" "$c:$n: a second entry named mariadb-pod-kill"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "nfs-server-pod-kill")).name = "NFS_kill"' "$c"
  n="$(line_of "$c" "- name: NFS_kill")"
  expect_problem "a name outside the pattern" "$root" "$c:$n: name \"NFS_kill\" does not match $NAME_RE"

  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "mariadb-pod-kill")).environments = []' "$c"
  n="$(awk '/^- name: mariadb-pod-kill$/ { on = 1 } on && /^  environments:/ { print NR; exit }' "$c")"
  expect_problem "empty environments" "$root" "$c:$n: entry mariadb-pod-kill: environments is empty"
  yq -i '(.[] | select(.name == "mariadb-pod-kill")).environments = ["ci", "staging"]' "$c"
  expect_problem "an environment other than ci or lab" "$root" \
    "$c:$n: entry mariadb-pod-kill: environments holds a value other than ci and lab: ci, staging"

  local value
  for value in 0s 30 2m ""; do
    fixture "$root"
    c="$root/catalogue.yaml"
    yq -i "(.[] | select(.name == \"mariadb-pod-kill\")).duration = \"$value\"" "$c"
    n="$(awk '/^- name: mariadb-pod-kill$/ { on = 1 } on && /^  duration:/ { print NR; exit }' "$c")"
    expect_problem "a duration of '$value'" "$root" \
      "$c:$n: entry mariadb-pod-kill: duration \"$value\" is not <n>s with n >= 1"
  done
  fixture "$root"
  c="$root/catalogue.yaml"
  yq -i '(.[] | select(.name == "mariadb-pod-kill")) |= del(.recovery)' "$c"
  n="$(line_of "$c" "- name: mariadb-pod-kill")"
  expect_problem "a missing recovery, at the entry's line" "$root" \
    "$c:$n: entry mariadb-pod-kill: recovery \"\" is not <n>s with n >= 1"
}

test_the_repository() {
  echo "Test: the repository's catalogue and suite headers agree"
  local out entries
  out="$(check_catalogue "$SUITES_DIR" "$CATALOGUE")"
  assert_eq "check_catalogue reports nothing" "" "$out"
  entries="$(yq '.[].name' "$CATALOGUE")"
  assert_eq "the catalogue holds 50 entries" "50" "$(grep -c . <<<"$entries")"
  assert_eq "nine suites carry a none line" "9" \
    "$(grep -l '^# Chaos random: none (' "$SUITES_DIR"/*/chainsaw-test.yaml | grep -c .)"
  assert_eq "the four lab faults carry a source" "lab F1 lab F2 lab F3 lab F4" \
    "$(yq -r '[.[] | select(.source != null) | .source] | sort | join(" ")' "$CATALOGUE")"
  assert_eq "only the fake compute is ci alone" "nova-fake-compute-pod-kill" \
    "$(yq -r '[.[] | select((.environments | length) == 1 and .environments[0] == "ci") | .name] | join(" ")' "$CATALOGUE")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: yq or jq not installed (every check of this file skipped)"
  echo ""
  echo "Results: 0 passed, 0 failed, 1 skipped"
  exit 0
fi

test_the_suites
test_the_entries
test_the_repository

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
