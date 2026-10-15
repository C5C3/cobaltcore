#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/nix-lock.sh — Translate the release pins into the locks the Nix image
# builds read (docs/reference/ci-cd/nix-image-build.md).
#
# Usage, from anywhere in the repository:
#   hack/nix-lock.sh <name> <release>   write one lock
#   hack/nix-lock.sh --all              write every lock and the noVNC hash,
#                                       and delete the locks nothing pins
#   hack/nix-lock.sh --check            run --all, then exit 1 when the tree
#                                       under nix/locks/ or images/nova/novnc.json
#                                       changed
#
# `make nix-lock` and `make nix-lock-check` run the last two in the lock-tools
# dev shell of flake.nix, whose uv wrote the committed locks.
#
# A lock is nix/locks/<release>/<name>/ with three files:
#   sources.json    for a service: the ref releases/<release>/source-refs.yaml
#                   pins, the commit that ref names and the NAR hash of its
#                   tree, the project name of the tree, and the repo-relative
#                   paths of patches/<name>/<release>/*.patch; the build
#                   fetches by commit and hash, never through the tag. For
#                   tempest: the tempest version and the project name.
#   pyproject.toml  a virtual uv project. For a service: the project as a git
#                   source at that commit with its pip_extras, the
#                   pip_packages of releases/<release>/extra-packages.yaml, the
#                   venv-builder pins, the merged constraints as
#                   constraint-dependencies, and a test dependency group. For
#                   tempest: the five releases/<release>/test-refs.yaml pins,
#                   the clients the e2e suites run, the venv-builder pins and
#                   the merged constraints, without a test group.
#   uv.lock         what `uv lock` resolves from pyproject.toml, seeded with
#                   the committed uv.lock so a rerun with unchanged inputs
#                   writes the same lock
#
# --all also prefetches the noVNC commit images/nova/novnc.json names and
# writes its hash into that file.
#
# The tracked releases/ and overrides/ files are only read: the constraint
# overrides are merged into a temporary copy by
# scripts/apply-constraint-overrides.sh. The files move into place only after
# `uv lock` succeeded, so a failed run leaves the previous lock untouched.
# A release patch that edits requirements.txt or test-requirements.txt is
# refused: the lock is resolved from the upstream files.
#
# Requires uv, nix (with the nix-command and flakes features), yq (mikefarah),
# jq, python3 and git on PATH. `nix flake prefetch github:...` calls the GitHub
# API; set `access-tokens = github.com=<token>` in nix.conf if unauthenticated
# runs hit its rate limit.

set -euo pipefail

usage() {
  echo "Usage: $0 <name> <release> | --all | --check" >&2
  exit 1
}

case "$#:${1:-}" in
  1:--all | 1:--check) MODE="${1#--}" ;;
  2:-*) usage ;;
  2:*) MODE=one ;;
  *) usage ;;
esac

# Builtins only up to the tool check, so a PATH without coreutils still gets
# the release and tool messages.
SCRIPT_DIR="${BASH_SOURCE[0]%/*}"
[ "$SCRIPT_DIR" != "${BASH_SOURCE[0]}" ] || SCRIPT_DIR=.
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
cd "$REPO_ROOT"

check_release() {
  if [ ! -f "releases/$1/source-refs.yaml" ]; then
    echo "nix-lock.sh: releases/$1/source-refs.yaml not found: is '$1' a release directory?" >&2
    exit 1
  fi
}

[ "$MODE" != one ] || check_release "$2"

missing=()
for tool in uv nix yq jq python3 git; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if [ "${#missing[@]}" -gt 0 ]; then
  echo "nix-lock.sh: required tool(s) not on PATH: ${missing[*]}" >&2
  exit 1
fi

NIX=(nix --extra-experimental-features 'nix-command flakes')

# The interpreter of the images, pkgs.python312 in flake.nix: the locks
# resolve for it, with the one the lock-tools shell puts on PATH.
PYTHON_MINOR=3.12

# PEP 503 normalisation, for comparing a requirement name with a constraint.
normalize() {
  local name="${1,,}"
  name="${name//_/-}"
  echo "${name//./-}"
}

# A TOML basic string.
toml_str() {
  local s="${1//\\/\\\\}"
  printf '"%s"' "${s//\"/\\\"}"
}

# One indented array element per argument.
toml_items() {
  local item
  for item in "$@"; do
    printf '    %s,\n' "$(toml_str "$item")"
  done
}

# prefetch prints nix's JSON answer for a github: reference. The empty cache
# directory keeps nix from answering with an earlier resolution of a mutable
# tag, which it otherwise falls back to when GitHub is unreachable; a github:
# reference needs no flake registry. A failure exits with nix's status and
# stderr, before anything is written.
prefetch() {
  XDG_CACHE_HOME="${TMP}/cache" "${NIX[@]}" flake prefetch --option flake-registry '' --json "$1"
}

