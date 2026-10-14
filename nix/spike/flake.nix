# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The Nix image-build spike of #1401 Phase 0: a throwaway flake that builds
# one example of every shape the later phases repeat and whose results
# nix/spike/README.md records as decisions D1 to D12. It is evaluated on
# x86_64-linux only, from the repository root:
#
#   nix build ./nix/spike#keystone-image
#
# The flake is not registered in the root flake.nix, no CI job builds it, and
# Renovate does not re-lock it (renovate.json disables every update under
# nix/spike/).
{
  description = "cobaltcore Nix image-build spike (#1401 Phase 0, x86_64-linux only)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    uv2nix = {
      url = "github:pyproject-nix/uv2nix";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    pyproject-build-systems = {
      url = "github:pyproject-nix/build-system-pkgs";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.uv2nix.follows = "uv2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      nixpkgs,
      pyproject-nix,
      uv2nix,
      pyproject-build-systems,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      inherit (pkgs) lib;
      python = pkgs.python312;

      mkPythonEnv = import ./lib/python-env.nix {
        inherit
          pkgs
          lib
          python
          uv2nix
          pyproject-nix
          pyproject-build-systems
          ;
      };

      fhsLayout = import ./lib/fhs-layout.nix { inherit pkgs; };

      keystone = mkPythonEnv {
        service = "keystone";
        release = "2026.1";
      };
      glance = mkPythonEnv {
        service = "glance";
        release = "2026.1";
      };
    in
    {
      packages.${system} = {
        keystone-venv = keystone.venv;
        keystone-image = import ./images/keystone.nix {
          inherit pkgs fhsLayout;
          inherit (keystone) venv;
        };
        glance-env = glance.venv;
      };
    };
}
