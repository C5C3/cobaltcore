#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify the contract cobaltcore.dev builds its OpenStack section from.
# cobaltcore-dev/docs checks out docs/, tests/ and deploy/ of this repository
# and renders the pages docs/.vitepress/publish.json lists, mounted under
# another path. The test checks that
#
#   1. publish.json parses and names the canonical site, the repository and
#      a non-empty list of publish patterns;
#   2. every publish pattern matches at least one page under docs/, with the
#      consumer's glob rules: `*` matches within one path segment, `**` across
#      segments;
#   3. no page imports code with `<<< @/`: `@` is the source root of whichever
#      site renders the page, so only a path relative to the page resolves in
#      both builds;
#   4. every code import of a published page resolves, relative to the page,
#      to a file under docs/, tests/ or deploy/, the paths the consumer checks
#      out.
#
# DOCS_DIR overrides the docs tree; it must sit below the project root.
#
# Usage: bash tests/unit/docs/cobaltcore_dev_publish_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

DOCS_DIR="${DOCS_DIR:-$PROJECT_ROOT/docs}"
MANIFEST="$DOCS_DIR/.vitepress/publish.json"

# record <ok 0|1> <description>: one PASS or FAIL.
record() {
  if [[ "$1" -eq 1 ]]; then
    echo "  PASS: $2"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $2"
    FAIL=$((FAIL + 1))
  fi
}

# manifest_query <keys|patterns|published>: prints the manifest's keys, its
# publish patterns that match no page (prefixed with "unmatched "), or the
# published pages relative to DOCS_DIR, one per line. Exits non-zero when the
# manifest does not parse.
manifest_query() {
  python3 -I - "$MANIFEST" "$DOCS_DIR" "$1" <<'EOF'
import json, os, re, sys
manifest, docs, mode = sys.argv[1:]
with open(manifest, encoding="utf-8") as f:
    data = json.load(f)
if mode == "keys":
    for key in ("canonical", "repository", "publish"):
        value = data.get(key)
        ok = bool(value) and (isinstance(value, list) if key == "publish" else isinstance(value, str))
        print(f"{key} {'ok' if ok else 'missing'}")
    sys.exit(0)
def regex(glob):
    parts = (re.escape(p).replace(r"\*", "[^/]*") for p in glob.split("**"))
    return re.compile("^" + ".*".join(parts) + "$")
pages = []
for d, dirs, names in os.walk(docs):
    dirs[:] = [x for x in dirs if not x.startswith(".")]
    pages += [os.path.relpath(os.path.join(d, n), docs).replace(os.sep, "/") for n in names if n.endswith(".md")]
pages.sort()
patterns = [(p, regex(p)) for p in data.get("publish", [])]
if mode == "patterns":
    for p, r in patterns:
        if not any(r.match(page) for page in pages):
            print(f"unmatched {p}")
else:
    for page in pages:
        if any(r.match(page) for _, r in patterns):
            print(page)
EOF
}

# --- Test 1: the manifest ---
test_manifest_parses() {
  echo "Test: publish.json names the canonical site, the repository and the publish patterns"
  if [[ ! -f "$MANIFEST" ]]; then
    record 0 "${MANIFEST#"$PROJECT_ROOT"/} exists"
    return
  fi
  local keys line
  if ! keys="$(manifest_query keys 2>&1)"; then
    record 0 "${MANIFEST#"$PROJECT_ROOT"/} parses as JSON ($keys)"
    return
  fi
  while IFS= read -r line; do
    record "$([[ "$line" == *" ok" ]] && echo 1 || echo 0)" "publish.json holds '${line% *}'"
  done <<<"$keys"
}

# --- Test 2: every pattern publishes a page ---
test_patterns_match() {
  echo "Test: every publish pattern matches at least one page"
  local unmatched
  if ! unmatched="$(manifest_query patterns 2>&1)"; then
    record 0 "the publish patterns can be read ($unmatched)"
    return
  fi
  if [[ -z "$unmatched" ]]; then
    record 1 "every publish pattern matches a page"
    return
  fi
  local line
  while IFS= read -r line; do
    record 0 "publish pattern '${line#unmatched }' matches a page"
  done <<<"$unmatched"
}

# --- Test 3: no import relative to the source root ---
test_no_source_root_imports() {
  echo "Test: no page imports code relative to the source root ('<<< @/')"
  local hits hit file rest
  hits="$(find "$DOCS_DIR" -path "$DOCS_DIR/.vitepress" -prune -o -type f -name '*.md' \
    -exec grep -HnE '^<<<[[:space:]]*@/' {} + || true)"
  if [[ -z "$hits" ]]; then
    record 1 "every code import names a path relative to its page"
    return
  fi
  while IFS= read -r hit; do
    file="${hit%%:*}"
    rest="${hit#*:}"
    record 0 "${file#"$DOCS_DIR"/}:${rest%%:*} imports relative to the source root"
  done <<<"$hits"
}

# --- Test 4: the imports of published pages resolve inside the checkout ---
test_published_imports_resolve() {
  echo "Test: every code import of a published page resolves under docs/, tests/ or deploy/"
  local published page spec target resolved found=0
  if ! published="$(manifest_query published 2>&1)"; then
    record 0 "the published pages can be listed ($published)"
    return
  fi
  while IFS= read -r page; do
    [[ -z "$page" ]] && continue
    while IFS= read -r spec; do
      [[ -z "$spec" ]] && continue
      found=$((found + 1))
      spec="${spec#<<<}"
      spec="${spec#"${spec%%[![:space:]]*}"}"
      target="${spec%%[#{[:space:]]*}"
      resolved="$(realpath -m --relative-to="$PROJECT_ROOT" "$(dirname "$DOCS_DIR/$page")/$target")"
      if [[ "$resolved" =~ ^(docs|tests|deploy)/ && -f "$PROJECT_ROOT/$resolved" ]]; then
        record 1 "$page imports $resolved"
      else
        record 0 "$page imports '$target', which resolves to no file under docs/, tests/ or deploy/ ($resolved)"
      fi
    done < <(grep -E '^<<<[[:space:]]' "$DOCS_DIR/$page" || true)
  done <<<"$published"
  if ((found == 0)); then
    record 0 "the published pages hold at least one code import"
  fi
}

test_manifest_parses
test_patterns_match
test_no_source_root_imports
test_published_imports_resolve

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
