#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Verify cinder container image meets requirements
# Usage: bash tests/container-images/verify_cinder.sh [image_name]
# Default image: c5c3/cinder:27.0.0
# Requires: Docker daemon running

set -euo pipefail

IMAGE="${1:-c5c3/cinder:27.0.0}"

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/lib/assertions.sh
source "$SCRIPT_DIR/../lib/assertions.sh"

# --- Test 1: cinder-manage console script runs ---
test_cinder_manage_version() {
  echo "Test: cinder-manage --version succeeds"
  # Prints the version of the tag the image was built from (27.0.0 or
  # 28.0.0), which pbr writes into the package metadata at install time.
  # Plain --version reads no configuration file and opens no database, so it
  # proves the console script uv generated is runnable.
  local version_output exit_code=0
  version_output=$(docker run --rm "$IMAGE" cinder-manage --version 2>&1) || exit_code=$?

  assert_eq "cinder-manage --version exits 0" "0" "$exit_code"
  assert_not_empty "version output is non-empty" "$version_output"
}

# --- Test 2: cinder-status console script runs ---
test_cinder_status_help() {
  echo "Test: cinder-status --help succeeds"
  # Plain --help needs no config and no database, so it proves a second
  # console script uv generated is runnable.
  local help_output exit_code=0
  help_output=$(docker run --rm "$IMAGE" cinder-status --help 2>&1) || exit_code=$?

  assert_eq "cinder-status --help exits 0" "0" "$exit_code"
  assert_not_empty "help output is non-empty" "$help_output"
}

# --- Test 3: cinder is importable ---
test_cinder_importable() {
  echo "Test: cinder imports cleanly"
  local exit_code=0
  docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c "import cinder" > /dev/null 2>&1 || exit_code=$?

  assert_eq "import cinder exits 0" "0" "$exit_code"
}

# --- Test 4: the driver and backend libraries are importable ---
test_driver_libraries_importable() {
  echo "Test: the driver and backend libraries import cleanly"
  # The libraries the NFS driver, the Barbican key manager, the S3 backup
  # driver, the coordination and task-flow layers and privsep ride on. They
  # arrive as transitive requirements pinned in upper-constraints, so nothing
  # else in this image would notice if a constraint bump dropped one of them.
  # cinder.wsgi.api is deliberately absent from this list: importing it runs
  # wsgi.initialize_application() at module level and dies with
  # oslo_service.wsgi.ConfigNotFound in a bare image. Test 5 inspects that
  # module instead of importing it.
  # Stderr is echoed on failure so the module that went missing is named
  # rather than collapsed into a bare exit code over seven imports.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    "import cinder.wsgi.wsgi, os_brick.remotefs.remotefs, castellan.key_manager.barbican_key_manager, boto3, tooz, taskflow, oslo_privsep" \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "import of the driver and backend libraries exits 0" "0" "$exit_code"
}

# --- Test 5: the uWSGI module path and its application symbol resolve ---
test_wsgi_module_resolvable() {
  echo "Test: cinder.wsgi.api resolves and binds application"
  # The cinder-operator launches uWSGI with --module
  # cinder.wsgi.api:application, so the module path and the application
  # symbol both have to exist in the image. Resolve the module spec instead
  # of importing it: importing cinder.wsgi.api executes
  # wsgi.initialize_application() at module level (spike #985, evidence part
  # 1, section (c)), which reads the configuration the operator passes
  # through --pyargv and the bare image does not carry. find_spec answers
  # half the question this test asks, whether the module path handed to uWSGI
  # resolves, without running module-level code, so do not simplify it into
  # a plain import.
  # ast.parse over the module source answers the other half without
  # executing it either. Both pinned tags open with an `application = None`
  # sentinel and rebind it only inside `with lock:` and `if application is
  # None:`, so the direct children of the module node are not enough: the
  # walk has to reach into nested statement bodies (`with`, `if`, `try`),
  # and a tree whose only binding is that sentinel has to be rejected. It
  # stops at every scope boundary though (FunctionDef, AsyncFunctionDef,
  # ClassDef, Lambda), because uWSGI looks `application` up as a module
  # global and a binding in a nested scope is a different symbol. Were a
  # partial upstream restructure to move the lock block into a helper and
  # leave the sentinel behind, counting that helper's assignment would ship
  # an image where uWSGI binds `application` to None and every request to
  # the cinder API fails.
  # Stderr is captured rather than discarded, so an unresolvable module and
  # a renamed symbol do not collapse into the same bare exit code.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import ast, importlib.util, sys
spec = importlib.util.find_spec("cinder.wsgi.api")
if spec is None or spec.origin is None:
    sys.exit("cinder.wsgi.api does not resolve to a module file")
tree = ast.parse(open(spec.origin).read())
nested = {id(c) for n in ast.walk(tree)
          if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda))
          for c in ast.walk(n) if c is not n}
