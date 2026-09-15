#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify nova container image meets requirements
# Usage: bash tests/container-images/verify_nova.sh [image_name]
# Default image: c5c3/nova:32.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/nova:32.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: nova-manage console script runs ---
test_nova_manage_version() {
  echo "Test: nova-manage --version succeeds"
  # Prints the version of the tag the image was built from (32.0.0 or
  # 33.0.0), which pbr writes into the package metadata at install time.
  # Plain --version reads no configuration file and opens no database, so it
  # proves the console script uv generated is runnable.
  local version_output exit_code=0
  version_output=$(docker run --rm "$IMAGE" nova-manage --version 2>&1) || exit_code=$?

  assert_eq "nova-manage --version exits 0" "0" "$exit_code"
  assert_not_empty "version output is non-empty" "$version_output"
}

# --- Test 2: nova-status console script runs ---
test_nova_status_help() {
  echo "Test: nova-status --help succeeds"
  # Plain --help needs no config and no database, so it proves a second
  # console script uv generated is runnable.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" nova-status --help 2>&1) || exit_code=$?

  assert_eq "nova-status --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 3: nova is importable ---
test_nova_importable() {
  echo "Test: nova imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import nova" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import nova exits 0" "0" "$exit_code"
}

# --- Test 4: the driver and proxy libraries are importable ---
test_driver_libraries_importable() {
  echo "Test: the driver and proxy libraries import cleanly"
  # The fake driver the CI fixture of #1018 runs, the port-plugging and volume
  # libraries os-vif and os-brick, the websocket proxy websockify behind
  # nova-novncproxy, privsep and the unified-limits client. They arrive as
  # transitive requirements pinned in upper-constraints, so nothing else in
  # this image would notice if a constraint bump dropped one of them.
  # Stderr is echoed on failure so the module that went missing is named
  # rather than collapsed into a bare exit code over six imports.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    "import nova.virt.fake, os_vif, os_brick, websockify, oslo_privsep, oslo_limit" \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "import of the driver and proxy libraries exits 0" "0" "$exit_code"

  # The libvirt binding is absent by design (#1014 D14): the control-plane
  # roles run no hypervisor, and the fake driver imports without it. An image
  # that grew libvirt-python through a requirement change carries a compiled
  # binding and the libvirt0 library nothing in this image needs.
  exit_code=0
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import libvirt" 2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -ne 0 ] || echo "    $err"

  assert_nonzero_exit "import libvirt fails" "$exit_code"
  assert_contains "the libvirt binding is absent" "$err" "ModuleNotFoundError"
}

# --- Test 5: the uWSGI module paths and their application symbols resolve ---
test_wsgi_modules_resolvable() {
  echo "Test: nova.wsgi.osapi_compute and nova.wsgi.metadata bind application"
  # The nova-operator launches uWSGI with --module
  # nova.wsgi.osapi_compute:application for the compute API and --module
  # nova.wsgi.metadata:application for the metadata API, so both module paths
  # and both application symbols have to exist in the image. Resolve the
  # module spec instead of importing it: both modules build the application at
  # import time through wsgi_app.init_application, which reads the
  # configuration the operator passes through OS_NOVA_CONFIG_DIR and
  # OS_NOVA_CONFIG_FILES and raises ConfigFilesNotFoundError in a bare image.
  # At 33.0.0 both also call monkey_patch.patch(backend="threading") at import
  # (#1015 section (a.1)). find_spec answers half the question this test asks,
  # whether the module path handed to uWSGI resolves, without running
  # module-level code, so do not simplify it into a plain import.
  # find_spec imports the parent packages of a dotted name and raises
  # ModuleNotFoundError when one is missing, so that case is folded into the
  # same `does not resolve` exit.
  # ast.parse over the module source answers the other half without executing
  # it either. Both pinned tags open with an `application = None` sentinel and
  # rebind it only inside `with lock:` and `if application is None:`, so the
  # direct children of the module node are not enough: the walk has to reach
  # into nested statement bodies (`with`, `if`, `try`), and a tree whose only
  # binding is that sentinel has to be rejected. It stops at every scope
  # boundary though (FunctionDef, AsyncFunctionDef, ClassDef, Lambda), because
  # uWSGI looks `application` up as a module global and a binding in a nested
  # scope is a different symbol. Were a partial upstream restructure to move
  # the lock block into a helper and leave the sentinel behind, counting that
  # helper's assignment would ship an image where uWSGI binds `application` to
  # None and every request to that API fails.
  # Stderr is captured rather than discarded, so an unresolvable module and a
  # renamed symbol do not collapse into the same bare exit code.
  local module exit_code err

  for module in nova.wsgi.osapi_compute nova.wsgi.metadata; do
    exit_code=0
    err=$(docker run --rm "$IMAGE" \
      /var/lib/openstack/bin/python -c \
      'import ast, importlib.util, sys
module = sys.argv[1]
try:
    spec = importlib.util.find_spec(module)
except ModuleNotFoundError:
    spec = None
if spec is None or spec.origin is None:
    sys.exit(f"{module} does not resolve to a module file")
tree = ast.parse(open(spec.origin).read())
nested = {id(c) for n in ast.walk(tree)
          if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda))
          for c in ast.walk(n) if c is not n}
bindings = [n for n in ast.walk(tree)
            if isinstance(n, ast.Assign) and id(n) not in nested
            for t in n.targets if isinstance(t, ast.Name) and t.id == "application"]
if not bindings:
    sys.exit(f"{module} binds no module-level application")
if all(isinstance(b.value, ast.Constant) and b.value.value is None for b in bindings):
    sys.exit(f"{module} only binds application = None at module level")' \
      "$module" 2>&1 > /dev/null) || exit_code=$?
    [ "$exit_code" -eq 0 ] || echo "    $err"

    assert_eq "$module resolves and binds application" "0" "$exit_code"
  done
}

