# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The image of a CobaltCore operator: one expression builds every
# operators/<op>/ that has a go.mod, which flake.nix finds by reading the
# directory, so a new operator gets its <op>-operator-image without a flake
# edit. The manager is the static binary of the module's main package, and
# the image is nix/lib/static-image.nix around it, with the four static labels
# and no version or upstream commit.
#
# buildGoModule has no go.work support, so the build runs with GOWORK=off:
# operators/<op>/go.mod and its relative replace directives decide the module
# graph. Every module a go.mod replaces with a relative path has to be in the
# source, so the source holds internal/common, the operator and every sibling
# operator its go.mod replaces (c5c3 replaces the nine others, neutron
# replaces ovn). The replace lines are parsed here; a line that is neither a
# sibling nor internal/common, such as a replace block or a path outside the
# repository, fails evaluation instead of leaving a module out of the source.
# The source leaves out go.work, the tests, and the helm/, dashboards/ and
# config/ directories, which no go:embed reads, so a commit to another
# operator, a chart, a dashboard, a kustomize file or a test leaves the
# derivation unchanged.
#
# proxyVendor keeps the fixed-output derivation of the modules to the module
# download cache: `go mod vendor` would copy the replaced internal/common into
# vendor/, and the vendor hash would move with every commit to it. The hash
# is operators/<op>/vendor.json's vendorHash, which hack/nix-lock.sh writes
# (make nix-lock). An empty vendorHash fails the module build with the hash
# it got, which is how the generator learns it.
#
# With cover = true the expression builds the coverage-instrumented variant
# the e2e, chaos and Tempest legs deploy: the manager is built with -cover
# -covermode=atomic and a -coverpkg of the operator's module and
# internal/common, and the image is tagged cover. Such a binary writes its
# counters to GOCOVERDIR, which internal/common/bootstrap flushes every
# minute. The flags change neither the source nor the module download, so
# vendor.json's vendorHash holds for both variants.
#
# Arguments:
#   pkgs          the nixpkgs-images package set
#   staticImage   nix/lib/static-image.nix applied to pkgs
#   root          the repository root
# and then:
#   name          the directory under operators/
#   pin           an attrset with vendorHash; operators/<name>/vendor.json by
#                 default, whose inputsHash only hack/nix-lock.sh reads
#   cover         true for the coverage-instrumented variant; false by default
{
  pkgs,
  staticImage,
  root,
}:
let
  inherit (pkgs) lib;
  inherit (lib) fileset;
in
{
  name,
  cover ? false,
  pin ?
    let
      path = root + "/operators/${name}/vendor.json";
    in
    if builtins.pathExists path then
      lib.importJSON path
    else
      throw "operator-image: operators/${name}/vendor.json is missing; run make nix-lock",
}:
let
  opDir = op: root + "/operators/${op}";

  # The sibling operator a replace line names, null for internal/common.
  # builtins.match is POSIX ERE, which has no backreference, so the two
  # captures are compared instead; it matches the whole line.
  siblingOf =
    line:
    let
      m = builtins.match "replace github\\.com/c5c3/cobaltcore/operators/([a-z0-9]+) => \\.\\./([a-z0-9]+)" line;
    in
    if m != null && builtins.elemAt m 0 == builtins.elemAt m 1 then
      builtins.elemAt m 0
    else if line == "replace github.com/c5c3/cobaltcore/internal/common => ../../internal/common" then
      null
    else
      throw "operator-image: operators/${name}/go.mod: replace line not understood: ${line}";

  replaceLines = builtins.filter (lib.hasPrefix "replace ") (
    lib.splitString "\n" (builtins.readFile (opDir name + "/go.mod"))
  );
  siblings = builtins.filter (sib: sib != null) (map siblingOf replaceLines);
  modules = [ name ] ++ siblings;

  included = fileset.unions ([ (root + "/internal/common") ] ++ map opDir modules);
  excluded = fileset.unions (
    [ (fileset.fileFilter (f: lib.hasSuffix "_test.go" f.name) root) ]
    ++ lib.concatMap (
      op:
      map (dir: fileset.maybeMissing (opDir op + "/${dir}")) [
        "helm"
        "dashboards"
        "config"
      ]
    ) modules
  );
  src = fileset.toSource {
    inherit root;
    fileset = fileset.difference included excluded;
  };

  coverPkgs = lib.concatStringsSep "," [
    "github.com/c5c3/cobaltcore/operators/${name}/..."
    "github.com/c5c3/cobaltcore/internal/common/..."
  ];

  manager = pkgs.buildGo127Module (
    {
      pname = "${name}-operator";
      version = "latest";
      inherit src;
      modRoot = "operators/${name}";
      subPackages = [ "." ];
      env = {
        GOWORK = "off";
        CGO_ENABLED = 0;
      };
      proxyVendor = true;
      inherit (pin) vendorHash;
      ldflags = [
        "-s"
        "-w"
      ];
      # The unit tests need the envtest binaries; make test runs them.
      doCheck = false;
      postInstall = ''
        mv "$out/bin/${name}" "$out/bin/manager"
      '';
      passthru = { inherit siblings; };
    }
    // lib.optionalAttrs cover {
      pname = "${name}-operator-cover";
      # The pinned buildGoModule reads no buildFlags and replaces a caller's
      # env.GOFLAGS, so the coverage flags join GOFLAGS before go install.
      preBuild = ''
        export GOFLAGS="$GOFLAGS -cover -covermode=atomic -coverpkg=${coverPkgs}"
      '';
    }
  );
in
staticImage (
  {
    name = "${name}-operator";
    tag = "latest";
    description = "CobaltCore ${name} operator";
    inherit manager;
    version = null;
  }
  // lib.optionalAttrs cover {
    tag = "cover";
    description = "CobaltCore ${name} operator, coverage-instrumented";
  }
)
