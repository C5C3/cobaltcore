# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The nova control-plane services: API, metadata, scheduler, conductor and
# the noVNC proxy. No WSGI entry script ships: the nova-operator launches
# uWSGI on the module paths `nova.wsgi.osapi_compute:application` and
# `nova.wsgi.metadata:application` with OS_NOVA_CONFIG_DIR and
# OS_NOVA_CONFIG_FILES. The image stays config-free; the package data files
# api-paste.ini, rootwrap.conf and rootwrap.d/compute.filters ship with the
# venv under /var/lib/openstack/etc/nova/. No qemu-utils: the control-plane
# roles convert nothing. No libvirt: the fake driver imports without it.
# images/nova-compute/ builds the compute image from the same source pin.
#
# DEVIATION from the original build-pipeline design: the image runs as the
# generic 'openstack' user (UID/GID 42424) instead of a per-service user; see
# the DEVIATION paragraph of nix/lib/service-image.nix.
{
  pkgs,
  serviceImage,
  env,
  ...
}:
let
  # The noVNC web assets the nova-novncproxy serves, pinned by tag and commit
  # in novnc.json (Renovate moves both, `make nix-lock` writes the hash). The
  # version check fails a bump that moved the tag without the commit instead
  # of shipping old assets under a new version. The copied set is what the
  # two pages load: vnc_lite.html imports core/rfb.js; vnc.html loads
  # app/ui.js, which reads defaults.json, mandatory.json and package.json (the
  # version the UI shows) and the assets under app/; vendor/ carries pako,
  # the inflate library core/ imports; LICENSE.txt is the licence notice. The
  # nova-operator's `[vnc] novncproxy_base_url` names vnc_lite.html.
  pin = pkgs.lib.importJSON ./novnc.json;
  novnc =
    pkgs.runCommand "novnc-${pin.version}"
      {
        nativeBuildInputs = [ pkgs.jq ];
        src = pkgs.fetchFromGitHub {
          owner = "novnc";
          repo = "noVNC";
          inherit (pin) rev hash;
        };
      }
      ''
        if [ "v$(jq -r .version "$src/package.json")" != ${pkgs.lib.escapeShellArg pin.version} ]; then
          echo ${pkgs.lib.escapeShellArg "novnc.json: version ${pin.version} does not match package.json"} >&2
          exit 1
        fi
        mkdir -p "$out"
        cp -r "$src"/{app,core,vendor,vnc.html,vnc_lite.html,defaults.json,mandatory.json,package.json,LICENSE.txt} "$out/"
      '';
in
serviceImage {
  inherit env;
  description = "OpenStack nova service";

  # The exec readiness probe of the nova-scheduler and nova-conductor
  # processes: it reports ready when a process of this container holds a
  # socket established to the broker port. The nova-operator wires it as
  # `readinessProbe.exec.command: ["/var/lib/openstack/bin/nova-amqp-ready"]`;
  # the two paths must stay in lockstep.
  extraRoots."bin/nova-amqp-ready" = pkgs.runCommand "nova-amqp-ready" { } ''
    install -m 0555 ${./nova-amqp-ready} "$out"
  '';

  rootLinks."usr/share/novnc" = novnc;

  # The state directories Nova writes to, empty and owned by 42424 with
  # /var/lib/nova itself. /var/lib/nova is [DEFAULT] state_path, tmp is the
  # [oslo_concurrency] lock_path, and instances is instances_path, which a
  # compute expects to exist. The nova-operator mounts an emptyDir over the
  # tree, so this layout is what a plain `docker run` gets.
  stateDirs = [
    "var/lib/nova"
    "var/lib/nova/instances"
    "var/lib/nova/tmp"
  ];
}
