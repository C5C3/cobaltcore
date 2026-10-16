#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-nix-build-e2e-images.sh — Build the Nix images of an e2e run.
#
# Runs inside the nixos/nix container step "Build Nix images" of the
# build-e2e-images job (.github/workflows/ci.yaml), with the workspace as its
# working directory. It reads the image set hack/ci-resolve-e2e-images.sh
# wrote:
#
#   BUILD_SERVICE_IMAGES    one "<service> <release> [<image>]" line per image;
#                           a line without a third field is the service's own
#                           image, a line with one (nova-compute) the derived
#                           image of that name built from the service's lock
#   BUILD_TEMPEST_RELEASES  the releases whose Tempest image is built
#   BUILD_OVN               true when the ovn image is built
#   OVN_VERSION             the pinned OVN version, the tag docker load
#                           prints for the ovn image; required with BUILD_OVN
#   BUILD_LIBVIRT           true when the libvirt image is built
#
# It builds the flake attributes <name>-<rel_>-image, <rel_> the release with
# its dot replaced by _ and <name> the service or, on a three-field line, the
# derived image, and ovn-image and libvirt-image, which carry no release, in
# one nix invocation that runs one job per CPU, so the derivations of every
# image build side by side. It then writes each image stream to
# .nix-e2e/<name>-<release>.tar and appends "<name> <release>" to
# .nix-e2e/images.txt, which the "Load Nix images" step reads to load and tag
# the images on the runner; for the ovn image the release is OVN_VERSION, for
# the libvirt image latest, the tags docker load prints for them.
#
# The container is unprivileged and cannot create the namespaces of the Nix
# sandbox, so the builds run without it. The builders, which run the setup
# code of every sdist the locks build, therefore run as build users of their
# own, never as the container's root: the runner mounts its Docker socket and
# the workspace into the container, and a build user can neither open the
# socket nor write the workspace. It can read /github/runner_temp, so the job
# checks out without persisting its token there. The group e2ebld (gid 40000)
# and its users (uids 40001 and up, one per job) are written to the
# container's account files ($NIX_E2E_ETC, /etc by default), because the
# image's nixbld users have the uids of the build users of a Nix daemon on the
# host, and such a daemon kills every process of a build uid whenever one of
# its own builds ends, a container build included. Nix
# substitutes from cache.nixos.org and, once CACHIX_PUBLIC_KEY is set, from
# the cobaltcore Cachix cache; an unreachable cache only warns and Nix builds
# the rest from source. The flake is evaluated as a git checkout owned by the
# runner's user, which git refuses without the safe.directory entry. The EXIT
# trap hands .nix-e2e to the owner of the workspace, so the runner can clean
# the files the container's root wrote.
#
# Exit codes: 0 when every image is built or there is nothing to build; 1 on a
# malformed line or a BUILD_OVN without OVN_VERSION; nix's status when an
# attribute is missing or a build fails.

set -euo pipefail

git config --global --add safe.directory "$GITHUB_WORKSPACE"
cd "$GITHUB_WORKSPACE"

# The public signing key of the cobaltcore Cachix cache, .publicSigningKeys[0]
# of https://app.cachix.org/api/v1/cache/cobaltcore. Empty while the cache
# does not exist (the API answered 401 on 2026-10-15): without a key the cache
# is not passed to Nix.
CACHIX_PUBLIC_KEY=""

NIX=(nix --extra-experimental-features 'nix-command flakes')
OUT=.nix-e2e
ETC="${NIX_E2E_ETC:-/etc}"

mkdir -p "$OUT"
: >"$OUT/images.txt"
trap 'chown -R "$(stat -c %u:%g "$GITHUB_WORKSPACE")" "$OUT"' EXIT

# images holds "<name> <release>" and attrs the flake attribute of each, at
# the same index.
images=()
attrs=()
while IFS= read -r line; do
  read -r -a fields <<<"$line"
  case "${#fields[@]}" in
    0) ;;
    2)
      images+=("${fields[0]} ${fields[1]}")
      attrs+=(".#${fields[0]}-${fields[1]//./_}-image")
      ;;
    3)
      images+=("${fields[2]} ${fields[1]}")
      attrs+=(".#${fields[2]}-${fields[1]//./_}-image")
      ;;
    *)
      echo "ci-nix-build-e2e-images.sh: malformed line '${line}'" >&2
      exit 1
      ;;
  esac
done <<<"${BUILD_SERVICE_IMAGES:-}"
for release in ${BUILD_TEMPEST_RELEASES:-}; do
  images+=("tempest ${release}")
  attrs+=(".#tempest-${release//./_}-image")
done
if [ "${BUILD_OVN:-}" = "true" ]; then
  if [ -z "${OVN_VERSION:-}" ]; then
    echo "ci-nix-build-e2e-images.sh: BUILD_OVN is true but OVN_VERSION is empty" >&2
    exit 1
  fi
  images+=("ovn ${OVN_VERSION}")
  attrs+=(".#ovn-image")
fi
if [ "${BUILD_LIBVIRT:-}" = "true" ]; then
  images+=("libvirt latest")
  attrs+=(".#libvirt-image")
fi

if [ "${#images[@]}" -eq 0 ]; then
  echo "ci-nix-build-e2e-images.sh: nothing to build"
  exit 0
fi

substituters=()
if [ -n "$CACHIX_PUBLIC_KEY" ]; then
  substituters=(--option extra-substituters https://cobaltcore.cachix.org
    --option extra-trusted-public-keys "$CACHIX_PUBLIC_KEY")
fi

jobs=$(nproc)
members=()
for i in $(seq 1 "$jobs"); do
  members+=("e2ebld${i}")
  echo "e2ebld${i}:x:$((40000 + i)):40000:e2e build user ${i}:/var/empty:/bin/false" >>"$ETC/passwd"
done
(IFS=,; echo "e2ebld:x:40000:${members[*]}") >>"$ETC/group"

echo "::group::Building ${attrs[*]}"
"${NIX[@]}" build --option sandbox false --option build-users-group e2ebld --max-jobs "$jobs" \
  ${substituters[@]+"${substituters[@]}"} \
  "${attrs[@]}" --print-build-logs --out-link "$OUT/result"
echo "::endgroup::"

# nix build links the result of its first attribute at result and that of
# the attribute at index i at result-<i>.
for i in "${!images[@]}"; do
  name="${images[$i]% *}"
  release="${images[$i]#* }"
  link="$OUT/result"
  [ "$i" -eq 0 ] || link="$OUT/result-$i"
  "$link" >"$OUT/${name}-${release}.tar"
  echo "${name} ${release}" >>"$OUT/images.txt"
done
