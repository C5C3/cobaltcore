#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the glance invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Glance CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/horizon/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes schema validation
for every rule OTHER than the one under test, ensuring the admission error is
attributable to that single field.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, so the create-rejection fixtures never depend on
the shared ``openstack`` namespace existing.

Usage:

    # Regenerate all fixtures from this single source of truth.
    python3 _generate.py

    # CI-friendly drift check: exit non-zero if any on-disk fixture diverges
    # from the regenerated content (or an orphan fixture file exists).
    python3 _generate.py --check
"""

from __future__ import annotations

import re
import sys
from dataclasses import dataclass
from pathlib import Path

# Matches every two-digit-prefixed fixture in this directory. Used by the
# orphan-detection sweep in main() so a fixture removed from FIXTURES but
# left on disk is reported as drift (both directions are guarded).
_FIXTURE_FILENAME_PATTERN = re.compile(r"^[0-9]{2}-.+\.yaml$")

LICENSE_HEADER = """\
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""

# Canonical valid Glance CR scaffold. Any future required field on GlanceSpec
# must be added below AND verified against every fixture. Placeholders:
# {name} CR name, {release} openStackRelease value, {image} image block body,
# {database} database block body, {cache} cache block body, {endpoint}
# keystoneEndpoint value, {secret_name} serviceUser.secretRef name, {extra}
# trailing spec additions.
SCAFFOLD = """\
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: Glance
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
  deployment:
    replicas: 1
  image:
{image}
  database:
{database}
  cache:
{cache}
  keystoneEndpoint: {endpoint}
  serviceUser:
    secretRef:
      name: {secret_name}
{extra}"""

VALID_RELEASE = "2025.2"

VALID_IMAGE = """\
    repository: ghcr.io/c5c3/glance
    tag: "2025.2\""""

VALID_DATABASE = """\
    clusterRef:
      name: openstack-db
    database: glance
    secretRef:
      name: glance-db"""

VALID_CACHE = """\
    clusterRef:
      name: openstack-memcached"""