bindings = [n for n in ast.walk(tree)
            if isinstance(n, ast.Assign) and id(n) not in nested
            for t in n.targets if isinstance(t, ast.Name) and t.id == "application"]
if not bindings:
    sys.exit("cinder.wsgi.api binds no module-level application")
if all(isinstance(b.value, ast.Constant) and b.value.value is None for b in bindings):
    sys.exit("cinder.wsgi.api only binds application = None at module level")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "cinder.wsgi.api resolves and binds application" "0" "$exit_code"
}

# --- Test 6: the shipped package data files are present ---
test_package_data_files_shipped() {
  echo "Test: the package data files ship in the image"
  # The data files the package declares (setup.cfg data_files at 27.0.0,
  # pyproject.toml [tool.setuptools.data-files] at 28.0.0). Nothing in the
  # Dockerfile copies etc/cinder/ by hand, so a tag that drops the data-file
  # declaration fails here instead of at container start. The cinder-operator
  # (#987) points api_paste_config, resource_query_filters_file and
  # rootwrap_config at these absolute paths.
  local file exit_code

  for file in /var/lib/openstack/etc/cinder/api-paste.ini \
    /var/lib/openstack/etc/cinder/resource_filters.json \
    /var/lib/openstack/etc/cinder/rootwrap.conf \
    /var/lib/openstack/etc/cinder/rootwrap.d/volume.filters; do
    exit_code=0
    docker run --rm "$IMAGE" test -f "$file" || exit_code=$?

    assert_eq "$file is present" "0" "$exit_code"
  done
}

# --- Test 7: the companion console scripts run ---
test_companion_scripts_runnable() {
  echo "Test: companion console scripts run"
  # The process inventory of #979. Each of these runs as its own process from
  # its own console script instead of through the uWSGI module path of test
  # 5, so a script uv failed to generate stays invisible to every other check
  # here. --help is what makes the check reach past the wrapper: oslo.config
  # answers it inside argparse and exits 0 before any configuration file is
  # read, so the script has to import its entry-point target first. A wrapper
  # uv still generates after a constraint bump drops a transitive import dies
  # with ModuleNotFoundError here instead of at container start. Stderr is
  # echoed on failure so the missing module is named rather than collapsed
  # into a bare exit code.
  local script name err exit_code

  for script in /var/lib/openstack/bin/cinder-scheduler \
    /var/lib/openstack/bin/cinder-volume \
    /var/lib/openstack/bin/cinder-backup \
    /var/lib/openstack/bin/cinder-api; do
    exit_code=0
    err=$(docker run --rm "$IMAGE" "$script" --help 2>&1 >/dev/null) || exit_code=$?
    [ "$exit_code" -eq 0 ] || echo "    $err"
    name="$(basename "$script")"

    assert_eq "$name --help exits 0" "0" "$exit_code"
  done
}

