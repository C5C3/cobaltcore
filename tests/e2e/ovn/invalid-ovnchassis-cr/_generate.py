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

# The parts of an otherwise valid spec.fabric block, one per field. A fabric
# fixture overrides the one part its rule reads and keeps the rest, so the block
# breaks no rule but the one under test. vrf, mtu and bfd are omitted by default
# and take their schema defaults (fabric, table 100, MTU 9000, BFD 300/300/3).
VALID_FABRIC_UPLINKS = """\
    uplinks:
    - interface: fab1
      bridge: br-fab1
    - interface: fab2
      bridge: br-fab2"""

VALID_FABRIC_NODE = """\
    - name: node-1
      asn: 4200000001
      loopbackIPv4: 192.0.2.1
      loopbackIPv6: "2001:db8::1\""""

# A second node row that collides with nothing in VALID_FABRIC_NODE.
SECOND_FABRIC_NODE = """\
    - name: node-2
      asn: 4200000002
      loopbackIPv4: 192.0.2.2
      loopbackIPv6: "2001:db8::2\""""

VALID_FABRIC_RANGES = """\
    announceableRanges:
    - 198.51.100.0/24
    - 2001:db8::/64"""

VALID_FABRIC_ADVERTISE = "    advertiseVRFIDs: [42]"

VALID_FABRIC_LEARN = "    learnVRFIDs: [10]"


