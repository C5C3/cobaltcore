#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the horizon invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Horizon CR scaffold used by
every ``invalid-cr`` rejection test, mirroring
``tests/e2e/keystone/invalid-cr/_generate.py``. Each fixture mutates exactly
one aspect of the canonical scaffold so the surrounding CR passes schema
validation for every rule OTHER than the one under test, ensuring the
admission error is attributable to that single field.

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

# Canonical valid Horizon CR scaffold. Any future required field on
# HorizonSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {image} image block body, {cache} cache
# block body, {endpoint} keystoneEndpoint value, {secret_key_name}
# secretKeyRef name, {extra} trailing spec additions.
SCAFFOLD = """\
apiVersion: horizon.openstack.c5c3.io/v1alpha1
kind: Horizon
metadata:
  name: {name}
  namespace: openstack
spec:
  deployment:
    replicas: 1
  image:
{image}
  cache:
{cache}
  keystoneEndpoint: {endpoint}
  secretKeyRef:
    name: {secret_key_name}
    key: secret-key
{extra}"""

VALID_IMAGE = """\
    repository: ghcr.io/c5c3/horizon
    tag: "2025.2\""""

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
    image: str = VALID_IMAGE
    cache: str = VALID_CACHE
    endpoint: str = VALID_ENDPOINT
    secret_key_name: str = "horizon-secret-key"
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            image=self.image,
            cache=self.cache,
            endpoint=self.endpoint,
            secret_key_name=self.secret_key_name,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-image-both-tag-digest.yaml",
        comment="spec.image with both tag and digest violates the ImageSpec XOR CEL rule.",
        name="horizon-invalid-image-both",
        image=(
            "    repository: ghcr.io/c5c3/horizon\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="01-bad-keystone-endpoint.yaml",
        comment="spec.keystoneEndpoint with a non-http(s) scheme violates the CRD pattern.",
        name="horizon-invalid-endpoint",
        endpoint="ldap://keystone.openstack.svc:389",
    ),
    Fixture(
        filename="02-cache-both-modes.yaml",
        comment="spec.cache with both clusterRef and servers violates the CacheSpec XOR CEL rule.",
        name="horizon-invalid-cache-both",
        cache=(
            "    clusterRef:\n"
            "      name: openstack-memcached\n"
            "    servers:\n"
            "    - memcached-0:11211"
        ),
    ),
    Fixture(
        filename="03-empty-secret-key-name.yaml",
        comment="spec.secretKeyRef.name empty violates the SecretRefSpec MinLength marker.",
        name="horizon-invalid-secret-name",
        secret_key_name='""',
    ),
    Fixture(
        filename="04-autoscaling-min-above-max.yaml",
        comment="spec.autoscaling.minReplicas above maxReplicas violates the AutoscalingSpec CEL rule.",
        name="horizon-invalid-autoscaling",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 5\n"
            "    maxReplicas: 2\n"
            "    targetCPUUtilization: 80\n"
        ),
    ),
    Fixture(
        filename="05-gateway-empty-hostname.yaml",
        comment="spec.gateway without a hostname violates the GatewaySpec MinLength marker.",
        name="horizon-invalid-gateway",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            "      name: openstack-gw\n"
            '    hostname: ""\n'
        ),
    ),
    Fixture(
        filename="06-secret-key-in-extraconfig.yaml",
        comment=(
            "SECRET_KEY in spec.extraConfig is rejected by the validating webhook —\n"
            "the key material is env-var-injected and must never enter the ConfigMap."
        ),
        name="horizon-invalid-extraconfig",
        extra=(
            "  extraConfig:\n"
            '    SECRET_KEY: "leaked"\n'
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
        print("run `python3 tests/e2e/horizon/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
