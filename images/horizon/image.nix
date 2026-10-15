# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The horizon dashboard. The horizon-operator runs it under uWSGI with
# `--module openstack_dashboard.wsgi` (the module ships `application`), so no
# entry script ships, and mounts the rendered Django settings at
# /etc/openstack-dashboard/, where the horizon package's
# openstack_dashboard/local/local_settings.py link points
# (nix/lib/python-env.nix). The static assets are built into
# /var/lib/openstack/horizon-static here, collectstatic and offline
# compression included, so the runtime image needs no init container.
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
  inherit (env.python) sitePackages;
  inherit (env) release;

  # A store path is read-only, so the Django management commands run against
  # a writable copy of openstack_dashboard with build-time settings. The copy
  # keeps the venv's symlinks, so only the settings file and the bytecode are
  # new. CACHES must be a working in-process backend: horizon wraps the
  # Angular template-cache preloads in {% cache %} (horizon/_scripts.html),
  # and with the default PyMemcacheCache pointing at an unreachable
  # 127.0.0.1:11211 the cache lookup raises, django-compressor silently skips
  # that block, and the shipped manifest.json lacks its key: every page render
  # then fails at runtime with OfflineGenerationError once a real Memcached is
  # reachable. compress collects the templates into a set, so the key order of
  # manifest.json follows the hash seed of the run; the keys are rewritten
  # sorted, in compress's own format, so every build writes the same file.
  horizonStatic = pkgs.runCommand "horizon-static-${release}" { nativeBuildInputs = [ env.venv ]; } ''
    mkdir build
    cp -r --no-preserve=mode "$(readlink -f ${env.venv}/${sitePackages}/openstack_dashboard)" build/openstack_dashboard
    rm -f build/openstack_dashboard/local/local_settings.py
    cat > build/openstack_dashboard/local/local_settings.py <<SETTINGS
    DEBUG = False
    SECRET_KEY = "build-time-throwaway"
    STATIC_ROOT = "$out"
    COMPRESS_OFFLINE = True
    ALLOWED_HOSTS = ["*"]
    CACHES = {"default": {"BACKEND": "django.core.cache.backends.locmem.LocMemCache"}}
    SETTINGS
    export PYTHONPATH="$PWD/build" DJANGO_SETTINGS_MODULE=openstack_dashboard.settings
    python -m django collectstatic --noinput
    python -m django compress --force
    python - "$out/dashboard/manifest.json" <<'PY'
    import json, sys
    with open(sys.argv[1]) as f:
        manifest = json.load(f)
    with open(sys.argv[1], "w") as f:
        f.write(json.dumps(manifest, indent=2, sort_keys=True))
    PY
  '';
in
serviceImage {
  inherit env;
  description = "OpenStack horizon service";

  extraRoots."horizon-static" = horizonStatic;

  # The mount point of the settings ConfigMap, owned by root like the rest of
  # the image.
  extraFakeRootCommands = ''
    mkdir -p /etc/openstack-dashboard
    chmod 0755 /etc/openstack-dashboard
  '';
}
