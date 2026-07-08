#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid KeystoneIdentityBackend Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneIdentityBackend scaffold
used by every rejection test in this directory. Each fixture mutates exactly
one aspect of the canonical scaffold so the surrounding CR passes schema
validation for every rule OTHER than the one under test, ensuring the
admission error is attributable to that single rule.

Three fixture categories share this scaffold:

* Create-rejection fixtures are each applied once and rejected at admission by
  a CEL XValidation rule, a kubebuilder marker, or webhook.validate().
* Update-rejection fixtures share the name ``immutable-backend``:
  ``09-immutable-base`` is the valid base CR applied first, and the ``10``-
  ``12`` variants (plus ``29-immutable-type``) are applied as UPDATEs of that
  base so the CRD CEL transition rules (``self == oldSelf``, evaluated only
  on UPDATE) reject the field change.
* The duplicate-domain pair: ``13-duplicate-domain`` is a second, otherwise
  valid backend whose domain name collides case-insensitively with the
  ``09-immutable-base`` CR on the same Keystone; the validating webhook
  rejects it.
* OIDC rejection fixtures (``19``-``25``) exercise the federation surface:
  union mismatches, discovery-shape conflicts, scheme/secret-key contracts,
  and mapping-rule completeness.
* The OIDC sibling set: ``18-oidc-base`` is a valid OIDC backend applied
  first, and ``26``-``28`` collide with it on identityProviderName,
  remoteIDAttribute uniformity, and the single-introspection-backend limit.
  ``30`` enables introspection with an http endpoint, which the webhook
  rejects (mod_auth_openidc requires https there).

Usage:

    # Regenerate all fixtures from this single source of truth.
    python3 _generate.py

    # CI-friendly drift check: exit non-zero if any on-disk fixture diverges
    # from the regenerated content (or an orphan fixture exists on disk).
    python3 _generate.py --check
"""

from __future__ import annotations

import re
import sys
from dataclasses import dataclass
from pathlib import Path

# Matches every two-digit-prefixed fixture in this directory. Used by the
# orphan-detection sweep in main() so a fixture removed from FIXTURES but left
# on disk is reported as drift.
_FIXTURE_FILENAME_PATTERN = re.compile(r"^[0-9]{2}-.+\.yaml$")

LICENSE_HEADER = """\
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""

# Canonical valid LDAP block. Any future required field on LDAPBackendSpec
# must be added below AND verified against every fixture.
LDAP_DEFAULT = """\
  ldap:
    url: ldap://ldap.example.com:389
    bindCredentialsSecretRef:
      name: corp-ldap-bind
    suffix: dc=example,dc=com
    users:
      treeDN: ou=people,dc=example,dc=com
"""

# LDAP block with an http:// URL — rejected by the LDAPBackendSpec.URL
# Pattern marker (and defense-in-depth in the webhook).
LDAP_BAD_URL = """\
  ldap:
    url: http://ldap.example.com:389
    bindCredentialsSecretRef:
      name: corp-ldap-bind
    suffix: dc=example,dc=com
    users:
      treeDN: ou=people,dc=example,dc=com
"""

# LDAP block that sets bindCredentialsSecretRef.key — rejected by the
# validating webhook (the bind Secret's data keys are fixed by contract).
LDAP_BINDREF_KEY = """\
  ldap:
    url: ldap://ldap.example.com:389
    bindCredentialsSecretRef:
      name: corp-ldap-bind
      key: bindpw
    suffix: dc=example,dc=com
    users:
      treeDN: ou=people,dc=example,dc=com
"""

# LDAP block whose suffix embeds a newline (the double-quoted YAML `\n` escape
# parses to a real newline). RenderINI writes values verbatim, so the newline
# would inject an extra [ldap] line — here re-enabling user_allow_create,
# defeating the readOnly forcing. The suffix field carries only a MinLength
# marker (no anchored charset guard), so the validating webhook's
# control-character guard is the gate.
LDAP_CTRL_SUFFIX = """\
  ldap:
    url: ldap://ldap.example.com:389
    bindCredentialsSecretRef:
      name: corp-ldap-bind
    suffix: "dc=example,dc=com\\nuser_allow_create = true"
    users:
      treeDN: ou=people,dc=example,dc=com
"""

# Canonical valid OIDC block. Any future required field on OIDCBackendSpec
# must be added below AND verified against every fixture.
OIDC_DEFAULT = """\
  oidc:
    issuer: https://idp.example.com/realms/forge
    clientID: keystone
    clientSecretRef:
      name: corp-oidc-client
"""

