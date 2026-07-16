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
