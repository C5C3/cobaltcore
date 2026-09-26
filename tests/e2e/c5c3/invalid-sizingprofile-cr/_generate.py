#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the SizingProfile invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid SizingProfile scaffold used by
every rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-keystoneservice-cr/_generate.py``. Each fixture
mutates exactly one aspect of the canonical scaffold, so the surrounding CR
passes every rule OTHER than the one under test and the admission error is
attributable to that rule.

A SizingProfile is cluster-scoped, so the fixtures carry no
metadata.namespace at all. Every fixture is rejected at admission, so none
persists and no name can collide with a parallel run or wedge cleanup.

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

# Canonical SizingProfile scaffold. Placeholders:
#   {name}  metadata.name
#   {base}  the spec.base value
#   {spec}  further spec fields (indent 2) or ""
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: SizingProfile
metadata:
  name: {name}
spec:
  base: {base}
{spec}"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    base: str = "Minimal"
    spec: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(name=self.name, base=self.base, spec=self.spec)
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-base-unknown.yaml",
        comment=(
            "spec.base names no built-in profile. Rejected by the Enum marker on\n"
            "SizingProfileName before the webhook runs; the error carries\n"
            "`Unsupported value`."
        ),
        name="c5c3-e2e-sizing-base-unknown",
        base="Large",
    ),
    Fixture(
        filename="01-request-above-limit.yaml",
        comment=(
            "A memory request above its limit (webhook-only): the schema cannot compare two\n"
            "quantities, and every projected pod would be refused. The error names\n"
            "`spec.messaging.resources.requests.memory`."
        ),
        name="c5c3-e2e-sizing-request-above-limit",
        spec=(
            "  messaging:\n"
            "    resources:\n"
            "      requests:\n"
            "        memory: 2Gi\n"
            "      limits:\n"
            "        memory: 1Gi\n"
        ),
    ),
    Fixture(
        filename="02-node-selector-bad-key.yaml",
        comment=(
            "A nodeSelector key with a space is not a label key (webhook-only): the schema\n"
            "cannot check the keys of a map. The error names `spec.nodeSelector` and the key."
        ),
        name="c5c3-e2e-sizing-bad-selector",
        spec=(
            "  nodeSelector:\n"
            '    "bad key": control\n'
        ),
    ),
    Fixture(
        filename="03-database-replicas-two.yaml",
        comment=(
            "database.replicas: 2 (webhook-only): two Galera nodes cannot hold a majority.\n"
            "The error names `spec.database.replicas` and carries `2 cannot hold a majority`."
        ),
        name="c5c3-e2e-sizing-db-two",
        spec=(
            "  database:\n"
            "    replicas: 2\n"
        ),
    ),
    Fixture(
        filename="04-cache-memory-limit-too-low.yaml",
        comment=(
            "cache.resources.limits.memory: 64Mi (webhook-only): the Memcached operator\n"
            "requires maxMemoryMB (64) plus 32Mi. The error carries `memory limit must be at\n"
            "least 96Mi`."
        ),
        name="c5c3-e2e-sizing-cache-64mi",
        spec=(
            "  cache:\n"
            "    resources:\n"
            "      limits:\n"
            "        memory: 64Mi\n"
        ),
    ),
    Fixture(
        filename="05-spread-max-skew-zero.yaml",
        comment=(
            "A spread entry with maxSkew: 0 allows no skew at all. Rejected by the Minimum\n"
            "marker on SpreadConstraintSpec.MaxSkew before the webhook runs; the error names\n"
            "`maxSkew` and carries `should be greater than or equal to 1`."
        ),
        name="c5c3-e2e-sizing-max-skew-zero",
        spec=(
            "  glance:\n"
            "    api:\n"
            "      spreadConstraints:\n"
            "      - maxSkew: 0\n"
            "        topologyKey: kubernetes.io/hostname\n"
            "        whenUnsatisfiable: ScheduleAnyway\n"
        ),
    ),
    Fixture(
        filename="06-priority-class-missing.yaml",
        comment=(
            "priorityClassName names no PriorityClass (webhook-only): every projected pod\n"
            "would be refused at admission. The error names `spec.priorityClassName` and\n"
            "carries `Not found`."
        ),
        name="c5c3-e2e-sizing-missing-class",
        spec="  priorityClassName: c5c3-e2e-absent-priority-class\n",
    ),
    Fixture(
        filename="07-autoscaling-behavior-period-above-max.yaml",
        comment=(
            "An autoscaling scale-down policy with periodSeconds: 1801 (webhook-only):\n"
            "autoscaling/v2 caps a policy period at 1800 seconds, and the embedded type\n"
            "carries no bounds, so the schema admits it. The error names\n"
            "`spec.keystone.api.autoscaling.behavior.scaleDown.policies[0].periodSeconds`\n"
            "and carries `periodSeconds must be between 1 and 1800`."
        ),
        name="c5c3-e2e-sizing-behavior-period",
        spec=(
            "  keystone:\n"
            "    api:\n"
            "      autoscaling:\n"
            "        maxReplicas: 3\n"
            "        targetCPUUtilization: 80\n"
            "        behavior:\n"
            "          scaleDown:\n"
            "            policies:\n"
            "            - type: Pods\n"
            "              value: 1\n"
            "              periodSeconds: 1801\n"
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
        print("run `python3 tests/e2e/c5c3/invalid-sizingprofile-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
