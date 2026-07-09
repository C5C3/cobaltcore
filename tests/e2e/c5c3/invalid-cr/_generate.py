#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the c5c3 ControlPlane invalid-CR Chainsaw fixtures.

Single source of truth for the minimal ControlPlane scaffold used by every
``invalid-cr`` rejection (and acceptance-base) test, mirroring
``tests/e2e/keystone/invalid-cr/_generate.py`` and the leaner
``tests/e2e/horizon/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes every rule OTHER
than the one under test, making the admission error attributable to that field.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test in
its own ephemeral namespace, which isolates the one-ControlPlane-per-namespace
webhook from the parallel c5c3 suites pinned to the shared ``openstack``
namespace, and makes the two accepted transition bases (which persist) coexist
without colliding.

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
# orphan-detection sweep in main() so a fixture removed from FIXTURES but left
# on disk is reported as drift (both directions are guarded).
_FIXTURE_FILENAME_PATTERN = re.compile(r"^[0-9]{2}-.+\.yaml$")

LICENSE_HEADER = """\
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""

# Canonical ControlPlane scaffold. Any future required field on ControlPlaneSpec
# must be added below AND verified against every fixture. Placeholders:
#   {name}           metadata.name
#   {infrastructure} the whole spec.infrastructure block (indent 2) or ""
#   {keystone}       the spec.services.keystone body (indent 6) or "" for nil
#   {horizon}        the spec.services.horizon entry (indent 4) or ""
#
# korc.adminCredential.applicationCredential is intentionally omitted: the
# defaulting webhook materializes it (rotation.mode etc.) before the CRD's
# required-field check runs, exactly as the minimal managed fixtures rely on.
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: {name}
spec:
  openStackRelease: "2025.2"
{infrastructure}  services:
    keystone:
{keystone}{horizon}  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
      passwordSecretRef:
        name: external-admin
        key: password
"""

# A valid External keystone service body (indent 6): the issue's sketch shape.
VALID_EXTERNAL_KEYSTONE = (
    "      mode: External\n"
    "      external:\n"
    "        authURL: https://keystone.example.com/v3\n"
)