# collect_tempest_inputs fills lock's DEPENDENCIES for the tempest lock and
# writes its sources.json. Tempest installs from PyPI: its version and the
# four plugins come from test-refs.yaml, and nothing is fetched. The openstack
# CLI with its placement and barbican plugins runs in the e2e verify Jobs;
# python-subunit, junitxml and defusedxml turn a run into the JUnit report
# hack/tempest/run-tests.sh writes.
collect_tempest_inputs() {
  local TEST_REFS="releases/${RELEASE}/test-refs.yaml" pkg pin
  for pkg in tempest barbican-tempest-plugin keystone-tempest-plugin \
    neutron-tempest-plugin cinder-tempest-plugin; do
    pin=$(yq ".\"${pkg}\"" "$TEST_REFS")
    if [ -z "$pin" ] || [ "$pin" = "null" ]; then
      echo "nix-lock.sh: no key '${pkg}' in ${TEST_REFS}" >&2
      exit 1
    fi
    DEPENDENCIES+=("${pkg}==${pin}")
  done
  DEPENDENCIES+=(python-openstackclient osc-placement python-barbicanclient
    python-subunit junitxml defusedxml)
  jq -n --arg ref "${DEPENDENCIES[0]#tempest==}" '{ref: $ref, project: "tempest"}' \
    >"${TMP}/work/sources.json"
}

