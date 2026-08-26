#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the OVNCentral invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid OVNCentral scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/placement/invalid-cr/_generate.py``. Each fixture mutates exactly
one aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

The scaffold names an issuer and nothing else. spec.tls is the only required
field on OVNCentralSpec; every other value the two databases, northd, and the
backup run with is either a schema default or resolved by the operator at
reconcile time.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Each fixture
comment names the layer that answers it, and the matching Chainsaw step asserts
that layer's message.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, so the create-rejection fixtures never depend on
the shared ``openstack`` namespace existing.

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

# Canonical valid OVNCentral scaffold. Any future required field on
# OVNCentralSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {tls} the whole spec.tls block, {extra}
# trailing spec additions.
SCAFFOLD = """\
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNCentral
metadata:
  name: {name}
spec:
{tls}
{extra}"""

VALID_NAME = "ovn"

VALID_TLS = """\
  tls:
    issuerRef:
      name: openstack-ovn-ca"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str = VALID_NAME
    tls: str = VALID_TLS
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(name=self.name, tls=self.tls, extra=self.extra)
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-northbound-replicas-even.yaml",
        comment=(
            "spec.northbound.replicas set to an even number violates the CEL rule\n"
            "(self % 2 == 1) on OVNDatabaseSpec.Replicas, a schema-level rejection the\n"
            "API server answers before the validating webhook runs. An even Raft cluster\n"
            "tolerates no more failures than the odd one below it and has two ways to\n"
            "split the vote."
        ),
        extra=(
            "  northbound:\n"
            "    replicas: 2\n"
        ),
    ),
    Fixture(
        filename="01-northbound-replicas-above-maximum.yaml",
        comment=(
            "spec.northbound.replicas above the CRD Maximum=5 marker. The value is odd,\n"
            "so the odd-count CEL rule stays satisfied and the marker is the only rule\n"
            "this fixture breaks. Past five members the write latency of the extra Raft\n"
            "round trips outweighs the added fault tolerance."
        ),
        extra=(
            "  northbound:\n"
            "    replicas: 7\n"
        ),
    ),
    Fixture(
        filename="02-southbound-nodeportbase-below-range.yaml",
        comment=(
            "spec.southbound.nodePortBase below the CRD Minimum=30000 marker. The value\n"
            "sits one port under the start of the Kubernetes default node-port range, so\n"
            "the per-member Services the operator projects could not be created."
        ),
        extra=(
            "  southbound:\n"
            "    nodePortBase: 29999\n"
        ),
    ),
    Fixture(
        filename="03-northbound-nodeportbase-no-room.yaml",
        comment=(
            "spec.northbound.nodePortBase whose range runs past the top of the node-port\n"
            "range is rejected by the validating webhook alone. The rule correlates the\n"
            "base with the replica count, which no marker can express: 32766 clears the\n"
            "Maximum=32767 marker on its own, and only the third member's port falls\n"
            "outside. replicas is spelled out so the count in the error message comes\n"
            "from the fixture rather than from the schema default."
        ),
        extra=(
            "  northbound:\n"
            "    replicas: 3\n"
            "    nodePortBase: 32766\n"
        ),
    ),
    Fixture(
        filename="04-nodeport-ranges-overlap.yaml",
        comment=(
            "Two node-port ranges that overlap are rejected by the validating webhook\n"
            "alone. The northbound base is moved onto the southbound default (30651), so\n"
            "both databases claim 30651 through 30653 and their Services collide. The\n"
            "webhook reports the collision at spec.southbound.nodePortBase, the range the\n"
            "moved base ran into, not at the field this fixture changes."
        ),
        extra=(
            "  northbound:\n"
            "    nodePortBase: 30651\n"
        ),
    ),
    Fixture(
        filename="05-northbound-electiontimer-below-minimum.yaml",
        comment=(
            "spec.northbound.electionTimerMs below the CRD Minimum=1000 marker. A timer\n"
            "under a second re-elects on every hiccup of the link between the Raft\n"
            "members, and every election is a write outage."
        ),
        extra=(
            "  northbound:\n"
            "    electionTimerMs: 999\n"
        ),
    ),
    Fixture(
        filename="06-southbound-inactivityprobe-negative.yaml",
        comment=(
            "spec.southbound.inactivityProbeMs below the CRD Minimum=0 marker. Zero\n"
            "already means no probe at all, so a negative value names no behaviour\n"
            "ovsdb-server has."
        ),
        extra=(
            "  southbound:\n"
            "    inactivityProbeMs: -1\n"
        ),
    ),
    Fixture(
        filename="07-northbound-storage-size-pattern.yaml",
        comment=(
            "spec.northbound.storage.size in decimal units violates the CRD pattern\n"
            "(^[0-9]+(Mi|Gi|Ti)$), which admits binary units only. On a volume this small\n"
            "the difference between 1G and 1Gi is large enough to matter, and accepting\n"
            "both spellings invites the confusion."
        ),
        extra=(
            "  northbound:\n"
            "    storage:\n"
            '      size: "1G"\n'
        ),
    ),
    Fixture(
        filename="08-northd-threads-above-maximum.yaml",
        comment=(
            "spec.northd.threads above the CRD Maximum=16 marker. Past a handful of\n"
            "threads the lock contention inside northd eats the gain from the extra\n"
            "parallelism, so the ceiling stays low."
        ),
        extra=(
            "  northd:\n"
            "    threads: 17\n"
        ),
    ),
    Fixture(
        filename="09-tls-issuerref-name-empty.yaml",
        comment=(
            "spec.tls.issuerRef.name empty violates the MinLength=1 marker on\n"
            "OVNIssuerRef, which the API server answers before the webhook's\n"
            "field.Required twin runs. Without an issuer there is no CA to authenticate\n"
            "the OVN connections against, and OVN's own RBAC is keyed on the certificate\n"
            "CN."
        ),
        tls=(
            "  tls:\n"
            "    issuerRef:\n"
            '      name: ""'
        ),
    ),
    Fixture(
        filename="10-tls-issuerref-kind-invalid.yaml",
        comment=(
            "spec.tls.issuerRef.kind outside the CRD enum (Issuer, ClusterIssuer). Those\n"
            "are the only two kinds cert-manager issues a Certificate from, so the schema\n"
            "enum is the sole gate and the webhook carries no twin."
        ),
        tls=(
            "  tls:\n"
            "    issuerRef:\n"
            "      name: openstack-ovn-ca\n"
            "      kind: Foo"
        ),
    ),
    Fixture(
        filename="11-backup-retentiondays-zero.yaml",
        comment=(
            "spec.backup.retentionDays below the CRD Minimum=1 marker. Zero would have\n"
            "the run delete the snapshot it just took. validateBackup repeats the floor\n"
            "in the webhook layer, but the schema answers first."
        ),
        extra=(
            "  backup:\n"
            "    retentionDays: 0\n"
        ),
    ),
    Fixture(
        filename="12-backup-schedule-invalid.yaml",
        comment=(
            "spec.backup.schedule that is not a cron expression is rejected by the\n"
            "validating webhook alone: the field carries no schema marker, and the\n"
            "webhook parses it with the same library the CronJob controller uses. Left\n"
            "admitted, that controller would refuse the projected CronJob with an error\n"
            "nothing surfaces back onto the OVNCentral."
        ),
        extra=(
            "  backup:\n"
            '    schedule: "not a cron"\n'
        ),
    ),
    Fixture(
        filename="13-backup-s3-endpoint-pattern.yaml",
        comment=(
            "A plaintext spec.backup.s3.endpoint violates the CRD pattern (^https://).\n"
            "The upload carries the access key beside a full snapshot of both\n"
            "databases, and SigV4 authenticates a request without encrypting it.\n"
            "bucket and credentialsSecretRef are spelled out because the schema\n"
            "requires them alongside endpoint; omitted, the required-properties check\n"
            "would answer instead of the pattern."
        ),
        extra=(
            "  backup:\n"
            "    s3:\n"
            "      bucket: ovn-backups\n"
            "      endpoint: http://minio.internal:9000\n"
            "      credentialsSecretRef:\n"
            "        name: ovn-backup-s3\n"
        ),
    ),
    Fixture(
        filename="14-name-too-long.yaml",
        comment=(
            "A metadata.name of 46 characters is rejected by the validating webhook\n"
            "alone. The bound is 45: the backup CronJob is named {name}-backup and the\n"
            "API server caps a CronJob name at 52 characters, so a longer CR name would\n"
            "be admitted and then fail every reconcile on a child the API server refuses.\n"
            "The name is a valid DNS-1123 subdomain apart from its length, so the bound\n"
            "is the only rule it breaks."
        ),
        name="ovn-invalid-name-with-forty-six-characters-set",
    ),
    Fixture(
        filename="15-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered cluster,\n"
            "so the operator would have nowhere to place the CR's children. The webhook\n"
            "repeats it via validation.TargetClusterRef, but the schema answers first."
        ),
        extra=(
            "  targetClusterRef:\n"
            '    name: ""\n'
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
        print("run `python3 tests/e2e/ovn/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
