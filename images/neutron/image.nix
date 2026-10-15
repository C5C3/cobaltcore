# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The neutron networking service and its OVN metadata agent. No WSGI entry
# script ships: neutron/wsgi/api.py binds a module-level `application`, so the
# neutron-operator launches uWSGI with `--module neutron.wsgi.api` and locates
# the configuration through OS_NEUTRON_CONFIG_DIR and OS_NEUTRON_CONFIG_FILES.
# The image stays config-free; the package data file
# /var/lib/openstack/etc/neutron/api-paste.ini ships with the venv.
#
# DNS resolution: the venv holds ovs 3.7.0 (2026.1) and 3.7.1 (2026.2), whose
# ovs/dns_resolve.py resolves no hostname without the unbound Python module.
# The image ships none; OVNCentral.status.dbAddress and
# status.internalDbAddress publish IP addresses, and the operator renders
# [ovn] ovn_nb_connection and ovn_sb_connection from those.
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
serviceImage {
  inherit env;
  description = "OpenStack neutron service";

  packages = [
    # neutron shells out to `ovsdb-client transact` in pre_fork_initialize
    # (neutron/common/ovn/utils.py) and dies with "[Errno 2] No such file or
    # directory: 'ovsdb-client'" without the binary. The ovsdb wire protocol
    # is schema-independent, so it talks to any OVN release's server.
    pkgs.openvswitch
    # The neutron-ovn-metadata-agent spawns `haproxy -f <cfg>` per network
    # (neutron/agent/metadata/driver_base.py) through `ip netns exec`
    # (neutron/agent/linux/ip_lib.py, neutron/agent/linux/external_process.py).
    pkgs.haproxy
    pkgs.iproute2
    # The agent creates the ovnmeta-<network> namespace through
    # IpNetnsCommand.add (neutron/agent/linux/ip_lib.py), which runs
    # `sysctl -w net.ipv4.conf.all.promote_secondaries=1` inside it through
    # `ip netns exec`. Without the binary that fails with 'exec of "sysctl"
    # failed: No such file or directory', and no network gets its haproxy.
    # The Docker image had /usr/sbin/sysctl from procps.
    pkgs.procps
  ];
}
