#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check that [barbican] timeout bounds the keystoneauth session
# castellan reads keys through, for the verify scripts under
# tests/container-images/ of the images whose operator renders the key (nova,
# nova-compute, cinder).
#
# It proves the image build applied the castellan row of
# nix/lib/python-env.nix (#1799) to the installed castellan. Without the
# patch castellan builds its session with no timeout, and a key read against a
# Keystone or Barbican that stops answering blocks for as long as TCP does.
# The first case points [barbican] barbican_endpoint and auth_endpoint at a
# local server that accepts and never answers, sets timeout = 1, and expects
# BarbicanKeyManager.get to raise castellan's KeyManagerError within 5 s. The
# second case leaves timeout unset and, with keystoneauth's Session mocked,
# expects the session to be built with timeout=None, which is what an
# unpatched castellan never passes. A 10 s alarm ends a call that blocks, so
# an unbounded session fails the check without hanging the job. Stderr is
# echoed on failure so the reason is named. Kept here so the check is edited
# once instead of once per image.

# assert_castellan_barbican_session_bounded
# Assert that [barbican] timeout bounds castellan's session in $IMAGE. Needs
# $IMAGE and the caller's PASS/FAIL counters.
assert_castellan_barbican_session_bounded() {
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import signal
import socket
import sys
import threading
import time
import uuid
from unittest import mock

from castellan.common import exception
from castellan.common.credentials import keystone_password
from castellan.key_manager import barbican_key_manager
from oslo_config import cfg


def still_blocked(signum, frame):
    sys.exit("a key read was still blocked after 10 s: castellan built its "
             "session without [barbican] timeout")


signal.signal(signal.SIGALRM, still_blocked)
signal.alarm(10)

# A server that accepts connections and never answers. Every accepted
# socket stays referenced: a closed one would answer with EOF at once.
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen(8)
SILENT = "http://127.0.0.1:%d" % listener.getsockname()[1]
accepted = []


def accept():
    while True:
        accepted.append(listener.accept()[0])


threading.Thread(target=accept, daemon=True).start()


def key_manager(timeout=None):
    conf = cfg.ConfigOpts()
    conf([], default_config_files=[])
    manager = barbican_key_manager.BarbicanKeyManager(conf)
    conf.set_override("barbican_endpoint", SILENT, group="barbican")
    conf.set_override("auth_endpoint", SILENT + "/v3", group="barbican")
    if timeout is not None:
        conf.set_override("timeout", timeout, group="barbican")
    return manager


context = keystone_password.KeystonePassword(
    password="secret", auth_url=SILENT + "/v3", username="nova",
    user_domain_name="Default", project_name="service",
    project_domain_name="Default")

start = time.monotonic()
try:
    key_manager(timeout=1).get(context, str(uuid.uuid4()))
except exception.KeyManagerError:
    took = time.monotonic() - start
    if took >= 5:
        sys.exit("with [barbican] timeout = 1, the KeyManagerError took "
                 "%.2f s, expected under 5 s" % took)
except Exception as e:
    sys.exit("with [barbican] timeout = 1, get raised %r, expected "
             "castellan.common.exception.KeyManagerError" % (e,))
else:
    sys.exit("with [barbican] timeout = 1, get returned against a server "
             "that never answers")

with mock.patch("keystoneauth1.session.Session") as session:
    try:
        key_manager()._get_barbican_client(context)
    except exception.KeyManagerError:
        pass
kwargs = session.call_args.kwargs if session.call_args else None
if kwargs is None or "timeout" not in kwargs:
    sys.exit("without [barbican] timeout, the session was built with %r, "
             "expected a timeout keyword" % (kwargs,))
if kwargs["timeout"] is not None:
    sys.exit("without [barbican] timeout, the session timeout is %r, "
             "expected None" % (kwargs["timeout"],))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "[barbican] timeout bounds castellan's session" "0" "$exit_code"
}