# A valid brownfield infrastructure block (indent 2, trailing newline). Used by
# the Managed-mode fixtures and the transition bases so infrastructure is present
# where the mode requires it.
MANAGED_INFRA = (
    "  infrastructure:\n"
    "    database:\n"
    "      host: db.example.com\n"
    "      database: openstack\n"
    "      secretRef:\n"
    "        name: db-creds\n"
    "    cache:\n"
    "      backend: dogpile.cache.pymemcache\n"
    "      servers:\n"
    "      - mc:11211\n"
)


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or a transition base)."""

    filename: str
    comment: str
    name: str
    keystone: str = VALID_EXTERNAL_KEYSTONE
    infrastructure: str = ""
    horizon: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            infrastructure=self.infrastructure,
            keystone=self.keystone,
            horizon=self.horizon,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix (Test: c5c3-invalid-cr) ---
    Fixture(
        filename="00-external-in-managed-explicit.yaml",
        comment="services.keystone.external set with explicit mode: Managed violates the CEL rule.",
        name="cp-external-in-managed",
        keystone=(
            "      mode: Managed\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="01-external-in-managed-unset.yaml",
        comment=(
            "services.keystone.external set with mode unset (defaults Managed)\n"
            "violates the CEL rule after the mode default is applied."
        ),
        name="cp-external-unset-mode",
        keystone=(
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="02-external-mode-without-external.yaml",
        comment="mode: External without the external block violates the CEL rule.",
        name="cp-external-no-block",
        keystone="      mode: External\n",
    ),
    Fixture(
        filename="03-external-with-infrastructure.yaml",
        comment="spec.infrastructure set in External mode is forbidden by the webhook (cross-field).",
        name="cp-external-with-infra",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="04-external-with-horizon.yaml",
        comment="services.horizon set in External mode is forbidden by the webhook (P2, cross-field).",
        name="cp-external-with-horizon",
        horizon="    horizon: {}\n",
    ),
    Fixture(
        filename="05-external-replicas.yaml",
        comment="services.keystone.replicas is forbidden in External mode (CEL).",
        name="cp-external-replicas",
        keystone=VALID_EXTERNAL_KEYSTONE + "      replicas: 3\n",
    ),
    Fixture(
        filename="06-external-image.yaml",
        comment="services.keystone.image is forbidden in External mode (CEL).",
        name="cp-external-image",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      image:\n"
            + "        repository: ghcr.io/c5c3/keystone\n"
            + '        tag: "2025.2"\n'
        ),
    ),
    Fixture(
        filename="07-external-policy-overrides.yaml",
        comment="services.keystone.policyOverrides is forbidden in External mode (CEL).",
        name="cp-external-policy",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      policyOverrides:\n"
            + "        rules:\n"
            + '          "identity:get_user": "role:admin"\n'
        ),
    ),
    Fixture(
        filename="08-external-rotation-interval.yaml",
        comment="services.keystone.rotationInterval is forbidden in External mode (CEL).",
        name="cp-external-rotation",
        keystone=VALID_EXTERNAL_KEYSTONE + "      rotationInterval: 24h\n",
    ),
    Fixture(
        filename="09-external-gateway.yaml",
        comment="services.keystone.gateway is forbidden in External mode (CEL).",
        name="cp-external-gateway",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      gateway:\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "        hostname: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="10-external-public-endpoint.yaml",
        comment="services.keystone.publicEndpoint is forbidden in External mode (CEL, P2).",
        name="cp-external-public",
        keystone=VALID_EXTERNAL_KEYSTONE + "      publicEndpoint: https://keystone.example.com/v3\n",
    ),
    Fixture(
        filename="11-external-authurl-missing.yaml",
        comment="external without authURL violates the CRD required-field check.",
        name="cp-external-no-authurl",
        keystone=(
            "      mode: External\n"
            "      external: {}\n"
        ),
    ),
    Fixture(
        filename="12-external-authurl-not-url.yaml",
        comment="external.authURL without an http(s) scheme violates the CRD pattern.",
        name="cp-external-bad-authurl",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="13-external-endpoint-type-invalid.yaml",
        comment="external.endpointType outside the enum violates the CRD enum.",
        name="cp-external-bad-endpoint",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        endpointType: gopher\n"
        ),
    ),
    Fixture(
        filename="14-external-cabundle-empty-name.yaml",
        comment="external.caBundleSecretRef.name empty violates the SecretRefSpec MinLength marker.",
        name="cp-external-empty-ca",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        caBundleSecretRef:\n"
            '          name: ""\n'
        ),
    ),
    # --- transition wave A: Managed -> External (Test: c5c3-invalid-cr-managed-to-external) ---
    Fixture(
        filename="15-transition-base-managed.yaml",
        comment=(
            "Accepted base for the Managed->External transition test: a brownfield,\n"
            "keystone-unset (staged-adoption) ControlPlane — not External, so\n"
            "infrastructure is required and present."
        ),
        name="cp-transition-a",
        keystone="",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="16-transition-to-external.yaml",
        comment=(
            "UPDATE of the accepted base to a valid External shape is rejected outright:\n"
            "adopting an existing installation must be a fresh External-mode ControlPlane."
        ),
        name="cp-transition-a",
    ),
    # --- transition wave B: External -> Managed (Test: c5c3-invalid-cr-external-to-managed) ---
    Fixture(
        filename="17-transition-base-external.yaml",
        comment=(
            "Accepted base for the External->Managed transition test: the issue's minimal\n"
            "External sketch CR (mode + external.authURL + passwordSecretRef, no\n"
            "infrastructure). Doubles as the sketch-CR acceptance proof."
        ),
        name="cp-transition-b",
    ),
    Fixture(
        filename="18-transition-to-managed.yaml",
        comment=(
            "UPDATE of the accepted External base to a Managed shape is rejected with the\n"
            "reserved phase-3 takeover message. external is explicitly nulled, not merely\n"
            "omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so an\n"
            "omitted external would be RETAINED from the External base and trip the\n"
            "intra-struct CEL rule (external forbidden in Managed mode) at CRD validation,\n"
            "before the validating webhook's transition gate ever runs. Nulling external\n"
            "removes the block, yielding the clean Managed shape whose only remaining\n"
            "violation is the External->Managed transition the webhook rejects with phase-3."
        ),
        name="cp-transition-b",
        keystone=(
            "      mode: Managed\n"
            "      external: null\n"
        ),
        infrastructure=MANAGED_INFRA,
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

    # Orphan sweep (both directions): a fixture file on disk that is not declared
    # in FIXTURES is drift too.
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
        print("run `python3 tests/e2e/c5c3/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
