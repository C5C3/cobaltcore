# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# openstack-hypervisor-operator (hvo), the manager manager.nix compiles from
# the upstream main commit source.json pins with the patches under patches/
# applied, on the static image of nix/lib/static-image.nix. Upstream
# publishes a Helm chart for every main commit but no image under a fixed tag
# for it, only the moving 'latest'; this build fills that gap. The chart of a
# main commit renders the image as <repository>:<appVersion> with appVersion
# sha-<full commit>, and the image is published under that tag, so the chart
# runs it with only the repository overridden. Release-independent: the
# image has no key in releases/*/source-refs.yaml, and the pinned commit,
# not a release, names its tags. Like the other images it is rebuilt by
# every build-images run on main, which moves sha-<commit>,
# upstream-<commit> and latest to that build.
#
# DEVIATION from the original build-pipeline design: the image creates no
# 'openstack' user. The operator is a static Go
# binary, so it runs on the image that stands in for the distroless base the
# CobaltCore operator images use (nix/lib/operator-image.nix), as that base's
# nonroot user, uid 65532. Upstream's own image is Alpine with uid 4200; its
# chart only sets runAsNonRoot and no command, which any non-root user and
# the Entrypoint satisfy.
{ pkgs, staticImage, ... }:
let
  manager = import ./manager.nix { inherit pkgs; };
in
staticImage {
  name = "openstack-hypervisor-operator";
  tag = manager.version;
  description = "openstack-hypervisor-operator built from a pinned upstream main commit with CobaltCore patches";
  inherit manager;
  upstreamCommit = manager.src.rev;
}