# collect_service_inputs fills lock's DEPENDENCIES, TEST_GROUP and SOURCE_LINE
# for a service and writes its sources.json: the ref source-refs.yaml pins,
# fetched by commit and hash, the project name of the fetched tree with its
# extras, the extra pip packages, and the test group of its suite.
collect_service_inputs() {
  local REFS="releases/${RELEASE}/source-refs.yaml"
  local REF REV HASH TREE PREFETCH PROJECT EXTRAS line pkg patch
  local -a PATCHES=() TEST_REQS=() runners=()
  local -A IN_TEST_REQS=()

  REF=$(yq ".\"${NAME}\"" "$REFS")
  if [ -z "$REF" ] || [ "$REF" = "null" ]; then
    echo "nix-lock.sh: no key '${NAME}' in ${REFS}" >&2
    exit 1
  fi

  shopt -s nullglob
  PATCHES=(patches/"${NAME}"/"${RELEASE}"/*.patch)
  shopt -u nullglob

  # The lock is resolved from the requirements of the upstream commit,
  # while the build installs the patched tree, so a patch that edits them
  # would be missing from the lock.
  for patch in "${PATCHES[@]}"; do
    if grep -qE '^\+\+\+ b/(test-)?requirements\.txt$' "$patch"; then
      echo "nix-lock.sh: ${patch} edits the requirements, which the lock is resolved without" >&2
      exit 1
    fi
  done

  PREFETCH=$(prefetch "github:openstack/${NAME}/${REF}")
  REV=$(jq -r '.locked.rev' <<<"$PREFETCH")
  HASH=$(jq -r '.hash' <<<"$PREFETCH")
  TREE=$(jq -r '.storePath' <<<"$PREFETCH")

  # The distribution name pbr and uv know the project by, which differs
  # from the source-refs.yaml key where upstream says so (placement ships
  # as openstack-placement). -I keeps a module of the fetched tree off
  # sys.path.
  PROJECT=$(cd "$TREE" && python3 -I - "$NAME" <<'PYEOF'
import configparser, os, sys
name = None
cp = configparser.ConfigParser()
try:
    if cp.read("setup.cfg") and cp.has_option("metadata", "name"):
        name = cp.get("metadata", "name")
except configparser.Error:
    pass
if not name and os.path.exists("pyproject.toml"):
    import tomllib
    with open("pyproject.toml", "rb") as f:
        name = tomllib.load(f).get("project", {}).get("name")
print(name or sys.argv[1])
PYEOF
)

  # The project with its extras and the extra pip packages.
  EXTRAS=$(yq -r ".\"${NAME}\".pip_extras // [] | join(\",\")" "releases/${RELEASE}/extra-packages.yaml")
  DEPENDENCIES=("${PROJECT}${EXTRAS:+[${EXTRAS}]}")
  while IFS= read -r pkg; do
    [ -n "$pkg" ] && DEPENDENCIES+=("$pkg")
  done < <(yq -r ".\"${NAME}\".pip_packages // [] | .[]" "releases/${RELEASE}/extra-packages.yaml")

  # The test group: the runner of the suite unless the project's
  # test-requirements.txt lists it, then that file without comments, blank
  # lines and pip options. stestr runs a suite with a .stestr.conf; the
  # others (horizon) run under pytest, and hacking is the pin horizon's
  # tox py3 env adds for its hacking-rule tests.
  if [ -f "${TREE}/test-requirements.txt" ]; then
    while IFS= read -r line; do
      line="${line%%#*}"
      line="${line%"${line##*[![:space:]]}"}"
      line="${line#"${line%%[![:space:]]*}"}"
      [ -z "$line" ] || [[ "$line" == -* ]] && continue
      TEST_REQS+=("$line")
      if [[ "$line" =~ ^([A-Za-z0-9][A-Za-z0-9._-]*) ]]; then
        IN_TEST_REQS["$(normalize "${BASH_REMATCH[1]}")"]=1
      fi
    done <"${TREE}/test-requirements.txt"
  fi
  if [ -f "${TREE}/.stestr.conf" ]; then
    runners=(stestr testtools)
  else
    runners=(pytest 'hacking>=7.0.0,<7.1.0')
  fi
  for pkg in "${runners[@]}"; do
    [[ "$pkg" =~ ^([A-Za-z0-9][A-Za-z0-9._-]*) ]]
    [ -n "${IN_TEST_REQS[$(normalize "${BASH_REMATCH[1]}")]:-}" ] || TEST_GROUP+=("$pkg")
  done
  TEST_GROUP+=("${TEST_REQS[@]}")

  SOURCE_LINE="${PROJECT} = { git = $(toml_str "https://github.com/openstack/${NAME}"), rev = $(toml_str "$REV") }"
  jq -n --arg ref "$REF" --arg rev "$REV" --arg hash "$HASH" --arg project "$PROJECT" \
    '{ref: $ref, rev: $rev, hash: $hash, project: $project, patches: $ARGS.positional}' \
    --args "${PATCHES[@]}" >"${TMP}/work/sources.json"
}

# merge_constraints fills lock's CONSTRAINTS with the release's
# upper-constraints.txt, the overrides merged, and CONSTRAINED with their
# normalised names. The merge rules are those of the Docker path: the script
# edits releases/<release>/upper-constraints.txt relative to its working
# directory, here a copy of the two files.
merge_constraints() {
  local line
  mkdir -p "${TMP}/releases/${RELEASE}"
  cp "releases/${RELEASE}/upper-constraints.txt" "${TMP}/releases/${RELEASE}/"
  if [ -f "overrides/${RELEASE}/constraints.txt" ]; then
    mkdir -p "${TMP}/overrides/${RELEASE}"
    cp "overrides/${RELEASE}/constraints.txt" "${TMP}/overrides/${RELEASE}/"
  fi
  (cd "$TMP" && bash "${REPO_ROOT}/scripts/apply-constraint-overrides.sh" "$RELEASE" >/dev/null)

  while IFS= read -r line; do
    [[ "$line" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*=== ]] || continue
    CONSTRAINED["$(normalize "${line%%===*}")"]=1
    CONSTRAINTS+=("${line/===/==}")
  done <"${TMP}/releases/${RELEASE}/upper-constraints.txt"
}

# add_venv_builder_pins appends the venv-builder pins to lock's DEPENDENCIES.
# A package the constraints pin is listed by name only: the constraints
# re-pin it, as the comment in images/venv-builder/requirements.txt
# describes.
add_venv_builder_pins() {
  local line name
  while IFS= read -r line; do
    [[ "$line" =~ ^([A-Za-z0-9][A-Za-z0-9._-]*)==[^=] ]] || continue
    name="${BASH_REMATCH[1]}"
    if [ -n "${CONSTRAINED[$(normalize "$name")]:-}" ]; then
      DEPENDENCIES+=("$name")
    else
      DEPENDENCIES+=("$line")
    fi
  done <images/venv-builder/requirements.txt
}

# write_pyproject prints the virtual project of lock: a test dependency group
# only with a TEST_GROUP, a git source only with a SOURCE_LINE.
write_pyproject() {
  echo "# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company"
  echo "#"
  echo "# SPDX-License-Identifier: Apache-2.0"
  echo "#"
  echo "# Generated by hack/nix-lock.sh ${NAME} ${RELEASE}; regenerate with make nix-lock, do not edit."
  echo
  echo "[project]"
  echo "name = $(toml_str "cobaltcore-${NAME}-${RELEASE}")"
  echo 'version = "0"'
  echo "requires-python = \"==${PYTHON_MINOR}.*\""
  echo "dependencies = ["
  toml_items "${DEPENDENCIES[@]}"
  echo "]"
  if [ "${#TEST_GROUP[@]}" -gt 0 ]; then
    echo
    echo "[dependency-groups]"
    echo "test = ["
    toml_items "${TEST_GROUP[@]}"
    echo "]"
  fi
  echo
  echo "[tool.uv]"
  echo "package = false"
  echo "constraint-dependencies = ["
  toml_items "${CONSTRAINTS[@]}"
  echo "]"
  if [ -n "$SOURCE_LINE" ]; then
    echo
    echo "[tool.uv.sources]"
    echo "$SOURCE_LINE"
  fi
}

# lock writes nix/locks/<release>/<name>/. It runs in a subshell of its
# caller, so its trap and its exit end that lock only. The steps share its
# locals through bash's dynamic scope: the inputs of tempest or of a service,
# the merged constraints and the venv-builder pins fill DEPENDENCIES,
# TEST_GROUP, SOURCE_LINE and CONSTRAINTS, which write_pyproject renders.
lock() {
  local NAME="$1" RELEASE="$2"
  local DEST="nix/locks/${RELEASE}/${NAME}" SOURCE_LINE=""
  local -a DEPENDENCIES=() CONSTRAINTS=() TEST_GROUP=()
  local -A CONSTRAINED=()

  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  mkdir -p "${TMP}/work"

  if [ "$NAME" = tempest ]; then
    collect_tempest_inputs
  else
    collect_service_inputs
  fi
  merge_constraints
  add_venv_builder_pins
  write_pyproject >"${TMP}/work/pyproject.toml"

  # uv keeps every locked version the inputs still allow, so the committed
  # lock makes a rerun with unchanged inputs write the same file instead of
  # moving the packages no constraint pins to PyPI's newest.
  [ ! -f "${DEST}/uv.lock" ] || cp "${DEST}/uv.lock" "${TMP}/work/uv.lock"

  # A resolution failure exits here with uv's status and output; the trap
  # removes the work directory and the committed lock stays as it was.
  (cd "${TMP}/work" && UV_PYTHON="$PYTHON_MINOR" uv lock)

  # TMP may lie on another filesystem, where mv copies file by file. The copy
  # goes to a staging directory beside the lock first, so the move into place
  # renames within one filesystem.
  mkdir -p "$DEST"
  STAGE=$(mktemp -d "${DEST}/.stage.XXXXXX")
  trap 'rm -rf "$TMP" "$STAGE"' EXIT
  cp "${TMP}/work/pyproject.toml" "${TMP}/work/uv.lock" "${TMP}/work/sources.json" "$STAGE/"
  mv "${STAGE}/pyproject.toml" "${STAGE}/uv.lock" "${STAGE}/sources.json" "$DEST/"
  echo "nix-lock.sh: wrote ${DEST}"
}

# lock_novnc writes the NAR hash of the noVNC commit images/nova/novnc.json
# names into that file, in the key order version, rev, hash.
lock_novnc() {
  local pin=images/nova/novnc.json rev hash
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  rev=$(jq -r '.rev' "$pin")
  hash=$(prefetch "github:novnc/noVNC/${rev}" | jq -r '.hash')
  jq --arg hash "$hash" '{version: .version, rev: .rev, hash: $hash}' "$pin" >"${TMP}/novnc.json"
  cat "${TMP}/novnc.json" >"$pin"
  echo "nix-lock.sh: wrote ${pin}"
}

# lock_all writes every lock the release files pin and deletes every
# directory under nix/locks/ they do not name.
lock_all() {
  local refs release name dir
  local -A wanted=()
  for refs in releases/*/source-refs.yaml; do
    release="${refs#releases/}"
    release="${release%/source-refs.yaml}"
    wanted["$release"]=1
    while IFS= read -r name; do
      wanted["$release/$name"]=1
      (lock "$name" "$release")
    done < <(yq -r 'keys | .[]' "$refs")
    wanted["$release/tempest"]=1
    (lock tempest "$release")
  done
  (lock_novnc)

  shopt -s nullglob
  for dir in nix/locks/*/ nix/locks/*/*/; do
    dir="${dir%/}"
    if [ -d "$dir" ] && [ -z "${wanted[${dir#nix/locks/}]:-}" ]; then
      rm -rf "$dir"
      echo "nix-lock.sh: deleted ${dir}"
    fi
  done
  shopt -u nullglob
}

case "$MODE" in
  one)
    (lock "$1" "$2")
    ;;
  all)
    lock_all
    ;;
  check)
    lock_all
    stale=$(git status --porcelain -- nix/locks images/nova/novnc.json)
    if [ -n "$stale" ]; then
      echo "$stale" >&2
      echo "nix-lock.sh: the locks are stale; run make nix-lock and commit the result" >&2
      exit 1
    fi
    echo "nix-lock.sh: the locks are current"
    ;;
esac
