#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneProject invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneProject scaffold used by
every rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-keystoneuser-cr/_generate.py``. Each fixture mutates
one aspect of the canonical scaffold, so the surrounding CR passes every rule
other than the one under test and the admission error is attributable to that
rule.

The kind has no webhook: a target cluster, where an order may live, runs none.
Every rejection here is therefore the CRD schema's own (patterns, minimums and
CEL rules).

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``05``) are each applied once and rejected
  at admission.
* One immutability wave keyed by the metadata.name ``kp-immutable``. It opens
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

# Canonical KeystoneProject scaffold. Placeholders:
#   {name}          metadata.name
#   {ref_name}      the spec.controlPlaneRef.name line (indent 4) or ""
#   {ref_extra}     further spec.controlPlaneRef fields (indent 4) or ""
#   {project_name}  the spec.projectName line (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneProject
metadata:
  name: {name}
spec:
  controlPlaneRef:
{ref_name}{ref_extra}{project_name}"""


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or the immutability base)."""

    filename: str
    comment: str
    name: str
    controlplane_name: str = "cp-invalid-cr-absent"
    ref_extra: str = ""
    project_name: str = ""

    def render(self) -> str:
        ref_name = f"    name: {self.controlplane_name}\n" if self.controlplane_name else ""
        body = SCAFFOLD.format(
            name=self.name,
            ref_name=ref_name,
            ref_extra=self.ref_extra,
            project_name=self.project_name,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


# The immutability base's declared project name. The wave below changes one
# field per fixture.
_BASE_PROJECT_NAME = "  projectName: svc-project\n"

FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-projectname-comma.yaml",
        comment=(
            "projectName carrying a comma is rejected by the CRD pattern that mirrors\n"
            "K-ORC's KeystoneName: admitted, the name would only be rejected by the\n"
            "K-ORC CRD, which wedges the reconcile. The error carries 'should match'."
        ),
        name="kp-invalid-projectname-comma",
        project_name="  projectName: a,b\n",
    ),
    Fixture(
        filename="01-projectname-empty.yaml",
        comment=(
            "An empty projectName is rejected by the CRD minimum: the default is\n"
            "spelled by leaving the field out, which means metadata.name. The error\n"
            "carries 'should be at least 1 chars'."
        ),
        name="kp-invalid-projectname-empty",
        project_name='  projectName: ""\n',
    ),
    Fixture(
        filename="02-projectname-too-long.yaml",
        comment=(
            "A 256-byte projectName is rejected by the CRD maximum that mirrors K-ORC's\n"
            "KeystoneName cap. The error carries 'Too long'."
        ),
        name="kp-invalid-projectname-too-long",
        project_name="  projectName: " + "a" * 256 + "\n",
    ),
    Fixture(
        filename="03-name-too-long.yaml",
        comment=(
            "A 64-byte metadata.name is rejected by the root CEL rule: the name is\n"
            "carried as a label value on the order's children, which Kubernetes caps\n"
            "at 63 bytes. The error carries 'metadata.name must be at most 63 bytes'."
        ),
        name="kp-" + "x" * 61,
    ),
    Fixture(
        filename="04-controlplaneref-name-missing.yaml",
        comment=(
            "A controlPlaneRef without a name is rejected by the required list of the\n"
            "CRD schema. No defaulting webhook runs for the kind, so nothing fills the\n"
            "field in before validation. The error carries 'Required value'."
        ),
        name="kp-invalid-ref-name-missing",
        controlplane_name="",
        ref_extra="    namespace: cp-namespace\n",
    ),
    Fixture(
        filename="05-controlplaneref-namespace-invalid.yaml",
        comment=(
            "controlPlaneRef.namespace 'Bad_NS' violates the RFC-1123 label pattern\n"
            "(CRD marker). The error carries 'should match'."
        ),
        name="kp-invalid-ref-namespace",
        ref_extra="    namespace: Bad_NS\n",
    ),
    # --- immutability wave: 06 is applied first and admitted, 07-09 are applied
    #     as UPDATEs of it (Chainsaw applies a same-name fixture to a live object
    #     as an RFC 7386 merge patch) ---
    Fixture(
        filename="06-immutable-base.yaml",
        comment=(
            "Valid base for the immutability wave. It is applied FIRST and must\n"
            "SUCCEED. The 07-09 variants reuse this metadata.name, so Chainsaw applies\n"
            "each of them to the live object as an UPDATE, which is what makes the CEL\n"
            "transition rules evaluate at all. projectName is declared explicitly."
        ),
        name="kp-immutable",
        project_name=_BASE_PROJECT_NAME,
    ),
    Fixture(
        filename="07-immutable-controlplaneref-name.yaml",
        comment=(
            "controlPlaneRef.name is immutable (CEL transition rule): the edit would\n"
            "strand the Keystone project the order created on the old plane. The error\n"
            "carries 'controlPlaneRef.name is immutable'."
        ),
        name="kp-immutable",
        controlplane_name="cp-other",
        project_name=_BASE_PROJECT_NAME,
    ),
    Fixture(
        filename="08-immutable-controlplaneref-namespace.yaml",
        comment=(
            "controlPlaneRef.namespace is immutable (CEL transition rule), also when\n"
            "the base left it to the order's own namespace. The error carries\n"
            "'controlPlaneRef.namespace is immutable'."
        ),
        name="kp-immutable",
        ref_extra="    namespace: elsewhere\n",
        project_name=_BASE_PROJECT_NAME,
    ),
    Fixture(
        filename="09-immutable-projectname.yaml",
        comment=(
            "projectName is immutable (root CEL transition rule over the effective\n"
            "name): the name identifies a live Keystone project. The error carries\n"
            "'projectName is immutable'."
        ),
        name="kp-immutable",
        project_name="  projectName: svc-project-renamed\n",
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
        print("run `python3 tests/e2e/c5c3/invalid-keystoneproject-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
