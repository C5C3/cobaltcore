# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The keystone identity service. The keystone-operator runs it under uWSGI
# with `--wsgi-file /var/lib/openstack/bin/keystone-wsgi-public`; the venv
# carries uwsgi from the venv-builder pins and keystone with its ldap extra,
# whose python-ldap and lxml bring libldap, libsasl and libxml2 in their
# closure. The image ships no mod_wsgi: nothing loads it.
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
  description = "OpenStack keystone service";

  # pyproject.nix generates no PBR wsgi_scripts, so the entry script uWSGI
  # loads is written here. The callable matches keystone's setup.cfg
  # wsgi_scripts entry.
  extraRoots."bin/keystone-wsgi-public" = pkgs.writeScript "keystone-wsgi-public" ''
    #!/var/lib/openstack/bin/python
    from keystone.server.wsgi import initialize_public_application
    application = initialize_public_application()
  '';
}