# --- Test 6: the shipped package data files are present ---
test_package_data_files_shipped() {
  echo "Test: the package data files ship in the image"
  # The data files the package declares (data_files in setup.cfg at 32.0.0,
  # [tool.setuptools.data-files] in pyproject.toml at 33.0.0). Nothing in the
  # Dockerfile copies etc/nova/ by hand, so a tag that drops the data-file
  # declaration fails here instead of at container start. The nova-operator
  # (#1017) points api_paste_config and rootwrap_config at these absolute
  # paths.
  # One container checks the three files and prints the ones that are absent.
  local missing

  missing=$(docker run --rm "$IMAGE" sh -c \
    'for file; do test -f "$file" || echo "$file"; done' sh \
    /var/lib/openstack/etc/nova/api-paste.ini \
    /var/lib/openstack/etc/nova/rootwrap.conf \
    /var/lib/openstack/etc/nova/rootwrap.d/compute.filters 2>&1) ||
    missing="docker run failed: $missing"

  assert_eq "the three package data files are present" "" "$missing"
}

# --- Test 7: the console scripts ship and run ---
test_console_scripts() {
  echo "Test: the console scripts ship and run"
  # The eleven entry points uv generates from [console_scripts] of setup.cfg
  # at 32.0.0 and [project.scripts] of pyproject.toml at 33.0.0. A tag or a
  # constraint bump that renames one of them fails here instead of at
  # container start. One container checks all eleven and prints the names
  # that are absent or not executable.
  local name err exit_code missing

  missing=$(docker run --rm "$IMAGE" sh -c \
    'for name; do test -x "/var/lib/openstack/bin/$name" || echo "$name"; done' sh \
    nova-compute nova-conductor nova-manage nova-novncproxy \
    nova-policy nova-rootwrap nova-rootwrap-daemon nova-scheduler \
    nova-serialproxy nova-spicehtml5proxy nova-status 2>&1) ||
    missing="docker run failed: $missing"

  assert_eq "the eleven console scripts are present and executable" "" "$missing"

  # The four processes the nova-operator (#1017) and the #1018 fixture start
  # from a console script rather than through the uWSGI module paths of test
  # 5. --help is what makes the check reach past the wrapper: oslo.config
  # answers it inside argparse and exits 0 before any configuration file is
  # read, so the script has to import its entry-point target first. A wrapper
  # uv still generates after a constraint bump drops a transitive import dies
  # with ModuleNotFoundError here instead of at container start. Stderr is
  # echoed on failure so the missing module is named rather than collapsed
  # into a bare exit code.
  for name in nova-scheduler nova-conductor nova-novncproxy nova-compute; do
    exit_code=0
    err=$(docker run --rm "$IMAGE" "/var/lib/openstack/bin/$name" --help 2>&1 >/dev/null) || exit_code=$?
    [ "$exit_code" -eq 0 ] || echo "    $err"

    assert_eq "$name --help exits 0" "0" "$exit_code"
  done
}

