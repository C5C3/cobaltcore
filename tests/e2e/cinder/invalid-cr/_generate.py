#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the cinder invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Cinder CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/neutron/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

The scaffold carries the five required properties of CinderSpec: cache,
database, image, messaging and openStackRelease. It is deliberately
Keystone-free — spec.keystoneEndpoint and spec.serviceUser are omitted
together, which the pairing rule allows — so the two fixtures that pin that
pairing are the only ones naming either half. The MariaDB, the Memcached, the
RabbitmqCluster and the Secrets the scaffold references do not exist in the
ephemeral namespace: admission tolerates the dangling references (GitOps
ordering), so they never compete with the rule a fixture pins.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Four rules have
no schema counterpart and are answered by the webhook: the cron grammar of
spec.dbPurge.schedule, the two spec.extraConfig checks (the map is
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

# Canonical valid Cinder CR scaffold. Any future required field on CinderSpec
# must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {release} the openStackRelease value, {image},
# {database}, {cache} and {messaging} the whole block each names, {extra}
# trailing spec additions.
SCAFFOLD = """\
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: Cinder
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
{image}
{database}
{cache}
{messaging}
{extra}"""

VALID_RELEASE = "2025.2"

VALID_IMAGE = """\
  image:
    repository: ghcr.io/c5c3/cinder
    tag: "2025.2\""""

VALID_DATABASE = """\
  database:
    clusterRef:
      name: openstack-db
    database: cinder
    secretRef:
      name: cinder-db"""

VALID_CACHE = """\
  cache:
    clusterRef:
      name: openstack-memcached"""

VALID_MESSAGING = """\
  messaging:
    clusterRef:
      name: openstack-rabbitmq"""


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
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            image=self.image,
            database=self.database,
            cache=self.cache,
            messaging=self.messaging,
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
        name="cinder-invalid-release-pattern",
        release="2025.9",
    ),
    Fixture(
        filename="01-image-tag-and-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL rule\n"
            "(has(self.tag) != has(self.digest)); validateImage mirrors it in the\n"
            "webhook with the same message, but the schema answers first."
        ),
        name="cinder-invalid-image-both",
        image=(
            "  image:\n"
            "    repository: ghcr.io/c5c3/cinder\n"
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
        name="cinder-invalid-database-both",
        database=(
            "  database:\n"
            "    clusterRef:\n"
            "      name: openstack-db\n"
            "    database: cinder\n"
            "    secretRef:\n"
            "      name: cinder-db\n"
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
        name="cinder-invalid-cache-both",
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
        name="cinder-invalid-messaging-both",
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: openstack-rabbitmq\n"
            "    secretRef:\n"
            "      name: cinder-transport-url"
        ),
    ),
    Fixture(
        filename="05-messaging-neither-mode.yaml",
        comment=(
            "spec.messaging present but naming neither mode violates the same\n"
            "MessagingSpec XOR CEL rule from the other side. The block is required on\n"
            "CinderSpec, so an empty object is the way to reach the rule: an omitted\n"
            "spec.messaging is answered by the required-properties list instead, which\n"
            "pins nothing about the XOR."
        ),
        name="cinder-invalid-messaging-neither",
        messaging="  messaging: {}",
    ),
    Fixture(
        filename="06-messaging-tls-without-cabundle-name.yaml",
        comment=(
            "spec.messaging.tls with an unnamed CA bundle Secret has nothing to verify\n"
            "the broker against. The schema answers: caBundleSecretRef is a required\n"
            "property of the TLS block and its name carries the MinLength=1 marker of\n"
            "the shared SecretRefSpec, so the empty name is rejected before any\n"
            "webhook runs."
        ),
        name="cinder-invalid-messaging-tls",
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
        filename="07-dbpurge-schedule-invalid.yaml",
        comment=(
            "spec.dbPurge.schedule outside the cron grammar is rejected by the\n"
            "validating webhook alone. The field carries no CRD pattern on purpose: the\n"
            "accepted grammar includes descriptors such as @daily, which no regex\n"
            "expresses without also rejecting valid expressions, so\n"
            "validation.CronSchedule is the sole gate. The value carries four fields\n"
            "where five are expected, so it is rejected at admission rather than by\n"
            "the CronJob create the reconciler would otherwise attempt."
        ),
        name="cinder-invalid-dbpurge-schedule",
        extra=(
            "  dbPurge:\n"
            '    schedule: "* * * *"\n'
        ),
    ),
    Fixture(
        filename="08-dbpurge-retentiondays-below-minimum.yaml",
        comment=(
            "spec.dbPurge.retentionDays below the Minimum=1 marker is answered by the\n"
            "schema. A zero-day window purges the rows an in-flight request just\n"
            "wrote, so the floor keeps the CronJob from racing the API. The webhook\n"
            "repeats the bound, but the schema answers first."
        ),
        name="cinder-invalid-dbpurge-retention",
        extra=(
            "  dbPurge:\n"
            "    retentionDays: 0\n"
        ),
    ),
    Fixture(
        filename="09-internaltenant-userid-empty.yaml",
        comment=(
            "spec.internalTenant.userID empty violates the MinLength=1 marker on\n"
            "InternalTenantSpec. Cinder passes the ID to the volume API without\n"
            "resolving it through Keystone, so an empty one creates the image-volume\n"
            "cache's volumes under nothing. The webhook repeats it as a\n"
            "field.Required, but the schema answers first."
        ),
        name="cinder-invalid-internaltenant-userid",
        extra=(
            "  internalTenant:\n"
            "    projectID: 8f3a1c0e5b7d4a2f9c6e1b8d3a7f5c40\n"
            '    userID: ""\n'
        ),
    ),
    Fixture(
        filename="10-keymanager-without-keystone-endpoint.yaml",
        comment=(
            "spec.keyManager without spec.keystoneEndpoint violates the CEL rule on\n"
            "CinderSpec: castellan reaches Barbican with the Keystone credentials from\n"
            "spec.serviceUser, so a key manager on a Keystone-free deployment can\n"
            "authenticate against nothing. The webhook mirrors it with the same\n"
            "message, but the schema answers first."
        ),
        name="cinder-invalid-keymanager-no-keystone",
        extra=(
            "  keyManager:\n"
            "    type: Barbican\n"
            "    barbican:\n"
            "      endpoint: http://barbican.openstack.svc.cluster.local:9311\n"
        ),
    ),
    Fixture(
        filename="11-serviceuser-without-keystone-endpoint.yaml",
        comment=(
            "spec.serviceUser without spec.keystoneEndpoint violates the pairing CEL\n"
            "rule on CinderSpec: the endpoint names who to authenticate against and\n"
            "the service user carries the credentials, so either alone renders a\n"
            "[keystone_authtoken] section the service cannot use. The webhook mirrors\n"
            "it with the same message, but the schema answers first."
        ),
        name="cinder-invalid-serviceuser-no-keystone",
        extra=(
            "  serviceUser:\n"
            "    secretRef:\n"
            "      name: cinder-service-password\n"
        ),
    ),
    Fixture(
        filename="12-volume-replicas-above-one.yaml",
        comment=(
            "spec.volume.deployment.replicas above one violates the single-writer CEL\n"
            "rule on CinderSpec. A cinder-volume owns its backend through a host\n"
            "identity rather than through a lock, so a second process under the same\n"
            "identity has the same volume state open twice, which the NFS drivers\n"
            "refuse. validateSingletonDeployment mirrors it in the webhook with the\n"
            "same message, but the schema answers first."
        ),
        name="cinder-invalid-volume-replicas",
        extra=(
            "  volume:\n"
            "    deployment:\n"
            "      replicas: 2\n"
        ),
    ),
    Fixture(
        filename="13-backup-strategy-not-recreate.yaml",
        comment=(
            "spec.backup.deployment.strategy.type other than Recreate violates the\n"
            "second single-writer CEL rule on CinderSpec: a rolling update overlaps\n"
            "the surge pod with the outgoing one, and both hold the backup target open\n"
            "under the same host identity. replicas is spelled out at 1 because the\n"
            "shared DeploymentSpec defaults a present deployment block to three, which\n"
            "would trip the replica rule as well and stop this fixture isolating the\n"
            "strategy one."
        ),
        name="cinder-invalid-backup-strategy",
        extra=(
            "  backup:\n"
            "    deployment:\n"
            "      replicas: 1\n"
            "      strategy:\n"
            "        type: RollingUpdate\n"
        ),
    ),
    Fixture(
        filename="14-extraconfig-rejected-owned-key.yaml",
        comment=(
            "spec.extraConfig setting [DEFAULT] auth_strategy is rejected by the\n"
            "validating webhook: extraConfig is a preserve-unknown-fields map, so CEL\n"
            "cannot constrain its keys and admission is the only gate. The key is\n"
            "Rejected rather than merely owned because extraConfig has the last word\n"
            "in the merge, so a value other than keystone selects a WSGI pipeline\n"
            "without token validation the moment the pods load the rendered file."
        ),
        name="cinder-invalid-extraconfig-owned",
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            "      auth_strategy: noauth\n"
        ),
    ),
    Fixture(
        filename="15-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in a known section is rejected\n"
            "by the validating webhook against the embedded cinder 2025.2 option\n"
            "catalog. [DEFAULT] is a section the catalog carries, so the rejection is\n"
            "the unknown-option one rather than the unknown-section one."
        ),
        name="cinder-invalid-extraconfig-option",
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            '      no_such_option: "1"\n'
        ),
    ),
    Fixture(
        filename="16-name-too-long.yaml",
        comment=(
            "A metadata.name of 44 characters is rejected by the validating webhook\n"
            "alone. The bound is 43: the purge CronJob is named {name}-db-purge, 9\n"
            "characters on top of the CR name, against the 52-character cap Kubernetes\n"
            "puts on a CronJob name. The name is a valid DNS-1123 subdomain apart from\n"
            "its length, so the bound is the only rule it breaks. The rule runs in\n"
            "ValidateCreate only, so the finalizer-removal update never trips over it."
        ),
        name="cinder-invalid-name-one-past-the-43-char-cap",
    ),
    Fixture(
        filename="17-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the CR's children.\n"
            "The webhook repeats it via validation.TargetClusterRef, but the schema\n"
            "answers first."
        ),
        name="cinder-invalid-targetclusterref",
        extra=(
            "  targetClusterRef:\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="18-gateway-without-keystone.yaml",
        comment=(
            "spec.gateway on a Keystone-free Cinder violates the spec-level CEL rule\n"
            "(!has(self.gateway) || has(self.keystoneEndpoint)); the webhook mirrors it\n"
            "with the same message, but the schema answers first. Without\n"
            "spec.keystoneEndpoint the API renders auth_strategy = noauth, a pipeline\n"
            "that takes the project from the request URL and validates no token, so an\n"
            "HTTPRoute in front of it would serve every volume operation of every\n"
            "project unauthenticated. The scaffold is Keystone-free already, so the\n"
            "gateway block alone is what this fixture adds."
        ),
        name="cinder-invalid-gateway-no-keystone",
        extra=(
            "  gateway:\n"
            "    hostname: cinder.example.com\n"
            "    parentRef:\n"
            "      name: openstack-gateway\n"
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
        print("run `python3 tests/e2e/cinder/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