# OIDC block with an ldap:// issuer — rejected by the OIDCBackendSpec.Issuer
# Pattern marker (^https?://) and defense-in-depth in the webhook.
OIDC_BAD_ISSUER = """\
  oidc:
    issuer: ldap://not-an-idp.example.com
    clientID: keystone
    clientSecretRef:
      name: corp-oidc-client
"""

# OIDC block that sets BOTH discovery shapes — rejected by the CEL rule on
# OIDCBackendSpec (providerMetadataURL and endpoints are mutually exclusive).
OIDC_DISCOVERY_CONFLICT = """\
  oidc:
    issuer: https://idp.example.com/realms/forge
    providerMetadataURL: https://idp.example.com/realms/forge/.well-known/openid-configuration
    endpoints:
      authorizationEndpoint: https://idp.example.com/auth
      tokenEndpoint: https://idp.example.com/token
      jwksURI: https://idp.example.com/certs
    clientID: keystone
    clientSecretRef:
      name: corp-oidc-client
"""

# OIDC block that sets clientSecretRef.key — rejected by the validating
# webhook (the client Secret's data key is fixed by contract).
OIDC_CLIENTREF_KEY = """\
  oidc:
    issuer: https://idp.example.com/realms/forge
    clientID: keystone
    clientSecretRef:
      name: corp-oidc-client
      key: secret
"""


@dataclass(frozen=True)
class Fixture:
    """One invalid-backend fixture: filename + per-fixture overrides."""

    filename: str
    name: str
    comment: str
    domain_name: str = "corp"
    domain_extra: str | None = None
    include_type: bool = True
    backend_type: str = "LDAP"
    ldap: str | None = LDAP_DEFAULT
    trailing: str | None = None


def render(fixture: Fixture) -> str:
    """Render a single fixture from the canonical scaffold."""
    parts: list[str] = [
        LICENSE_HEADER,
        fixture.comment.rstrip() + "\n",
        "apiVersion: keystone.openstack.c5c3.io/v1alpha1\n",
        "kind: KeystoneIdentityBackend\n",
        "metadata:\n",
        f"  name: {fixture.name}\n",
        "spec:\n",
        "  keystoneRef:\n",
        "    name: keystone\n",
        "  domain:\n",
        f"    name: {fixture.domain_name}\n",
    ]
    if fixture.domain_extra is not None:
        parts.append(fixture.domain_extra)
    if fixture.include_type:
        parts.append(f"  type: {fixture.backend_type}\n")
    if fixture.ldap is not None:
        parts.append(fixture.ldap)
    if fixture.trailing is not None:
        parts.append(fixture.trailing)
    return "".join(parts)


