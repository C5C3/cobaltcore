#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the invalid CinderBackupBackend fixture generator.

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
* The two accepted base fixtures carry different spec.cinderRef names, so
  neither trips the single-attachment rule the other pins.
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

# Number of fixtures emitted by _generate.py: four create-rejection fixtures
# (00-03), the cinderRef-immutability pair (04-base, 05-update), the
# single-attachment pair (06-base, 07-second) and the export-injection fixture
# (08). Bumping this value requires adding the matching Fixture entry AND the
# matching `file: <name>` line in chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 9

# The fixtures Chainsaw applies expecting success. They persist for the rest of
# the run, so the single-attachment rule must not see them as siblings of each
# other.
_ACCEPTED_BASES = ("04-immutable-cinderref-base.yaml", "06-second-backup-base.yaml")


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("cinderbackupbackend_invalid_cr_generate", _GENERATOR)
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

    def test_accepted_bases_use_distinct_cinderrefs(self) -> None:
        """Both bases persist, and one Cinder takes one backup backend."""
        refs = [
            fixture.cinder_ref
            for fixture in self.generator.FIXTURES
            if fixture.filename in _ACCEPTED_BASES
        ]
        self.assertEqual(len(refs), len(_ACCEPTED_BASES), f"missing base fixture: {refs}")
        self.assertEqual(len(refs), len(set(refs)), f"bases share a cinderRef: {refs}")


if __name__ == "__main__":
    unittest.main()
