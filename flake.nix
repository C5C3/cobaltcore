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
{
  description = "forge development environment mirroring the CI toolchain";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
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
          # xargs). go_1_26 / nodejs_24 fall back to the unversioned attrs if a
          # future nixpkgs revision renames them.
          default = pkgs.mkShell {
            name = "forge-devshell";

            packages = [
              (pkgs.go_1_26 or pkgs.go)
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
              _forge_root="$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")"
              if [ -f "$_forge_root/hack/nix-devshell-hook.sh" ]; then
                # shellcheck source=/dev/null
                source "$_forge_root/hack/nix-devshell-hook.sh"
              else
                echo "forge: hack/nix-devshell-hook.sh not found under $_forge_root — run 'nix develop' from within the forge checkout." >&2
              fi
              unset _forge_root
            '';
          };
        }
      );
    };
}
