#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneUser invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneUser scaffold used by every
rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-keystoneservice-cr/_generate.py``. Each fixture mutates
one aspect of the canonical scaffold, so the surrounding CR passes every rule
other than the one under test and the admission error is attributable to that
rule.

The kind has no webhook: a target cluster, where an order may live, runs none.
Every rejection here is therefore the CRD schema's own (patterns, minimums and
CEL rules), and no fixture leans on a defaulting webhook.

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``05`` and ``11``) are each applied once and
  rejected at admission.
* One immutability wave keyed by the metadata.name ``ku-immutable``. It opens
  with a valid base (``06``) that is applied first and admitted, and every later
  fixture of the wave renders that base with one field changed, so Chainsaw
  applies it to the live object as an UPDATE and the CEL transition rules
  evaluate.

The fixtures carry NO metadata.namespace: Chainsaw runs each Test in its own
ephemeral namespace and injects it.

The referenced ControlPlane ``cp-invalid-cr-absent`` never exists, by design.
Admission tolerates the dangling reference, and the reconciler parks the
admitted order on ControlPlaneNotFound without installing its finalizer, so no
fixture can wedge Chainsaw's namespace cleanup.

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

# Canonical KeystoneUser scaffold. Placeholders:
#   {name}                metadata.name
#   {ref_name}            the spec.controlPlaneRef.name line (indent 4) or ""
#   {ref_extra}           further spec.controlPlaneRef fields (indent 4) or ""
#   {user_name}           the spec.userName line (indent 2) or ""
#   {password_generation} the spec.passwordGeneration line (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneUser
metadata:
  name: {name}
spec:
  controlPlaneRef:
{ref_name}{ref_extra}{user_name}{password_generation}"""


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or the immutability base)."""

    filename: str
    comment: str
    name: str
    controlplane_name: str = "cp-invalid-cr-absent"
    ref_extra: str = ""
    user_name: str = ""
    password_generation: str = ""

    def render(self) -> str:
        ref_name = f"    name: {self.controlplane_name}\n" if self.controlplane_name else ""
        body = SCAFFOLD.format(
            name=self.name,
            ref_name=ref_name,
            ref_extra=self.ref_extra,
            user_name=self.user_name,
            password_generation=self.password_generation,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


# The immutability base's declared values. The wave below changes one of them
# per fixture.
_BASE_USER_NAME = "  userName: svc-user\n"
_BASE_GENERATION = "  passwordGeneration: 2\n"

FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-username-comma.yaml",
        comment=(
            "userName carrying a comma is rejected by the CRD pattern that mirrors\n"
            "K-ORC's OpenStackName: admitted, the name would only be rejected by the\n"
            "K-ORC CRD, which wedges the reconcile. The error carries 'should match'."
        ),
        name="ku-invalid-username-comma",
        user_name="  userName: a,b\n",
    ),
    Fixture(
        filename="01-username-empty.yaml",
        comment=(
            "An empty userName is rejected by the CRD minimum: the default is spelled\n"
            "by leaving the field out, which means metadata.name. The error carries\n"
            "'should be at least 1 chars'."
        ),
        name="ku-invalid-username-empty",
        user_name='  userName: ""\n',
    ),
    Fixture(
        filename="02-passwordgeneration-zero.yaml",
        comment=(
            "passwordGeneration 0 is rejected by the CRD minimum; the schema default\n"
            "is 1. The error carries 'should be greater than or equal to 1'."
        ),
        name="ku-invalid-generation-zero",
        password_generation="  passwordGeneration: 0\n",
    ),
    Fixture(
        filename="03-name-too-long.yaml",
        comment=(
            "A 64-byte metadata.name is rejected by the root CEL rule: the name is\n"
            "carried as a label value on the order's children, which Kubernetes caps\n"
            "at 63 bytes. The error carries 'metadata.name must be at most 63 bytes'."
        ),
        name="ku-" + "x" * 61,
    ),
    Fixture(
        filename="04-controlplaneref-name-missing.yaml",
        comment=(
            "A controlPlaneRef without a name is rejected by the required list of the\n"
            "CRD schema. No defaulting webhook runs for the kind, so nothing fills the\n"
            "field in before validation. The error carries 'Required value'."
        ),
        name="ku-invalid-ref-name-missing",
        controlplane_name="",
        ref_extra="    namespace: cp-namespace\n",
    ),
    Fixture(
        filename="05-controlplaneref-namespace-invalid.yaml",
        comment=(
            "controlPlaneRef.namespace 'Bad_NS' violates the RFC-1123 label pattern\n"
            "(CRD marker). The error carries 'should match'."
        ),
        name="ku-invalid-ref-namespace",
        ref_extra="    namespace: Bad_NS\n",
    ),
    # --- immutability wave: 06 is applied first and admitted, 07-10 are applied
    #     as UPDATEs of it (Chainsaw applies a same-name fixture to a live object
    #     as an RFC 7386 merge patch) ---
    Fixture(
        filename="06-immutable-base.yaml",
        comment=(
            "Valid base for the immutability wave. It is applied FIRST and must\n"
            "SUCCEED. The 07-10 variants reuse this metadata.name, so Chainsaw applies\n"
            "each of them to the live object as an UPDATE, which is what makes the CEL\n"
            "transition rules evaluate at all. userName is declared explicitly and\n"
            "passwordGeneration starts at 2, so a lowered value is reachable."
        ),
        name="ku-immutable",
        user_name=_BASE_USER_NAME,
        password_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="07-immutable-controlplaneref-name.yaml",
        comment=(
            "controlPlaneRef.name is immutable (CEL transition rule): the edit would\n"
            "strand the Keystone user the order created on the old plane. The error\n"
            "carries 'controlPlaneRef.name is immutable'."
        ),
        name="ku-immutable",
        controlplane_name="cp-other",
        user_name=_BASE_USER_NAME,
        password_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="08-immutable-controlplaneref-namespace.yaml",
        comment=(
            "controlPlaneRef.namespace is immutable (CEL transition rule), also when\n"
            "the base left it to the order's own namespace. The error carries\n"
            "'controlPlaneRef.namespace is immutable'."
        ),
        name="ku-immutable",
        ref_extra="    namespace: elsewhere\n",
        user_name=_BASE_USER_NAME,
        password_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="09-immutable-username.yaml",
        comment=(
            "userName is immutable (root CEL transition rule over the effective name):\n"
            "the name identifies a live Keystone user. The error carries 'userName is\n"
            "immutable'."
        ),
        name="ku-immutable",
        user_name="  userName: svc-user-renamed\n",
        password_generation=_BASE_GENERATION,
    ),
    Fixture(
        filename="10-passwordgeneration-decrease.yaml",
        comment=(
            "passwordGeneration may only increase (CEL transition rule): a lower value\n"
            "would roll the password back. The error carries 'passwordGeneration may\n"
            "only increase'."
        ),
        name="ku-immutable",
        user_name=_BASE_USER_NAME,
        password_generation="  passwordGeneration: 1\n",
    ),
    # --- create-rejection matrix, continued after the wave ---
    Fixture(
        filename="11-username-too-long.yaml",
        comment=(
            "A 256-byte userName is rejected by the CRD maximum that mirrors K-ORC's\n"
            "OpenStackName cap: admitted, the name would only be rejected by the K-ORC\n"
            "CRD, which wedges the reconcile. The error carries 'Too long'."
        ),
        name="ku-invalid-username-too-long",
        user_name="  userName: " + "a" * 256 + "\n",
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
        print("run `python3 tests/e2e/c5c3/invalid-keystoneuser-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
