# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The nova 2026.1 image of the spike with the rootwrap and sudo posture of
# images/nova-compute/Dockerfile and the ceph bindings: the venv under the
# FHS façade, without the libvirt binding and the host tools.
#
# A setuid binary cannot live in /nix/store, so fakeRootCommands copy sudo
# to /usr/bin/sudo with mode 4755 and write its configuration under /etc.
# images/nova-compute/sudoers is installed verbatim and checked with visudo.
# nixpkgs' sudo links PAM. A NOPASSWD rule skips the auth phase but still runs
# the account and session phases, which the PAM stack permits. The auth phase
# denies, so a rule that would ask for a password fails instead of granting
# root without one. Permitting the account phase is a spike shortcut; Ubuntu
# noble's /etc/pam.d/sudo runs the pam_unix account check (README.md, D8).
#
# The venv's bin/python pins sys.prefix to the venv's store path, so the
# ceph-bindings.pth that puts /var/lib/openstack/ceph-bindings on sys.path
# is written into the venv itself, where every entry point reads it.
{
  pkgs,
  python,
  fhsLayout,
  cephBindings,
  rootwrapConf,
  venv,
}:
let
  defaults = import ../lib/image-defaults.nix;

  novaVenv = venv.overrideAttrs (old: {
    postInstall = (old.postInstall or "") + ''
      echo /var/lib/openstack/ceph-bindings > "$out/${python.sitePackages}/ceph-bindings.pth"
    '';
  });

  rootwrapConfFile = pkgs.writeText "rootwrap.conf" (rootwrapConf {
    filtersPath = "/var/lib/openstack/etc/nova/rootwrap.d";
    execDirs = [
      "${novaVenv}/bin"
      "${pkgs.coreutils}/bin"
    ];
  });

  pamSudo = pkgs.writeText "pam-sudo" ''
    auth required pam_deny.so
    account sufficient pam_permit.so
    session required pam_permit.so
  '';
in
pkgs.dockerTools.streamLayeredImage {
  name = "spike/nova-rootwrap";
  tag = "2026.1";
  inherit (defaults) created;

  contents = [
    (fhsLayout {
      venv = novaVenv;
      extraRoots."ceph-bindings" = "${cephBindings}/${python.sitePackages}";
    })
    pkgs.coreutils
  ];

  enableFakechroot = true;
  fakeRootCommands = ''
    mkdir -p /usr/bin /etc/sudoers.d /etc/pam.d /etc/nova /run/sudo/ts

    install -o root -g root -m 4755 ${pkgs.sudo}/bin/sudo /usr/bin/sudo
    printf 'root ALL=(ALL) ALL\n@includedir /etc/sudoers.d\n' > /etc/sudoers
    chown root:root /etc/sudoers
    chmod 0440 /etc/sudoers
    install -o root -g root -m 0440 ${../../../images/nova-compute/sudoers} /etc/sudoers.d/nova-compute
    ${pkgs.sudo}/bin/visudo -csf /etc/sudoers.d/nova-compute
    install -o root -g root -m 0644 ${pamSudo} /etc/pam.d/sudo

    chown root:root /run/sudo /run/sudo/ts
    chmod 0700 /run/sudo/ts

    install -o root -g root -m 0644 ${rootwrapConfFile} /etc/nova/rootwrap.conf
  '';

  config = {
    User = "${toString defaults.uid}:${toString defaults.gid}";
    Env = defaults.env;
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "nova-rootwrap";
      "org.opencontainers.image.description" =
        "OpenStack nova 2026.1 with the rootwrap and sudo posture of nova-compute (spike)";
    };
  };
}
