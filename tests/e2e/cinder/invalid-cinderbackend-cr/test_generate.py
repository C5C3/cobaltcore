#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the invalid CinderBackend fixture generator.

Mirrors tests/e2e/glance/invalid-glancebackend-cr/test_generate.py: guards the
canonical-scaffold contract at a layer that runs without a Kubernetes
cluster — accidental fixture removal/rename is caught here in milliseconds
instead of waiting for the Chainsaw E2E job to fail at the apply step.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml`` — guards against renames or accidental deletions.
* Filenames are unique within ``FIXTURES``.
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
* The name-bound fixture sits exactly one character past
  MaxBackendNamePlusCinderRef and every other fixture clears the budget.
* The name-length fixture sits exactly one character past MaxBackendNameLength
  and every other fixture clears that bound.
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

# Number of fixtures emitted by _generate.py: five create-rejection fixtures
# (00-04), the cinderRef-immutability pair (05-base, 06-update), the two
# name-bound fixtures (07 shared budget, 08 per-name) and the export-injection
# fixture (09). Bumping this value requires adding the matching Fixture entry
# AND the matching `file: <name>` line in chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 10

# MaxBackendNamePlusCinderRef in
# operators/cinder/api/v1alpha1/cinderbackend_webhook.go: the 63-character label
# cap less the separator and the 15 characters of "-service-remove".
_MAX_NAME_BUDGET = 47
_NAME_BOUND_FIXTURE = "07-name-bound-exceeded.yaml"

# MaxBackendNameLength in the same file: the 63-character cap on an annotation
# key's name part less the 28 characters the dedupe key spends around the name.
_MAX_NAME_LENGTH = 35
_NAME_LENGTH_FIXTURE = "08-name-length-exceeded.yaml"


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("cinderbackend_invalid_cr_generate", _GENERATOR)
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

    def test_name_budget_within_webhook_bound(self) -> None:
        """Only the name-bound fixture may exceed the shared 47-character budget.

        It sits exactly one character past it, so the fixture keeps pinning the
        budget itself rather than some longer pair that would still be rejected
        if the budget moved. Every other fixture has to clear it, otherwise the
        create carries a second, unrelated metadata.name error next to the one
        the fixture exists to pin.
        """
        for fixture in self.generator.FIXTURES:
            budget = len(fixture.name) + len(fixture.cinder_ref)
            if fixture.filename == _NAME_BOUND_FIXTURE:
                self.assertEqual(budget, _MAX_NAME_BUDGET + 1)
                continue
            self.assertLessEqual(
                budget,
                _MAX_NAME_BUDGET,
                f"{fixture.filename} exceeds MaxBackendNamePlusCinderRef",
            )

    def test_name_length_within_webhook_bound(self) -> None:
        """Only the name-length fixture may exceed MaxBackendNameLength.

        Same isolation contract as the shared budget above: the fixture sits
        exactly one character past the bound, and every other fixture clears it
        so no create carries a second, unrelated metadata.name error next to the
        one it exists to pin.
        """
        for fixture in self.generator.FIXTURES:
            if fixture.filename == _NAME_LENGTH_FIXTURE:
                self.assertEqual(len(fixture.name), _MAX_NAME_LENGTH + 1)
                continue
            self.assertLessEqual(
                len(fixture.name),
                _MAX_NAME_LENGTH,
                f"{fixture.filename} exceeds MaxBackendNameLength",
            )


if __name__ == "__main__":
    unittest.main()
