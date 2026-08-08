#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the barbican invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid Barbican CR scaffold used by every
``invalid-cr`` rejection test, mirroring
``tests/e2e/placement/invalid-cr/_generate.py``. Each fixture mutates exactly
one aspect of the canonical scaffold so the surrounding CR passes validation for
every rule OTHER than the one under test, which makes the admission error
attributable to that single field.

Many of these rejections are answered by the CRD schema, not by the validating
webhook: the API server validates the object against the structural schema and
its CEL rules before it calls any validating webhook, so wherever both layers
carry the same rule the schema message is the one the user sees. Each fixture
comment names the layer that answers it, and the matching Chainsaw step asserts
that layer's message.

Two rule groups have no fixture here:

* The control-character guard is one loop over a table of free-string fields
  (spec.region, the four spec.serviceUser name fields, spec.gateway.hostname).
  Two entries are covered — serviceUser.username and gateway.hostname, which
  render into different sections and so carry different consequences — and the
  remaining entries are the same rule with a different path.
* spec.logging.perLoggerLevels rejects an empty logger name as well as an
  out-of-range level. The empty-name branch is the same CEL rule on the same
  map as the level branch, one predicate apart, so only the level is pinned.

Several bounds markers are the mirror image of one that is pinned: the Minimum
on preStopSleepSeconds and on the two autoscaling replica fields, and the
Maximum on targetMemoryUtilization. The corpus carries one fixture per marker
class rather than one per field.

Every fixture name stays within MaxBarbicanNameLength (43 characters). The bound
is checked on every create, so a longer name would put a second, unrelated error
next to the one the fixture exists to pin. The long-name fixture is the single
deliberate exception.

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

# Canonical valid Barbican CR scaffold. Any future required field on
# BarbicanSpec must be added below AND verified against every fixture.
# Placeholders: {name} CR name, {release} openStackRelease value, {deployment}
# the spec.deployment block body, {image} image block body, {database} database
# block body, {cache} cache block body, {endpoint} the whole keystoneEndpoint
# line (empty for the fixture that omits the field, hence its position right at
# the start of the template line that carries `serviceUser:`), {service_user}
# serviceUser block body, {extra} trailing spec additions.
#
# spec.cache.backend is left out although the CRD requires it: the defaulting
# webhook fills it before the schema is checked, exactly as the happy-path
# fixtures rely on.
SCAFFOLD = """\
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: Barbican
metadata:
  name: {name}
spec:
  openStackRelease: "{release}"
  deployment:
{deployment}
  image:
{image}
  database:
{database}
  cache:
{cache}
{endpoint}  serviceUser:
{service_user}
{extra}"""

VALID_RELEASE = "2025.2"

VALID_DEPLOYMENT = "    replicas: 1"

VALID_IMAGE = """\
    repository: ghcr.io/c5c3/barbican
    tag: "2025.2\""""

VALID_DATABASE = """\
    clusterRef:
      name: openstack-db
    database: barbican
    secretRef:
      name: barbican-db"""

VALID_CACHE = """\
    clusterRef:
      name: openstack-memcached"""

VALID_ENDPOINT = "http://keystone.openstack.svc.cluster.local:5000/v3"

