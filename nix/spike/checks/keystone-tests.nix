# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The keystone 2026.1 unit tests as a flake check: stestr over the patched
# source tree with the venv that carries the test dependency group, as
# hack/ci-run-unit-tests.sh runs them in the venv-builder container, but
# offline in the build sandbox.
#
# The exclude list of the release applies. A test that fails only inside the
# sandbox (no network, no passwd entry for the build user) goes into
# sandboxExcludes below with its error, never into releases/, so the release
# excludes stay those CI applies. With OS_TEST_DBAPI_ADMIN_CONNECTION unset,
# oslo.db's opportunistic MySQL and PostgreSQL tests skip, as they do in a CI
# run with the variable empty.
#
# $out/testresults.subunit is the subunit stream of the run, $out/summary.txt
# the totals of `stestr last`. The check fails when `stestr run` fails.
# nuke-refs blanks the hashes of the store paths the tracebacks name, so the
# output references neither the test venv nor the source. subunit-crc.py then
# recomputes the packet checksums nuke-refs left stale; a subunit reader
# rejects a packet with a stale checksum as a parse error.
#
# stestr starts one worker per core Nix grants the build (NIX_BUILD_CORES),
# not one per host CPU.
{
  pkgs,
  env,
}:
let
  inherit (env) service release;

  releaseExcludes = ../../.. + "/releases/${release}/test-excludes/${service}.txt";

  sandboxExcludes = pkgs.writeText "${service}-${release}-sandbox-excludes.txt" ''
    # Tests that fail only inside the Nix build sandbox, one regex per line,
    # each with the error it fails with there.

    # _is_xmlsec1_installed() runs `which xmlsec1`, and the sandbox has no
    # which: FileNotFoundError: [Errno 2] No such file or directory: 'which'.
    # In CI which exists, xmlsec1 does not, and the test skips.
    keystone\.tests\.unit\.test_v3_federation\.SAMLGenerationTests\.test_saml_signing
  '';
in
pkgs.runCommand "${service}-${builtins.replaceStrings [ "." ] [ "_" ] release}-tests"
  {
    nativeBuildInputs = [
      env.testVenv
      pkgs.nukeReferences
    ];
  }
  ''
    cp -r --no-preserve=mode ${env.src} source
    cd source
    export HOME="$TMPDIR/home"
    mkdir -p "$HOME"
    unset OS_TEST_DBAPI_ADMIN_CONNECTION

    stestr init
    status=0
    cat ${releaseExcludes} ${sandboxExcludes} > excludes.txt
    stestr run --concurrency "$NIX_BUILD_CORES" --exclude-list excludes.txt || status=$?

    mkdir -p "$out"
    stestr last --subunit > "$out/testresults.subunit" || true
    stestr last > last.txt || true
    sed -n '/^Totals$/,/^Sum of execute time/p' last.txt > "$out/summary.txt"
    nuke-refs "$out/testresults.subunit" "$out/summary.txt"
    python3 ${./subunit-crc.py} "$out/testresults.subunit"
    exit "$status"
  ''
