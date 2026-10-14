# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Recompute the CRC32 that ends each packet of a subunit v2 stream.

nuke-refs blanks the store-path hashes inside the packets and leaves their
checksums as they were, so a subunit reader rejects every packet it touched
as a parse error. The blanking keeps every packet's length, and this script
rewrites the stream in place with the checksum of each packet's bytes.

A packet is the signature 0xB3, two bytes of flags with the version 2 in
their top four bits, its own length as a subunit Number (the top two bits of
the first byte give the bytes that follow), the body, and the big-endian
CRC32 of everything before it.

Usage: python3 subunit-crc.py <stream>

Exits 1 when the file is no subunit v2 stream. Only the standard library is
used.
"""

import sys
import zlib

path = sys.argv[1]
with open(path, "rb") as f:
    data = bytearray(f.read())

pos = 0
while pos < len(data):
    extra = data[pos + 3] >> 6 if pos + 3 < len(data) else 0
    length = int.from_bytes(data[pos + 3 : pos + 4 + extra], "big") & ((1 << (8 * extra + 6)) - 1)
    if (
        data[pos] != 0xB3
        or not 8 + extra <= length <= len(data) - pos
        or data[pos + 1] >> 4 != 2
    ):
        sys.exit(f"subunit-crc.py: {path}: no subunit v2 packet at byte {pos}")
    end = pos + length
    data[end - 4 : end] = zlib.crc32(data[pos : end - 4]).to_bytes(4, "big")
    pos = end

with open(path, "wb") as f:
    f.write(data)
