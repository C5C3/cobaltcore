#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# audit-nix-lock-drift.sh — Nix-free checks that every lock under nix/locks/
# and every image pin still matches the pins hack/nix-lock.sh derives it from.
# Releases are the directories under releases/, the locks of a release the
# keys of its source-refs.yaml plus tempest:
#   L1  inventory: nix/locks/<rel>/<name>/ holds sources.json, pyproject.toml
#       and uv.lock for every lock of every release, and every directory under
#       nix/locks/ is such a lock
#   L2  source ref: sources.json carries the ref source-refs.yaml pins, a
#       project, a 40-character rev, a sha256- hash and the sorted paths of
#       patches/<name>/<rel>/*.patch, in the order the build applies them;
#       for the tempest lock the tempest key of test-refs.yaml and the
#       project tempest
#   L3  project inputs: [project].dependencies of pyproject.toml opens with the
#       project and its pip_extras, then lists the pip_packages of
#       extra-packages.yaml and the base pins of nix/base-requirements.txt and
#       nothing else, a base pin by name alone where the merged constraints
#       pin it, and [tool.uv.sources] fetches the project at the rev of
#       sources.json. The tempest lock opens with tempest==<version> and holds
#       the five test-refs.yaml pins hack/nix-lock.sh installs and the base
#       pins. A block of extra-packages.yaml without a source-refs.yaml key is
#       the dependency group of one lock, with the block's extras and pip
#       packages
#   L4  constraints: constraint-dependencies equals the name==version form of
#       the name===version lines of upper-constraints.txt after
#       scripts/apply-constraint-overrides.sh merged the overrides into a copy
#   L5  image pins: images/*/source.json and images/*/novnc.json carry a
#       40-character rev and a hash, an ovs pin where a .nix file beside them
#       reads pin.ovs, and a vendorHash beside a manager.nix
#   L6  operators: hack/nix-lock.sh --check-operators
#   L7  [INFO] only: a lock whose newest commit is older than the newest
#       commit of a pin it derives from
#
# Only `make nix-lock-check` regenerates the locks, so only it sees a uv.lock
# that resolves differently on unchanged inputs: trust it over this script.
# Requires yq (mikefarah) v4.49.1 or newer, which reads the TOML of L3 and L4,
# and jq; L7 needs git and degrades to an [INFO] without it.
# Read-only: the constraint merge runs on a copy under mktemp -d.
# Exit code 1 on any [FAIL].

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
cd "${REPO_ROOT}"

