# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# mod_auth_openidc, the OIDC module of the keystone-federation-proxy image,
# compiled from the commit images/keystone-federation-proxy/source.json pins.
# images/keystone-federation-proxy/image.nix loads the result. nixpkgs-images
# has no mod_auth_openidc attribute, so the module is built here. The file
# and its pin sit beside the image, not under nix/pkgs/, whose changes
# rebuild every service, tempest and nova-compute image: a module bump
# rebuilds the proxy only.
#
# The tree is fetched by commit and NAR hash, never through the tag. `make
# nix-lock` writes the hash of the commit "rev" names, and the rev in the
# fetch name makes a rev moved without its hash fail the fetch instead of
# building the tree of the old hash.
#
# The configure flags give the module the Docker image shipped: Redis
# support through hiredis and zlib compression, without brotli (configure's
# default) and without jq claim expressions (off unless --with-jq). An
# explicit --with-hiredis makes configure fail when hiredis is missing
# instead of building without Redis support.
#
# postPatch is the guard that the tag and the commit of the pin still agree:
# the AC_INIT line of configure.ac at the pinned commit names the version.
# Upstream's `make check` runs in the build. `make install` would write into
# apxs' LIBEXECDIR, the read-only store path of httpd, so installPhase copies
# the module alone. stdenv does not strip modules/, and unstripped DWARF
# keeps httpd's dev output, every -dev input and gcc in the closure.
# stripDebugList strips it, and disallowedRequisites fails the build on a
# nixpkgs that brings those references back instead of growing the image.
{ pkgs }:
let
  inherit (pkgs) lib;
  pin = lib.importJSON ./source.json;
  version = lib.removePrefix "v" pin.version;
in
pkgs.stdenv.mkDerivation {
  pname = "mod_auth_openidc";
  inherit version;

  src = pkgs.fetchFromGitHub {
    name = "mod_auth_openidc-${pin.rev}-source";
    owner = "OpenIDC";
    repo = "mod_auth_openidc";
    inherit (pin) rev hash;
  };

  postPatch = ''
    if ! grep -qF ${lib.escapeShellArg "AC_INIT([mod_auth_openidc],[${version}],"} configure.ac; then
      found=$(grep -m 1 -o 'AC_INIT([^)]*)' configure.ac || true)
      echo ${lib.escapeShellArg "mod_auth_openidc.nix: images/keystone-federation-proxy/source.json names ${pin.version}, but configure.ac at ${pin.rev} declares "}"$found" >&2
      exit 1
    fi
  '';

  nativeBuildInputs = with pkgs; [
    autoreconfHook
    pkg-config
  ];
  buildInputs = with pkgs; [
    apacheHttpd
    apr
    aprutil
    curl
    openssl
    jansson
    cjose
    pcre2
    hiredis
    zlib
    check
  ];

  configureFlags = [
    "--with-apxs=${pkgs.apacheHttpd.dev}/bin/apxs"
    "--with-hiredis"
  ];

  enableParallelBuilding = true;
  doCheck = true;

  installPhase = ''
    runHook preInstall
    install -D -m 0555 src/.libs/mod_auth_openidc.so "$out/modules/mod_auth_openidc.so"
    runHook postInstall
  '';

  stripDebugList = [ "modules" ];
  disallowedRequisites = [
    pkgs.apacheHttpd.dev
    pkgs.stdenv.cc.cc
  ];
}
