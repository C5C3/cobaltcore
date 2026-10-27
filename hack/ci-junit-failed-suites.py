#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Print the failed suites of chainsaw JUnit reports.

Chainsaw's JUNIT-TEST report holds one ``<testsuite>`` per suite, named by the
suite's directory (``tests/e2e-chaos/<suite>``), with one ``<testcase>`` that
carries ``<failure message="...">`` when the suite failed. For every testsuite
whose testcase carries a ``<failure>`` or an ``<error>``, this prints one line
``<suite>\\t<message>``: the last path element of the testsuite name and the
first line of the message attribute, ``-`` when it is empty. Lines follow the
argument order, then document order.

hack/ci-report-nightly-chaos.sh runs it as ``python3 -I`` on the reports of a
red ``e2e-chaos`` leg and files one issue per printed suite.

Exit codes: 0 the reports were read (no argument and no failure print
nothing); 2 a report is no readable XML, reported as
``ERROR: <path> is no JUnit report: <reason>`` on stderr before any suite is
printed. Only the standard library is used.
"""

from __future__ import annotations

import sys
import xml.etree.ElementTree as ET

EXIT_OK = 0
EXIT_INPUT = 2


def failure_message(testsuite: ET.Element) -> str | None:
    """Return the message of the testsuite's first failure, or None.

    The message is the first line of the attribute with tabs turned into
    spaces, so the printed line keeps two fields; an empty one reads ``-``.
    """
    for testcase in testsuite.iter("testcase"):
        for child in testcase:
            if child.tag in ("failure", "error"):
                lines = (child.get("message") or "").splitlines()
                first = lines[0].replace("\t", " ").strip() if lines else ""
                return first or "-"
    return None


def main(argv: list[str]) -> int:
    """Print the failed suites of the reports named in argv."""
    roots = []
    for path in argv:
        try:
            roots.append(ET.parse(path).getroot())
        except (ET.ParseError, OSError) as exc:
            print(f"ERROR: {path} is no JUnit report: {exc}", file=sys.stderr)
            return EXIT_INPUT
    for root in roots:
        for testsuite in root.iter("testsuite"):
            message = failure_message(testsuite)
            if message is None:
                continue
            suite = (testsuite.get("name") or "").rstrip("/").rsplit("/", 1)[-1]
            print(f"{suite or '-'}\t{message}")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
