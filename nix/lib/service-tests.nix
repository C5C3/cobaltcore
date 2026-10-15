# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The unit tests of one lock, offline in the build sandbox, against the
# patched source with the venv that carries the test dependency group
# (nix/lib/python-env.nix). Two derivations:
#
#   run    <name>-<rel_>-test-run, which succeeds whenever the suite ran.
#          $out/exit-status holds the exit status of the suite;
#          $out/testresults.subunit and $out/summary.txt (the totals of
#          `stestr last`) or the JUnit XML files of a pytest suite hold its
#          results. A failed suite therefore still has an output the
#          test-service-images job uploads.
#   check  <name>-<rel_>-tests, which prints summary.txt and fails unless the
#          run's exit status is 0, naming the run to delete for a rerun.
#
# With withDatabases, MySQL and PostgreSQL run on 127.0.0.1 inside the
# sandbox and OS_TEST_DBAPI_ADMIN_CONNECTION names them, so oslo.db's
# opportunistic database tests run instead of skipping. A server that does
# not start fails the run with a service-tests: line in the build log before
# the suite runs: the store keeps no output of that failure, and the next
# build starts the servers again.
#
# A suite with a .stestr.conf runs under stestr with the release exclude list
# releases/<release>/test-excludes/<name>.txt and sandboxExcludes below; a
# suite without one (horizon) runs under pytest the way horizon's tox py3 env
# runs it. nuke-refs blanks the store paths the results name, so the output
# references neither the test venv nor the source, and subunit-crc.py then
# recomputes the packet checksums nuke-refs left stale.
#
# stestr starts one worker per core Nix grants the build (NIX_BUILD_CORES),
# not one per host CPU.
{ pkgs }:
{
  env,
  withDatabases ? true,
}:
let
  inherit (pkgs) lib;
  inherit (env) name release attr;

  # Tests that fail only inside the build sandbox, one regex per line under
  # the lock name and the releases they fail in, each with the error they
  # fail with there. A release where the test passes or skips itself keeps
  # it. They never go into releases/<release>/test-excludes/, which stay the
  # excludes of the upstream suite.
  sandboxExcludes = {
    # validate_import_uri resolves foo.com and example.com with
    # socket.getaddrinfo, and the sandbox has no network:
    # "AssertionError: False is not true". glance 33.0.0 mocks the lookup.
    glance = {
      releases = [ "2026.1" ];
      patterns = ''
        glance\.tests\.unit\.common\.test_utils\.ImportURITestCase\.test_(ignored_filtering_options|validate_import_uri)$
      '';
    };
    # The tests write a script with a #!/bin/bash shebang and run it through
    # env, and the sandbox has /bin/sh alone: "env: '/tmp/pm_env_.../tmp...':
    # No such file or directory" (exit code 127).
    neutron = {
      releases = [
        "2026.1"
        "2026.2"
      ];
      patterns = ''
        neutron\.tests\.unit\.agent\.linux\.test_external_process\.TestProcessManagerScript\.test_(enable|disable)_check_process_id_env_var$
      '';
    };
    # oslo_utils.secretutils finds libcrypt through ctypes.util.find_library,
    # which finds nothing without an ldconfig cache: "RuntimeError: libcrypt
    # is not available". nova 34.0.0 skips the test with that message.
    nova = {
      releases = [ "2026.1" ];
      patterns = ''
        nova\.tests\.unit\.virt\.disk\.test_inject\.VirtDiskTest\.test_inject_admin_password$
      '';
    };
  };
  sandboxExclude = sandboxExcludes.${name} or { releases = [ ]; };

  releaseExcludes = ../../releases + "/${release}/test-excludes/${name}.txt";
  excludes = pkgs.writeText "${attr}-excludes.txt" (
    lib.optionalString (builtins.pathExists releaseExcludes) (builtins.readFile releaseExcludes)
    + "\n"
    + lib.optionalString (builtins.elem release sandboxExclude.releases) sandboxExclude.patterns
  );

  run =
    pkgs.runCommand "${attr}-test-run"
      {
        # The test tools of the venv-builder container the suites ran in before:
        # git, which, qemu-img (cinder's TestFormatInspectors build their
        # fixture images with it) and openssl (nova's keypair and crypto tests
        # run it), which that container had through ca-certificates.
        nativeBuildInputs = [
          env.testVenv
          pkgs.nukeReferences
          pkgs.git
          pkgs.which
          pkgs.qemu-utils
          pkgs.openssl
        ]
        ++ lib.optionals withDatabases [
          pkgs.mysql84
          pkgs.postgresql_18
        ];
      }
      (
        ''
          cp -r --no-preserve=mode ${env.src} source
          cd source
          export HOME="$TMPDIR/home"
          mkdir -p "$HOME" "$out"
          # pbr trusts PKG-INFO only when its Name is the project's
          # distribution name, which can differ from the lock name.
          printf 'Metadata-Version: 2.1\nName: %s\nVersion: %s\n' \
            ${lib.escapeShellArg env.project} ${lib.escapeShellArg env.version} > PKG-INFO
          # nova's unit suite runs under eventlet, the way its tox py3 env
          # does. At 33.0.0 nova/cmd/scheduler.py selects the threading
          # backend at import while nova/tests/unit/__init__.py has already
          # selected eventlet, and oslo.service raises BackendAlreadySelected
          # during discovery. The other services never read the variable.
          export OS_NOVA_DISABLE_EVENTLET_PATCHING=False

          status=0
          fail() {
            echo "service-tests: $1" >&2
            exit 1
          }
        ''
        + lib.optionalString withDatabases ''
          mysql_exit=0
          mysqld --initialize-insecure --datadir="$TMPDIR/mysql" > "$TMPDIR/mysql-init.log" 2>&1 \
            || mysql_exit=$?
          if [ "$mysql_exit" -ne 0 ]; then
            cat "$TMPDIR/mysql-init.log" >&2
            fail "mysqld --initialize-insecure failed (exit $mysql_exit)"
          else
            mysqld --datadir="$TMPDIR/mysql" --socket="$TMPDIR/mysql.sock" --tmpdir="$TMPDIR" \
              --bind-address=127.0.0.1 --port=3306 --mysqlx=OFF \
              --pid-file="$TMPDIR/mysql.pid" --log-error="$TMPDIR/mysql.err" &
            mysql_pid=$!
            answered=false
            for _ in $(seq 1 60); do
              if mysqladmin --socket="$TMPDIR/mysql.sock" -u root ping > /dev/null 2>&1; then
                answered=true
                break
              fi
              sleep 1
            done
            if [ "$answered" = true ]; then
              mysql --socket="$TMPDIR/mysql.sock" -u root <<'SQL' || fail "mysql could not set up the root accounts"
          ALTER USER 'root'@'localhost' IDENTIFIED BY 'openstack';
          CREATE USER 'root'@'127.0.0.1' IDENTIFIED BY 'openstack';
          GRANT ALL PRIVILEGES ON *.* TO 'root'@'127.0.0.1' WITH GRANT OPTION;
          SQL
            else
              cat "$TMPDIR/mysql.err" >&2 || true
              fail "mysqld did not answer within 60 s"
            fi
          fi

          echo openstack > "$TMPDIR/pgpass"
          if initdb -D "$TMPDIR/postgres" -U openstack --pwfile="$TMPDIR/pgpass" > "$TMPDIR/initdb.log" 2>&1 \
            && pg_ctl -w -D "$TMPDIR/postgres" -l "$TMPDIR/postgres.log" start \
              -o "-c listen_addresses=127.0.0.1 -c port=5432 -k $TMPDIR"; then
            createdb -h 127.0.0.1 -U openstack openstack || fail "createdb openstack failed"
          else
            cat "$TMPDIR/initdb.log" "$TMPDIR/postgres.log" >&2 || true
            fail "postgres did not start"
          fi

          export OS_TEST_DBAPI_ADMIN_CONNECTION="sqlite://;mysql+pymysql://root:openstack@127.0.0.1/;postgresql+psycopg2://openstack:openstack@127.0.0.1/openstack"
        ''
        + ''
          # horizon's test settings write the django-compressor output to
          # /tmp, whose group the user namespace of the sandbox does not map;
          # Django's FileSystemStorage then fails the chown to that group with
          # "OSError: [Errno 22] Invalid argument: '/tmp/CACHE/css/...'". The
          # run points that test-only setting at the build directory.
          if [ -f horizon/test/settings.py ]; then
            sed -i 's|^COMPRESS_ROOT = "/tmp/"$|COMPRESS_ROOT = "'"$TMPDIR"'/"|' horizon/test/settings.py
          fi

          if [ -f .stestr.conf ]; then
            stestr init
            stestr run --concurrency "$NIX_BUILD_CORES" --exclude-list ${excludes} || status=$?
          elif [ -f tools/unit_tests.sh ]; then
            # horizon up to 26.x drives its own pytest invocations with the
            # settings module of each project and writes JUnit XML to
            # test_reports/.
            bash tools/unit_tests.sh "$(pwd)" || status=$?
            copied=0
            for report in test_reports/*.xml; do
              [ -e "$report" ] || continue
              cp "$report" "$out/"
              copied=1
            done
            if [ "$copied" -eq 0 ]; then
              echo "service-tests: no JUnit XML under test_reports/*.xml for ${name}" >&2
            fi
          elif [ ${lib.escapeShellArg name} = horizon ]; then
            # horizon 27.0.0 dropped tools/unit_tests.sh, and its tox.ini
            # runs these four pytest invocations itself. Each project needs
            # its own Django settings module; a bare pytest run has none,
            # and every test module fails collection with
            # ImproperlyConfigured.
            run_pytest() {
              local project="$1" settings="$2"
              shift 2
              python -m pytest -v --ds="$settings" \
                --junitxml="$out/''${project}_test_results.xml" "$@" || status=1
            }
            run_pytest openstack_auth openstack_auth.tests.settings openstack_auth
            run_pytest horizon horizon.test.settings horizon
            run_pytest openstack_dashboard openstack_dashboard.test.settings \
              -m "not selenium and not integration and not plugin_test" \
              --ignore=openstack_dashboard/test/selenium openstack_dashboard
            run_pytest plugin openstack_dashboard.test.settings openstack_dashboard/test/test_plugins
          else
            python -m pytest --junitxml="$out/testresults.xml" || status=$?
          fi
        ''
        + lib.optionalString withDatabases ''
          kill "$mysql_pid" 2> /dev/null || true
          wait "$mysql_pid" || true
          pg_ctl -w -D "$TMPDIR/postgres" stop -m fast || true
        ''
        + ''
          echo "$status" > "$out/exit-status"
          if [ -d .stestr ]; then
            # Without a recorded run (a failed discovery), stestr prints its
            # error to stdout and exits 1, and the stream is dropped.
            if stestr last --subunit > testresults.subunit; then
              mv testresults.subunit "$out/testresults.subunit"
            fi
            stestr last > last.txt || true
            sed -n '/^Totals$/,/^Sum of execute time/p' last.txt > "$out/summary.txt"
          fi
          find "$out" -type f -exec nuke-refs {} +
          if [ -s "$out/testresults.subunit" ]; then
            ${env.python.interpreter} ${./subunit-crc.py} "$out/testresults.subunit"
          fi
        ''
      );

  check = pkgs.runCommand "${attr}-tests" { } ''
    if [ -f ${run}/summary.txt ]; then
      cat ${run}/summary.txt
    fi
    status=$(cat ${run}/exit-status)
    if [ "$status" != 0 ]; then
      echo "${name} ${release} unit tests failed (exit $status)" >&2
      echo "to run the suite again, remove the result links that nix-store --query --roots ${run} lists, then run: nix store delete ${run}" >&2
      exit 1
    fi
    touch "$out"
  '';
in
{
  inherit run check;
}
