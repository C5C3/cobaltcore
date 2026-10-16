# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# OVN and Open vSwitch daemons and client tools, compiled from the commits
# images/ovn/source.json pins. images/ovn/image.nix ships the result.
# Release-independent: the image has no key in releases/*/source-refs.yaml
# and is not part of the per-service, per-release build matrix, so it is
# built once per OVN version instead of once per OpenStack release.
#
# Open vSwitch comes from the commit that OVN's 'ovs' git submodule points
# to at the pinned OVN commit (decision D1 of #898: one pin, one Renovate
# rule). OVN's Documentation/intro/install/general.rst notes under Build
# Requirements that the submodule is "not recommended to be used as a
# source for OVS build". ovn-kubernetes builds its OVS from exactly that
# gitlink in dist/images/Dockerfile.fedora, and D1 keeps the gitlink.
#
# Both trees are fetched by commit and NAR hash, never through the tag or
# upstream's .gitmodules: `make nix-lock` reads the gitlink at the pinned OVN
# commit into ovs.rev and writes both hashes, so a moved tag or gitlink
# fails the lock check instead of changing the image. nixpkgs' ovn package
# is no substitute: it pins its own tag, fetches OVS through the submodule,
# installs no OVS daemons and compiles a store path as its sysconfdir.
#
# Both trees are configured with --prefix=$out, so the binaries and the
# ovs-ctl and ovn-ctl scripts run from the store, and with
# --localstatedir=/var and --sysconfdir=/etc, so the daemons and the scripts
# look for their runtime state, logs and configuration where the operators
# mount them. The install overrides only redirect the directories `make
# install` creates; the compiled-in paths stay /var/run/ovn,
# /var/run/openvswitch, /var/log/openvswitch and /etc. stdenv's move-sbin
# hook moves sbin/ into bin/ and leaves sbin -> bin.
{ pkgs }:
let
  inherit (pkgs) lib;
  pin = lib.importJSON ./source.json;
  version = lib.removePrefix "v" pin.version;
  # The store path of a fetch depends on its name and hash only. The rev in
  # the name makes a rev moved without its hash fail the fetch instead of
  # building the tree of the old hash.
  ovsSrc = pkgs.fetchFromGitHub {
    name = "ovs-${pin.ovs.rev}-source";
    owner = "openvswitch";
    repo = "ovs";
    inherit (pin.ovs) rev hash;
  };
in
pkgs.stdenv.mkDerivation {
  pname = "ovn";
  inherit version;

  src = pkgs.fetchFromGitHub {
    name = "ovn-${pin.rev}-source";
    owner = "ovn-org";
    repo = "ovn";
    inherit (pin) rev hash;
  };

  # The OVN tree carries the submodule as an empty directory. cp keeps the
  # file modes of the OVS tree, so boot.sh stays executable.
  postUnpack = ''
    rm -rf "$sourceRoot/ovs"
    cp -r ${ovsSrc} "$sourceRoot/ovs"
    chmod -R u+w "$sourceRoot/ovs"
  '';

  postPatch = ''
    patchShebangs --build .
  '';

  # Build requirements from OVN's Documentation/intro/install/general.rst:
  # GNU make, a C compiler, libssl, libunbound, autoconf, automake and
  # libtool. OVS also needs python3 for its code generation.
  nativeBuildInputs = with pkgs; [
    autoconf
    automake
    libtool
    pkg-config
    python3
  ];
  buildInputs = with pkgs; [
    libcap_ng
    openssl
    unbound
  ];

  dontConfigure = true;

  # OVS is built in tree first, then OVN against that tree:
  # --with-ovs-source is enough for an in-tree OVS build.
  buildPhase = ''
    runHook preBuild
    (cd ovs && ./boot.sh && ./configure --prefix=$out --localstatedir=/var --sysconfdir=/etc && make -j"$NIX_BUILD_CORES")
    ./boot.sh
    ./configure --prefix=$out --localstatedir=/var --sysconfdir=/etc --with-ovs-source="$PWD/ovs"
    make -j"$NIX_BUILD_CORES"
    runHook postBuild
  '';

  # Without the overrides `make install` creates /etc/bash_completion.d and
  # the run, log, PKI and database directories, which a build cannot write.
  #
  # What the image does not need goes after the install: the headers, the
  # static libraries, the man pages and $out/etc, which would turn
  # /etc/openvswitch into a read-only store link in the image. So do the
  # Python tools: the OVS Python package, the bugtool plugins, and every
  # script under bin/, sbin/ and the two scripts/ directories whose first
  # line names python3, with the links they leave dangling (ovn-detrace,
  # ovs-tcpdump, ovs-vtep and the like). They would pull python3 into the
  # closure, and the Docker image shipped them without a Python to run them.
  # disallowedRequisites fails the build on a reference a later version adds.
  installPhase = ''
    runHook preInstall
    make -C ovs install sysconfdir=$out/etc LOGDIR=$TMPDIR/dummy RUNDIR=$TMPDIR/dummy PKIDIR=$TMPDIR/dummy DBDIR=$TMPDIR/dummy
    make install sysconfdir=$out/etc OVN_RUNDIR=$TMPDIR/dummy LOGDIR=$TMPDIR/dummy DBDIR=$TMPDIR/dummy

    rm -rf "$out/etc" "$out/include" "$out/lib" "$out/share/man" "$out/share/openvswitch/python"
    find "$out" -type d -name bugtool-plugins -prune -exec rm -rf {} +
    for dir in "$out/bin" "$out/sbin" "$out/share/openvswitch/scripts" "$out/share/ovn/scripts"; do
      [ -d "$dir" ] || continue
      find "$dir" -type f -print0 | while IFS= read -r -d "" file; do
        IFS= read -r first <"$file" || true
        case "$first" in
          "#!"*python3*) rm "$file" ;;
        esac
      done
      find "$dir" -xtype l -delete
    done
    runHook postInstall
  '';

  disallowedRequisites = [ pkgs.python3 ];

  # The OVS and OVN testsuites need privileges and hours;
  # tests/container-images/verify_ovn.sh is the contract of the image.
  doCheck = false;

  # The guard that the tag and the commit of the pin still agree.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    "$out/bin/ovn-northd" --version >"$TMPDIR/ovn-northd-version"
    IFS= read -r first <"$TMPDIR/ovn-northd-version"
    if [ "$first" != ${lib.escapeShellArg "ovn-northd ${version}"} ]; then
      echo ${lib.escapeShellArg "ovn.nix: images/ovn/source.json names ${pin.version}, but the tree at ${pin.rev} builds "}"$first" >&2
      exit 1
    fi
    runHook postInstallCheck
  '';
}