# --- Test 8: the apt-installed helper binaries are wired ---
test_apt_wiring() {
  echo "Test: the apt-installed helper binaries are wired"
  local output exit_code=0

  # nfs-common. NfsDriver.do_setup probes for mount.nfs through the root
  # helper, so the binary has to exist. mount.nfs without arguments exits
  # non-zero by design, so the check is on the file rather than on a run.
  docker run --rm "$IMAGE" test -x /usr/sbin/mount.nfs || exit_code=$?
  assert_eq "mount.nfs is present and executable" "0" "$exit_code"

  # qemu-utils. cinder/image/image_utils.py (fetch_to_raw, resize_image,
  # convert_image) and the LUKS qcow2 pre-create shell out to qemu-img.
  exit_code=0
  output=$(docker run --rm "$IMAGE" qemu-img --version 2>&1) || exit_code=$?
  assert_eq "qemu-img --version exits 0" "0" "$exit_code"
  assert_contains "qemu-img names its version" "$output" "qemu-img version"

  # sudo. The root helper is `sudo cinder-rootwrap` and the image carries no
  # sudoers entry for the service user (#979 D3), so sudo is present and
  # unusable. A passing `sudo -n true` would mean the image grew a sudoers
  # entry and the restricted posture no longer holds.
  exit_code=0
  docker run --rm "$IMAGE" sudo --version > /dev/null 2>&1 || exit_code=$?
  assert_eq "sudo --version exits 0" "0" "$exit_code"

  exit_code=0
  docker run --rm "$IMAGE" sudo -n true > /dev/null 2>&1 || exit_code=$?
  assert_nonzero_exit "sudo -n true is refused" "$exit_code"
}