# --- Test 8: the noVNC console assets ship at the pinned version ---
test_novnc_assets() {
  echo "Test: the noVNC assets ship readable at the pinned version"
  # What nova-novncproxy serves from /usr/share/novnc. vnc_lite.html is the
  # page `[vnc] novncproxy_base_url` names (#1014 D8) and imports core/rfb.js;
  # vnc.html loads app/ui.js, which reads defaults.json and mandatory.json.
  local exit_code missing pinned="" shipped=""

  # The proxy reads these files as the openstack user, which is who a plain
  # `docker run` is, and the COPY --link carries the upstream modes over. One
  # container checks the six files and prints the ones that are absent or
  # unreadable.
  missing=$(docker run --rm "$IMAGE" sh -c \
    'for file; do test -f "$file" && test -r "$file" || echo "$file"; done' sh \
    /usr/share/novnc/vnc_lite.html \
    /usr/share/novnc/vnc.html \
    /usr/share/novnc/core/rfb.js \
    /usr/share/novnc/app/ui.js \
    /usr/share/novnc/defaults.json \
    /usr/share/novnc/mandatory.json 2>&1) ||
    missing="docker run failed: $missing"

  assert_eq "the noVNC files are present and readable by the service user" "" "$missing"

  # The version app/ui.js shows, against the ARG NOVNC_VERSION pin the
  # Renovate customManager bumps. The build guard compares the same two values
  # while the source tree is still around; a content pin that slipped past it,
  # or an asset tree copied from elsewhere, fails here.
  pinned=$(grep '^ARG NOVNC_VERSION=' "$SCRIPT_DIR/../../images/nova/Dockerfile" || true)
  pinned="${pinned#*=}"
  pinned="${pinned#v}"

  exit_code=0
  shipped=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    'import json; print(json.load(open("/usr/share/novnc/package.json"))["version"])' 2>&1) || exit_code=$?
  assert_eq "the noVNC package.json is readable" "0" "$exit_code"
  assert_eq "noVNC package.json version equals the ARG NOVNC_VERSION pin" \
    "$pinned" "$shipped"
}