def fabric(
    *,
    vrf: str = "",
    uplinks: str = VALID_FABRIC_UPLINKS,
    mtu: str = "",
    nodes: str = VALID_FABRIC_NODE,
    ranges: str = VALID_FABRIC_RANGES,
    advertise: str = VALID_FABRIC_ADVERTISE,
    learn: str = VALID_FABRIC_LEARN,
    bfd: str = "",
) -> str:
    """Render a spec.fabric block for the {extra} placeholder.

    Every keyword is one part of the block; an empty string leaves the part
    out, and ``nodes`` holds the list entries without the ``nodes:`` key.
    """
    parts = ["  fabric:", vrf, uplinks, mtu]
    if nodes:
        parts += ["    nodes:", nodes]
    parts += [ranges, advertise, learn, bfd]
    return "\n".join(part for part in parts if part) + "\n"


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
    Fixture(
        filename="13-jobs-priorityclass-not-found.yaml",
        comment=(
            "spec.jobs.priorityClassName naming a PriorityClass that does not exist is\n"
            "rejected by the validating webhook alone (validation.PriorityClassExists):\n"
            "the object is cluster-scoped, so no marker can express the reference. The\n"
            "webhook resolves it through the operator's uncached API reader, as the\n"
            "operator's ServiceAccount, so a missing get grant on\n"
            "scheduling.k8s.io/priorityclasses would surface as an Internal error\n"
            "instead of Not found: the step also pins the operator's RBAC."
        ),
        extra=(
            "  jobs:\n"
            "    priorityClassName: ovn-invalid-missing-priority-class\n"
        ),
    ),
    Fixture(
        filename="14-ovs-revalidatorthreads-zero.yaml",
        comment=(
            "spec.ovs.revalidatorThreads of 0 violates the Minimum=1 marker: ovs-vswitchd\n"
            "needs at least one revalidator to expire datapath flows. The schema answers\n"
            "before the validating webhook, which carries the same rule as its twin."
        ),
        extra=(
            "  ovs:\n"
            "    revalidatorThreads: 0\n"
        ),
    ),
    Fixture(
        filename="15-vertical-autoscaling-foreign-resource.yaml",
        comment=(
            "spec.verticalAutoscaling.minAllowed naming ephemeral-storage violates the CEL\n"
            "rule on VerticalAutoscalingSpec; the webhook mirrors it. The VPA the operator\n"
            "writes controls cpu and memory only."
        ),
        extra=(
            "  verticalAutoscaling:\n"
            "    updateMode: \"Off\"\n"
            "    minAllowed:\n"
            "      ephemeral-storage: 1Gi\n"
        ),
    ),
    Fixture(
        filename="16-vertical-autoscaling-min-above-max.yaml",
        comment=(
            "spec.verticalAutoscaling.minAllowed.cpu above maxAllowed.cpu is rejected by\n"
            "the validating webhook alone: CEL cannot compare quantities across two maps."
        ),
        extra=(
            "  verticalAutoscaling:\n"
            "    updateMode: \"Off\"\n"
            "    minAllowed:\n"
            "      cpu: \"2\"\n"
            "    maxAllowed:\n"
            "      cpu: \"1\"\n"
        ),
    ),
    Fixture(
        filename="17-vertical-autoscaling-update-mode.yaml",
        comment=(
            "spec.verticalAutoscaling.updateMode outside Off, Initial, Recreate,\n"
            "InPlaceOrRecreate and Auto violates the Enum marker; the webhook mirrors it."
        ),
        extra=(
            "  verticalAutoscaling:\n"
            "    updateMode: Sometimes\n"
        ),
    ),
    Fixture(
        filename="18-vertical-autoscaling-min-replicas-zero.yaml",
        comment=(
            "spec.verticalAutoscaling.minReplicas of zero violates the Minimum=1 marker;\n"
            "the webhook mirrors it."
        ),
        extra=(
            "  verticalAutoscaling:\n"
            "    updateMode: Auto\n"
            "    minReplicas: 0\n"
        ),
    ),
    Fixture(
        filename="19-fabric-uplinks-empty.yaml",
        comment=(
            "spec.fabric.uplinks empty violates the MinItems=1 marker. A fabric\n"
            "block attaches a node through its uplinks, so a block without one\n"
            "has nothing to peer over."
        ),
        extra=fabric(uplinks="    uplinks: []"),
    ),
    Fixture(
        filename="20-fabric-uplinks-too-many.yaml",
        comment=(
            "Five spec.fabric.uplinks entries violate the MaxItems=4 marker. The\n"
            "schema is the only layer that bounds the list."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-fab1\n"
            "    - interface: fab2\n"
            "      bridge: br-fab2\n"
            "    - interface: fab3\n"
            "      bridge: br-fab3\n"
            "    - interface: fab4\n"
            "      bridge: br-fab4\n"
            "    - interface: fab5\n"
            "      bridge: br-fab5"
        )),
    ),
    Fixture(
        filename="21-fabric-uplink-interface-pattern.yaml",
        comment=(
            "A spec.fabric.uplinks interface of 16 characters violates the CRD\n"
            "pattern (^[a-zA-Z0-9_.-]{1,15}$): the kernel's IFNAMSIZ leaves 15\n"
            "usable bytes, so no NIC carries a longer name."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab-sixteen-char\n"
            "      bridge: br-fab1"
        )),
    ),
    Fixture(
        filename="22-fabric-uplink-bridge-pattern.yaml",
        comment=(
            "A spec.fabric.uplinks bridge of 16 characters violates the same\n"
            "pattern: the peer bridge appears as a Linux interface too."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-sixteen-chars"
        )),
    ),
    Fixture(
        filename="23-fabric-uplink-duplicate-interface.yaml",
        comment=(
            "Two spec.fabric.uplinks entries naming one interface are rejected by\n"
            "the schema: interface is the +listMapKey of the list, so the API\n"
            "server answers before the webhook is called."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-fab1\n"
            "    - interface: fab1\n"
            "      bridge: br-fab2"
        )),
    ),
    Fixture(
        filename="24-fabric-uplink-duplicate-bridge.yaml",
        comment=(
            "Two spec.fabric.uplinks entries sharing one bridge are rejected by the\n"
            "validating webhook alone: the list-map key covers the interface side\n"
            "only, and a bridge cannot take two NICs as two peerings."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-fab1\n"
            "    - interface: fab2\n"
            "      bridge: br-fab1"
        )),
    ),
    Fixture(
        filename="25-fabric-uplink-bridge-is-interface.yaml",
        comment=(
            "A spec.fabric.uplinks bridge named after an uplink interface is\n"
            "rejected by the validating webhook alone: the bridge would take the\n"
            "NIC's own name in the kernel."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-fab1\n"
            "    - interface: fab2\n"
            "      bridge: fab1"
        )),
    ),
    Fixture(
        filename="26-fabric-uplink-bridge-br-int.yaml",
        comment=(
            "A spec.fabric.uplinks bridge named br-int is rejected by the validating\n"
            "webhook alone. br-int is the integration bridge ovn-controller owns,\n"
            "and the fabric script would enslave it to the fabric VRF."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: br-int"
        )),
    ),
    Fixture(
        filename="27-fabric-vrf-name-pattern.yaml",
        comment=(
            "A spec.fabric.vrf.name of 16 characters violates the CRD pattern\n"
            "(^[a-zA-Z0-9_.-]{1,15}$): the VRF is a Linux interface."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      name: fabric-sixteen-c"
        )),
    ),
    Fixture(
        filename="28-fabric-vrf-name-ovnvrf-prefix.yaml",
        comment=(
            "A spec.fabric.vrf.name starting with ovnvrf is rejected by the\n"
            "validating webhook alone: ovn-controller maintains its VRFs under\n"
            "that prefix and deletes one it no longer needs."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      name: ovnvrf1"
        )),
    ),
    Fixture(
        filename="29-fabric-vrf-name-taken-by-uplink.yaml",
        comment=(
            "A spec.fabric.vrf.name an uplink interface also carries is rejected by\n"
            "the validating webhook alone, on the interface: the VRF device, the\n"
            "bridges and the NICs share one kernel namespace."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      name: fab1"
        )),
    ),
    Fixture(
        filename="30-fabric-vrf-table-zero.yaml",
        comment=(
            "spec.fabric.vrf.table of 0 violates the Minimum=1 marker: the kernel\n"
            "has no table 0 to bind a VRF to."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      table: 0"
        )),
    ),
    Fixture(
        filename="31-fabric-vrf-table-kernel.yaml",
        comment=(
            "spec.fabric.vrf.table of 254 is rejected by the validating webhook\n"
            "alone: 253, 254 and 255 are the kernel's default, main and local\n"
            "tables, and the fabric routes must stay out of them."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      table: 254"
        )),
    ),
    Fixture(
        filename="32-fabric-vrf-table-is-ovn-vrf-id.yaml",
        comment=(
            "spec.fabric.vrf.table equal to a listed VRF id is rejected by the\n"
            "validating webhook alone: ovn-controller creates ovnvrf<id> with\n"
            "table <id>, so the two VRFs would share one table."
        ),
        extra=fabric(vrf=(
            "    vrf:\n"
            "      table: 42"
        )),
    ),
    Fixture(
        filename="33-fabric-mtu-below-minimum.yaml",
        comment=(
            "spec.fabric.mtu of 1279 violates the Minimum=1280 marker, the\n"
            "smallest MTU an IPv6 link may carry."
        ),
        extra=fabric(mtu="    mtu: 1279"),
    ),
    Fixture(
        filename="34-fabric-node-asn-below-range.yaml",
        comment=(
            "spec.fabric.nodes[].asn of 4199999999 violates the Minimum=4200000000\n"
            "marker: a node takes a private 4-byte ASN."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE.replace("4200000001", "4199999999")),
    ),
    Fixture(
        filename="35-fabric-node-asn-above-range.yaml",
        comment=(
            "spec.fabric.nodes[].asn of 4294967295 violates the Maximum=4294967294\n"
            "marker: the last 4-byte ASN is reserved."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE.replace("4200000001", "4294967295")),
    ),
    Fixture(
        filename="36-fabric-node-duplicate-asn.yaml",
        comment=(
            "Two spec.fabric.nodes rows sharing one ASN are rejected by the\n"
            "validating webhook alone: a BGP speaker drops a route whose AS path\n"
            "carries its own ASN, so the two nodes would drop each other's routes."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE + "\n" + SECOND_FABRIC_NODE.replace("4200000002", "4200000001")),
    ),
    Fixture(
        filename="37-fabric-node-loopback-ipv4-format.yaml",
        comment=(
            "A spec.fabric.nodes[].loopbackIPv4 that is no IPv4 address violates the\n"
            "Format=ipv4 marker."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE.replace("192.0.2.1", "192.0.2.256")),
    ),
    Fixture(
        filename="38-fabric-node-loopback-ipv6-format.yaml",
        comment=(
            "A spec.fabric.nodes[].loopbackIPv6 that is no IPv6 address violates the\n"
            "Format=ipv6 marker."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE.replace("2001:db8::1", "2001:db8::g")),
    ),
    Fixture(
        filename="39-fabric-node-duplicate-loopback.yaml",
        comment=(
            "Two spec.fabric.nodes rows sharing one IPv4 loopback are rejected by\n"
            "the validating webhook alone: both nodes would announce the same\n"
            "host route."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE + "\n" + SECOND_FABRIC_NODE.replace("192.0.2.2", "192.0.2.1")),
    ),
    Fixture(
        filename="40-fabric-announceable-range-format.yaml",
        comment=(
            "A spec.fabric.announceableRanges entry that is no CIDR violates the\n"
            "Format=cidr marker on the list items."
        ),
        extra=fabric(ranges=(
            "    announceableRanges:\n"
            "    - not-a-cidr"
        )),
    ),
    Fixture(
        filename="41-fabric-announceable-range-host-bits.yaml",
        comment=(
            "A spec.fabric.announceableRanges entry with host bits set passes the\n"
            "cidr format and is rejected by the validating webhook alone, which\n"
            "names the network address the range masks to."
        ),
        extra=fabric(ranges=(
            "    announceableRanges:\n"
            "    - 198.51.100.7/24"
        )),
    ),
    Fixture(
        filename="42-fabric-advertise-without-ranges.yaml",
        comment=(
            "spec.fabric.advertiseVRFIDs without an announceable range is rejected\n"
            "by the validating webhook alone: the node's outbound prefix lists\n"
            "would permit no route OVN advertises."
        ),
        extra=fabric(ranges=""),
    ),
    Fixture(
        filename="43-fabric-vrf-id-zero.yaml",
        comment=(
            "A spec.fabric.advertiseVRFIDs entry of 0 violates the Minimum=1 marker\n"
            "on the list items: ovn-controller names no VRF ovnvrf0."
        ),
        extra=fabric(advertise="    advertiseVRFIDs: [0]"),
    ),
    Fixture(
        filename="44-fabric-vrf-id-advertised-and-learned.yaml",
        comment=(
            "A VRF id in both spec.fabric.advertiseVRFIDs and learnVRFIDs is\n"
            "rejected by the validating webhook alone: importing in both\n"
            "directions between one ovnvrf<id> and the fabric VRF is untested."
        ),
        extra=fabric(learn="    learnVRFIDs: [10, 42]"),
    ),
    Fixture(
        filename="45-fabric-bfd-interval-below-minimum.yaml",
        comment=(
            "spec.fabric.bfd.receiveIntervalMs of 9 violates the Minimum=10 marker,\n"
            "the bound frr-k8s puts on the interval."
        ),
        extra=fabric(bfd=(
            "    bfd:\n"
            "      receiveIntervalMs: 9"
        )),
    ),
    Fixture(
        filename="46-fabric-bfd-multiplier-below-minimum.yaml",
        comment=(
            "spec.fabric.bfd.detectMultiplier of 1 violates the Minimum=2 marker,\n"
            "the bound frr-k8s puts on the multiplier."
        ),
        extra=fabric(bfd=(
            "    bfd:\n"
            "      detectMultiplier: 1"
        )),
    ),
    Fixture(
        filename="47-fabric-node-duplicate-name.yaml",
        comment=(
            "Two spec.fabric.nodes rows naming one node are rejected by the schema:\n"
            "name is the +listMapKey of the list, so the API server answers before\n"
            "the webhook is called."
        ),
        extra=fabric(nodes=VALID_FABRIC_NODE + "\n" + SECOND_FABRIC_NODE.replace("node-2", "node-1")),
    ),
    Fixture(
        filename="48-fabric-uplink-bridge-is-vrf.yaml",
        comment=(
            "A spec.fabric.uplinks bridge named after the fabric VRF is rejected by\n"
            "the validating webhook alone, on the bridge: the VRF device, the\n"
            "bridges and the NICs share one kernel namespace."
        ),
        extra=fabric(uplinks=(
            "    uplinks:\n"
            "    - interface: fab1\n"
            "      bridge: fabric"
        )),
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
