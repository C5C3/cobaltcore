#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the step list of the deploy run follows the banners of
# hack/deploy-infra.sh.
#
# The script logs every step as `log "=== Step <n>/<m>: ... ==="` and the
# parts of its release wait as `log "Phase <name>: ..."`. "## Deployment
# Sequence" of docs/reference/infrastructure/e2e-deployment.md describes the
# steps as a numbered list, 1 to m, and the phases as bullets
# "   - Phase <name>:" under the item of the step whose banner the first
# phase follows. check_step_list compares the two. The figure
# deploy-infra-run numbers its steps with badges, 1 to m in the .drawio and
# the .svg. The step count also stands on that page, on the Infrastructure
# Manifests page, on the extended quick start, in the inventory of
# docs/contributing/architecture-diagrams.md and in the figure, as
# "<k>-step deployment", "Step n/<k>", "runs <k> steps", "make deploy-infra
# as <k> numbered steps", "the <k> steps the script logs" and "the <k> steps
# of `make deploy-infra`", with k a number word from one to twelve in the
# last four; every k that is not m is reported, also in a phrase a hard
# wrap splits. It prints one problem per line, sorted, and nothing when the
# list and the figure follow the script. The fixture tests prove each check
# on a scratch tree before the last test runs it on the repository.
#
# Usage: bash tests/unit/docs/deploy_infra_step_list_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

E2E_PAGE="reference/infrastructure/e2e-deployment.md"
PAGES="$E2E_PAGE
reference/infrastructure/infrastructure-manifests.md
quick-start-extended.md
contributing/architecture-diagrams.md"
FIGURE="deploy-infra-run"

# "N <number>" for every item of the step list and "P <name>" for every
# phase bullet under item phase_step, under every item when the script logs
# no phase and phase_step is empty.
# shellcheck disable=SC2016 # an awk program, not shell
LIST_AWK='
$0 == "## Deployment Sequence" { section = 1; next }
section && /^#/ { exit }
section && /^[0-9]+\. / {
  n = $0; sub(/\..*/, "", n); print "N " n; item = n + 0; next
}
section && item && (phase_step == "" || item == phase_step + 0) && /^   - Phase [^:]*:/ {
  p = $0; sub(/^   - Phase /, "", p); sub(/:.*/, "", p); print "P " p
}
'

# "<file>:<line>: <phrase>, the script has <m> steps" for every phrase that
# states a step count other than m. The pages wrap their prose, so each line
# is read joined to the line before, without its indentation and its
# trailing blanks or carriage return, and a phrase is reported with the line
# it ends on.
# shellcheck disable=SC2016 # an awk program, not shell
COUNT_AWK='
BEGIN {
  n = split("one two three four five six seven eight nine ten eleven twelve", word, " ")
  for (i = 1; i <= n; i++) number[word[i]] = i
  w = "(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)"
  phrase = "[0-9]+-step deployment|Step n/[0-9]+|runs " w " steps|make deploy-infra as " w " numbered steps|[Tt]he " w " steps (the script logs|of `make deploy-infra`)"
}
{
  cur = $0; sub(/^[ \t]+/, "", cur); sub(/[ \t\r]+$/, "", cur)
  line = prev " " cur; off = length(prev) + 1; end = 0
  while (match(line, phrase)) {
    found = substr(line, RSTART, RLENGTH)
    end += RSTART + RLENGTH - 1
    line = substr(line, RSTART + RLENGTH)
    # A phrase that ends on the line before was reported with that line.
    if (end <= off) continue
    if (match(found, /[0-9]+/)) k = substr(found, RSTART, RLENGTH) + 0
    else if (match(found, w)) k = number[substr(found, RSTART, RLENGTH)]
    if (k != m) print file ":" FNR ": " found ", the script has " m " steps"
  }
  prev = cur
}
'

