#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid NovaCompute Chainsaw fixtures.

Single source of truth for the minimal valid NovaCompute scaffold used by every
rejection test in this directory, mirroring
``tests/e2e/ovn/invalid-ovnchassis-cr/_generate.py``. Each fixture mutates
exactly one aspect of the canonical scaffold so the surrounding CR passes
validation for every rule OTHER than the one under test, which makes the
admission error attributable to that single field.

The scaffold names a Nova and a node selector, the two required fields on
NovaComputeSpec. The novaRef deliberately points at a Nova that does not exist
in the ephemeral namespace: admission tolerates the dangling reference (a pool
is commonly applied beside its Nova), and the extraConfig catalog check it
would feed is then skipped with a warning, so the reference never competes
with the rule a fixture pins.

Where the CRD schema and the validating webhook carry the same rule, the API
server answers with the schema message, because it validates the structural
schema and its CEL rules before it calls any validating webhook. Each fixture
comment names the layer that answers it, and the matching Chainsaw step
asserts that layer's message.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace.

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

# Canonical valid NovaCompute scaffold. Any future required field on
# NovaComputeSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {nova_ref} the whole spec.novaRef block,
# {node_selector} the whole spec.nodeSelector block, {extra} trailing spec
# additions.
SCAFFOLD = """\
apiVersion: nova.openstack.c5c3.io/v1alpha1
kind: NovaCompute
metadata:
  name: {name}
spec:
{nova_ref}
{node_selector}
{extra}"""

VALID_NAME = "pool"

VALID_NOVA_REF = """\
  novaRef:
    name: nova"""

VALID_NODE_SELECTOR = """\
  nodeSelector:
    openstack.c5c3.io/nova-compute-pool: a"""

# The metadata.name bound is the instance label every child carries.
MAX_NAME_LENGTH = 63


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str = VALID_NAME
    nova_ref: str = VALID_NOVA_REF
    node_selector: str = VALID_NODE_SELECTOR
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            nova_ref=self.nova_ref,
            node_selector=self.node_selector,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-novaref-name-empty.yaml",
        comment=(
            "spec.novaRef.name empty violates the MinLength=1 marker on NovaRef, which\n"
            "the API server answers before the webhook's field.Required twin runs. A\n"
            "pool that names no Nova has no contract to mount and no API to register\n"
            "its services with."
        ),
        nova_ref=(
            "  novaRef:\n"
            '    name: ""'
        ),
    ),
    Fixture(
        filename="01-nodeselector-empty.yaml",
        comment=(
            "spec.nodeSelector empty violates the MinProperties=1 marker. An empty\n"
            "selector matches every node rather than none, so it would start\n"
            "nova-compute on the control-plane nodes too. The webhook repeats the\n"
            "check, but the schema answers first."
        ),
        node_selector="  nodeSelector: {}",
    ),
    Fixture(
        filename="02-name-too-long.yaml",
        comment=(
            "A metadata.name of 64 characters is rejected by the validating webhook\n"
            "alone. The name is the app.kubernetes.io/instance label of every child,\n"
            "and Kubernetes caps a label value at 63 characters. The name is a valid\n"
            "DNS-1123 subdomain apart from its length, so the bound is the only rule it\n"
            "breaks."
        ),
        name="nova-compute-pool-with-an-invalid-name-past-the-63-char-boundary",
    ),
    Fixture(
        filename="03-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the DaemonSet. The\n"
            "webhook repeats it via validation.TargetClusterRef, but the schema answers\n"
            "first."
        ),
        extra=(
            "  targetClusterRef:\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="04-toleration-offboarding.yaml",
        comment=(
            "A toleration of kvm.cloud.sap/offboarding:NoExecute without\n"
            "tolerationSeconds is rejected by the validating webhook alone.\n"
            "openstack-hypervisor-operator deletes a node's compute service only once\n"
            "every agent pod on the node that tolerates the taint indefinitely is gone,\n"
            "and a nova-compute that stays registers the service again."
        ),
        extra=(
            "  tolerations:\n"
            "  - key: kvm.cloud.sap/offboarding\n"
            "    operator: Exists\n"
            "    effect: NoExecute\n"
        ),
    ),
    Fixture(
        filename="05-toleration-wildcard.yaml",
        comment=(
            "A toleration with operator Exists and no key tolerates every taint, the\n"
            "offboarding one included, and is rejected by the validating webhook under\n"
            "the same message. The webhook matches the taint the way\n"
            "openstack-hypervisor-operator does, so the wildcard cannot walk past it."
        ),
        extra=(
            "  tolerations:\n"
            "  - operator: Exists\n"
        ),
    ),
    Fixture(
        filename="06-cpumodels-without-custom.yaml",
        comment=(
            "spec.libvirt.cpuModels with cpuMode host-model violates the CEL rule on\n"
            "NovaComputeLibvirtSpec: libvirt reads cpu_models only for the custom mode,\n"
            "so the list would be rendered and ignored."
        ),
        extra=(
            "  libvirt:\n"
            "    cpuMode: host-model\n"
            "    cpuModels:\n"
            "    - Haswell-noTSX\n"
        ),
    ),
    Fixture(
        filename="07-custom-without-cpumodels.yaml",
        comment=(
            "spec.libvirt.cpuMode custom without cpuModels violates the same CEL rule:\n"
            "nova-compute refuses to start on a custom mode that names no CPU model."
        ),
        extra=(
            "  libvirt:\n"
            "    cpuMode: custom\n"
        ),
    ),
    Fixture(
        filename="08-virttype-invalid.yaml",
        comment=(
            "spec.libvirt.virtType outside the CRD enum (kvm, qemu). The schema enum is\n"
            "the sole gate."
        ),
        extra=(
            "  libvirt:\n"
            "    virtType: xen\n"
        ),
    ),
    Fixture(
        filename="09-imagestype-rbd.yaml",
        comment=(
            "spec.libvirt.imagesType rbd is outside the CRD enum (default, qcow2, raw,\n"
            "flat): the nova-compute image carries no Ceph client and no pod mounts a\n"
            "ceph.conf, so an rbd backend could not start."
        ),
        extra=(
            "  libvirt:\n"
            "    imagesType: rbd\n"
        ),
    ),
    Fixture(
        filename="10-extraconfig-rejected-host.yaml",
        comment=(
            "spec.extraConfig [DEFAULT] host is rejected by the validating webhook\n"
            "alone: extraConfig is a free-form map the schema cannot constrain. The host\n"
            "is the node name, from the downward API, and the aggregates, the drain and\n"
            "openstack-hypervisor-operator all find the service by it."
        ),
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            "      host: compute-1\n"
        ),
    ),
    Fixture(
        filename="11-maxunavailable-with-ondelete.yaml",
        comment=(
            "spec.updateStrategy.maxUnavailable paired with OnDelete is rejected by the\n"
            "validating webhook alone: the field is an int-or-string with no marker to\n"
            "correlate it with the type, and under OnDelete nothing reads it."
        ),
        extra=(
            "  updateStrategy:\n"
            "    type: OnDelete\n"
            "    maxUnavailable: 1\n"
        ),
    ),
    Fixture(
        filename="12-image-tag-and-digest.yaml",
        comment=(
            "spec.image with both a tag and a digest violates the CEL rule on the shared\n"
            "ImageSpec: exactly one of the two pins the image."
        ),
        extra=(
            "  image:\n"
            "    repository: ghcr.io/c5c3/nova-compute\n"
            "    tag: \"2025.2\"\n"
            "    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000\n"
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
        print("run `python3 tests/e2e/nova/invalid-novacompute-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
