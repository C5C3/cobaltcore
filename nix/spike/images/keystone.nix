# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The keystone 2026.1 image of the spike: the venv under the FHS façade, the
# WSGI entry script images/keystone/Dockerfile writes, the user and the OCI
# labels of the Docker image.
#
# The venv and the script reach the image through /var/lib/openstack only.
# As contents entries the venv would also link its bin/, lib/ and pyvenv.cfg
# into the image root, and the script could not merge below the symlinked
# /var/lib/openstack.
{
  pkgs,
  fhsLayout,
  venv,
}:
let
  defaults = import ../lib/image-defaults.nix;

  # uv's --prefix install generates no wsgi_scripts, so the Dockerfile writes
  # this script; pyproject.nix generates none either.
  wsgiPublic = pkgs.writeScript "keystone-wsgi-public" ''
    #!/var/lib/openstack/bin/python
    from keystone.server.wsgi import initialize_public_application
    application = initialize_public_application()
  '';
in
pkgs.dockerTools.streamLayeredImage {
  name = "spike/keystone";
  tag = "2026.1";
  inherit (defaults) created;

  contents = [
    (fhsLayout {
      inherit venv;
      extraRoots."bin/keystone-wsgi-public" = wsgiPublic;
    })
    pkgs.coreutils
  ];

  config = {
    User = "${toString defaults.uid}:${toString defaults.gid}";
    Env = defaults.env;
    Labels = defaults.labels // {
      "org.opencontainers.image.title" = "keystone";
      "org.opencontainers.image.description" = "OpenStack keystone service";
    };
  };
}