# check_step_list <script> <docs-root>: print one problem per line, sorted,
# and nothing when the step list and the figure under <docs-root>/diagrams
# follow the banners of the script. A missing script, page or figure file, a
# script without a banner, banners that disagree and a section without a
# numbered item are each the only line.
check_step_list() {
  local script="$1" docs="$2"
  local page file banners values m phase_step list numbers expected badges

  if [[ ! -f "$script" ]]; then
    echo "missing $script"
    return
  fi
  while IFS= read -r page; do
    if [[ ! -f "$docs/$page" ]]; then
      echo "missing $docs/$page"
      return
    fi
  done <<<"$PAGES"
  for file in "$docs/diagrams/$FIGURE.drawio" "$docs/diagrams/$FIGURE.svg"; do
    if [[ ! -f "$file" ]]; then
      echo "missing $file"
      return
    fi
  done

  # awk, not grep: GNU grep reads a file with UTF-8 bytes as binary in the
  # POSIX locale and then prints no match.
  banners="$(awk 'match($0, /log "=== Step [0-9]+\/[0-9]+:/) {
    s = substr($0, RSTART, RLENGTH); sub(/^log "=== Step /, "", s); sub(/:$/, "", s); print s
  }' "$script")"
  if [[ -z "$banners" ]]; then
    echo "no step banner in $script"
    return
  fi
  values="$(awk '!seen[$0]++' <<<"$banners" | tr '\n' ' ')"
  # One total m on every banner, and every n from 1 to m.
  if ! m="$(awk -F/ '
    { total[$2] = 1; step[$1 + 0] = 1; m = $2 + 0 }
    END {
      for (t in total) totals++
      if (totals != 1) exit 1
      for (n in step) if (n + 0 < 1 || n + 0 > m) exit 1
      for (i = 1; i <= m; i++) if (!(i in step)) exit 1
      print m
    }' <<<"$banners")"; then
    echo "step banners disagree: ${values% }"
    return
  fi

  # The step whose banner the first phase follows.
  phase_step="$(awk 'match($0, /log "=== Step [0-9]+\//) {
    s = substr($0, RSTART, RLENGTH); gsub(/[^0-9]/, "", s)
  }
  /log "Phase / { print s; exit }' "$script")"
  list="$(awk -v phase_step="$phase_step" "$LIST_AWK" "$docs/$E2E_PAGE")"
  if ! grep -q '^N ' <<<"$list"; then
    echo "no step list in $docs/$E2E_PAGE"
    return
  fi

  {
    numbers="$(sed -n 's/^N //p' <<<"$list" | tr '\n' ' ')"
    expected="$(awk -v m="$m" 'BEGIN { for (i = 1; i <= m; i++) printf "%d ", i }')"
    if [[ "$numbers" != "$expected" ]]; then
      echo "step list numbers: ${numbers% }, expected 1 to $m"
    fi

    # The badges: a cell in the badge style of the .drawio, a <text> that
    # holds only a number in the .svg.
    for file in "$FIGURE.drawio" "$FIGURE.svg"; do
      badges="$(awk '
        /style="ellipse;aspect=fixed;fillColor=#17202B;/ && match($0, /value="[0-9]+"/) {
          s = substr($0, RSTART, RLENGTH); gsub(/[^0-9]/, "", s); print s
        }
        match($0, /<text [^>]*>[0-9]+<\/text>/) {
          s = substr($0, RSTART, RLENGTH); sub(/<\/text>$/, "", s); sub(/.*>/, "", s); print s
        }
      ' "$docs/diagrams/$file" | sort -n | tr '\n' ' ')"
      if [[ "$badges" != "$expected" ]]; then
        badges="${badges% }"
        echo "figure badges: ${badges:-none}, expected 1 to $m (diagrams/$file)"
      fi
    done

    {
      awk 'match($0, /log "Phase [^:]*:/) {
        s = substr($0, RSTART, RLENGTH); sub(/^log "Phase /, "", s); sub(/:$/, "", s); print "S " s
      }' "$script"
      grep '^P ' <<<"$list" || true
    } | awk '
      $1 == "S" { script[$2] = 1 }
      $1 == "P" { listed[$2] = 1 }
      END {
        for (p in script) if (!(p in listed)) print "phase not in the list: " p
        for (p in listed) if (!(p in script)) print "phase not in the script: " p
      }
    '

    while IFS= read -r file; do
      awk -v m="$m" -v file="$file" "$COUNT_AWK" "$docs/$file"
    done <<<"$PAGES
