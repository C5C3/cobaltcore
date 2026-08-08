#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
"""Generator for the invalid BarbicanSecretStore Chainsaw fixtures.

Single source of truth for the minimal valid BarbicanSecretStore scaffold used
by every rejection test in this directory, modeled on
``tests/e2e/glance/invalid-glancebackend-cr/_generate.py``. Each fixture mutates
exactly one aspect of the canonical scaffold so the surrounding CR passes
validation for every rule OTHER than the one under test, which makes the
admission error attributable to that single rule.

Three fixture categories share this scaffold:

* Create-rejection fixtures (00-11) are each applied once and rejected at
  admission by a CEL XValidation rule, a kubebuilder marker, or
  webhook.validate().
* Immutability pairs. The base CR is applied first and must SUCCEED; the second
  fixture reuses its name, so Chainsaw applies it as an UPDATE and the CRD
  transition rule (evaluated only on UPDATE) rejects it. There is one pair per
  transition rule that can be reached: barbicanRef (12/13), kvMountpoint
  (14/15), and the managed-vs-brownfield mode (16/17). The type-immutability
  rule has no pair: OpenBao is the only enum value, so a changed type is
  already rejected by the Enum marker and the transition rule never gets to
  answer (the glance backend corpus omits its type pair for the same reason).
* Sibling pairs. The base CR is applied first and must SUCCEED; the second
  fixture is a different store attached to the same Barbican, which the
  validating webhook rejects — the single-default rule (18/19) and the
  OpenBao-uniqueness rule (20/21). Both rules answer the second default at once,
  because OpenBao is the only store type; the assertion there pins the
  single-default message and 20/21 isolates the uniqueness rule with a
  non-default store.

Every base carries its own spec.barbicanRef.name: the two sibling rules key on
that name, so distinct references keep the five surviving bases from rejecting
each other.

Every fixture name stays within MaxBarbicanSecretStoreNameLength (55
characters). The bound is checked on every create, so a longer name would put a
second, unrelated error next to the one the fixture exists to pin. The long-name
fixture is the single deliberate exception.

The fixtures deliberately carry NO metadata.namespace: Chainsaw runs each Test
in its own ephemeral namespace, which isolates the sibling List from the
parallel barbican suites pinned to the shared ``openstack`` namespace and lets
the accepted bases (which persist) get torn down with the namespace. The stores
carry no finalizer, so that teardown never blocks on the OpenBao servers the
bases name but the ephemeral namespace does not run.

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

# Canonical valid BarbicanSecretStore scaffold. Any future required field on
# BarbicanSecretStoreSpec must be added below AND verified against every
# fixture. Placeholders: {name} CR name, {barbican_ref} spec.barbicanRef.name,
# {type} spec.type value, {open_bao} the openBao block (empty string to omit
# it), {extra} trailing spec additions (isDefault, extraOptions).
SCAFFOLD = """\
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: BarbicanSecretStore
metadata:
  name: {name}
spec:
  barbicanRef:
    name: {barbican_ref}
  type: {type}
{open_bao}{extra}"""

# Valid managed openBao block (required exactly when type is OpenBao). The
# referenced OpenBaoCluster does not have to exist at admission time. It carries
# its trailing newline so a fixture that appends {extra} stays well-formed.
MANAGED_OPEN_BAO = """\
  openBao:
    instanceRef:
      name: openbao-instance
