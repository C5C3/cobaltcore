#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid OVNChassis Chainsaw fixtures.

Single source of truth for the minimal valid OVNChassis scaffold used by every
rejection test in this directory, mirroring
``tests/e2e/ovn/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

The scaffold names a central and a node selector, the two required fields on
OVNChassisSpec. The centralRef deliberately points at an OVNCentral that does
not exist in the ephemeral namespace: admission tolerates the dangling
reference (GitOps ordering), so the reference never competes with the rule a
fixture pins.

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

# Canonical valid OVNChassis scaffold. Any future required field on
# OVNChassisSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {central_ref} the whole spec.centralRef block,
# {node_selector} the whole spec.nodeSelector block, {extra} trailing spec
# additions.
SCAFFOLD = """\
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNChassis
metadata:
  name: {name}
spec:
{central_ref}
{node_selector}
{extra}"""

VALID_NAME = "chassis"

VALID_CENTRAL_REF = """\
  centralRef:
    name: ovn"""

VALID_NODE_SELECTOR = """\
  nodeSelector:
    openstack.c5c3.io/chassis: "true\""""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str = VALID_NAME
    central_ref: str = VALID_CENTRAL_REF
    node_selector: str = VALID_NODE_SELECTOR
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            central_ref=self.central_ref,
            node_selector=self.node_selector,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-centralref-name-empty.yaml",
        comment=(
            "spec.centralRef.name empty violates the MinLength=1 marker on\n"
            "OVNCentralRef, which the API server answers before the webhook's\n"
            "field.Required twin runs. A chassis with no control plane to register\n"
            "with has no Southbound database to read its flows from."
        ),
        central_ref=(
            "  centralRef:\n"
            '    name: ""'
        ),
    ),
    Fixture(
        filename="01-nodeselector-empty.yaml",
        comment=(
            "spec.nodeSelector empty violates the MinProperties=1 marker. An empty\n"
            "selector matches every node rather than none, so it would start\n"
            "ovn-controller on the control-plane nodes and on whatever else joins the\n"
            "cluster later. The webhook repeats the check as field.Required, but the\n"
            "schema answers first."
        ),
        node_selector="  nodeSelector: {}",
    ),
    Fixture(
        filename="02-gateway-nodeselector-empty.yaml",
        comment=(
            "spec.gateway.nodeSelector empty violates the MinProperties=1 marker on\n"
            "OVNGatewaySpec. The gateway selector narrows the set spec.nodeSelector\n"
            "already matched, so an empty one promotes every selected node to a\n"
            "gateway and spreads the external connectivity onto nodes with no uplink\n"
            "to carry it."
        ),
        extra=(
            "  gateway:\n"
            "    nodeSelector: {}\n"
        ),
    ),
    Fixture(
        filename="03-bridgemappings-physicalnetwork-pattern.yaml",
        comment=(
            "A spec.bridgeMappings physicalNetwork outside the CRD pattern\n"
            "(^[a-z0-9]([-a-z0-9]*[a-z0-9])?$). The grammar is the DNS-1123 label a\n"
            "Neutron network name is bounded to, so an underscore and a capital both\n"
            "name a segment Neutron cannot have."
        ),
        extra=(
            "  bridgeMappings:\n"
            "  - physicalNetwork: Phys_Net\n"
            "    bridge: br-ex\n"
        ),
    ),
    Fixture(
        filename="04-bridgemappings-bridge-pattern.yaml",
        comment=(
            "A spec.bridgeMappings bridge of 16 characters violates the CRD pattern\n"
            "(^[a-zA-Z0-9_.-]{1,15}$). The bridge appears as a Linux interface and the\n"
            "kernel's IFNAMSIZ leaves 15 usable bytes, so a longer name is one Open\n"
            "vSwitch could not create."
        ),
        extra=(
            "  bridgeMappings:\n"
            "  - physicalNetwork: physnet1\n"
            "    bridge: br-sixteen-chars\n"
        ),
    ),
    Fixture(
        filename="05-bridgemappings-duplicate-bridge.yaml",
        comment=(
            "Two spec.bridgeMappings entries sharing one bridge are rejected by the\n"
            "validating webhook alone. The +listMapKey=physicalNetwork already makes\n"
            "the network side unique at the schema layer, so a repeated network never\n"
            "reaches the webhook; the bridge side has no schema counterpart, and a\n"
            "repeated bridge renders an ovn-bridge-mappings string whose second entry\n"
            "silently shadows the first."
        ),
        extra=(
            "  bridgeMappings:\n"
            "  - physicalNetwork: physnet1\n"
            "    bridge: br-ex\n"
            "  - physicalNetwork: physnet2\n"
            "    bridge: br-ex\n"
        ),
    ),
    Fixture(
        filename="06-encaptype-invalid.yaml",
        comment=(
            "spec.encapType outside the CRD enum (geneve, vxlan). GRE carries no\n"
            "logical metadata OVN can use between chassis, so the schema enum is the\n"
            "sole gate and the webhook carries no twin."
        ),
        extra="  encapType: gre\n",
    ),
    Fixture(
        filename="07-updatestrategy-maxunavailable-with-ondelete.yaml",
        comment=(
            "spec.updateStrategy.maxUnavailable paired with OnDelete is rejected by the\n"
            "validating webhook alone: the field is an int-or-string with no marker to\n"
            "correlate it with the type. Under OnDelete nothing reads it, so leaving it\n"
            "admitted would let it read as an effective rollout pace while the pace is\n"
            "really set by whoever deletes the pods."
        ),
        extra=(
            "  updateStrategy:\n"
            "    type: OnDelete\n"
            "    maxUnavailable: 1\n"
        ),
    ),
    Fixture(
        filename="08-updatestrategy-maxunavailable-zero.yaml",
        comment=(
            "spec.updateStrategy.maxUnavailable of 0 under RollingUpdate is rejected by\n"
            "the validating webhook alone, for the same reason: an int-or-string field\n"
            "carries no Minimum marker. Zero stalls the rollout instead of pacing it,\n"
            "leaving the DaemonSet on the old image with nothing reporting why. The\n"
            "type is spelled out so the fixture reads as the RollingUpdate case rather\n"
            "than relying on the schema default."
        ),
        extra=(
            "  updateStrategy:\n"
            "    type: RollingUpdate\n"
            "    maxUnavailable: 0\n"
        ),
    ),
    Fixture(
        filename="09-remoteprobeinterval-negative.yaml",
        comment=(
            "spec.remoteProbeIntervalMs below the CRD Minimum=0 marker. Zero already\n"
            "means no probe at all, so a negative value names no behaviour\n"
            "ovn-controller has."
        ),
        extra="  remoteProbeIntervalMs: -1\n",
    ),
    Fixture(
        filename="10-name-too-long.yaml",
        comment=(
            "A metadata.name of 43 characters is rejected by the validating webhook\n"
            "alone. The bound is 42: the per-node chassis-deletion Job is named\n"
            "{name}-chassis-del-{8 hex}, 21 characters on top of the CR name, against\n"
            "the 63-character cap Kubernetes puts on an object name. The name is a\n"
            "valid DNS-1123 subdomain apart from its length, so the bound is the only\n"
            "rule it breaks."
        ),
        name="ovn-chassis-invalid-name-past-42-char-bound",
    ),
    Fixture(
        filename="11-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the DaemonSets. The\n"
            "webhook repeats it via validation.TargetClusterRef, but the schema answers\n"
            "first."
        ),
        extra=(
            "  targetClusterRef:\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="12-updatestrategy-maxunavailable-zero-percent.yaml",
        comment=(
            "spec.updateStrategy.maxUnavailable of \"0%\" under RollingUpdate wedges the\n"
            "rollout exactly the way the integer 0 does, and an int-or-string field\n"
            "carries no marker that would catch either. The webhook resolves the value\n"
            "before it judges it, so the percentage form cannot walk past the bound the\n"
            "integer form is held to."
        ),
        extra=(
            "  updateStrategy:\n"
            "    type: RollingUpdate\n"
            '    maxUnavailable: "0%"\n'
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
        print("run `python3 tests/e2e/ovn/invalid-ovnchassis-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