VALID_SERVICE_USER = """\
    secretRef:
      name: barbican-service-password"""


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    release: str = VALID_RELEASE
    deployment: str = VALID_DEPLOYMENT
    image: str = VALID_IMAGE
    database: str = VALID_DATABASE
    cache: str = VALID_CACHE
    # None omits the keystoneEndpoint line entirely (the required-field fixture).
    endpoint: str | None = VALID_ENDPOINT
    service_user: str = VALID_SERVICE_USER
    extra: str = ""

    def render(self) -> str:
        endpoint = "" if self.endpoint is None else f"  keystoneEndpoint: {self.endpoint}\n"
        body = SCAFFOLD.format(
            name=self.name,
            release=self.release,
            deployment=self.deployment,
            image=self.image,
            database=self.database,
            cache=self.cache,
            endpoint=endpoint,
            service_user=self.service_user,
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
            "the validating webhook runs. 2025.9 is deliberately well-formed apart from\n"
            "the minor: it pins the [12] class rather than the digit count."
        ),
        name="barbican-invalid-release-pattern",
        release="2025.9",
    ),
    Fixture(
        filename="01-replicas-below-minimum.yaml",
        comment=(
            "spec.deployment.replicas below the CRD Minimum=1 marker. The value is -1\n"
            "rather than 0 because DeploymentSpec.Default() normalizes a zero to\n"
            "DefaultReplicas in the mutating webhook, which runs before schema\n"
            "validation — a CR asking for 0 replicas is admitted and scaled up. Only a\n"
            "negative count survives defaulting and reaches the Minimum marker."
        ),
        name="barbican-invalid-replicas",
        deployment="    replicas: -1",
    ),
    Fixture(
        filename="02-image-both-tag-digest.yaml",
        comment=(
            "spec.image with both tag and digest violates the ImageSpec XOR CEL rule\n"
            "(has(self.tag) != has(self.digest)); the webhook mirrors it with the same\n"
            "message."
        ),
        name="barbican-invalid-image-both",
        image=(
            "    repository: ghcr.io/c5c3/barbican\n"
            '    tag: "2025.2"\n'
            "    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        ),
    ),
    Fixture(
        filename="03-image-neither-tag-nor-digest.yaml",
        comment=(
            "spec.image with neither tag nor digest violates the same ImageSpec XOR CEL\n"
            "rule from the other side: a repository alone names no reproducible image,\n"
            "and the reconciler would render a bare repository reference that resolves\n"
            "to :latest. Both fields are omitted rather than set to an empty string,\n"
            "which the tag/digest patterns would reject first."
        ),
        name="barbican-invalid-image-neither",
        image="    repository: ghcr.io/c5c3/barbican",
    ),
    Fixture(
        filename="04-database-clusterref-and-host.yaml",
        comment=(
            "spec.database with both clusterRef and host violates the shared\n"
            "DatabaseSpec XOR CEL rule; the webhook mirrors it via DatabaseXOR."
        ),
        name="barbican-invalid-database-both",
        database=(
            "    clusterRef:\n"
            "      name: openstack-db\n"
            "    database: barbican\n"
            "    secretRef:\n"
            "      name: barbican-db\n"
            "    host: mariadb.example.com"
        ),
    ),
    Fixture(
        filename="05-database-neither-clusterref-nor-host.yaml",
        comment=(
            "spec.database with neither clusterRef nor host violates the same\n"
            "DatabaseSpec XOR CEL rule from the other side: the operator has no way to\n"
            "resolve a connection URL, so [database] connection would name no server at\n"
            "all."
        ),
        name="barbican-invalid-database-neither",
        database=(
            "    database: barbican\n"
            "    secretRef:\n"
            "      name: barbican-db"
        ),
    ),
    Fixture(
        filename="06-database-dynamic-without-clusterref.yaml",
        comment=(
            "spec.database.credentialsMode Dynamic without clusterRef violates the\n"
            "second DatabaseSpec CEL rule; the webhook mirrors it via\n"
            "DynamicCredentialsRequireClusterRef. Dynamic credentials are minted against\n"
            "a MariaDB CR the operator manages, so there is nothing to mint against in\n"
            "brownfield (host) mode. The host field keeps the XOR rule satisfied so this\n"
            "fixture reaches the credentialsMode rule."
        ),
        name="barbican-invalid-database-dynamic",
        database=(
            "    host: mariadb.example.com\n"
            "    credentialsMode: Dynamic\n"
            "    database: barbican\n"
            "    secretRef:\n"
            "      name: barbican-db"
        ),
    ),
    Fixture(
        filename="07-cache-clusterref-and-servers.yaml",
        comment=(
            "spec.cache with both clusterRef and servers violates the shared CacheSpec\n"
            "XOR CEL rule; the webhook mirrors it via CacheXOR."
        ),
        name="barbican-invalid-cache-both",
        cache=(
            "    clusterRef:\n"
            "      name: openstack-memcached\n"
            "    servers:\n"
            "    - memcached-0:11211"
        ),
    ),
    Fixture(
        filename="08-cache-neither-clusterref-nor-servers.yaml",
        comment=(
            "spec.cache with neither clusterRef nor servers violates the same CacheSpec\n"
            "XOR CEL rule from the other side. backend is spelled out because the CRD\n"
            "requires it and the defaulting webhook would otherwise be the only thing\n"
            "filling it, which would obscure what this fixture omits on purpose."
        ),
        name="barbican-invalid-cache-neither",
        cache="    backend: dogpile.cache.pymemcache",
    ),
    Fixture(
        filename="09-cache-server-control-char.yaml",
        comment=(
            "A spec.cache.servers entry carrying a newline violates the items pattern\n"
            "(^[^\\n\\r]*$) on CacheSpec.Servers. The list renders verbatim into\n"
            "[keystone_authtoken] memcached_servers, so a newline would inject a whole\n"
            "config section past the (section, key)-keyed extraConfig gates, which\n"
            "inspect map structure and never look inside a value. The webhook repeats\n"
            "the check via CacheNoControlChars for objects that bypass the schema."
        ),
        name="barbican-invalid-cache-control-char",
        cache=(
            "    servers:\n"
            '    - "memcached-0:11211\\n[DEFAULT]\\ndb_auto_create = true"'
        ),
    ),
    Fixture(
        filename="10-secretstoreref-empty-name.yaml",
        comment=(
            "spec.secretStoreRef.name empty violates the MinLength=1 marker on the\n"
            "shared SecretStoreRefSpec. An unnamed store would leave every\n"
            "ExternalSecret and PushSecret the operator renders pointing at nothing.\n"
            "The webhook repeats it via validation.SecretStoreRef."
        ),
        name="barbican-invalid-secretstoreref",
        extra=(
            "  secretStoreRef:\n"
            "    kind: SecretStore\n"
            '    name: ""\n'
        ),
    ),
    Fixture(
        filename="11-keystone-endpoint-missing.yaml",
        comment=(
            "spec.keystoneEndpoint omitted. What answers is the CRD schema, not the\n"
            "webhook's field.Required branch: the defaulting webhook marshals the whole\n"
            "typed object back into its admission patch, and BarbicanSpec.KeystoneEndpoint\n"
            "carries no omitempty tag, so the patch materializes the field as an empty\n"
            "string. By the time the schema is checked the property is present and the\n"
            "empty value fails the minLength and pattern guards. The fixture still earns\n"
            "its place: it pins that no layer invents a Keystone URL for a CR that names\n"
            "none."
        ),
        name="barbican-invalid-endpoint-missing",
        endpoint=None,
    ),
    Fixture(
        filename="12-keystone-endpoint-bad-scheme.yaml",
        comment=(
            "spec.keystoneEndpoint with a non-http(s) scheme violates the CRD pattern\n"
            "(^https?://); the webhook's validateEndpointURL mirrors it. The value is\n"
            "rendered as [keystone_authtoken] auth_url, which keystonemiddleware can\n"
            "only reach over http or https."
        ),
        name="barbican-invalid-endpoint-scheme",
        endpoint="ftp://keystone.openstack.svc.cluster.local:5000/v3",
    ),
    Fixture(
        filename="13-keystone-public-endpoint-not-a-url.yaml",
        comment=(
            "spec.keystonePublicEndpoint with a non-numeric port is rejected by the\n"
            "validating webhook alone. The value clears the CRD pattern, which\n"
            "constrains the scheme only; url.Parse is what refuses the port, so\n"
            "validateEndpointURL is the sole gate. Left admitted, the string would reach\n"
            "clients as the [keystone_authtoken] www_authenticate_uri a 401 points them\n"
            "at."
        ),
        name="barbican-invalid-public-endpoint",
        extra="  keystonePublicEndpoint: http://keystone.example.com:not-a-port/v3\n",
    ),
    Fixture(
        filename="14-serviceuser-username-control-char.yaml",
        comment=(
            "spec.serviceUser.username carrying a newline is rejected by the validating\n"
            "webhook alone: the field has no schema marker, and it renders verbatim into\n"
            "[keystone_authtoken] username. A newline therefore injects the\n"
            "db_auto_create line the extraConfig ownership gate exists to refuse. The\n"
            "username is set explicitly because the defaulting webhook fills it only\n"
            "when empty."
        ),
        name="barbican-invalid-username-control-char",
        service_user=(
            '    username: "barbican\\n[DEFAULT]\\ndb_auto_create = true"\n'
            "    secretRef:\n"
            "      name: barbican-service-password"
        ),
    ),
    Fixture(
        filename="15-gateway-hostname-control-char.yaml",
        comment=(
            "spec.gateway.hostname carrying a newline is rejected by the validating\n"
            "webhook alone: the MinLength marker bounds the length, not the content. The\n"
            "hostname reaches the renderer as [DEFAULT] host_href, and [DEFAULT] is\n"
            "written first, so an injected line lands in a section the operator writes\n"
            "nothing else into and nothing later overrides it. The HTTPRoute step would\n"
            "refuse the hostname, but it runs after the config Secret has been written\n"
            "and mounted."
        ),
        name="barbican-invalid-gateway-control-char",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            "      name: openstack-gw\n"
            '    hostname: "barbican.example.com\\n[DEFAULT]\\ndb_auto_create = true"\n'
        ),
    ),
    Fixture(
        filename="16-logging-format-invalid.yaml",
        comment=(
            "spec.logging.format outside the CRD enum (text, json). oslo.log renders\n"
            "either a plain line or a JSON document, so there is no third format to\n"
            "select; the schema enum answers and the webhook's NotSupported twin never\n"
            "runs."
        ),
        name="barbican-invalid-logging-format",
        extra=(
            "  logging:\n"
            "    format: yaml\n"
        ),
    ),
    Fixture(
        filename="17-logging-level-invalid.yaml",
        comment=(
            "spec.logging.level outside the CRD enum (DEBUG, INFO, WARNING, ERROR,\n"
            "CRITICAL). TRACE is a level oslo.log does not define, so the schema enum\n"
            "answers first and the webhook's NotSupported twin never runs."
        ),
        name="barbican-invalid-logging-level",
        extra=(
            "  logging:\n"
            "    level: TRACE\n"
        ),
    ),
    Fixture(
        filename="18-logging-per-logger-level-invalid.yaml",
        comment=(
            "A spec.logging.perLoggerLevels value outside the accepted set. A CRD enum on\n"
            "additionalProperties is not expressible, so the constraint is written as an\n"
            "`in [...]` CEL rule on the map; the webhook repeats it per entry. The name is\n"
            "a real oslo.log logger so the fixture violates the value rule and nothing\n"
            "else."
        ),
        name="barbican-invalid-per-logger-level",
        extra=(
            "  logging:\n"
            "    perLoggerLevels:\n"
            "      sqlalchemy.engine: TRACE\n"
        ),
    ),
    Fixture(
        filename="19-termination-grace-below-minimum.yaml",
        comment=(
            "spec.deployment.terminationGracePeriodSeconds below the CRD Minimum=10\n"
            "marker. preStopSleepSeconds is pinned to 0 so the drain-window CEL rule on\n"
            "DeploymentSpec stays satisfied (0 < 9) and the Minimum marker is the only\n"
            "rule this fixture breaks; left unset it would resolve to 5 and break that\n"
            "rule too."
        ),
        name="barbican-invalid-grace-period",
        deployment=(
            "    replicas: 1\n"
            "    terminationGracePeriodSeconds: 9\n"
            "    preStopSleepSeconds: 0"
        ),
    ),
    Fixture(
        filename="20-prestop-not-below-grace.yaml",
        comment=(
            "spec.deployment.preStopSleepSeconds equal to terminationGracePeriodSeconds\n"
            "violates the drain-window CEL rule on DeploymentSpec: the pod would spend\n"
            "its entire grace period asleep in the preStop hook and be SIGKILLed with\n"
            "requests still in flight. The webhook repeats the rule with the two resolved\n"
            "numbers in its message."
        ),
        name="barbican-invalid-prestop",
        deployment=(
            "    replicas: 1\n"
            "    terminationGracePeriodSeconds: 30\n"
            "    preStopSleepSeconds: 30"
        ),
    ),
    Fixture(
        filename="21-harakiri-not-below-drain-window.yaml",
        comment=(
            "spec.apiServer.uwsgi.harakiri equal to the drain window\n"
            "(terminationGracePeriodSeconds - preStopSleepSeconds = 25) is rejected by\n"
            "the validating webhook alone. The rule correlates a uWSGI field with two\n"
            "spec.deployment fields, which no marker can express. A harakiri that does\n"
            "not fit inside the window lets uWSGI kill a request after SIGKILL has\n"
            "already taken the pod. The grace and preStop values are spelled out so the\n"
            "arithmetic in the error message does not depend on the webhook's defaults."
        ),
        name="barbican-invalid-harakiri",
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
        filename="22-uwsgi-keepalive-timeout-without-keepalive.yaml",
        comment=(
            "spec.apiServer.uwsgi.httpKeepAliveTimeout set while httpKeepAlive is false\n"
            "violates the UWSGISpec CEL rule: the timeout flag is only emitted under\n"
            "--http-keepalive. httpKeepAlive is set EXPLICITLY to false because the\n"
            "defaulting webhook restores true when the pointer is nil, which would\n"
            "satisfy the rule and make this CR admissible."
        ),
        name="barbican-invalid-uwsgi-keepalive",
        extra=(
            "  apiServer:\n"
            "    uwsgi:\n"
            "      httpKeepAlive: false\n"
            "      httpKeepAliveTimeout: 30\n"
        ),
    ),
    Fixture(
        filename="23-strategy-recreate-with-rollingupdate.yaml",
        comment=(
            "spec.deployment.strategy of type Recreate carrying a rollingUpdate block is\n"
            "rejected by the validating webhook alone. DeploymentStrategy is an embedded\n"
            "upstream type with no CEL rule of its own here, so admission is the only\n"
            "gate before the Deployment controller refuses the child object and the CR\n"
            "stalls with a rendered workload it cannot apply."
        ),
        name="barbican-invalid-strategy",
        deployment=(
            "    replicas: 1\n"
            "    strategy:\n"
            "      type: Recreate\n"
            "      rollingUpdate:\n"
            "        maxUnavailable: 1"
        ),
    ),
    Fixture(
        filename="24-autoscaling-min-above-max.yaml",
        comment=(
            "spec.autoscaling.minReplicas above maxReplicas violates the shared\n"
            "AutoscalingSpec CEL rule; the webhook mirrors it. The rule is declared on\n"
            "the type, so the API server reports it at the parent path spec.autoscaling\n"
            "rather than at the minReplicas field."
        ),
        name="barbican-invalid-autoscaling-range",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 5\n"
            "    maxReplicas: 2\n"
            "    targetCPUUtilization: 80\n"
        ),
    ),
    Fixture(
        filename="25-autoscaling-no-target.yaml",
        comment=(
            "spec.autoscaling with neither targetCPUUtilization nor\n"
            "targetMemoryUtilization violates the second AutoscalingSpec CEL rule; the\n"
            "webhook mirrors it as field.Required on the block. An HPA with no metric\n"
            "has nothing to scale on, so it would hold the replica count wherever it\n"
            "found it."
        ),
        name="barbican-invalid-autoscaling-no-target",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 1\n"
            "    maxReplicas: 3\n"
        ),
    ),
    Fixture(
        filename="26-autoscaling-cpu-utilization-above-max.yaml",
        comment=(
            "spec.autoscaling.targetCPUUtilization above the CRD Maximum=100 marker. The\n"
            "field is a utilization percentage of the container's CPU request, so 150\n"
            "asks the HPA to hold pods at a level the request cannot express. The replica\n"
            "bounds are kept consistent so the marker is the only rule broken."
        ),
        name="barbican-invalid-autoscaling-cpu",
        extra=(
            "  autoscaling:\n"
            "    minReplicas: 1\n"
            "    maxReplicas: 3\n"
            "    targetCPUUtilization: 150\n"
        ),
    ),
    Fixture(
        filename="27-autoscaling-max-below-replicas.yaml",
        comment=(
            "spec.autoscaling.maxReplicas below spec.deployment.replicas while\n"
            "minReplicas is unset is rejected by the validating webhook alone: the\n"
            "reconciler resolves an absent minReplicas to spec.deployment.replicas, so\n"
            "the rendered HPA would carry minReplicas 3 against maxReplicas 2 and the\n"
            "API server would refuse it. The rule correlates two top-level spec fields,\n"
            "which no marker on AutoscalingSpec can reach."
        ),
        name="barbican-invalid-autoscaling-implicit-min",
        deployment="    replicas: 3",
        extra=(
            "  autoscaling:\n"
            "    maxReplicas: 2\n"
            "    targetCPUUtilization: 80\n"
        ),
    ),
    Fixture(
        filename="28-networkpolicy-empty-ingress.yaml",
        comment=(
            "spec.networkPolicy with an empty ingress list violates the CEL rule on\n"
            "NetworkPolicySpec; the webhook mirrors it. A NetworkPolicy with no ingress\n"
            "source denies all inbound traffic, so the API would be unreachable while\n"
            "every readiness probe still passed. The rule is declared on the type, so the\n"
            "API server reports it at spec.networkPolicy, not at .ingress."
        ),
        name="barbican-invalid-networkpolicy",
        extra=(
            "  networkPolicy:\n"
            "    ingress: []\n"
        ),
    ),
    Fixture(
        filename="29-gateway-empty-hostname.yaml",
        comment=(
            "spec.gateway.hostname empty violates the MinLength=1 marker on the shared\n"
            "GatewaySpec. An HTTPRoute without a hostname would attach to the Gateway and\n"
            "match every request reaching its listener. The webhook repeats the check as\n"
            "field.Required."
        ),
        name="barbican-invalid-gateway-hostname",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            "      name: openstack-gw\n"
            '    hostname: ""\n'
        ),
    ),
    Fixture(
        filename="30-gateway-empty-parentref-name.yaml",
        comment=(
            "spec.gateway.parentRef.name empty violates the MinLength=1 marker on\n"
            "GatewayParentRefSpec. An HTTPRoute has to name the Gateway it attaches to;\n"
            "an unnamed parent leaves the route unattached and the API unreachable from\n"
            "outside. The webhook repeats the check as field.Required."
        ),
        name="barbican-invalid-gateway-parentref",
        extra=(
            "  gateway:\n"
            "    parentRef:\n"
            '      name: ""\n'
            "    hostname: barbican.example.com\n"
        ),
    ),
    Fixture(
        filename="31-policyoverrides-no-source.yaml",
        comment=(
            "spec.policyOverrides set but empty violates the CEL rule on the field: an\n"
            "override block has to carry either inline rules or a configMapRef, or there\n"
            "is no policy document to render and the operator would still point\n"
            "[oslo_policy] policy_file at one."
        ),
        name="barbican-invalid-policyoverrides-empty",
        extra="  policyOverrides: {}\n",
    ),
    Fixture(
        filename="32-policyoverrides-empty-rule-value.yaml",
        comment=(
            "A spec.policyOverrides.rules entry with an empty value violates the CEL rule\n"
            "on the shared PolicySpec; the webhook mirrors it via\n"
            "policy.ValidatePolicyRules. oslo.policy reads an empty rule as 'allow\n"
            "everyone', which is the opposite of what spelling the rule out means. The\n"
            "rule is declared on the type, so the API server reports it at\n"
            "spec.policyOverrides rather than at the map entry."
        ),
        name="barbican-invalid-policyoverrides-rule-value",
        extra=(
            "  policyOverrides:\n"
            "    rules:\n"
            '      "secret:get": ""\n'
        ),
    ),
    Fixture(
        filename="33-dbclean-retentiondays-zero.yaml",
        comment=(
            "spec.dbClean.retentionDays of 0 violates the CRD Minimum=1 marker on\n"
            "DBCleanSpec; validateDBClean repeats it as defense in depth. A zero-day\n"
            "window would hard-delete the rows an in-flight request just soft-deleted."
        ),
        name="barbican-invalid-dbclean-retention",
        extra=(
            "  dbClean:\n"
            "    retentionDays: 0\n"
        ),
    ),
    Fixture(
        filename="34-dbclean-invalid-schedule.yaml",
        comment=(
            "spec.dbClean.schedule with a non-cron value is rejected by the validating\n"
            "webhook alone — the cron grammar has no CRD schema counterpart, because the\n"
            "accepted grammar includes descriptors such as @daily that no regex expresses\n"
            "without also rejecting valid expressions. DBCleanSpec.Schedule is therefore\n"
            "validated exclusively via validation.CronSchedule."
        ),
        name="barbican-invalid-dbclean-schedule",
        extra=(
            "  dbClean:\n"
            '    schedule: "totally-not-cron"\n'
        ),
    ),
    Fixture(
        filename="35-extraconfig-empty-section.yaml",
        comment=(
            "spec.extraConfig with an empty section name is rejected by the validating\n"
            "webhook — extraConfig is a free-form map of maps, so no marker reaches its\n"
            "keys and the webhook is the sole gate. A nameless section would render as a\n"
            "bare [] line in barbican.conf."
        ),
        name="barbican-invalid-cfg-empty-section",
        extra=(
            "  extraConfig:\n"
            '    "":\n'
            '      quota_secrets: "10"\n'
        ),
    ),
    Fixture(
        filename="36-extraconfig-section-control-char.yaml",
        comment=(
            "spec.extraConfig with a newline in a SECTION name is rejected by the\n"
            "validating webhook. The renderer writes '[%s]' verbatim, so the newline\n"
            "opens a second section — here [DEFAULT], whose db_auto_create the operator\n"
            "owns — from a place the ownership and catalog gates never look: they key on\n"
            "the section name as a whole."
        ),
        name="barbican-invalid-cfg-section-control-char",
        extra=(
            "  extraConfig:\n"
            '    "quotas\\n[DEFAULT]\\ndb_auto_create = true":\n'
            '      quota_secrets: "10"\n'
        ),
    ),
    Fixture(
        filename="37-extraconfig-empty-key.yaml",
        comment=(
            "spec.extraConfig with an empty option key is rejected by the validating\n"
            "webhook — a bare `= value` line must never reach the rendered\n"
            "barbican.conf."
        ),
        name="barbican-invalid-cfg-empty-key",
        extra=(
            "  extraConfig:\n"
            "    quotas:\n"
            '      "": "10"\n'
        ),
    ),
    Fixture(
        filename="38-extraconfig-value-control-char.yaml",
        comment=(
            "spec.extraConfig carrying a newline in an option VALUE is rejected by the\n"
            "validating webhook. The rendered INI writes `key = value` verbatim, so the\n"
            "newline would inject a whole [DEFAULT] db_auto_create line past the\n"
            "ownership and catalog gates — they key on (section, key) names and never\n"
            "look inside a value. The section and key here are both catalog-known and\n"
            "unowned, which is exactly the shape that would otherwise be admitted."
        ),
        name="barbican-invalid-cfg-value-control-char",
        extra=(
            "  extraConfig:\n"
            "    quotas:\n"
            '      quota_secrets: "10\\n[DEFAULT]\\ndb_auto_create = true"\n'
        ),
    ),
    Fixture(
        filename="39-extraconfig-unknown-option.yaml",
        comment=(
            "spec.extraConfig setting an unknown option in the known [quotas] section is\n"
            "rejected by the validating webhook: the singular spelling is absent from the\n"
            "barbican 2025.2 option catalog, so a typo'd key can never silently reach the\n"
            "rendered barbican.conf. The value is quoted because extraConfig is a map of\n"
            "string to string: a bare YAML integer would draw a schema type error and\n"
            "never reach the catalog check."
        ),
        name="barbican-invalid-cfg-unknown-option",
        extra=(
            "  extraConfig:\n"
            "    quotas:\n"
            '      quota_secret: "10"\n'
        ),
    ),
    Fixture(
        filename="40-extraconfig-unknown-section.yaml",
        comment=(
            "spec.extraConfig declaring an unknown section 'quota' (a typo for [quotas])\n"
            "is rejected by the validating webhook: the section is absent from the\n"
            "barbican 2025.2 option catalog, so a typo'd section name can never silently\n"
            "reach the rendered barbican.conf. The name is deliberately outside the\n"
            "'secretstore:' prefix the catalog scan exempts, which the per-store sections\n"
            "take their names from."
        ),
        name="barbican-invalid-cfg-unknown-section",
        extra=(
            "  extraConfig:\n"
            "    quota:\n"
            '      quota_secrets: "10"\n'
        ),
    ),
    Fixture(
        filename="41-extraconfig-owned-password.yaml",
        comment=(
            "spec.extraConfig setting [keystone_authtoken] password is rejected by the\n"
            "validating webhook. The operator owns it via spec.serviceUser.secretRef and\n"
            "the middleware reads it from the OS_KEYSTONE_AUTHTOKEN__PASSWORD env\n"
            "override, so a file value is inert at runtime but would copy the service\n"
            "password into the config Secret every API pod mounts — one of the three\n"
            "Rejected entries in the owned-key registry."
        ),
        name="barbican-invalid-cfg-owned-password",
        extra=(
            "  extraConfig:\n"
            "    keystone_authtoken:\n"
            "      password: s3cr3t\n"
        ),
    ),
    Fixture(
        filename="42-extraconfig-owned-root-token.yaml",
        comment=(
            "spec.extraConfig setting [vault_plugin] root_token_id is rejected by the\n"
            "validating webhook. The operator authenticates the vault plugin through a\n"
            "mount-scoped AppRole and never renders a root token; honoring the override\n"
            "would replace that credential with an unscoped one and write it in plain\n"
            "text into the config Secret, both done the moment the pods load the file.\n"
            "The option is in the catalog, so the registry's Rejected flag is what\n"
            "answers rather than the unknown-option scan."
        ),
        name="barbican-invalid-cfg-owned-root-token",
        extra=(
            "  extraConfig:\n"
            "    vault_plugin:\n"
            '      root_token_id: "hvs.example-root-token"\n'
        ),
    ),
    Fixture(
        filename="43-resources-request-above-limit.yaml",
        comment=(
            "A spec.deployment.resources memory request above its limit is rejected by\n"
            "the validating webhook alone: ResourceRequirements is an embedded upstream\n"
            "type carrying no cross-field marker, so admission is the only gate before\n"
            "the kubelet refuses to admit the pod. Both maps are non-empty, so the\n"
            "defaulting webhook leaves the block untouched."
        ),
        name="barbican-invalid-resources",
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
        filename="44-priorityclass-not-found.yaml",
        comment=(
            "spec.deployment.priorityClassName naming a PriorityClass that does not exist\n"
            "is rejected by the validating webhook alone, via\n"
            "validation.PriorityClassExists. The object is cluster-scoped, so no marker\n"
            "can express the reference; the webhook resolves it through the uncached\n"
            "API reader. A typo'd name would otherwise leave every pod unschedulable\n"
            "with the CR reporting nothing wrong."
        ),
        name="barbican-invalid-priorityclass",
        deployment=(
            "    replicas: 1\n"
            "    priorityClassName: barbican-no-such-priority-class"
        ),
    ),
    Fixture(
        filename="45-topologyspread-selector-mismatch.yaml",
        comment=(
            "A spec.deployment.topologySpreadConstraints selector that does not equal the\n"
            "Deployment's own selector labels (app.kubernetes.io/name=barbican,\n"
            "app.kubernetes.io/instance=<CR name>) is rejected by the validating webhook\n"
            "via validation.TopologySpreadSelector. The constraint correlates with labels\n"
            "the operator derives from metadata.name, so no marker can express it. A\n"
            "selector matching nothing spreads no pods while reporting no error at all."
        ),
        name="barbican-invalid-topologyspread",
        deployment=(
            "    replicas: 1\n"
            "    topologySpreadConstraints:\n"
            "    - maxSkew: 1\n"
            "      topologyKey: kubernetes.io/hostname\n"
            "      whenUnsatisfiable: DoNotSchedule\n"
            "      labelSelector:\n"
            "        matchLabels:\n"
            "          app: barbican"
        ),
    ),
    Fixture(
        filename="46-name-too-long-for-db-clean-cronjob.yaml",
        comment=(
            "A metadata.name of 44 characters, one past MaxBarbicanNameLength, is\n"
            "rejected by the validating webhook on CREATE: the {name}-db-clean CronJob\n"
            "would exceed the 52-character cap the API server enforces on CronJob names,\n"
            "so the CR would admit cleanly and then fail every reconcile. The rule runs\n"
            "in ValidateCreate only, so the finalizer-removal update never trips over it."
        ),
        name="barbican-invalid-name-above-the-db-clean-cap",
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
        print("run `python3 tests/e2e/barbican/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
