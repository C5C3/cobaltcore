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
  ``12`` variants are applied as UPDATEs of that base so the CRD CEL
  transition rules (``self == oldSelf``, evaluated only on UPDATE) reject the
  field change. A type-immutability update fixture is deliberately absent:
  LDAP is the only enum value in Phase 1, so any changed type is already
  rejected by the Enum marker before the transition rule could fire.
* The duplicate-domain pair: ``13-duplicate-domain`` is a second, otherwise
  valid backend whose domain name collides case-insensitively with the
  ``09-immutable-base`` CR on the same Keystone; the validating webhook
  rejects it.

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


@dataclass(frozen=True)
class Fixture:
    """One invalid-backend fixture: filename + per-fixture overrides."""

    filename: str
    name: str
    comment: str
    domain_name: str = "corp"
    domain_extra: str | None = None
    include_type: bool = True
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
        parts.append("  type: LDAP\n")
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
