# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The keystone operator binary built two ways, the two shapes Phase 6 of
# #1401 chooses between. operators/Dockerfile builds it with
# `CGO_ENABLED=0 go build` inside the go.work workspace; buildGoModule has no
# go.work support, and `go mod vendor` refuses to run in workspace mode.
#
#   modroot     one module: GOWORK=off, so operators/keystone/go.mod and its
#               relative replace of internal/common decide the build.
#               proxyVendor keeps the fixed-output derivation to the module
#               download cache: `go mod vendor` would copy the replaced
#               internal/common into vendor/, and its vendorHash would change
#               with every internal/common commit.
#   workvendor  the workspace: a fixed-output derivation runs
#               `go work vendor` beside go.work, and the build uses that
#               vendor directory with -mod=vendor.
{
  pkgs,
  lib,
}:
let
  # GOWORK=off: operators/keystone/go.mod replaces only internal/common, so
  # a commit to another operator leaves this source and the build unchanged.
  modrootSrc = lib.fileset.toSource {
    root = ../../..;
    fileset = lib.fileset.unions [
      ../../../internal/common
      ../../../operators/keystone
    ];
  };

  # go.work names every module, so the workspace build needs them all.
  workSrc = lib.fileset.toSource {
    root = ../../..;
    fileset = lib.fileset.unions [
      ../../../go.work
      ../../../internal/common
      ../../../operators
    ];
  };

  common = {
    version = "spike";
    modRoot = "operators/keystone";
    subPackages = [ "." ];
    ldflags = [
      "-s"
      "-w"
    ];
    # The unit tests need envtest binaries; the spike builds the binary only.
    doCheck = false;
  };

  workspaceVendorDir = pkgs.stdenvNoCC.mkDerivation {
    name = "cobaltcore-go-work-vendor";
    src = workSrc;
    nativeBuildInputs = [
      pkgs.go_1_27
      pkgs.cacert
    ];
    env.GOTOOLCHAIN = "local";
    buildPhase = ''
      runHook preBuild
      export GOCACHE="$TMPDIR/go-cache" GOPATH="$TMPDIR/go"
      go work vendor
      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      cp -r vendor "$out"
      runHook postInstall
    '';
    dontFixup = true;
    outputHashMode = "recursive";
    outputHash = "sha256-0/74+cjMXC57jxGgFnSgRqcZL8cRTEvdJVMxqF/mq5o=";
  };
in
{
  modroot = pkgs.buildGo127Module (
    common
    // {
      pname = "keystone-operator-modroot";
      src = modrootSrc;
      proxyVendor = true;
      vendorHash = "sha256-c5+glSNAe5pMFCxqWQYUfp5iO8MoPUPXomYuuwm81AU=";
      env = {
        GOWORK = "off";
        CGO_ENABLED = 0;
      };
    }
  );

  workvendor = pkgs.buildGo127Module (
    common
    // {
      pname = "keystone-operator-workvendor";
      src = workSrc;
      vendorHash = null;
      postPatch = ''
        cp -r --no-preserve=mode ${workspaceVendorDir} vendor
      '';
      env.CGO_ENABLED = 0;
    }
  );
}
