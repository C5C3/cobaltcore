#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check of the ceph-bindings.pth the cinder, glance and nova-compute
# images write, for their verify scripts under tests/container-images/.
#
# The venv sees the rados and rbd modules through one directory,
# /var/lib/openstack/ceph-bindings, that a ceph-bindings.pth in the venv's
# site-packages names, so the two modules resolve while no other system
# package does. The modules come from one of two origins:
#
#   apt  nova-compute, built by images/nova-compute/Dockerfile: noble
#        installs the bindings into /usr/lib/python3/dist-packages together
#        with every other python3-* package an apt entry pulls in, and the
#        Dockerfile links the two extension modules into the directory.
#   nix  cinder and glance, built by nix/lib/service-image.nix: the
#        directory is the site-packages of nix/pkgs/ceph-bindings.nix, a
#        store path that holds the two modules alone.
#
# Kept here so a change to the lines is edited once instead of once per
# image, where missing one checks that image against a stale expectation.
#
# The .pth and the import are checked apart, so a lost .pth and a lost package
# fail differently. The resolved module paths prove the origin, not a stray
# wheel. dist-packages must be absent from sys.path, so no noble module the
# venv lacks resolves from there: in nova-compute ceph-common pulls in
# python3-chardet, which requests would try before the venv's
# charset_normalizer.

# The directory the images put rados and rbd into; the .pth names it.
CEPH_BINDINGS_DIR=/var/lib/openstack/ceph-bindings
# Where noble's python3-rados and python3-rbd install the two modules.
CEPH_BINDINGS_SYSTEM_DIR=/usr/lib/python3/dist-packages
# The store path of nix/pkgs/ceph-bindings.nix, built for Python 3.12.
CEPH_BINDINGS_STORE_PATTERN='^/nix/store/[a-z0-9]{32}-python3\.12-ceph-bindings-'

# assert_ceph_bindings_wired [apt|nix]
# Assert the .pth, the import of rados and rbd from the given origin (apt by
# default), and the sys.path the venv ends up with. Needs $IMAGE and the
# caller's PASS/FAIL counters. Stderr is echoed on failure so the missing
# module is named.
assert_ceph_bindings_wired() {
  local origin="${1:-apt}" output exit_code=0 purelib paths path module

  purelib=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import sysconfig; print(sysconfig.get_path("purelib"))' 2>/dev/null) || exit_code=$?
  assert_eq "reading the venv's site-packages path exits 0" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" test -f "$purelib/ceph-bindings.pth" || exit_code=$?
  assert_eq "ceph-bindings.pth is present in the venv's site-packages" "0" "$exit_code"

  output=$(docker run --rm "$IMAGE" cat "$purelib/ceph-bindings.pth" 2>&1) || true
  assert_eq "ceph-bindings.pth names $CEPH_BINDINGS_DIR" "$CEPH_BINDINGS_DIR" "$output"

  exit_code=0
  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import os, rados, rbd
print(os.path.realpath(rados.__file__), os.path.realpath(rbd.__file__))' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "import rados, rbd exits 0" "0" "$exit_code"
  paths="${output##*$'\n'}"
  if [ "$origin" = nix ]; then
    for module in rados rbd; do
      if [ "$module" = rados ]; then path="${paths% *}"; else path="${paths#* }"; fi
      if [[ "$path" =~ $CEPH_BINDINGS_STORE_PATTERN ]]; then
        echo "  PASS: $module comes from nix/pkgs/ceph-bindings.nix"
        PASS=$((PASS + 1))
      else
        echo "  FAIL: $module comes from nix/pkgs/ceph-bindings.nix"
        echo "    expected a path matching: $CEPH_BINDINGS_STORE_PATTERN"
        echo "    actual:                   $path"
        FAIL=$((FAIL + 1))
      fi
    done
  else
    assert_starts_with "rados comes from the apt package" "${paths% *}" "$CEPH_BINDINGS_SYSTEM_DIR/"
    assert_starts_with "rbd comes from the apt package" "${paths#* }" "$CEPH_BINDINGS_SYSTEM_DIR/"
  fi

  exit_code=0
  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    "import sys
sys.exit('$CEPH_BINDINGS_SYSTEM_DIR is on sys.path'
         if '$CEPH_BINDINGS_SYSTEM_DIR' in sys.path else 0)" \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $output"
  assert_eq "$CEPH_BINDINGS_SYSTEM_DIR stays off the venv's sys.path" "0" "$exit_code"
}
