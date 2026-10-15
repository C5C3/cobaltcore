# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The barbican key manager. No WSGI entry script ships, neither generated
# nor hand-written: both releases ship barbican/wsgi/api.py with a
# module-level `application`, so the barbican-operator launches uWSGI with the
# module path `barbican.wsgi.api:application` against the configuration it
# mounts at /etc/barbican/. The module path and the operator's uWSGI flag have
# to stay in lockstep. The image stays config-free.
#
# DEVIATION from the original build-pipeline design: the image runs as the
# generic 'openstack' user (UID/GID 42424) instead of a per-service user; see
# the DEVIATION paragraph of nix/lib/service-image.nix.
{ serviceImage, env, ... }:
serviceImage {
  inherit env;
  description = "OpenStack barbican service";
}
