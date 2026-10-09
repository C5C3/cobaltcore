#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify renovate.json points Renovate's native flux manager at the Flux
# HelmReleases, so a chart release past the ceiling of a range opens a reviewed
# PR that raises the ceiling and keeps the floor. The test pins:
#   - the three flux.managerFilePatterns, replayed with Perl against every
#     HelmRelease and HelmRepository file under deploy/flux-system/ and
#     deploy/kind/, and against deploy/lab/ and deploy/examples/, of which only
#     the release.yaml and source.yaml of deploy/lab/metal-stack/ceph match:
#     the lab Ceph has no kind sibling
#   - the order of the flux packageRules: the base rule (widen, no automerge,
#     3-day cooldown, timestamp-optional) is the first flux rule, so every later
#     one overrides it
#   - the versioning: helm rule for charts from a type: oci HelmRepository, and
#     a chart version spelling (x.y.z or >=…) that rule can read
#   - the exclusions: the c5c3-charts releases this repository publishes, and
#     exactly the files a customManager also reads (one owner per pin, never
#     none)
#   - the per-chart policies: bump plus automerged minor/patch for gateway-helm
#     and headlamp, exact csi-driver-nfs and rook-ceph pins with majors
#     disabled and minor/patch unmerged, one group
#     each for the two mariadb-operator charts and for kube-prometheus-stack
#     with prometheus-operator-crds
#
# Every expectation is read from the manifests, never from a hard-coded chart
# version, so a Renovate bump keeps this test green.
#
# Schema validation of renovate.json runs once, in the sibling
# tests/unit/renovate/fluxoperator_custommanager_test.sh; this test does not
# repeat it. Renovate itself is not run: a dry run needs the network and the
# upstream registries (see docs/contributing/dependency-management.md).
#
# Usage: bash tests/unit/renovate/flux_helmrelease_manager_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

RENOVATE_FILE="$PROJECT_ROOT/renovate.json"

