# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The image of a static Go binary, in place of the
# gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
# base the Dockerfiles of these images ran on. images/<name>/image.nix calls
# it with the manager its manager.nix builds, and nix/lib/operator-image.nix
# with the manager of each CobaltCore operator.
#
# The image carries what the distroless base gave a static binary: the
# base's /etc/passwd and /etc/group (root with the home /root, nobody and
# nonroot, 65532) and an /etc/nsswitch.conf, the CA bundle, /etc/services and
# /etc/protocols from iana-etc, /usr/share/zoneinfo from tzdata, /tmp with
# mode 1777, /root with mode 0700 and /home/nonroot owned by 65532. The
# binary sits at /usr/bin/manager, a link to the store path of the build.
# The configuration is that of the base, User, WorkingDir and Env, plus the
# Entrypoint, without a Cmd. The labels are the licence and vendor labels of
# nix/lib/image-defaults.nix plus title and description, and two optional
# ones: org.opencontainers.image.version and io.c5c3.upstream-commit, the
# upstream commit the binary is built from. The hvo and kna images pass an
# upstream commit and keep the tag as their version; the operator images pass
# neither, so they carry the four static labels alone.
#
# Arguments:
#   name, tag        the image reference docker load prints
#   description      the org.opencontainers.image.description label
#   manager          a derivation with bin/manager
#   upstreamCommit   the io.c5c3.upstream-commit label; null for none
#   version          the org.opencontainers.image.version label, the tag by
#                    default; null for none
#   user             Config.User, "uid:gid"; nonroot by default
{ pkgs }:
let
  # The uid and gid of the base's nonroot user.
  nonroot = "65532";
in
{
  name,
  tag,
  description,
  manager,
  upstreamCommit ? null,
  version ? tag,
  user ? "${nonroot}:${nonroot}",
}:
let
  inherit (pkgs) lib;
  defaults = import ./image-defaults.nix;

  # The lines of the base's files. dockerTools.fakeNss differs: its root
  # has the home /var/empty, which no process can write.
  nss = pkgs.runCommand "static-image-nss" { } ''
    mkdir -p "$out/etc"
    printf '%s\n' \
      'root:x:0:0:root:/root:/sbin/nologin' \
      'nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin' \
      'nonroot:x:${nonroot}:${nonroot}:nonroot:/home/nonroot:/sbin/nologin' >"$out/etc/passwd"
    printf '%s\n' 'root:x:0:' 'nobody:x:65534:' 'tty:x:5:' 'staff:x:50:' \
      'nonroot:x:${nonroot}:' >"$out/etc/group"
    echo 'hosts: files dns' >"$out/etc/nsswitch.conf"
  '';

  root = pkgs.runCommand "${name}-root" { } ''
    mkdir -p "$out/usr/bin" "$out/usr/share"
    ln -s ${manager}/bin/manager "$out/usr/bin/manager"
    ln -s ${pkgs.tzdata}/share/zoneinfo "$out/usr/share/zoneinfo"
  '';
in
pkgs.dockerTools.streamLayeredImage {
  inherit name tag;
  inherit (defaults) created;
  passthru = { inherit manager; };

  contents = [
    nss
    pkgs.dockerTools.caCertificates
    pkgs.iana-etc
    root
  ];

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p -m 1777 /tmp
    chmod 1777 /tmp
    mkdir -p -m 0700 /root
    mkdir -p /home/nonroot
    chown ${nonroot}:${nonroot} /home/nonroot
  '';

  config = {
    User = user;
    Entrypoint = [ "/usr/bin/manager" ];
    WorkingDir = "/home/nonroot";
    Env = [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
    ];
    Labels =
      defaults.labels
      // {
        "org.opencontainers.image.title" = name;
        "org.opencontainers.image.description" = description;
      }
      // lib.optionalAttrs (version != null) { "org.opencontainers.image.version" = version; }
      // lib.optionalAttrs (upstreamCommit != null) { "io.c5c3.upstream-commit" = upstreamCommit; };
  };
}
