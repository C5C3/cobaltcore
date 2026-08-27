#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the neutron invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Neutron CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/placement/invalid-cr/_generate.py``. Each fixture mutates exactly
one aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

The scaffold carries the eight required properties of NeutronSpec: cache,
database, image, keystoneEndpoint, messaging, openStackRelease, ovn and
serviceUser. The OVNCentral spec.ovn.centralRef names does not exist in the
ephemeral namespace: admission tolerates the dangling reference (GitOps
ordering), so the reference never competes with the rule a fixture pins. The
same holds for the RabbitmqCluster, the MariaDB, the Memcached and the two
Secrets.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Four rules have
no schema counterpart and are answered by the webhook: the cron grammar of
spec.ovnDBSync.schedule, the two spec.extraConfig checks (the map is
preserve-unknown-fields, which CEL cannot constrain), and the metadata.name
bound, which comes from the name of a child object. Each fixture comment names
the layer that answers it, and the matching Chainsaw step asserts that layer's
message.

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

# Canonical valid Neutron CR scaffold. Any future required field on NeutronSpec
# must be added below AND verified against every fixture. keystoneEndpoint and
# serviceUser are written out rather than parameterized because no fixture in
# this corpus varies them.
# Placeholders: {name} CR name, {release} the openStackRelease value, {image},
# {database}, {cache}, {messaging} and {ovn} the whole block each names, {extra}
# trailing spec additions.
SCAFFOLD = """\
apiVersion: neutron.openstack.c5c3.io/v1alpha1
kind: Neutron
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
{image}
{database}
{cache}
{messaging}
{ovn}
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: neutron-service-password
{extra}"""

VALID_RELEASE = "2025.2"

VALID_IMAGE = """\
  image:
    repository: ghcr.io/c5c3/neutron
    tag: "2025.2\""""

VALID_DATABASE = """\
  database:
    clusterRef:
      name: openstack-db
    database: neutron
    secretRef:
      name: neutron-db"""

VALID_CACHE = """\
  cache:
    clusterRef:
      name: openstack-memcached"""

VALID_MESSAGING = """\
  messaging:
    clusterRef:
      name: openstack-rabbitmq"""

