# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The Tempest API testing image: tempest and the barbican, keystone, neutron
# and cinder plugins at the versions releases/<release>/test-refs.yaml pins,
# the openstack CLI with its placement and barbican plugins for the e2e
# verify Jobs, and subunit2junitxml for the JUnit report. The tempest Job
# mounts its configuration at /etc/tempest/ and runs
# hack/tempest/run-tests.sh, which sets HOME=/tmp, so the read-only
# /var/lib/openstack needs no owner change.
#
# /usr/share/cirros/ holds the cirros 0.6.3 disk the guest suite of
# tests/e2e-nova-libvirt/guest-dataplane/ boots, so no CI run downloads it.
# Its URL and sha256 are the ones deploy/overlays/kind/hypervisor-operator-fixtures/
# image.yaml pins, and tests/unit/images/tempest_cirros_pin_test.sh fails
# until both files carry the same pair.
#
# The image runs as the generic 'openstack' user (UID/GID 42424) like the
# service images; see the DEVIATION paragraph of nix/lib/service-image.nix.
{
  pkgs,
  serviceImage,
  env,
  ...
}:
serviceImage {
  inherit env;
  description = "OpenStack Tempest API testing framework";

  rootLinks."usr/share/cirros/cirros-0.6.3-x86_64-disk.img" = pkgs.fetchurl {
    url = "https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img";
    sha256 = "7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b";
  };

  # The mount point of the tempest configuration, owned by root.
  extraFakeRootCommands = ''
    mkdir -p /etc/tempest
    chmod 0755 /etc/tempest
  '';
}
