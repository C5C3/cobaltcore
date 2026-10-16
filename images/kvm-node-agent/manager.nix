# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The manager binary of kvm-node-agent (kna), compiled from the upstream
# main commit images/kvm-node-agent/source.json pins with the patches under
# patches/ applied. The pin is a main commit: upstream publishes a Helm chart
# for every main commit but no image for it, and a fix kna needs outside SAP
# ships here as a patch while it is proposed upstream.
#
# buildGoModule applies the patches to the module fetch and to the build
# alike; none of them touches go.mod or go.sum. A pin move on which a patch
# no longer applies fails the build in the patch phase; re-cut the patch
# against the new commit. The checkPhase runs patch-checks.sh, the checks of
# the patches, in the patched tree. Once upstream carries a change, drop the
# patch together with its checks there; once no patch remains, patches/ can
# go too. The ldflags are upstream's (Makefile, the build/manager target)
# without the build date, so `manager --version` prints
# `manager version sha-<commit> ()`, with the version set to the tag the
# image is published under. Upstream's 'generate' step is skipped: the
# generated files are committed.
{
  pkgs,
  pin ? pkgs.lib.importJSON ./source.json,
}:
let
  inherit (pkgs) lib;
  pname = "kvm-node-agent";
  version = "sha-${pin.rev}";
in
pkgs.buildGo127Module {
  inherit pname version;

  src = pkgs.fetchFromGitHub {
    # The store path of a fetch depends on its name and hash only. The rev in
    # the name makes a rev moved without its hash fail the fetch instead of
    # building the tree of the old hash.
    name = "${pname}-${pin.rev}-source";
    owner = "cobaltcore-dev";
    repo = pname;
    inherit (pin) rev hash;
  };
  inherit (pin) vendorHash;

  # readDir lists the names in order, so the patches apply by number.
  patches = lib.optionals (builtins.pathExists ./patches) (
    map (name: ./patches + "/${name}") (
      lib.filter (lib.hasSuffix ".patch") (builtins.attrNames (builtins.readDir ./patches))
    )
  );
  # GNU patch applies a hunk whose outer context lines differ (fuzz 2) by
  # default; --fuzz=0 fails the patch phase on any context mismatch.
  patchFlags = [
    "-p1"
    "--fuzz=0"
  ];

  subPackages = [ "cmd" ];
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/sapcc/go-api-declarations/bininfo.binName=manager"
    "-X github.com/sapcc/go-api-declarations/bininfo.version=${version}"
    "-X github.com/sapcc/go-api-declarations/bininfo.commit=${pin.rev}"
  ];

  checkPhase = ''
    runHook preCheck
    bash ${./patch-checks.sh}
    runHook postCheck
  '';

  postInstall = ''
    mv "$out/bin/cmd" "$out/bin/manager"
  '';
}