FIXTURES: list[Fixture] = [
    Fixture(
        filename="00-missing-type.yaml",
        name="invalid-missing-type",
        include_type=False,
        ldap=None,
        comment="""\
# KeystoneIdentityBackend without spec.type. The field is required by the
# CRD schema (no default — Phase 1 has a single enum value but the field
# stays explicit so the federation phases extend it compatibly). Admission
# must reject this CR with an $error referencing the substring "type".""",
    ),
    Fixture(
        filename="01-union-mismatch.yaml",
        name="invalid-union-mismatch",
        ldap=None,
        comment="""\
# KeystoneIdentityBackend with type LDAP but no spec.ldap block. The union
# rule is enforced by the spec-level CEL XValidation
# ((self.type == 'LDAP') == has(self.ldap)) and by defense-in-depth in the
# validating webhook. Admission must reject this CR with an $error
# referencing the message "exactly one backend block matching spec.type".""",
    ),
    Fixture(
        filename="02-domain-default.yaml",
        name="invalid-domain-default",
        domain_name="default",
        comment="""\
# KeystoneIdentityBackend whose spec.domain.name is "default". The Default
# domain hosts the SQL-backed service users and the bootstrap admin and must
# never be re-pointed at an external directory. Enforced by the CEL
# XValidation rule on DomainSpec.Name (self.lowerAscii() != 'default') and
# by the validating webhook. Admission must reject this CR with an $error
# referencing "must never be backed by an external identity backend".""",
    ),
    Fixture(
        filename="03-domain-default-mixed-case.yaml",
        name="invalid-domain-default-case",
        domain_name="Default",
        comment="""\
# Same rule as 02-domain-default.yaml with mixed casing: the CEL rule
# lower-cases before comparing, so "Default" is equally rejected — keystone
# domain-name lookups behave case-insensitively on MySQL's default
# collation, so the bootstrap admin domain would still collide.""",
    ),
    Fixture(
        filename="04-bad-url-scheme.yaml",
        name="invalid-bad-url-scheme",
        ldap=LDAP_BAD_URL,
        comment="""\
# KeystoneIdentityBackend whose spec.ldap.url uses http:// instead of
# ldap:// or ldaps://. Rejected by the Pattern marker on
# LDAPBackendSpec.URL (^ldaps?://) and by defense-in-depth in the
# validating webhook. Admission must reject this CR with an $error
# referencing the substring "ldap.url".""",
    ),
    Fixture(
        filename="05-bindref-key-set.yaml",
        name="invalid-bindref-key",
        ldap=LDAP_BINDREF_KEY,
        comment="""\
# KeystoneIdentityBackend that sets spec.ldap.bindCredentialsSecretRef.key.
# The bind Secret's data keys are fixed by contract ("username" and
# "password"), so a key override is rejected by the validating webhook.
# Admission must reject this CR with an $error referencing
# "data keys are fixed".""",
    ),
    Fixture(
        filename="06-extraoptions-typed-field.yaml",
        name="invalid-extraoptions-url",
        trailing="""\
  extraOptions:
    url: ldap://sneaky.example.com
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions carries "url" — an option
# rendered from the typed spec.ldap.url field. A duplicate would silently
# shadow the typed value, so the validating webhook's denylist rejects it.
# Admission must reject this CR with an $error referencing
# 'option "url" is owned by'.""",
    ),
    Fixture(
        filename="07-extraoptions-driver.yaml",
        name="invalid-extraoptions-driver",
        trailing="""\
  extraOptions:
    driver: sql
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions carries "driver" — the
# identity-driver wiring the operator owns. The validating webhook's
# denylist rejects it. Admission must reject this CR with an $error
# referencing 'option "driver" is owned by'.""",
    ),
    Fixture(
        filename="08-extraoptions-readonly-conflict.yaml",
        name="invalid-extraoptions-readonly",
        trailing="""\
  extraOptions:
    user_allow_create: "true"
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions re-enables
# user_allow_create while readOnly defaults to true. The projection forces
# the write-enabling options to false under readOnly, so the validating
# webhook rejects the contradiction. Admission must reject this CR with an
# $error referencing 'conflicts with readOnly: true'.""",
    ),
    # ── Update-rejection fixtures ──────────────────────────────────────────
    Fixture(
        filename="09-immutable-base.yaml",
        name="immutable-backend",
        comment="""\
# Valid base KeystoneIdentityBackend for the field-immutability
# update-rejection fixtures. It is applied FIRST and must SUCCEED. The
# variants 10-12 reuse this name so each is applied as an UPDATE of this
# object, exercising the CRD CEL transition rules on spec.keystoneRef,
# spec.domain.name, and spec.domain.mode. The referenced Keystone CR does
# not exist in the ephemeral namespace — by design admission does not
# require it (GitOps ordering), and the dangling reference merely surfaces
# as DomainReady=False/KeystoneNotFound.""",
    ),
    Fixture(
        filename="10-immutable-keystoneref.yaml",
        name="immutable-backend",
        trailing=None,
        comment="""\
# Update of the immutable-backend base CR that re-points spec.keystoneRef
# at a different Keystone. The spec-level CEL transition rule
# (self.keystoneRef.name == oldSelf.keystoneRef.name) rejects the change on
# UPDATE. Admission must reject this UPDATE with an $error referencing the
# substrings "keystoneRef" and "immutable".""",
        # keystoneRef override is spliced by a dedicated render path below.
    ),
    Fixture(
        filename="11-immutable-domain-name.yaml",
        name="immutable-backend",
        domain_name="renamed",
        comment="""\
# Update of the immutable-backend base CR that renames spec.domain.name
# from "corp" to "renamed". The CEL transition rule on DomainSpec.Name
# (self == oldSelf) rejects the change on UPDATE. Admission must reject
# this UPDATE with an $error referencing "domain.name is immutable".""",
    ),
    Fixture(
        filename="12-immutable-domain-mode.yaml",
        name="immutable-backend",
        domain_extra="""\
    mode: Adopt
""",
        comment="""\
# Update of the immutable-backend base CR that flips spec.domain.mode from
# the defaulted Manage to Adopt. The CEL transition rule on DomainSpec.Mode
# (self == oldSelf) rejects the change on UPDATE — flipping the mode would
# change deletion semantics for a domain provisioned under the old
# contract. Admission must reject this UPDATE with an $error referencing
# "domain.mode is immutable".""",
    ),
    Fixture(
        filename="13-duplicate-domain.yaml",
        name="duplicate-domain-backend",
        domain_name="CORP",
        comment="""\
# Second, otherwise valid KeystoneIdentityBackend whose domain name
# collides case-insensitively ("CORP" vs the base CR's "corp") on the same
# referenced Keystone. Two backends projecting the same
# keystone.<domain>.conf would fight over one domain, so the validating
# webhook's uniqueness rule rejects the newcomer. Applied AFTER
# 09-immutable-base.yaml; admission must reject it with an $error
# referencing "domain name collides".""",
    ),
    # ── Control-character (INI-injection) create-rejection fixtures ─────────
    Fixture(
        filename="14-ldap-control-char.yaml",
        name="invalid-ldap-control-char",
        ldap=LDAP_CTRL_SUFFIX,
        comment="""\
# KeystoneIdentityBackend whose spec.ldap.suffix embeds a newline. RenderINI
# writes every value verbatim as `key = value`, so a newline injects arbitrary
# [ldap] lines (here re-enabling user_allow_create, defeating the readOnly
# forcing). The suffix field carries only a MinLength marker, so the validating
# webhook's control-character guard is the gate. Admission must reject this CR
# with an $error referencing "must not contain newline or carriage-return".""",
    ),
    Fixture(
        filename="15-extraoptions-control-char.yaml",
        name="invalid-extraoptions-control-char",
        trailing="""\
  extraOptions:
    zzz_pwn: "x\\nuser_allow_create = true"
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions value embeds a newline. The
# key ("zzz_pwn") is not on the denylist, but the value smuggles
# "user_allow_create = true": RenderINI sorts keys, so it would render after
# the forced user_allow_create = false and win under oslo.config's
# last-value-wins scalar semantics. The validating webhook's control-character
# guard on extraOptions values rejects it. Admission must reject this CR with
# an $error referencing "must not contain newline or carriage-return".""",
    ),
    # ── extraOptions key-shape (INI-injection / denylist-evasion) fixtures ──
    Fixture(
        filename="16-extraoptions-key-control-char.yaml",
        name="invalid-extraoptions-key-control-char",
        trailing="""\
  extraOptions:
    "zzz_pwn\\nuser_allow_create = true": "x"
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions KEY embeds a newline (the
# double-quoted YAML `\\n` escape parses to a real newline). RenderINI writes
# every option verbatim as `key = value`, so a newline in the key injects an
# arbitrary [ldap] line — here re-enabling user_allow_create past the readOnly
# forcing — the same attack as 15 moved from the value to the key. The CRD's
# additionalProperties schema accepts the key (no propertyNames constraint), so
# the validating webhook's extraOptions key allowlist (^[A-Za-z0-9_]+$) is the
# gate. Admission must reject this CR with an $error referencing
# "option name must match".""",
    ),
    Fixture(
        filename="17-extraoptions-key-trailing-space.yaml",
        name="invalid-extraoptions-key-trailing-space",
        trailing="""\
  extraOptions:
    "user_allow_create ": "true"
""",
        comment="""\
# KeystoneIdentityBackend whose spec.extraOptions KEY is "user_allow_create "
# (trailing space). The space means it is not string-equal to the denylisted /
# forced "user_allow_create", so it evades both the exact-match denylist and
# the readOnly forced-option check — yet oslo.config strips the trailing space
# and treats it as a duplicate user_allow_create, overriding the forced false.
# The validating webhook's extraOptions key allowlist (^[A-Za-z0-9_]+$) rejects
# the malformed key. Admission must reject this CR with an $error referencing
# "option name must match".""",
    ),
    # ── OIDC federation fixtures ─────────────────────────────────────────────
    Fixture(
        filename="18-oidc-base.yaml",
        name="oidc-base-backend",
        domain_name="sso-base",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT + """\
    oauth2Introspection:
      enabled: true
""",
        comment="""\
# Valid base OIDC KeystoneIdentityBackend for the sibling-rejection fixtures.
# It is applied FIRST and must SUCCEED. Its identityProviderName defaults to
# the CR name ("oidc-base-backend"), its remoteIDAttribute defaults to
# HTTP_OIDC_ISS, and it claims the single oauth2Introspection slot — the
# 26-28 fixtures collide with each of those in turn.""",
    ),
    Fixture(
        filename="19-oidc-union-mismatch.yaml",
        name="invalid-oidc-union-mismatch",
        backend_type="OIDC",
        ldap=None,
        comment="""\
# KeystoneIdentityBackend with type OIDC but no spec.oidc block. The union
# rule is enforced by the spec-level CEL XValidation
# ((self.type == 'OIDC') == has(self.oidc)) and by defense-in-depth in the
# validating webhook. Admission must reject this CR with an $error
# referencing "type OIDC requires spec.oidc".""",
    ),
    Fixture(
        filename="20-oidc-block-on-ldap.yaml",
        name="invalid-oidc-block-on-ldap",
        trailing=OIDC_DEFAULT,
        comment="""\
# KeystoneIdentityBackend with type LDAP that also carries a spec.oidc
# block. The OIDC union rule ((self.type == 'OIDC') == has(self.oidc))
# rejects the stray block. Admission must reject this CR with an $error
# referencing "type OIDC requires spec.oidc".""",
    ),
    Fixture(
        filename="21-oidc-bad-issuer-scheme.yaml",
        name="invalid-oidc-bad-issuer",
        domain_name="sso-bad-issuer",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_BAD_ISSUER,
        comment="""\
# KeystoneIdentityBackend whose spec.oidc.issuer uses ldap:// instead of
# http:// or https://. Rejected by the Pattern marker on
# OIDCBackendSpec.Issuer (^https?://) and by defense-in-depth in the
# validating webhook. Admission must reject this CR with an $error
# referencing the substring "oidc.issuer".""",
    ),
    Fixture(
        filename="22-oidc-discovery-conflict.yaml",
        name="invalid-oidc-discovery-conflict",
        domain_name="sso-discovery",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DISCOVERY_CONFLICT,
        comment="""\
# KeystoneIdentityBackend whose spec.oidc sets BOTH providerMetadataURL and
# endpoints. The two discovery shapes are mutually exclusive (CEL rule on
# OIDCBackendSpec plus webhook defense-in-depth). Admission must reject this
# CR with an $error referencing "mutually exclusive".""",
    ),
    Fixture(
        filename="23-oidc-clientsecretref-key.yaml",
        name="invalid-oidc-clientref-key",
        domain_name="sso-clientref",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_CLIENTREF_KEY,
        comment="""\
# KeystoneIdentityBackend that sets spec.oidc.clientSecretRef.key. The
# client Secret's data key is fixed by contract ("clientSecret"), so a key
# override is rejected by the validating webhook — mirroring the LDAP bind
# Secret contract. Admission must reject this CR with an $error referencing
# "data key is fixed".""",
    ),
    Fixture(
        filename="24-mappings-on-ldap.yaml",
        name="invalid-mappings-on-ldap",
        trailing="""\
  mappings:
  - local:
    - groups: "{0}"
    remote:
    - type: HTTP_OIDC_ISS
""",
        comment="""\
# KeystoneIdentityBackend of type LDAP that carries spec.mappings — federation
# vocabulary gated to OIDC backends by the spec-level CEL rule
# (!has(self.mappings) || self.type == 'OIDC') and webhook defense-in-depth.
# Admission must reject this CR with an $error referencing
# "mappings are only supported on federation backends".""",
    ),
    Fixture(
        filename="25-oidc-mapping-without-remote.yaml",
        name="invalid-oidc-mapping-no-remote",
        domain_name="sso-mapping",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT + """\
  mappings:
  - local:
    - groups: "{0}"
""",
        comment="""\
# KeystoneIdentityBackend whose mapping rule has no remote matchers. Every
# rule needs at least one local and one remote entry (schema `required` +
# MinItems markers plus webhook defense-in-depth). Admission must reject
# this CR with an $error referencing the substring "remote".""",
    ),
    # ── OIDC sibling-rejection fixtures (applied after 18-oidc-base) ─────────
    Fixture(
        filename="26-oidc-duplicate-idp-name.yaml",
        name="invalid-oidc-duplicate-idp",
        domain_name="sso-two",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT + """\
    identityProviderName: oidc-base-backend
""",
        comment="""\
# Second OIDC backend whose identityProviderName collides with the
# 18-oidc-base CR's default (its CR name) on the same Keystone. The name is
# a path segment of the federation API objects and the protected websso
# Locations, so the validating webhook enforces uniqueness. Admission must
# reject this CR with an $error referencing
# "identity provider name collides".""",
    ),
    Fixture(
        filename="27-oidc-conflicting-remote-id.yaml",
        name="invalid-oidc-remote-id",
        domain_name="sso-three",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT + """\
    remoteIDAttribute: HTTP_OIDC_ISSUER
""",
        comment="""\
# Second OIDC backend whose remoteIDAttribute (HTTP_OIDC_ISSUER) differs
# from the 18-oidc-base CR's default (HTTP_OIDC_ISS) on the same Keystone.
# The attribute renders into the single [openid] section of keystone.conf,
# so it must be uniform across every OIDC backend of one Keystone
# (webhook-enforced). Admission must reject this CR with an $error
# referencing "remoteIDAttribute must be uniform".""",
    ),
    Fixture(
        filename="28-oidc-second-introspection.yaml",
        name="invalid-oidc-second-introspection",
        domain_name="sso-four",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT + """\
    oauth2Introspection:
      enabled: true
""",
        comment="""\
# Second OIDC backend that enables oauth2Introspection while the
# 18-oidc-base CR already claims the slot. mod_auth_openidc's OIDCOAuth*
# resource-server directives are server-scoped, so at most one OIDC backend
# per Keystone may enable introspection (webhook-enforced). Admission must
# reject this CR with an $error referencing "at most one OIDC backend".""",
    ),
    Fixture(
        filename="29-immutable-type.yaml",
        name="immutable-backend",
        backend_type="OIDC",
        ldap=None,
        trailing=OIDC_DEFAULT,
        comment="""\
# Update of the immutable-backend base CR that flips spec.type from LDAP to
# OIDC (with a matching oidc block so only the transition rule fires). The
# spec-level CEL transition rule (self.type == oldSelf.type) rejects the
# change on UPDATE. Admission must reject this UPDATE with an $error
# referencing "type is immutable".""",
    ),
    Fixture(
        filename="30-oidc-http-introspection.yaml",
        name="invalid-oidc-http-introspection",
        domain_name="sso-five",
        backend_type="OIDC",
        ldap=None,
        trailing="""\
  oidc:
    issuer: https://idp.example.com/realms/forge-five
    clientID: keystone
    clientSecretRef:
      name: corp-oidc-client
    endpoints:
      authorizationEndpoint: https://idp.example.com/auth
      tokenEndpoint: https://idp.example.com/token
      jwksURI: https://idp.example.com/certs
      introspectionEndpoint: http://idp.example.com/introspect
    oauth2Introspection:
      enabled: true
""",
        comment="""\
# OIDC backend enabling oauth2Introspection with an http (not https)
# explicit introspectionEndpoint. mod_auth_openidc's
# OIDCOAuthIntrospectionEndpoint is https-only at Apache config-parse time —
# an http endpoint would crash-loop the sidecar — so the webhook rejects it
# at admission. Admission must reject this CR with an $error referencing
# "introspectionEndpoint must use https://".""",
    ),
]


