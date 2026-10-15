# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# A development environment that mirrors the CI toolchain. `nix develop` yields
# the same tools a CI run gets: the base runtimes come from the pinned nixpkgs
# below; the tools the pipeline pins *exactly* (controller-gen, gofumpt,
# golangci-lint, setup-envtest, kustomize, chainsaw, kind, kubectl, flux, the
# helm-unittest plugin, and the envtest assets) are installed by
# hack/nix-devshell-hook.sh, which reads the pins where they already live
# (ci.yaml, Makefile, hack/install-test-deps.sh) so no version is declared twice.
#
# Deliberately NOT provided: Docker (a daemon cannot be nix-provisioned; kind
# needs a running Docker — see docs/contributing/nix-dev-environment.md).
#
# The flake also builds the container images that moved off their Dockerfile
# (docs/reference/ci-cd/nix-image-build.md). nixpkgs (nixos-unstable) feeds the
# dev shell, nixpkgs-images (nixos-26.05, the channel the nixpkgs security team
# backports to) feeds the images, and pyproject-nix, uv2nix and
# pyproject-build-systems turn the locks under nix/locks/ into the Python
# environments of the service and tempest images. On x86_64-linux and
# aarch64-linux, the two platforms build-images.yaml builds on, the images
# input adds these outputs:
#
#   packages.<system>.<name>-image   a dockerTools.streamLayeredImage built
#                                    from images/<name>/image.nix; a Python
#                                    image is <name>-<rel_>-image, <rel_> the
#                                    release with its dot replaced by _; the
#                                    names are flat because nix flake check
#                                    rejects a nested attribute set under
#                                    packages
#   checks.<system>.nix-lint         nixfmt, statix and deadnix over flake.nix,
#                                    nix/lib/, nix/pkgs/ and images/;
#                                    nix/spike/ is the Phase 0 record and
#                                    stays outside the set
#   checks.<system>.fhs-layout       nix/lib/fhs-layout.nix built with a venv,
#                                    an extraRoots and a rootLinks entry
#   devShells.<system>.image-tools   skopeo and vulnix for the
#                                    build-nix-image composite action
#
# On all four systems, devShells.<system>.lock-tools carries the uv, yq, jq,
# git and Python hack/nix-lock.sh runs (make nix-lock).
{
  description = "cobaltcore development environment mirroring the CI toolchain";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixpkgs-images.url = "github:NixOS/nixpkgs/nixos-26.05";

    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs-images";
    };

    uv2nix = {
      url = "github:pyproject-nix/uv2nix";
      inputs = {
        pyproject-nix.follows = "pyproject-nix";
        nixpkgs.follows = "nixpkgs-images";
      };
    };

    pyproject-build-systems = {
      url = "github:pyproject-nix/build-system-pkgs";
      inputs = {
        pyproject-nix.follows = "pyproject-nix";
        uv2nix.follows = "uv2nix";
        nixpkgs.follows = "nixpkgs-images";
      };
    };
  };

  outputs =
    {
      nixpkgs,
      nixpkgs-images,
      pyproject-nix,
      uv2nix,
      pyproject-build-systems,
      ...
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;

      imageSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forImageSystems = nixpkgs-images.lib.genAttrs imageSystems;
      pkgsFor = system: nixpkgs-images.legacyPackages.${system};
      # The interpreter of the images, and the one uv resolves the locks for
      # in the lock-tools shell (PYTHON_MINOR of hack/nix-lock.sh).
      pythonFor = pkgs: pkgs.python312;

      # The locks under nix/locks/, release directories first and name
      # directories below them, are the inventory of the Python images:
      # hack/nix-lock.sh --check keeps them equal to the release pins. attr
      # is <name>-<rel_>, the release with its dot replaced, because an
      # unquoted flake attribute cannot contain one.
      inherit (nixpkgs-images) lib;
      subdirs =
        dir: builtins.attrNames (lib.filterAttrs (_: type: type == "directory") (builtins.readDir dir));
      locks = lib.concatMap (
        release:
        map (name: {
          inherit name release;
          attr = "${name}-${lib.replaceStrings [ "." ] [ "_" ] release}";
        }) (subdirs (./nix/locks + "/${release}"))
      ) (subdirs ./nix/locks);
      # What the Python images of one system are built from, evaluated once
      # per system for packages and checks.
      imageLibs = forImageSystems (
        system:
        let
          pkgs = pkgsFor system;
          python = pythonFor pkgs;
          fhsLayout = import ./nix/lib/fhs-layout.nix { inherit pkgs; };
          mkPythonEnv = import ./nix/lib/python-env.nix {
            inherit
              pkgs
              lib
              python
              uv2nix
              pyproject-nix
              pyproject-build-systems
              ;
          };
          envs = lib.listToAttrs (
            map (
              lock:
              lib.nameValuePair lock.attr (mkPythonEnv {
                inherit (lock) name release attr;
              })
            ) locks
          );
        in
        {
          inherit pkgs fhsLayout;
          envFor = lock: envs.${lock.attr};
          serviceImage = import ./nix/lib/service-image.nix { inherit pkgs fhsLayout; };
          cephBindings = pkgs.callPackage ./nix/pkgs/ceph-bindings.nix { python3Packages = python.pkgs; };
        }
      );

      # The Nix files the lint check reads. fileFilter takes a path, not a
      # file set, so the filter runs over the tree and the intersection keeps
      # the three roots.
      lintSource =
        let
          inherit (nixpkgs-images.lib) fileset;
        in
        fileset.toSource {
          root = ./.;
          fileset = fileset.intersection (fileset.fileFilter (f: f.hasExt "nix") ./.) (
            fileset.unions [
              ./flake.nix
              ./nix/lib
              ./nix/pkgs
              ./images
            ]
          );
        };

      # The tools hack/nix-lock.sh runs, from the images input: its uv wrote
      # the committed locks, and its bash 5 runs the generator's associative
      # arrays on macOS too.
      lockToolsShells = forAllSystems (system: {
        lock-tools = nixpkgs-images.legacyPackages.${system}.mkShell {
          name = "cobaltcore-lock-tools";
          packages = with nixpkgs-images.legacyPackages.${system}; [
            bash
            uv
            yq-go
            jq
            git
            (pythonFor nixpkgs-images.legacyPackages.${system})
          ];
        };
      });

      imageToolsShells = forImageSystems (system: {
        image-tools = (pkgsFor system).mkShell {
          name = "cobaltcore-image-tools";
          packages = [
            (pkgsFor system).skopeo
            (pkgsFor system).vulnix
          ];
        };
      });
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          # Plain mkShell (with a C compiler): `go test -race` needs cgo, matching
          # the CI runner. The GNU userland is included on purpose so macOS
          # behaves like the Linux-only CI runner (e.g. chainsaw-lint uses GNU
          # xargs). go_1_27 / nodejs_24 fall back to the unversioned attrs if a
          # future nixpkgs revision renames them.
          default = pkgs.mkShell {
            name = "cobaltcore-devshell";

            packages = [
              (pkgs.go_1_27 or pkgs.go)
              (pkgs.nodejs_24 or pkgs.nodejs)
              pkgs.python3
              pkgs.kubernetes-helm
              pkgs.shellcheck
              pkgs.yq-go
              pkgs.jq
              pkgs.git
              pkgs.gnumake
              pkgs.curl
              pkgs.coreutils
              pkgs.gnugrep
              pkgs.gnused
              pkgs.gawk
              pkgs.findutils
            ];

            # Source the hook from the working tree (not a ${./…} store path) so
            # it reads the live pins and resolves the repo root via git.
            shellHook = ''
              _cobaltcore_root="$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")"
              if [ -f "$_cobaltcore_root/hack/nix-devshell-hook.sh" ]; then
                # shellcheck source=/dev/null
                source "$_cobaltcore_root/hack/nix-devshell-hook.sh"
              else
                echo "cobaltcore: hack/nix-devshell-hook.sh not found under $_cobaltcore_root — run 'nix develop' from within the cobaltcore checkout." >&2
              fi
              unset _cobaltcore_root
            '';
          };
        }
        // lockToolsShells.${system}
        // imageToolsShells.${system} or { }
      );

      packages = forImageSystems (
        system:
        let
          inherit (imageLibs.${system})
            pkgs
            fhsLayout
            envFor
            serviceImage
            cephBindings
            ;
        in
        {
          backup-shifter-image = import ./images/backup-shifter/image.nix {
            inherit pkgs fhsLayout;
          };
        }
        // lib.listToAttrs (
          map (
            lock:
            lib.nameValuePair "${lock.attr}-image" (
              import (./images + "/${lock.name}/image.nix") {
                inherit pkgs serviceImage cephBindings;
                env = envFor lock;
              }
            )
          ) locks
        )
      );

      checks = forImageSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          nix-lint =
            pkgs.runCommand "nix-lint"
              {
                nativeBuildInputs = [
                  pkgs.nixfmt
                  pkgs.statix
                  pkgs.deadnix
                ];
              }
              ''
                cd ${lintSource}
                find . -name '*.nix' -print0 | xargs -0 nixfmt --check
                statix check .
                deadnix --fail .
                touch "$out"
              '';

          # The façade with a Python environment, an extraRoots entry and a
          # rootLinks entry, independent of any service lock.
          fhs-layout =
            let
              fhsLayout = import ./nix/lib/fhs-layout.nix { inherit pkgs; };
              venv = pkgs.runCommand "venv-fixture" { } ''
                mkdir -p "$out/bin"
                touch "$out/bin/svc"
              '';
              layout = fhsLayout {
                inherit venv;
                extraRoots."etc/svc/svc.conf" = pkgs.writeText "svc-conf-fixture" "x";
                rootLinks."usr/share/fixture" = pkgs.writeText "root-link-fixture" "y";
              };
            in
            pkgs.runCommand "fhs-layout-check" { } ''
              test -e ${layout}/var/lib/openstack/bin/svc
              test "$(cat ${layout}/var/lib/openstack/etc/svc/svc.conf)" = x
              test "$(cat ${layout}/usr/share/fixture)" = y
              test -x ${layout}/bin/sh
              test -e ${layout}/usr/bin/env
              test -s ${layout}/etc/ssl/certs/ca-certificates.crt
              touch "$out"
            '';
        }
      );
    };
}
