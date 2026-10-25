#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Fast unit tests for the c5c3 invalid-CR fixture generator.

Mirrors tests/e2e/keystone/invalid-cr/test_generate.py and
tests/e2e/horizon/invalid-cr/test_generate.py: guards the canonical-scaffold
contract at a layer that runs without a Kubernetes cluster — accidental fixture
removal/rename is caught here in milliseconds instead of waiting for the
Chainsaw E2E job to fail at the apply step.

Coverage:

* ``FIXTURES`` lists exactly the generated fixtures the chainsaw suite expects.
* Every ``Fixture.filename`` is referenced by an ``apply.file:`` entry in
  ``chainsaw-test.yaml`` — guards against renames or accidental deletions.
* Every ``apply.file:`` entry in ``chainsaw-test.yaml`` names a declared fixture
  — guards the other direction: a block left behind for a fixture ``FIXTURES``
  no longer declares reaches the cluster-bound job as an apply of a missing file.
* Filenames are unique within ``FIXTURES``.
* No fixture carries a metadata.namespace (Chainsaw injects the ephemeral one).
* ``_generate.py --check`` passes in-process, so on-disk drift (either
  direction, including orphan files) fails the unit test.
* The webhook name the negative arms in ``chainsaw-test.yaml`` scan for is the
  one the Helm chart registers, so those arms cannot go vacuous behind a rename.
* The release-downgrade pair lowers only spec.openStackRelease, from a release
  above ``VALID_RELEASE`` to ``VALID_RELEASE``.
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
_EXPECTED_FIXTURE_COUNT = 163


def _load_generator() -> types.ModuleType:
    spec = importlib.util.spec_from_file_location("c5c3_invalid_cr_generate", _GENERATOR)
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
        # Chainsaw injects the ephemeral namespace; a hardcoded namespace would
        # break the one-ControlPlane-per-namespace isolation across Tests.
        #
        # Anchored at the metadata indent level (2 spaces, where metadata.name
        # sits). A bare substring scan would also catch the deeply-indented
        # spec.services.<svc>.namespace field, which pins no metadata.namespace at
        # all — it names the namespace a SERVICE is placed in.
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

    def test_remotecompute_fixtures_publish_through_public_endpoints(self) -> None:
        # Fixtures 114 to 118 each break one remote-compute rule and publish every
        # service the other rules name. A gateway anywhere would bring in the
        # gateway host rules (105-nova-public-endpoint-host-mismatch.yaml), which
        # could answer in place of the rule under test.
        remote = [f for f in self.generator.FIXTURES if "-nova-remotecompute-" in f.filename]
        self.assertEqual(len(remote), 5)
        for fixture in remote:
            rendered = fixture.render()
            self.assertIn("remoteCompute:", rendered, fixture.filename)
            self.assertNotIn("gateway:", rendered, f"{fixture.filename} must publish through publicEndpoint alone")

    def test_namespace_assignment_fixtures_render_at_spec_level(self) -> None:
        # Fixtures 138 to 141 each carry a spec.namespaceAssignments block. It is
        # a spec-level list beside korc, not a korc child, so it must render at
        # indent 2 ahead of the korc block, and no other fixture may carry one.
        assigned = [f for f in self.generator.FIXTURES if "namespaceAssignments:" in f.render()]
        self.assertEqual(
            sorted(f.filename for f in assigned),
            sorted(f.filename for f in self.generator.FIXTURES if "-namespace-assignment-" in f.filename),
        )
        self.assertEqual(len(assigned), 4)
        for fixture in assigned:
            rendered = fixture.render()
            self.assertIn("\n  namespaceAssignments:\n", rendered, fixture.filename)
            self.assertLess(
                rendered.index("\n  namespaceAssignments:\n"),
                rendered.index("\n  korc:\n"),
                f"{fixture.filename} must render the assignments before korc",
            )

    def test_release_downgrade_pair_lowers_only_the_release(self) -> None:
        # c5c3-invalid-cr-release-downgrade expects the update from 158 to 159 to
        # draw the downgrade error alone. That needs 159 at VALID_RELEASE (the
        # floor), 158 above it, and no other difference. A pair at one release
        # is admitted, which only the cluster-bound e2e-operator job would see.
        by_name = {fixture.filename: fixture for fixture in self.generator.FIXTURES}
        base = by_name["158-transition-base-release.yaml"]
        down = by_name["159-transition-release-downgrade.yaml"]
        self.assertEqual(down.release, self.generator.VALID_RELEASE)
        base_key, down_key = (tuple(map(int, f.release.split("."))) for f in (base, down))
        self.assertGreater(base_key, down_key)
        base_lines, down_lines = (
            [
                line
                for line in f.render().splitlines()
                if not line.startswith("#") and "openStackRelease:" not in line
            ]
            for f in (base, down)
        )
        self.assertEqual(base_lines, down_lines)

    def test_webhook_name_matches_deployed_chart(self) -> None:
        # The negative arms on the CEL steps assert the rejection carries NO
        # ControlPlane webhook denial. Against a renamed webhook every rejection
        # satisfies them, the webhook denial they exist to exclude included, so
        # the name they scan for has to be the one the chart registers.
        chart = _CHART_WEBHOOK_TEMPLATE.read_text(encoding="utf-8")
        match = re.search(r'"validatingWebhookName"\s+"([^"]*controlplane[^"]*)"', chart)
        self.assertIsNotNone(match, "chart declares no ControlPlane validating webhook")
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
