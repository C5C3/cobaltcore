#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the nova invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Nova CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/cinder/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

The scaffold carries the nine required properties of NovaSpec: apiDatabase,
cache, database, image, keystoneEndpoint, messaging, metadata, openStackRelease
and serviceUser. Nova has no Keystone-free posture, so unlike the cinder
scaffold this one names a Keystone endpoint and a service user from the start.
The MariaDB, the Memcached, the RabbitmqCluster and the Secrets the scaffold
references do not exist in the ephemeral namespace: admission tolerates the
dangling references (GitOps ordering), so they never compete with the rule a
fixture pins.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. The schema
answers the markers (patterns, minima, MinLength), the database CEL rules on
NovaSpec and on spec.database, the remote-compute CEL rule on NovaSpec, and the
shared types' CEL rules. The console-proxy
rule on NovaSpec has no fixture: the defaulting webhook removes a disabled
proxy's deployment block before the schema measures it, so only a
webhook-less API server (the CRD-only envtest) can observe it. Seven rules have
no schema counterpart and are answered by the webhook alone: the cron grammar of
spec.dbArchive.schedule, the two spec.extraConfig checks (the map is
preserve-unknown-fields, which CEL cannot constrain), the two metadata.name rules,
which come from the names of child objects, the newline check on the typed
fields rendered into nova.conf, and the console route's path. Each fixture
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

# Canonical valid Nova CR scaffold. Any future required field on NovaSpec must
# be added below AND verified against every fixture.
# Placeholders: {name} CR name, {release} the openStackRelease value, {image},
# {apiDatabase}, {database}, {cache}, {messaging}, {keystoneEndpoint},
# {serviceUser} and {metadata} the whole block each names, {extra} trailing
# spec additions.
SCAFFOLD = """\
apiVersion: nova.openstack.c5c3.io/v1alpha1
kind: Nova
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
{image}
{apiDatabase}
{database}
{cache}
{messaging}
{keystoneEndpoint}
{serviceUser}
{metadata}
{extra}"""

VALID_NAME = "nova-invalid"

VALID_RELEASE = "2025.2"

VALID_IMAGE = """\
  image:
    repository: ghcr.io/c5c3/nova
    tag: "2025.2\""""

# Nova splits its state across two schemas, so the scaffold carries two database
# blocks. They name different schemas and share the default credentialsMode,
# which is what the two kind-level database rules require.
VALID_API_DATABASE = """\
  apiDatabase:
    clusterRef:
      name: mariadb
    database: nova_api
    secretRef:
      name: nova-api-db"""

VALID_DATABASE = """\
  database:
    clusterRef:
      name: mariadb
    database: nova
    secretRef:
      name: nova-db"""

VALID_CACHE = """\
  cache:
    clusterRef:
      name: memcached"""

VALID_MESSAGING = """\
  messaging:
    clusterRef:
      name: rabbitmq"""

VALID_KEYSTONE_ENDPOINT = "  keystoneEndpoint: http://keystone.openstack.svc:5000"

VALID_SERVICE_USER = """\
  serviceUser:
    secretRef:
      name: nova-service-user"""

