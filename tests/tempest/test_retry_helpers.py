#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for the Tempest retry helpers.

Covers hack/tempest/extract-failed.py end-to-end and
hack/tempest/merge-retry-junit.py via its importable rewrite_junit() entry
point. The subunit stream loader is not exercised here so the tests can run
on any host with only defusedxml installed.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import os
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET
from pathlib import Path


_PROJECT_ROOT = Path(__file__).resolve().parent.parent.parent
_HACK_TEMPEST = _PROJECT_ROOT / "hack" / "tempest"


def _load(mod_name: str, path: Path):
    spec = importlib.util.spec_from_file_location(mod_name, path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


extract_failed = _load("extract_failed", _HACK_TEMPEST / "extract-failed.py")
merge_retry_junit = _load(
    "merge_retry_junit", _HACK_TEMPEST / "merge-retry-junit.py"
)


class _JunitFixtureMixin:
    def _write_junit(self, xml_body: str) -> str:
        fh = tempfile.NamedTemporaryFile(
            mode="w", suffix=".xml", delete=False, encoding="utf-8"
        )
        fh.write(xml_body)
        fh.close()
        self.addCleanup(os.unlink, fh.name)
        return fh.name


class ExtractFailedTests(_JunitFixtureMixin, unittest.TestCase):
    def _run(self, junit_path: str) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        saved_argv = sys.argv
        try:
            sys.argv = ["extract-failed.py", junit_path]
            with (
                contextlib.redirect_stdout(stdout),
                contextlib.redirect_stderr(stderr),
            ):
                rc = extract_failed.main()
        finally:
            sys.argv = saved_argv
        return rc, stdout.getvalue(), stderr.getvalue()

    def test_happy_path_single_failure(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuite name="tempest" tests="2" failures="1" errors="0">'
            '  <testcase classname="tempest.api.foo.Bar" name="test_ok"/>'
            '  <testcase classname="tempest.api.foo.Bar" name="test_bad">'
            "    <failure>boom</failure>"
            "  </testcase>"
            "</testsuite>"
        )
        rc, out, _ = self._run(junit)
        self.assertEqual(rc, 0)
        self.assertEqual(
            out.strip().splitlines(),
            [r"^tempest\.api\.foo\.Bar\.test_bad(\[|$)"],
        )

    def test_parametrized_variants_dedup_to_one_pattern(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuite name="tempest">'
            '  <testcase classname="tempest.api.foo.Bar"'
            '            name="test_bad[id-abc]">'
            "    <failure>boom</failure>"
            "  </testcase>"
            '  <testcase classname="tempest.api.foo.Bar"'
            '            name="test_bad[id-xyz]">'
            "    <error>crash</error>"
            "  </testcase>"
            "</testsuite>"
        )
        rc, out, _ = self._run(junit)
        self.assertEqual(rc, 0)
        lines = out.strip().splitlines()
        self.assertEqual(
            lines, [r"^tempest\.api\.foo\.Bar\.test_bad(\[|$)"]
        )

    def test_regex_metacharacters_escaped(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            "<testsuite>"
            '  <testcase classname="tempest.api.v3.Foo+Bar"'
            '            name="test_q[id-x]">'
            "    <failure/>"
            "  </testcase>"
            "</testsuite>"
        )
        rc, out, _ = self._run(junit)
        self.assertEqual(rc, 0)
        line = out.strip()
        # classname dots escaped
        self.assertIn(r"tempest\.api\.v3", line)
        # + character escaped (regex metachar)
        self.assertIn(r"\+", line)
        # still the anchored form
        self.assertTrue(line.startswith("^"))
        self.assertTrue(line.endswith(r"(\[|$)"))

    def test_no_failures_produces_no_output(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            "<testsuite>"
            '  <testcase classname="pkg.C" name="test_ok"/>'
            "</testsuite>"
        )
        rc, out, _ = self._run(junit)
        self.assertEqual(rc, 0)
        self.assertEqual(out.strip(), "")

    def test_mixed_failure_and_error_both_listed(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            "<testsuite>"
            '  <testcase classname="pkg.C" name="f_method">'
            "    <failure/>"
            "  </testcase>"
            '  <testcase classname="pkg.C" name="e_method">'
            "    <error/>"
            "  </testcase>"
            "</testsuite>"
        )
        rc, out, _ = self._run(junit)
        self.assertEqual(rc, 0)
        lines = sorted(out.strip().splitlines())
        self.assertEqual(
            lines,
            [
                r"^pkg\.C\.e_method(\[|$)",
                r"^pkg\.C\.f_method(\[|$)",
            ],
        )

    def test_missing_file_returns_error(self) -> None:
        rc, _, err = self._run("/nonexistent/path/junit.xml")
        self.assertEqual(rc, 1)
        self.assertIn("error reading", err)

    def test_malformed_xml_returns_error(self) -> None:
        junit = self._write_junit("this is not xml")
        rc, _, err = self._run(junit)
        self.assertEqual(rc, 1)
        self.assertIn("error reading", err)


class RewriteJunitTests(_JunitFixtureMixin, unittest.TestCase):
    def test_resolved_failure_rewritten_as_flake(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuites errors="0" failures="1">'
            '  <testsuite name="tempest" tests="1" failures="1" errors="0">'
            '    <testcase classname="pkg.C" name="test_bad">'
            "      <failure>boom</failure>"
            "    </testcase>"
            "  </testsuite>"
            "</testsuites>"
        )
        fixed, still = merge_retry_junit.rewrite_junit(
            junit, {"pkg.C.test_bad"}
        )
        self.assertEqual((fixed, still), (1, 0))

        root = ET.parse(junit).getroot()
        self.assertEqual(root.attrib["failures"], "0")
        ts = root.find("testsuite")
        assert ts is not None
        self.assertEqual(ts.attrib["failures"], "0")
        tc = ts.find("testcase")
        assert tc is not None
        self.assertIsNone(tc.find("failure"))
        sysout = tc.find("system-out")
        assert sysout is not None
        self.assertIn("flaky", sysout.text or "")

    def test_still_failing_left_untouched(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuite name="tempest" tests="1" failures="1" errors="0">'
            '  <testcase classname="pkg.C" name="test_bad">'
            "    <failure>boom</failure>"
            "  </testcase>"
            "</testsuite>"
        )
        fixed, still = merge_retry_junit.rewrite_junit(junit, set())
        self.assertEqual((fixed, still), (0, 1))

        root = ET.parse(junit).getroot()
        self.assertEqual(root.attrib["failures"], "1")
        tc = root.find("testcase")
        assert tc is not None
        self.assertIsNotNone(tc.find("failure"))

    def test_mixed_failure_and_error_decrements_both_counters(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuites errors="1" failures="1">'
            '  <testsuite name="tempest" tests="1" failures="1" errors="1">'
            '    <testcase classname="pkg.C" name="test_bad">'
            "      <failure>boom</failure>"
            "      <error>also boom</error>"
            "    </testcase>"
            "  </testsuite>"
            "</testsuites>"
        )
        fixed, _ = merge_retry_junit.rewrite_junit(
            junit, {"pkg.C.test_bad"}
        )
        self.assertEqual(fixed, 1)

        root = ET.parse(junit).getroot()
        self.assertEqual(root.attrib["failures"], "0")
        self.assertEqual(root.attrib["errors"], "0")
        ts = root.find("testsuite")
        assert ts is not None
        self.assertEqual(ts.attrib["failures"], "0")
        self.assertEqual(ts.attrib["errors"], "0")

    def test_parametrized_variants_handled_independently(self) -> None:
        # Regression test for W-001: one variant passes on retry, another
        # still fails. Only the passing variant must be rewritten.
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuites errors="0" failures="2">'
            '  <testsuite name="tempest" tests="2" failures="2" errors="0">'
            '    <testcase classname="pkg.C" name="test_bad[id-ok]">'
            "      <failure>boom</failure>"
            "    </testcase>"
            '    <testcase classname="pkg.C" name="test_bad[id-still-bad]">'
            "      <failure>boom</failure>"
            "    </testcase>"
            "  </testsuite>"
            "</testsuites>"
        )
        fixed, still = merge_retry_junit.rewrite_junit(
            junit, {"pkg.C.test_bad[id-ok]"}
        )
        self.assertEqual((fixed, still), (1, 1))

        root = ET.parse(junit).getroot()
        self.assertEqual(root.attrib["failures"], "1")
        ts = root.find("testsuite")
        assert ts is not None
        self.assertEqual(ts.attrib["failures"], "1")

        cases = {
            tc.attrib["name"]: tc for tc in ts.findall("testcase")
        }
        self.assertIsNone(cases["test_bad[id-ok]"].find("failure"))
        self.assertIsNotNone(
            cases["test_bad[id-still-bad]"].find("failure")
        )

    def test_empty_passed_on_retry_is_noop(self) -> None:
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuite name="tempest">'
            '  <testcase classname="pkg.C" name="test_ok"/>'
            "</testsuite>"
        )
        fixed, still = merge_retry_junit.rewrite_junit(junit, set())
        self.assertEqual((fixed, still), (0, 0))

    def test_non_numeric_counter_does_not_crash(self) -> None:
        # Regression test for I-001: a malformed counter attribute must not
        # take down the whole merge.
        junit = self._write_junit(
            '<?xml version="1.0"?>'
            '<testsuite name="tempest" failures="NaN" errors="0">'
            '  <testcase classname="pkg.C" name="test_bad">'
            "    <failure>boom</failure>"
            "  </testcase>"
            "</testsuite>"
        )
        fixed, _ = merge_retry_junit.rewrite_junit(
            junit, {"pkg.C.test_bad"}
        )
        self.assertEqual(fixed, 1)

        root = ET.parse(junit).getroot()
        # Counter left as-is (unparseable) but the testcase was still
        # rewritten without an exception.
        self.assertEqual(root.attrib["failures"], "NaN")
        tc = root.find("testcase")
        assert tc is not None
        self.assertIsNone(tc.find("failure"))


if __name__ == "__main__":
    unittest.main()