FAIL_COUNT=0
fail() { echo "[FAIL] $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
pass() { echo "[PASS] $*"; }
info() { echo "[INFO] $*"; }
hdr()  { echo; echo "=== $* ==="; }

# read_lock's "\t" needs the string escapes of yq v4.49.1.
if ! command -v jq >/dev/null 2>&1 || ! command -v yq >/dev/null 2>&1 \
  || ! grep -q 'mikefarah' <<<"$(yq --version 2>&1)" \
  || [[ "$(yq -n -r '"a\tb"' 2>/dev/null)" != $'a\tb' ]]; then
  echo "[FAIL] audit-nix-lock-drift: yq (mikefarah) v4.49.1 or newer and jq are required"
  exit 1
fi
if [[ ! -d releases ]]; then
  echo "[FAIL] audit-nix-lock-drift: no releases/ directory under ${REPO_ROOT}"
  exit 1
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/nix-lock-drift.XXXXXX")"
trap 'rm -rf "${TMP}"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# first — the first stdin line. awk drains its input, so an upstream writer
# never takes a SIGPIPE that pipefail would turn into a failure (head would).
first() { awk 'NR == 1'; }

# is_rev <s> — true when <s> is a 40-character commit.
is_rev() { grep -Eq '^[0-9a-f]{40}$' <<<"$1"; }

# has_line <line> <lines> — true when <line> is one of the lines of <lines>.
has_line() { grep -qxF -- "$1" <<<"$2"; }

# normalize — the PEP 503 names of the stdin lines, as normalize of
# hack/nix-lock.sh compares a requirement with a constraint.
normalize() { awk '{ n = tolower($0); gsub(/[._]/, "-", n); print n }'; }

# some <list> — the first five lines of <list> joined by ", ", and how many
# more there are.
some() {
  awk 'NF { n++; if (n <= 5) s = s (n > 1 ? ", " : "") $0 }
    END { if (n > 5) s = s " and " (n - 5) " more"; print s }' <<<"$1"
}

# differ <expected> <actual> — "missing a, b; extra c" for the lines of the
# two lists that the other lacks, compared as sorted lists; empty when they
# agree.
differ() {
  local missing extra out=""
  printf '%s\n' "$1" | awk 'NF' | LC_ALL=C sort >"${TMP}/expected"
  printf '%s\n' "$2" | awk 'NF' | LC_ALL=C sort >"${TMP}/actual"
  missing="$(LC_ALL=C comm -23 "${TMP}/expected" "${TMP}/actual")"
  extra="$(LC_ALL=C comm -13 "${TMP}/expected" "${TMP}/actual")"
  [[ -z "${missing}" ]] || out="missing $(some "${missing}")"
  [[ -z "${extra}" ]] || out="${out:+${out}; }extra $(some "${extra}")"
  echo "${out}"
}

# json_error — the message jq wrote to ${TMP}/err, or why it wrote none: the
# file is JSON, but no object.
json_error() {
  local msg
  msg="$(first <"${TMP}/err")"
  echo "${msg:-not a JSON object}"
}

# lock_names <rel> — the locks of a release: its source-refs.yaml keys, then
# tempest.
lock_names() { cat "${TMP}/${1}.keys"; echo tempest; }

# entries <rel> <name> <kind> — the entries of one kind (dep, group, member,
# constraint, source) that read_lock read from the lock's pyproject.toml,
# without the kind column.
entries() { awk -F '\t' -v k="$3" '$1 == k { sub(/^[^\t]*\t/, ""); print }' "${TMP}/${1}/${2}.entries"; }

# block_extras <file> <block> — the pip_extras of a block of an
# extra-packages.yaml, joined by ","; block_packages <file> <block> — its
# pip_packages, one per line. A missing block reads as empty. L3 calls them
# only for a file whose every block they read without an error.
block_extras() { yq -r ".\"${2}\".pip_extras // [] | join(\",\")" "$1"; }
block_packages() { yq -r ".\"${2}\".pip_packages // [] | .[]" "$1"; }

# The test-refs.yaml pins the tempest lock installs, as
# collect_tempest_inputs of hack/nix-lock.sh lists them.
TEMPEST_PINS="tempest barbican-tempest-plugin keystone-tempest-plugin neutron-tempest-plugin cinder-tempest-plugin"

# ---------------------------------------------------------------------------
# Releases
# ---------------------------------------------------------------------------
RELEASES=""
for dir in releases/*/; do
  [[ -d "${dir}" ]] || continue
  rel="${dir#releases/}"
  rel="${rel%/}"
  if [[ ! -f "releases/${rel}/source-refs.yaml" ]]; then
    fail "releases/${rel}: no source-refs.yaml"
    continue
  fi
  if ! yq -r 'keys | .[]' "releases/${rel}/source-refs.yaml" >"${TMP}/keys" 2>"${TMP}/err"; then
    fail "releases/${rel}/source-refs.yaml: $(first <"${TMP}/err")"
    continue
  fi
  mv "${TMP}/keys" "${TMP}/${rel}.keys" # audit-skills: allow
  mkdir -p "${TMP}/${rel}" # audit-skills: allow
  RELEASES="${RELEASES} ${rel}"
done

# ---------------------------------------------------------------------------
# L1 — inventory
# ---------------------------------------------------------------------------
hdr "L1: every lock of a release exists, and every directory under nix/locks/ is one"
for rel in ${RELEASES}; do
  while IFS= read -r name; do
    lock="nix/locks/${rel}/${name}"
    missing=""
    for f in sources.json pyproject.toml uv.lock; do
      [[ -f "${lock}/${f}" ]] || missing="${missing:+${missing}, }${f}"
    done
    if [[ -n "${missing}" ]]; then
      fail "${lock}: no ${missing}"
    else
      pass "${lock}: sources.json, pyproject.toml and uv.lock present"
    fi
  done < <(lock_names "${rel}")
done
for dir in nix/locks/*/ nix/locks/*/*/; do
  [[ -d "${dir}" ]] || continue
  dir="${dir%/}"
  rel="${dir#nix/locks/}"
  name=""
  if [[ "${rel}" == */* ]]; then
    name="${rel#*/}"
    rel="${rel%%/*}"
  fi
  if [[ ! -f "${TMP}/${rel}.keys" ]]; then
    # A release whose source-refs.yaml is missing or unreadable is a finding
    # already. The locks of a release that does not exist are reported once,
    # for the release's directory.
    [[ -n "${name}" || -d "releases/${rel}" ]] || fail "${dir}: no release pins it"
  elif [[ -n "${name}" ]] && ! has_line "${name}" "$(lock_names "${rel}")"; then
    fail "${dir}: no release pins it"
  fi
done

# ---------------------------------------------------------------------------
# L2 — source ref
# ---------------------------------------------------------------------------
hdr "L2: sources.json carries the pinned ref, its commit and hash, and the patches"
for rel in ${RELEASES}; do
  while IFS= read -r name; do
    src="nix/locks/${rel}/${name}/sources.json"
    [[ -f "${src}" ]] || continue
    if ! jq -e 'type == "object"' "${src}" >/dev/null 2>"${TMP}/err"; then
      fail "${src}: $(json_error)"
      continue
    fi
    problems=""
    ref="$(jq -r '.ref // ""' "${src}")"
    if [[ "${name}" == tempest ]]; then
      refs="releases/${rel}/test-refs.yaml"
      pin="$(yq -r '.tempest // ""' "${refs}" 2>/dev/null || true)"
    else
      refs="releases/${rel}/source-refs.yaml"
      pin="$(yq -r ".\"${name}\" // \"\"" "${refs}")"
    fi
    [[ "${ref}" == "${pin}" ]] ||
      problems="ref ${ref:-(none)} differs from ${pin:-(none)}, the ${name} pin of ${refs}"
    # nix/lib/python-env.nix attaches the fetched tree to the package of
    # this name.
    project="$(jq -r '.project // ""' "${src}")"
    if [[ "${name}" == tempest ]]; then
      [[ "${project}" == tempest ]] || problems="${problems:+${problems}; }project ${project:-(none)} is not tempest"
    else
      [[ -n "${project}" ]] || problems="${problems:+${problems}; }project is empty"
      rev="$(jq -r '.rev // ""' "${src}")"
      hash="$(jq -r '.hash // ""' "${src}")"
      is_rev "${rev}" || problems="${problems:+${problems}; }rev ${rev:-(none)} is no 40-character commit"
      [[ "${hash}" == sha256-* ]] || problems="${problems:+${problems}; }hash ${hash:-(none)} does not start with sha256-"
      patches=""
      for patch in "patches/${name}/${rel}"/*.patch; do
        [[ -f "${patch}" ]] && patches="${patches}${patch}"$'\n'
      done
      # The build applies the patches in list order.
      listed="$(jq -r '.patches // [] | .[]?' "${src}")"
      diff="$(differ "${patches}" "${listed}")"
      [[ -n "${diff}" || "${patches%$'\n'}" == "${listed}" ]] || diff="listed out of order"
      [[ -z "${diff}" ]] || problems="${problems:+${problems}; }patches differ from patches/${name}/${rel}/: ${diff}"
    fi
    if [[ -n "${problems}" ]]; then
      fail "${src}: ${problems}"
    elif [[ "${name}" == tempest ]]; then
      pass "${src}: ref matches ${refs}, project is tempest"
    else
      pass "${src}: ref, commit, hash and patches match the pins, project is ${project}"
    fi
  done < <(lock_names "${rel}")
done

# ---------------------------------------------------------------------------
# The merged constraints and the pyproject.toml entries L3 and L4 read
# ---------------------------------------------------------------------------
# merge <rel> writes ${TMP}/<rel>.constraints, the name==version form of the
# name===version lines of the merged upper-constraints.txt, and
# ${TMP}/<rel>.constrained, their PEP 503 names, as merge_constraints of
# hack/nix-lock.sh does; it returns 1 when the merge failed, which it reports
# as a finding of L4.
merge() {
  local rel="$1" work="${TMP}/merge-${1}" status=0
  mkdir -p "${work}/releases/${rel}" # audit-skills: allow
  if [[ -f "releases/${rel}/upper-constraints.txt" ]]; then
    cp "releases/${rel}/upper-constraints.txt" "${work}/releases/${rel}/" # audit-skills: allow
  fi
  if [[ -f "overrides/${rel}/constraints.txt" ]]; then
    mkdir -p "${work}/overrides/${rel}" # audit-skills: allow
    cp "overrides/${rel}/constraints.txt" "${work}/overrides/${rel}/" # audit-skills: allow
  fi
  (cd "${work}" && bash "${REPO_ROOT}/scripts/apply-constraint-overrides.sh" "${rel}" >/dev/null 2>"${TMP}/err") || status=$?
  if [[ "${status}" -ne 0 ]]; then
    fail "L4 ${rel}: apply-constraint-overrides.sh exited ${status} ($(first <"${TMP}/err"))"
    return 1
  fi
  awk '/^[A-Za-z0-9][A-Za-z0-9._-]*===/ { sub(/===/, "=="); print }' \
    "${work}/releases/${rel}/upper-constraints.txt" >"${TMP}/${rel}.constraints"
  awk '/^[A-Za-z0-9][A-Za-z0-9._-]*===/ { sub(/===.*/, ""); print }' \
    "${work}/releases/${rel}/upper-constraints.txt" | normalize >"${TMP}/${rel}.constrained"
}

