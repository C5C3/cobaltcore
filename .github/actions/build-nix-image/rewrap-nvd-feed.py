#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Rewrap one fkie-cad/nvd-json-data-feeds year file into a NIST 2.0 feed.

Usage: rewrap-nvd-feed.py <src> <dst>

<src> is the path of a release asset pair without its extension: <src>.json.xz
holds the feed, <src>.meta its sha256 over the decompressed JSON. The script
checks that hash, moves the CVE items from the mirror's {"cve_items": [cve, …]}
wrapper into the {"vulnerabilities": [{"cve": …}]} wrapper of NIST's 2.0 feeds,
which vulnix parses, and writes <dst> gzip-compressed, the encoding vulnix
expects from a mirror. The items themselves are untouched.
"""

import gzip
import hashlib
import json
import lzma
import sys


def main(argv):
    if len(argv) != 3:
        print(f"usage: {argv[0]} <src> <dst>", file=sys.stderr)
        return 2
    src, dst = argv[1], argv[2]
    with open(f"{src}.meta", encoding="utf-8") as f:
        meta = dict(line.rstrip("\n").split(":", 1) for line in f if ":" in line)
    with lzma.open(f"{src}.json.xz") as f:
        raw = f.read()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != meta.get("sha256"):
        print(
            f"::error::{src}.json.xz: sha256 {digest} does not match "
            f"{meta.get('sha256')} from {src}.meta"
        )
        return 1
    feed = json.loads(raw)
    del raw
    if "cve_items" not in feed:
        print(f"::error::{src}.json.xz has no cve_items; the feed format changed")
        return 1
    with gzip.open(dst, "wt", encoding="utf-8", compresslevel=1) as f:
        json.dump({"vulnerabilities": [{"cve": c} for c in feed["cve_items"]]}, f)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
