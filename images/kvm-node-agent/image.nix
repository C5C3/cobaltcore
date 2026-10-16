# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# kvm-node-agent (kna), the manager manager.nix compiles from the upstream
# main commit source.json pins with the patches under patches/ applied, on
# the static image of nix/lib/static-image.nix. Upstream publishes a Helm
# chart for every main commit but no image for it
# (ghcr.io/cobaltcore-dev/kvm-node-agent:sha-<commit> answers 404). The chart
# of a main commit renders the image as <repository>:<appVersion> with
# appVersion sha-<full commit>, and the image is published under that tag,
# so the chart runs it with only the repository overridden.
# Release-independent: the image has no key in releases/*/source-refs.yaml,
# and the pinned commit, not a release, names its tags. Like the other
# images it is rebuilt by every build-images run on main, which moves
# sha-<commit>, upstream-<commit> and latest to that build.
#
# DEVIATION from the original build-pipeline design: the image creates no
# 'openstack' user. The agent is a static Go
# binary, so it runs on the image that stands in for the distroless base the
# CobaltCore operator images use (operators/Dockerfile), but as root instead
# of that base's nonroot user: kna authenticates to the host's system bus and
# starts units there, and a Debian host's dbus-daemon drops a uid it cannot
# resolve (#1167). Upstream's own image is Alpine with USER 42438:42438, a
# user that exists on SAP's hosts only.
{ pkgs, staticImage, ... }:
let
  manager = import ./manager.nix { inherit pkgs; };
in
staticImage {
  name = "kvm-node-agent";
  tag = manager.version;
  description = "kvm-node-agent built from a pinned upstream main commit with CobaltCore patches";
  inherit manager;
  upstreamCommit = manager.src.rev;
  user = "0:0";
}
