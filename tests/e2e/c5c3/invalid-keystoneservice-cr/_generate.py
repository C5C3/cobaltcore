#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the KeystoneService invalid-CR Chainsaw fixtures.

Single source of truth for the minimal valid KeystoneService scaffold used by
every rejection test in this directory, mirroring the mechanics of
``tests/e2e/c5c3/invalid-cr/_generate.py`` and the corpus shape of
``tests/e2e/keystone/invalid-identitybackend-cr/_generate.py``. Each fixture
mutates exactly one aspect of the canonical scaffold, so the surrounding CR
passes every rule OTHER than the one under test and the admission error is
attributable to that rule.

Two fixture categories share the scaffold:

* Create-rejection fixtures (``00``-``13``) are each applied once and rejected
  at admission by the spec-level CEL rule, a CRD marker, the endpoint
  listType=map key, or the validating webhook.
* Two immutability waves, each keyed by its own metadata.name. Each opens
  with a valid base that is applied first and admitted, and every later
  fixture of the wave renders that base with exactly one field changed, so
  Chainsaw applies it to the live object as an UPDATE. The ``ks-immutable``
  wave (``14``-``22``) leaves catalog.serviceName and account.domainName to
  their fallbacks; the ``ks-immutable-explicit`` wave (``23``-``25``) declares
  both, which is the half the CEL transition rules on those two fields can see
  at all: a transition rule does not evaluate when the old object left the
  optional field unset.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace and injects it.

The referenced ControlPlane ``cp-invalid-cr-absent`` never exists, by design.
Admission tolerates the dangling reference (GitOps ordering, so a registration
may be applied before its plane), the reconciler parks the CR on
Ready=False/ControlPlaneNotFound, and reconcileDelete fails open while the plane
is absent and releases the finalizer. No fixture can therefore wedge Chainsaw's
namespace cleanup.

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

# Canonical KeystoneService scaffold. Any future required field on
# KeystoneServiceSpec must be added below AND verified against every fixture.
# Placeholders:
#   {name}              metadata.name
#   {controlplane_name} the spec.controlPlaneRef.name value
#   {ref_extra}         further spec.controlPlaneRef fields (indent 4) or ""
#   {catalog}           the whole spec.catalog block (indent 2) or ""
#   {account}           the whole spec.account block (indent 2) or ""
#
# metadata.namespace is absent on purpose (Chainsaw injects the ephemeral one).
# Every account block declares spec.account.userName explicitly, so no fixture
# leans on the defaulting webhook materializing it from metadata.name; the
# fallbacks this corpus does exercise are catalog.serviceName and
# account.domainName (fixtures 18 and 20).
SCAFFOLD = """\
apiVersion: c5c3.io/v1alpha1
kind: KeystoneService
metadata:
  name: {name}
spec:
  controlPlaneRef:
    name: {controlplane_name}
{ref_extra}{catalog}{account}"""

# A valid catalog block (indent 2): one service row plus one public endpoint.
# The fixtures that mutate one aspect of it spell their block out instead.
VALID_CATALOG = """\
  catalog:
    serviceType: object-store
    endpoints:
    - interface: public
      url: https://swift.example.com
"""

# A valid account block (indent 2): a user, its referenced project, and one
# role. serviceName and domainName are deliberately absent so the ks-immutable
# base relies on their fallbacks.
VALID_ACCOUNT = """\
  account:
    userName: svc-object-store
    project:
      name: service
    roles:
    - service
"""

# The declared-value counterparts of the two blocks above, shared by the
# ks-immutable-explicit wave: identical except that catalog.serviceName and
# account.domainName are spelled out instead of left to their fallbacks. That is
# what makes the CEL transition rules on those two fields evaluate at all.
EXPLICIT_CATALOG = """\
  catalog:
    serviceType: object-store
    serviceName: entry-one
    endpoints:
    - interface: public
      url: https://swift.example.com
"""

EXPLICIT_ACCOUNT = """\
  account:
    userName: svc-object-store
    domainName: Default
    project:
      name: service
    roles:
    - service
"""


