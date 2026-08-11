#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the placement invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Placement CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/glance/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

Most of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Each fixture
comment names the layer that answers it, and the matching Chainsaw step asserts
that layer's message.

There is no long-name fixture here. Glance needs one because its db-purge
CronJob leaves only 52 characters for the CR name; placement renders no CronJob,
so its webhook enforces no metadata.name bound at all (see the ValidateCreate
doc comment in operators/placement/api/v1alpha1/placement_webhook.go).

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

# Canonical valid Placement CR scaffold. Any future required field on
# PlacementSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {release} the whole openStackRelease line
# (empty for the fixture that omits the field, hence its position right at the
# start of the template line that carries `deployment:`), {deployment} the
# spec.deployment block body, {image} image block body, {database} database
# block body, {cache} cache block body, {endpoint} keystoneEndpoint value,
# {service_user} serviceUser block body, {extra} trailing spec additions.
SCAFFOLD = """\
apiVersion: placement.openstack.c5c3.io/v1alpha1
kind: Placement
metadata:
  name: {name}
spec:
{release}  deployment:
{deployment}
  image:
{image}
  database:
{database}
  cache:
{cache}
  keystoneEndpoint: {endpoint}
  serviceUser:
{service_user}
{extra}"""

VALID_RELEASE = "2025.2"

VALID_DEPLOYMENT = "    replicas: 1"

VALID_IMAGE = """\
    repository: ghcr.io/c5c3/placement
    tag: "2025.2\""""

VALID_DATABASE = """\
    clusterRef:
      name: openstack-db
    database: placement
    secretRef:
      name: placement-db"""

VALID_CACHE = """\
    clusterRef:
      name: openstack-memcached"""

VALID_ENDPOINT = "http://keystone.openstack.svc.cluster.local:5000/v3"