def render_fixture(fixture: Fixture) -> str:
    """Render a fixture, special-casing the keystoneRef-update variant."""
    rendered = render(fixture)
    if fixture.filename == "10-immutable-keystoneref.yaml":
        rendered = rendered.replace("    name: keystone\n", "    name: keystone-other\n", 1)
    return rendered


def main(argv: list[str]) -> int:
    check_only = "--check" in argv
    here = Path(__file__).resolve().parent
    drift: list[str] = []
    for fixture in FIXTURES:
        rendered = render_fixture(fixture)
        target = here / fixture.filename
        # encoding=utf-8 is pinned so the em-dash characters in the rendered
        # comments round-trip deterministically on runners with a non-UTF-8
        # locale.
        if target.exists() and target.read_text(encoding="utf-8") == rendered:
            continue
        if check_only:
            drift.append(fixture.filename)
        else:
            target.write_text(rendered, encoding="utf-8")
            print(f"wrote {fixture.filename}")
    # Orphan sweep: a fixture removed from FIXTURES but left on disk is
    # reported as drift in check mode and deleted otherwise, so the on-disk
    # set always matches FIXTURES (bidirectional drift detection).
    expected = {f.filename for f in FIXTURES}
    for path in sorted(here.iterdir()):
        if (
            path.is_file()
            and _FIXTURE_FILENAME_PATTERN.match(path.name)
            and path.name not in expected
        ):
            if check_only:
                drift.append(f"{path.name} (orphan: not in FIXTURES)")
            else:
                path.unlink()
                print(f"removed orphan {path.name}")
    if check_only and drift:
        print(
            "drift detected — re-run `python3 _generate.py` to refresh:",
            file=sys.stderr,
        )
        for filename in drift:
            print(f"  {filename}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
