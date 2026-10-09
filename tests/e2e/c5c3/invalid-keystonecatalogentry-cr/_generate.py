#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneCatalogEntry invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneCatalogEntry scaffold used by
every rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-keystoneuser-cr/_generate.py``. Each fixture mutates
one aspect of the canonical scaffold, so the surrounding CR passes every rule
other than the one under test and the admission error is attributable to that
rule.

The kind has no webhook: a target cluster, where an order may live, runs none.
Every rejection here is therefore the CRD schema's own (patterns, minimums and
CEL rules).

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``11``) are each applied once and rejected
  at admission.
* One immutability wave keyed by the metadata.name ``kce-immutable``. It opens
  with a valid base (``12``) that is applied first and admitted, and every later
  fixture of the wave renders that base with one field changed, so Chainsaw
  applies it to the live object as an UPDATE and the CEL transition rules
  evaluate.

The fixtures carry NO metadata.namespace: Chainsaw runs each Test in its own
ephemeral namespace and injects it.

The referenced ControlPlane ``cp-invalid-cr-absent`` never exists, by design.
Admission tolerates the dangling reference, and the reconciler parks the
admitted order on ControlPlaneNotFound without installing its finalizer, so no
fixture can wedge Chainsaw's namespace cleanup.

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

# Canonical KeystoneCatalogEntry scaffold. Placeholders:
#   {name}          metadata.name
#   {ref_name}      the spec.controlPlaneRef.name line (indent 4) or ""
#   {ref_extra}     further spec.controlPlaneRef fields (indent 4) or ""
#   {service_type}  the spec.serviceType line (indent 2) or ""
#   {service_name}  the spec.serviceName line (indent 2) or ""
#   {endpoints}     the spec.endpoints block (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneCatalogEntry
metadata:
  name: {name}