# jq prelude: flux_rules yields every packageRule that matches the flux manager,
# in file order.
FLUX_RULES='def flux_rules:
  .packageRules[] | select(((.matchManagers // []) | index("flux")) != null);'

# paths_matching <patterns> <paths> — print each path (one per line) that one of
# the Renovate regex patterns (one per line, written /…/ or /…/i) matches.
paths_matching() {
  PATTERNS="$1" perl -ne '
    BEGIN {
      for (split /\n/, $ENV{PATTERNS}) {
        next unless m{^/(.*)/(i?)$};
        push @re, ($2 ? qr/$1/i : qr/$1/);
      }
    }
    chomp;
    next if $_ eq "";
    for my $re (@re) {
      if ($_ =~ $re) { print "$_\n"; last; }
    }
  ' <<<"$2"
}

# helmrelease_files — the files under deploy/flux-system/ and deploy/kind/ that
# carry a HelmRelease, as repo-relative paths.
helmrelease_files() {
  (cd "$PROJECT_ROOT" \
    && grep -rlE '^kind: HelmRelease[[:space:]]*$' deploy/flux-system deploy/kind 2>/dev/null \
    | sort)
}

# chart_of <file> — the spec.chart.spec.chart value of a HelmRelease manifest.
chart_of() {
  awk '/^      chart: / { print $2; exit }' "$PROJECT_ROOT/$1"
}

# chart_versions <file> — one line per HelmRelease document in the file: the
# first indented version: value with its quotes stripped, or <missing>. A
# release on a chartRef prints nothing. Any indentation and quoting is read, so
# a version Renovate cannot use is reported rather than skipped.
chart_versions() {
  awk -v sq="'" '
    function flush() {
      if (kind == "HelmRelease" && !ref) print (ver == "" ? "<missing>" : ver)
      kind = ""; ver = ""; ref = 0
    }
    /^---/ { flush(); next }
    /^kind:/ { kind = $2 }
    /^[[:space:]]+chartRef:/ { ref = 1 }
    ver == "" && /^[[:space:]]+version:/ {
      ver = $0
      sub(/^[[:space:]]+version:[[:space:]]*/, "", ver)
      sub(/[[:space:]]+#.*$/, "", ver)
      gsub(/"/, "", ver)
      gsub(sq, "", ver)
      sub(/[[:space:]]+$/, "", ver)
    }
    END { flush() }
  ' "$PROJECT_ROOT/$1"
}

# package_of <file> — the packageName the flux manager gives the chart of a
# HelmRelease: the chart name for an HTTP HelmRepository, the url without
# oci:// plus the chart name for a type: oci one. The HelmRepository is read
# from deploy/flux-system/sources/<sourceRef.name>.yaml.
package_of() {
  local chart src url
  chart="$(chart_of "$1")"
  src="$(awk '/^        name: / { print $2; exit }' "$PROJECT_ROOT/$1")"
  url="$(awk '/^  url: / { print $2; exit }' "$PROJECT_ROOT/deploy/flux-system/sources/$src.yaml" 2>/dev/null)"
  case "$url" in
    oci://*) echo "${url#oci://}/$chart" ;;
    *) echo "$chart" ;;
  esac
}

test_flux_file_patterns() {
  echo "Test: the flux manager reads deploy/flux-system/, deploy/kind/ and the lab Ceph's chart, nothing else under deploy/lab/ or deploy/examples/"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local patterns
  patterns="$(jq -c '.flux.managerFilePatterns // empty' "$RENOVATE_FILE")"
  if [ -z "$patterns" ]; then
    echo "  FAIL: flux.managerFilePatterns is missing from renovate.json (the flux manager then reads only gotk-components.yaml)"
    FAIL=$((FAIL + 1))
    return
  fi
  assert_eq "flux.managerFilePatterns names the two Flux trees and the two Flux files of the lab Ceph" \
    '["/^deploy/flux-system/(releases|sources)/.+\\.yaml$/","/^deploy/kind/.+\\.yaml$/","/^deploy/lab/metal-stack/ceph/(release|source)\\.yaml$/"]' \
    "$patterns"

  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local pattern_lines flux_files matched f missed=0
  pattern_lines="$(jq -r '.flux.managerFilePatterns[]' "$RENOVATE_FILE")"
  flux_files="$(cd "$PROJECT_ROOT" \
    && grep -rlE '^kind: Helm(Release|Repository)[[:space:]]*$' deploy/flux-system deploy/kind 2>/dev/null \
    | sort)"
  if [ -z "$flux_files" ]; then
    echo "  FAIL: no HelmRelease or HelmRepository manifest found under deploy/flux-system/ or deploy/kind/"
    FAIL=$((FAIL + 1))
  else
    matched="$(paths_matching "$pattern_lines" "$flux_files")"
    while IFS= read -r f; do
      if ! grep -qxF -- "$f" <<<"$matched"; then
        echo "  FAIL: $f holds a HelmRelease or HelmRepository but matches no flux.managerFilePatterns entry"
        FAIL=$((FAIL + 1))
        missed=$((missed + 1))
      fi
    done <<<"$flux_files"
    if [ "$missed" -eq 0 ]; then
      echo "  PASS: all $(grep -c . <<<"$flux_files") HelmRelease and HelmRepository files match a flux pattern"
      PASS=$((PASS + 1))
    fi
  fi

  # The lab Ceph has no kind overlay to inherit, so its HelmRelease and
  # HelmRepository are the two files under deploy/lab/ the flux manager reads.
  local outside strays ceph
  ceph="$(printf '%s\n' deploy/lab/metal-stack/ceph/release.yaml deploy/lab/metal-stack/ceph/source.yaml)"
  outside="$(cd "$PROJECT_ROOT" && find deploy/lab deploy/examples -type f 2>/dev/null | sort)"
  strays="$(paths_matching "$pattern_lines" "$outside" | grep -vxF -- "$ceph" || true)"
  if [ -z "$strays" ]; then
    echo "  PASS: no other file under deploy/lab/ or deploy/examples/ matches a flux pattern"
    PASS=$((PASS + 1))
  else
    while IFS= read -r f; do
      echo "  FAIL: $f is outside the two Flux trees but matches a flux pattern"
      FAIL=$((FAIL + 1))
    done <<<"$strays"
  fi
  assert_eq "the release.yaml and source.yaml of deploy/lab/metal-stack/ceph match a flux pattern" \
    "$ceph" "$(paths_matching "$pattern_lines" "$ceph")"
}

test_base_rule_is_first() {
  echo "Test: the first flux packageRule is the base rule every later flux rule overrides"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (5 checks skipped)"
    SKIP=$((SKIP + 5))
    return
  fi

  local first
  first="$(jq -c "${FLUX_RULES}"' [flux_rules][0] // empty' "$RENOVATE_FILE")"
  if [ -z "$first" ]; then
    echo "  FAIL: no packageRule matches the flux manager"
    FAIL=$((FAIL + 5))
    return
  fi

  assert_eq "the first flux rule matches on matchManagers alone" \
    '["matchManagers"]' \
    "$(jq -c '[keys[] | select(startswith("match"))]' <<<"$first")"
  assert_eq "base rule rangeStrategy is widen (keeps the floor, raises the ceiling)" \
    "widen" "$(jq -r '.rangeStrategy' <<<"$first")"
  assert_eq "base rule does not automerge" \
    "false" "$(jq -r '.automerge' <<<"$first")"
  assert_eq "base rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$first")"
  assert_eq "base rule accepts a release without a timestamp (ghcr.io publishes none)" \
    "timestamp-optional" "$(jq -r '.minimumReleaseAgeBehaviour' <<<"$first")"
}

test_oci_ranges_use_helm_versioning() {
  echo "Test: chart ranges on an OCI source use helm versioning and a spelling it reads"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (2 checks skipped)"
    SKIP=$((SKIP + 2))
    return
  fi

  local rule
  rule="$(jq -c "${FLUX_RULES}"' [flux_rules
    | select(.matchDatasources == ["docker"] and .matchCurrentValue == "/^>=/")][0] // empty' \
    "$RENOVATE_FILE")"
  if [ -z "$rule" ]; then
    echo "  FAIL: no flux packageRule with matchDatasources [docker] and matchCurrentValue /^>=/ (OCI chart ranges are skipped as invalid-value)"
    FAIL=$((FAIL + 1))
  else
    assert_eq "the docker-datasource range rule sets versioning helm" \
      "helm" "$(jq -r '.versioning' <<<"$rule")"
  fi

  local f value checked=0 bad=0 exact_re='^[0-9]+\.[0-9]+\.[0-9]+$'
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    while IFS= read -r value; do
      [ -z "$value" ] && continue
      checked=$((checked + 1))
      if [ "$value" = "<missing>" ]; then
        echo "  FAIL: $f holds a HelmRelease with neither a chart version nor a chartRef"
        FAIL=$((FAIL + 1))
        bad=$((bad + 1))
      elif [[ ! "$value" =~ $exact_re && "$value" != ">="* ]]; then
        echo "  FAIL: $f spells its chart version \"$value\"; write x.y.z or >=X <Y, or Renovate skips it as invalid-value on an OCI source"
        FAIL=$((FAIL + 1))
        bad=$((bad + 1))
      fi
    done <<<"$(chart_versions "$f")"
  done <<<"$(helmrelease_files)"

  if [ "$checked" -eq 0 ]; then
    echo "  FAIL: no HelmRelease chart version found under deploy/flux-system/ or deploy/kind/"
    FAIL=$((FAIL + 1))
  elif [ "$bad" -eq 0 ]; then
    echo "  PASS: all $checked HelmRelease chart versions are x.y.z or start with >="
    PASS=$((PASS + 1))
  fi
}

test_c5c3_charts_are_excluded() {
  echo "Test: the charts this repository publishes are switched off for the flux manager"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (3 checks skipped)"
    SKIP=$((SKIP + 3))
    return
  fi

  local url expected rule
  url="$(awk '/^  url: / { print $2; exit }' "$PROJECT_ROOT/deploy/flux-system/sources/c5c3-charts.yaml")"
  assert_not_empty "the c5c3-charts HelmRepository carries a url" "$url"
  expected="${url#oci://}/**"

  rule="$(jq -c --arg pkg "$expected" "${FLUX_RULES}"' [flux_rules
    | select(((.matchPackageNames // []) | index($pkg)) != null)][0] // empty' \
    "$RENOVATE_FILE")"
  if [ -z "$rule" ]; then
    echo "  FAIL: no flux packageRule names $expected (the c5c3-charts url without oci://, plus /**)"
    FAIL=$((FAIL + 2))
    return
  fi
  assert_eq "the rule for $expected disables it" \
    "false" "$(jq -r '.enabled' <<<"$rule")"
  assert_eq "the rule for $expected applies to every update type" \
    "false" "$(jq -r 'has("matchUpdateTypes")' <<<"$rule")"
}

test_one_owner_per_file() {
  echo "Test: the flux manager is switched off for exactly the files a customManager reads"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi
  if ! command -v perl >/dev/null 2>&1; then
    echo "  SKIP: perl not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local flux_pats cm_pats disabled files both f missing=0 extra=0
  flux_pats="$(jq -r '.flux.managerFilePatterns[]?' "$RENOVATE_FILE")"
  cm_pats="$(jq -r '.customManagers[]?.managerFilePatterns[]?' "$RENOVATE_FILE")"
  disabled="$(jq -r "${FLUX_RULES}"' flux_rules
    | select(.enabled == false and (has("matchUpdateTypes") | not))
    | (.matchFileNames // [])[]' "$RENOVATE_FILE")"
  files="$(cd "$PROJECT_ROOT" && find deploy -type f -name '*.yaml' | sort)"
  both="$(paths_matching "$cm_pats" "$(paths_matching "$flux_pats" "$files")")"

  if [ -z "$both" ]; then
    echo "  FAIL: no file under deploy/ is read by both the flux manager and a customManager (a pattern list is empty or wrong)"
    FAIL=$((FAIL + 1))
  else
    while IFS= read -r f; do
      if ! grep -qxF -- "$f" <<<"$disabled"; then
        echo "  FAIL: $f is read by a customManager and by the flux manager; list it in the matchFileNames of the flux rule with enabled: false"
        FAIL=$((FAIL + 1))
        missing=$((missing + 1))
      fi
    done <<<"$both"
    if [ "$missing" -eq 0 ]; then
      echo "  PASS: all $(grep -c . <<<"$both") files a customManager reads are switched off for the flux manager"
      PASS=$((PASS + 1))
    fi
  fi

  # The reverse direction: a listed file no customManager reads has no owner,
  # so its chart range silently stops being tracked.
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    [ "$f" = "deploy/flux-system/releases/k-orc.yaml" ] && continue
    if ! grep -qxF -- "$f" <<<"$both"; then
      echo "  FAIL: $f is switched off for the flux manager but no customManager reads it, so no manager tracks its pins; drop it from the matchFileNames of the flux rule with enabled: false"
      FAIL=$((FAIL + 1))
      extra=$((extra + 1))
    fi
  done <<<"$disabled"
  if [ "$extra" -eq 0 ]; then
    echo "  PASS: every file switched off for the flux manager is read by a customManager, apart from the K-ORC release"
    PASS=$((PASS + 1))
  fi

  if grep -qxF -- "deploy/flux-system/releases/k-orc.yaml" <<<"$disabled"; then
    echo "  PASS: the K-ORC image in deploy/flux-system/releases/k-orc.yaml stays untracked"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: deploy/flux-system/releases/k-orc.yaml is not switched off for the flux manager (its image moves with the bump-korc-pin skill)"
    FAIL=$((FAIL + 1))
  fi

  assert_eq "no customManager reads the chart files the flux manager owns" \
    "0" \
    "$(jq '[.customManagers[].managerFilePatterns[]
      | select(test("envoy-gateway|headlamp|nfs/release"))] | length' "$RENOVATE_FILE")"
}

test_floor_tracked_charts() {
  echo "Test: gateway-helm and headlamp bump their floor and automerge minor/patch"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (15 checks skipped)"
    SKIP=$((SKIP + 15))
    return
  fi

  local gw_file="deploy/kind/base/envoy-gateway.yaml"
  local hl_file="deploy/kind/base/headlamp.yaml"
  local url gw_chart gateway headlamp
  url="$(awk '/^  url: oci:\/\// { print $2; exit }' "$PROJECT_ROOT/$gw_file")"
  gw_chart="$(chart_of "$gw_file")"
  headlamp="$(chart_of "$hl_file")"
  assert_not_empty "$gw_file carries an OCI HelmRepository url" "$url"
  assert_not_empty "$gw_file carries a chart name" "$gw_chart"
  assert_not_empty "$hl_file carries a chart name" "$headlamp"
  gateway="${url#oci://}/${gw_chart}"

  local bump
  bump="$(jq -c "${FLUX_RULES}"' [flux_rules | select(.rangeStrategy == "bump")]' "$RENOVATE_FILE")"
  assert_eq "one flux rule uses rangeStrategy bump" \
    "1" "$(jq 'length' <<<"$bump")"
  assert_eq "the bump rule lists the gateway-helm and headlamp charts" \
    "$(jq -cn --arg a "$gateway" --arg b "$headlamp" '[$a, $b] | sort')" \
    "$(jq -c '[.[].matchPackageNames[]?] | sort' <<<"$bump")"

  local pkg file group rule gw_group=""
  while IFS='|' read -r pkg file group; do
    rule="$(jq -c --arg pkg "$pkg" "${FLUX_RULES}"' [flux_rules
      | select(((.matchPackageNames // []) | index($pkg)) != null
          and ((.matchUpdateTypes // []) | index("minor")) != null)][0] // empty' \
      "$RENOVATE_FILE")"
    if [ -z "$rule" ]; then
      echo "  FAIL: no flux packageRule for minor/patch updates of $pkg"
      FAIL=$((FAIL + 4))
      continue
    fi
    assert_eq "$pkg minor/patch rule is scoped to $file" \
      "[\"$file\"]" "$(jq -c '.matchFileNames' <<<"$rule")"
    assert_eq "$pkg minor/patch updates are automerged" \
      "true" "$(jq -r '.automerge' <<<"$rule")"
    assert_eq "$pkg minor/patch rule waits minimumReleaseAge=3 days" \
      "3 days" "$(jq -r '.minimumReleaseAge' <<<"$rule")"
    assert_eq "$pkg minor/patch rule groupName is $group" \
      "$group" "$(jq -r '.groupName' <<<"$rule")"
    [ "$file" = "$gw_file" ] && gw_group="$(jq -r '.groupName' <<<"$rule")"
  done <<EOF
${gateway}|${gw_file}|envoy-gateway
${headlamp}|${hl_file}|headlamp
EOF

  # The CRD pin rides the chart's group, so one PR moves the control plane and
  # its CRDs.
  assert_eq "the gateway-helm chart shares its group with the envoyproxy/gateway CRD pin" \
    "$(jq -r '[.packageRules[]
      | select(((.matchManagers // []) | index("custom.regex")) != null
          and ((.matchPackageNames // []) | index("envoyproxy/gateway")) != null
          and ((.matchUpdateTypes // []) | index("minor")) != null)][0].groupName // empty' \
      "$RENOVATE_FILE")" \
    "$gw_group"

  assert_eq "no flux rule disables a gateway-helm or headlamp major" \
    "0" \
    "$(jq --arg a "$gateway" --arg b "$headlamp" "${FLUX_RULES}"' [flux_rules
      | select(.enabled == false
          and ((.matchUpdateTypes // []) | index("major")) != null
          and ((.matchPackageNames // []) | index($a) != null or index($b) != null))]
      | length' "$RENOVATE_FILE")"
}

test_csi_driver_nfs_rules() {
  echo "Test: the csi-driver-nfs pin keeps majors disabled and minor/patch unmerged"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (4 checks skipped)"
    SKIP=$((SKIP + 4))
    return
  fi

  local path="deploy/kind/nfs/release.yaml" pkg
  pkg="$(chart_of "$path")"
  assert_not_empty "$path carries a chart name" "$pkg"

  local major_rule minor_rule
  major_rule="$(jq -c --arg path "$path" --arg pkg "$pkg" '.packageRules[]
    | select(
        .matchManagers == ["flux"]
        and ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  minor_rule="$(jq -c --arg path "$path" --arg pkg "$pkg" '.packageRules[]
    | select(
        .matchManagers == ["flux"]
        and ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no flux packageRule disabling majors for $path"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major csi-driver-nfs chart updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no flux packageRule for minor/patch csi-driver-nfs chart updates"
    FAIL=$((FAIL + 2))
    return
  fi

  assert_eq "minor/patch chart updates are not automerged (privileged workload)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch chart rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
}

test_rook_ceph_rules() {
  echo "Test: the rook-ceph pin of the lab Ceph keeps majors disabled and minor/patch unmerged"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (6 checks skipped)"
    SKIP=$((SKIP + 6))
    return
  fi

  local path="deploy/lab/metal-stack/ceph/release.yaml" pkg version
  pkg="$(chart_of "$path")"
  version="$(chart_versions "$path")"
  assert_eq "$path carries the chart rook-ceph" "rook-ceph" "$pkg"
  assert_eq "its version is one exact version, not a range" "true" \
    "$([[ "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] && echo true || echo false)"

  local major_rule minor_rule
  major_rule="$(jq -c --arg path "$path" --arg pkg "$pkg" '.packageRules[]
    | select(
        .matchManagers == ["flux"]
        and ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("major")) != null)
      )' "$RENOVATE_FILE" | head -1)"
  minor_rule="$(jq -c --arg path "$path" --arg pkg "$pkg" '.packageRules[]
    | select(
        .matchManagers == ["flux"]
        and ((.matchFileNames // []) | index($path)) != null
        and (((.matchPackageNames // []) | index($pkg)) != null)
        and (((.matchUpdateTypes // []) | index("minor")) != null)
      )' "$RENOVATE_FILE" | head -1)"

  if [ -z "$major_rule" ]; then
    echo "  FAIL: no flux packageRule disabling majors for $path"
    FAIL=$((FAIL + 1))
  else
    assert_eq "major rook-ceph chart updates are disabled" \
      "false" "$(jq -r '.enabled' <<<"$major_rule")"
  fi

  if [ -z "$minor_rule" ]; then
    echo "  FAIL: no flux packageRule for minor/patch rook-ceph chart updates"
    FAIL=$((FAIL + 3))
    return
  fi
  assert_eq "minor/patch chart updates are not automerged (a Rook minor moves the supported Ceph versions)" \
    "false" "$(jq -r '.automerge' <<<"$minor_rule")"
  assert_eq "minor/patch chart rule waits minimumReleaseAge=3 days" \
    "3 days" "$(jq -r '.minimumReleaseAge' <<<"$minor_rule")"
  assert_eq "minor/patch chart rule groupName is rook-ceph chart" \
    "rook-ceph chart" "$(jq -r '.groupName' <<<"$minor_rule")"
}

test_crd_coupled_charts_grouped() {
  echo "Test: a chart and the CRD chart it runs against move in one PR"

  if ! command -v jq >/dev/null 2>&1; then
    echo "  SKIP: jq not installed (8 checks skipped)"
    SKIP=$((SKIP + 8))
    return
  fi

  local op_file crds_file group op crds rules
  while IFS='|' read -r op_file crds_file group; do
    op="$(package_of "$op_file")"
    crds="$(package_of "$crds_file")"
    assert_not_empty "$op_file carries a chart name" "$op"
    assert_not_empty "$crds_file carries a chart name" "$crds"

    rules="$(jq -c --arg a "$op" --arg b "$crds" "${FLUX_RULES}"' [flux_rules
      | select((.matchPackageNames // []) | index($a) != null and index($b) != null)]' \
      "$RENOVATE_FILE")"
    assert_eq "one flux rule names both $op and $crds" \
      "1" "$(jq 'length' <<<"$rules")"
    assert_eq "that rule groups them as $group" \
      "$group" "$(jq -r '.[0].groupName // empty' <<<"$rules")"
  done <<EOF
deploy/flux-system/releases/mariadb-operator.yaml|deploy/flux-system/releases/mariadb-operator-crds.yaml|mariadb-operator chart
deploy/kind/prometheus/release.yaml|deploy/flux-system/releases/prometheus-operator-crds.yaml|prometheus-operator charts
EOF
}

# --- Run ---
test_flux_file_patterns
test_base_rule_is_first
test_oci_ranges_use_helm_versioning
test_c5c3_charts_are_excluded
test_one_owner_per_file
test_floor_tracked_charts
test_csi_driver_nfs_rules
test_rook_ceph_rules
test_crd_coupled_charts_grouped

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
