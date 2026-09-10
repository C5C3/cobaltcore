#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid CinderBackupBackend Chainsaw fixtures.

Single source of truth for the minimal valid CinderBackupBackend scaffold used
by every rejection test in this directory, modeled on
``tests/e2e/glance/invalid-glancebackend-cr/_generate.py``. Each fixture mutates
exactly one aspect of the canonical scaffold so the surrounding CR passes
validation for every rule OTHER than the one under test, which makes the
admission error attributable to that single rule.

Three fixture categories share this scaffold:

* Create-rejection fixtures are each applied once and rejected at admission by
  a CEL XValidation rule, a kubebuilder marker, or webhook.validate().
* The cinderRef immutability pair: ``04-immutable-cinderref-base`` is the valid
  base CR applied first, and ``05-immutable-cinderref`` reuses its name so it is
  applied as an UPDATE that re-points spec.cinderRef.name, which the CRD CEL
  transition rule (self == oldSelf, evaluated only on UPDATE) rejects. A
  type-immutability update fixture is deliberately absent: NFS is the only
  Phase-1 enum value, so a changed type is already rejected by the Enum marker.
* The single-attachment pair: ``06-second-backup-base`` is a valid backup
  backend applied first, and ``07-second-backup-backend`` attaches a second one
  to the same Cinder, which the validating webhook rejects. It is the
  create-rejection that needs an existing object.

Every fixture names its own spec.cinderRef so the two accepted bases never
compete for the single-attachment rule.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, which isolates the sibling List from the
parallel cinder suites pinned to the shared ``openstack`` namespace and lets the
two accepted bases (which persist) get torn down with the namespace. They
reference Cinders that do not exist there, which admission deliberately
tolerates (GitOps ordering).

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

# Canonical valid CinderBackupBackend scaffold. Any future required field on
# CinderBackupBackendSpec must be added below AND verified against every
# fixture.
# Placeholders: {name} CR name, {cinder_ref} spec.cinderRef.name, {type}
# spec.type value, {nfs} the nfs block body (empty string to omit it), {extra}
# trailing spec additions (fileSize, compression, extraOptions).
SCAFFOLD = """\
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackupBackend
metadata:
  name: {name}
spec:
  cinderRef:
    name: {cinder_ref}
  type: {type}
{nfs}{extra}"""

# Valid nfs block (required exactly when type is NFS). Carries its trailing
# newline so a fixture that appends {extra} stays well-formed.
VALID_NFS = """\
  nfs:
    server: nfs.example.com
    path: /exports/cinder-backup
"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    cinder_ref: str = "cinder"
    backend_type: str = "NFS"
    nfs: str = VALID_NFS
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            cinder_ref=self.cinder_ref,
            type=self.backend_type,
            nfs=self.nfs,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-type-nfs-without-nfs-block.yaml",
        comment=(
            "type NFS without a spec.nfs block violates the union CEL rule\n"
            "((self.type == 'NFS') == has(self.nfs)); the webhook mirrors it with the\n"
            "same message, but the schema answers first."
        ),
        name="cinderbackupbackend-no-nfs",
        nfs="",
    ),
    Fixture(
        filename="01-filesize-below-minimum.yaml",
        comment=(
            "spec.fileSize below the Minimum=1048576 marker is answered by the schema.\n"
            "A chunk smaller than a MiB turns a large volume into a backup of tens of\n"
            "thousands of objects, each with its own round trip. The value is a\n"
            "multiple of 32768 so the MultipleOf marker stays satisfied and the\n"
            "fixture isolates the floor. The webhook repeats the bound, but the schema\n"
            "answers first."
        ),
        name="cinderbackupbackend-small-chunk",
        extra="  fileSize: 32768\n",
    ),
    Fixture(
        filename="02-compression-not-in-enum.yaml",
        comment=(
            "spec.compression outside the CRD enum (none, zlib, bz2, zstd) is answered\n"
            "by the schema. The four values are cinder's own choices for [DEFAULT]\n"
            "backup_compression_algorithm, so a fifth fails every backup at runtime\n"
            "rather than at admission. The webhook repeats the enum, but the schema\n"
            "answers first."
        ),
        name="cinderbackupbackend-bad-compression",
        extra="  compression: lz4\n",
    ),
    Fixture(
        filename="03-extraoptions-denylist.yaml",
        comment=(
            "spec.extraOptions carrying backup_driver duplicates an option the\n"
            "operator renders from spec.type; the validating webhook's denylist\n"
            "rejects it, because a duplicate would shadow the typed value depending on\n"
            "render order."
        ),
        name="cinderbackupbackend-denylist",
        extra=(
            "  extraOptions:\n"
            '    backup_driver: "cinder.backup.drivers.swift.SwiftBackupDriver"\n'
        ),
    ),
    Fixture(
        filename="04-immutable-cinderref-base.yaml",
        comment=(
            "Valid base CinderBackupBackend for the cinderRef-immutability pair. It is\n"
            "applied FIRST and must SUCCEED; 05-immutable-cinderref reuses this name\n"
            "so it is applied as an UPDATE. The referenced Cinder does not have to\n"
            "exist at admission time (GitOps ordering), so the base is admitted."
        ),
        name="cinderbackupbackend-immutable",
        cinder_ref="cinder-immutable",
    ),
    Fixture(
        filename="05-immutable-cinderref.yaml",
        comment=(
            "Update of the cinderbackupbackend-immutable base CR that re-points\n"
            "spec.cinderRef.name. The spec-level CEL transition rule\n"
            "(self.cinderRef.name == oldSelf.cinderRef.name) rejects the change on\n"
            "UPDATE: re-pointing would leave the backups it already wrote recorded\n"
            "against a deployment that cannot read them."
        ),
        name="cinderbackupbackend-immutable",
        cinder_ref="cinder-repointed",
    ),
    Fixture(
        filename="06-second-backup-base.yaml",
        comment=(
            "Valid base CinderBackupBackend for the single-attachment pair. It is\n"
            "applied FIRST and must SUCCEED; 07-second-backup-backend attaches a\n"
            "second one to the same Cinder, which the webhook rejects. Its cinderRef\n"
            "differs from every other fixture's so the two accepted bases never\n"
            "compete for this rule."
        ),
        name="cinderbackupbackend-a",
        cinder_ref="cinder-single",
    ),
    Fixture(
        filename="07-second-backup-backend.yaml",
        comment=(
            "Second CinderBackupBackend attached to the same Cinder as\n"
            "06-second-backup-base. The backup driver is a property of the single\n"
            "cinder-backup Deployment rather than one of several backends it serves,\n"
            "so the validating webhook's sibling List rejects the newcomer. Applied\n"
            "AFTER 06-second-backup-base. This substitutes for the untestable\n"
            "type-immutability update (NFS is the only enum value)."
        ),
        name="cinderbackupbackend-b",
        cinder_ref="cinder-single",
    ),
    Fixture(
        filename="08-nfs-path-newline.yaml",
        comment=(
            "spec.nfs.path carrying a newline violates the ^/[!-~]*$ pattern on\n"
            "NFSBackupBackendSpec.Path, which the schema answers before the webhook's\n"
            "validateNFSExport twin. The value is rendered verbatim as the\n"
            "backup_share option, where the reconcile-time control-character guard\n"
            "would delete the backup Deployment outright rather than reject the edit."
        ),
        name="cinderbackupbackend-path-newline",
        nfs=(
            "  nfs:\n"
            "    server: nfs.example.com\n"
            '    path: "/exports/cinder-backup\\nbackup_driver = swift"\n'
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
        print("run `python3 tests/e2e/cinder/invalid-cinderbackupbackend-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