VALID_OVN = """\
  ovn:
    centralRef:
      name: ovn-central"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    release: str = VALID_RELEASE
    image: str = VALID_IMAGE
    database: str = VALID_DATABASE
    cache: str = VALID_CACHE
    messaging: str = VALID_MESSAGING
    ovn: str = VALID_OVN
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            image=self.image,
            database=self.database,
            cache=self.cache,
            messaging=self.messaging,
            ovn=self.ovn,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-openstackrelease-pattern.yaml",
        comment=(
            "spec.openStackRelease with a non-cadence minor violates the CRD pattern\n"
            "(^\\d{4}\\.[12]$), a schema-level rejection the API server answers before\n"
            "the validating webhook runs. 2025.9 is deliberately well-formed apart\n"
            "from the minor: it pins the [12] class rather than the digit count."
        ),
        name="neutron-invalid-release-pattern",
        release="2025.9",
    ),
    Fixture(
        filename="01-image-tag-and-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL rule\n"
            "(has(self.tag) != has(self.digest)); validateImage mirrors it in the\n"
            "webhook with the same message, but the schema answers first."
        ),
        name="neutron-invalid-image-both",
        image=(
            "  image:\n"
            "    repository: ghcr.io/c5c3/neutron\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="02-database-clusterref-and-host.yaml",
        comment=(
            "spec.database with both clusterRef and host violates the shared\n"
            "DatabaseSpec XOR CEL rule; the webhook mirrors it via DatabaseXOR."
        ),
        name="neutron-invalid-database-both",
        database=(
            "  database:\n"
            "    clusterRef:\n"
            "      name: openstack-db\n"
            "    database: neutron\n"
            "    secretRef:\n"
            "      name: neutron-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="03-cache-clusterref-and-servers.yaml",
        comment=(
            "spec.cache with both clusterRef and servers violates the shared CacheSpec\n"
            "XOR CEL rule: the two modes resolve [keystone_authtoken]\n"
            "memcached_servers differently, and naming both leaves no rule for which\n"
            "one wins. The webhook mirrors it via CacheXOR."
        ),
        name="neutron-invalid-cache-both",
        cache=(
            "  cache:\n"
            "    clusterRef:\n"
            "      name: openstack-memcached\n"
            "    servers:\n"
            "    - memcached.example.com:11211"
        ),
    ),
    Fixture(
        filename="04-messaging-clusterref-and-secretref.yaml",
        comment=(
            "spec.messaging with both clusterRef and secretRef violates the shared\n"
            "MessagingSpec XOR CEL rule: managed mode derives the transport URL from\n"
            "the RabbitmqCluster, brownfield mode reads it from the Secret, and naming\n"
            "both leaves no rule for which URL the pods get. The webhook mirrors it via\n"
            "MessagingXOR."
        ),
        name="neutron-invalid-messaging-both",
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: openstack-rabbitmq\n"
            "    secretRef:\n"
            "      name: neutron-transport-url"
        ),
    ),
    Fixture(
        filename="05-messaging-neither-mode.yaml",
        comment=(
            "spec.messaging present but naming neither mode violates the same\n"
            "MessagingSpec XOR CEL rule from the other side. The block is required on\n"
            "NeutronSpec, so an empty object is the way to reach the rule: an omitted\n"
            "spec.messaging is answered by the required-properties list instead, which\n"
            "pins nothing about the XOR."
        ),
        name="neutron-invalid-messaging-neither",
        messaging="  messaging: {}",
    ),
    Fixture(
        filename="06-messaging-tls-without-cabundle-name.yaml",
        comment=(
            "spec.messaging.tls with an unnamed CA bundle Secret has nothing to verify\n"
            "the broker against. The schema answers: caBundleSecretRef is a required\n"
            "property of the TLS block and its name carries the MinLength=1 marker of\n"
            "the shared SecretRefSpec, so the empty name is rejected before the\n"
            "webhook's field.Required twin runs."
        ),
        name="neutron-invalid-messaging-tls",
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: openstack-rabbitmq\n"
            "    tls:\n"
            "      caBundleSecretRef:\n"
            '        name: ""'
        ),
    ),
    Fixture(
        filename="07-ovn-centralref-name-empty.yaml",
        comment=(
            "spec.ovn.centralRef.name empty violates the MinLength=1 marker on\n"
            "OVNCentralRef, which the API server answers before the webhook's\n"
            "field.Required twin runs. The ML2/OVN mechanism driver writes the logical\n"
            "network model into the Northbound database of the named OVNCentral, so a\n"
            "Neutron without one has nothing to program."
        ),
        name="neutron-invalid-ovn-centralref",
        ovn=(
            "  ovn:\n"
            "    centralRef:\n"
            '      name: ""'
        ),
    ),
    Fixture(
        filename="08-ovndbsync-schedule-invalid.yaml",
        comment=(
            "spec.ovnDBSync.schedule outside the cron grammar is rejected by the\n"
            "validating webhook alone. The field carries no CRD pattern on purpose: the\n"
            "accepted grammar includes descriptors such as @daily, which no regex\n"
            "expresses without also rejecting valid expressions, so\n"
            "validation.CronSchedule is the sole gate. The value carries two fields\n"
            "where five are expected, so it is rejected at admission rather than by\n"
            "the CronJob create the reconciler would otherwise attempt."
        ),
        name="neutron-invalid-ovndbsync-schedule",
        extra=(
            "  ovnDBSync:\n"
            '    schedule: "x y"\n'
        ),
    ),
    Fixture(
        filename="09-ovndbsync-syncmode-invalid.yaml",
        comment=(
            "spec.ovnDBSync.syncMode outside the CRD enum (log, repair) is answered by\n"
            "the schema. The two values are the two modes\n"
            "neutron-ovn-db-sync-util has, so a third names no behaviour the CronJob\n"
            "could run."
        ),
        name="neutron-invalid-ovndbsync-syncmode",
        extra=(
            "  ovnDBSync:\n"
            "    syncMode: wipe\n"
        ),
    ),
    Fixture(
        filename="10-extraconfig-rejected-owned-key.yaml",
        comment=(
            "spec.extraConfig setting [ovn] ovn_nb_connection is rejected by the\n"
            "validating webhook: extraConfig is a preserve-unknown-fields map, so CEL\n"
            "cannot constrain its keys and admission is the only gate. The key is\n"
            "Rejected rather than merely owned because the connection string is\n"
            "resolved from spec.ovn.centralRef, and another address points the\n"
            "mechanism driver at a logical model this Neutron does not own."
        ),
        name="neutron-invalid-extraconfig-owned",
        extra=(
            "  extraConfig:\n"
            "    ovn:\n"
            '      ovn_nb_connection: "tcp:1.2.3.4:6641"\n'
        ),
    ),
    Fixture(
        filename="11-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in a known section is rejected\n"
            "by the validating webhook against the embedded neutron 2025.2 option\n"
            "catalog. [DEFAULT] is a section the catalog carries, so the rejection is\n"
            "the unknown-option one rather than the unknown-section one."
        ),
        name="neutron-invalid-extraconfig-option",
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            '      no_such_option: "1"\n'
        ),
    ),
    Fixture(
        filename="12-name-too-long.yaml",
        comment=(
            "A metadata.name of 41 characters is rejected by the validating webhook\n"
            "alone. The bound is 40: the sync CronJob is named {name}-ovn-db-sync, 12\n"
            "characters on top of the CR name, against the 52-character cap Kubernetes\n"
            "puts on a CronJob name. The name is a valid DNS-1123 subdomain apart from\n"
            "its length, so the bound is the only rule it breaks. The rule runs in\n"
            "ValidateCreate only, so the finalizer-removal update never trips over it."
        ),
        name="neutron-invalid-name-past-the-40-char-cap",
    ),
    Fixture(
        filename="13-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the CR's children.\n"
            "The webhook repeats it via validation.TargetClusterRef, but the schema\n"
            "answers first."
        ),
        name="neutron-invalid-targetclusterref",
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
        print("run `python3 tests/e2e/neutron/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