spec:
  controlPlaneRef:
{ref_name}{ref_extra}{service_type}{service_name}{endpoints}"""

_SERVICE_TYPE = "  serviceType: dns\n"


def _endpoints(*rows: tuple[str, str]) -> str:
    """Render a spec.endpoints block of (interface, url) rows."""
    return "  endpoints:\n" + "".join(f"  - interface: {iface}\n    url: {url}\n" for iface, url in rows)


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or the immutability base)."""

    filename: str
    comment: str
    name: str
    controlplane_name: str = "cp-invalid-cr-absent"
    ref_extra: str = ""
    service_type: str = _SERVICE_TYPE
    service_name: str = ""
    endpoints: str = ""

    def render(self) -> str:
        ref_name = f"    name: {self.controlplane_name}\n" if self.controlplane_name else ""
        body = SCAFFOLD.format(
            name=self.name,
            ref_name=ref_name,
            ref_extra=self.ref_extra,
            service_type=self.service_type,
            service_name=self.service_name,
            endpoints=self.endpoints,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


# The immutability base's declared values. The wave below changes one of them
# per fixture.
_BASE_SERVICE_NAME = "  serviceName: svc-dns\n"
_BASE_ENDPOINTS = _endpoints(("public", "https://dns.example.test/v2"))

FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-servicetype-missing.yaml",
        comment=(
            "An entry without a serviceType is rejected by the CRD required list. The\n"
            "error carries 'Required value'."
        ),
        name="kce-invalid-servicetype-missing",
        service_type="",
    ),
    Fixture(
        filename="01-servicetype-identity.yaml",
        comment=(
            "serviceType identity is rejected by a CEL rule: the identity catalog entry\n"
            "is ControlPlane-owned. Every other type is admitted, the built-in ones\n"
            "included. The error carries 'ControlPlane-owned'."
        ),
        name="kce-invalid-servicetype-identity",
        service_type="  serviceType: identity\n",
    ),
    Fixture(
        filename="02-servicetype-uppercase.yaml",
        comment=(
            "serviceType 'DNS' violates the DNS-1123 label pattern (CRD marker). The\n"
            "error carries 'should match'."
        ),
        name="kce-invalid-servicetype-uppercase",
        service_type="  serviceType: DNS\n",
    ),
    Fixture(
        filename="03-servicename-comma.yaml",
        comment=(
            "serviceName carrying a comma is rejected by the CRD pattern that mirrors\n"
            "K-ORC's OpenStackName. The error carries 'should match'."
        ),
        name="kce-invalid-servicename-comma",
        service_name="  serviceName: a,b\n",
    ),
    Fixture(
        filename="04-servicename-empty.yaml",
        comment=(
            "An empty serviceName is rejected by the CRD minimum: the default is\n"
            "spelled by leaving the field out, which means metadata.name. The error\n"
            "carries 'should be at least 1 chars'."
        ),
        name="kce-invalid-servicename-empty",
        service_name='  serviceName: ""\n',
    ),
    Fixture(
        filename="05-endpoint-url-scheme.yaml",
        comment=(
            "An endpoint URL with the ftp scheme violates the http(s) pattern of the\n"
            "endpoint type KeystoneService shares. The error carries 'should match'."
        ),
        name="kce-invalid-endpoint-url-scheme",
        endpoints=_endpoints(("public", "ftp://x")),
    ),
    Fixture(
        filename="06-endpoint-interface-invalid.yaml",
        comment=(
            "An endpoint interface outside public, internal and admin is rejected by\n"
            "the CRD enum. The error carries 'Unsupported value'."
        ),
        name="kce-invalid-endpoint-interface",
        endpoints=_endpoints(("private", "https://dns.example.test/v2")),
    ),
    Fixture(
        filename="07-endpoint-duplicate-interface.yaml",
        comment=(
            "Two endpoint rows sharing the interface 'public' collide on the listMapKey:\n"
            "the list is a listType=map keyed by interface, so the apiserver rejects the\n"
            "duplicate before any rule of ours runs. The error carries 'Duplicate value'."
        ),
        name="kce-invalid-endpoint-duplicate",
        endpoints=_endpoints(("public", "https://dns.example.test/v2"), ("public", "https://dns.example.test/v2")),
    ),
    Fixture(
        filename="08-endpoint-url-too-long.yaml",
        comment=(
            "A 1025-byte endpoint URL is rejected by the CRD maximum that mirrors\n"
            "K-ORC's own EndpointResourceSpec.URL cap. The error carries 'Too long'."
        ),
        name="kce-invalid-endpoint-url-too-long",
        endpoints=_endpoints(("public", "https://" + "a" * (1025 - len("https://")))),
    ),
    Fixture(
        filename="09-name-too-long.yaml",
        comment=(
            "A 64-byte metadata.name is rejected by the root CEL rule: the name is\n"
            "carried as a label value on the order's children, which Kubernetes caps\n"
            "at 63 bytes. The error carries 'metadata.name must be at most 63 bytes'."
        ),
        name="kce-" + "x" * 60,
    ),
    Fixture(
        filename="10-controlplaneref-name-missing.yaml",
        comment=(
            "A controlPlaneRef without a name is rejected by the required list of the\n"
            "CRD schema. No defaulting webhook runs for the kind, so nothing fills the\n"
            "field in before validation. The error carries 'Required value'."
        ),
        name="kce-invalid-ref-name-missing",
        controlplane_name="",
        ref_extra="    namespace: cp-namespace\n",
    ),
    Fixture(
        filename="11-controlplaneref-namespace-invalid.yaml",
        comment=(
            "controlPlaneRef.namespace 'Bad_NS' violates the RFC-1123 label pattern\n"
            "(CRD marker). The error carries 'should match'."
        ),
        name="kce-invalid-ref-namespace",
        ref_extra="    namespace: Bad_NS\n",
    ),
    # --- immutability wave: 12 is applied first and admitted, 13-16 are applied
    #     as UPDATEs of it (Chainsaw applies a same-name fixture to a live object
    #     as an RFC 7386 merge patch) ---
    Fixture(
        filename="12-immutable-base.yaml",
        comment=(
            "Valid base for the immutability wave. It is applied FIRST and must\n"
            "SUCCEED. The 13-16 variants reuse this metadata.name, so Chainsaw applies\n"
            "each of them to the live object as an UPDATE, which is what makes the CEL\n"
            "transition rules evaluate at all. serviceName is declared explicitly."
        ),
        name="kce-immutable",
        service_name=_BASE_SERVICE_NAME,
        endpoints=_BASE_ENDPOINTS,
    ),
    Fixture(
        filename="13-immutable-controlplaneref-name.yaml",
        comment=(
            "controlPlaneRef.name is immutable (CEL transition rule): the edit would\n"
            "strand the catalog rows the order created on the old plane. The error\n"
            "carries 'controlPlaneRef.name is immutable'."
        ),
        name="kce-immutable",
        controlplane_name="cp-other",
        service_name=_BASE_SERVICE_NAME,
        endpoints=_BASE_ENDPOINTS,
    ),
    Fixture(
        filename="14-immutable-controlplaneref-namespace.yaml",
        comment=(
            "controlPlaneRef.namespace is immutable (CEL transition rule), also when\n"
            "the base left it to the order's own namespace. The error carries\n"
            "'controlPlaneRef.namespace is immutable'."
        ),
        name="kce-immutable",
        ref_extra="    namespace: elsewhere\n",
        service_name=_BASE_SERVICE_NAME,
        endpoints=_BASE_ENDPOINTS,
    ),
    Fixture(
        filename="15-immutable-servicetype.yaml",
        comment=(
            "serviceType is immutable (CEL transition rule): the collision probe runs\n"
            "only while no managed Service exists. The error carries 'serviceType is\n"
            "immutable'."
        ),
        name="kce-immutable",
        service_type="  serviceType: image\n",
        service_name=_BASE_SERVICE_NAME,
        endpoints=_BASE_ENDPOINTS,
    ),
    Fixture(
        filename="16-immutable-servicename.yaml",
        comment=(
            "serviceName is immutable (root CEL transition rule over the effective\n"
            "name), for the reason serviceType is. The error carries 'serviceName is\n"
            "immutable'."
        ),
        name="kce-immutable",
        service_name="  serviceName: svc-dns-renamed\n",
        endpoints=_BASE_ENDPOINTS,
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
        print("run `python3 tests/e2e/c5c3/invalid-keystonecatalogentry-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
