# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# A stand-in for the environment nix/lib/python-env.nix returns, so the
# service-tests check of flake.nix runs nix/lib/service-tests.nix without a
# service: this directory is the source, and its suite has one passing and
# one failing test.
pkgs:
let
  python = pkgs.python312;
in
{
  name = "fixture";
  release = "0";
  attr = "fixture-0";
  project = "fixture";
  version = "0";
  src = ./.;
  inherit python;
  testVenv = python.withPackages (ps: [
    ps.stestr
    ps.testtools
  ]);
}