@dataclass(frozen=True)
class Fixture:
    """One generated fixture (a rejection case or the immutability base)."""

    filename: str
    comment: str
    name: str
    controlplane_name: str = "cp-invalid-cr-absent"
    ref_extra: str = ""
    catalog: str = ""
    account: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            controlplane_name=self.controlplane_name,
            ref_extra=self.ref_extra,
            catalog=self.catalog,
            account=self.account,
        )
        comment_lines = "".join(f"# {line}\n" for line in self.comment.splitlines())
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    # --- create-rejection matrix ---
    Fixture(
        filename="00-no-blocks.yaml",
        comment=(
            "A KeystoneService declaring neither a catalog nor an account block asks for no\n"
            "registration at all. Rejected by the spec-level CEL rule, which lives at the\n"
            "schema layer so it binds even where no validating webhook is registered. The\n"
            "error carries the substring:\n"
            "at least one of spec.catalog or spec.account must be set"
        ),
        name="ks-invalid-no-blocks",
    ),
    Fixture(
        filename="01-catalog-identity-type.yaml",
        comment=(
            "catalog.serviceType 'identity' is rejected by the CEL rule on the field: the\n"
            "identity catalog entry is ControlPlane-owned in both modes (created in Managed,\n"
            "imported in External), so registering it here would leave two owners writing one\n"
            "row. The error carries the substring:\n"
            "the identity catalog entry is ControlPlane-owned"
        ),
        name="ks-invalid-identity-type",
        catalog=(
            "  catalog:\n"
            "    serviceType: identity\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="02-catalog-type-bad-shape.yaml",
        comment=(
            "catalog.serviceType 'Object_Store' violates the DNS-1123 label pattern (CRD\n"
            "marker): the type is embedded verbatim in the names of the child K-ORC CRs, so\n"
            "an upper-case letter or an underscore would project a child the apiserver\n"
            "rejects. The error names 'serviceType' and carries 'should match'."
        ),
        name="ks-invalid-type-shape",
        catalog=(
            "  catalog:\n"
            "    serviceType: Object_Store\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="03-catalog-name-comma.yaml",
        comment=(
            "catalog.serviceName carrying a comma violates the CRD pattern, which mirrors\n"
            "K-ORC's own OpenStackName: a comma admitted here would only move the rejection\n"
            "to the child Service CR, where it wedges the reconcile in an exponential backoff\n"
            "that no KeystoneService field error explains. The error names 'serviceName' and\n"
            "carries 'should match'."
        ),
        name="ks-invalid-name-comma",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            '    serviceName: "swift,v2"\n'
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="04-endpoint-url-bad-scheme.yaml",
        comment=(
            "catalog.endpoints[].url with a non-http(s) scheme violates the CRD pattern. The\n"
            "value is advertised verbatim as the catalog endpoint, so a scheme no OpenStack\n"
            "client dials could never serve one. The error names 'url' and carries\n"
            "'should match'."
        ),
        name="ks-invalid-url-scheme",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: ftp://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="05-endpoint-interface-off-enum.yaml",
        comment=(
            "catalog.endpoints[].interface 'gopher' is outside the CRD enum (public,\n"
            "internal, admin), the three interfaces a Keystone catalog row can be published\n"
            "under. The error names 'interface' and carries 'supported values'."
        ),
        name="ks-invalid-interface-enum",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            "    endpoints:\n"
            "    - interface: gopher\n"
            "      url: https://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="06-endpoint-duplicate-interface.yaml",
        comment=(
            "Two endpoint rows sharing the interface 'public' collide on the listMapKey: the\n"
            "list is a listType=map keyed by interface, so an entry publishes at most one row\n"
            "per interface and the apiserver rejects the duplicate before any rule of ours\n"
            "runs. Both rows are otherwise valid, so the key is the only violation. The error\n"
            "names 'endpoints' and carries 'Duplicate value'."
        ),
        name="ks-invalid-duplicate-interface",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
    ),
    Fixture(
        filename="07-account-username-comma.yaml",
        comment=(
            "account.userName carrying a comma violates the CRD pattern, mirroring K-ORC's\n"
            "OpenStackName exactly as the catalog service name does: the cast on the child\n"
            "User CR would be rejected there instead. The error names 'userName' and carries\n"
            "'should match'."
        ),
        name="ks-invalid-username-comma",
        account=(
            "  account:\n"
            '    userName: "glance,admin"\n'
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="08-account-domainname-comma.yaml",
        comment=(
            "account.domainName carrying a comma violates the CRD pattern, which mirrors\n"
            "K-ORC's KeystoneName filter. The domain scopes both the user and its project, so\n"
            "a name the child CRs reject would strand the whole account. The error names\n"
            "'domainName' and carries 'should match'."
        ),
        name="ks-invalid-domainname-comma",
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            '    domainName: "corp,root"\n'
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="09-account-project-name-empty.yaml",
        comment=(
            "account.project.name empty violates the MinLength marker. The project is what\n"
            "the service user's role assignments are scoped to, so an unnamed one resolves to\n"
            "nothing. The error names 'project.name' and carries\n"
            "'should be at least 1 chars long'."
        ),
        name="ks-invalid-project-name-empty",
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    project:\n"
            '      name: ""\n'
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="10-account-role-comma.yaml",
        comment=(
            "An account.roles entry carrying a comma violates the items pattern: each role is\n"
            "projected as an unmanaged K-ORC Role import whose name filter is the same one.\n"
            "The error names 'roles' and carries 'should match'."
        ),
        name="ks-invalid-role-comma",
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            '    - "member,admin"\n'
        ),
    ),
    Fixture(
        filename="11-controlplaneref-name-empty.yaml",
        comment=(
            "controlPlaneRef.name empty violates the MinLength marker on ControlPlaneRefSpec.\n"
            "Every child this CR projects authenticates through the referenced plane's admin\n"
            "credential, so an empty name resolves to no plane at all. The catalog block is\n"
            "the valid one, so the reference is the only violation. The error names\n"
            "'controlPlaneRef.name' and carries 'should be at least 1 chars long'."
        ),
        name="ks-invalid-ref-name-empty",
        controlplane_name='""',
        catalog=VALID_CATALOG,
    ),
    Fixture(
        filename="12-controlplaneref-namespace-bad-shape.yaml",
        comment=(
            "controlPlaneRef.namespace 'Tenant_A' violates the RFC 1123 label pattern (CRD\n"
            "marker): no Kubernetes namespace can carry that name, so the reference could\n"
            "never resolve to an object. The error names 'controlPlaneRef.namespace' and\n"
            "carries 'should match'."
        ),
        name="ks-invalid-ref-namespace-shape",
        ref_extra="    namespace: Tenant_A\n",
        catalog=VALID_CATALOG,
    ),
    Fixture(
        filename="13-metadata-name-overlong.yaml",
        comment=(
            "A 211-byte metadata.name is rejected by the validating webhook's child-name\n"
            "bound, the one rule the CRD schema cannot express: nothing caps metadata.name\n"
            "below the apiserver's own 253 bytes, but every child K-ORC CR carries the\n"
            "43-byte base overhead every KeystoneService is charged (this CR declares no\n"
            "account, so it is not charged the wider roles budget), and 211 + 43 = 254\n"
            "crosses the limit. Without the gate, admission would accept a CR whose reconcile\n"
            "then wedges projecting a child the apiserver rejects. The error carries the\n"
            "substring 'shorten the KeystoneService name'."
        ),
        name="ks-" + "x" * 208,
        catalog=VALID_CATALOG,
    ),
    # --- immutability wave: 14 is applied first and admitted, 15-21 are applied
    #     as UPDATEs of it (Chainsaw applies a same-name fixture to a live object
    #     as an RFC 7386 merge patch) ---
    Fixture(
        filename="14-immutable-base.yaml",
        comment=(
            "Valid base for the immutability wave. It is applied FIRST and must SUCCEED. The\n"
            "15-21 variants reuse this metadata.name, so Chainsaw applies each of them to the\n"
            "live object as an RFC 7386 merge patch, i.e. as an UPDATE, which is what makes\n"
            "the CEL transition rules and the webhook freezes evaluate at all. serviceName\n"
            "and domainName are deliberately left to their fallbacks (metadata.name and the\n"
            "referenced ControlPlane's admin domain): fixtures 18 and 20 add them explicitly,\n"
            "which is the webhook-only half of each freeze."
        ),
        name="ks-immutable",
        catalog=VALID_CATALOG,
        account=VALID_ACCOUNT,
    ),
    Fixture(
        filename="15-immutable-controlplaneref-name.yaml",
        comment=(
            "UPDATE re-pointing controlPlaneRef.name at another ControlPlane, rejected by the\n"
            "CEL transition rule on spec.controlPlaneRef. The edit would strand the Keystone\n"
            "user, project and catalog row the registration already created on the old plane,\n"
            "owned by nothing. The error carries the substring:\n"
            "controlPlaneRef.name is immutable"
        ),
        name="ks-immutable",
        controlplane_name="cp-other",
        catalog=VALID_CATALOG,
        account=VALID_ACCOUNT,
    ),
    Fixture(
        filename="16-immutable-controlplaneref-namespace.yaml",
        comment=(
            "UPDATE adding an explicit controlPlaneRef.namespace over the empty default,\n"
            "rejected by the validating webhook's effective-namespace comparison. The freeze\n"
            "is webhook-only: an empty value means the CR's own namespace, and no CEL rule on\n"
            "a spec field can read metadata.namespace to resolve it. The error carries the\n"
            "substring:\n"
            "controlPlaneRef.namespace is immutable"
        ),
        name="ks-immutable",
        ref_extra="    namespace: elsewhere\n",
        catalog=VALID_CATALOG,
        account=VALID_ACCOUNT,
    ),
    Fixture(
        filename="17-immutable-servicetype.yaml",
        comment=(
            "UPDATE changing catalog.serviceType, rejected by the CEL transition rule on the\n"
            "field. The collision probe that decides whether a pre-existing row of this type\n"
            "and name may be taken over runs only while no managed Service child exists, so\n"
            "an in-place edit re-shapes the registered row without ever re-probing. The error\n"
            "carries the substring 'serviceType is immutable'."
        ),
        name="ks-immutable",
        catalog=(
            "  catalog:\n"
            "    serviceType: block-storage\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
        account=VALID_ACCOUNT,
    ),
    Fixture(
        filename="18-immutable-servicename-explicit.yaml",
        comment=(
            "UPDATE adding an explicit catalog.serviceName, rejected by the validating\n"
            "webhook, which compares the EFFECTIVE name: the base relied on the metadata.name\n"
            "fallback, so naming the entry something else IS a catalog rename. The CEL\n"
            "transition rule on the field cannot see it, because a transition rule does not\n"
            "evaluate when the old object left the optional field unset. The error carries\n"
            "the substring 'serviceName is immutable'."
        ),
        name="ks-immutable",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            "    serviceName: renamed-entry\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
        account=VALID_ACCOUNT,
    ),
    Fixture(
        filename="19-immutable-username.yaml",
        comment=(
            "UPDATE renaming account.userName, rejected by the CEL transition rule on the\n"
            "field: the name identifies a live Keystone user, so the edit would mint a second\n"
            "one and strand the first, still holding the password its consumers authenticate\n"
            "with. The error carries the substring 'userName is immutable'."
        ),
        name="ks-immutable",
        catalog=VALID_CATALOG,
        account=(
            "  account:\n"
            "    userName: svc-renamed\n"
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="20-immutable-domainname-explicit.yaml",
        comment=(
            "UPDATE adding an explicit account.domainName over the fallback, rejected by the\n"
            "validating webhook, which compares the DECLARED value. Resolving the fallback\n"
            "needs the referenced ControlPlane's admin domain, which admission cannot read,\n"
            "so the value is rejected even where it names that same domain. The error carries\n"
            "the substring 'domainName is immutable'."
        ),
        name="ks-immutable",
        catalog=VALID_CATALOG,
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    domainName: Default\n"
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="21-immutable-project-create.yaml",
        comment=(
            "UPDATE flipping account.project.create from the defaulted false to true,\n"
            "rejected by the CEL transition rule on the project field: the flip would hand\n"
            "the registration ownership of a project it merely referenced, and deleting the\n"
            "CR would then delete that live project. The error carries the substring:\n"
            "project.create is immutable"
        ),
        name="ks-immutable",
        catalog=VALID_CATALOG,
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    project:\n"
            "      name: service\n"
            "      create: true\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    Fixture(
        filename="22-immutable-project-name.yaml",
        comment=(
            "UPDATE re-pointing account.project.name at another project, rejected by the CEL\n"
            "transition rule on the project field: the role assignments the registration\n"
            "already minted stay behind on the old project, so the edit scopes every later\n"
            "one to a project the earlier ones never reached. The error carries the\n"
            "substring:\n"
            "project.name is immutable"
        ),
        name="ks-immutable",
        catalog=VALID_CATALOG,
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    project:\n"
            "      name: other-project\n"
            "    roles:\n"
            "    - service\n"
        ),
    ),
    # --- ks-immutable-explicit wave: 23 is applied first and admitted, 24 and
    #     25 are applied as UPDATEs of it. The base DECLARES the two optional
    #     fields the ks-immutable base leaves to their fallbacks, which is the
    #     only way the CEL transition rules on them evaluate ---
    Fixture(
        filename="23-immutable-base-explicit.yaml",
        comment=(
            "Valid base for the declared-value half of the immutability wave. It is applied\n"
            "FIRST of its wave and must SUCCEED. Unlike the ks-immutable base it DECLARES\n"
            "catalog.serviceName and account.domainName: a CEL transition rule does not\n"
            "evaluate when the old object left the optional field unset, so fixtures 18 and\n"
            "20 reach only the webhook and the schema-layer half of both freezes is\n"
            "reachable from this base alone. 24 and 25 change one declared value each."
        ),
        name="ks-immutable-explicit",
        catalog=EXPLICIT_CATALOG,
        account=EXPLICIT_ACCOUNT,
    ),
    Fixture(
        filename="24-immutable-servicename-cel.yaml",
        comment=(
            "UPDATE renaming the DECLARED catalog.serviceName, rejected by the CEL transition\n"
            "rule on the field. This is the schema-layer half of the freeze fixture 18 pins\n"
            "at the webhook, and the rule is what still rejects the rename for a caller that\n"
            "bypasses webhook admission. Because the webhook mirrors the message verbatim,\n"
            "the chainsaw step additionally requires the error to carry no webhook denial.\n"
            "The error carries the substring:\n"
            "serviceName is immutable"
        ),
        name="ks-immutable-explicit",
        catalog=(
            "  catalog:\n"
            "    serviceType: object-store\n"
            "    serviceName: entry-two\n"
            "    endpoints:\n"
            "    - interface: public\n"
            "      url: https://swift.example.com\n"
        ),
        account=EXPLICIT_ACCOUNT,
    ),
    Fixture(
        filename="25-immutable-domainname-cel.yaml",
        comment=(
            "UPDATE moving the DECLARED account.domainName to another domain, rejected by the\n"
            "CEL transition rule on the field. This is the schema-layer half of the freeze\n"
            "fixture 20 pins at the webhook, and the rule is what still rejects the move for\n"
            "a caller that bypasses webhook admission. Because the webhook mirrors the\n"
            "message verbatim, the chainsaw step additionally requires the error to carry no\n"
            "webhook denial. The error carries the substring:\n"
            "domainName is immutable"
        ),
        name="ks-immutable-explicit",
        catalog=EXPLICIT_CATALOG,
        account=(
            "  account:\n"
            "    userName: svc-object-store\n"
            "    domainName: other-domain\n"
            "    project:\n"
            "      name: service\n"
            "    roles:\n"
            "    - service\n"
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
        print("run `python3 tests/e2e/c5c3/invalid-keystoneservice-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
