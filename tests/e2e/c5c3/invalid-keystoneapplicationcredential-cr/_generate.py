#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneApplicationCredential invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneApplicationCredential
scaffold used by every rejection test in this directory, mirroring the
mechanics of ``tests/e2e/c5c3/invalid-keystoneroleassignment-cr/_generate.py``.
Each fixture mutates one aspect of the canonical scaffold, so the surrounding
CR passes every rule other than the one under test and the admission error is
attributable to that rule.

The kind has no webhook: a target cluster, where an order may live, runs none.
Every rejection here is therefore the CRD schema's own (minimums, required
fields and CEL rules, the duration rules of spec.rotation included).

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``09`` and ``16``) are each applied once
  and rejected at admission.
* One immutability wave keyed by the metadata.name ``kac-immutable``. It opens
  with a valid base (``10``) that is applied first and admitted, and every later
  fixture of the wave renders that base with one field changed, so Chainsaw
  applies it to the live object as an UPDATE and the CEL transition rules
  evaluate.

The fixtures carry NO metadata.namespace: Chainsaw runs each Test in its own
ephemeral namespace and injects it.

The referenced ControlPlane ``cp-invalid-cr-absent`` never exists, by design,
and neither do the referenced KeystoneUser and KeystoneProject. Admission
tolerates the dangling references, and the reconciler parks the admitted order
on ControlPlaneNotFound without installing its finalizer, so no fixture can
wedge Chainsaw's namespace cleanup.

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

# Canonical KeystoneApplicationCredential scaffold. Placeholders:
#   {name}                   metadata.name
#   {ref_name}               the spec.controlPlaneRef.name line (indent 4) or ""
#   {ref_extra}              further spec.controlPlaneRef fields (indent 4) or ""
#   {user_ref}               the spec.userRef block (indent 2) or ""
#   {project_ref}            the spec.projectRef block (indent 2) or ""
#   {credential_generation}  the spec.credentialGeneration line (indent 2) or ""
#   {rotation}               the spec.rotation block (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneApplicationCredential
metadata:
  name: {name}