diagrams/$FIGURE.drawio
diagrams/$FIGURE.svg"
  } | LC_ALL=C sort
}

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# build_fixture: write a consistent tree and print its root. deploy-infra.sh
# logs five steps, Step 1 in two branches, and the phases 1 and 2b inside
# Step 4. docs/ holds the four pages: the deploy page with a numbered
# teardown list above "## Deployment Sequence" and one in the subsection
# below it, "5-step deployment" on the deploy page and the quick start, the
# count in words on every page and "nine numbered steps" of another figure
# on the manifests page. docs/diagrams/ holds the figure with the badges 1 to
# 5 and its legend.
build_fixture() {
  local root n
  root="$(mktemp -d "$TMP_ROOT/fixture.XXXXXX")"
  mkdir -p "$root/docs/reference/infrastructure" "$root/docs/contributing" "$root/docs/diagrams"
  cat >"$root/deploy-infra.sh" <<'EOF'
main() {
  if [[ "$EXTERNAL_CLUSTER" == true ]]; then
    log "=== Step 1/5: External cluster: no cluster is created ==="
  else
    log "=== Step 1/5: Create kind cluster ==="
  fi
  log "=== Step 2/5: Install Flux ==="
  log "=== Step 3/5: Apply base kustomize overlay ==="
  log "=== Step 4/5: Wait for HelmReleases ==="
  log "Phase 1: Waiting for cert-manager..."
  log "Phase 2b: Waiting for the Kustomization..."
  log "=== Step 5/5: Apply infrastructure kustomize overlay ==="
}
EOF
  cat >"$root/docs/$E2E_PAGE" <<'EOF'
# Infrastructure E2E Deployment

| `make deploy-infra` | Runs the 5-step deployment into the kind cluster |

Then:

0. The lab hypervisors.
1. The base overlay.

## Deployment Sequence

The script runs five steps and logs each as `=== Step n/5: ... ===`.

![The run of make deploy-infra as five numbered steps.](../../diagrams/deploy-infra-run.svg)

1. **Create or check the cluster.**
2. **Install Flux.**
3. **Apply the base overlay.**
4. **Wait for the releases**, in two phases:
   - Phase 1: cert-manager is Ready.
   - Phase 2b: the Kustomization is Ready.
5. **Apply the infrastructure overlay.**

After Step 5 the script ends.

### Steps and phases

1. Not the step list.
EOF
  cat >"$root/docs/reference/infrastructure/infrastructure-manifests.md" <<'EOF'
# Infrastructure Manifests

The two kustomizations, by hand.

![The admin credential path in nine numbered steps.](../../diagrams/admin-path.svg)
EOF
  cat >"$root/docs/quick-start-extended.md" <<'EOF'
# Quick Start (Extended)

The `deploy-infra` target runs a 5-step deployment sequence.

![The run of make deploy-infra as five numbered steps.](./diagrams/deploy-infra-run.svg)
EOF
  cat >"$root/docs/contributing/architecture-diagrams.md" <<'EOF'
# Architecture Diagrams

| `deploy-infra-run` | The five steps of `make deploy-infra` |
EOF
  {
    echo '<mxfile host="drawio" type="device"><diagram id="r" name="R"><mxGraphModel><root>'
    for n in 1 2 3 4 5; do
      echo "<mxCell id=\"r-$n\" value=\"$n\" style=\"ellipse;aspect=fixed;fillColor=#17202B;strokeColor=none;\" vertex=\"1\" parent=\"1\"/>"
    done
    echo '<mxCell id="r-6" value="A numbered box is one of the five steps the script logs as Step n/5." style="text;" vertex="1" parent="1"/>'
    echo '</root></mxGraphModel></diagram></mxfile>'
  } >"$root/docs/diagrams/$FIGURE.drawio"
  {
    echo '<svg xmlns="http://www.w3.org/2000/svg">'
    for n in 1 2 3 4 5; do
      echo '<circle cx="0" cy="0" r="18" fill="#17202B"/>'
      echo "<text x=\"0\" y=\"0\">$n</text>"
    done
    echo '<text x="0" y="0">A numbered box is one of the five steps the script logs as Step n/5.</text>'
    echo '</svg>'
  } >"$root/docs/diagrams/$FIGURE.svg"
  printf '%s\n' "$root"
}

