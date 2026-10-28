# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The Python environment of one lock, built by uv2nix from
# nix/locks/<release>/<name>/ (written by hack/nix-lock.sh).
#
# Returns:
#   venv       the project with its default dependencies
#   venvWith   a function from a list of dependency group names to the venv
#              of the default dependencies plus those groups; the group
#              names of the lock's pyproject.toml (test, and the image name
#              of a group hack/nix-lock.sh writes for a derived image)
#   testVenv   venvWith [ "test" ]
#   libvirtPython
#              the lock's libvirt-python package, or null; the nova locks
#              carry it in their nova-compute group, and flake.nix checks
#              what it builds against (checks.<system>.libvirt-pairing)
#   src        the project source, fetched by the commit and NAR hash in
#              sources.json with the release patches applied; null for a lock
#              without a source (tempest, which installs from PyPI)
#   name       the arguments, so a check names the release files and its
#   release    derivations after the environment it is given; attr is the
#   attr       <name>-<rel_> prefix of the flake attributes of the lock
#   python     the interpreter of the venvs
#   version    the ref sources.json pins, the image's version label
#   project    the distribution name of the project (openstack-placement for
#              the placement lock)
#
# Wheels are preferred. uv.lock records no build systems, so every package
# built from an sdist, and the project itself, gets its build system and
# native libraries from the fixups overlay below.
{
  pkgs,
  lib,
  python,
  libvirt,
  uv2nix,
  pyproject-nix,
  pyproject-build-systems,
}:
{
  name,
  release,
  attr,
}:
let
  lockDir = ../locks + "/${release}/${name}";
  sources = lib.importJSON (lockDir + "/sources.json");
  hasSource = sources ? rev;

  # The patch paths in sources.json are relative to the repository root.
  src =
    if hasSource then
      pkgs.applyPatches {
        name = "${name}-${sources.ref}-source";
        src = pkgs.fetchFromGitHub {
          owner = "openstack";
          repo = name;
          inherit (sources) rev hash;
        };
        patches = map (patch: ../.. + "/${patch}") sources.patches;
      }
    else
      null;

  workspace = uv2nix.lib.workspace.loadWorkspace { workspaceRoot = lockDir; };

  lockOverlay = workspace.mkPyprojectOverlay { sourcePreference = "wheel"; };

  # What the packages built from source need besides the sdist: the build
  # system uv.lock does not record, native libraries, and for the project its
  # fetched source; and the fixes installed wheels need. A row applies only
  # to a package the lock contains.
  sourceBuilds = {
    # tempest's lock: 0.7 ships no wheel, and its setup.py fails with
    # "ModuleNotFoundError: No module named 'setuptools'" without one.
    junitxml.buildSystem.setuptools = [ ];
    # The 2026.1 locks of neutron and tempest: 0.7.0 ships no wheel, and its
    # setup.py fails the way junitxml's does.
    ncclient.buildSystem.setuptools = [ ];
    # lhafile (glance), libsass (horizon) and zstd (cinder 2026.2) ship no
    # aarch64-linux wheel at their locked versions and build from the sdist
    # there; each setup.py imports setuptools, which uv.lock does not record.
    # On x86_64-linux their wheels install and the rows change nothing.
    lhafile.buildSystem.setuptools = [ ];
    libsass.buildSystem.setuptools = [ ];
    zstd.buildSystem.setuptools = [ ];
    ovs.buildSystem.setuptools = [ ];
    # glance's locks: 2.9.11 and 2.9.12 ship no Linux wheel. setup.py runs
    # pg_config from PATH, the way nixpkgs' own psycopg2 points it at
    # libpq.pg_config.
    psycopg2 = {
      buildSystem.setuptools = [ ];
      buildInputs = [ pkgs.libpq ];
      nativeBuildInputs = [ pkgs.libpq.pg_config ];
    };
    # pyudev loads libudev with ctypes.util.find_library, which finds nothing
    # without an ldconfig cache, in the build sandbox as in the images:
    # "ImportError: No library named udev" stops the discovery of cinder's
    # suite at rtslib. The library path is written into the module, the way
    # nixpkgs' own pyudev does it.
    pyudev.attrs.postInstall = ''
      substituteInPlace "$out/${python.sitePackages}/pyudev/_ctypeslib/utils.py" \
        --replace-fail "library_name = find_library(name)" \
        "library_name = '${lib.getLib pkgs.udev}/lib/libudev.so.1' if name == 'udev' else find_library(name)"
    '';
    # dogpile.cache 1.5.0's PyMemcacheBackend drops the socket_timeout that
    # oslo.cache passes it, so a Keystone or Nova cache call blocked for as
    # long as TCP did: 213 s for one read under a 120 s Memcached partition
    # (#1560). The patch hands socket_timeout to pymemcache as its connect
    # and read timeout and, unless distributed_lock is set, turns every
    # pymemcache error into a cache miss.
    # A dogpile.cache bump that moves the patched lines fails here with
    # "Hunk #N FAILED", the re-check #1562 tracks.
    dogpile-cache.attrs.postInstall = ''
      patch -p1 --fuzz=0 --no-backup-if-mismatch -d "$out/${python.sitePackages}" \
        < ${./patches/dogpile-cache-1.5.0-pymemcache-timeouts.patch}
    '';
    # barbican's test group: 0.10.0 ships no wheel, and its setup.py fails
    # with "ModuleNotFoundError: No module named 'setuptools'".
    pykmip.buildSystem.setuptools = [ ];
    # setup.cfg asks setuptools to byte-compile in a subprocess that imports
    # distutils, which Python 3.12 removed; with bytecode writing disabled
    # setuptools skips that step, and the installer compiles bytecode itself.
    python-ldap = {
      buildSystem.setuptools = [ ];
      buildInputs = [
        pkgs.openldap
        pkgs.cyrus_sasl
      ];
      attrs.PYTHONDONTWRITEBYTECODE = "1";
    };
    simplegeneric.buildSystem.setuptools = [ ];
    testresources.buildSystem = {
      pbr = [ ];
      setuptools = [ ];
    };
    uwsgi.buildSystem.setuptools = [ ];
    # The nova locks' nova-compute group: PyPI ships no wheel. setup.py finds
    # libvirt through pkg-config and generates the binding from the
    # libvirt-api.xml that `pkg-config --variable=libvirt_api libvirt` names.
    # libvirt is nix/pkgs/libvirt.nix, the one the libvirt image runs.
    libvirt-python = {
      buildSystem.setuptools = [ ];
      nativeBuildInputs = [ pkgs.pkg-config ];
      buildInputs = [ libvirt ];
    };
  }
  // lib.optionalAttrs hasSource {
    # pbr reads the version from git metadata, which the fetched tree lacks:
    # "Versioning for this project requires either an sdist tarball, or
    # access to an upstream git repository". PBR_VERSION supplies the tag.
    # pbr also takes the file list from `git ls-files`, and a project whose
    # pyproject.toml names only its top-level package (keystone 29.0.0)
    # gets its subpackages through that list. The tree is therefore
    # committed to a throwaway repository with a fixed author and date. The
    # fetched tree holds exactly the files upstream tracks, some of which
    # its .gitignore matches (horizon's openstack_dashboard/local/__init__.py
    # and local/enabled/), so every file is added with --force.
    #
    # The commit runs with maintenance.auto=false. Without it, git commit
    # starts `git maintenance run --auto --detach`, whose `git repack -d`
    # deletes the loose object directories under .git/objects/ while the
    # build already walks the tree: setuptools then fails at random with
    # "[Errno 2] No such file or directory: './.git/objects/60'".
    # tests/unit/images/python_env_pbr_commit_test.sh runs this hook.
    ${sources.project} = {
      buildSystem = {
        pbr = [ ];
        setuptools = [ ];
      };
      nativeBuildInputs = [ pkgs.git ];
      attrs = {
        inherit src;
        PBR_VERSION = sources.ref;
        preBuild = ''
          git init --quiet --initial-branch=main
          git add --all --force
          GIT_AUTHOR_DATE=1970-01-01T00:00:01Z GIT_COMMITTER_DATE=1970-01-01T00:00:01Z git -c maintenance.auto=false -c user.name=nix -c user.email=nix@localhost \
            commit --quiet --message "${name} ${sources.ref}"
        '';
      }
      // projectHooks.${name} or { };
    };
  };

  # What a project's package does beyond the install, by lock name.
  projectHooks = {
    # The horizon-operator mounts the rendered Django settings ConfigMap at
    # /etc/openstack-dashboard/. The link makes the installed
    # openstack_dashboard package import it as
    # openstack_dashboard.local.local_settings (the upstream settings.py
    # extension point). It dangles in the store by design and resolves once
    # the ConfigMap is mounted at runtime.
    horizon.postInstall = ''
      ln -sf /etc/openstack-dashboard/local_settings.py \
        "$out/${python.sitePackages}/openstack_dashboard/local/local_settings.py"
    '';
  };

  fixups =
    final: prev:
    lib.mapAttrs (
      name: fix:
      prev.${name}.overrideAttrs (
        old:
        fix.attrs or { }
        // {
          nativeBuildInputs =
            old.nativeBuildInputs
            ++ final.resolveBuildSystem (fix.buildSystem or { })
            ++ fix.nativeBuildInputs or [ ];
          buildInputs = old.buildInputs or [ ] ++ fix.buildInputs or [ ];
        }
      )
    ) (lib.filterAttrs (name: _: prev ? ${name}) sourceBuilds);

  pythonSet = (pkgs.callPackage pyproject-nix.build.packages { inherit python; }).overrideScope (
    lib.composeManyExtensions [
      pyproject-build-systems.overlays.wheel
      lockOverlay
      fixups
    ]
  );

  # The default dependencies of the lock's one workspace member with the
  # named groups enabled.
  venvWith =
    groups:
    pythonSet.mkVirtualEnv "cobaltcore-${name}-${release}-${lib.concatStringsSep "-" groups}-env" (
      lib.mapAttrs (_: _: groups) workspace.deps.default
    );
in
{
  inherit
    src
    name
    release
    attr
    python
    ;
  inherit (sources) project;
  version = sources.ref;
  inherit venvWith;
  venv = pythonSet.mkVirtualEnv "cobaltcore-${name}-${release}-env" workspace.deps.default;
  testVenv = venvWith [ "test" ];
  libvirtPython = pythonSet.libvirt-python or null;
}
