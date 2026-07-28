#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the c5c3 ControlPlane invalid-CR Chainsaw fixtures.

Single source of truth for the minimal ControlPlane scaffold used by every
``invalid-cr`` rejection (and acceptance-base) test, mirroring
``tests/e2e/keystone/invalid-cr/_generate.py`` and the leaner
``tests/e2e/horizon/invalid-cr/_generate.py``. Each fixture mutates exactly one
aspect of the canonical scaffold so the surrounding CR passes every rule OTHER
than the one under test, making the admission error attributable to that field.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test in
its own ephemeral namespace, which isolates the one-ControlPlane-per-namespace
webhook from the parallel c5c3 suites pinned to the shared ``openstack``
namespace, and makes the two accepted transition bases (which persist) coexist
without colliding.

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

# Canonical ControlPlane scaffold. Any future required field on ControlPlaneSpec
# must be added below AND verified against every fixture. Placeholders:
#   {name}                metadata.name
#   {global_extra_config} the spec.globalExtraConfig block (indent 2) or ""
#   {infrastructure}      the whole spec.infrastructure block (indent 2) or ""
#   {keystone}            the spec.services.keystone body (indent 6) or "" for nil
#   {horizon}             the spec.services.horizon entry (indent 4) or ""
#   {glance}              the spec.services.glance entry (indent 4) or ""
#
# korc.adminCredential.applicationCredential is intentionally omitted: the
# defaulting webhook materializes it (rotation.mode etc.) before the CRD's
# required-field check runs, exactly as the minimal managed fixtures rely on.
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: {name}
spec:
  openStackRelease: "2025.2"
{global_extra_config}{infrastructure}  services:
    keystone:
{keystone}{horizon}{glance}  korc:
    adminCredential:
      cloudCredentialsRef:
        cloudName: admin
      passwordSecretRef:
        name: external-admin
        key: password
{service_accounts}"""

# A valid External keystone service body (indent 6): the issue's sketch shape.
VALID_EXTERNAL_KEYSTONE = (
    "      mode: External\n"
    "      external:\n"
    "        authURL: https://keystone.example.com/v3\n"
)

# A valid brownfield infrastructure block (indent 2, trailing newline). Used by
# the Managed-mode fixtures and the transition bases so infrastructure is present
# where the mode requires it.
MANAGED_INFRA = (
    "  infrastructure:\n"
    "    database:\n"
    "      host: db.example.com\n"
    "      database: openstack\n"
    "      secretRef:\n"
    "        name: db-creds\n"
    "    cache:\n"
    "      backend: dogpile.cache.pymemcache\n"
    "      servers:\n"
    "      - mc:11211\n"
)


# A valid glance service body (indent 4): one S3 backend promoted to the default
# store. The three glance-block fixtures below mutate exactly one aspect of it,
# and the External-mode forbid fixture reuses it whole.
VALID_GLANCE = (
    "    glance:\n"
    "      backends:\n"
    "      - name: primary\n"
    "        type: S3\n"
    "        isDefault: true\n"
    "        s3:\n"
    "          endpoint: https://s3.example.com\n"
    "          bucket: images\n"
    "          credentialsSecretRef:\n"
    "            name: glance-s3-creds\n"
)


# A valid, MANAGED dedicated backing-services block for the Keystone service
# (indent 6, to be appended to a Managed keystone body). Every dedicated fixture
# below mutates exactly one aspect of it.
VALID_DEDICATED_KEYSTONE = (
    "      dedicatedBackingServices:\n"
    "        database:\n"
    "          clusterRef:\n"
    "            name: cp-dedicated-db\n"
    "          database: keystone\n"
    "          secretRef:\n"
    "            name: keystone-db\n"
    "        cache:\n"
    "          clusterRef:\n"
    "            name: cp-dedicated-cache\n"
    "          backend: dogpile.cache.pymemcache\n"
)


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or a transition base)."""

    filename: str
    comment: str
    name: str
    keystone: str = VALID_EXTERNAL_KEYSTONE
    infrastructure: str = ""
    horizon: str = ""
    glance: str = ""
    # The spec.globalExtraConfig block (indent 2, trailing newline) or "".
    global_extra_config: str = ""
    # The spec.korc.serviceAccounts block (indent 4, trailing newline) or "".
    service_accounts: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            global_extra_config=self.global_extra_config,
            infrastructure=self.infrastructure,
            keystone=self.keystone,
            horizon=self.horizon,
            glance=self.glance,
            service_accounts=self.service_accounts,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix (Test: c5c3-invalid-cr) ---
    Fixture(
        filename="19-federation-proxy-image-in-external.yaml",
        comment=(
            "services.keystone.federationProxyImage in External mode violates the CEL rule:\n"
            "no Keystone workload is deployed, so there is no sidecar to image."
        ),
        name="cp-external-proxy-image",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "      federationProxyImage:\n"
            "        repository: ghcr.io/c5c3/keystone-federation-proxy\n"
            "        tag: dev\n"
        ),
    ),
    Fixture(
        filename="20-horizon-public-endpoint-not-a-url.yaml",
        comment=(
            "services.horizon.publicEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern. Keystone matches the derived WebSSO origin verbatim, so an\n"
            "unusable endpoint could never match any dashboard."
        ),
        name="cp-horizon-bad-endpoint",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      publicEndpoint: ftp://horizon.example.com\n"
        ),
    ),
    Fixture(
        filename="21-horizon-gateway-hostname-wildcard.yaml",
        comment=(
            "services.horizon.gateway.hostname must be a concrete DNS name. Gateway API\n"
            "permits a wildcard here, but the reconciler derives the WebSSO origin from it\n"
            "and Keystone compares that origin verbatim, so a wildcard would match no\n"
            "dashboard and silently break every federated login."
        ),
        name="cp-horizon-wildcard-gateway",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      gateway:\n"
            "        hostname: '*.example.com'\n"
            "        parentRef:\n"
            "          name: openstack-gw\n"
        ),
    ),
    Fixture(
        filename="22-horizon-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.horizon.publicEndpoint must name the same host as\n"
            "services.horizon.gateway.hostname. Django derives the WebSSO origin it sends\n"
            "from the request Host header — i.e. from the gateway hostname — and Keystone\n"
            "compares it verbatim, so a divergent host is rejected only AFTER the user has\n"
            "already entered their corporate credentials at the identity provider."
        ),
        name="cp-horizon-endpoint-host-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      gateway:\n"
            "        hostname: horizon.example.com\n"
            "        parentRef:\n"
            "          name: openstack-gw\n"
            "      publicEndpoint: https://dashboard.example.com\n"
        ),
    ),
    Fixture(
        filename="23-horizon-public-endpoint-with-query.yaml",
        comment=(
            "services.horizon.publicEndpoint must be a bare origin. The ^https?:// CRD\n"
            "pattern anchors only the prefix, so a query string is schema-legal — and the\n"
            "derived origin https://horizon.example.com?utm=1/auth/websso/ is accepted by\n"
            "Keystone's own trusted_dashboard validation and then matches nothing, failing\n"
            "every federated login after the user has authenticated at the identity\n"
            "provider. Only the webhook rejects it."
        ),
        name="cp-horizon-endpoint-with-query",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      publicEndpoint: https://horizon.example.com?utm=1\n"
        ),
    ),
    Fixture(
        filename="00-external-in-managed-explicit.yaml",
        comment="services.keystone.external set with explicit mode: Managed violates the CEL rule.",
        name="cp-external-in-managed",
        keystone=(
            "      mode: Managed\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="01-external-in-managed-unset.yaml",
        comment=(
            "services.keystone.external set with mode unset (defaults Managed)\n"
            "violates the CEL rule after the mode default is applied."
        ),
        name="cp-external-unset-mode",
        keystone=(
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="02-external-mode-without-external.yaml",
        comment="mode: External without the external block violates the CEL rule.",
        name="cp-external-no-block",
        keystone="      mode: External\n",
    ),
    Fixture(
        filename="03-external-with-infrastructure.yaml",
        comment="spec.infrastructure set in External mode is forbidden by the webhook (cross-field).",
        name="cp-external-with-infra",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="04-external-with-horizon.yaml",
        comment="services.horizon set in External mode is forbidden by the webhook (P2, cross-field).",
        name="cp-external-with-horizon",
        horizon="    horizon: {}\n",
    ),
    Fixture(
        filename="05-external-replicas.yaml",
        comment="services.keystone.replicas is forbidden in External mode (CEL).",
        name="cp-external-replicas",
        keystone=VALID_EXTERNAL_KEYSTONE + "      replicas: 3\n",
    ),
    Fixture(
        filename="06-external-image.yaml",
        comment="services.keystone.image is forbidden in External mode (CEL).",
        name="cp-external-image",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      image:\n"
            + "        repository: ghcr.io/c5c3/keystone\n"
            + '        tag: "2025.2"\n'
        ),
    ),
    Fixture(
        filename="07-external-policy-overrides.yaml",
        comment="services.keystone.policyOverrides is forbidden in External mode (CEL).",
        name="cp-external-policy",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      policyOverrides:\n"
            + "        rules:\n"
            + '          "identity:get_user": "role:admin"\n'
        ),
    ),
    Fixture(
        filename="08-external-rotation-interval.yaml",
        comment="services.keystone.rotationInterval is forbidden in External mode (CEL).",
        name="cp-external-rotation",
        keystone=VALID_EXTERNAL_KEYSTONE + "      rotationInterval: 24h\n",
    ),
    Fixture(
        filename="09-external-gateway.yaml",
        comment="services.keystone.gateway is forbidden in External mode (CEL).",
        name="cp-external-gateway",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      gateway:\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "        hostname: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="10-external-public-endpoint.yaml",
        comment="services.keystone.publicEndpoint is forbidden in External mode (CEL, P2).",
        name="cp-external-public",
        keystone=VALID_EXTERNAL_KEYSTONE + "      publicEndpoint: https://keystone.example.com/v3\n",
    ),
    Fixture(
        filename="11-external-authurl-missing.yaml",
        comment="external without authURL violates the CRD required-field check.",
        name="cp-external-no-authurl",
        keystone=(
            "      mode: External\n"
            "      external: {}\n"
        ),
    ),
    Fixture(
        filename="12-external-authurl-not-url.yaml",
        comment="external.authURL without an http(s) scheme violates the CRD pattern.",
        name="cp-external-bad-authurl",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: keystone.example.com\n"
        ),
    ),
    Fixture(
        filename="13-external-endpoint-type-invalid.yaml",
        comment="external.endpointType outside the enum violates the CRD enum.",
        name="cp-external-bad-endpoint",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        endpointType: gopher\n"
        ),
    ),
    Fixture(
        filename="14-external-cabundle-empty-name.yaml",
        comment="external.caBundleSecretRef.name empty violates the SecretRefSpec MinLength marker.",
        name="cp-external-empty-ca",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "        caBundleSecretRef:\n"
            '          name: ""\n'
        ),
    ),
    # Catalog stewardship (still the create-rejection matrix). The numbering picks
    # up at 19 because 15-18 were already taken by the transition waves below.
    Fixture(
        filename="24-external-catalog-identity-entry.yaml",
        comment=(
            "external.catalog.managedEntries declaring the identity type is rejected:\n"
            "the identity catalog entry is owned by the External-mode imports (CEL)."
        ),
        name="cp-external-catalog-identity",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          managedEntries:\n"
            + "          - type: identity\n"
        ),
    ),
    Fixture(
        filename="25-external-catalog-entry-type-invalid.yaml",
        comment=(
            "external.catalog.managedEntries[].type outside the DNS-1123 label shape is\n"
            "rejected (CRD pattern): the type names the child K-ORC CRs."
        ),
        name="cp-external-catalog-bad-type",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          managedEntries:\n"
            + "          - type: Image_Service\n"
        ),
    ),
    Fixture(
        filename="26-external-catalog-endpoint-url-invalid.yaml",
        comment=(
            "external.catalog.managedEntries[].endpoints[].url without an http(s) scheme\n"
            "is rejected (CRD pattern)."
        ),
        name="cp-external-catalog-bad-url",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          managedEntries:\n"
            + "          - type: image\n"
            + "            endpoints:\n"
            + "            - interface: public\n"
            + "              url: glance.example.com\n"
        ),
    ),
    Fixture(
        filename="27-external-catalog-duplicate-interface.yaml",
        comment=(
            "Two endpoints of one managed entry sharing an interface are rejected by the\n"
            "apiserver: endpoints is a listType=map keyed on interface."
        ),
        name="cp-external-catalog-dup-iface",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          managedEntries:\n"
            + "          - type: image\n"
            + "            endpoints:\n"
            + "            - interface: public\n"
            + "              url: https://a.example.com\n"
            + "            - interface: public\n"
            + "              url: https://b.example.com\n"
        ),
    ),
    Fixture(
        filename="28-external-catalog-entry-name-invalid.yaml",
        comment=(
            "external.catalog.managedEntries[].name carrying a comma is rejected (CRD\n"
            "pattern): the name is cast to K-ORC's OpenStackName, whose own pattern is\n"
            "^[^,]+$, so admitting it here would wedge the reconcile on a K-ORC CRD\n"
            "rejection no ControlPlane field error explains."
        ),
        name="cp-external-catalog-bad-name",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          managedEntries:\n"
            + "          - type: image\n"
            + "            name: glance,v2\n"
        ),
    ),
    Fixture(
        filename="29-external-catalog-identity-service-name-invalid.yaml",
        comment=(
            "external.catalog.identityServiceName carrying a comma is rejected (CRD\n"
            "pattern), exactly like managedEntries[].name: it is cast to K-ORC's\n"
            "OpenStackName on the Service import filter, whose own pattern is ^[^,]+$."
        ),
        name="cp-external-catalog-bad-identity-name",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "        catalog:\n"
            + "          identityServiceName: keystone,v3\n"
        ),
    ),
    # Service accounts (still the create-rejection matrix). Mode-independent, so
    # they hang off the default External keystone base (no infrastructure needed).
    Fixture(
        filename="30-service-account-name-invalid.yaml",
        comment=(
            "spec.korc.serviceAccounts[].name outside the DNS-1123 label shape is\n"
            "rejected (CRD pattern): the name keys the list and names the child K-ORC\n"
            "CRs and Secrets."
        ),
        name="cp-sa-bad-name",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: Nova_Service\n"
            "      project:\n"
            "        name: service\n"
        ),
    ),
    Fixture(
        filename="31-service-account-missing-project-name.yaml",
        comment=(
            "spec.korc.serviceAccounts[].project.name is required (CRD required-field\n"
            "check and webhook): every account is associated with a project."
        ),
        name="cp-sa-no-project-name",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: nova\n"
            "      project: {}\n"
        ),
    ),
    Fixture(
        filename="32-service-account-admin-identity-collision.yaml",
        comment=(
            "A service account whose effective (userName, domainName) equals the admin\n"
            "identity is rejected by the webhook: a managed User would take over the\n"
            "admin user and rotate its password."
        ),
        name="cp-sa-admin-collision",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: nova\n"
            "      userName: admin\n"
            "      domainName: Default\n"
            "      project:\n"
            "        name: service\n"
        ),
    ),
    Fixture(
        filename="33-service-account-duplicate-identity.yaml",
        comment=(
            "Two service accounts resolving to the same (userName, domainName) are\n"
            "rejected by the webhook: they would project two managed Users onto one\n"
            "Keystone user and race its password."
        ),
        name="cp-sa-dup-identity",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: nova\n"
            "      project:\n"
            "        name: service\n"
            "    - name: nova-secondary\n"
            "      userName: nova\n"
            "      project:\n"
            "        name: service\n"
        ),
    ),
    Fixture(
        filename="34-service-account-duplicate-managed-project.yaml",
        comment=(
            "Two create:true service accounts naming the same project in one domain are\n"
            "rejected by the webhook: each managed Project would adopt the other's row."
        ),
        name="cp-sa-dup-managed-project",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: nova\n"
            "      project:\n"
            "        name: service\n"
            "        create: true\n"
            "    - name: glance\n"
            "      project:\n"
            "        name: service\n"
            "        create: true\n"
        ),
    ),
    Fixture(
        filename="46-service-account-target-namespace-unassigned.yaml",
        comment=(
            "spec.korc.serviceAccounts[].targetNamespace naming a namespace that is\n"
            "neither the ControlPlane's own nor one assigned to a service via\n"
            "spec.services.<svc>.namespace is rejected by the webhook: the consumer\n"
            "credentials Secret is delivered through that namespace's openbao-tenant-store,\n"
            "and none is provisioned for a namespace the ControlPlane neither owns nor\n"
            "placed a service in. The knob is inert this commit — the rule guards it before\n"
            "the delivery reconciler wiring lands."
        ),
        name="cp-sa-target-namespace-unassigned",
        service_accounts=(
            "    serviceAccounts:\n"
            "    - name: nova\n"
            "      targetNamespace: shared-services\n"
            "      project:\n"
            "        name: service\n"
        ),
    ),
    # Per-service dedicated backing services (still the create-rejection matrix).
    Fixture(
        filename="35-dedicated-in-external.yaml",
        comment=(
            "services.keystone.dedicatedBackingServices in External mode violates the CEL\n"
            "rule: an External ControlPlane provisions no backing services at all, so there\n"
            "is nothing to make dedicated."
        ),
        name="cp-dedicated-external",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      dedicatedBackingServices:\n"
            + "        cache:\n"
            + "          clusterRef:\n"
            + "            name: cp-dedicated-cache\n"
            + "          backend: dogpile.cache.pymemcache\n"
        ),
    ),
    Fixture(
        filename="36-dedicated-empty-block.yaml",
        comment=(
            "An empty dedicatedBackingServices block violates the CEL rule: it requests no\n"
            "backing-service class at all. Omit the block entirely to share the\n"
            "ControlPlane-wide instances (the default)."
        ),
        name="cp-dedicated-empty",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices: {}\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="37-dedicated-database-dynamic-credentials.yaml",
        comment=(
            "credentialsMode Dynamic on a DEDICATED database is rejected by the webhook: the\n"
            "OpenBao database engine carries one connection and one role per namespace,\n"
            "bootstrapped against the SHARED cluster, so no engine role can issue credentials\n"
            "for a dedicated instance — an admitted CR would wedge on an ExternalSecret that\n"
            "can never sync. The CRD CEL rule does not catch it (clusterRef IS set, so the\n"
            "Dynamic-requires-managed-mode rule passes)."
        ),
        name="cp-dedicated-dynamic",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          credentialsMode: Dynamic\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="38-dedicated-database-replicas-two.yaml",
        comment=(
            "A two-replica DEDICATED database is rejected by the webhook, exactly as a\n"
            "two-replica shared one is: the managed-MariaDB projection turns any replicas>1\n"
            "into a Galera cluster, and a two-node Galera cluster cannot hold a majority. The\n"
            "CRD marker only enforces Minimum=1, so the webhook is the enforcement point."
        ),
        name="cp-dedicated-replicas-two",
        keystone=(
            "      mode: Managed\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
            "          replicas: 2\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="39-dedicated-clusterref-collision.yaml",
        comment=(
            "Two services' dedicated caches naming the same managed clusterRef are rejected\n"
            "by the webhook: both would resolve to a single Memcached child CR that the two\n"
            "projections then fight over, silently voiding the very isolation the opt-in\n"
            "exists for."
        ),
        name="cp-dedicated-collision",
        keystone="      mode: Managed\n" + VALID_DEDICATED_KEYSTONE,
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      dedicatedBackingServices:\n"
            "        cache:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-cache\n"
            "          backend: dogpile.cache.pymemcache\n"
        ),
    ),
    # --- per-service Glance (issue #672, still the create-rejection matrix) ---
    Fixture(
        filename="47-external-with-glance.yaml",
        comment=(
            "services.glance set in External mode is forbidden by the webhook (cross-field,\n"
            "mirrors services.horizon): Glance needs its own External-mode design. The glance\n"
            "block itself is valid, so the ONLY violation is the cross-field forbid."
        ),
        name="cp-external-with-glance",
        glance=VALID_GLANCE,
    ),
    Fixture(
        filename="48-glance-backends-empty.yaml",
        comment=(
            "services.glance.backends with no entry violates the CRD MinItems floor: an empty\n"
            "list projects no GlanceBackend, so Glance has no image store at all."
        ),
        name="cp-glance-backends-empty",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends: []\n"
        ),
    ),
    Fixture(
        filename="49-glance-two-defaults.yaml",
        comment=(
            "Two backends both setting isDefault violate the single-default CEL rule: the\n"
            "Glance default_backend would be ambiguous. The webhook mirrors the rule."
        ),
        name="cp-glance-two-defaults",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3.example.com\n"
            "          bucket: images\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds\n"
            "      - name: secondary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3-2.example.com\n"
            "          bucket: images2\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds-2\n"
        ),
    ),
    Fixture(
        filename="50-glance-s3-block-missing.yaml",
        comment=(
            "A backend of type S3 without the s3 block violates the type/s3 union CEL rule:\n"
            "the driver parameters live in that block. The webhook mirrors the rule."
        ),
        name="cp-glance-s3-missing",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
        ),
    ),
    Fixture(
        filename="51-glance-public-endpoint-not-a-url.yaml",
        comment=(
            "services.glance.publicEndpoint with a non-http(s) scheme violates the CRD\n"
            "pattern. The value is advertised verbatim as the public image catalog\n"
            "Endpoint, so an unusable scheme could never serve external clients."
        ),
        name="cp-glance-bad-endpoint",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=VALID_GLANCE + "      publicEndpoint: ftp://glance.example.com\n",
    ),
    Fixture(
        filename="52-glance-public-endpoint-host-mismatch.yaml",
        comment=(
            "services.glance.publicEndpoint must name the same host as\n"
            "services.glance.gateway.hostname. The Gateway listener is what routes that\n"
            "hostname to the Glance API, so a divergent host advertises a catalog endpoint\n"
            "that never reaches it — and the value is projected into no child CR, so this\n"
            "webhook is the only gate on the URL every image client resolves."
        ),
        name="cp-glance-endpoint-host-mismatch",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      gateway:\n"
            + "        hostname: glance.example.com\n"
            + "        parentRef:\n"
            + "          name: openstack-gw\n"
            + "      publicEndpoint: https://images.example.com\n"
        ),
    ),
    Fixture(
        filename="53-glance-public-endpoint-with-query.yaml",
        comment=(
            "services.glance.publicEndpoint must be a bare origin. The ^https?:// pattern\n"
            "anchors only the prefix, so a query string is schema-legal — and since the\n"
            "Glance API is served at the root, clients append the API path to the catalog\n"
            "endpoint and 404 on every image call."
        ),
        name="cp-glance-endpoint-with-query",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=VALID_GLANCE + "      publicEndpoint: https://glance.example.com?utm=1\n",
    ),
    # --- per-service databaseCredentialsMode override (issue #683, still the
    #     create-rejection matrix) ---
    Fixture(
        filename="54-keystone-credentials-mode-override-in-external.yaml",
        comment=(
            "services.keystone.databaseCredentialsMode in External mode violates the CEL\n"
            "rule: no managed database is provisioned, so there is no credentials mode to\n"
            "override. A Static value is otherwise valid, so the ONLY violation is the\n"
            "External-mode forbid."
        ),
        name="cp-external-credentials-mode",
        keystone=VALID_EXTERNAL_KEYSTONE + "      databaseCredentialsMode: Static\n",
    ),
    Fixture(
        filename="55-keystone-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Keystone service is rejected by the\n"
            "webhook when Keystone declares a DEDICATED database: the override retargets the\n"
            "shared database this service does not use, and a dedicated database is\n"
            "Static-only. The CRD Enum admits the value (Static|Dynamic) and no CEL rule\n"
            "spans the dedicated block, so the webhook is the enforcement point."
        ),
        name="cp-override-dynamic-dedicated-ks",
        keystone=(
            "      mode: Managed\n"
            "      databaseCredentialsMode: Dynamic\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: keystone\n"
            "          secretRef:\n"
            "            name: keystone-db\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="56-glance-override-dynamic-on-dedicated.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on the Glance service is rejected by the webhook\n"
            "when Glance declares a DEDICATED database, mirroring the Keystone case: the\n"
            "override retargets the shared database this service does not use, and a\n"
            "dedicated database is Static-only. The defaulting webhook injects the glance\n"
            "service account, so the only violation is the credentials-mode override."
        ),
        name="cp-override-dynamic-dedicated-gl",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            "    glance:\n"
            "      databaseCredentialsMode: Dynamic\n"
            "      backends:\n"
            "      - name: primary\n"
            "        type: S3\n"
            "        isDefault: true\n"
            "        s3:\n"
            "          endpoint: https://s3.example.com\n"
            "          bucket: images\n"
            "          credentialsSecretRef:\n"
            "            name: glance-s3-creds\n"
            "      dedicatedBackingServices:\n"
            "        database:\n"
            "          clusterRef:\n"
            "            name: cp-dedicated-db\n"
            "          database: glance\n"
            "          secretRef:\n"
            "            name: glance-db\n"
        ),
    ),
    Fixture(
        filename="57-override-dynamic-on-brownfield.yaml",
        comment=(
            "databaseCredentialsMode Dynamic on a service using the SHARED database is\n"
            "rejected by the webhook when that shared database is brownfield (host set, no\n"
            "clusterRef), mirroring the commonv1 Dynamic-requires-clusterRef contract one\n"
            "level up: the dynamic engine issues per-tenant DB users only against a cluster\n"
            "the operator provisions. The CRD Enum admits the value and the keystone CEL rule\n"
            "forbids the field only in External mode, so the webhook is the enforcement point."
        ),
        name="cp-override-dynamic-brownfield",
        keystone=(
            "      mode: Managed\n"
            "      databaseCredentialsMode: Dynamic\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave C: shared -> dedicated (Test: c5c3-invalid-cr-shared-to-dedicated) ---
    Fixture(
        filename="40-transition-base-shared.yaml",
        comment=(
            "Accepted base for the shared->dedicated transition test: a Managed ControlPlane\n"
            "whose Keystone service shares the ControlPlane-wide backing services (the\n"
            "default — no dedicatedBackingServices block)."
        ),
        name="cp-transition-c",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="41-transition-to-dedicated.yaml",
        comment=(
            "UPDATE of the accepted shared base onto dedicated backing services is rejected:\n"
            "the flip would re-point the consuming child's (immutable) database fields at a\n"
            "different instance while the previously-provisioned one keeps running with the\n"
            "data still on it. The freeze is webhook-only — no CEL transition rule — so a\n"
            "later transition feature can relax it to a gated migration."
        ),
        name="cp-transition-c",
        keystone="      mode: Managed\n" + VALID_DEDICATED_KEYSTONE,
        infrastructure=MANAGED_INFRA,
    ),
    # --- per-service namespaces (issue #646) ---
    Fixture(
        filename="42-namespace-in-external-mode.yaml",
        comment=(
            "services.keystone.namespace in External mode violates the CEL rule: no Keystone\n"
            "workload is deployed, so there is nothing to place in a namespace of its own."
        ),
        name="cp-external-namespace",
        keystone=(
            "      mode: External\n"
            "      external:\n"
            "        authURL: https://keystone.example.com/v3\n"
            "      namespace:\n"
            "        name: identity\n"
        ),
    ),
    Fixture(
        filename="43-namespace-lifecycle-conflict.yaml",
        comment=(
            "Two services co-located in ONE namespace must agree on its lifecycle. They share\n"
            "that namespace's backing services and its tenant store, so they cannot disagree\n"
            "on whether the operator owns it: the Managed declaration would have the teardown\n"
            "delete the namespace the External one declared untouchable."
        ),
        name="cp-ns-lifecycle-conflict",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: shared-services\n"
            "        lifecycle: Managed\n"
        ),
        horizon=(
            "    horizon:\n"
            "      namespace:\n"
            "        name: shared-services\n"
            "        lifecycle: External\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave D: namespace assignment freeze (Test: c5c3-invalid-cr-namespace-freeze) ---
    Fixture(
        filename="44-transition-base-namespaced.yaml",
        comment=(
            "Accepted base for the namespace-assignment freeze test: a Managed ControlPlane\n"
            "whose Keystone service is placed in a pre-existing namespace it does not own.\n"
            "The External lifecycle is deliberate — the operator never creates that\n"
            "namespace, so the CR parks on NamespacesReady=False/NamespaceNotFound and\n"
            "provisions nothing, leaving no side effects for the rejection step to clean up."
        ),
        name="cp-transition-d",
        keystone=(
            "      mode: Managed\n"
            "      namespace:\n"
            "        name: invalid-cr-preexisting\n"
            "        lifecycle: External\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="45-transition-remove-namespace.yaml",
        comment=(
            "UPDATE dropping the namespace assignment from the accepted base is rejected: the\n"
            "assignment is create-only. Moving a live service across namespaces would leave\n"
            "its backing services, its secret store, and every OpenBao path scoped to the old\n"
            "namespace behind with no migration path. namespace is explicitly nulled, not\n"
            "merely omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so\n"
            "an omitted block would simply be retained and the update would be admitted."
        ),
        name="cp-transition-d",
        keystone=(
            "      mode: Managed\n"
            "      namespace: null\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- transition wave A: Managed -> External (Test: c5c3-invalid-cr-managed-to-external) ---
    Fixture(
        filename="15-transition-base-managed.yaml",
        comment=(
            "Accepted base for the Managed->External transition test: a brownfield,\n"
            "keystone-unset (staged-adoption) ControlPlane — not External, so\n"
            "infrastructure is required and present."
        ),
        name="cp-transition-a",
        keystone="",
        infrastructure=MANAGED_INFRA,
    ),
    Fixture(
        filename="16-transition-to-external.yaml",
        comment=(
            "UPDATE of the accepted base to a valid External shape is rejected outright:\n"
            "adopting an existing installation must be a fresh External-mode ControlPlane."
        ),
        name="cp-transition-a",
    ),
    # --- transition wave B: External -> Managed (Test: c5c3-invalid-cr-external-to-managed) ---
    Fixture(
        filename="17-transition-base-external.yaml",
        comment=(
            "Accepted base for the External->Managed transition test: the issue's minimal\n"
            "External sketch CR (mode + external.authURL + passwordSecretRef, no\n"
            "infrastructure). Doubles as the sketch-CR acceptance proof."
        ),
        name="cp-transition-b",
    ),
    Fixture(
        filename="18-transition-to-managed.yaml",
        comment=(
            "UPDATE of the accepted External base to a Managed shape is rejected with the\n"
            "reserved phase-3 takeover message. external is explicitly nulled, not merely\n"
            "omitted: Chainsaw applies an UPDATE as an RFC 7386 JSON merge patch, so an\n"
            "omitted external would be RETAINED from the External base and trip the\n"
            "intra-struct CEL rule (external forbidden in Managed mode) at CRD validation,\n"
            "before the validating webhook's transition gate ever runs. Nulling external\n"
            "removes the block, yielding the clean Managed shape whose only remaining\n"
            "violation is the External->Managed transition the webhook rejects with phase-3."
        ),
        name="cp-transition-b",
        keystone=(
            "      mode: Managed\n"
            "      external: null\n"
        ),
        infrastructure=MANAGED_INFRA,
    ),
    # --- freeform extraConfig admission (issue #704, create-rejection matrix) ---
    Fixture(
        filename="58-global-extraconfig-unknown-option.yaml",
        comment=(
            "spec.globalExtraConfig sets an unknown option (providr, a typo of the\n"
            "keystone [token] provider option) in a known section: rejected by the webhook\n"
            "because it is absent from the keystone 2025.2 option catalog. The finding is\n"
            "attributed back to the global block that carried it, so the message names\n"
            "spec.globalExtraConfig[token][providr]."
        ),
        name="cp-global-extraconfig-unknown",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        global_extra_config=(
            "  globalExtraConfig:\n"
            "    token:\n"
            "      providr: fernet\n"
        ),
    ),
    Fixture(
        filename="59-glance-extraconfig-unknown-option.yaml",
        comment=(
            "services.glance.extraConfig sets an unknown option (default_backend_typo) in\n"
            "the known [glance_store] section: rejected by the webhook because it is absent\n"
            "from the glance 2025.2 option catalog. The rest of the glance block is valid, so\n"
            "the ONLY violation is the per-service catalog check."
        ),
        name="cp-glance-extraconfig-unknown",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      extraConfig:\n"
            + "        glance_store:\n"
            + "          default_backend_typo: rbd\n"
        ),
    ),
    Fixture(
        filename="60-keystone-extraconfig-in-external.yaml",
        comment=(
            "services.keystone.extraConfig in External mode violates the CEL rule: no\n"
            "Keystone workload is deployed, so there is no child config the merged block\n"
            "would project. The value is otherwise a shape-valid INI block, so the ONLY\n"
            "violation is the External-mode forbid."
        ),
        name="cp-external-extraconfig",
        keystone=(
            VALID_EXTERNAL_KEYSTONE
            + "      extraConfig:\n"
            + "        DEFAULT:\n"
            + '          debug: "true"\n'
        ),
    ),
    Fixture(
        filename="61-horizon-extraconfig-secret-key.yaml",
        comment=(
            "SECRET_KEY in services.horizon.extraConfig is forbidden by the webhook\n"
            "(Family A, unconditional): it is managed via services.horizon.secretKeyRef, and\n"
            "an override in extraConfig would collide with the operator-projected Django\n"
            "session key."
        ),
        name="cp-horizon-extraconfig-secret-key",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        horizon=(
            "    horizon:\n"
            "      extraConfig:\n"
            "        SECRET_KEY: hunter2\n"
        ),
    ),
    Fixture(
        filename="62-glance-importfiltering-allow-and-deny-hosts.yaml",
        comment=(
            "services.glance.importFiltering sets allowedHosts AND disallowedHosts, which\n"
            "violates the CEL mutual-exclusivity rule: glance evaluates the deny-list only\n"
            "while the allow-list is empty, so the deny-list would be silently dropped. The\n"
            "rest of the glance block is valid, so the ONLY violation is the host pair."
        ),
        name="cp-glance-importfiltering-hosts",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      importFiltering:\n"
            + "        allowedHosts:\n"
            + "        - mirror.example.com\n"
            + "        disallowedHosts:\n"
            + "        - 169.254.169.254\n"
        ),
    ),
    Fixture(
        filename="63-glance-staging-sizelimit-zero.yaml",
        comment=(
            "services.glance.staging.sizeLimit of 0 is rejected by the validating webhook,\n"
            "which reuses the glance module's exported validator: a resource.Quantity\n"
            "renders as x-kubernetes-int-or-string in the CRD schema, which carries no\n"
            "Minimum marker, so admission is the sole gate against an unusable\n"
            "scratch-volume bound — a 1Mi floor, since the schema pattern also admits\n"
            "the sub-byte milli suffix (`100m` for `100Mi`). The rest of the glance\n"
            "block is valid, so the ONLY violation is the size limit."
        ),
        name="cp-glance-staging-sizelimit",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      staging:\n"
            + "        sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="64-glance-imagecache-sizelimit-zero.yaml",
        comment=(
            "services.glance.imageCache.sizeLimit of 0 is rejected by the validating\n"
            "webhook, which reuses the glance module's exported validator: a\n"
            "resource.Quantity renders as x-kubernetes-int-or-string in the CRD schema,\n"
            "which carries no Minimum marker, so admission is the sole gate against an\n"
            "unusable cache bound — a 1Mi floor, since the schema pattern also admits\n"
            "the sub-byte milli suffix (`100m` for `100Mi`). The rest of the glance\n"
            "block is valid, so the ONLY violation is the size limit."
        ),
        name="cp-glance-imagecache-sizelimit",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      imageCache:\n"
            + "        sizeLimit: 0\n"
        ),
    ),
    Fixture(
        filename="65-glance-importplugins-inject-key-colon.yaml",
        comment=(
            "services.glance.importPlugins.injectMetadata.properties carries a property\n"
            "name with a colon, rejected by the validating webhook through the glance\n"
            "module's exported ValidateImportPlugins. A map key has no CRD schema\n"
            "counterpart, so admission is the sole gate on this one: the rendered\n"
            "[inject_metadata_properties] inject value is an oslo Dict whose parser\n"
            "splits each pair on the first colon, so the name would be truncated there\n"
            "and its remainder injected as part of the value. The rest of the glance\n"
            "block is valid, so the ONLY violation is the property name."
        ),
        name="cp-glance-importplugins-inject-colon",
        keystone="      mode: Managed\n",
        infrastructure=MANAGED_INFRA,
        glance=(
            VALID_GLANCE
            + "      importPlugins:\n"
            + "        injectMetadata:\n"
            + "          properties:\n"
            + '            "hw:disk_bus": virtio\n'
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
        print("run `python3 tests/e2e/c5c3/invalid-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
