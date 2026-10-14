#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify nix/spike/checks/subunit-crc.py gives the subunit v2 stream of the
# keystone check valid checksums again after nuke-refs blanked the store-path
# hashes in it, and refuses a file that is no subunit v2 stream.
#
# The fixture is a two-packet stream python-subunit's StreamResultToBytes
# wrote: a passing test with a one-byte packet length, then a traceback that
# names a store path, with a two-byte packet length. The input is that stream
# with the hash blanked to e's as nuke-refs does, its last CRC32 still the one
# of the original bytes; the expected output is the stream python-subunit
# writes for the blanked traceback.
#
# Usage: bash tests/unit/images/nix_spike_subunit_crc_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

PASS=0
FAIL=0
SKIP=0

# shellcheck source=tests/lib/assertions.sh
source "$PROJECT_ROOT/tests/lib/assertions.sh"

SUBUNIT_CRC="$PROJECT_ROOT/nix/spike/checks/subunit-crc.py"

# The two packets up to the CRC32 of the second, in hex.
PACKETS=b32903150c64656d6f2e746573745f6f6bbea5839ab3297040ab0c64656d6f2e746573745f74621d746578742f782d74726163
PACKETS+=656261636b3b636861727365743d757466380974726163656261636b406b46696c6520222f6e69782f73746f72652f6565656565
PACKETS+=6565656565656565656565656565656565656565656565656565652d707974686f6e332d332e31322e31332f6c69622f707974686f
PACKETS+=6e332e31322f756e6974746573742f636173652e7079222c206c696e65203538
BLANKED="${PACKETS}deed799e"
EXPECTED="${PACKETS}2401589a"

# write_hex writes the bytes of a hex string to a file.
write_hex() {
  python3 -c 'import sys; sys.stdout.buffer.write(bytes.fromhex(sys.argv[1]))' "$1" >"$2"
}

# read_hex prints the bytes of a file as a hex string.
read_hex() {
  python3 -c 'import sys; print(open(sys.argv[1], "rb").read().hex())' "$1"
}

test_blanked_stream_gets_valid_checksums() {
  echo "Test: a stream whose store-path hashes nuke-refs blanked carries the checksums python-subunit writes"
  local tmp exit_code=0
  tmp=$(mktemp -d)
  write_hex "$BLANKED" "$tmp/testresults.subunit"

  python3 "$SUBUNIT_CRC" "$tmp/testresults.subunit" || exit_code=$?
  assert_eq "the run exits 0" "0" "$exit_code"
  assert_eq "the stream is the one python-subunit writes for the blanked traceback" \
    "$EXPECTED" "$(read_hex "$tmp/testresults.subunit")"
  rm -rf "$tmp"
}

test_other_files_are_refused() {
  echo "Test: a file that is no subunit v2 stream, or ends inside a packet, exits 1 and stays as it was"
  local tmp output exit_code=0
  tmp=$(mktemp -d)

  echo "Ran: 5747 tests" >"$tmp/summary.txt"
  output=$(python3 "$SUBUNIT_CRC" "$tmp/summary.txt" 2>&1) || exit_code=$?
  assert_eq "a text file exits 1" "1" "$exit_code"
  assert_contains "the message names the offset" "$output" "no subunit v2 packet at byte 0"
  assert_eq "the text file is unchanged" "Ran: 5747 tests" "$(cat "$tmp/summary.txt")"

  exit_code=0
  write_hex "${BLANKED%??}" "$tmp/testresults.subunit"
  output=$(python3 "$SUBUNIT_CRC" "$tmp/testresults.subunit" 2>&1) || exit_code=$?
  assert_eq "a stream cut inside its second packet exits 1" "1" "$exit_code"
  assert_contains "the message names the second packet" "$output" "no subunit v2 packet at byte 21"
  assert_eq "the cut stream is unchanged" "${BLANKED%??}" "$(read_hex "$tmp/testresults.subunit")"
  rm -rf "$tmp"
}

test_malformed_packets_are_refused() {
  echo "Test: a packet shorter than its own header, or with flags of another subunit version, exits 1 and stays as it was"
  local tmp output exit_code=0
  tmp=$(mktemp -d)

  # A four-byte length field that declares 8 bytes, the size of a packet
  # whose length field is one byte.
  write_hex b32000c000000800 "$tmp/short.subunit"
  output=$(python3 "$SUBUNIT_CRC" "$tmp/short.subunit" 2>&1) || exit_code=$?
  assert_eq "a packet shorter than its header exits 1" "1" "$exit_code"
  assert_contains "the message names the offset" "$output" "no subunit v2 packet at byte 0"
  assert_eq "the short packet is unchanged" "b32000c000000800" "$(read_hex "$tmp/short.subunit")"

  # The first packet of the fixture with version 1 in the top four bits of
  # its flags.
  exit_code=0
  write_hex b31903150c64656d6f2e746573745f6f6bbea5839a "$tmp/v1.subunit"
  output=$(python3 "$SUBUNIT_CRC" "$tmp/v1.subunit" 2>&1) || exit_code=$?
  assert_eq "a packet of another version exits 1" "1" "$exit_code"
  assert_contains "the message names the offset" "$output" "no subunit v2 packet at byte 0"
  assert_eq "the packet of another version is unchanged" \
    "b31903150c64656d6f2e746573745f6f6bbea5839a" "$(read_hex "$tmp/v1.subunit")"
  rm -rf "$tmp"
}

if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP: python3 not installed (every case needs it)"
  SKIP=$((SKIP + 1))
else
  test_blanked_stream_gets_valid_checksums
  test_other_files_are_refused
  test_malformed_packets_are_refused
fi

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