VALID_METADATA = """\
  metadata:
    sharedSecretRef:
      name: nova-metadata-secret"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str = VALID_NAME
    release: str = VALID_RELEASE
    image: str = VALID_IMAGE
    api_database: str = VALID_API_DATABASE
    database: str = VALID_DATABASE
    cache: str = VALID_CACHE
    messaging: str = VALID_MESSAGING
    keystone_endpoint: str = VALID_KEYSTONE_ENDPOINT
    service_user: str = VALID_SERVICE_USER
    metadata: str = VALID_METADATA
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            image=self.image,
            apiDatabase=self.api_database,
            database=self.database,
            cache=self.cache,
            messaging=self.messaging,
            keystoneEndpoint=self.keystone_endpoint,
            serviceUser=self.service_user,
            metadata=self.metadata,
            extra=self.extra,
        )
        # A block dropped from the scaffold (spec.messaging on the
        # messaging-missing fixture) renders its placeholder line empty. Drop
        # such a line so no fixture carries a blank line in the middle of its
        # spec.
        body = "".join(line for line in body.splitlines(keepends=True) if line != "\n")
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
        release="2025.9",
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
            "    repository: ghcr.io/c5c3/nova\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="02-apidatabase-clusterref-and-host.yaml",
        comment=(
            "spec.apiDatabase with both clusterRef and host violates the shared\n"
            "DatabaseSpec XOR CEL rule; the webhook mirrors it via DatabaseXOR. The\n"
            "rule sits on the shared type, so it reaches both of Nova's database\n"
            "blocks and each gets its own fixture."
        ),
        api_database=(
            "  apiDatabase:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova_api\n"
            "    secretRef:\n"
            "      name: nova-api-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="03-database-clusterref-and-host.yaml",
        comment=(
            "spec.database with both clusterRef and host violates the same shared\n"
            "DatabaseSpec XOR CEL rule as the apiDatabase fixture above, on the cell\n"
            "half of Nova's state."
        ),
        database=(
            "  database:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova\n"
            "    secretRef:\n"
            "      name: nova-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="04-cache-clusterref-and-servers.yaml",
        comment=(
            "spec.cache with both clusterRef and servers violates the shared CacheSpec\n"
            "XOR CEL rule: the two modes resolve [cache] memcache_servers differently,\n"
            "and naming both leaves no rule for which one wins. The webhook mirrors it\n"
            "via CacheXOR."
        ),
        cache=(
            "  cache:\n"
            "    clusterRef:\n"
            "      name: memcached\n"
            "    servers:\n"
            "    - memcached.example.com:11211"
        ),
    ),
    Fixture(
        filename="05-messaging-clusterref-and-secretref.yaml",
        comment=(
            "spec.messaging with both clusterRef and secretRef violates the shared\n"
            "MessagingSpec XOR CEL rule: managed mode derives the transport URL from\n"
            "the RabbitmqCluster, brownfield mode reads it from the Secret, and naming\n"
            "both leaves no rule for which URL the pods get. The webhook mirrors it via\n"
            "MessagingXOR."
        ),
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: rabbitmq\n"
            "    secretRef:\n"
            "      name: nova-transport-url"
        ),
    ),
    Fixture(
        filename="06-messaging-neither-mode.yaml",
        comment=(
            "spec.messaging present but naming neither mode violates the same\n"
            "MessagingSpec XOR CEL rule from the other side. An omitted spec.messaging\n"
            "reaches the rule as the same empty object, which the next fixture pins."
        ),
        messaging="  messaging: {}",
    ),
    Fixture(
        filename="07-messaging-tls-without-cabundle-name.yaml",
        comment=(
            "spec.messaging.tls with an unnamed CA bundle Secret has nothing to verify\n"
            "the broker against. The schema answers: caBundleSecretRef is a required\n"
            "property of the TLS block and its name carries the MinLength=1 marker of\n"
            "the shared SecretRefSpec, so the empty name is rejected before any\n"
            "webhook runs."
        ),
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: rabbitmq\n"
            "    tls:\n"
            "      caBundleSecretRef:\n"
            '        name: ""'
        ),
    ),
    Fixture(
        filename="08-messaging-missing.yaml",
        comment=(
            "spec.messaging omitted. What answers is the MessagingSpec XOR CEL rule, not\n"
            "the required-properties list: the defaulting webhook marshals the whole\n"
            "typed object back into its admission patch, and NovaSpec.Messaging is a\n"
            "non-pointer struct without an omitempty tag, so the patch materializes the\n"
            "block as an empty object. By the time the schema is checked the property\n"
            "is present and names neither mode. The fixture still earns its place: it\n"
            "pins that no layer invents a broker for a CR that names none. The API\n"
            "hands every instance request to the conductor over the bus and every\n"
            "nova-compute reaches the control plane the same way, so a Nova without a\n"
            "broker accepts requests nothing acts on."
        ),
        messaging="",
    ),
    Fixture(
        filename="09-dbarchive-schedule-invalid.yaml",
        comment=(
            "spec.dbArchive.schedule outside the cron grammar is rejected by the\n"
            "validating webhook alone. The field carries no CRD pattern on purpose: the\n"
            "accepted grammar includes descriptors such as @daily, which no regex\n"
            "expresses without also rejecting valid expressions, so\n"
            "validation.CronSchedule is the sole gate. The value carries four fields\n"
            "where five are expected, so it is rejected at admission rather than by\n"
            "the CronJob create the reconciler would otherwise attempt."
        ),
        extra=(
            "  dbArchive:\n"
            '    schedule: "* * * *"\n'
        ),
    ),
    Fixture(
        filename="10-dbarchive-maxrows-below-minimum.yaml",
        comment=(
            "spec.dbArchive.maxRows below the Minimum=1 marker is answered by the\n"
            "schema. A run bounded at zero rows moves nothing, so the CronJob would\n"
            "fire on schedule and leave the soft-delete backlog exactly where it was.\n"
            "The webhook repeats the bound, but the schema answers first."
        ),
        extra=(
            "  dbArchive:\n"
            "    maxRows: 0\n"
        ),
    ),
    Fixture(
        filename="11-dbarchive-retentiondays-below-minimum.yaml",
        comment=(
            "spec.dbArchive.retentionDays below the Minimum=1 marker is answered by the\n"
            "schema. A zero-day window archives the rows an in-flight request just\n"
            "wrote, so the floor keeps the CronJob from racing the API. The webhook\n"
            "repeats the bound, but the schema answers first."
        ),
        extra=(
            "  dbArchive:\n"
            "    retentionDays: 0\n"
        ),
    ),
    Fixture(
        filename="12-scheduler-workers-below-minimum.yaml",
        comment=(
            "spec.scheduler.workers below the Minimum=1 marker is answered by the\n"
            "schema. A scheduler pod with no worker process starts, reports nothing\n"
            "wrong and schedules nothing. The webhook repeats the floor, but the schema\n"
            "answers first."
        ),
        extra=(
            "  scheduler:\n"
            "    workers: 0\n"
        ),
    ),
    Fixture(
        filename="13-databases-same-schema.yaml",
        comment=(
            "spec.apiDatabase and spec.database naming one schema violates the first\n"
            "database CEL rule on NovaSpec. The two halves run their own db-sync\n"
            "against a different set of migrations, so pointing them at one schema\n"
            "installs both migration histories into it. Only the apiDatabase schema\n"
            "name moves, so the pair collides on the cell schema the scaffold already\n"
            "carries."
        ),
        api_database=(
            "  apiDatabase:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova\n"
            "    secretRef:\n"
            "      name: nova-api-db"
        ),
    ),
    Fixture(
        filename="14-databases-credentialsmode-mismatch.yaml",
        comment=(
            "spec.apiDatabase on Dynamic credentials while spec.database keeps the\n"
            "Static default violates the second database CEL rule on NovaSpec: the two\n"
            "schemas share a credential path, so a deployment cannot issue one of them\n"
            "dynamic credentials and the other a static password. The apiDatabase block\n"
            "keeps its clusterRef, which is what the Dynamic mode of the shared\n"
            "DatabaseSpec requires, so this fixture pins the mismatch rule alone."
        ),
        api_database=(
            "  apiDatabase:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova_api\n"
            "    credentialsMode: Dynamic\n"
            "    secretRef:\n"
            "      name: nova-api-db"
        ),
    ),
    Fixture(
        filename="15-metadata-sharedsecretref-empty-name.yaml",
        comment=(
            "spec.metadata.sharedSecretRef.name empty violates the MinLength=1 marker\n"
            "on the shared SecretRefSpec. The secret holds the value the Neutron\n"
            "metadata agent signs proxied requests with, and the block is required\n"
            "because that value has no default: the same one has to be configured on\n"
            "the Neutron side, so the operator reads it rather than inventing it."
        ),
        metadata=(
            "  metadata:\n"
            "    sharedSecretRef:\n"
            '      name: ""'
        ),
    ),
    Fixture(
        filename="16-endpoints-override-not-url.yaml",
        comment=(
            "spec.endpoints.placement.override without a scheme violates the\n"
            "^https?:// pattern the schema puts on every endpoint override. The value\n"
            "is rendered as [placement] endpoint_override, which the client passes to\n"
            "the HTTP layer verbatim, so a bare host:port reaches Placement on no\n"
            "protocol at all."
        ),
        extra=(
            "  endpoints:\n"
            "    placement:\n"
            "      override: placement.openstack.svc:8778\n"
        ),
    ),
    Fixture(
        filename="17-extraconfig-rejected-owned-key.yaml",
        comment=(
            "spec.extraConfig setting [database] connection is rejected by the\n"
            "validating webhook: extraConfig is a preserve-unknown-fields map, so CEL\n"
            "cannot constrain its keys and admission is the only gate. The key is\n"
            "Rejected rather than merely owned because the runtime value arrives\n"
            "through the OS_DATABASE__CONNECTION env override, so a file override is\n"
            "inert at runtime and only achieves copying the database password into the\n"
            "config Secret every pod mounts."
        ),
        extra=(
            "  extraConfig:\n"
            "    database:\n"
            "      connection: mysql+pymysql://nova:secret@elsewhere.example.com/nova\n"
        ),
    ),
    Fixture(
        filename="18-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in a known section is rejected\n"
            "by the validating webhook against the embedded nova 2025.2 option\n"
            "catalog. [DEFAULT] is a section the catalog carries, so the rejection is\n"
            "the unknown-option one rather than the unknown-section one."
        ),
        extra=(
            "  extraConfig:\n"
            "    DEFAULT:\n"
            '      no_such_option: "1"\n'
        ),
    ),
    Fixture(
        filename="19-name-too-long.yaml",
        comment=(
            "A metadata.name of 42 characters is rejected by the validating webhook\n"
            "alone. The bound is 41: the archive CronJob is named {name}-db-archive, 11\n"
            "characters on top of the CR name, against the 52-character cap Kubernetes\n"
            "puts on a CronJob name. The name is a valid DNS-1123 subdomain apart from\n"
            "its length, so the bound is the only rule it breaks. The rule runs in\n"
            "ValidateCreate only, so the finalizer-removal update never trips over it."
        ),
        name="nova-invalid-name-one-past-the-41-char-cap",
    ),
    Fixture(
        filename="20-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered\n"
            "cluster, so the operator would have nowhere to place the CR's children.\n"
            "The webhook repeats it via validation.TargetClusterRef, but the schema\n"
            "answers first."
        ),
        extra=(
            "  targetClusterRef:\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="21-dbarchive-sleep-below-minimum.yaml",
        comment=(
            "spec.dbArchive.sleep below the Minimum=0 marker is answered by the schema.\n"
            "A negative pause between batches has no meaning to sleep, which would fail\n"
            "every run of the archive CronJob. The webhook repeats the bound, but the\n"
            "schema answers first."
        ),
        extra=(
            "  dbArchive:\n"
            "    sleep: -1\n"
        ),
    ),
    Fixture(
        filename="22-keystoneendpoint-not-url.yaml",
        comment=(
            "spec.keystoneEndpoint without a scheme violates the ^https?:// pattern on\n"
            "the field. Every authenticated call Nova makes starts at this URL, so a\n"
            "bare host:port would fail token validation on every API request. The\n"
            "webhook repeats the check through url.Parse, but the schema answers first."
        ),
        keystone_endpoint="  keystoneEndpoint: keystone.openstack.svc:5000",
    ),
    Fixture(
        filename="23-apidatabase-names-cell0.yaml",
        comment=(
            "spec.apiDatabase naming the cell0 schema derived from spec.database\n"
            "violates the third database CEL rule on NovaSpec. cell0 is provisioned as\n"
            "<database>_cell0 on the cell block's user, so the nova_api migrations\n"
            "would run into the schema map_cell0 maps. The webhook mirrors the rule,\n"
            "but the schema answers first."
        ),
        api_database=(
            "  apiDatabase:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova_cell0\n"
            "    secretRef:\n"
            "      name: nova-api-db"
        ),
    ),
    Fixture(
        filename="24-database-name-leaves-no-room-for-cell0.yaml",
        comment=(
            "spec.database.database of 59 characters violates the size rule on\n"
            "spec.database: cell0 is provisioned as <database>_cell0, which has to fit\n"
            "the 64-character schema limit, so the bound is 58. The name matches the\n"
            "shared schema-name pattern and MaxLength=64, so the size rule is the only\n"
            "one it breaks. The webhook mirrors the rule, but the schema answers first."
        ),
        database=(
            "  database:\n"
            "    clusterRef:\n"
            "      name: mariadb\n"
            "    database: nova_cell_schema_named_one_character_past_the_58_char_bound\n"
            "    secretRef:\n"
            "      name: nova-db"
        ),
    ),
    Fixture(
        filename="25-name-collides-with-sibling-child.yaml",
        comment=(
            "A metadata.name ending in -api is rejected by the validating webhook alone.\n"
            "The Nova nova-invalid in the same namespace names its nova_api MariaDB\n"
            "Database, User and Grant nova-invalid-api, which is exactly what this CR\n"
            "would name its cell schema's, so deleting either CR would delete the\n"
            "other's. The rule runs in ValidateCreate only, like the length bound."
        ),
        name="nova-invalid-api",
    ),
    Fixture(
        filename="26-region-control-chars.yaml",
        comment=(
            "spec.region carrying a newline is rejected by the validating webhook alone:\n"
            "the field has no CRD pattern. The value is rendered verbatim into\n"
            "nova.conf and into the compute-config fragment every hypervisor loads, so\n"
            "the newline would inject the [workarounds] section below into both."
        ),
        extra='  region: "RegionOne\\n[workarounds]\\ndisable_rootwrap = true"\n',
    ),
    Fixture(
        filename="27-consoleproxy-gateway-path.yaml",
        comment=(
            "spec.consoleProxy.gateway with a path prefix is rejected by the validating\n"
            "webhook alone. The console URL the API hands a browser names a page at the\n"
            "root of the console hostname and the noVNC client opens its WebSocket\n"
            "there too, so a prefix route would match neither while it reports\n"
            "Accepted."
        ),
        extra=(
            "  consoleProxy:\n"
            "    gateway:\n"
            "      parentRef:\n"
            "        name: gateway\n"
            "      hostname: console.example.com\n"
            "      path: /console\n"
        ),
    ),
    Fixture(
        filename="28-remotecompute-without-messaging-tls.yaml",
        comment=(
            "spec.remoteCompute on a plaintext bus violates the remote-compute CEL rule\n"
            "on NovaSpec (!has(self.remoteCompute) || has(self.messaging.tls)): a\n"
            "compute on another cluster verifies the broker against the messaging CA\n"
            "bundle, and a plaintext bus carries none. The webhook mirrors it with a\n"
            "Required error on spec.messaging.tls, but the schema answers first."
        ),
        extra=(
            "  remoteCompute:\n"
            "    keystoneEndpoint: https://keystone.example.com/v3\n"
            "    transportURLSecretRef:\n"
            "      name: nova-remote-transport\n"
        ),
    ),
    Fixture(
        filename="29-remotecompute-keystoneendpoint-not-url.yaml",
        comment=(
            "spec.remoteCompute.keystoneEndpoint without a scheme violates the\n"
            "^https:// pattern the field carries, a schema-level rejection. The bus is\n"
            "verified, so the remote-compute CEL rule is met and the pattern is the\n"
            "only rule that fails."
        ),
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: rabbitmq\n"
            "    tls:\n"
            "      caBundleSecretRef:\n"
            "        name: nova-messaging-ca"
        ),
        extra=(
            "  remoteCompute:\n"
            "    keystoneEndpoint: keystone.example.com\n"
            "    transportURLSecretRef:\n"
            "      name: nova-remote-transport\n"
        ),
    ),
    Fixture(
        filename="30-remotecompute-keystoneendpoint-plaintext.yaml",
        comment=(
            "spec.remoteCompute.keystoneEndpoint over plain http violates the ^https://\n"
            "pattern the field carries, a schema-level rejection: every compute on\n"
            "another cluster sends the nova service-user password to this URL. The\n"
            "webhook mirrors it with `must use scheme https`, but the schema answers\n"
            "first. The bus is verified, so the pattern is the only rule that fails."
        ),
        messaging=(
            "  messaging:\n"
            "    clusterRef:\n"
            "      name: rabbitmq\n"
            "    tls:\n"
            "      caBundleSecretRef:\n"
            "        name: nova-messaging-ca"
        ),
        extra=(
            "  remoteCompute:\n"
            "    keystoneEndpoint: http://keystone.example.com/v3\n"
            "    transportURLSecretRef:\n"
            "      name: nova-remote-transport\n"
        ),
    ),
    Fixture(
        filename="31-api-deployment-nodeselector-invalid-key.yaml",
        comment=(
            "spec.api.deployment.nodeSelector with the key \"bad key\" is not a qualified label\n"
            "name. The schema admits any string key on the map, so the validating\n"
            "webhook (validation.NodeSelectorLabels) is the only gate before the\n"
            "rendered pod template reaches the API server and is refused there."
        ),
        extra=(
            "  api:\n"
            "    deployment:\n"
            "      nodeSelector:\n"
            '        "bad key": x\n'
        ),
    ),
    Fixture(
        filename="32-jobs-resources-request-above-limit.yaml",
        comment=(
            "A spec.jobs.resources memory request above its limit is rejected by the\n"
            "validating webhook alone (validation.RequestsWithinLimits):\n"
            "ResourceRequirements is an embedded upstream type carrying no cross-field\n"
            "marker, so admission is the only gate before the rendered Job and CronJob\n"
            "pod templates reach the API server and are refused there."
        ),
        extra=(
            "  jobs:\n"
            "    resources:\n"
            "      requests:\n"
            "        memory: 1Gi\n"
            "      limits:\n"
            "        memory: 512Mi\n"
        ),
    ),
    Fixture(
        filename="33-api-deployment-toleration-empty-key-equal.yaml",
        comment=(
            "spec.api.deployment.tolerations[0] with no key and the operator Equal: an\n"
            "empty key only means \"match all keys\" with Exists. The schema has no rule\n"
            "for it on the embedded upstream Toleration, so the validating webhook\n"
            "(validation.Tolerations) is the only gate before the rendered pod template\n"
            "reaches the API server and is refused there."
        ),
        extra=(
            "  api:\n"
            "    deployment:\n"
            "      tolerations:\n"
            "      - operator: Equal\n"
            "        value: x\n"
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
        print("run `python3 tests/e2e/nova/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
