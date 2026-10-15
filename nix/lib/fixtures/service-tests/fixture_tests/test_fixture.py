# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""One passing and one failing test for the service-tests check of flake.nix.

The check builds the run of nix/lib/service-tests.nix over this suite and
asserts that a run with a failing test still writes its exit status, its
subunit stream and a summary that counts the failure. The failure message
names the interpreter's store path, which nuke-refs blanks inside the stream.
"""

import sys

import testtools


class FixtureTest(testtools.TestCase):
    """The two outcomes a suite reports."""

    def test_passes(self) -> None:
        """Pass."""
        self.assertEqual(2, 1 + 1)

    def test_fails(self) -> None:
        """Fail on purpose, naming a store path nuke-refs blanks."""
        self.fail(f"this fixture test fails on purpose under {sys.executable}")