VALID_ENDPOINT = "http://keystone.openstack.svc.cluster.local:5000/v3"


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    release: str = VALID_RELEASE
    image: str = VALID_IMAGE
    database: str = VALID_DATABASE
    cache: str = VALID_CACHE
    endpoint: str = VALID_ENDPOINT
    secret_name: str = "glance-service-password"
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            image=self.image,
            database=self.database,
            cache=self.cache,
            endpoint=self.endpoint,
            secret_name=self.secret_name,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-image-both-tag-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL\n"
            "rule (has(self.tag) != has(self.digest)); the webhook mirrors it."
        ),
        name="glance-invalid-image-both",
        image=(
            "    repository: ghcr.io/c5c3/glance\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="01-openstackrelease-pattern.yaml",
        comment=(
            "spec.openStackRelease with a non-cadence value violates the CRD\n"
            "pattern (^\\d{4}\\.[12]$); the webhook / release.ParseRelease agree."
        ),
        name="glance-invalid-release",
        release="queens",
    ),
    Fixture(
        filename="02-database-clusterref-and-host.yaml",
        comment=(
            "spec.database with both clusterRef and host violates the shared\n"
            "DatabaseSpec XOR CEL rule; the webhook mirrors it via DatabaseXOR."
        ),
        name="glance-invalid-database-both",
        database=(
            "    clusterRef:\n"
            "      name: openstack-db\n"
            "    database: glance\n"
            "    secretRef:\n"
            "      name: glance-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="03-cache-clusterref-and-servers.yaml",
        comment=(
            "spec.cache with both clusterRef and servers violates the shared\n"
            "CacheSpec XOR CEL rule; the webhook mirrors it via CacheXOR."
        ),
        name="glance-invalid-cache-both",
        cache=(
            "    clusterRef:\n"
            "      name: openstack-memcached\n"
            "    servers:\n"
            "    - memcached-0:11211"
        ),
    ),
    Fixture(
        filename="04-keystone-endpoint-bad-scheme.yaml",
        comment=(
            "spec.keystoneEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern (^https?://); the webhook validateEndpointURL mirrors it."
        ),
        name="glance-invalid-endpoint",
        endpoint="ftp://keystone.openstack.svc.cluster.local:5000/v3",
    ),
    Fixture(
        filename="05-autoscaling-min-above-max.yaml",
        comment=(
            "spec.autoscaling.minReplicas above maxReplicas violates the shared\n"
            "AutoscalingSpec CEL rule; the webhook mirrors it."
        ),
        name="glance-invalid-autoscaling",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 5\n"
            "    maxReplicas: 2\n"
            "    targetCPUUtilization: 80\n"
        ),
    ),
    Fixture(
        filename="06-gateway-empty-hostname.yaml",
        comment=(
            "spec.gateway with an empty hostname violates the GatewaySpec\n"
            "MinLength=1 marker; the webhook also requires it when gateway is set."
        ),
        name="glance-invalid-gateway",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            "      name: openstack-gw\n"
            '    hostname: ""\n'
        ),
    ),
    Fixture(
        filename="07-uwsgi-keepalive-timeout-without-keepalive.yaml",
        comment=(
            "spec.apiServer.uwsgi.httpKeepAliveTimeout set while httpKeepAlive is\n"
            "false violates the UWSGISpec CEL rule (the timeout flag is only\n"
            "emitted under --http-keepalive). httpKeepAlive is set EXPLICITLY to\n"
            "false because the defaulting webhook restores true when it is unset,\n"
            "which would satisfy the rule and make this CR admissible."
        ),
        name="glance-invalid-uwsgi-keepalive",
        extra=(
            "  apiServer:\n"
            "    uwsgi:\n"
            "      httpKeepAlive: false\n"
            "      httpKeepAliveTimeout: 30\n"
        ),
    ),
    Fixture(
        filename="08-extraconfig-empty-section.yaml",
        comment=(
            "spec.extraConfig with an empty section name is rejected by the\n"
            "validating webhook — extraConfig is a preserve-unknown-fields map, so\n"
            "CEL cannot constrain its keys and the webhook is the sole gate."
        ),
        name="glance-invalid-extraconfig-section",
        extra=(
            "  extraConfig:\n"
            '    "":\n'
            "      foo: bar\n"
        ),
    ),
    Fixture(
        filename="09-extraconfig-empty-key.yaml",
        comment=(
            "spec.extraConfig with an empty option key is rejected by the\n"
            "validating webhook — a bare `= value` line must never reach the\n"
            "rendered glance-api.conf."
        ),
        name="glance-invalid-extraconfig-key",
        extra=(
            "  extraConfig:\n"
            "    image_import_opts:\n"
            '      "": bar\n'
        ),
    ),
    Fixture(
        filename="10-extraconfig-owned-password.yaml",
        comment=(
            "spec.extraConfig setting [keystone_authtoken] password is rejected by\n"
            "the validating webhook. The operator owns it via\n"
            "spec.serviceUser.secretRef and the middleware reads it from the\n"
            "OS_KEYSTONE_AUTHTOKEN__PASSWORD env override, so a file value is inert\n"
            "at runtime but would leak the service password into the\n"
            "namespace-readable ConfigMap — one of the registry's Rejected entries."
        ),
        name="glance-invalid-extraconfig-password",
        extra=(
            "  extraConfig:\n"
            "    keystone_authtoken:\n"
            "      password: s3cr3t\n"
        ),
    ),
    Fixture(
        filename="11-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option 'default_backend_typo' in\n"
            "the known [glance_store] section is rejected by the validating webhook:\n"
            "the option is absent from the glance 2025.2 option catalog, so a typo'd\n"
            "key can never silently reach the rendered glance-api.conf."
        ),
        name="glance-invalid-extraconfig-unknown-option",
        extra=(
            "  extraConfig:\n"
            "    glance_store:\n"
            "      default_backend_typo: s3\n"
        ),
    ),
    Fixture(
        filename="12-extraconfig-unknown-section.yaml",
        comment=(
            "spec.extraConfig declaring an unknown section 'glance_stor' (a typo for\n"
            "[glance_store]) is rejected by the validating webhook: the section is\n"
            "absent from the glance 2025.2 option catalog, so a typo'd section name\n"
            "can never silently reach the rendered glance-api.conf."
        ),
        name="glance-invalid-extraconfig-unknown-section",
        extra=(
            "  extraConfig:\n"
            "    glance_stor:\n"
            "      default_backend: s3\n"
        ),
    ),
    Fixture(
        filename="13-importfiltering-allow-and-deny-hosts.yaml",
        comment=(
            "spec.importFiltering with both allowedHosts and disallowedHosts non-empty\n"
            "violates the ImportFilteringSpec CEL rule: glance ignores the deny-list\n"
            "whenever the matching allow-list is non-empty, so accepting both would\n"
            "silently drop the denied host. The webhook mirrors it via\n"
            "validateImportFiltering."
        ),
        name="glance-invalid-importfiltering-hosts",
        extra=(
            "  importFiltering:\n"
            "    allowedHosts:\n"
            "    - mirror.example.com\n"
            "    disallowedHosts:\n"
            "    - 169.254.169.254\n"
        ),
    ),
    Fixture(
        filename="14-importfiltering-host-control-char.yaml",
        comment=(
            "spec.importFiltering.disallowedHosts carries a host with an embedded newline.\n"
            "The host lists are the only free-form strings in the block and are joined\n"
            "verbatim into the [import_filtering_opts] keys of glance-api.conf, so a\n"
            "newline would render a whole [profiler] section the extraConfig ownership\n"
            "and catalog gates never see — they inspect map structure, not values. Only\n"
            "the webhook rejects this: the item markers bound length, not content."
        ),
        name="glance-invalid-importfiltering-control-char",
        extra=(
            "  importFiltering:\n"
            "    disallowedHosts:\n"
            '    - "evil.example.com\\n[profiler]\\nenabled = true"\n'
        ),
    ),
    Fixture(
        filename="15-extraconfig-owned-importfiltering.yaml",
        comment=(
            "spec.extraConfig setting an [import_filtering_opts] key is rejected by\n"
            "the validating webhook. All six keys are Rejected registry entries owned\n"
            "by spec.importFiltering: the typed field expresses every one of them, so\n"
            "an extraConfig override buys no reach — only a way around the exclusivity\n"
            "rules, the host INI guard, and the warning that flags a loosened filter."
        ),
        name="glance-invalid-extraconfig-importfiltering",
        extra=(
            "  extraConfig:\n"
            "    import_filtering_opts:\n"
            '      disallowed_hosts: ""\n'
        ),
    ),
    Fixture(
        filename="16-extraconfig-value-control-char.yaml",
        comment=(
            "spec.extraConfig carrying a newline in an option VALUE is rejected by the\n"
            "validating webhook. The rendered INI writes `key = value` verbatim, so the\n"
            "newline would inject a whole [profiler] section past the ownership and\n"
            "catalog gates — they key on (section, key) names and never look inside a\n"
            "value. The section and key here are both catalog-known and owned, which is\n"
            "exactly the shape that would otherwise be admitted."
        ),
        name="glance-invalid-extraconfig-value-control-char",
        extra=(
            "  extraConfig:\n"
            "    glance_store:\n"
            '      default_backend: "s3\\n[profiler]\\nenabled = true"\n'
        ),
    ),
    Fixture(
        filename="17-dbpurge-retentiondays-zero.yaml",
        comment=(
            "spec.dbPurge.retentionDays of 0 violates the DBPurgeSpec CRD\n"
            "Minimum=1 marker; the webhook's defense-in-depth check mirrors it."
        ),
        name="glance-invalid-dbpurge-retentiondays",
        extra=(
            "  dbPurge:\n"
            "    retentionDays: 0\n"
        ),
    ),
    Fixture(
        filename="18-dbpurge-invalid-schedule.yaml",
        comment=(
            "spec.dbPurge.schedule with a non-cron value is rejected by the\n"
            "validating webhook — the cron grammar has no CRD schema counterpart,\n"
            "so DBPurgeSpec.Schedule is validated exclusively via\n"
            "validation.CronSchedule."
        ),
        name="glance-invalid-dbpurge-schedule",
        extra=(
            "  dbPurge:\n"
            '    schedule: "totally-not-cron"\n'
        ),
    ),
    Fixture(
        filename="19-name-too-long-for-purge-cronjob.yaml",
        comment=(
            "A metadata.name of 44 characters is rejected by the validating\n"
            "webhook: the {name}-db-purge CronJob would exceed the 52-character\n"
            "cap Kubernetes enforces on CronJob names, so the CR would admit and\n"
            "then never reconcile."
        ),
        name="glance-invalid-name-far-too-long-for-cronjob",
    ),
    Fixture(
        filename="20-staging-sizelimit-zero.yaml",
        comment=(
            "spec.staging.sizeLimit of 0 is rejected by the validating webhook —\n"
            "a resource.Quantity renders as x-kubernetes-int-or-string in the CRD\n"
            "schema, which carries no Minimum marker, so ValidateStaging is the\n"
            "sole gate against an unusable scratch-volume bound. It enforces a 1Mi\n"
            "floor rather than mere positivity, because the schema pattern also\n"
            "admits the sub-byte milli suffix (`100m` for `100Mi`)."
        ),
        name="glance-invalid-staging-sizelimit",
        extra=(
            "  staging:\n"
            "    sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="21-staging-unbounded-with-sizelimit.yaml",
        comment=(
            "spec.staging.unbounded and spec.staging.sizeLimit contradict each\n"
            "other: unbounded renders both scratch emptyDirs with no sizeLimit at\n"
            "all, so there is no bound left for sizeLimit to name. The CEL rule on\n"
            "StagingSpec rejects the pair at the schema level, with the validating\n"
            "webhook repeating it for the ControlPlane copy of the same type."
        ),
        name="glance-invalid-staging-unbounded",
        extra=(
            "  staging:\n"
            "    unbounded: true\n"
            "    sizeLimit: 40Gi\n"
        ),
    ),
    Fixture(
        filename="22-imagecache-sizelimit-zero.yaml",
        comment=(
            "spec.imageCache.sizeLimit of 0 is rejected by the validating webhook —\n"
            "a resource.Quantity renders as x-kubernetes-int-or-string in the CRD\n"
            "schema, which carries no Minimum marker, so ValidateImageCache is the\n"
            "sole gate against an unusable cache bound. It enforces a 1Mi floor\n"
            "rather than mere positivity, because the schema pattern also admits the\n"
            "sub-byte milli suffix (`100m` for `100Mi`)."
        ),
        name="glance-invalid-imagecache-sizelimit",
        extra=(
            "  imageCache:\n"
            "    sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="23-imagecache-interval-below-floor.yaml",
        comment=(
            "spec.imageCache.maintenanceInterval below a minute is rejected by the\n"
            "validating webhook — a metav1.Duration renders as a plain string in the\n"
            "CRD schema, a shape that carries no floor at all, so ValidateImageCache\n"
            "is the sole gate. Every tick walks the cache directory and its sqlite\n"
            "index, so a sub-minute loop spends the pod's local-disk bandwidth on\n"
            "maintenance rather than on the downloads the cache exists to accelerate."
        ),
        name="glance-invalid-imagecache-interval",
        extra=(
            "  imageCache:\n"
            "    maintenanceInterval: 59s\n"
        ),
    ),
    Fixture(
        filename="24-imagecache-middleware-name-collision.yaml",
        comment=(
            "spec.middleware[].name of `cache` alongside spec.imageCache is rejected\n"
            "by the validating webhook: the operator injects that paste filter itself\n"
            "while the cache is on and renders its [filter:cache] section, so the two\n"
            "definitions would collide and the pipeline would end up with wiring\n"
            "nobody wrote. It correlates two spec fields, so no schema marker can\n"
            "express it and admission is the sole gate. The reservation lasts only as\n"
            "long as spec.imageCache is set — drop that block and this same\n"
            "middleware is admissible again."
        ),
        name="glance-invalid-imagecache-middleware",
        extra=(
            "  imageCache: {}\n"
            "  middleware:\n"
            "  - name: cache\n"
            "    filterFactory: example.middleware:Filter.factory\n"
            "    position: after\n"
        ),
    ),
)


def main() -> int:
    check = "--check" in sys.argv[1:]
    here = Path(__file__).resolve().parent
    drift = False

    for fixture in FIXTURES:
        target = here / fixture.filename
        content = fixture.render()
        if check:
            on_disk = target.read_text(encoding="utf-8") if target.exists() else None
            if on_disk != content:
                print(f"DRIFT: {fixture.filename}")
                drift = True
        else:
            target.write_text(content, encoding="utf-8")
            print(f"wrote {fixture.filename}")

    # Orphan sweep (both directions): a fixture file on disk that is not
    # declared in FIXTURES is drift too.
    declared = {fixture.filename for fixture in FIXTURES}
    for path in sorted(here.iterdir()):
        if not _FIXTURE_FILENAME_PATTERN.match(path.name):
            continue
        if path.name in declared:
            continue
        if check:
            print(f"DRIFT: orphan fixture {path.name} not declared in FIXTURES")
            drift = True
        else:
            path.unlink()
            print(f"removed orphan {path.name}")

    if check and drift:
        print("run `python3 tests/e2e/glance/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