# check <root>: run the check on a fixture tree.
check() {
  check_step_list "$1/deploy-infra.sh" "$1/docs"
}

# rewrite <file> <sed-script>: apply a sed script in place (BSD and GNU sed).
rewrite() {
  sed "$2" "$1" >"$1.tmp" && mv "$1.tmp" "$1"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test_consistent_fixture_reports_nothing() {
  echo "Test: a step list that follows the banners reports nothing"

  local root
  root="$(build_fixture)"
  assert_eq "no problems" "" "$(check "$root")"
}

test_list_with_fewer_items_than_banners() {
  echo "Test: a list with fewer items than the script has steps is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^5\. /d'
  assert_eq "four items against five banners" \
    "step list numbers: 1 2 3 4, expected 1 to 5" "$(check "$root")"
}

test_list_with_a_gap() {
  echo "Test: a list that skips a number is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^[35]\. /d'
  assert_eq "a list numbered 1, 2, 4" \
    "step list numbers: 1 2 4, expected 1 to 5" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^5\. /6. /'
  assert_eq "the last item renumbered" \
    "step list numbers: 1 2 3 4 6, expected 1 to 5" "$(check "$root")"
}

test_phase_missing_from_the_list() {
  echo "Test: a phase the script logs and the list lacks is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^   - Phase 2b:/d'
  assert_eq "phase 2b" "phase not in the list: 2b" "$(check "$root")"

  # A Phase bullet under another item is not a phase of the list.
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^   - Phase 2b:/d'
  rewrite "$root/docs/$E2E_PAGE" 's/^5\. .*/&\
   - Phase 2b: under item 5./'
  assert_eq "phase 2b under item 5" "phase not in the list: 2b" "$(check "$root")"
}

test_phase_missing_from_the_script() {
  echo "Test: a phase the list names and the script does not log is reported"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^   - Phase 2b:.*/&\
   - Phase 3: the infrastructure releases are Ready./'
  assert_eq "phase 3" "phase not in the script: 3" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" '/log "Phase 1:/d'
  assert_eq "phase 1 removed from the script" "phase not in the script: 1" "$(check "$root")"
}

test_phases_follow_the_step_of_the_script() {
  echo "Test: the phases are read under the step the script logs them in"

  local root
  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" '/log "Phase /d'
  rewrite "$root/deploy-infra.sh" 's/^  log "=== Step 3\/5:.*/&\
  log "Phase 1: Waiting for cert-manager..."\
  log "Phase 2b: Waiting for the Kustomization..."/'
  rewrite "$root/docs/$E2E_PAGE" '/^   - Phase /d'
  rewrite "$root/docs/$E2E_PAGE" 's/^3\. .*/&\
   - Phase 1: cert-manager is Ready.\
   - Phase 2b: the Kustomization is Ready./'
  assert_eq "phases inside Step 3, bullets under item 3" "" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" '/log "Phase /d'
  assert_eq "a script without a phase" \
    "phase not in the script: 1"$'\n'"phase not in the script: 2b" "$(check "$root")"
}

test_step_count_on_a_page() {
  echo "Test: a page that names another step count is reported with its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/quick-start-extended.md" 's/5-step deployment/7-step deployment/'
  assert_eq "a 7-step deployment" \
    "quick-start-extended.md:3: 7-step deployment, the script has 5 steps" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/reference/infrastructure/infrastructure-manifests.md" \
    's/^The two kustomizations, by hand\.$/A 4-step deployment, then a 6-step deployment./'
  assert_eq "two counts on one line" \
    "reference/infrastructure/infrastructure-manifests.md:3: 4-step deployment, the script has 5 steps"$'\n'"reference/infrastructure/infrastructure-manifests.md:3: 6-step deployment, the script has 5 steps" \
    "$(check "$root")"
}

test_step_count_in_words_and_notation() {
  echo "Test: a count in words or as Step n/<k> that is not the script's is reported with its line"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/runs five steps/runs six steps/'
  assert_eq "runs six steps" \
    "$E2E_PAGE:12: runs six steps, the script has 5 steps" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's#Step n/5#Step n/6#'
  rewrite "$root/docs/diagrams/$FIGURE.drawio" 's#Step n/5#Step n/6#'
  rewrite "$root/docs/diagrams/$FIGURE.svg" 's#Step n/5#Step n/6#'
  assert_eq "Step n/6 on the page and in the figure" \
    "diagrams/$FIGURE.drawio:7: Step n/6, the script has 5 steps"$'\n'"diagrams/$FIGURE.svg:12: Step n/6, the script has 5 steps"$'\n'"$E2E_PAGE:12: Step n/6, the script has 5 steps" \
    "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/quick-start-extended.md" 's/as five numbered steps/as four numbered steps/'
  rewrite "$root/docs/contributing/architecture-diagrams.md" 's/The five steps of/The seven steps of/'
  rewrite "$root/docs/diagrams/$FIGURE.svg" 's/the five steps the script logs/the six steps the script logs/'
  assert_eq "an alt text, the inventory and the legend" \
    "contributing/architecture-diagrams.md:3: The seven steps of \`make deploy-infra\`, the script has 5 steps"$'\n'"diagrams/$FIGURE.svg:12: the six steps the script logs, the script has 5 steps"$'\n'"quick-start-extended.md:5: make deploy-infra as four numbered steps, the script has 5 steps" \
    "$(check "$root")"
}

test_step_count_across_a_line_break() {
  echo "Test: a count that a hard wrap splits is reported with the line it ends on"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^The script runs five steps and logs/The script runs six\
steps and logs/'
  assert_eq "runs six, then steps on the next line" \
    "$E2E_PAGE:13: runs six steps, the script has 5 steps" "$(check "$root")"

  # A blank or a carriage return at the end of the wrapped line is no part
  # of the phrase.
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^The script runs five steps and logs/The script runs six \
steps and logs/'
  assert_eq "runs six with a trailing blank, then steps" \
    "$E2E_PAGE:13: runs six steps, the script has 5 steps" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" $'s/^The script runs five steps and logs/The script runs six\r\\\nsteps and logs/'
  assert_eq "runs six with a carriage return, then steps" \
    "$E2E_PAGE:13: runs six steps, the script has 5 steps" "$(check "$root")"

  # The indentation of a list item's next line is no part of the phrase.
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^5\. .*/& The last of the seven\
   steps the script logs./'
  assert_eq "the seven, then the indented rest of the item" \
    "$E2E_PAGE:23: the seven steps the script logs, the script has 5 steps" "$(check "$root")"
}

test_figure_badges() {
  echo "Test: badges that are not 1 to m in a file of the figure are reported for that file"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/diagrams/$FIGURE.drawio" '/value="5"/d'
  assert_eq "badges 1 to 4 against five banners" \
    "figure badges: 1 2 3 4, expected 1 to 5 (diagrams/$FIGURE.drawio)" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/diagrams/$FIGURE.svg" 's#>3</text>#>6</text>#'
  assert_eq "badge 6 in place of 3" \
    "figure badges: 1 2 4 5 6, expected 1 to 5 (diagrams/$FIGURE.svg)" "$(check "$root")"

  # A number in another style is no badge.
  root="$(build_fixture)"
  rewrite "$root/docs/diagrams/$FIGURE.drawio" 's/style="ellipse;aspect=fixed;fillColor=#17202B;/style="ellipse;fillColor=#17202B;/'
  assert_eq "no cell in the badge style" \
    "figure badges: none, expected 1 to 5 (diagrams/$FIGURE.drawio)" "$(check "$root")"
}

test_missing_file_is_the_only_report() {
  echo "Test: a missing script or page is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^5\. /d'
  assert_eq "no script" "missing $root/absent.sh" \
    "$(check_step_list "$root/absent.sh" "$root/docs")"

  rm "$root/docs/$E2E_PAGE"
  assert_eq "no deploy page" "missing $root/docs/$E2E_PAGE" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/docs/reference/infrastructure/infrastructure-manifests.md"
  assert_eq "no manifests page" \
    "missing $root/docs/reference/infrastructure/infrastructure-manifests.md" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/docs/quick-start-extended.md"
  assert_eq "no quick start" "missing $root/docs/quick-start-extended.md" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/docs/contributing/architecture-diagrams.md"
  assert_eq "no diagrams page" \
    "missing $root/docs/contributing/architecture-diagrams.md" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^5\. /d'
  rm "$root/docs/diagrams/$FIGURE.drawio"
  assert_eq "no drawio" "missing $root/docs/diagrams/$FIGURE.drawio" "$(check "$root")"

  root="$(build_fixture)"
  rm "$root/docs/diagrams/$FIGURE.svg"
  assert_eq "no svg" "missing $root/docs/diagrams/$FIGURE.svg" "$(check "$root")"
}

test_no_banner_is_the_only_report() {
  echo "Test: a script without a step banner is the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" '/=== Step /d'
  assert_eq "no banner" "no step banner in $root/deploy-infra.sh" "$(check "$root")"
}

test_disagreeing_banners_are_the_only_report() {
  echo "Test: banners with two totals or a gap are the only report"

  local root
  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" 's#Step 2/5:#Step 2/6:#'
  assert_eq "two totals" \
    "step banners disagree: 1/5 2/6 3/5 4/5 5/5" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" '/Step 3\/5:/d'
  assert_eq "no Step 3" "step banners disagree: 1/5 2/5 4/5 5/5" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/deploy-infra.sh" 's#Step 5/5:#Step 6/5:#'
  assert_eq "a step beyond the total" \
    "step banners disagree: 1/5 2/5 3/5 4/5 6/5" "$(check "$root")"
}

test_no_step_list_is_the_only_report() {
  echo "Test: a section without a numbered item is the only report, other lists are not read"

  local root
  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" '/^## Deployment Sequence$/,/^### /{/^[0-9]/d;}'
  rewrite "$root/docs/quick-start-extended.md" 's/5-step/7-step/'
  assert_eq "no item in the section" "no step list in $root/docs/$E2E_PAGE" "$(check "$root")"

  root="$(build_fixture)"
  rewrite "$root/docs/$E2E_PAGE" 's/^## Deployment Sequence$/## Deployment/'
  assert_eq "no section" "no step list in $root/docs/$E2E_PAGE" "$(check "$root")"
}

test_repository_step_list_follows_the_script() {
  echo "Test: the repository's step list follows hack/deploy-infra.sh"

  local script="$PROJECT_ROOT/hack/deploy-infra.sh"
  local copy
  assert_eq "no problems" "" "$(check_step_list "$script" "$PROJECT_ROOT/docs")"

  # The same check on a copy of the page without the Phase 3b bullet proves
  # it reads the real list and the real script.
  copy="$(mktemp -d "$TMP_ROOT/copy.XXXXXX")"
  mkdir -p "$copy/reference/infrastructure" "$copy/contributing" "$copy/diagrams"
  cp "$PROJECT_ROOT/docs/$E2E_PAGE" "$PROJECT_ROOT/docs/reference/infrastructure/infrastructure-manifests.md" \
    "$copy/reference/infrastructure/"
  cp "$PROJECT_ROOT/docs/quick-start-extended.md" "$copy/"
  cp "$PROJECT_ROOT/docs/contributing/architecture-diagrams.md" "$copy/contributing/"
  cp "$PROJECT_ROOT/docs/diagrams/$FIGURE.drawio" "$PROJECT_ROOT/docs/diagrams/$FIGURE.svg" "$copy/diagrams/"
  rewrite "$copy/$E2E_PAGE" '/^   - Phase 3b:/d'
  assert_eq "a copy without Phase 3b" "phase not in the list: 3b" \
    "$(check_step_list "$script" "$copy")"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
test_consistent_fixture_reports_nothing
test_list_with_fewer_items_than_banners
test_list_with_a_gap
test_phase_missing_from_the_list
test_phase_missing_from_the_script
test_phases_follow_the_step_of_the_script
test_step_count_on_a_page
test_step_count_in_words_and_notation
test_step_count_across_a_line_break
test_figure_badges
test_missing_file_is_the_only_report
test_no_banner_is_the_only_report
test_disagreeing_banners_are_the_only_report
test_no_step_list_is_the_only_report
test_repository_step_list_follows_the_script

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
