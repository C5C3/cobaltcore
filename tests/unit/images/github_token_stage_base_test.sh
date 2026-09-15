#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify no Dockerfile stage that mounts the github_token BuildKit secret
# takes files from venv-builder: through its FROM, a COPY --from or a RUN bind
# mount, directly or through another stage of the same Dockerfile.
#
# venv-builder installs git and then runs `uv pip install -r requirements.txt`
# as root, which builds uwsgi from its sdist: third-party setup code that can
# replace /usr/bin/git or write a git configuration, and whatever it leaves
# behind is inherited by every stage built on venv-builder. The build-service-
# images job hands the stage the workflow token, which carries packages:
# write. A stage fetching with that token therefore starts from an image that
# ran no PyPI code (python-base, ubuntu:noble), takes no files from one that
# did, and installs git from the noble archive itself. Copied files count as
# much as inherited ones: python-base puts /var/lib/openstack/bin first on
# PATH, so a git copied there shadows the archive's.
#
# Usage: bash tests/unit/images/github_token_stage_base_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

# token_stage_taint <Dockerfile> prints "<stage><TAB><tainted|clean>" for every
# stage whose RUN mounts the github_token secret. A stage is tainted when its
# FROM, a COPY --from or a RUN --mount from= names venv-builder or an earlier
# tainted stage, wherever in the stage that line sits. Stage names and
# references compare lowercased, as BuildKit resolves them, and a stage is
# referable by its index as well; an unnamed stage is named by its index. Only
# a line that does not continue the previous one starts an instruction, so a
# printf'd `from x import y` line inside a RUN opens no stage.
token_stage_taint() {
  awk '
    /^[[:space:]]*#/ { next }
    !cont && toupper($1) == "FROM" {
      n++
      i = 2
      while ($i ~ /^--/) i++
      from[n] = tolower($i)
      name[n] = (toupper($(i + 1)) == "AS") ? tolower($(i + 2)) : n - 1
      stage[name[n]] = n
      stage[n - 1] = n
      next
    }
    /--mount=type=secret,id=github_token/ { uses[n] = 1 }
    {
      cont = /\\[[:space:]]*$/
      line = $0
      while (match(line, /[-=,]from=[^ ,\\]+/)) {
        ref = substr(line, RSTART, RLENGTH)
        sub(/^[-=,]from=/, "", ref)
        pulls[n] = pulls[n] " " tolower(ref)
        line = substr(line, RSTART + RLENGTH)
      }
    }
    END {
      for (s = 1; s <= n; s++) {
        t = (from[s] == "venv-builder") || ((from[s] in stage) && tainted[stage[from[s]]])
        k = split(pulls[s], refs, " ")
        for (j = 1; j <= k; j++)
          if (refs[j] == "venv-builder" || ((refs[j] in stage) && tainted[stage[refs[j]]])) t = 1
        tainted[s] = t
        if (uses[s]) print name[s] "\t" (t ? "tainted" : "clean")
      }
    }
  ' "$1"
}

# --- Test 1: token stages take no files from venv-builder ---
test_token_stages_take_no_files_from_venv_builder() {
  echo "Test: no stage mounting the github_token secret takes files from venv-builder"

  local dockerfile rel name taint found="" offenders=""
  for dockerfile in "$PROJECT_ROOT"/images/*/Dockerfile; do
    rel="${dockerfile#"$PROJECT_ROOT"/}"
    while IFS=$'\t' read -r name taint; do
      [ -n "$name" ] || continue
      found="$found $rel"
      [ "$taint" != "tainted" ] || offenders="$offenders $rel:$name"
    done < <(token_stage_taint "$dockerfile")
  done

  # A parser that finds no token stage would pass the check below vacuously.
  assert_contains "the nova fetch stage is found" "$found" "images/nova/Dockerfile"
  assert_contains "the OVN fetch stage is found" "$found" "images/ovn/Dockerfile"
  assert_eq "no github_token stage takes files from venv-builder" "" "$offenders"
}

# --- Test 2: every way venv-builder files reach a stage taints it ---
test_copies_mounts_and_stage_references_taint_a_token_stage() {
  echo "Test: COPY --from, a bind mount, a stage index and a case-folded stage name taint a token stage"

  local tmp got
  tmp="$(mktemp -d)"
  cat > "$tmp/Dockerfile" <<'EOF'
FROM venv-builder AS Build
RUN true

FROM python-base AS copier
COPY --from=build /var/lib/openstack /var/lib/openstack
RUN --mount=type=secret,id=github_token true

FROM python-base AS binder
RUN --mount=type=bind,from=build,target=/tmp/build \
    --mount=type=secret,id=github_token true

FROM build AS chained
RUN printf '#!/usr/bin/python3\n\
from os import path\n' > /tmp/probe
RUN --mount=type=secret,id=github_token true

FROM python-base AS indexed
COPY --from=0 /var/lib/openstack /var/lib/openstack
RUN --mount=type=secret,id=github_token true

FROM python-base AS clean
# COPY --from=build
COPY --from=ghcr.io/astral-sh/uv:0.12.5 /uv /bin/
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=secret,id=github_token true
EOF
  got="$(token_stage_taint "$tmp/Dockerfile" | tr '\t' '=' | paste -s -d ' ' -)"
  rm -rf "$tmp"

  assert_eq "each way in is tainted and the control stage stays clean" \
    "copier=tainted binder=tainted chained=tainted indexed=tainted clean=clean" "$got"
}

# --- Run ---
test_token_stages_take_no_files_from_venv_builder
test_copies_mounts_and_stage_references_taint_a_token_stage

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