spec:
  controlPlaneRef:
{ref_name}{ref_extra}{user_ref}{project_ref}{credential_generation}{rotation}"""

_USER_REF = "  userRef:\n    name: workflow\n"
_PROJECT_REF = "  projectRef:\n    name: workflow-project\n"

# The immutability base declares credentialGeneration 2, so a lowered value is
# reachable.
_BASE_GENERATION = "  credentialGeneration: 2\n"


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or the immutability base)."""

    filename: str
    comment: str
    name: str
    controlplane_name: str = "cp-invalid-cr-absent"
    ref_extra: str = ""
    user_ref: str = _USER_REF
    project_ref: str = _PROJECT_REF
    credential_generation: str = ""
    rotation: str = ""

    def render(self) -> str:
        ref_name = f"    name: {self.controlplane_name}\n" if self.controlplane_name else ""
        body = SCAFFOLD.format(
            name=self.name,
            ref_name=ref_name,
            ref_extra=self.ref_extra,
            user_ref=self.user_ref,
            project_ref=self.project_ref,
            credential_generation=self.credential_generation,
            rotation=self.rotation,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-userref-missing.yaml",
        comment=(
            "An order without a userRef is rejected by the CRD required list. The error\n"
            "carries 'Required value'."
        ),
        name="kac-invalid-userref-missing",
        user_ref="",
    ),
    Fixture(
        filename="01-projectref-missing.yaml",
        comment=(
            "An order without a projectRef is rejected by the CRD required list. The\n"
            "error carries 'Required value'."
        ),
        name="kac-invalid-projectref-missing",
        project_ref="",
    ),
    Fixture(
        filename="02-userref-name-empty.yaml",
        comment=(
            "An empty userRef.name is rejected by the CRD minimum: a reference names an\n"
            "order. The error carries 'should be at least 1 chars'."
        ),
        name="kac-invalid-userref-name-empty",
        user_ref='  userRef:\n    name: ""\n',
    ),
    Fixture(
        filename="03-credentialgeneration-zero.yaml",
        comment=(
            "credentialGeneration 0 is rejected by the CRD minimum; the schema default\n"
            "is 1. The error carries 'should be greater than or equal to 1'."
        ),
        name="kac-invalid-generation-zero",
        credential_generation="  credentialGeneration: 0\n",
    ),
    Fixture(
        filename="04-name-too-long.yaml",
        comment=(
            "A 64-byte metadata.name is rejected by the root CEL rule: the name is\n"
            "carried as a label value on the order's children, which Kubernetes caps\n"
            "at 63 bytes. The error carries 'metadata.name must be at most 63 bytes'."
        ),
        name="kac-" + "x" * 60,
    ),
    Fixture(
        filename="05-controlplaneref-name-missing.yaml",
        comment=(
            "A controlPlaneRef without a name is rejected by the required list of the\n"
            "CRD schema. No defaulting webhook runs for the kind, so nothing fills the\n"
            "field in before validation. The error carries 'Required value'."
        ),
        name="kac-invalid-ref-name-missing",
        controlplane_name="",
        ref_extra="    namespace: cp-namespace\n",
    ),
    Fixture(
        filename="06-controlplaneref-namespace-invalid.yaml",
        comment=(
            "controlPlaneRef.namespace 'Bad_NS' violates the RFC-1123 label pattern\n"
            "(CRD marker). The error carries 'should match'."
        ),
        name="kac-invalid-ref-namespace",
        ref_extra="    namespace: Bad_NS\n",
    ),
    Fixture(
        filename="07-rotation-interval-below-minimum.yaml",
        comment=(
            "A rotation interval of 30s is rejected by the CEL rule on the field: the\n"
            "schedule is off (0s) or rotates at most once a minute. The grace period\n"
            "is 0s, so the block rule holds. The error carries 'rotation.interval is\n"
            "0s or at least 1m'."
        ),
        name="kac-invalid-interval",
        rotation="  rotation:\n    interval: 30s\n    gracePeriod: 0s\n",
    ),
    Fixture(
        filename="08-rotation-grace-not-below-interval.yaml",
        comment=(
            "A grace period as long as the interval is rejected by the CEL rule on the\n"
            "block: the superseded credential must be gone before the next rotation is\n"
            "due. The error carries 'rotation.gracePeriod must be shorter than\n"
            "rotation.interval'."
        ),
        name="kac-invalid-grace-interval",
        rotation="  rotation:\n    interval: 1h\n    gracePeriod: 1h\n",
    ),
    Fixture(
        filename="09-rotation-grace-negative.yaml",
        comment=(
            "A negative grace period is rejected by the CEL rule on the field. The\n"
            "error carries 'rotation.gracePeriod is not negative'."
        ),
        name="kac-invalid-grace-negative",
        rotation="  rotation:\n    gracePeriod: -1h\n",
    ),
    # --- immutability wave: 10 is applied first and admitted, 11-15 are applied
    #     as UPDATEs of it (Chainsaw applies a same-name fixture to a live object
    #     as an RFC 7386 merge patch) ---
    Fixture(
        filename="10-immutable-base.yaml",
        comment=(
            "Valid base for the immutability wave. It is applied FIRST and must\n"
            "SUCCEED. The 11-15 variants reuse this metadata.name, so Chainsaw applies\n"
            "each of them to the live object as an UPDATE, which is what makes the CEL\n"
            "transition rules evaluate at all. credentialGeneration starts at 2, so a\n"
            "lowered value is reachable."
        ),
        name="kac-immutable",
        credential_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="11-immutable-controlplaneref-name.yaml",
        comment=(
            "controlPlaneRef.name is immutable (CEL transition rule): the edit would\n"
            "strand the credentials the order minted on the old plane. The error\n"
            "carries 'controlPlaneRef.name is immutable'."
        ),
        name="kac-immutable",
        controlplane_name="cp-other",
        credential_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="12-immutable-controlplaneref-namespace.yaml",
        comment=(
            "controlPlaneRef.namespace is immutable (CEL transition rule), also when\n"
            "the base left it to the order's own namespace. The error carries\n"
            "'controlPlaneRef.namespace is immutable'."
        ),
        name="kac-immutable",
        ref_extra="    namespace: elsewhere\n",
        credential_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="13-immutable-userref.yaml",
        comment=(
            "userRef is immutable (CEL transition rule): the credentials belong to the\n"
            "user they were minted for. The error carries 'userRef is immutable'."
        ),
        name="kac-immutable",
        user_ref="  userRef:\n    name: another-user\n",
        credential_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="14-immutable-projectref.yaml",
        comment=(
            "projectRef is immutable (CEL transition rule): the credentials are scoped\n"
            "to the project they were minted on. The error carries 'projectRef is\n"
            "immutable'."
        ),
        name="kac-immutable",
        project_ref="  projectRef:\n    name: another-project\n",
        credential_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="15-credentialgeneration-decrease.yaml",
        comment=(
            "credentialGeneration may only increase (CEL transition rule): a rotation\n"
            "is never rolled back. The error carries 'credentialGeneration may only\n"
            "increase'."
        ),
        name="kac-immutable",
        credential_generation="  credentialGeneration: 1\n",
    ),
    Fixture(
        filename="16-rotation-interval-above-maximum.yaml",
        comment=(
            "A rotation interval of 87601h is rejected by the CEL rule on the field:\n"
            "the mint time plus the interval plus the grace period has to stay inside\n"
            "the range of a Go duration. The grace period keeps its 24h default, so the\n"
            "block rule holds. The error carries 'rotation.interval is at most 87600h'."
        ),
        name="kac-invalid-interval-maximum",
        rotation="  rotation:\n    interval: 87601h\n",
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
        print("run `python3 tests/e2e/c5c3/invalid-keystoneapplicationcredential-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