# --- Test 9: the AMQP readiness probe answers ---
test_amqp_readiness_probe() {
  echo "Test: cinder-amqp-ready answers about this container's connection"
  # The exec readiness probe of the cinder-scheduler, cinder-volume and
  # cinder-backup processes (#979 D2), which report ready when a process of
  # their container holds a socket established to the broker port, the
  # semantics of kolla's healthcheck_port. The cinder-operator (#987) wires
  # this path as readinessProbe.exec.command, so the path is part of the
  # contract.
  local output exit_code=0 mate waited hold_connection

  # The python source that puts an established connection to the broker port
  # into the container's own network namespace: a listener on the loopback
  # and a client socket connected to it.
  hold_connection='import socket, sys
srv = socket.socket()
srv.bind(("127.0.0.1", 5672))
srv.listen(1)
cli = socket.create_connection(("127.0.0.1", 5672))
conn, _ = srv.accept()
'

  docker run --rm "$IMAGE" test -x /var/lib/openstack/bin/cinder-amqp-ready || exit_code=$?
  assert_eq "cinder-amqp-ready is present and executable" "0" "$exit_code"

  # A bare container holds no connection, so exit 1 with the "no established
  # connection" message is the answer here. The probe prints its message to
  # stdout, so only stdout is captured.
  exit_code=0
  output=$(docker run --rm "$IMAGE" cinder-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "cinder-amqp-ready exits 1 without a broker" "1" "$exit_code"
  assert_contains "cinder-amqp-ready names the default broker port" \
    "$output" "no established connection to broker port 5672"

  exit_code=0
  output=$(docker run --rm -e CINDER_AMQP_PORT=1 "$IMAGE" cinder-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "cinder-amqp-ready exits 1 with CINDER_AMQP_PORT=1" "1" "$exit_code"
  assert_contains "cinder-amqp-ready honours CINDER_AMQP_PORT" \
    "$output" "no established connection to broker port 1"

  # A CINDER_AMQP_PORT carrying a transport URL instead of a port number is a
  # one-line message on stderr, not a traceback that sends the reader looking
  # for a broken probe instead of a broken Secret key. The value stays out of
  # that message: the key this misconfiguration is confused with carries the
  # broker password, and kubelet copies an exec probe's output verbatim into
  # the Unhealthy event, which every principal that can read events in the
  # namespace can read.
  exit_code=0
  output=$(docker run --rm \
    -e CINDER_AMQP_PORT='rabbit://cinder:s3cr3t@openstack-rabbitmq.openstack.svc:5672/' \
    "$IMAGE" cinder-amqp-ready 2>&1 > /dev/null) || exit_code=$?
  assert_nonzero_exit "cinder-amqp-ready refuses a malformed CINDER_AMQP_PORT" "$exit_code"
  assert_contains "cinder-amqp-ready names the malformed CINDER_AMQP_PORT" \
    "$output" "CINDER_AMQP_PORT is not a port number"
  assert_not_contains "cinder-amqp-ready does not echo the malformed value" \
    "$output" "s3cr3t"
  assert_not_contains "cinder-amqp-ready prints no traceback" "$output" "Traceback"

  # A number that is not a port is the same misconfiguration: an unset int32
  # field of the cinder-operator's CR renders as 0, and no connection table
  # row can carry that remote port. Without the range check the pods stay
  # NotReady for the life of the deployment printing the message a broker
  # outage prints, which sends the reader to RabbitMQ instead of the Secret.
  exit_code=0
  output=$(docker run --rm -e CINDER_AMQP_PORT=0 "$IMAGE" \
    cinder-amqp-ready 2>&1 > /dev/null) || exit_code=$?
  assert_nonzero_exit "cinder-amqp-ready refuses an out-of-range CINDER_AMQP_PORT" "$exit_code"
  assert_contains "cinder-amqp-ready names the out-of-range CINDER_AMQP_PORT" \
    "$output" "CINDER_AMQP_PORT is not a port number"

  # The ready branch, which decides whether any cinder pod ever goes Ready:
  # the probe runs as a child of the process holding the connection, so the
  # socket inode it matches is one of that container's own.
  exit_code=0
  output=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/python -c \
    "${hold_connection}import subprocess
sys.exit(subprocess.run([\"/var/lib/openstack/bin/cinder-amqp-ready\"]).returncode)" \
    2>/dev/null) || exit_code=$?
  assert_eq "cinder-amqp-ready exits 0 on an established connection" "0" "$exit_code"
  assert_contains "cinder-amqp-ready names the connected broker port" \
    "$output" "established to broker port 5672"

  # A descriptor that vanishes mid-scan must cost its own entry, not every
  # remaining descriptor of that process, the broker socket among them:
  # cinder-volume closes descriptors constantly and every probe cycle can race
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
    runpy.run_path(\"/var/lib/openstack/bin/cinder-amqp-ready\", run_name=\"__main__\")
except SystemExit as exc:
    sys.exit(exc.code)" 2>/dev/null) || exit_code=$?
  assert_eq "cinder-amqp-ready scans past a vanished descriptor" "0" "$exit_code"
  assert_contains "cinder-amqp-ready still finds the socket behind it" \
    "$output" "established to broker port 5672"

  # A namespace-mate's connection is not this container's. /proc/net/tcp* is
  # scoped to the network namespace, which every container of a pod shares,
  # so a container joined to it reads the row in state 01 while holding none
  # of the sockets behind it. Co-locate cinder-volume with cinder-backup and
  # a probe that stops at the connection state answers for both.
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

  exit_code=0
  output=$(docker run --rm --network "container:$mate" "$IMAGE" \
    cinder-amqp-ready 2>/dev/null) || exit_code=$?
  assert_eq "cinder-amqp-ready exits 1 on a namespace-mate's connection" "1" "$exit_code"
  assert_contains "cinder-amqp-ready does not adopt the mate's connection" \
    "$output" "no established connection to broker port 5672"

  # That isolation is not owed by the pod spec. Under
  # shareProcessNamespace: true the mate's processes are listed in /proc, and
  # the probe still answers "not ready" because it counts only the processes
  # sharing its mount namespace, which a container keeps to itself either
  # way. --pid container:<mate> shares the PID namespace and not the mount
  # namespace, the same split that pod field produces.
  exit_code=0
  output=$(docker run --rm --network "container:$mate" --pid "container:$mate" \
    "$IMAGE" cinder-amqp-ready 2>/dev/null) || exit_code=$?
  docker rm -f "$mate" > /dev/null 2>&1 || true
  assert_eq "cinder-amqp-ready exits 1 under a shared PID namespace" "1" "$exit_code"
  assert_contains "cinder-amqp-ready does not adopt the mate's connection under a shared PID namespace" \
    "$output" "no established connection to broker port 5672"
}

# --- Test 10: the NFS driver patch is applied ---
test_nfs_patch_applied() {
  echo "Test: NfsDriver._qemu_img_info runs as the service user"
  # Proves both build paths applied
  # patches/cinder/<release>/0001-nfs-run-qemu-img-info-as-the-service-user.patch
  # (#979 D3a), which flips the driver's one forced-root call. Under the
  # restricted posture there is no root to fall back to, so an image built
  # from an unpatched tree fails every operation that inspects an image
  # format.
  # The check is on the value _qemu_img_info_base resolves, not on the source
  # text: that method computes `run_as_root or self._execute_as_root`, so the
  # flipped argument only reaches qemu-img once _execute_as_root is False,
  # which is what nas_secure_file_operations = true leaves behind. Both halves
  # are asserted, because the second one is the configuration the
  # cinder-operator (#987) has to write for the patch to take effect at all.
  # cinder.objects.register_all() runs first because
  # cinder/volume/drivers/remotefs.py annotates method signatures with
  # objects.Volume, which the versioned-object registry only populates after
  # registration; without it the import dies with AttributeError before the
  # driver is ever called. Stderr is echoed on failure so the reason is named
  # rather than collapsed into a bare exit code.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import sys
from unittest import mock
import cinder.objects
cinder.objects.register_all()
from cinder.image import image_utils
from cinder.volume.drivers import nfs


def resolved(execute_as_root):
    """Report the run_as_root qemu_img_info is called with."""
    # Constructing the driver needs the operator configuration this image
    # does not carry, so bind the two attributes _qemu_img_info reads.
    driver = nfs.NfsDriver.__new__(nfs.NfsDriver)
    driver.configuration = mock.Mock(nfs_mount_point_base="/var/lib/cinder/mnt")
    driver._execute_as_root = execute_as_root
    with mock.patch.object(image_utils, "qemu_img_info") as info:
        info.return_value.image = None
        info.return_value.backing_file = None
        driver._qemu_img_info("/var/lib/cinder/mnt/hash/volume-1", "volume-1")
    return info.call_args.kwargs["run_as_root"]


if resolved(False):
    sys.exit("NfsDriver._qemu_img_info still forces run_as_root=True")
if not resolved(True):
    sys.exit("NfsDriver._qemu_img_info no longer follows _execute_as_root")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "NfsDriver._qemu_img_info follows nas_secure_file_operations" "0" "$exit_code"
}

# --- Test 11: the create-from-image patch is applied ---
test_create_from_image_patch_applied() {
  echo "Test: the create-from-image qemu-img calls run as the service user"
  # Proves both build paths applied
  # patches/cinder/<release>/0002-create-from-image-run-qemu-img-as-the-service-user.patch,
  # which flips the two forced-root calls above the driver on the
  # create-from-image path: the volume manager inspecting the image it just
  # downloaded (CreateVolumeFromSpecTask._create_from_image_cache_or_download)
  # and the one inside image_utils.fetch_verify_image. Patch 0001 covers the
  # driver alone, and neither call goes through it. Unpatched, the manager
  # call ends every create-from-image in `error`, and the fetch_verify_image
  # call is swallowed by get_qemu_data, which then skips the backing-file and
  # data-file checks for a raw image (#979 D3, the first Tempest run of
  # PR #995).
  # Each is checked on the run_as_root the call resolves, not on the source
  # text. The manager call is driven through the real task method with the
  # fetch, the space check, the signature check and the download stubbed out,
  # so an upstream rewrite of that block is caught rather than passed over.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import contextlib
import sys
from unittest import mock
import cinder.objects
cinder.objects.register_all()
from cinder.image import image_utils
from cinder.volume.flows.manager import create_volume


@contextlib.contextmanager
def fake_fetch(*args, **kwargs):
    yield "/var/lib/cinder/conversion/image_fetch_probe"


def manager_run_as_root():
    """Report the run_as_root the volume manager inspects the image with."""
    # Constructing the task needs a manager, a driver and a database this
    # image does not carry, so bind the attributes the method reads.
    task = create_volume.CreateVolumeFromSpecTask.__new__(
        create_volume.CreateVolumeFromSpecTask)
    task.image_volume_cache = None
    task.db = mock.Mock()
    task.driver = mock.Mock()
    task._create_from_image_download = mock.Mock(return_value=None)
    volume = mock.Mock(size=1, id="volume-1",
                       service_topic_queue="cinder@nfs1")
    with mock.patch.object(image_utils, "qemu_img_info") as info, \
            mock.patch.object(image_utils.TemporaryImages, "fetch",
                              fake_fetch), \
            mock.patch.object(image_utils, "check_available_space"), \
            mock.patch.object(image_utils, "verify_glance_image_signature"), \
            mock.patch.object(image_utils, "check_virtual_size",
                              return_value=1), \
            mock.patch.object(create_volume.fileutils, "ensure_tree"):
        task._create_from_image_cache_or_download(
            mock.Mock(), volume, None, "image-1",
            {"size": 1048576, "disk_format": "raw"}, mock.Mock())
    if info.call_args is None:
        sys.exit("the volume manager inspected no image; the probe no longer "
                 "drives the patched block")
    return info.call_args.kwargs.get("run_as_root", True)


def fetch_verify_run_as_root():
    """Report the run_as_root fetch_verify_image inspects the image with."""
    image_service = mock.Mock()
    image_service.show.return_value = {"disk_format": "raw"}
    with mock.patch.object(image_utils, "fetch"), \
            mock.patch.object(image_utils, "get_qemu_data",
                              return_value=None) as data:
        image_utils.fetch_verify_image(
            mock.Mock(), image_service, "image-1",
            "/var/lib/cinder/conversion/image_fetch_probe")
    if data.call_args is None:
        sys.exit("fetch_verify_image inspected no image; the probe no longer "
                 "drives the patched call")
    return data.call_args.args[4]


if manager_run_as_root():
    sys.exit("the volume manager still inspects the downloaded image as root")
if fetch_verify_run_as_root():
    sys.exit("fetch_verify_image still inspects the downloaded image as root")' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "the create-from-image qemu-img calls run unprivileged" "0" "$exit_code"
}

# --- Test 12: runs as openstack user ---
test_runs_as_openstack_user() {
  echo "Test: container runs as openstack user"
  local whoami_output exit_code=0
  whoami_output=$(docker run --rm "$IMAGE" whoami 2>&1) || exit_code=$?

  assert_eq "whoami exits 0" "0" "$exit_code"
  assert_eq "whoami outputs openstack" "openstack" "$whoami_output"
}

# --- Test 13: no build tools in final image ---
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

# --- Test 14: uwsgi is runnable (serves the cinder API at runtime) ---
test_uwsgi_runnable() {
  echo "Test: uwsgi --version succeeds"
  # Transitively proves the libpython3.12t64 apt wiring: the venv-builder
  # uwsgi binary links libpython3.12.so.1.0, which python-base does not ship.
  local version exit_code=0
  version=$(docker run --rm "$IMAGE" /var/lib/openstack/bin/uwsgi --version 2>&1) || exit_code=$?

  assert_eq "uwsgi --version exits 0" "0" "$exit_code"
  assert_not_empty "uwsgi version output is non-empty" "$version"
}

# --- Test 15: the state directories ship empty and owned by 42424 ---
test_state_directories() {
  echo "Test: the state directories ship empty and owned by 42424"
  # /var/lib/cinder is [DEFAULT] state_path. mnt and backup_mount are the two
  # os-brick mount bases, conversion is image_conversion_dir, tmp is
  # [oslo_concurrency] lock_path and coordination is the tooz file backend
  # directory. All five have to exist, be empty and be owned by 42424, the
  # UID the container runs under. The cinder-operator (#987) mounts an
  # emptyDir over the tree, so this is the layout a plain `docker run` gets.
  local output exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/cinder -mindepth 1 -maxdepth 1 -type d -printf "%f\n" | sort | tr "\n" " "' 2>&1) || exit_code=$?
  assert_eq "the five state directories are present" \
    "backup_mount conversion coordination mnt tmp " "$output"

  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'find /var/lib/cinder -mindepth 2 | wc -l' 2>&1) || exit_code=$?
  assert_eq "the state directories are empty" "0" "$(echo "$output" | tr -d ' ')"

  # The owning UID of the whole tree as a sorted set, so a directory the
  # chown -R missed shows up here instead of only on the one that is checked.
  exit_code=0
  output=$(docker run --rm "$IMAGE" \
    sh -c 'stat -c %u /var/lib/cinder /var/lib/cinder/* | sort -u | tr "\n" " "' 2>&1) || exit_code=$?
  assert_eq "the state tree is owned by 42424 alone" "42424 " "$output"
}

# --- Test 16: pkg_resources is importable ---
test_pkg_resources_importable() {
  echo "Test: pkg_resources and the os_win cinder requires import cleanly"
  # os-win is a requirement of cinder 27.0.0 and os_win/_utils.py imports
  # pkg_resources at module level, which setuptools 81 dropped from the wheel.
  # Left unconstrained, the 2025.2 resolution lands on setuptools 84 and the
  # import fails, taking `hack/gen-option-catalog.sh --check` with it. The
  # stage-1 install holds the runtime venv below setuptools 81; this test is
  # what notices when that constraint goes missing.
  # cinder 28.0.0 dropped the Windows drivers and os-win with them, so the
  # 2026.1 image has no os_win to import. The os_win half follows the
  # requirements the installed cinder declares rather than the tag: it stays
  # mandatory wherever cinder requires os-win, and a cinder that declares no
  # requirements at all fails instead of skipping it.
  # Stderr is echoed on failure so the traceback names the module.
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import importlib.metadata, re, sys
import pkg_resources
declared = {re.split(r"[^A-Za-z0-9._-]", r, maxsplit=1)[0].lower().replace("_", "-")
            for r in importlib.metadata.requires("cinder") or []}
if not declared:
    sys.exit("cinder declares no requirements")
if "os-win" in declared:
    import os_win' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "import of pkg_resources and the required os_win exits 0" "0" "$exit_code"
}

# --- Run all tests ---
echo "=== cinder container verification tests ==="
echo "Image: $IMAGE"
echo ""
test_cinder_manage_version
echo ""
test_cinder_status_help
echo ""
test_cinder_importable
echo ""
test_driver_libraries_importable
echo ""
test_wsgi_module_resolvable
echo ""
test_package_data_files_shipped
echo ""
test_companion_scripts_runnable
echo ""
test_apt_wiring
echo ""
test_amqp_readiness_probe
echo ""
test_nfs_patch_applied
echo ""
test_create_from_image_patch_applied
echo ""
test_runs_as_openstack_user
echo ""
test_no_build_tools_in_final_image
echo ""
test_uwsgi_runnable
echo ""
test_state_directories
echo ""
test_pkg_resources_importable
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
