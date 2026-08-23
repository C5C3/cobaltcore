#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the KeystoneService invalid-CR fixture generator.

Mirrors tests/e2e/c5c3/invalid-cr/test_generate.py and
tests/e2e/keystone/invalid-identitybackend-cr/test_generate.py: guards the
canonical-scaffold contract at a layer that runs without a Kubernetes cluster,
so an accidental fixture removal or rename is caught here in milliseconds
instead of waiting for the Chainsaw E2E job to fail at the apply step.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml``, which guards against renames or accidental deletions.
* Every ``apply.file:`` entry in ``chainsaw-test.yaml`` names a declared
  fixture. That is the other direction: a block left behind for a fixture
  ``FIXTURES`` no longer declares reaches the cluster-bound job as an apply of
  a missing file.
* Filenames are unique within ``FIXTURES``.
* No fixture carries a metadata.namespace (Chainsaw injects the ephemeral one).
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
* The webhook name the negative arms in ``chainsaw-test.yaml`` scan for is the
  one the Helm chart registers, so those arms cannot go vacuous behind a rename.
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
# The chart the e2e cluster installs, which is what actually registers the
# validating webhook. It hardcodes the name independently of the
# +kubebuilder:webhook marker, so the chart is the source of truth here.
_CHART_WEBHOOK_TEMPLATE = (
    _HERE.parents[3]
    / "operators/c5c3/helm/c5c3-operator/templates/webhook-configuration.yaml"
)

# Number of fixtures emitted by _generate.py. Bumping this value requires adding
# the matching Fixture entry AND the matching `file: <name>` line in
# chainsaw-test.yaml.
_EXPECTED_FIXTURE_COUNT = 26


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location(
        "c5c3_invalid_keystoneservice_cr_generate", _GENERATOR
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
        # The reverse of the check above. Removing a fixture means deleting both
        # its Fixture entry and its try:/apply: block; a leftover block passes
        # `_generate.py --check` (nothing on disk drifted) and passes chainsaw
        # lint (which validates schema, not referenced paths), so without this it
        # only fails once the cluster-bound e2e-operator job applies it.
        declared = {fixture.filename for fixture in self.generator.FIXTURES}
        for name in re.findall(r"file:\s*(\S+)", self.chainsaw):
            self.assertIn(
                name,
                declared,
                f"chainsaw-test.yaml applies {name}, which FIXTURES no longer declares",
            )

    def test_no_fixture_pins_a_namespace(self) -> None:
        # Chainsaw injects the ephemeral namespace; a hardcoded one would pin
        # every fixture of this suite into a single shared namespace.
        #
        # Anchored at the metadata indent level (2 spaces, where metadata.name
        # sits). The only namespace field a fixture may legitimately carry is
        # spec.controlPlaneRef.namespace, which renders at 4 spaces, so the
        # anchored scan cannot false-positive on it.
        for fixture in self.generator.FIXTURES:
            self.assertNotIn(
                "\n  namespace:",
                fixture.render(),
                f"{fixture.filename} must not pin a metadata.namespace",
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

    def test_webhook_name_matches_deployed_chart(self) -> None:
        # The negative arms in chainsaw-test.yaml pin the CEL layer by asserting
        # the rejection carries NO webhook denial. They stay non-vacuous only
        # while the name they scan for is the one the deployed webhook actually
        # carries: against a renamed webhook every rejection satisfies them,
        # including the webhook denial they exist to exclude. Renaming the
        # webhook fails the chart's own helm unittest, which points at the chart
        # test — this arm is what points at the chainsaw copy too.
        chart = _CHART_WEBHOOK_TEMPLATE.read_text(encoding="utf-8")
        match = re.search(r'"validatingWebhookName"\s+"([^"]*keystoneservice[^"]*)"', chart)
        self.assertIsNotNone(match, "chart declares no KeystoneService validating webhook")
        name = match.group(1)
        self.assertIn(
            f"(contains($error, '{name}')): false",
            self.chainsaw,
            f"chainsaw-test.yaml pins a webhook name the chart no longer registers "
            f"(chart declares {name}); the negative arms are vacuous",
        )

    def test_rendered_fixture_carries_spdx_header(self) -> None:
        for fixture in self.generator.FIXTURES:
            rendered = fixture.render()
            self.assertTrue(
                rendered.startswith("# SPDX-FileCopyrightText:"),
                f"{fixture.filename} must start with the SPDX header",
            )


if __name__ == "__main__":
    unittest.main()
