#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the SizingProfile invalid-CR fixture generator.

Mirrors tests/e2e/c5c3/invalid-keystoneservice-cr/test_generate.py: guards
the canonical-scaffold contract at a layer that runs without a Kubernetes
cluster, so an accidental fixture removal or rename is caught here in
milliseconds instead of waiting for the Chainsaw E2E job to fail at the apply
step.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml``, and every ``apply.file:`` entry names a declared
  fixture.
* Filenames are unique within ``FIXTURES``.
* No fixture carries a namespace: the kind is cluster-scoped.
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
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
_EXPECTED_FIXTURE_COUNT = 7


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location(
        "c5c3_invalid_sizingprofile_cr_generate", _GENERATOR
    )
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

    def test_no_orphan_chainsaw_reference(self) -> None:
        declared = {fixture.filename for fixture in self.generator.FIXTURES}
        for name in re.findall(r"file:\s*(\S+)", self.chainsaw):
            self.assertIn(
                name,
                declared,
                f"chainsaw-test.yaml applies {name}, which FIXTURES no longer declares",
            )

    def test_no_fixture_carries_a_namespace(self) -> None:
        # SizingProfile is cluster-scoped: a namespace would be ignored at best
        # and would suggest the kind is namespaced.
        for fixture in self.generator.FIXTURES:
            self.assertNotIn(
                "namespace:",
                fixture.render(),
                f"{fixture.filename} must not carry a namespace",
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


if __name__ == "__main__":
    unittest.main()
