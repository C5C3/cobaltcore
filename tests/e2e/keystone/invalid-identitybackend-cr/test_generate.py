#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the invalid KeystoneIdentityBackend fixture generator.

Guards the canonical-scaffold contract at a layer that runs without a
Kubernetes cluster — accidental fixture removal/rename is caught here in
milliseconds instead of waiting for the Chainsaw E2E job to fail at the apply
step.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml`` — guards against renames or accidental deletions.
* Filenames are unique within ``FIXTURES`` — guards against copy/paste typos
  collapsing two fixtures onto a single on-disk file.

The generator itself is also exercised: ``_generate.py --check`` is invoked
in-process so a drift between ``FIXTURES`` and the on-disk fixtures fails the
unit test, mirroring the Makefile / CI ``verify-invalid-cr-fixtures`` target
without a subprocess hop.
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

# Number of fixtures emitted by _generate.py: nine create-rejection fixtures
# (00-08), the update-rejection set (09-immutable-base and 10-12), the
# duplicate-domain fixture (13), the two control-character INI-injection
# create-rejection fixtures (14-15), and the two extraOptions key-shape
# fixtures (16 key control-char, 17 denylist-evading trailing space). Bumping
# this value requires adding the matching Fixture entry AND the matching
# `file: <name>` line in chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 18


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("identitybackend_generate", _GENERATOR)
    assert spec and spec.loader, f"failed to load spec for {_GENERATOR}"
    module = importlib.util.module_from_spec(spec)
    # Register before exec_module so @dataclass(frozen=True) can resolve
    # cls.__module__ via sys.modules during class construction.
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_generate = _load_generator()


class FixturesContractTests(unittest.TestCase):
    """The ``FIXTURES`` list pins the chainsaw suite contract."""

    def test_fixture_count_matches_expected(self) -> None:
        self.assertEqual(
            len(_generate.FIXTURES),
            _EXPECTED_FIXTURE_COUNT,
            "FIXTURES count drifted from chainsaw-test.yaml expectation; "
            "update both _generate.py and chainsaw-test.yaml together.",
        )

    def test_fixture_filenames_are_unique(self) -> None:
        filenames = [f.filename for f in _generate.FIXTURES]
        self.assertEqual(
            len(filenames),
            len(set(filenames)),
            f"duplicate Fixture.filename in _generate.py: {filenames}",
        )

    def test_every_fixture_filename_is_referenced_by_chainsaw_test(self) -> None:
        chainsaw_text = _CHAINSAW_TEST.read_text(encoding="utf-8")
        # `apply.file:` references appear as `file: NN-name.yaml` in the
        # chainsaw step body. Match the literal `file: ` prefix to avoid
        # false positives from comment text mentioning the filename.
        referenced = set(
            re.findall(r"^\s*file:\s*([\w.-]+)$", chainsaw_text, re.MULTILINE)
        )
        for fixture in _generate.FIXTURES:
            self.assertIn(
                fixture.filename,
                referenced,
                f"Fixture {fixture.filename!r} is not referenced by any "
                f"`file:` step in {_CHAINSAW_TEST.name}; either add the step "
                f"or remove the fixture from FIXTURES.",
            )

    def test_keystoneref_update_fixture_repoints_reference(self) -> None:
        # The keystoneRef-update variant is the one fixture the generator
        # splices after rendering; guard the splice so a scaffold change
        # cannot silently turn the UPDATE into a no-op that admission accepts.
        fixture = next(
            f for f in _generate.FIXTURES if f.filename == "10-immutable-keystoneref.yaml"
        )
        rendered = _generate.render_fixture(fixture)
        self.assertIn("name: keystone-other", rendered)
        self.assertNotIn("name: keystone\n", rendered.split("domain:")[0].split("keystoneRef:")[1])


class GeneratorDriftTests(unittest.TestCase):
    """``_generate.py --check`` must report no drift against on-disk fixtures."""

    def test_check_mode_returns_zero(self) -> None:
        self.assertEqual(
            _generate.main(["--check"]),
            0,
            "_generate.py --check reported drift; re-run "
            "`python3 tests/e2e/keystone/invalid-identitybackend-cr/_generate.py` to refresh.",
        )


if __name__ == "__main__":
    unittest.main()