# read_lock <rel> <name> writes ${TMP}/<rel>/<name>.entries, the entries of
# the lock's pyproject.toml, one tab-separated line each in file order: dep
# <item> for [project].dependencies, group <name> and member <name> <item> for
# [dependency-groups], constraint <item> for [tool.uv].constraint-dependencies,
# source <name> <rev> for [tool.uv.sources]. It returns 1, and L3 reports
# yq's message, when the file cannot be read or one of the lists holds
# anything but strings.
read_lock() {
  yq -p toml -o yaml -r '
    (.project.dependencies // []
      | (select(tag == "!!seq" and all_c(tag == "!!str")) // error("[project].dependencies is not a list of strings"))
      | .[] | "dep\t" + .),
    (.["dependency-groups"] // {} | to_entries | .[] | .key as $g
      | (("group\t" + $g), (.value
        | (select(tag == "!!seq" and all_c(tag == "!!str")) // error("[dependency-groups]." + $g + " is not a list of strings"))
        | .[] | "member\t" + $g + "\t" + .))),
    (.tool.uv["constraint-dependencies"] // []
      | (select(tag == "!!seq" and all_c(tag == "!!str")) // error("[tool.uv].constraint-dependencies is not a list of strings"))
      | .[] | "constraint\t" + .),
    (.tool.uv.sources // {} | to_entries | .[] | "source\t" + .key + "\t" + (.value.rev // ""))
  ' "nix/locks/${1}/${2}/pyproject.toml" >"${TMP}/${1}/${2}.entries" 2>"${TMP}/${1}/${2}.err"
}

# scripts/apply-constraint-overrides.sh deletes a constraint with the in-place
# edit and the I flag of GNU sed, which the sed of macOS and busybox lack: it
# would fail on every release there, so the merge is skipped instead.
gnu_sed() {
  printf 'A===1\n' >"${TMP}/probe"
  sed -i '/^a===/Id' "${TMP}/probe" 2>/dev/null && [[ ! -s "${TMP}/probe" ]] # audit-skills: allow
}

MERGED=""
if ! gnu_sed; then
  info "L4 skipped: scripts/apply-constraint-overrides.sh needs GNU sed; run the audit inside nix develop .#lock-tools"
else
  for rel in ${RELEASES}; do
    if merge "${rel}"; then
      MERGED="${MERGED} ${rel}"
    fi
  done
fi

# ---------------------------------------------------------------------------
# L3 — project inputs
# ---------------------------------------------------------------------------
hdr "L3: the dependencies of pyproject.toml carry the project, its packages and the base pins"
BASE_PINS="$(awk '/^[A-Za-z0-9][A-Za-z0-9._-]*==[^=]/' nix/base-requirements.txt)"
for rel in ${RELEASES}; do
  extra="releases/${rel}/extra-packages.yaml"
  # hack/nix-lock.sh stops on an extra-packages.yaml it cannot read, so no
  # lock derives from one: without it, L3 checks no extras, pip packages or
  # dependency groups of the release.
  extra_ok=0
  if [[ ! -f "${extra}" ]]; then
    fail "releases/${rel}: no extra-packages.yaml"
  elif [[ "$(yq 'tag' "${extra}" 2>"${TMP}/err")" != '!!map' ]]; then
    msg="$(first <"${TMP}/err")"
    fail "${extra}: ${msg:-not a YAML map}"
  else
    extra_ok=1
    # hack/nix-lock.sh reads a block with the expressions of block_extras and
    # block_packages and stops on one yq rejects, such as an extras scalar.
    while IFS= read -r block; do
      if ! { block_extras "${extra}" "${block}" && block_packages "${extra}" "${block}"; } >/dev/null 2>"${TMP}/err"; then
        fail "${extra}: the block ${block}: $(first <"${TMP}/err")"
        extra_ok=0
      fi
    done < <(yq -r 'keys | .[]' "${extra}")
  fi
  # The blocks of extra-packages.yaml without a source-refs.yaml key: the
  # images hack/nix-lock.sh builds from another key's lock, one dependency
  # group each.
  derived=""
  [[ "${extra_ok}" -eq 0 ]] ||
    derived="$(yq -r 'keys | .[]' "${extra}" | { grep -vxF -f "${TMP}/${rel}.keys" || true; })"
  if has_line "${rel}" "$(tr ' ' '\n' <<<"${MERGED}")"; then
    constrained="$(cat "${TMP}/${rel}.constrained")"
  else
    info "releases/${rel}: the base pins are not checked, the constraints did not merge"
    constrained=""
  fi
  while IFS= read -r name; do
    py="nix/locks/${rel}/${name}/pyproject.toml"
    [[ -f "${py}" ]] || continue
    if ! read_lock "${rel}" "${name}"; then
      fail "${py}: $(first <"${TMP}/${rel}/${name}.err")"
      continue
    fi
    deps="$(entries "${rel}" "${name}" dep)"
    problems=""
    wanted=""
    project=""
    # head_dep stays empty where a finding names its source already: a
    # sources.json without a project (L2), a test-refs.yaml without a
    # tempest pin, an extra-packages.yaml L3 cannot read.
    head_dep=""
    if [[ "${name}" == tempest ]]; then
      refs="releases/${rel}/test-refs.yaml"
      unpinned=""
      for pkg in ${TEMPEST_PINS}; do
        pin="$(yq -r ".\"${pkg}\" // \"\"" "${refs}" 2>/dev/null || true)"
        if [[ -z "${pin}" ]]; then
          unpinned="${unpinned:+${unpinned}, }${pkg}"
          continue
        fi
        wanted="${wanted}${pkg}==${pin}"$'\n'
        [[ "${pkg}" != tempest ]] || head_dep="tempest==${pin}"
      done
      [[ -z "${unpinned}" ]] || problems="${refs} pins no ${unpinned}"
    else
      project="$(jq -r '.project // ""' "nix/locks/${rel}/${name}/sources.json" 2>/dev/null || true)"
      if [[ "${extra_ok}" -eq 1 ]]; then
        extras="$(block_extras "${extra}" "${name}")"
        [[ -z "${project}" ]] || head_dep="${project}${extras:+[${extras}]}"
        wanted="$(block_packages "${extra}" "${name}")"
      fi
    fi
    # The base pins as add_base_pins of hack/nix-lock.sh lists them.
    if [[ -n "${constrained}" ]]; then
      while IFS= read -r line; do
        [[ -n "${line}" ]] || continue
        base="${line%%==*}"
        if has_line "$(normalize <<<"${base}")" "${constrained}"; then
          wanted="${wanted}"$'\n'"${base}"
        else
          wanted="${wanted}"$'\n'"${line}"
        fi
      done <<<"${BASE_PINS}"
    fi
    first_dep="$(first <<<"${deps}")"
    if [[ -n "${head_dep}" && "${first_dep}" != "${head_dep}" ]]; then
      problems="${problems:+${problems}; }the first dependency is ${first_dep:-(none)}, not ${head_dep}"
    fi
    if [[ "${name}" != tempest && -n "${constrained}" && "${extra_ok}" -eq 1 ]]; then
      # hack/nix-lock.sh lists nothing else after the project of a service.
      diff="$(differ "${wanted}" "$(awk 'NR > 1' <<<"${deps}")")"
      [[ -z "${diff}" ]] ||
        problems="${problems:+${problems}; }the dependencies after the project differ from the pip packages and base pins: ${diff}"
    else
      # The tempest lock also holds the clients of the e2e suites, without
      # the merged constraints the base pins are unknown, and without a
      # readable extra-packages.yaml the pip packages: the known pins only
      # have to be present.
      lacking=""
      while IFS= read -r pkg; do
        [[ -z "${pkg}" ]] || has_line "${pkg}" "${deps}" || lacking="${lacking}${pkg}"$'\n'
      done <<<"${wanted}"
      [[ -z "${lacking}" ]] || problems="${problems:+${problems}; }the dependencies lack $(some "${lacking}")"
    fi
    # uv resolves the project at the rev of [tool.uv.sources], the build
    # fetches the rev of sources.json.
    if [[ -n "${project}" ]]; then
      rev="$(jq -r '.rev // ""' "nix/locks/${rel}/${name}/sources.json" 2>/dev/null || true)"
      src_rev="$(entries "${rel}" "${name}" source | awk -F '\t' -v p="${project}" '$1 == p { print $2 }')"
      [[ "${src_rev}" == "${rev}" ]] ||
        problems="${problems:+${problems}; }[tool.uv.sources] fetches ${project} at ${src_rev:-(none)}, not at the rev ${rev:-(none)} of sources.json"
    fi
    # The group of a derived image: the project with the block's extras,
    # when it names any, then the block's pip packages.
    while IFS= read -r group; do
      [[ -n "${group}" ]] && has_line "${group}" "${derived}" || continue
      echo "${rel}/${name}" >>"${TMP}/group-${rel}-${group}"
      extras="$(block_extras "${extra}" "${group}")"
      expected="$(block_packages "${extra}" "${group}")"
      [[ -z "${extras}" ]] || expected="${project}[${extras}]"$'\n'"${expected}"
      diff="$(differ "${expected}" "$(entries "${rel}" "${name}" member | awk -F '\t' -v g="${group}" '$1 == g { print $2 }')")"
      [[ -z "${diff}" ]] || problems="${problems:+${problems}; }the group ${group} differs from its block in ${extra}: ${diff}"
    done <<<"$(entries "${rel}" "${name}" group)"
    if [[ -n "${problems}" ]]; then
      fail "${py}: ${problems}"
    else
      pass "${py}: project, extras, pip packages and base pins match the pins"
    fi
  done < <(lock_names "${rel}")
  while IFS= read -r group; do
    [[ -n "${group}" ]] || continue
    locks="$(cat "${TMP}/group-${rel}-${group}" 2>/dev/null || true)"
    if [[ -z "${locks}" ]]; then
      fail "${extra}: the block ${group} is the dependency group of no lock"
    elif [[ "$(wc -l <<<"${locks}" | tr -d ' ')" -gt 1 ]]; then
      fail "${extra}: the block ${group} is a dependency group of $(some "${locks}")"
    fi
  done <<<"${derived}"
done

# ---------------------------------------------------------------------------
# L4 — constraints
# ---------------------------------------------------------------------------
hdr "L4: constraint-dependencies equals the merged upper-constraints"
for rel in ${MERGED}; do
  while IFS= read -r name; do
    py="nix/locks/${rel}/${name}/pyproject.toml"
    # A missing or unreadable file is a finding of L1 or L3 already.
    [[ -s "${TMP}/${rel}/${name}.entries" ]] || continue
    diff="$(differ "$(cat "${TMP}/${rel}.constraints")" "$(entries "${rel}" "${name}" constraint)")"
    if [[ -n "${diff}" ]]; then
      fail "${py}: constraint-dependencies differ from the merged upper-constraints: ${diff}"
    else
      pass "${py}: constraint-dependencies match the merged upper-constraints"
    fi
  done < <(lock_names "${rel}")
done

# ---------------------------------------------------------------------------
# L5 — image pins
# ---------------------------------------------------------------------------
hdr "L5: the image pins name a commit and its hashes"
found=0
for pin in images/*/source.json images/*/novnc.json; do
  [[ -f "${pin}" ]] || continue
  found=1
  if ! jq -e 'type == "object"' "${pin}" >/dev/null 2>"${TMP}/err"; then
    fail "${pin}: $(json_error)"
    continue
  fi
  dir="${pin%/*}"
  problems=""
  checked="rev and hash"
  is_rev "$(jq -r '.rev // ""' "${pin}")" || problems="rev is no 40-character commit"
  [[ -n "$(jq -r '.hash // ""' "${pin}")" ]] || problems="${problems:+${problems}; }hash is empty"
  if grep -qF 'pin.ovs' "${dir}"/*.nix 2>/dev/null; then
    checked="${checked}, ovs.rev and ovs.hash"
    is_rev "$(jq -r '.ovs.rev? // ""' "${pin}")" || problems="${problems:+${problems}; }ovs.rev is no 40-character commit"
    [[ -n "$(jq -r '.ovs.hash? // ""' "${pin}")" ]] || problems="${problems:+${problems}; }ovs.hash is empty"
  fi
  if [[ -f "${dir}/manager.nix" ]]; then
    checked="${checked}, vendorHash"
    [[ -n "$(jq -r '.vendorHash // ""' "${pin}")" ]] || problems="${problems:+${problems}; }vendorHash is empty"
  fi
  if [[ -n "${problems}" ]]; then
    fail "${pin}: ${problems}"
  else
    pass "${pin}: ${checked} set"
  fi
done
[[ "${found}" -eq 1 ]] || info "no image pin under images/"

# ---------------------------------------------------------------------------
# L6 — operator vendor hashes
# ---------------------------------------------------------------------------
hdr "L6: the operator vendor hashes are current"
status=0
out="$(bash hack/nix-lock.sh --check-operators 2>&1)" || status=$?
if [[ "${status}" -eq 0 ]]; then
  pass "hack/nix-lock.sh --check-operators: the operator vendor hashes are current"
else
  stale="$(grep -v '^nix-lock.sh: ' <<<"${out}" | awk 'NF' || true)"
  fail "hack/nix-lock.sh --check-operators exited ${status}: $(some "${stale:-$(first <<<"${out}")}")"
fi

# ---------------------------------------------------------------------------
# L7 — age of a lock and its pins ([INFO] only)
# ---------------------------------------------------------------------------
hdr "L7: locks older than a pin they derive from ([INFO] only)"
# newest <path>... — "<committer time> <date> <file>" of the newest commit
# that touched one of the paths, with the first of them it touched; empty
# when none did.
newest() {
  git log -1 --format='%ct %cs' --name-only -- "$@" |
    awk 'NR == 1 { at = $0; next } NF && file == "" { file = $0 } END { if (at != "") print at " " file }'
}

shallow=""
if ! command -v git >/dev/null 2>&1; then
  info "L7 skipped: git is missing"
elif ! shallow="$(git rev-parse --is-shallow-repository 2>"${TMP}/err")"; then
  info "L7 skipped: git cannot read the checkout ($(first <"${TMP}/err"))"
elif [[ "${shallow}" == true ]]; then
  info "L7 skipped: shallow clone"
else
  for rel in ${RELEASES}; do
    while IFS= read -r name; do
      lock="nix/locks/${rel}/${name}"
      if [[ "${name}" == tempest ]]; then
        pins=("releases/${rel}/test-refs.yaml")
      else
        pins=("releases/${rel}/source-refs.yaml" "releases/${rel}/extra-packages.yaml" "patches/${name}/${rel}")
      fi
      pins+=("releases/${rel}/upper-constraints.txt" "overrides/${rel}/constraints.txt" nix/base-requirements.txt)
      lock_at="$(newest "${lock}")"
      pin_at="$(newest "${pins[@]}")"
      lock_date="${lock_at#* }"
      lock_date="${lock_date%% *}"
      if [[ -z "${lock_at}" ]]; then
        info "${lock}: no commit yet"
      elif [[ -n "${pin_at}" && "${pin_at%% *}" -gt "${lock_at%% *}" ]]; then
        pin_date="${pin_at#* }"
        info "${lock}: ${pin_date#* } changed on ${pin_date%% *}, after the lock (${lock_date}); make nix-lock-check decides"
      else
        info "${lock}: no pin changed after the lock (${lock_date})"
      fi
    done < <(lock_names "${rel}")
  done
fi

# ---------------------------------------------------------------------------
hdr "Summary"
if [[ ${FAIL_COUNT} -eq 0 ]]; then
  echo "[PASS] no lock drift findings; make nix-lock-check is the authoritative gate"
  exit 0
else
  echo "[FAIL] ${FAIL_COUNT} lock drift finding(s); fix with make nix-lock, confirm with make nix-lock-check"
  exit 1
fi
