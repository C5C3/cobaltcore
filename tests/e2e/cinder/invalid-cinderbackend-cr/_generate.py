#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid CinderBackend Chainsaw fixtures.

Single source of truth for the minimal valid CinderBackend scaffold used by
every rejection test in this directory, modeled on
``tests/e2e/glance/invalid-glancebackend-cr/_generate.py``. Each fixture mutates
exactly one aspect of the canonical scaffold so the surrounding CR passes
validation for every rule OTHER than the one under test, which makes the
admission error attributable to that single rule.

Two fixture categories share this scaffold:

* Create-rejection fixtures are each applied once and rejected at admission by
  a CEL XValidation rule, a kubebuilder marker, or webhook.validate().
* The cinderRef immutability pair: ``05-immutable-cinderref-base`` is the valid
  base CR applied first, and ``06-immutable-cinderref`` reuses its name so it is
  applied as an UPDATE that re-points spec.cinderRef.name, which the CRD CEL
  transition rule (self == oldSelf, evaluated only on UPDATE) rejects. A
  type-immutability update fixture is deliberately absent: NFS is the only
  Phase-1 enum value, so a changed type is already rejected by the Enum marker.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, which isolates them from the parallel cinder
suites pinned to the shared ``openstack`` namespace and lets the accepted base
(which persists) get torn down with the namespace. The base references a Cinder
that does not exist there: admission tolerates the dangling reference (GitOps
ordering), and the service-remove finalizer the backend controller holds is
released unconditionally when the parent Cinder is gone, so namespace cleanup
never wedges.

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

# Canonical valid CinderBackend scaffold. Any future required field on
# CinderBackendSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {cinder_ref} spec.cinderRef.name, {type}
# spec.type value, {nfs} the nfs block body (empty string to omit it), {extra}
# trailing spec additions (extraOptions, imageVolumeCache).
SCAFFOLD = """\
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackend
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
    path: /exports/cinder
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
        name="cinderbackend-no-nfs",
        nfs="",
    ),
    Fixture(
        filename="01-reserved-name-default.yaml",
        comment=(
            'metadata.name "default" names cinder.conf\'s [DEFAULT] section, so the\n'
            "backend's own options would be read as service-wide ones. The validating\n"
            "webhook rejects the exact reserved name; the schema has no counterpart,\n"
            "because the section names come from the embedded option catalogs."
        ),
        name="default",
    ),
    Fixture(
        filename="02-nfs-path-not-absolute.yaml",
        comment=(
            "spec.nfs.path without a leading slash violates the ^/ pattern on\n"
            "NFSBackendSpec.Path. The path reaches the mount command verbatim, where a\n"
            "relative one names nothing on the server. The schema is the only gate:\n"
            "the webhook carries no counterpart."
        ),
        name="cinderbackend-relative-path",
        nfs=(
            "  nfs:\n"
            "    server: nfs.example.com\n"
            "    path: exports/cinder\n"
        ),
    ),
    Fixture(
        filename="03-extraoptions-denylist.yaml",
        comment=(
            "spec.extraOptions carrying volume_driver duplicates an option the\n"
            "operator renders from spec.type; the validating webhook's denylist\n"
            "rejects it, because a duplicate would shadow the typed value depending on\n"
            "render order."
        ),
        name="cinderbackend-denylist",
        extra=(
            "  extraOptions:\n"
            '    volume_driver: "cinder.volume.drivers.lvm.LVMVolumeDriver"\n'
        ),
    ),
    Fixture(
        filename="04-extraoptions-bad-charset.yaml",
        comment=(
            'spec.extraOptions key "nfs-mount-attempts" carries dashes, which the\n'
            "validating webhook's key allowlist (^[A-Za-z0-9_]+$) rejects before the\n"
            "denylist runs: oslo.config option names are snake_case, and a variant the\n"
            "denylist is blind to would otherwise reach the rendered section."
        ),
        name="cinderbackend-bad-key",
        extra=(
            "  extraOptions:\n"
            '    nfs-mount-attempts: "3"\n'
        ),
    ),
    Fixture(
        filename="05-immutable-cinderref-base.yaml",
        comment=(
            "Valid base CinderBackend for the cinderRef-immutability pair. It is\n"
            "applied FIRST and must SUCCEED; 06-immutable-cinderref reuses this name\n"
            "so it is applied as an UPDATE. The referenced Cinder does not have to\n"
            "exist at admission time (GitOps ordering), so the base is admitted."
        ),
        name="cinderbackend-immutable",
        cinder_ref="cinder-immutable",
    ),
    Fixture(
        filename="06-immutable-cinderref.yaml",
        comment=(
            "Update of the cinderbackend-immutable base CR that re-points\n"
            "spec.cinderRef.name. The spec-level CEL transition rule\n"
            "(self.cinderRef.name == oldSelf.cinderRef.name) rejects the change on\n"
            "UPDATE: re-pointing would strand the volumes the old deployment created\n"
            "under a host identity nothing serves anymore."
        ),
        name="cinderbackend-immutable",
        cinder_ref="cinder-repointed",
    ),
    Fixture(
        filename="07-name-bound-exceeded.yaml",
        comment=(
            "metadata.name (13 characters) plus spec.cinderRef.name (35) exceeds the\n"
            "shared 47-character budget by one, which the validating webhook alone\n"
            "answers: detaching the backend runs a <cinder>-<name>-service-remove Job\n"
            "whose name is copied into a label value Kubernetes caps at 63 characters.\n"
            "The budget is shared rather than per-name because either name may be the\n"
            "long one, so the overrun is put on the Cinder name here: metadata.name\n"
            "stays inside its own 35-character bound, which 08 pins on its own. The\n"
            "rule runs in ValidateCreate only, so the finalizer-removal update never\n"
            "trips over it."
        ),
        name="cinderbackend",
        cinder_ref="cinder-name-past-the-shared-budgets",
    ),
    Fixture(
        filename="08-name-length-exceeded.yaml",
        comment=(
            "metadata.name of 36 characters exceeds its own 35-character bound by one,\n"
            "which the validating webhook alone answers: the detach records the Job's\n"
            "terminal state under the annotation key\n"
            "cobaltcore.c5c3.io/last-service-remove-<name>-job-uid on the Cinder, and\n"
            "Kubernetes caps an annotation key's name part at 63 characters. The pair\n"
            "clears the shared 47-character budget (36 + 6), so this fixture pins the\n"
            "per-name bound alone."
        ),
        name="cinderbackend-name-past-the-35-chars",
    ),
    Fixture(
        filename="09-nfs-path-newline.yaml",
        comment=(
            "spec.nfs.path carrying a newline violates the ^/[!-~]*$ pattern on\n"
            "NFSBackendSpec.Path, which the schema answers before the webhook's\n"
            "validateNFSExport twin. The operator writes \"server:path\" verbatim into\n"
            "the nfs_shares_config file the cinder-volume pod mounts, so a newline\n"
            "would add a second export line naming a share nothing validated, mounted\n"
            "or derived an egress rule for."
        ),
        name="cinderbackend-path-newline",
        nfs=(
            "  nfs:\n"
            "    server: nfs.example.com\n"
            '    path: "/exports/cinder\\nnfs.example.com:/exports/other"\n'
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
        print("run `python3 tests/e2e/cinder/invalid-cinderbackend-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
