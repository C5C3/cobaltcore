# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The image of a Python service or of tempest, built from the venv of one
# lock (nix/lib/python-env.nix). images/<name>/image.nix calls it with what
# that image adds; flake.nix builds the result as <name>-<rel_>-image.
#
# The image is the FHS façade of nix/lib/fhs-layout.nix around the venv, plus
# the commands the operators, the e2e suites and the teardown run inside a
# service container besides the venv's own: coreutils (sleep, date -u -d,
# du --exclude, env, test, timeout), findutils, grep, sed and awk.
# /etc/services and /etc/protocols come from iana-etc, /usr/share/zoneinfo
# from tzdata. /tmp and /var/tmp have mode 1777, /var/run links to /run.
#
# sudo is installed setuid root at /usr/bin/sudo, because a setuid binary
# cannot live in /nix/store, with /etc/sudoers granting root alone and an
# empty /etc/sudoers.d: the user runs no command as root, the posture noble's
# sudo gave the Docker images. The PAM stack denies authentication, so a rule
# that asks for a password fails instead of granting root.
#
# The venv gets bin/python3 when it ships only bin/python, because
# hack/gen-option-catalog.sh and the autoscaling e2e suite call python3.
#
# The venv's bin/uwsgi becomes a copy of the uwsgi binary instead of the link
# to the uwsgi package the venv builder writes. uWSGI hands its embedded
# interpreter the path readlink /proc/self/exe gives (core/utils.c,
# uwsgi_get_binary_path) as the program name, and Python looks for pyvenv.cfg
# beside and above that path. The link resolves into the uwsgi package's store
# path, where no pyvenv.cfg is, so uWSGI ran with the base interpreter's
# sys.prefix and every --module and --wsgi-file import of a service package
# failed with ModuleNotFoundError. The copy sits beside the venv's pyvenv.cfg,
# where pip put the binary in the Docker images. tests/lib/uwsgi.sh checks the
# import in every service image.
#
# With cephBindings set, the rados and rbd modules sit at
# /var/lib/openstack/ceph-bindings, and ceph-bindings.pth in the venv's own
# site-packages names that directory: the venv's bin/python pins sys.prefix
# to its store path, so a .pth below the façade's /var/lib/openstack would
# never be read.
#
# Arguments besides env (python-env.nix) and description:
#   packages               store paths linked into the image root (bin/ on PATH)
#   extraRoots, rootLinks  as nix/lib/fhs-layout.nix takes them
#   cephBindings           nix/pkgs/ceph-bindings.nix, or null
#   stateDirs              directories relative to the image root, created
#                          empty and owned by 42424 together with everything
#                          below them
#   extraFakeRootCommands  shell commands run as root in the image root last
#
# The configuration has no Cmd and no Entrypoint: the operators render every
# command onto the container. The environment is that of
# nix/lib/image-defaults.nix with PATH extended to the FHS search path the
# Docker images had, so a tool a rootLinks entry places under /usr/sbin
# resolves. The labels are the licence and vendor labels of
# nix/lib/image-defaults.nix plus title, description, and version, the ref
# the lock pins, which moves only with the lock.
#
# DEVIATION from the original build-pipeline design: the original design's
# Keystone Dockerfile example creates a per-service user. Every service image
# and tempest run as the generic 'openstack' user and group (UID/GID 42424)
# that nix/lib/fhs-layout.nix writes, so one pod security context fits every
# service and no image needs a user of its own.
{ pkgs, fhsLayout }:
{
  env,
  description,
  packages ? [ ],
  extraRoots ? { },
  rootLinks ? { },
  cephBindings ? null,
  stateDirs ? [ ],
  extraFakeRootCommands ? "",
}:
let
  inherit (pkgs) lib;
  inherit (env) name release;
  defaults = import ./image-defaults.nix;
  uid = toString defaults.uid;
  gid = toString defaults.gid;
  inherit (env.python) sitePackages;

  venv = env.venv.overrideAttrs (old: {
    postInstall =
      (old.postInstall or "")
      + ''
        if [ ! -e "$out/bin/python3" ]; then
          ln -s python "$out/bin/python3"
        fi
        if [ -L "$out/bin/uwsgi" ]; then
          cp --remove-destination "$(readlink -f "$out/bin/uwsgi")" "$out/bin/uwsgi"
          chmod 0755 "$out/bin/uwsgi"
        fi
      ''
      + lib.optionalString (cephBindings != null) ''
        echo /var/lib/openstack/ceph-bindings > "$out/${sitePackages}/ceph-bindings.pth"
      '';
  });

  pamSudo = pkgs.writeText "pam-sudo" ''
    auth required pam_deny.so
    account sufficient pam_permit.so
    session required pam_permit.so
  '';
in
pkgs.dockerTools.streamLayeredImage {
  inherit name;
  tag = release;
  inherit (defaults) created;

  contents = [
    (fhsLayout {
      inherit venv;
      extraRoots =
        extraRoots
        // lib.optionalAttrs (cephBindings != null) {
          "ceph-bindings" = "${cephBindings}/${sitePackages}";
        };
      rootLinks = rootLinks // {
        "usr/share/zoneinfo" = "${pkgs.tzdata}/share/zoneinfo";
      };
    })
    pkgs.coreutils
    pkgs.findutils
    pkgs.gnugrep
    pkgs.gnused
    pkgs.gawk
    pkgs.iana-etc
  ]
  ++ packages;

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p -m 1777 /tmp /var/tmp
    chmod 1777 /tmp /var/tmp
    mkdir -p /run
    ln -s /run /var/run

    mkdir -p /usr/bin /etc/sudoers.d /etc/pam.d /run/sudo/ts
    install -o root -g root -m 4755 ${pkgs.sudo}/bin/sudo /usr/bin/sudo
    printf 'root ALL=(ALL) ALL\n@includedir /etc/sudoers.d\n' > /etc/sudoers
    chown root:root /etc/sudoers
    chmod 0440 /etc/sudoers
    install -o root -g root -m 0644 ${pamSudo} /etc/pam.d/sudo
    chown root:root /run/sudo /run/sudo/ts
    chmod 0700 /run/sudo/ts
  ''
  + lib.concatMapStrings (dir: ''
    mkdir -p ${lib.escapeShellArg "/${dir}"}
    chown -R ${uid}:${gid} ${lib.escapeShellArg "/${dir}"}
  '') stateDirs
  + extraFakeRootCommands;

  config = {
    User = "${uid}:${gid}";
    Env = [
      "PATH=/var/lib/openstack/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    ]
    ++ lib.filter (e: !lib.hasPrefix "PATH=" e) defaults.env;
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = name;
      "org.opencontainers.image.description" = description;
      "org.opencontainers.image.version" = env.version;
    };
  };
}
