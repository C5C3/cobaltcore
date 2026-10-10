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
* The immutability updates: ``04-immutable-cinderref-base`` is the valid base
  CR applied first, and ``05-immutable-cinderref`` and ``16-immutable-type``
  reuse its name so each is applied as an UPDATE, the first re-pointing
  spec.cinderRef.name and the second changing spec.type from NFS to RBD. The CRD
  CEL transition rules (self == oldSelf, evaluated only on UPDATE) reject both,
  so the base stays in place for the second.
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
# spec.type value, {nfs} the nfs block body and {rbd} the rbd block body (empty
# string to omit either), {extra} trailing spec additions (fileSize,
# compression, extraOptions).
SCAFFOLD = """\
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackupBackend
metadata:
  name: {name}
spec:
  cinderRef:
    name: {cinder_ref}
  type: {type}
{nfs}{rbd}{extra}"""

# Valid nfs block (required exactly when type is NFS). Carries its trailing
# newline so a fixture that appends {extra} stays well-formed.
VALID_NFS = """\
  nfs:
    server: nfs.example.com
    path: /exports/cinder-backup
"""

# Valid rbd block (required exactly when type is RBD). Carries its trailing
# newline for the same reason as VALID_NFS.
VALID_RBD = """\
  rbd:
    pool: backups
    user: cinder-backup
    monitors:
    - ceph-mon.openstack.svc.cluster.local
    networks:
    - 10.244.0.0/16
    keySecretRef:
      name: ceph-client-cinder-backup
"""


def _rbd_with(old: str, new: str) -> str:
    """Return VALID_RBD with one line changed, failing loudly on a miss.

    A replacement that matched nothing would render a valid block, and the
    fixture would be admitted instead of pinning the rule it names.
    """
    if old not in VALID_RBD:
        raise ValueError(f"{old!r} is not part of VALID_RBD")
    return VALID_RBD.replace(old, new)


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    cinder_ref: str = "cinder"
    backend_type: str = "NFS"
    nfs: str = VALID_NFS
    rbd: str = ""
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            cinder_ref=self.cinder_ref,
            type=self.backend_type,
            nfs=self.nfs,
            rbd=self.rbd,
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
            "AFTER 06-second-backup-base."
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
    Fixture(
        filename="09-type-rbd-without-rbd-block.yaml",
        comment=(
            "type RBD without a spec.rbd block violates the second half of the union\n"
            "CEL rule ((self.type == 'RBD') == has(self.rbd)); the webhook mirrors it\n"
            "with the same message, but the schema answers first."
        ),
        name="cinderbackupbackend-no-rbd",
        backend_type="RBD",
        nfs="",
    ),
    Fixture(
        filename="10-type-nfs-with-rbd-block.yaml",
        comment=(
            "type NFS carrying a spec.rbd block beside spec.nfs violates the union CEL\n"
            "rule: exactly one backup backend block, matching spec.type. The schema\n"
            "answers before the webhook twin."
        ),
        name="cinderbackupbackend-nfs-and-rbd",
        rbd=VALID_RBD,
    ),
    Fixture(
        filename="11-rbd-user-client-prefix.yaml",
        comment=(
            "spec.rbd.user with the client. prefix violates the field-level CEL rule\n"
            "on RBDBackupBackendSpec.User: the value is the cephx name the backup\n"
            "driver passes as --id and names the keyring section client.<user>, so the\n"
            "prefix would be doubled. The webhook twin carries the same message; the\n"
            "schema answers first."
        ),
        name="cinderbackupbackend-rbd-client-prefix",
        backend_type="RBD",
        nfs="",
        rbd=_rbd_with("    user: cinder-backup\n", "    user: client.cinder-backup\n"),
    ),
    Fixture(
        filename="12-rbd-monitors-empty.yaml",
        comment=(
            "An empty spec.rbd.monitors list violates its MinItems=1 marker: the\n"
            "backup driver would have no monitor to connect to. The schema is the\n"
            "only gate for the count."
        ),
        name="cinderbackupbackend-rbd-no-monitors",
        backend_type="RBD",
        nfs="",
        rbd=_rbd_with("    monitors:\n    - ceph-mon.openstack.svc.cluster.local\n", "    monitors: []\n"),
    ),
    Fixture(
        filename="13-rbd-network-not-cidr.yaml",
        comment=(
            "A spec.rbd.networks entry without a prefix length violates the IPv4 CIDR\n"
            "item pattern. Each entry becomes an ipBlock peer of the Ceph egress rule,\n"
            "where the API server would reject it on the Cinder's NetworkPolicy; the\n"
            "webhook twin also rejects host bits, which the pattern cannot see."
        ),
        name="cinderbackupbackend-rbd-bad-network",
        backend_type="RBD",
        nfs="",
        rbd=_rbd_with("    - 10.244.0.0/16\n", '    - "10.244.0.0"\n'),
    ),
    Fixture(
        filename="14-rbd-key-secret-name-invalid.yaml",
        comment=(
            "spec.rbd.keySecretRef.name that is no DNS-1123 subdomain violates the\n"
            "object-name pattern: no Secret can carry the name, so the gate would wait\n"
            "forever. The schema answers before the webhook twin."
        ),
        name="cinderbackupbackend-rbd-bad-secret",
        backend_type="RBD",
        nfs="",
        rbd=_rbd_with("      name: ceph-client-cinder-backup\n", "      name: Ceph_Key\n"),
    ),
    Fixture(
        filename="15-extraoptions-rbd-denylist.yaml",
        comment=(
            "spec.extraOptions carrying backup_ceph_pool on an RBD target is rejected\n"
            "by the validating webhook's per-type denylist: the operator renders the\n"
            "option from spec.rbd.pool. The schema has no counterpart; past the\n"
            "webhook, the renderer keeps the typed value instead."
        ),
        name="cinderbackupbackend-rbd-denylist",
        backend_type="RBD",
        nfs="",
        rbd=VALID_RBD,
        extra=(
            "  extraOptions:\n"
            '    backup_ceph_pool: "images"\n'
        ),
    ),
    Fixture(
        filename="16-immutable-type.yaml",
        comment=(
            "Update of the cinderbackupbackend-immutable base CR (04) that changes\n"
            "spec.type from NFS to RBD with a well-formed spec.rbd block. The\n"
            "spec-level CEL transition rule (self.type == oldSelf.type) rejects the\n"
            "change on UPDATE: a restore reads a backup with the driver that wrote it.\n"
            "It is applied after 05, whose rejected update leaves the base in place.\n"
            "The merge patch leaves the base's nfs block in place, so the merged object\n"
            "also trips the exactly-one rule. The API server evaluates every CEL rule\n"
            "and returns both messages, and the suite asserts on the transition one."
        ),
        name="cinderbackupbackend-immutable",
        cinder_ref="cinder-immutable",
        backend_type="RBD",
        nfs="",
        rbd=VALID_RBD,
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