# --- Test 9: the AMQP readiness probe answers ---
test_amqp_readiness_probe() {
  echo "Test: nova-amqp-ready answers about this container's connection"
  # The exec readiness probe of the nova-scheduler and nova-conductor
  # processes (#1014 D1), which report ready when a process of their container
  # holds a socket established to the broker port, the semantics of kolla's
  # healthcheck_port. The nova-operator (#1017) wires this path as
  # readinessProbe.exec.command, so the path is part of the contract.
  local output exit_code=0 mate waited hold_connection run_probe

  # The python source that puts an established connection to the broker port
  # into the container's own network namespace: a listener on the loopback
  # and a client socket connected to it, on HOLD_PORT or 5672. The listener
  # never accepts, so the process owns the client end alone: the server end
  # waits in the backlog without a descriptor and its row carries inode 0.
  # Both rows are in state 01, and only the client's carries the broker port
  # as its remote port, so a probe reading the local address of a row finds
  # no connection of its own here.
  hold_connection='import os, socket, sys
port = int(os.environ.get("HOLD_PORT", "5672"))
srv = socket.socket()
srv.bind(("127.0.0.1", port))
srv.listen(1)
cli = socket.create_connection(("127.0.0.1", port))
'
  # Runs the probe as a child of the process holding the connection and exits
  # with its status.
  run_probe='import subprocess
sys.exit(subprocess.run(["/var/lib/openstack/bin/nova-amqp-ready"]).returncode)'

  docker run --rm "$IMAGE" test -x /var/lib/openstack/bin/nova-amqp-ready || exit_code=$?
  assert_eq "nova-amqp-ready is present and executable" "0" "$exit_code"

  # A bare container holds no connection, so exit 1 with the "no established
  # connection" message is the answer here. The probe prints its message to
  # stdout, so only stdout is captured.
  exit_code=0
  output=$(docker run --rm "$IMAGE" nova-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 1 without a broker" "1" "$exit_code"
  assert_contains "nova-amqp-ready names the default broker port" \
    "$output" "no established connection to broker port 5672"

  exit_code=0
  output=$(docker run --rm -e NOVA_AMQP_PORT=1 "$IMAGE" nova-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 1 with NOVA_AMQP_PORT=1" "1" "$exit_code"
  assert_contains "nova-amqp-ready honours NOVA_AMQP_PORT" \
    "$output" "no established connection to broker port 1"

  # A NOVA_AMQP_PORT carrying a transport URL instead of a port number is a
  # one-line message on stderr, not a traceback that sends the reader looking
  # for a broken probe instead of a broken Secret key. The value stays out of
  # that message: the key this misconfiguration is confused with carries the
  # broker password, and kubelet copies an exec probe's output verbatim into
  # the Unhealthy event, which every principal that can read events in the
  # namespace can read.
  exit_code=0
  output=$(docker run --rm \
    -e NOVA_AMQP_PORT='rabbit://nova:s3cr3t@openstack-rabbitmq.openstack.svc:5672/' \
    "$IMAGE" nova-amqp-ready 2>&1 > /dev/null) || exit_code=$?
  assert_nonzero_exit "nova-amqp-ready refuses a malformed NOVA_AMQP_PORT" "$exit_code"
  assert_contains "nova-amqp-ready names the malformed NOVA_AMQP_PORT" \
    "$output" "NOVA_AMQP_PORT is not a port number"
  assert_not_contains "nova-amqp-ready does not echo the malformed value" \
    "$output" "s3cr3t"
  assert_not_contains "nova-amqp-ready prints no traceback" "$output" "Traceback"

  # A number that is not a port is the same misconfiguration: an unset int32
  # field of the nova-operator's CR renders as 0, and no connection table row
  # can carry that remote port. Without the range check the pods stay NotReady
  # for the life of the deployment printing the message a broker outage
  # prints, which sends the reader to RabbitMQ instead of the Secret.
  exit_code=0
  output=$(docker run --rm -e NOVA_AMQP_PORT=0 "$IMAGE" \
    nova-amqp-ready 2>&1 > /dev/null) || exit_code=$?
  assert_nonzero_exit "nova-amqp-ready refuses an out-of-range NOVA_AMQP_PORT" "$exit_code"
  assert_contains "nova-amqp-ready names the out-of-range NOVA_AMQP_PORT" \
    "$output" "NOVA_AMQP_PORT is not a port number"

  # The ready branch, which decides whether any nova pod ever goes Ready:
  # the probe runs as a child of the process holding the connection, so the
  # socket inode it matches is one of that container's own.
  exit_code=0
  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    "${hold_connection}${run_probe}" 2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 0 on an established connection" "0" "$exit_code"
  assert_contains "nova-amqp-ready names the connected broker port" \
    "$output" "established to broker port 5672"

  # The port compared is NOVA_AMQP_PORT, not a 5672 the probe carries itself.
  # A connection to 5671, the port of a TLS broker, counts with
  # NOVA_AMQP_PORT=5671, and a connection to 5672 does not.
  exit_code=0
  output=$(docker run --rm -e HOLD_PORT=5671 -e NOVA_AMQP_PORT=5671 "$IMAGE" \
    /var/lib/openstack/bin/python -c "${hold_connection}${run_probe}" \
    2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 0 on a connection to NOVA_AMQP_PORT=5671" "0" "$exit_code"
  assert_contains "nova-amqp-ready names the configured broker port" \
    "$output" "established to broker port 5671"

  exit_code=0
  output=$(docker run --rm -e NOVA_AMQP_PORT=5671 "$IMAGE" \
    /var/lib/openstack/bin/python -c "${hold_connection}${run_probe}" \
    2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 1 on a connection to 5672 with NOVA_AMQP_PORT=5671" \
    "1" "$exit_code"
  assert_contains "nova-amqp-ready reports no connection to port 5671" \
    "$output" "no established connection to broker port 5671"

  # A descriptor that vanishes mid-scan must cost its own entry, not every
  # remaining descriptor of that process, the broker socket among them:
  # nova-conductor closes descriptors constantly and every probe cycle can race
  # one. os.listdir on /proc/<pid>/fd lists the descriptor of the listing
  # itself, which is closed again by the time readlink reaches it, so freeing a
  # lower descriptor first puts that stale entry ahead of the socket. Running
  # the probe inside the holder is what puts it in the process that holds the
  # connection.
  exit_code=0
  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    "import runpy
dummy = open(\"/etc/hostname\")
${hold_connection}dummy.close()
try:
    runpy.run_path(\"/var/lib/openstack/bin/nova-amqp-ready\", run_name=\"__main__\")
except SystemExit as exc:
    sys.exit(exc.code)" 2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready scans past a vanished descriptor" "0" "$exit_code"
  assert_contains "nova-amqp-ready still finds the socket behind it" \
    "$output" "established to broker port 5672"

  # A namespace-mate's connection is not this container's. /proc/net/tcp* is
  # scoped to the network namespace, which every container of a pod shares,
  # so a container joined to it reads the row in state 01 while holding none
  # of the sockets behind it. A pod-mate holding its own broker connection
  # would otherwise answer for this container.
  mate=$(docker run --rm -d "$IMAGE" /var/lib/openstack/bin/python -c \
    "${hold_connection}import time
print(\"holding\", flush=True)
time.sleep(120)")
  waited=0
  while ! docker logs "$mate" 2>/dev/null | grep -q holding; do
    sleep 0.1
    waited=$((waited + 1))
    if [ "$waited" -gt 100 ]; then
      echo "    the connection holder did not report within 10s"
      break
    fi
  done

  # The control for the two checks below: the joined namespace shows the
  # mate's row in state 01 to the broker port (1628 is 5672 in hex). Without
  # it both would also pass against a mate that never got to hold its
  # connection, and could not tell a probe that filtered the row out from one
  # that never saw it.
  exit_code=0
  docker run --rm --network "container:$mate" "$IMAGE" /var/lib/openstack/bin/python -c \
    'import sys
rows = [r.split() for r in open("/proc/net/tcp").read().splitlines()[1:]]
sys.exit(0 if any(f[3] == "01" and f[2].endswith(":1628") for f in rows) else 1)' \
    || exit_code=$?
  assert_eq "the joined namespace shows the mate's established broker row" "0" "$exit_code"

  exit_code=0
  output=$(docker run --rm --network "container:$mate" "$IMAGE" \
    nova-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "nova-amqp-ready exits 1 on a namespace-mate's connection" "1" "$exit_code"
  assert_contains "nova-amqp-ready does not adopt the mate's connection" \
    "$output" "no established connection to broker port 5672"

  # That isolation is not owed by the pod spec. Under
  # shareProcessNamespace: true the mate's processes are listed in /proc, and
  # the probe still answers "not ready" because it counts only the processes
  # sharing its mount namespace, which a container keeps to itself either
  # way. --pid container:<mate> shares the PID namespace and not the mount
  # namespace, the same split that pod field produces.
  exit_code=0
  output=$(docker run --rm --network "container:$mate" --pid "container:$mate" \
    "$IMAGE" nova-amqp-ready 2>/dev/null) || exit_code=$?
  docker rm -f "$mate" > /dev/null 2>&1 || true
  assert_eq "nova-amqp-ready exits 1 under a shared PID namespace" "1" "$exit_code"
  assert_contains "nova-amqp-ready does not adopt the mate's connection under a shared PID namespace" \
    "$output" "no established connection to broker port 5672"
}

# --- Test 10: the apt wiring of the runtime stage ---
test_apt_wiring() {
  echo "Test: the apt-installed packages are wired"
  local version exit_code=0

  # libpython3.12t64. The venv-builder-compiled uwsgi binary links
  # libpython3.12.so.1.0, which python-base does not ship, so a uwsgi that
  # prints its version proves the one apt entry of extra-packages.yaml
  # arrived.
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?
  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"

  # No qemu-utils. The control-plane roles convert no images (#1014 D14), and
  # an image that grew the conversion tooling grew a compute-side dependency
  # the nova-operator never uses.
  exit_code=0
  docker run --rm "$IMAGE" which qemu-img > /dev/null 2>&1 || exit_code=$?
  assert_nonzero_exit "qemu-img not found" "$exit_code"

  # sudo. python-base ships it and the image carries no sudoers entry for the
  # service user, so sudo is present and unusable. nova-rootwrap is a
  # compute-side tool. A passing `sudo -n true` would mean the image grew a
  # sudoers entry and the restricted posture no longer holds.
  exit_code=0
  docker run --rm "$IMAGE" sudo --version > /dev/null 2>&1 || exit_code=$?
  assert_eq "sudo --version exits 0" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" sudo -n true > /dev/null 2>&1 || exit_code=$?
  assert_nonzero_exit "sudo -n true is refused" "$exit_code"
}

# --- Test 11: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 12: no build tools in final image ---
test_no_build_tools_in_final_image() {
  echo "Test: no build tools in final image"

  # gcc should not be present
  local gcc_exit=0
  docker run --rm "$IMAGE" which gcc > /dev/null 2>&1 || gcc_exit=$?
  assert_nonzero_exit "gcc not found" "$gcc_exit"

  # python3-dev should not be installed
  local pydev_exit=0
  docker run --rm "$IMAGE" dpkg -s python3-dev > /dev/null 2>&1 || pydev_exit=$?
  assert_nonzero_exit "python3-dev not installed" "$pydev_exit"

  # uv should not be present in the final image
  local uv_exit=0
  docker run --rm "$IMAGE" which uv > /dev/null 2>&1 || uv_exit=$?
  assert_nonzero_exit "uv not found" "$uv_exit"
}

# --- Test 13: the state directories ship empty and owned by 42424 ---
test_state_directories() {
  echo "Test: the state directories ship empty and owned by 42424"
  # /var/lib/nova is [DEFAULT] state_path, where a compute stores its node
  # identity. instances is instances_path, the default $state_path/instances,
  # and tmp is [oslo_concurrency] lock_path. Both have to exist, be empty and
  # be owned by 42424, the UID the container runs under. The nova-operator
  # (#1017) mounts an emptyDir over the tree, so this is the layout a plain
  # `docker run` gets.
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/nova -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort | tr "\n" " "' 2>&1) || exit_code=$?
  assert_eq "the two state directories are present" "instances tmp " "$output"

  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/nova -mindepth 2 | wc -l' 2>&1) || exit_code=$?
  assert_eq "the state directories are empty" "0" "$(echo "$output" | tr -d ' ')"

  # The owning UID of the whole tree as a sorted set, so a directory the
  # chown -R missed shows up here instead of only on the one that is checked.
  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'stat -c %u /var/lib/nova /var/lib/nova/* | sort -u | tr "\n" " "' 2>&1) || exit_code=$?
  assert_eq "the state tree is owned by 42424 alone" "42424 " "$output"
}

# --- Run all tests ---
echo "=== nova container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_nova_manage_version
echo ""
test_nova_status_help
echo ""
test_nova_importable
echo ""
test_driver_libraries_importable
echo ""
test_wsgi_modules_resolvable
echo ""
test_package_data_files_shipped
echo ""
test_console_scripts
echo ""
test_novnc_assets
echo ""
test_amqp_readiness_probe
echo ""
test_apt_wiring
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_state_directories
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
