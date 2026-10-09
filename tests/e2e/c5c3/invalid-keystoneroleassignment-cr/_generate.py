#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneRoleAssignment invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneRoleAssignment scaffold used by
every rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-keystoneuser-cr/_generate.py``. Each fixture mutates
one aspect of the canonical scaffold, so the surrounding CR passes every rule
other than the one under test and the admission error is attributable to that
rule.

The kind has no webhook: a target cluster, where an order may live, runs none.
Every rejection here is therefore the CRD schema's own (patterns, minimums and
CEL rules).

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``09``) are each applied once and rejected
  at admission.
* One immutability wave keyed by the metadata.name ``kra-immutable``. It opens
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

# Canonical KeystoneRoleAssignment scaffold. Placeholders:
#   {name}         metadata.name
#   {ref_name}     the spec.controlPlaneRef.name line (indent 4) or ""
#   {ref_extra}    further spec.controlPlaneRef fields (indent 4) or ""
#   {user_ref}     the spec.userRef block (indent 2) or ""
#   {project_ref}  the spec.projectRef block (indent 2) or ""
#   {role}         the spec.role line (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneRoleAssignment
metadata:
  name: {name}
spec:
  controlPlaneRef:
{ref_name}{ref_extra}{user_ref}{project_ref}{role}"""

_USER_REF = "  userRef:\n    name: workflow\n"
_PROJECT_REF = "  projectRef:\n    name: workflow-project\n"
_ROLE = "  role: member\n"


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
    role: str = _ROLE

    def render(self) -> str:
        ref_name = f"    name: {self.controlplane_name}\n" if self.controlplane_name else ""
        body = SCAFFOLD.format(
            name=self.name,
            ref_name=ref_name,
            ref_extra=self.ref_extra,
            user_ref=self.user_ref,
            project_ref=self.project_ref,
            role=self.role,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-role-comma.yaml",
        comment=(
            "role carrying a comma is rejected by the CRD pattern that mirrors K-ORC's\n"
            "KeystoneName. The error carries 'should match'."
        ),
        name="kra-invalid-role-comma",
        role="  role: a,b\n",
    ),
    Fixture(
        filename="01-role-empty.yaml",
        comment="An empty role is rejected by the CRD minimum. The error carries\n'should be at least 1 chars'.",
        name="kra-invalid-role-empty",
        role='  role: ""\n',
    ),
    Fixture(
        filename="02-role-too-long.yaml",
        comment=(
            "A 256-byte role is rejected by the CRD maximum that mirrors K-ORC's\n"
            "KeystoneName cap. The error carries 'Too long'."
        ),
        name="kra-invalid-role-too-long",
        role="  role: " + "r" * 256 + "\n",
    ),
    Fixture(
        filename="03-role-missing.yaml",
        comment="An order without a role is rejected by the CRD required list. The error\ncarries 'Required value'.",
        name="kra-invalid-role-missing",
        role="",
    ),
    Fixture(
        filename="04-userref-missing.yaml",
        comment=(
            "An order without a userRef is rejected by the CRD required list. The error\n"
            "carries 'Required value'."
        ),
        name="kra-invalid-userref-missing",
        user_ref="",
    ),
    Fixture(
        filename="05-projectref-missing.yaml",
        comment=(
            "An order without a projectRef is rejected by the CRD required list. The\n"
            "error carries 'Required value'."
        ),
        name="kra-invalid-projectref-missing",
        project_ref="",
    ),
    Fixture(
        filename="06-userref-name-empty.yaml",
        comment=(
            "An empty userRef.name is rejected by the CRD minimum: a reference names an\n"
            "order. The error carries 'should be at least 1 chars'."
        ),
        name="kra-invalid-userref-name-empty",
        user_ref='  userRef:\n    name: ""\n',
    ),
    Fixture(
        filename="07-name-too-long.yaml",
        comment=(
            "A 64-byte metadata.name is rejected by the root CEL rule: the name is\n"
            "carried as a label value on the order's children, which Kubernetes caps\n"
            "at 63 bytes. The error carries 'metadata.name must be at most 63 bytes'."
        ),
        name="kra-" + "x" * 60,
    ),
    Fixture(
        filename="08-controlplaneref-name-missing.yaml",
        comment=(
            "A controlPlaneRef without a name is rejected by the required list of the\n"
            "CRD schema. No defaulting webhook runs for the kind, so nothing fills the\n"
            "field in before validation. The error carries 'Required value'."
        ),
        name="kra-invalid-ref-name-missing",
        controlplane_name="",
        ref_extra="    namespace: cp-namespace\n",
    ),
    Fixture(
        filename="09-controlplaneref-namespace-invalid.yaml",
        comment=(
            "controlPlaneRef.namespace 'Bad_NS' violates the RFC-1123 label pattern\n"
            "(CRD marker). The error carries 'should match'."
        ),
        name="kra-invalid-ref-namespace",
        ref_extra="    namespace: Bad_NS\n",
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
            "transition rules evaluate at all."
        ),
        name="kra-immutable",
    ),
    Fixture(
        filename="11-immutable-controlplaneref-name.yaml",
        comment=(
            "controlPlaneRef.name is immutable (CEL transition rule): the edit would\n"
            "strand the assignment the order created on the old plane. The error\n"
            "carries 'controlPlaneRef.name is immutable'."
        ),
        name="kra-immutable",
        controlplane_name="cp-other",
    ),
    Fixture(
        filename="12-immutable-controlplaneref-namespace.yaml",
        comment=(
            "controlPlaneRef.namespace is immutable (CEL transition rule), also when\n"
            "the base left it to the order's own namespace. The error carries\n"
            "'controlPlaneRef.namespace is immutable'."
        ),
        name="kra-immutable",
        ref_extra="    namespace: elsewhere\n",
    ),
    Fixture(
        filename="13-immutable-userref.yaml",
        comment=(
            "userRef is immutable (CEL transition rule): the assignment binds the user\n"
            "it was created for. The error carries 'userRef is immutable'."
        ),
        name="kra-immutable",
        user_ref="  userRef:\n    name: another-user\n",
    ),
    Fixture(
        filename="14-immutable-projectref.yaml",
        comment=(
            "projectRef is immutable (CEL transition rule): the assignment binds the\n"
            "project it was created for. The error carries 'projectRef is immutable'."
        ),
        name="kra-immutable",
        project_ref="  projectRef:\n    name: another-project\n",
    ),
    Fixture(
        filename="15-immutable-role.yaml",
        comment=(
            "role is immutable (CEL transition rule): another role is another\n"
            "assignment. The error carries 'role is immutable'."
        ),
        name="kra-immutable",
        role="  role: reader\n",
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
        print("run `python3 tests/e2e/c5c3/invalid-keystoneroleassignment-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
