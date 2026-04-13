#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Merge retry subunit results into a Tempest JUnit XML report.

Reads the final outcome per test_id from a subunit v2 stream produced by a
retry `stestr run` and, for each testcase in the JUnit XML that failed on
the initial run but succeeded on retry, removes the failure/error children,
decrements the failures/errors counters on the enclosing `<testsuite>` (and
`<testsuites>` root if present), and appends a `<system-out>` note marking
the test as a resolved flake. Tests that still fail after retry are left
untouched.

The JUnit file is updated in place. A one-line summary is printed to stderr
with the count of resolved flakes and still-failing tests.
"""
from __future__ import annotations

import sys
import xml.etree.ElementTree as ET

from defusedxml.ElementTree import parse as _safe_parse


def _load_retry_outcomes(path: str) -> dict[str, str]:
    # Imported lazily so unit tests for rewrite_junit() can run without the
    # subunit/testtools packages installed on the host.
    from subunit.v2 import ByteStreamToStreamResult
    from testtools import StreamResult

    class _OutcomeCollector(StreamResult):
        """Collect the final terminal outcome per full test_id."""

        def __init__(self) -> None:
            super().__init__()
            self.outcomes: dict[str, str] = {}

        def status(self, test_id=None, test_status=None, **_: object) -> None:
            if not test_id or not test_status:
                return
            if test_status in {"success", "fail", "skip", "xfail", "uxsuccess"}:
                # Key by the full test_id (including any parametrization tag)
                # so variants of the same method are tracked independently
                # and cannot clobber each other last-wins.
                self.outcomes[test_id] = test_status

    collector = _OutcomeCollector()
    with open(path, "rb") as fh:
        stream = ByteStreamToStreamResult(fh, non_subunit_name="retry")
        stream.run(collector)
    return collector.outcomes


def _dec(elem: ET.Element, attr: str) -> None:
    try:
        value = int(elem.attrib.get(attr, "0"))
    except ValueError:
        return
    if value > 0:
        elem.attrib[attr] = str(value - 1)


def rewrite_junit(junit_path: str, passed_on_retry: set[str]) -> tuple[int, int]:
    """Rewrite resolved failures as flakes, in place. Returns (fixed, still_failing)."""
    tree = _safe_parse(junit_path)
    root = tree.getroot()

    fixed = 0
    still_failing = 0
    # Iterate direct-child testcases only: using ts.iter("testcase") would
    # re-visit nested testcases once per ancestor testsuite, which would
    # double-remove children and over-adjust counters.
    for ts in root.iter("testsuite"):
        for tc in list(ts.findall("testcase")):
            classname = tc.attrib.get("classname", "")
            name = tc.attrib.get("name", "")
            if not classname or not name:
                continue
            # Match JUnit testcases by their full classname.name (including
            # any "[id-...]" parametrization tag) so parametrized variants
            # are rewritten independently — see W-001.
            test_id = f"{classname}.{name}"
            fail_children = [c for c in tc if c.tag in ("failure", "error")]
            if not fail_children:
                continue
            if test_id in passed_on_retry:
                kinds = {c.tag for c in fail_children}
                for child in fail_children:
                    tc.remove(child)
                note = ET.SubElement(tc, "system-out")
                note.text = "flaky: failed on first run, passed on retry"
                for kind in kinds:
                    attr = "failures" if kind == "failure" else "errors"
                    _dec(ts, attr)
                    if root.tag == "testsuites":
                        _dec(root, attr)
                fixed += 1
            else:
                still_failing += 1

    tree.write(junit_path, xml_declaration=True, encoding="utf-8")
    return fixed, still_failing


def main() -> int:
    if len(sys.argv) != 3:
        print(
            "usage: merge-retry-junit.py JUNIT_XML RETRY_SUBUNIT",
            file=sys.stderr,
        )
        return 2

    junit_path, retry_path = sys.argv[1], sys.argv[2]

    outcomes = _load_retry_outcomes(retry_path)
    # xfail (expected failure) still represents a failing test by design, so
    # only real passes count as "passed on retry". uxsuccess is an unexpected
    # pass for a test marked xfail — the test code did not raise, so treat it
    # as a pass for flake-rewrite purposes.
    passed_on_retry = {
        tid for tid, status in outcomes.items()
        if status in {"success", "uxsuccess"}
    }

    fixed, still_failing = rewrite_junit(junit_path, passed_on_retry)
    print(
        f"Retry merge: {fixed} flaky test(s) passed on retry, "
        f"{still_failing} still failing.",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