"""


def brownfield_open_bao(kv_mountpoint: str = "") -> str:
    """Render a valid brownfield openBao block, optionally naming a mount.

    Brownfield mode is what the mount-related fixtures need: a managed store is
    frozen to the provisioned barbican/ mount, so a store that names any other
    mount is rejected by that rule before the rule under test is reached.
    """
    block = (
        "  openBao:\n"
        "    server:\n"
        "      url: https://openbao.example.com:8200\n"
        "      credentialsSecretRef:\n"
        "        name: barbican-openbao-credentials\n"
    )
    if kv_mountpoint:
        block += f"    kvMountpoint: {kv_mountpoint}\n"
    return block


@dataclass(frozen=True)
class Fixture:
    """One generated rejection fixture."""

    filename: str
    comment: str
    name: str
    barbican_ref: str = "barbican"
    store_type: str = "OpenBao"
    open_bao: str = MANAGED_OPEN_BAO
    extra: str = ""

    def render(self) -> str:
        body = SCAFFOLD.format(
            name=self.name,
            barbican_ref=self.barbican_ref,
            type=self.store_type,
            open_bao=self.open_bao,
            extra=self.extra,
        )
        # A blank comment line renders as a bare "#": "# " would ship trailing
        # whitespace into every fixture that separates comment paragraphs.
        comment_lines = "".join(
            f"# {line}\n" if line else "#\n" for line in self.comment.splitlines()
        )
        return LICENSE_HEADER + comment_lines + body


FIXTURES: tuple[Fixture, ...] = (
    Fixture(
        filename="00-type-openbao-without-openbao-block.yaml",
        comment=(
            "type OpenBao without a spec.openBao block violates the union CEL rule\n"
            "((self.type == 'OpenBao') == has(self.openBao)); validateStoreUnion mirrors\n"
            "it. There is nothing to resolve credentials against, so the store could\n"
            "never become ready."
        ),
        name="barbicanstore-no-openbao",
        open_bao="",
    ),
    Fixture(
        filename="01-openbao-instanceref-and-server.yaml",
        comment=(
            "spec.openBao with both instanceRef and server violates the mode CEL rule\n"
            "(has(self.instanceRef) != has(self.server)); validateStoreUnion mirrors it.\n"
            "The two modes disagree on who provisions the mount, so a store naming both\n"
            "names no server at all."
        ),
        name="barbicanstore-both-modes",
        open_bao=(
            "  openBao:\n"
            "    instanceRef:\n"
            "      name: openbao-instance\n"
            "    server:\n"
            "      url: https://openbao.example.com:8200\n"
            "      credentialsSecretRef:\n"
            "        name: barbican-openbao-credentials\n"
        ),
    ),
    Fixture(
        filename="02-openbao-neither-instanceref-nor-server.yaml",
        comment=(
            "spec.openBao with neither instanceRef nor server violates the same mode CEL\n"
            "rule from the other side. The block is written as an empty map: the schema\n"
            "default then fills kvMountpoint, so the mode rule is the only one this\n"
            "fixture breaks."
        ),
        name="barbicanstore-no-mode",
        open_bao="  openBao: {}\n",
    ),
    Fixture(
        filename="03-managed-store-foreign-mount.yaml",
        comment=(
            "A managed store (instanceRef) naming a kvMountpoint other than barbican\n"
            "violates the mount CEL rule on OpenBaoStoreSpec; validateStoreUnion mirrors\n"
            "it. The self-init contract provisions exactly one mount and the minted\n"
            "AppRole policy is scoped to it, so any other mount is a path the store's own\n"
            "credentials can neither read nor write."
        ),
        name="barbicanstore-foreign-mount",
        open_bao=(
            "  openBao:\n"
            "    instanceRef:\n"
            "      name: openbao-instance\n"
            "    kvMountpoint: secrets\n"
        ),
    ),
    Fixture(
        filename="04-managed-store-with-openbao-namespace.yaml",
        comment=(
            "A managed store (instanceRef) naming an OpenBao namespace violates the same\n"
            "mount CEL rule: the self-init contract provisions at the root namespace, so\n"
            "a scoped request addresses a mount the operator never created. Brownfield\n"
            "stores are free to name one, since their server is provisioned elsewhere."
        ),
        name="barbicanstore-namespaced",
        open_bao=(
            "  openBao:\n"
            "    instanceRef:\n"
            "      name: openbao-instance\n"
            "    namespace: tenant-a\n"
        ),
    ),
    Fixture(
        filename="05-brownfield-plaintext-url.yaml",
        comment=(
            "A brownfield store on a plaintext server URL violates the ^https:// pattern\n"
            "on OpenBaoServerSpec.URL, a schema-level rejection the API server answers\n"
            "before the webhook's defense-in-depth twin runs. The AppRole credentials and\n"
            "every secret barbican stores travel this URL, and a plaintext scheme would\n"
            "also make a supplied caBundleSecretRef a no-op."
        ),
        name="barbicanstore-plaintext-url",
        open_bao=(
            "  openBao:\n"
            "    server:\n"
            "      url: http://openbao.example.com:8200\n"
            "      credentialsSecretRef:\n"
            "        name: barbican-openbao-credentials\n"
        ),
    ),
    Fixture(
        filename="06-reserved-name-default.yaml",
        comment=(
            'metadata.name "default" collides with the reserved DEFAULT section of\n'
            "barbican.conf: the name becomes the suffix of this store's\n"
            "[secretstore:<name>] section, and barbican resolves that suffix against the\n"
            "flat section namespace of one config file. The validating webhook is the\n"
            "only gate. It is the sole reserved name reachable here — the rest of the\n"
            "reserved set ([keystone_authtoken], [vault_plugin], [oslo_policy] and\n"
            "friends) carries underscores, which no Kubernetes object name may contain."
        ),
        name="default",
    ),
    Fixture(
        filename="07-name-too-long-for-approle-secret.yaml",
        comment=(
            "A metadata.name of 56 characters, one past\n"
            "MaxBarbicanSecretStoreNameLength, is rejected by the validating webhook on\n"
            'CREATE: the operator appends "-approle" to name the AppRole credentials\n'
            "Secret, and the result would exceed the 63-character DNS label budget. The\n"
            "rule runs in ValidateCreate only, so the finalizer-removal update never\n"
            "trips over it."
        ),
        name="barbican-store-invalid-name-too-long-for-approle-secrets",
    ),
    Fixture(
        filename="08-extraoptions-empty-key.yaml",
        comment=(
            "spec.extraOptions with an empty option name is rejected by the validating\n"
            "webhook: the map key has no schema counterpart to bound it, and a nameless\n"
            "option would render as a bare `= value` line in [vault_plugin]."
        ),
        name="barbicanstore-empty-option",
        extra=(
            "  extraOptions:\n"
            '    "": "10"\n'
        ),
    ),
    Fixture(
        filename="09-extraoptions-bad-charset.yaml",
        comment=(
            'spec.extraOptions key "max-retries" carries dashes, which the validating\n'
            "webhook's key allowlist (^[A-Za-z0-9_]+$) rejects. The allowlist runs before\n"
            "the denylist because the denylist matches exact strings and is therefore\n"
            "blind to a newline in a key or to a denylist-evading trailing space."
        ),
        name="barbicanstore-bad-option-name",
        extra=(
            "  extraOptions:\n"
            '    max-retries: "3"\n'
        ),
    ),
    Fixture(
        filename="10-extraoptions-denylist.yaml",
        comment=(
            "spec.extraOptions carrying kv_mountpoint duplicates an option the operator\n"
            "renders from spec.openBao.kvMountpoint; the validating webhook's denylist\n"
            "rejects it. Depending on render order the duplicate would shadow the derived\n"
            "value or be shadowed by it, and either way the store's AppRole policy still\n"
            "covers only the mount the typed field names."
        ),
        name="barbicanstore-denylisted-option",
        extra=(
            "  extraOptions:\n"
            "    kv_mountpoint: secrets\n"
        ),
    ),
    Fixture(
        filename="11-extraoptions-value-control-char.yaml",
        comment=(
            "A spec.extraOptions value carrying a newline is rejected by the validating\n"
            "webhook. The renderer writes `key = value` verbatim into the process-global\n"
            "[vault_plugin] section, so the newline injects a root_token_id line — the\n"
            "credential the denylist exists to keep out of the rendered file, reached\n"
            "through a key that is on no list at all."
        ),
        name="barbicanstore-option-control-char",
        extra=(
            "  extraOptions:\n"
            '    max_retries: "3\\nroot_token_id = hvs.example-root-token"\n'
        ),
    ),
    Fixture(
        filename="12-immutable-barbicanref-base.yaml",
        comment=(
            "Valid base store for the barbicanRef-immutability pair. It is applied FIRST\n"
            "and must SUCCEED; 13-immutable-barbicanref reuses this name so it is applied\n"
            "as an UPDATE. The referenced Barbican does not have to exist at admission\n"
            "time (GitOps ordering), so the base is admitted."
        ),
        name="barbicanstore-immutable-ref",
        barbican_ref="barbican-immutable-ref",
    ),
    Fixture(
        filename="13-immutable-barbicanref.yaml",
        comment=(
            "Update of the barbicanstore-immutable-ref base CR that re-points\n"
            "spec.barbicanRef.name. The spec-level CEL transition rule\n"
            "(self.barbicanRef.name == oldSelf.barbicanRef.name) rejects the change on\n"
            "UPDATE: re-pointing would leave the old Barbican with a store nothing\n"
            "manages anymore and race the config projection on the new one."
        ),
        name="barbicanstore-immutable-ref",
        barbican_ref="barbican-repointed",
    ),
    Fixture(
        filename="14-immutable-kvmountpoint-base.yaml",
        comment=(
            "Valid base store for the kvMountpoint-immutability pair, in brownfield mode\n"
            "so it may name a mount of its own. It is applied FIRST and must SUCCEED;\n"
            "15-immutable-kvmountpoint reuses this name so it is applied as an UPDATE."
        ),
        name="barbicanstore-immutable-mount",
        barbican_ref="barbican-immutable-mount",
        open_bao=brownfield_open_bao("secrets-a"),
    ),
    Fixture(
        filename="15-immutable-kvmountpoint.yaml",
        comment=(
            "Update of the barbicanstore-immutable-mount base CR that names a different\n"
            "mount. The CEL transition rule on OpenBaoStoreSpec (self.kvMountpoint ==\n"
            "oldSelf.kvMountpoint) rejects it on UPDATE: the secret material already\n"
            "written under the old mount is addressed by that mount and is not reachable\n"
            "under a new one."
        ),
        name="barbicanstore-immutable-mount",
        barbican_ref="barbican-immutable-mount",
        open_bao=brownfield_open_bao("secrets-b"),
    ),
    Fixture(
        filename="16-immutable-mode-base.yaml",
        comment=(
            "Valid brownfield base store for the mode-immutability pair. It is applied\n"
            "FIRST and must SUCCEED; 17-immutable-mode reuses this name so it is applied\n"
            "as an UPDATE.\n"
            "\n"
            "The brownfield half is the base, not the update. chainsaw applies an existing\n"
            "object as a merge patch, so the update adds its branch without removing this\n"
            "one, and the transition rule under test is keyed on has(instanceRef): only a\n"
            "base that lacks instanceRef makes that predicate flip on the UPDATE. The\n"
            "keystone database-mode pair (tests/e2e/keystone/invalid-cr) is oriented the\n"
            "same way for the same reason."
        ),
        name="barbicanstore-immutable-mode",
        barbican_ref="barbican-immutable-mode",
        open_bao=brownfield_open_bao(),
    ),
    Fixture(
        filename="17-immutable-mode.yaml",
        comment=(
            "Update of the barbicanstore-immutable-mode base CR that swaps the brownfield\n"
            "server for a managed instanceRef. The CEL transition rule\n"
            "(has(self.instanceRef) == has(oldSelf.instanceRef)) rejects it on UPDATE: the\n"
            "switch re-points the plugin at a different server entirely.\n"
            "\n"
            "The merge patch leaves the base's server block in place, so the merged object\n"
            "also trips the exactly-one rule. The API server evaluates every CEL rule and\n"
            "returns both messages, and the suite asserts on the mode one. kvMountpoint is\n"
            "left unset in both halves so it resolves to the same default and the mount\n"
            "transition rule stays satisfied."
        ),
        name="barbicanstore-immutable-mode",
        barbican_ref="barbican-immutable-mode",
    ),
    Fixture(
        filename="18-default-base.yaml",
        comment=(
            "Valid default store for the single-default pair. It is applied FIRST and\n"
            "must SUCCEED; 19-second-default attaches a second default to the same\n"
            "Barbican, which the validating webhook rejects."
        ),
        name="barbicanstore-default-a",
        barbican_ref="barbican-default",
        extra="  isDefault: true\n",
    ),
    Fixture(
        filename="19-second-default.yaml",
        comment=(
            "Second isDefault store attached to the same Barbican as 18-default-base.\n"
            "Exactly one default store is allowed per Barbican, so the validating\n"
            "webhook's sibling List rejects the newcomer. Applied AFTER 18-default-base.\n"
            "The OpenBao-uniqueness rule answers this CR as well — OpenBao is the only\n"
            "store type, so any second store on one Barbican breaks it too — and both\n"
            "errors aggregate into the same admission response; 20/21 isolates that rule\n"
            "with a store that is not a default."
        ),
        name="barbicanstore-default-b",
        barbican_ref="barbican-default",
        extra="  isDefault: true\n",
    ),
    Fixture(
        filename="20-openbao-unique-base.yaml",
        comment=(
            "Valid base store for the OpenBao-uniqueness pair. It is applied FIRST and\n"
            "must SUCCEED; 21-second-openbao attaches a second OpenBao store to the same\n"
            "Barbican. Neither store is a default, so the single-default rule stays out\n"
            "of this pair."
        ),
        name="barbicanstore-unique-a",
        barbican_ref="barbican-unique",
    ),
    Fixture(
        filename="21-second-openbao.yaml",
        comment=(
            "Second OpenBao store attached to the same Barbican as 20-openbao-unique-base.\n"
            "The vault plugin reads its server URL, its credentials, and its mount from\n"
            "the process-global [vault_plugin] section every store in one barbican.conf\n"
            "shares, so a second store would not get a second server: whichever section\n"
            "rendered last would decide where the other store's secrets are written. The\n"
            "validating webhook's sibling List rejects it. Applied AFTER\n"
            "20-openbao-unique-base."
        ),
        name="barbicanstore-unique-b",
        barbican_ref="barbican-unique",
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
        print("run `python3 tests/e2e/barbican/invalid-barbicansecretstore-cr/_generate.py` to regenerate")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
