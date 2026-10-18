# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# OVN and Open vSwitch daemons and client tools, built by images/ovn/ovn.nix
# from the commits images/ovn/source.json pins, in one runtime image.
# Release-independent: the image has no key in releases/*/source-refs.yaml,
# and the OVN version, not a release, names its tag (decision D1 of #898).
#
# The image is the FHS façade of nix/lib/fhs-layout.nix with two root links,
# /usr/share/ovn and /usr/share/openvswitch, into the store path of the
# build, plus the commands the operators run in it. ovn-operator calls
# /usr/share/ovn/scripts/ovn-ctl, which finds ovs-lib by rewriting its own
# path, so both trees must sit side by side under /usr/share, and reads
# /usr/share/openvswitch/vswitch.ovsschema. The ovn-ctl and ovs-ctl scripts
# need coreutils (id, install, date, seq, sleep), findutils, grep, sed and
# awk; the database, relay and backup reconcilers call hostname -f, find and
# the ovsdb tools. kmod (modprobe) and iproute2 (ip) serve the chassis
# DaemonSets of #903, which load host kernel modules from inside this image
# and inspect interfaces; nixpkgs' kmod searches /lib/modules after the two
# NixOS paths, and the chassis pods mount the host's /lib/modules there.
# iputils (ping) serves the datapath probes of
# tests/e2e-ovn-overlay/geneve-datapath and of the ovn-southbound-outage
# chaos suite, which run as root: a store path carries no file capability.
# ldd is glibc's script, which verify_ovn.sh runs on the daemons. No python3,
# no compiler and no git here.
#
# /var/run links to /run, as in noble. The runtime state, configuration and
# log directories of both daemon families exist, owned by the user the
# daemons run as. ovsdb-server, ovn-northd and the client tools need no
# privileges, so the default user is 42424; the chassis pods of #903 set
# runAsUser 0 and the capabilities they need themselves. The image has no
# Entrypoint and no Cmd and no component switch: every workload names the
# daemon it runs.
#
# DEVIATION from the original build-pipeline design: the image is built
# without nix/lib/service-image.nix, so it carries the generic 'openstack'
# user and group (UID/GID 42424) itself, through the fakeNss entry of the
# façade. The distro packages would install per-daemon 'openvswitch' and
# 'ovn' users, but the source build carries no such packaging, and one
# identity across all service images keeps the pod security contexts
# uniform.
{ pkgs, fhsLayout, ... }:
let
  inherit (pkgs) lib;
  defaults = import ../../nix/lib/image-defaults.nix;
  uid = toString defaults.uid;
  gid = toString defaults.gid;
  ovn = import ./ovn.nix { inherit pkgs; };
  ldd = pkgs.runCommand "ldd" { } "mkdir -p $out/bin && ln -s ${pkgs.glibc.bin}/bin/ldd $out/bin/ldd";
  stateDirs = [
    "/run/ovn"
    "/run/openvswitch"
    "/var/lib/ovn"
    "/var/lib/openvswitch"
    "/var/log/ovn"
    "/var/log/openvswitch"
    "/etc/ovn"
    "/etc/openvswitch"
  ];
in
pkgs.dockerTools.streamLayeredImage {
  name = "ovn";
  tag = ovn.version;
  inherit (defaults) created;
  passthru = { inherit ovn; };

  contents = [
    (fhsLayout {
      rootLinks = {
        "usr/share/ovn" = "${ovn}/share/ovn";
        "usr/share/openvswitch" = "${ovn}/share/openvswitch";
      };
    })
    ovn
    pkgs.coreutils
    pkgs.findutils
    pkgs.gnugrep
    pkgs.gnused
    pkgs.gawk
    pkgs.hostname
    pkgs.kmod
    pkgs.iproute2
    pkgs.iputils
    ldd
  ];

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p -m 1777 /tmp
    chmod 1777 /tmp
    mkdir -p /run
    ln -s /run /var/run
  ''
  + lib.concatMapStrings (dir: ''
    mkdir -p ${dir}
    chown -R ${uid}:${gid} ${dir}
  '') stateDirs;

  config = {
    User = "${uid}:${gid}";
    Env = [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      "LANG=C.UTF-8"
    ];
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "ovn";
      "org.opencontainers.image.description" =
        "OVN and Open vSwitch daemons and client tools built from pinned upstream sources";
      "org.opencontainers.image.version" = ovn.version;
    };
  };
}
