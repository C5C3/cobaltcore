#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the neutron invalid-CR fixture generator.

Mirrors tests/e2e/placement/invalid-cr/test_generate.py: guards the
canonical-scaffold contract at a layer that runs without a Kubernetes
cluster, so an accidental fixture removal or rename is caught here in
milliseconds instead of at the apply step of the Chainsaw E2E job.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml``, which guards against renames or accidental
  deletions.
* Filenames are unique within ``FIXTURES``.
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
* The long-name fixture sits exactly one character past
  MaxNeutronNameLength and every other fixture clears the bound.
* The corpus carries the targetClusterRef fixture the validation-parity audit
  expects of every operator.
"""

from __future__ import annotations

import importlib.util
import sys
import types
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_GENERATOR = _HERE / "_generate.py"
_CHAINSAW_TEST = _HERE / "chainsaw-test.yaml"

# Number of fixtures emitted by _generate.py. Bumping this value requires
# adding the matching Fixture entry AND the matching `file: <name>` line in
# chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 14

# MaxNeutronNameLength in operators/neutron/api/v1alpha1/neutron_types.go: the
# 52-character CronJob cap less the 12 characters of "-ovn-db-sync".
_MAX_NAME_LENGTH = 40
_LONG_NAME_FIXTURE = "12-name-too-long.yaml"


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("neutron_invalid_cr_generate", _GENERATOR)
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

    def test_every_fixture_referenced_by_chainsaw(self) -> None:
        for fixture in self.generator.FIXTURES:
            self.assertIn(
                f"file: {fixture.filename}",
                self.chainsaw,
                f"{fixture.filename} is not applied by chainsaw-test.yaml",
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

    def test_scaffold_name_within_webhook_bound(self) -> None:
        """Only the long-name fixture may exceed MaxNeutronNameLength (40).

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
                f"{fixture.filename} name exceeds MaxNeutronNameLength",
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
