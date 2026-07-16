#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid GlanceBackend Chainsaw fixtures.

Single source of truth for the minimal valid GlanceBackend scaffold used by
every rejection test in this directory, modeled on
``tests/e2e/keystone/invalid-identitybackend-cr/_generate.py``. Each fixture
mutates exactly one aspect of the canonical scaffold so the surrounding CR
passes schema validation for every rule OTHER than the one under test, ensuring
the admission error is attributable to that single rule.

Three fixture categories share this scaffold:

* Create-rejection fixtures are each applied once and rejected at admission by
  a CEL XValidation rule, a kubebuilder marker, or webhook.validate().
* The glanceRef immutability pair: ``05-immutable-glanceref-base`` is the valid
  base CR applied first, and ``06-immutable-glanceref`` reuses its name so it is
  applied as an UPDATE that re-points spec.glanceRef.name, which the CRD CEL
  transition rule (self == oldSelf, evaluated only on UPDATE) rejects. A
  type-immutability update fixture is deliberately absent: S3 is the only
  Phase-1 enum value, so a changed type is already rejected by the Enum marker
  (mirroring the keystone identity-backend corpus). The single-default sibling
  rule below is the substitute update/create-rejection that needs an existing
  object.
* The single-default pair: ``07-default-base`` is a valid default backend
  applied first, and ``08-second-default`` attaches a second default to the
  same Glance, which the validating webhook rejects.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, which isolates the sibling-default List from the
parallel glance suites pinned to the shared ``openstack`` namespace and lets the
two accepted bases (which persist) get torn down with the namespace.

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

# Canonical valid GlanceBackend scaffold. Any future required field on
# GlanceBackendSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {glance_ref} spec.glanceRef.name, {type}
# spec.type value, {s3} the s3 block body (empty string to omit it), {extra}
# trailing spec additions (isDefault, extraOptions).
SCAFFOLD = """\
apiVersion: glance.openstack.c5c3.io/v1alpha1
kind: GlanceBackend
metadata:
  name: {name}
spec:
  glanceRef:
    name: {glance_ref}
  type: {type}
{s3}{extra}"""

# Valid s3 block (required exactly when type is S3). Carries its trailing
# newline so a fixture that appends {extra} stays well-formed.
VALID_S3 = """\
  s3:
    host: https://s3.example.com
    bucket: glance-images
    credentialsSecretRef:
      name: garage-s3-credentials
"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    glance_ref: str = "glance"
    backend_type: str = "S3"
    s3: str = VALID_S3
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            glance_ref=self.glance_ref,
            type=self.backend_type,
            s3=self.s3,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-type-s3-without-s3-block.yaml",
        comment=(
            "type S3 without a spec.s3 block violates the union CEL rule\n"
            "((self.type == 'S3') == has(self.s3)); the webhook mirrors it."
        ),
        name="glancebackend-no-s3",
        s3="",
    ),
    Fixture(
        filename="01-reserved-name-default.yaml",
        comment=(
            'metadata.name "default" collides with the reserved DEFAULT store\n'
            "section; the validating webhook rejects the exact reserved name."
        ),
        name="default",
    ),
    Fixture(
        filename="02-reserved-name-prefix.yaml",
        comment=(
            'metadata.name "os-glance-staging-store" uses the reserved os-glance-\n'
            "store-section prefix Glance owns; the validating webhook rejects it."
        ),
        name="os-glance-staging-store",
    ),
    Fixture(
        filename="03-extraoptions-denylist.yaml",
        comment=(
            "spec.extraOptions carrying s3_store_access_key duplicates an option the\n"
            "operator renders from spec.s3.credentialsSecretRef; the validating\n"
            "webhook's denylist rejects it."
        ),
        name="glancebackend-denylist",
        extra=(
            "  extraOptions:\n"
            '    s3_store_access_key: "placeholder"\n'
        ),
    ),
    Fixture(
        filename="04-extraoptions-bad-charset.yaml",
        comment=(
            'spec.extraOptions key "s3-store-timeout" carries dashes, which the\n'
            "validating webhook's key allowlist (^[A-Za-z0-9_]+$) rejects before the\n"
            "denylist runs."
        ),
        name="glancebackend-bad-key",
        extra=(
            "  extraOptions:\n"
            '    s3-store-timeout: "10"\n'
        ),
    ),
    Fixture(
        filename="05-immutable-glanceref-base.yaml",
        comment=(
            "Valid base GlanceBackend for the glanceRef-immutability pair. It is\n"
            "applied FIRST and must SUCCEED; 06-immutable-glanceref reuses this name\n"
            "so it is applied as an UPDATE. The referenced Glance does not have to\n"
            "exist at admission time (GitOps ordering), so the base is admitted."
        ),
        name="glancebackend-immutable",
        glance_ref="glance-immutable",
    ),
    Fixture(
        filename="06-immutable-glanceref.yaml",
        comment=(
            "Update of the glancebackend-immutable base CR that re-points\n"
            "spec.glanceRef.name. The spec-level CEL transition rule\n"
            "(self.glanceRef.name == oldSelf.glanceRef.name) rejects the change on\n"
            "UPDATE — re-pointing would orphan the store on the old Glance."
        ),
        name="glancebackend-immutable",
        glance_ref="glance-repointed",
    ),
    Fixture(
        filename="07-default-base.yaml",
        comment=(
            "Valid default GlanceBackend for the single-default pair. It is applied\n"
            "FIRST and must SUCCEED; 08-second-default attaches a second default to\n"
            "the same Glance, which the webhook rejects."
        ),
        name="glancebackend-default-a",
        glance_ref="glance-default",
        extra="  isDefault: true\n",
    ),
    Fixture(
        filename="08-second-default.yaml",
        comment=(
            "Second isDefault GlanceBackend attached to the same Glance as\n"
            "07-default-base. Exactly one default store is allowed per Glance, so\n"
            "the validating webhook's single-default List rejects the newcomer.\n"
            "Applied AFTER 07-default-base. This substitutes for the untestable\n"
            "type-immutability update (S3 is the only enum value)."
        ),
        name="glancebackend-default-b",
        glance_ref="glance-default",
        extra="  isDefault: true\n",
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
        print("run `python3 tests/e2e/glance/invalid-glancebackend-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
