# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The Python environment of one service and release, built by uv2nix from
# nix/spike/locks/<release>/<service>/ (written by generate-lock.sh).
#
# Returns:
#   venv       the service with its default dependencies
#   testVenv   the same plus the test dependency group
#   src        the service source, fetched by the commit and NAR hash in
#              sources.json with the release patches applied
#   service    the arguments, so a check names the release files and its
#   release    derivation after the environment it is given
#
# Wheels are preferred. uv.lock records no build systems, so every package
# built from an sdist, and the service itself, gets its build system and
# native libraries from the fixups overlay below.
{
  pkgs,
  lib,
  python,
  uv2nix,
  pyproject-nix,
  pyproject-build-systems,
}:
{ service, release }:
let
  lockDir = ../locks + "/${release}/${service}";
  sources = lib.importJSON (lockDir + "/sources.json");

  # The patch paths in sources.json are relative to the repository root.
  src = pkgs.applyPatches {
    name = "${service}-${sources.ref}-source";
    src = pkgs.fetchFromGitHub {
      owner = "openstack";
      repo = service;
      inherit (sources) rev hash;
    };
    patches = map (patch: ../../.. + "/${patch}") sources.patches;
  };

  workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = lockDir; };

  lockOverlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };

  # What the packages the spike's outputs build from source on x86_64-linux
  # need besides the sdist: the build system uv.lock does not record, native
  # libraries, and for the service its fetched source. A row applies only to
  # a package the lock contains.
  sourceBuilds = {
    # pbr reads the version from git metadata, which the fetched tree lacks:
    # "Versioning for this project requires either an sdist tarball, or
    # access to an upstream git repository". PBR_VERSION supplies the tag.
    # pbr also takes the file list from `git ls-files`, and a service whose
    # pyproject.toml names only its top-level package (keystone 29.0.0)
    # gets its subpackages through that list. The tree is therefore
    # committed to a throwaway repository with a fixed author and date.
    ${service} = {
      buildSystem = {
        pbr = [ ];
        setuptools = [ ];
      };
      nativeBuildInputs = [ pkgs.git ];
      attrs = {
        inherit src;
        PBR_VERSION = sources.ref;
        preBuild = ''
          git init --quiet --initial-branch=main
          git add --all
          GIT_AUTHOR_DATE=1970-01-01T00:00:01Z GIT_COMMITTER_DATE=1970-01-01T00:00:01Z git -c user.name=nix -c user.email=nix@localhost \
            commit --quiet --message "${service} ${sources.ref}"
        '';
      };
    };
    ovs.buildSystem.setuptools = [ ];
    # setup.cfg asks setuptools to byte-compile in a subprocess that imports
    # distutils, which Python 3.12 removed; with bytecode writing disabled
    # setuptools skips that step, and the installer compiles bytecode itself.
    python-ldap = {
      buildSystem.setuptools = [ ];
      buildInputs = [
        pkgs.openldap
        pkgs.cyrus_sasl
      ];
      attrs.PYTHONDONTWRITEBYTECODE = "1";
    };
    simplegeneric.buildSystem.setuptools = [ ];
    testresources.buildSystem = {
      pbr = [ ];
      setuptools = [ ];
    };
    uwsgi.buildSystem.setuptools = [ ];
  };

  fixups =
    final: prev:
    lib.mapAttrs (
      name: fix:
      prev.${name}.overrideAttrs (
        old:
        fix.attrs or { }
        // {
          nativeBuildInputs =
            old.nativeBuildInputs
            ++ final.resolveBuildSystem fix.buildSystem
            ++ fix.nativeBuildInputs or [ ];
          buildInputs = old.buildInputs or [ ] ++ fix.buildInputs or [ ];
        }
      )
    ) (lib.filterAttrs (name: _: prev ? ${name}) sourceBuilds);

  pythonSet = (pkgs.callPackage pyproject-nix.build.packages { inherit python; }).overrideScope (
    lib.composeManyExtensions [
      pyproject-build-systems.overlays.wheel
      lockOverlay
      fixups
    ]
  );
in
{
  inherit src service release;
  venv = pythonSet.mkVirtualEnv "cobaltcore-${service}-${release}-env" workspace.deps.default;
  testVenv = pythonSet.mkVirtualEnv "cobaltcore-${service}-${release}-test-env" workspace.deps.groups;
}
