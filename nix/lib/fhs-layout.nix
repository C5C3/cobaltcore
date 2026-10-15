# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The FHS façade of a Nix-built image: the in-image paths the operators and
# the verify scripts name as literals, resolved into /nix/store.
#
#   /var/lib/openstack     a symlink to a symlinkJoin of venv and extraRoots,
#                          so bin/* and etc/<service>/* resolve into the store
#   /bin/sh, /bin/bash     bash
#   /usr/bin/env           coreutils
#   /etc/ssl/certs/ca-certificates.crt
#                          dockerTools.caCertificates
#   /etc/passwd, /etc/group, /etc/nsswitch.conf
#                          dockerTools.fakeNss with the openstack user and
#                          group (42424) of the Docker images
#
# venv is the Python environment of a service image. An image without one
# (venv = null, the default) still gets every path above; /var/lib/openstack,
# the home directory of user 42424, is then an empty directory unless
# extraRoots names something.
#
# extraRoots maps a path below /var/lib/openstack to a store path, rootLinks
# a path relative to the image root (usr/share/novnc) to one. A target that
# does not exist fails the build and names the path.
{ pkgs }:
{
  venv ? null,
  extraRoots ? { },
  rootLinks ? { },
}:
let
  inherit (pkgs) lib;
  defaults = import ./image-defaults.nix;
  uid = toString defaults.uid;
  gid = toString defaults.gid;

  # The shell lines that link each entry of an attribute set below $out.
  linkAll =
    argument: entries:
    lib.concatStrings (
      lib.mapAttrsToList (rel: target: ''
        if [ ! -e ${lib.escapeShellArg target} ]; then
          echo ${lib.escapeShellArg "fhs-layout: ${argument}.\"${rel}\" names ${target}, which does not exist"} >&2
          exit 1
        fi
        mkdir -p "$(dirname "$out/"${lib.escapeShellArg rel})"
        ln -s ${lib.escapeShellArg target} "$out/"${lib.escapeShellArg rel}
      '') entries
    );

  extras = pkgs.runCommand "var-lib-openstack-extra-roots" { } (
    ''
      mkdir -p "$out"
    ''
    + linkAll "extraRoots" extraRoots
  );

  openstackRoot = pkgs.symlinkJoin {
    name = "var-lib-openstack";
    paths = lib.optional (venv != null) venv ++ [ extras ];
  };

  links = pkgs.runCommand "fhs-links" { } (
    ''
      mkdir -p "$out/var/lib" "$out/bin" "$out/usr/bin"
      ln -s ${openstackRoot} "$out/var/lib/openstack"
      ln -s ${pkgs.bash}/bin/bash "$out/bin/sh"
      ln -s ${pkgs.bash}/bin/bash "$out/bin/bash"
      ln -s ${pkgs.coreutils}/bin/env "$out/usr/bin/env"
    ''
    + linkAll "rootLinks" rootLinks
  );

  nss = pkgs.dockerTools.fakeNss.override {
    extraPasswdLines = [ "openstack:x:${uid}:${gid}::/var/lib/openstack:/usr/sbin/nologin" ];
    extraGroupLines = [ "openstack:x:${gid}:" ];
  };
in
pkgs.symlinkJoin {
  name = "fhs-layout";
  paths = [
    links
    nss
    pkgs.dockerTools.caCertificates
  ];
}
