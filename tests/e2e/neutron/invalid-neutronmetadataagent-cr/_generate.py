#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid NeutronMetadataAgent Chainsaw fixtures.

Single source of truth for the minimal valid NeutronMetadataAgent scaffold used
by every rejection test in this directory, mirroring
``tests/e2e/ovn/invalid-ovnchassis-cr/_generate.py``. Each fixture mutates
exactly one aspect of the canonical scaffold so the surrounding CR passes
validation for every rule OTHER than the one under test, which makes the
admission error attributable to that single field.

The scaffold names a release, an image and a chassis, the three required
properties of NeutronMetadataAgentSpec. The chassisRef deliberately points at an
OVNChassis that does not exist in the ephemeral namespace: admission tolerates
the dangling reference (GitOps ordering), so the reference never competes with
the rule a fixture pins.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Three rules
have no schema counterpart and are answered by the webhook: the two
spec.extraConfig checks (the map is preserve-unknown-fields, which CEL cannot
constrain) and the metadata.name bound, which comes from a label value. Each
fixture comment names the layer that answers it, and the matching Chainsaw step
asserts that layer's message.

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

# Canonical valid NeutronMetadataAgent scaffold. Any future required field on
# NeutronMetadataAgentSpec must be added below AND verified against every
# fixture.
# Placeholders: {name} CR name, {release} the openStackRelease value, {image}
# and {chassis_ref} the whole block each names, {extra} trailing spec additions.
SCAFFOLD = """\
apiVersion: neutron.openstack.c5c3.io/v1alpha1
kind: NeutronMetadataAgent
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
{image}
{chassis_ref}
{extra}"""

VALID_NAME = "neutron-metadata-agent"

VALID_RELEASE = "2025.2"

VALID_IMAGE = """\
  image:
    repository: ghcr.io/c5c3/neutron
    tag: "2025.2\""""

VALID_CHASSIS_REF = """\
  chassisRef:
    name: ovn-chassis"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str = VALID_NAME
    release: str = VALID_RELEASE
    image: str = VALID_IMAGE
    chassis_ref: str = VALID_CHASSIS_REF
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            image=self.image,
            chassis_ref=self.chassis_ref,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-chassisref-name-empty.yaml",
        comment=(
            "spec.chassisRef.name empty violates the MinLength=1 marker on\n"
            "OVNChassisRef, which the API server answers before the webhook's\n"
            "field.Required twin runs. The chassis is what puts the agent on a node\n"
            "and gives it the local OVS database to read, so an agent without one has\n"
            "nothing to attach to."
        ),
        chassis_ref=(
            "  chassisRef:\n"
            '    name: ""'
        ),
    ),
    Fixture(
        filename="01-image-tag-and-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL rule\n"
            "(has(self.tag) != has(self.digest)); validateImage mirrors it in the\n"
            "webhook with the same message, but the schema answers first."
        ),
        image=(
            "  image:\n"
            "    repository: ghcr.io/c5c3/neutron\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="02-image-repository-missing.yaml",
        comment=(
            "spec.image without a repository. What answers is the MinLength=1 marker,\n"
            "not the required-properties list: the defaulting webhook marshals the\n"
            "whole typed object back into its admission patch, and ImageSpec.Repository\n"
            "carries no omitempty tag, so the patch materializes the field as an empty\n"
            "string. By the time the schema is checked the property is present and the\n"
            "empty value fails the marker. The webhook repeats it as field.Required."
        ),
        image=(
            "  image:\n"
            '    tag: "2025.2"'
        ),
    ),
    Fixture(
        filename="03-messaging-clusterref-and-secretref.yaml",
        comment=(
            "spec.messaging with both clusterRef and secretRef violates the shared\n"
            "MessagingSpec XOR CEL rule: managed mode derives the transport URL from\n"
            "the RabbitmqCluster, brownfield mode reads it from the Secret, and naming\n"
            "both leaves no rule for which URL the agent gets. The block is optional on\n"
            "this kind, so the rule applies only once it is present."
        ),
        extra=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: openstack-rabbitmq\n"
            "    secretRef:\n"
            "      name: neutron-transport-url\n"
        ),
    ),
    Fixture(
        filename="04-novametadata-port-below-minimum.yaml",
        comment=(
            "spec.novaMetadata.port below the CRD Minimum=1 marker. The value is -1\n"
            "rather than 0 because the defaulting webhook fills a zero port with\n"
            "DefaultNovaMetadataPort before schema validation sees it, so a CR asking\n"
            "for port 0 is admitted and proxies to 8775. Only a negative port survives\n"
            "defaulting and reaches the marker."
        ),
        extra=(
            "  novaMetadata:\n"
            "    port: -1\n"
        ),
    ),
    Fixture(
        filename="05-novametadata-sharedsecretref-name-empty.yaml",
        comment=(
            "spec.novaMetadata.sharedSecretRef.name empty violates the MinLength=1\n"
            "marker on the shared SecretRefSpec, which answers before the webhook's\n"
            "field.Required twin. An unnamed Secret carries no shared secret to sign\n"
            "forwarded requests with, and Nova rejects an unsigned request when it is\n"
            "configured with a secret of its own."
        ),
        extra=(
            "  novaMetadata:\n"
            "    sharedSecretRef:\n"
            '      name: ""\n'
        ),
    ),
    Fixture(
        filename="06-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in a known section is rejected\n"
            "by the validating webhook against the embedded neutron 2025.2 option\n"
            "catalog. extraConfig is a preserve-unknown-fields map, so CEL cannot\n"
            "constrain its keys and admission is the only gate. [DEFAULT] is a section\n"
            "the catalog carries, so the rejection is the unknown-option one rather\n"
            "than the unknown-section one."
        ),
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            '      no_such_option: "1"\n'
        ),
    ),
    Fixture(
        filename="07-extraconfig-rejected-owned-key.yaml",
        comment=(
            "spec.extraConfig setting [ovs] ovsdb_connection is rejected by the\n"
            "validating webhook. The key is Rejected rather than merely owned because\n"
            "the agent reads the local OVS database over the socket the chassis pods\n"
            "share, and another address points it at a node whose ports it is not\n"
            "answering for."
        ),
        extra=(
            "  extraConfig:\n"
            "    ovs:\n"
            '      ovsdb_connection: "tcp:1.2.3.4:6640"\n'
        ),
    ),
    Fixture(
        filename="08-name-too-long.yaml",
        comment=(
            "A metadata.name of 64 characters is rejected by the validating webhook\n"
            "alone. The bound is 63: the name is the app.kubernetes.io/instance label\n"
            "value on every child, and Kubernetes caps a label value at 63 characters.\n"
            "The name is a valid DNS-1123 subdomain apart from its length, so the bound\n"
            "is the only rule it breaks. The rule runs in ValidateCreate only, so the\n"
            "finalizer-removal update never trips over it."
        ),
        name="neutron-metadata-agent-invalid-name-past-the-63-char-label-bound",
    ),
    Fixture(
        filename="09-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the agent DaemonSet.\n"
            "The webhook repeats it via validation.TargetClusterRef, but the schema\n"
            "answers first."
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
        print("run `python3 tests/e2e/neutron/invalid-neutronmetadataagent-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
