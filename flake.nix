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
#                                    packages. ovn-image compiles OVS and OVN
#                                    from images/ovn/source.json;
#                                    openstack-hypervisor-operator-image and
#                                    kvm-node-agent-image build the manager
#                                    of images/<name>/manager.nix on the
#                                    distroless-equivalent image of
#                                    nix/lib/static-image.nix. Their passthru
#                                    reaches the inner build: .ovn and
#                                    .manager. libvirt-image is
#                                    images/libvirt/image.nix on the libvirt
#                                    of nix/pkgs/libvirt.nix (.libvirt) and
#                                    the QEMU of images/libvirt/qemu.nix
#                                    (.qemu); nova-compute-<rel_>-image is
#                                    images/nova-compute/image.nix for each
#                                    nova lock, its venv with the lock's
#                                    nova-compute group and its rootwrap.conf
#                                    from nix/lib/rootwrap-conf.nix
#   packages.<system>.<name>-<rel_>-test-run
#                                    the unit tests of a service lock, a build
#                                    that succeeds whenever the suite ran and
#                                    holds the results
#   checks.<system>.<name>-<rel_>-tests
#                                    fails unless that run passed
#   checks.<system>.nix-lint         nixfmt, statix and deadnix over flake.nix,
#                                    nix/lib/, nix/pkgs/ and images/;
#                                    nix/spike/ is the Phase 0 record and
#                                    stays outside the set
#   checks.<system>.fhs-layout       nix/lib/fhs-layout.nix built with a venv,
#                                    an extraRoots and a rootLinks entry
#   checks.<system>.service-tests    nix/lib/service-tests.nix over a fixture
#                                    suite with a failing test, and the check
#                                    that fails on it
#   checks.<system>.libvirt-pairing  fails evaluation unless the
#                                    libvirt-python of every nova lock builds
#                                    against the libvirt libvirt-image runs
#   checks.<system>.libvirt-pairing-rejects-foreign
#                                    fails evaluation unless the same
#                                    comparison against nixpkgs' own libvirt
#                                    throws
#   devShells.<system>.image-tools   skopeo and vulnix for the
#                                    build-nix-image composite action
#
# On all four systems, devShells.<system>.lock-tools carries the uv, yq, jq,
# git and Python hack/nix-lock.sh runs (make nix-lock), and the pkg-config
# and libvirt that libvirt-python's setup.py asks for.
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
      self,
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
      # tempest has no unit tests.
      testedLocks = builtins.filter (lock: lock.name != "tempest") locks;
      # The nova locks are the inventory of the compute image: a release that
      # drops nova drops its nova-compute image.
      novaLocks = builtins.filter (lock: lock.name == "nova") locks;

      # What the Python images of one system are built from, evaluated once
      # per system for packages and checks.
      imageLibs = forImageSystems (
        system:
        let
          pkgs = pkgsFor system;
          python = pythonFor pkgs;
          fhsLayout = import ./nix/lib/fhs-layout.nix { inherit pkgs; };
          # The one libvirt of the libvirt image and of nova-compute's
          # libvirt-python.
          libvirt = pkgs.callPackage ./nix/pkgs/libvirt.nix { };
          mkPythonEnv = import ./nix/lib/python-env.nix {
            inherit
              pkgs
              lib
              python
              libvirt
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
          envFor = lock: envs.${lock.attr};
          serviceTests = import ./nix/lib/service-tests.nix { inherit pkgs; };
        in
        {
          inherit
            pkgs
            fhsLayout
            envFor
            serviceTests
            libvirt
            ;
          serviceImage = import ./nix/lib/service-image.nix { inherit pkgs fhsLayout; };
          staticImage = import ./nix/lib/static-image.nix { inherit pkgs; };
          # horizon's unit tests run under pytest without a database.
          testsFor =
            lock:
            serviceTests {
              env = envFor lock;
              withDatabases = lock.name != "horizon";
            };
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
      # arrays on macOS too. libvirt-python ships an sdist only, whose
      # setup.py runs pkg-config against libvirt before uv can read its
      # metadata; any libvirt.pc does, so nixpkgs' libvirt serves all four
      # systems.
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
            pkg-config
            libvirt
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
            staticImage
            testsFor
            cephBindings
            libvirt
            ;
        in
        {
          backup-shifter-image = import ./images/backup-shifter/image.nix {
            inherit pkgs fhsLayout;
          };
          ovn-image = import ./images/ovn/image.nix { inherit pkgs fhsLayout; };
          openstack-hypervisor-operator-image = import ./images/openstack-hypervisor-operator/image.nix {
            inherit pkgs staticImage;
          };
          kvm-node-agent-image = import ./images/kvm-node-agent/image.nix { inherit pkgs staticImage; };
          libvirt-image = import ./images/libvirt/image.nix { inherit pkgs libvirt; };
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
        // lib.listToAttrs (
          map (
            lock:
            lib.nameValuePair "nova-compute-${lib.removePrefix "nova-" lock.attr}-image" (
              import ./images/nova-compute/image.nix {
                inherit
                  pkgs
                  serviceImage
                  cephBindings
                  ;
                env = envFor lock;
              }
            )
          ) novaLocks
        )
        // lib.listToAttrs (
          map (lock: lib.nameValuePair "${lock.attr}-test-run" (testsFor lock).run) testedLocks
        )
      );

      checks = forImageSystems (
        system:
        let
          inherit (imageLibs.${system})
            pkgs
            envFor
            testsFor
            serviceTests
            ;

          # pairing <drvPath> — a check that builds when the libvirt-python
          # of every nova lock builds against the libvirt <drvPath> names,
          # and a throw otherwise.
          pairing =
            imageLibvirt:
            let
              bindingLibvirt =
                lock:
                let
                  binding = (envFor lock).libvirtPython;
                  found =
                    if binding == null then
                      [ ]
                    else
                      builtins.filter (d: (d.pname or "") == "libvirt") binding.buildInputs;
                in
                if found == [ ] then "no libvirt" else (builtins.head found).drvPath;
              mismatched = builtins.filter (lock: bindingLibvirt lock != imageLibvirt) novaLocks;
            in
            if mismatched != [ ] then
              throw "libvirt-pairing: libvirt-python of nova ${(builtins.head mismatched).release} builds against ${bindingLibvirt (builtins.head mismatched)}, the libvirt image runs ${imageLibvirt}"
            else
              pkgs.runCommand "libvirt-pairing" { } ''
                touch "$out"
              '';
        in
        lib.listToAttrs (
          map (lock: lib.nameValuePair "${lock.attr}-tests" (testsFor lock).check) testedLocks
        )
        // {
          # The libvirt-python of each nova lock builds against the libvirt
          # the libvirt image runs. A nixpkgs libvirt wired into either side
          # fails evaluation, so nix flake check --no-build catches it.
          libvirt-pairing = pairing self.packages.${system}.libvirt-image.libvirt.drvPath;

          # The same comparison against nixpkgs' own libvirt has to throw, so
          # libvirt-pairing compares something: at least one nova lock, and
          # the libvirt its binding is built against.
          libvirt-pairing-rejects-foreign =
            assert !(builtins.tryEval (pairing pkgs.libvirt.drvPath)).success;
            pkgs.runCommand "libvirt-pairing-rejects-foreign" { } ''
              touch "$out"
            '';

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

          # A run of nix/lib/service-tests.nix over a suite with one failing
          # test still writes its exit status, a subunit stream and a
          # summary that counts the failure, and the check of that run
          # fails and names the command that lists the result links
          # keeping the run alive. The failing test names a store path,
          # which nuke-refs blanks inside a packet, so the stream decodes
          # to the two results only if subunit-crc.py rewrote that
          # packet's checksum.
          service-tests =
            let
              env = import ./nix/lib/fixtures/service-tests/env.nix pkgs;
              inherit
                (serviceTests {
                  inherit env;
                  withDatabases = false;
                })
                run
                check
                ;
              failed = pkgs.testers.testBuildFailure check;
            in
            pkgs.runCommand "service-tests-check" { nativeBuildInputs = [ env.testVenv ]; } ''
              test "$(cat ${run}/exit-status)" != 0
              grep -q 'Failed: 1' ${run}/summary.txt
              grep -q '/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' ${run}/testresults.subunit
              python - ${run}/testresults.subunit <<'PY'
              import sys
              import subunit
              import testtools
              tests = []
              result = testtools.StreamToDict(tests.append)
              result.startTestRun()
              with open(sys.argv[1], "rb") as f:
                  subunit.ByteStreamToStreamResult(f, non_subunit_name="stdout").run(result)
              result.stopTestRun()
              got = {t["id"].rsplit(".", 1)[-1]: t["status"] for t in tests}
              assert got == {"test_passes": "success", "test_fails": "fail"}, got
              PY
              test "$(cat ${failed}/testBuildFailure.exit)" = 1
              grep -q 'fixture 0 unit tests failed (exit 1)' ${failed}/testBuildFailure.log
              grep -qF 'nix-store --query --roots ${run}' ${failed}/testBuildFailure.log
              touch "$out"
            '';
        }
      );
    };
}
