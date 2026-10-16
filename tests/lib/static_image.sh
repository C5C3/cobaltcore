#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check of the images nix/lib/static-image.nix builds, for
# verify_hvo.sh and verify_kna.sh under tests/container-images/.
#
# static-image.nix stands in for gcr.io/distroless/static:nonroot, so the
# image has to carry what that base gave a static binary: the CA bundle and
# SSL_CERT_FILE for TLS, /etc/passwd and /etc/group with root (home /root)
# and nonroot (65532), /etc/services, /usr/share/zoneinfo, /tmp with mode
# 1777, /root with mode 0700, and /home/nonroot owned by 65532 as the
# WorkingDir. The image has no shell, so the check reads the files of a
# created container with docker cp and their modes and owners from the
# listing of docker export. The owner pattern matches the listings of GNU
# tar (65532/65532) and of bsdtar (65532  65532).
#
# The caller sources tests/lib/assertions.sh, sets IMAGE and keeps the PASS
# and FAIL counters.

# static_image_file <container> <path> prints the file at <path>, following
# links, or nothing when it is missing.
static_image_file() {
  docker cp -L "$1:$2" - 2> /dev/null | tar -xOf - 2> /dev/null || true
}

# static_image_entry <listing> <path> prints the line of <path>, relative to
# the root, in a `tar -tv` listing.
static_image_entry() {
  grep -E " $2/?\$" <<< "$1" || true
}

test_static_image_layout() {
  echo "Test: the image carries the files and the configuration of the distroless base"
  local cid listing owned=no

  cid=$(docker create "$IMAGE")
  listing=$(docker export "$cid" | tar -tvf -)

  assert_contains "the CA bundle ships" \
    "$(static_image_file "$cid" /etc/ssl/certs/ca-certificates.crt)" "-----BEGIN CERTIFICATE-----"
  assert_contains "/etc/passwd gives root the home /root" \
    "$(static_image_file "$cid" /etc/passwd)" "root:x:0:0:root:/root:/sbin/nologin"
  assert_contains "/etc/passwd names nonroot 65532" \
    "$(static_image_file "$cid" /etc/passwd)" "nonroot:x:65532:65532:nonroot:/home/nonroot:/sbin/nologin"
  assert_contains "/etc/group names nonroot 65532" \
    "$(static_image_file "$cid" /etc/group)" "nonroot:x:65532:"
  assert_contains "/etc/services names https" \
    "$(static_image_file "$cid" /etc/services)" "https"
  assert_starts_with "/usr/share/zoneinfo/UTC is a zone file" \
    "$(static_image_file "$cid" /usr/share/zoneinfo/UTC | tr -d '\000')" "TZif"

  assert_starts_with "/tmp has mode 1777" "$(static_image_entry "$listing" tmp)" "drwxrwxrwt"
  assert_starts_with "/root has mode 0700" "$(static_image_entry "$listing" root)" "drwx------"
  if [[ "$(static_image_entry "$listing" home/nonroot)" =~ [[:space:]]65532[/[:space:]]+65532[[:space:]] ]]; then
    owned=yes
  fi
  assert_eq "/home/nonroot is owned by 65532:65532" "yes" "$owned"
  docker rm "$cid" > /dev/null

  assert_eq "WorkingDir is /home/nonroot" "/home/nonroot" \
    "$(docker inspect --format '{{.Config.WorkingDir}}' "$IMAGE")"
  assert_contains "SSL_CERT_FILE names the CA bundle" \
    "$(docker inspect --format '{{.Config.Env}}' "$IMAGE")" "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
}
