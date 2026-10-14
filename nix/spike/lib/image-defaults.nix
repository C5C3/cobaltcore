# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# What every image of the spike shares: the openstack user and group of the
# Docker images, which lib/fhs-layout.nix writes to /etc/passwd and the
# images run as, the fixed creation date, the environment, and the OCI
# labels that do not name the service.
{
  uid = 42424;
  gid = 42424;
  created = "1970-01-01T00:00:01Z";
  env = [
    "PATH=/var/lib/openstack/bin:/usr/bin:/bin"
    "LANG=C.UTF-8"
  ];
  labels = {
    "org.opencontainers.image.licenses" = "Apache-2.0";
    "org.opencontainers.image.vendor" = "SAP SE";
  };
}
