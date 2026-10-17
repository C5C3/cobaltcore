# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# Apache federation reverse-proxy sidecar for Keystone: mod_auth_openidc
# (OIDC) and mod_auth_mellon (SAML) in one image. The keystone operator
# projects it into the Keystone API pod only when at least one OIDC or SAML
# KeystoneIdentityBackend is attached; it terminates the federation flows on
# the protected websso, auth and redirect paths and reverse-proxies
# everything to the localhost-bound uWSGI. Release-independent:
# mod_auth_openidc is ./mod_auth_openidc.nix, built from the commit
# ./source.json pins, and Apache and mod_auth_mellon come from the
# nixpkgs-images revision flake.lock pins, not from an OpenStack release.
# nixpkgs does not strip modules/ either, so the mellon override strips it
# and guards the closure like ./mod_auth_openidc.nix does.
#
# The image is the FHS façade of nix/lib/fhs-layout.nix without a Python
# environment, plus coreutils for the verify script's id, test, timeout and
# du. Two root links name the store: /usr/sbin/apache2, the command the
# operator runs, is httpd, and /etc/keystone-federation-proxy/httpd-base.conf
# is ./httpd-base.conf with its LoadModule lines naming the store paths of
# httpd and both modules, so they are part of the image's closure.
# /etc/keystone-federation-proxy is a real directory with real conf.d/ and
# metadata/ directories, the mount points of the federation Secret; the
# container runtime creates the mellon/ mount point inside it, which a store
# link would not allow. /var/www/html exists because `apache2 -t` fails on a
# missing DocumentRoot, and /tmp, the pod's emptyDir, has mode 1777.
#
# PATH is the Docker image's, because the default of
# nix/lib/image-defaults.nix has no /usr/sbin. There is no Cmd and no
# Entrypoint: the operator sets the command. IdP TLS needs no environment
# variable: nixpkgs' curl falls back to OpenSSL's default CA file,
# /etc/ssl/certs/ca-certificates.crt, which the façade provides.
#
# DEVIATION from the original build-pipeline design: the image derives from
# no python-base, so it carries the generic 'openstack' user and group
# (UID/GID 42424) itself, through the fakeNss entry of the façade. The
# sidecar runs under the same identity as the keystone container it fronts,
# so the pod keeps one security context.
{ pkgs, fhsLayout, ... }:
let
  defaults = import ../../nix/lib/image-defaults.nix;
  modAuthOpenidc = import ./mod_auth_openidc.nix { inherit pkgs; };
  modAuthMellon = pkgs.apacheHttpdPackages.mod_auth_mellon.overrideAttrs {
    stripDebugList = [ "modules" ];
    disallowedRequisites = [
      pkgs.apacheHttpd.dev
      pkgs.stdenv.cc.cc
    ];
  };
  baseConf = pkgs.replaceVars ./httpd-base.conf {
    httpd = pkgs.apacheHttpd;
    mod_auth_openidc = modAuthOpenidc;
    mod_auth_mellon = modAuthMellon;
  };
in
pkgs.dockerTools.streamLayeredImage {
  name = "keystone-federation-proxy";
  tag = "latest";
  inherit (defaults) created;
  passthru = { inherit modAuthOpenidc modAuthMellon; };

  contents = [
    (fhsLayout {
      rootLinks = {
        "usr/sbin/apache2" = "${pkgs.apacheHttpd}/bin/httpd";
        "etc/keystone-federation-proxy/httpd-base.conf" = "${baseConf}";
      };
    })
    pkgs.coreutils
  ];

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p -m 1777 /tmp
    chmod 1777 /tmp
    mkdir -p /etc/keystone-federation-proxy/conf.d /etc/keystone-federation-proxy/metadata /var/www/html
  '';

  config = {
    User = "${toString defaults.uid}:${toString defaults.gid}";
    Env = [
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      "LANG=C.UTF-8"
    ];
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "keystone-federation-proxy";
      "org.opencontainers.image.description" =
        "Apache reverse-proxy sidecar for Keystone federation (mod_auth_openidc + mod_auth_mellon)";
    };
  };
}
