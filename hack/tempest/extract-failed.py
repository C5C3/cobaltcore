#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Print dotted IDs of failed/errored testcases from a JUnit XML file.

Used by the Tempest runners to build an `--include-list` for a serial retry
pass over flaky tests. Each line printed is a `classname.name` pair with regex
metacharacters escaped and anchored so that stestr's regex matcher rematches
the same test (and all of its parametrized id-tag variants) on retry.
"""
from __future__ import annotations

import re
import sys
import xml.etree.ElementTree as ET

from defusedxml.ElementTree import parse as _safe_parse


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: extract-failed.py JUNIT_XML", file=sys.stderr)
        return 2
    try:
        root = _safe_parse(sys.argv[1]).getroot()
    except (OSError, ET.ParseError) as exc:
        print(f"error reading {sys.argv[1]}: {exc}", file=sys.stderr)
        return 1

    seen: set[str] = set()
    for tc in root.iter("testcase"):
        classname = tc.attrib.get("classname", "")
        name = tc.attrib.get("name", "")
        if not classname or not name:
            continue
        if not any(child.tag in ("failure", "error") for child in tc):
            continue
        # Strip any trailing "[id-...]" tag from the method name so we retry
        # the test method itself, not a single parametrized variant.
        bare_name = re.sub(r"\[[^\]]*\]$", "", name)
        key = f"{classname}.{bare_name}"
        if key in seen:
            continue
        seen.add(key)
        # Anchor to the full classname.method prefix and allow an optional
        # id-tag suffix so stestr matches all variants of the method.
        pattern = "^" + re.escape(key) + r"(\[|$)"
        print(pattern)
    return 0


if __name__ == "__main__":
    sys.exit(main())
