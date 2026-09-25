#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the nova invalid-CR fixture generator.

Mirrors tests/e2e/cinder/invalid-cr/test_generate.py: guards the
canonical-scaffold contract at a layer that runs without a Kubernetes
cluster, so an accidental fixture removal or rename is caught here in
milliseconds instead of at the apply step of the Chainsaw E2E job.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml`` and every fixture ``chainsaw-test.yaml`` applies is
  declared in ``FIXTURES``, which guards against renames or accidental
  deletions in either direction.
* Filenames are unique and so are their two-digit prefixes.
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
* The long-name fixture sits exactly one character past MaxNovaNameLength and
  every other fixture clears the bound.
* No fixture carries a metadata.namespace, which would tie the corpus to a
  namespace the ephemeral Chainsaw run does not create.
* The corpus carries the targetClusterRef fixture the validation-parity audit
  expects of every operator.
"""

from __future__ import annotations

import importlib.util
import re
import sys
import types
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_GENERATOR = _HERE / "_generate.py"
_CHAINSAW_TEST = _HERE / "chainsaw-test.yaml"

# Number of fixtures emitted by _generate.py. Bumping this value requires adding
# the matching Fixture entry AND the matching `file: <name>` line in
# chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 35

# MaxNovaNameLength in operators/nova/api/v1alpha1/nova_webhook.go: the
# 52-character CronJob cap less the 11 characters of "-db-archive".
_MAX_NAME_LENGTH = 41
_LONG_NAME_FIXTURE = "19-name-too-long.yaml"

# Matches the fixture filenames chainsaw-test.yaml applies, for the
# chainsaw-to-FIXTURES direction of the cross-reference.
_APPLIED_FILE_PATTERN = re.compile(r"^\s*file:\s*([0-9]{2}-.+\.yaml)\s*$", re.MULTILINE)


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("nova_invalid_cr_generate", _GENERATOR)
    assert spec and spec.loader, f"failed to load spec for {_GENERATOR}"
    module = importlib.util.module_from_spec(spec)
    # Register before exec_module so @dataclass(frozen=True) can resolve
    # cls.__module__ via sys.modules during class construction.
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class TestFixtures(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.generator = _load_generator()
        cls.chainsaw = _CHAINSAW_TEST.read_text(encoding="utf-8")

    def test_fixture_count(self) -> None:
        self.assertEqual(len(self.generator.FIXTURES), _EXPECTED_FIXTURE_COUNT)

    def test_filenames_unique(self) -> None:
        names = [fixture.filename for fixture in self.generator.FIXTURES]
        self.assertEqual(len(names), len(set(names)), f"duplicate filenames in FIXTURES: {names}")

    def test_numbering_unique(self) -> None:
        """Two fixtures sharing a prefix read as one step in the chainsaw suite."""
        prefixes = [fixture.filename[:2] for fixture in self.generator.FIXTURES]
        self.assertEqual(
            len(prefixes), len(set(prefixes)), f"duplicate fixture numbers: {sorted(prefixes)}"
        )

    def test_every_fixture_referenced_by_chainsaw(self) -> None:
        for fixture in self.generator.FIXTURES:
            self.assertIn(
                f"file: {fixture.filename}",
                self.chainsaw,
                f"{fixture.filename} is not applied by chainsaw-test.yaml",
            )

    def test_every_chainsaw_reference_is_declared(self) -> None:
        declared = {fixture.filename for fixture in self.generator.FIXTURES}
        applied = set(_APPLIED_FILE_PATTERN.findall(self.chainsaw))
        self.assertEqual(
            applied - declared,
            set(),
            "chainsaw-test.yaml applies fixtures that are not declared in FIXTURES",
        )

    def test_no_drift(self) -> None:
        argv = sys.argv
        sys.argv = [str(_GENERATOR), "--check"]
        try:
            self.assertEqual(
                self.generator.main(),
                0,
                "fixtures drifted from _generate.py; regenerate them",
            )
        finally:
            sys.argv = argv

    def test_rendered_fixture_carries_spdx_header(self) -> None:
        for fixture in self.generator.FIXTURES:
            rendered = fixture.render()
            self.assertTrue(
                rendered.startswith("# SPDX-FileCopyrightText:"),
                f"{fixture.filename} must start with the SPDX header",
            )

    def test_remotecompute_fixtures_isolate_one_rule_each(self) -> None:
        """The three remote-compute fixtures differ in the bus they declare.

        Fixture 28 pins the CEL rule, so its bus must stay plaintext. Fixtures 29
        and 30 pin the keystoneEndpoint pattern, so their bus must be verified:
        without messaging.tls the CEL rule would reject them as well, and the
        step could no longer tell which rule answered.
        """
        rendered = {fixture.filename: fixture.render() for fixture in self.generator.FIXTURES}
        self.assertNotIn("    tls:\n", rendered["28-remotecompute-without-messaging-tls.yaml"])
        self.assertIn("    tls:\n", rendered["29-remotecompute-keystoneendpoint-not-url.yaml"])
        self.assertIn("    tls:\n", rendered["30-remotecompute-keystoneendpoint-plaintext.yaml"])

    def test_no_fixture_declares_a_namespace(self) -> None:
        """Chainsaw runs the suite in an ephemeral namespace it creates itself.

        A metadata.namespace would pin a fixture to a namespace that run has no
        reason to carry, so the create fails on the namespace rather than on the
        rule the fixture exists to pin.
        """
        for fixture in self.generator.FIXTURES:
            self.assertNotIn(
                "namespace:",
                fixture.render(),
                f"{fixture.filename} must not declare a namespace",
            )

    def test_scaffold_name_within_webhook_bound(self) -> None:
        """Only the long-name fixture may exceed MaxNovaNameLength (41).

        It sits exactly one character past the bound, so the fixture keeps
        pinning the bound itself rather than some longer name that would still
        be rejected if the bound moved. Every other fixture has to clear it,
        otherwise the create carries a second, unrelated metadata.name error
        next to the one the fixture exists to pin and the Chainsaw assertion
        stops isolating its rule.
        """
        for fixture in self.generator.FIXTURES:
            if fixture.filename == _LONG_NAME_FIXTURE:
                self.assertEqual(len(fixture.name), _MAX_NAME_LENGTH + 1)
                continue
            self.assertLessEqual(
                len(fixture.name),
                _MAX_NAME_LENGTH,
                f"{fixture.filename} name exceeds MaxNovaNameLength",
            )

    def test_targetclusterref_fixture_present(self) -> None:
        """The validation-parity audit expects this fixture in every corpus."""
        filenames = [fixture.filename for fixture in self.generator.FIXTURES]
        self.assertTrue(
            any(name.endswith("-targetclusterref-empty-name.yaml") for name in filenames),
            f"no targetclusterref-empty-name fixture in FIXTURES: {filenames}",
        )


if __name__ == "__main__":
    unittest.main()
