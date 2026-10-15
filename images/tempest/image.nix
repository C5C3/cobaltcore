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
# The image runs as the generic 'openstack' user (UID/GID 42424) like the
# service images; see the DEVIATION paragraph of nix/lib/service-image.nix.
{ serviceImage, env, ... }:
serviceImage {
  inherit env;
  description = "OpenStack Tempest API testing framework";

  # The mount point of the tempest configuration, owned by root.
  extraFakeRootCommands = ''
    mkdir -p /etc/tempest
    chmod 0755 /etc/tempest
  '';
}