VALID_SERVICE_USER = """\
    secretRef:
      name: placement-service-password"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    # None omits the openStackRelease line entirely (the required-field fixture).
    release: str | None = VALID_RELEASE
    deployment: str = VALID_DEPLOYMENT
    image: str = VALID_IMAGE
    database: str = VALID_DATABASE
    cache: str = VALID_CACHE
    endpoint: str = VALID_ENDPOINT
    service_user: str = VALID_SERVICE_USER
    extra: str = ""

    def render(self) -> str:
        release = "" if self.release is None else f'  openStackRelease: "{self.release}"\n'
        body = SCAFFOLD.format(
            name=self.name,
            release=release,
            deployment=self.deployment,
            image=self.image,
            database=self.database,
            cache=self.cache,
            endpoint=self.endpoint,
            service_user=self.service_user,
            extra=self.extra,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-openstackrelease-missing.yaml",
        comment=(
            "spec.openStackRelease omitted. What answers is the CRD pattern, not the\n"
            "required-properties list: the defaulting webhook marshals the whole typed\n"
            "object back into its admission patch, and PlacementSpec.OpenStackRelease\n"
            "carries no omitempty tag, so the patch materializes the field as an empty\n"
            "string. By the time the schema is checked the property is present and the\n"
            "empty value fails the pattern. The fixture still earns its place: it pins\n"
            "that no layer invents a release for a CR that names none."
        ),
        name="placement-invalid-release-missing",
        release=None,
    ),
    Fixture(
        filename="01-openstackrelease-pattern.yaml",
        comment=(
            "spec.openStackRelease with a non-cadence minor violates the CRD pattern\n"
            "(^\\d{4}\\.[12]$), a schema-level rejection the API server answers before\n"
            "the validating webhook runs. 2025.9 is deliberately well-formed apart\n"
            "from the minor: it pins the [12] class rather than the digit count."
        ),
        name="placement-invalid-release-pattern",
        release="2025.9",
    ),
    Fixture(
        filename="02-replicas-below-minimum.yaml",
        comment=(
            "spec.deployment.replicas below the CRD Minimum=1 marker. The value is -1\n"
            "rather than 0 because DeploymentSpec.Default() normalizes a zero to\n"
            "DefaultReplicas in the mutating webhook, which runs before schema\n"
            "validation — a CR asking for 0 replicas is admitted and scaled to 3. Only\n"
            "a negative count survives defaulting and reaches the Minimum marker. The\n"
            "keystone corpus makes the same choice for the same reason."
        ),
        name="placement-invalid-replicas",
        deployment="    replicas: -1",
    ),
    Fixture(
        filename="03-image-both-tag-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL rule\n"
            "(has(self.tag) != has(self.digest)); the webhook mirrors it with the same\n"
            "message."
        ),
        name="placement-invalid-image-both",
        image=(
            "    repository: ghcr.io/c5c3/placement\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="04-image-neither-tag-nor-digest.yaml",
        comment=(
            "spec.image with neither tag nor digest violates the same ImageSpec XOR CEL\n"
            "rule from the other side: a repository alone names no reproducible image,\n"
            "and the reconciler would render a bare repository reference that resolves\n"
            "to :latest. Both fields are omitted rather than set to an empty string,\n"
            "which the tag/digest patterns would reject first."
        ),
        name="placement-invalid-image-neither",
        image="    repository: ghcr.io/c5c3/placement",
    ),
    Fixture(
        filename="05-database-clusterref-and-host.yaml",
        comment=(
            "spec.database with both clusterRef and host violates the shared\n"
            "DatabaseSpec XOR CEL rule; the webhook mirrors it via DatabaseXOR."
        ),
        name="placement-invalid-database-both",
        database=(
            "    clusterRef:\n"
            "      name: openstack-db\n"
            "    database: placement\n"
            "    secretRef:\n"
            "      name: placement-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="06-database-neither-clusterref-nor-host.yaml",
        comment=(
            "spec.database with neither clusterRef nor host violates the same\n"
            "DatabaseSpec XOR CEL rule from the other side: the operator has no way to\n"
            "resolve a connection URL, so [placement_database] connection would name no\n"
            "server at all."
        ),
        name="placement-invalid-database-neither",
        database=(
            "    database: placement\n"
            "    secretRef:\n"
            "      name: placement-db"
        ),
    ),
    Fixture(
        filename="07-database-dynamic-without-clusterref.yaml",
        comment=(
            "spec.database.credentialsMode Dynamic without clusterRef violates the\n"
            "second DatabaseSpec CEL rule; the webhook mirrors it via\n"
            "DynamicCredentialsRequireClusterRef. Dynamic credentials are minted\n"
            "against a MariaDB CR the operator manages, so there is nothing to mint\n"
            "against in brownfield (host) mode. The host field keeps the XOR rule\n"
            "satisfied so this fixture reaches the credentialsMode rule."
        ),
        name="placement-invalid-database-dynamic",
        database=(
            "    host: mariadb.example.com\n"
            "    credentialsMode: Dynamic\n"
            "    database: placement\n"
            "    secretRef:\n"
            "      name: placement-db"
        ),
    ),
    Fixture(
        filename="08-cache-clusterref-and-servers.yaml",
        comment=(
            "spec.cache with both clusterRef and servers violates the shared CacheSpec\n"
            "XOR CEL rule; the webhook mirrors it via CacheXOR."
        ),
        name="placement-invalid-cache-both",
        cache=(
            "    clusterRef:\n"
            "      name: openstack-memcached\n"
            "    servers:\n"
            "    - memcached-0:11211"
        ),
    ),
    Fixture(
        filename="09-cache-neither-clusterref-nor-servers.yaml",
        comment=(
            "spec.cache with neither clusterRef nor servers violates the same CacheSpec\n"
            "XOR CEL rule from the other side. backend is spelled out because the CRD\n"
            "requires it and the defaulting webhook would otherwise be the only thing\n"
            "filling it, which would obscure what this fixture omits on purpose."
        ),
        name="placement-invalid-cache-neither",
        cache="    backend: dogpile.cache.pymemcache",
    ),
    Fixture(
        filename="10-cache-server-control-char.yaml",
        comment=(
            "A spec.cache.servers entry carrying a newline violates the items pattern\n"
            "(^[^\\n\\r]*$) on CacheSpec.Servers. The list renders verbatim into\n"
            "[keystone_authtoken] memcached_servers, so a newline would inject a whole\n"
            "config section past the (section, key)-keyed extraConfig gates, which\n"
            "inspect map structure and never look inside a value. The webhook repeats\n"
            "the check via CacheNoControlChars for objects that bypass the schema."
        ),
        name="placement-invalid-cache-control-char",
        cache=(
            "    servers:\n"
            '    - "memcached-0:11211\\n[api]\\nauth_strategy = noauth2"'
        ),
    ),
    Fixture(
        filename="11-secretstoreref-empty-name.yaml",
        comment=(
            "spec.secretStoreRef.name empty violates the MinLength=1 marker on the\n"
            "shared SecretStoreRefSpec. An unnamed store would leave every\n"
            "ExternalSecret and PushSecret the operator renders pointing at nothing.\n"
            "The webhook repeats it via validation.SecretStoreRef."
        ),
        name="placement-invalid-secretstoreref",
        extra=(
            "  secretStoreRef:\n"
            "    kind: SecretStore\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="12-keystone-endpoint-bad-scheme.yaml",
        comment=(
            "spec.keystoneEndpoint with a non-http(s) scheme violates the CRD pattern\n"
            "(^https?://); the webhook's validateEndpointURL mirrors it. The value is\n"
            "rendered as [keystone_authtoken] auth_url, which keystonemiddleware can\n"
            "only reach over http or https."
        ),
        name="placement-invalid-endpoint-scheme",
        endpoint="ftp://keystone.openstack.svc.cluster.local:5000/v3",
    ),
    Fixture(
        filename="13-keystone-public-endpoint-not-a-url.yaml",
        comment=(
            "spec.keystonePublicEndpoint with a non-numeric port is rejected by the\n"
            "validating webhook alone. The value clears the CRD pattern, which\n"
            "constrains the scheme only; url.Parse is what refuses the port, so\n"
            "validateEndpointURL is the sole gate. Left admitted, the string would\n"
            "reach clients as the [keystone_authtoken] www_authenticate_uri a 401\n"
            "points them at."
        ),
        name="placement-invalid-public-endpoint",
        extra="  keystonePublicEndpoint: http://keystone.example.com:not-a-port/v3\n",
    ),
    Fixture(
        filename="14-serviceuser-username-control-char.yaml",
        comment=(
            "spec.serviceUser.username carrying a newline is rejected by the validating\n"
            "webhook alone: the field has no schema marker, and it renders verbatim\n"
            "into [keystone_authtoken] username. A newline therefore injects the\n"
            "[api] auth_strategy line the extraConfig ownership gate exists to refuse.\n"
            "The username is set explicitly because the defaulting webhook fills it\n"
            "only when empty."
        ),
        name="placement-invalid-username-control-char",
        service_user=(
            '    username: "placement\\n[api]\\nauth_strategy = noauth2"\n'
            "    secretRef:\n"
            "      name: placement-service-password"
        ),
    ),
    Fixture(
        filename="15-logging-level-invalid.yaml",
        comment=(
            "spec.logging.level outside the CRD enum (DEBUG, INFO, WARNING, ERROR,\n"
            "CRITICAL). TRACE is a level oslo.log does not define, so the schema enum\n"
            "answers first and the webhook's NotSupported twin never runs."
        ),
        name="placement-invalid-logging-level",
        extra=(
            "  logging:\n"
            "    level: TRACE\n"
        ),
    ),
    Fixture(
        filename="16-logging-per-logger-level-invalid.yaml",
        comment=(
            "A spec.logging.perLoggerLevels value outside the accepted set. A CRD enum\n"
            "on additionalProperties is not expressible, so the constraint is written\n"
            "as an `in [...]` CEL rule on the map; the webhook repeats it per entry.\n"
            "The name is a real oslo.log logger so the fixture violates the value rule\n"
            "and nothing else."
        ),
        name="placement-invalid-per-logger-level",
        extra=(
            "  logging:\n"
            "    perLoggerLevels:\n"
            "      sqlalchemy.engine: TRACE\n"
        ),
    ),
    Fixture(
        filename="17-termination-grace-below-minimum.yaml",
        comment=(
            "spec.deployment.terminationGracePeriodSeconds below the CRD Minimum=10\n"
            "marker. preStopSleepSeconds is pinned to 0 so the drain-window CEL rule on\n"
            "DeploymentSpec stays satisfied (0 < 9) and the Minimum marker is the only\n"
            "rule this fixture breaks; left unset it would resolve to 5 and break that\n"
            "rule too."
        ),
        name="placement-invalid-grace-period",
        deployment=(
            "    replicas: 1\n"
            "    terminationGracePeriodSeconds: 9\n"
            "    preStopSleepSeconds: 0"
        ),
    ),
    Fixture(
        filename="18-prestop-not-below-grace.yaml",
        comment=(
            "spec.deployment.preStopSleepSeconds equal to terminationGracePeriodSeconds\n"
            "violates the drain-window CEL rule on DeploymentSpec: the pod would spend\n"
            "its entire grace period asleep in the preStop hook and be SIGKILLed with\n"
            "requests still in flight. The webhook repeats the rule with the two\n"
            "resolved numbers in its message."
        ),
        name="placement-invalid-prestop",
        deployment=(
            "    replicas: 1\n"
            "    terminationGracePeriodSeconds: 30\n"
            "    preStopSleepSeconds: 30"
        ),
    ),
    Fixture(
        filename="19-harakiri-not-below-drain-window.yaml",
        comment=(
            "spec.apiServer.uwsgi.harakiri equal to the drain window\n"
            "(terminationGracePeriodSeconds - preStopSleepSeconds = 25) is rejected by\n"
            "the validating webhook alone. The rule correlates a uWSGI field with two\n"
            "spec.deployment fields, which no marker can express. A harakiri that does\n"
            "not fit inside the window lets uWSGI kill a request after SIGKILL has\n"
            "already taken the pod. The grace and preStop values are spelled out so the\n"
            "arithmetic in the error message does not depend on the webhook's defaults."
        ),
        name="placement-invalid-harakiri",
        deployment=(
            "    replicas: 1\n"
            "    terminationGracePeriodSeconds: 30\n"
            "    preStopSleepSeconds: 5"
        ),
        extra=(
            "  apiServer:\n"
            "    uwsgi:\n"
            "      harakiri: 25\n"
        ),
    ),
    Fixture(
        filename="20-uwsgi-keepalive-timeout-without-keepalive.yaml",
        comment=(
            "spec.apiServer.uwsgi.httpKeepAliveTimeout set while httpKeepAlive is false\n"
            "violates the UWSGISpec CEL rule: the timeout flag is only emitted under\n"
            "--http-keepalive. httpKeepAlive is set EXPLICITLY to false because the\n"
            "defaulting webhook restores true when the pointer is nil, which would\n"
            "satisfy the rule and make this CR admissible."
        ),
        name="placement-invalid-uwsgi-keepalive",
        extra=(
            "  apiServer:\n"
            "    uwsgi:\n"
            "      httpKeepAlive: false\n"
            "      httpKeepAliveTimeout: 30\n"
        ),
    ),
    Fixture(
        filename="21-strategy-recreate-with-rollingupdate.yaml",
        comment=(
            "spec.deployment.strategy of type Recreate carrying a rollingUpdate block\n"
            "is rejected by the validating webhook alone. DeploymentStrategy is an\n"
            "embedded upstream type with no CEL rule of its own here, so admission is\n"
            "the only gate before the Deployment controller refuses the child object\n"
            "and the CR stalls with a rendered workload it cannot apply."
        ),
        name="placement-invalid-strategy",
        deployment=(
            "    replicas: 1\n"
            "    strategy:\n"
            "      type: Recreate\n"
            "      rollingUpdate:\n"
            "        maxUnavailable: 1"
        ),
    ),
    Fixture(
        filename="22-autoscaling-min-above-max.yaml",
        comment=(
            "spec.autoscaling.minReplicas above maxReplicas violates the shared\n"
            "AutoscalingSpec CEL rule; the webhook mirrors it. The rule is declared on\n"
            "the type, so the API server reports it at the parent path spec.autoscaling\n"
            "rather than at the minReplicas field."
        ),
        name="placement-invalid-autoscaling-range",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 5\n"
            "    maxReplicas: 2\n"
            "    targetCPUUtilization: 80\n"
        ),
    ),
    Fixture(
        filename="23-autoscaling-cpu-utilization-above-max.yaml",
        comment=(
            "spec.autoscaling.targetCPUUtilization above the CRD Maximum=100 marker.\n"
            "The field is a utilization percentage of the container's CPU request, so\n"
            "150 asks the HPA to hold pods at a level the request cannot express. The\n"
            "replica bounds are kept consistent so the marker is the only rule broken."
        ),
        name="placement-invalid-autoscaling-cpu",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 1\n"
            "    maxReplicas: 3\n"
            "    targetCPUUtilization: 150\n"
        ),
    ),
    Fixture(
        filename="24-networkpolicy-empty-ingress.yaml",
        comment=(
            "spec.networkPolicy with an empty ingress list violates the CEL rule on\n"
            "NetworkPolicySpec; the webhook mirrors it. A NetworkPolicy with no ingress\n"
            "source denies all inbound traffic, so the API would be unreachable while\n"
            "every readiness probe still passed. The rule is declared on the type, so\n"
            "the API server reports it at spec.networkPolicy, not at .ingress."
        ),
        name="placement-invalid-networkpolicy",
        extra=(
            "  networkPolicy:\n"
            "    ingress: []\n"
        ),
    ),
    Fixture(
        filename="25-gateway-empty-hostname.yaml",
        comment=(
            "spec.gateway.hostname empty violates the MinLength=1 marker on the shared\n"
            "GatewaySpec. An HTTPRoute without a hostname would attach to the Gateway\n"
            "and match every request reaching its listener. The webhook repeats the\n"
            "check as field.Required."
        ),
        name="placement-invalid-gateway",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            "      name: openstack-gw\n"
            '    hostname: ""\n'
        ),
    ),
    Fixture(
        filename="26-extraconfig-empty-section.yaml",
        comment=(
            "spec.extraConfig with an empty section name is rejected by the validating\n"
            "webhook — extraConfig is a preserve-unknown-fields map, so CEL cannot\n"
            "constrain its keys and the webhook is the sole gate. A nameless section\n"
            "would render as a bare [] line in placement.conf."
        ),
        name="placement-invalid-extraconfig-section",
        extra=(
            "  extraConfig:\n"
            '    "":\n'
            "      foo: bar\n"
        ),
    ),
    Fixture(
        filename="27-extraconfig-empty-key.yaml",
        comment=(
            "spec.extraConfig with an empty option key is rejected by the validating\n"
            "webhook — a bare `= value` line must never reach the rendered\n"
            "placement.conf."
        ),
        name="placement-invalid-extraconfig-key",
        extra=(
            "  extraConfig:\n"
            "    placement:\n"
            '      "": bar\n'
        ),
    ),
    Fixture(
        filename="28-extraconfig-value-control-char.yaml",
        comment=(
            "spec.extraConfig carrying a newline in an option VALUE is rejected by the\n"
            "validating webhook. The rendered INI writes `key = value` verbatim, so the\n"
            "newline would inject a whole [api] auth_strategy line past the ownership\n"
            "and catalog gates — they key on (section, key) names and never look inside\n"
            "a value. The section and key here are both catalog-known and unowned,\n"
            "which is exactly the shape that would otherwise be admitted."
        ),
        name="placement-invalid-extraconfig-value-control-char",
        extra=(
            "  extraConfig:\n"
            "    placement:\n"
            '      randomize_allocation_candidates: "true\\n[api]\\nauth_strategy = noauth2"\n'
        ),
    ),
    Fixture(
        filename="29-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in the known [placement]\n"
            "section is rejected by the validating webhook: the singular spelling is\n"
            "absent from the placement 2025.2 option catalog, so a typo'd key can never\n"
            "silently reach the rendered placement.conf. The value is quoted because\n"
            "extraConfig is a map of string to string: a bare YAML boolean would draw a\n"
            "schema type error and never reach the catalog check."
        ),
        name="placement-invalid-extraconfig-unknown-option",
        extra=(
            "  extraConfig:\n"
            "    placement:\n"
            '      randomize_allocation_candidate: "true"\n'
        ),
    ),
    Fixture(
        filename="30-extraconfig-unknown-section.yaml",
        comment=(
            "spec.extraConfig declaring an unknown section 'placemnt' (a typo for\n"
            "[placement]) is rejected by the validating webhook: the section is absent\n"
            "from the placement 2025.2 option catalog, so a typo'd section name can\n"
            "never silently reach the rendered placement.conf."
        ),
        name="placement-invalid-extraconfig-unknown-section",
        extra=(
            "  extraConfig:\n"
            "    placemnt:\n"
            '      randomize_allocation_candidates: "true"\n'
        ),
    ),
    Fixture(
        filename="31-extraconfig-owned-auth-strategy.yaml",
        comment=(
            "spec.extraConfig setting [api] auth_strategy is rejected by the validating\n"
            "webhook. extraConfig has the last word in the merge, so honoring noauth2\n"
            "would put the API on the no-auth middleware: every request\n"
            "unauthenticated, project and role taken from the x-auth-token header, and\n"
            "reachable from outside the cluster whenever spec.gateway is set. The\n"
            "damage lands the moment the pods load the rendered file, which is why the\n"
            "registry marks the key Rejected rather than Reported."
        ),
        name="placement-invalid-extraconfig-auth-strategy",
        extra=(
            "  extraConfig:\n"
            "    api:\n"
            "      auth_strategy: noauth2\n"
        ),
    ),
    Fixture(
        filename="32-extraconfig-owned-password.yaml",
        comment=(
            "spec.extraConfig setting [keystone_authtoken] password is rejected by the\n"
            "validating webhook. The operator owns it via spec.serviceUser.secretRef\n"
            "and the middleware reads it from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env\n"
            "override, so a file value is inert at runtime but would leak the service\n"
            "password into the namespace-readable ConfigMap — the second of the\n"
            "registry's Rejected entries. Reaching that error at all is the per-key\n"
            "registry exemption at work: password is in no catalog, so an unexempted\n"
            "key would draw the unknown-option verdict instead."
        ),
        name="placement-invalid-extraconfig-password",
        extra=(
            "  extraConfig:\n"
            "    keystone_authtoken:\n"
            "      password: s3cr3t\n"
        ),
    ),
    Fixture(
        filename="33-resources-request-above-limit.yaml",
        comment=(
            "A spec.deployment.resources memory request above its limit is rejected by\n"
            "the validating webhook alone: ResourceRequirements is an embedded upstream\n"
            "type carrying no cross-field marker, so admission is the only gate before\n"
            "the kubelet refuses to admit the pod. Both maps are non-empty, so the\n"
            "defaulting webhook leaves the block untouched."
        ),
        name="placement-invalid-resources",
        deployment=(
            "    replicas: 1\n"
            "    resources:\n"
            "      requests:\n"
            "        memory: 2Gi\n"
            "      limits:\n"
            "        memory: 1Gi"
        ),
    ),
    Fixture(
        filename="34-topologyspread-selector-mismatch.yaml",
        comment=(
            "A spec.deployment.topologySpreadConstraints selector that does not equal\n"
            "the Deployment's own selector labels (app.kubernetes.io/name=placement,\n"
            "app.kubernetes.io/instance=<CR name>) is rejected by the validating\n"
            "webhook via validation.TopologySpreadSelector. The constraint correlates\n"
            "with labels the operator derives from metadata.name, so no marker can\n"
            "express it. A selector matching nothing spreads no pods while reporting\n"
            "no error at all."
        ),
        name="placement-invalid-topologyspread",
        deployment=(
            "    replicas: 1\n"
            "    topologySpreadConstraints:\n"
            "    - maxSkew: 1\n"
            "      topologyKey: kubernetes.io/hostname\n"
            "      whenUnsatisfiable: DoNotSchedule\n"
            "      labelSelector:\n"
            "        matchLabels:\n"
            "          app: placement"
        ),
    ),
    Fixture(
        filename="35-targetclusterref-empty-name.yaml",
        comment=(
            "spec.targetClusterRef.name empty violates the MinLength=1 marker on the\n"
            "shared TargetClusterRefSpec. An unnamed target names no registered cluster,\n"
            "so the operator would have nowhere to place the CR's children. The webhook\n"
            "repeats it via validation.TargetClusterRef."
        ),
        name="placement-invalid-targetclusterref",
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
        print("run `python3 tests/e2e/placement/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
