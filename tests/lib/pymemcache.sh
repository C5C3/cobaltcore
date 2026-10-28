#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared check that oslo.cache's [cache] socket_timeout bounds the
# dogpile.cache pymemcache client of a Nix-built service image, for the verify
# scripts under tests/container-images/ of the services that read [cache].
#
# It proves the image build applied
# nix/lib/patches/dogpile-cache-1.5.0-pymemcache-timeouts.patch (#1560),
# which nix/lib/python-env.nix applies to the installed dogpile.cache.
# Without the patch, a cache call against a Memcached that stops answering
# blocks for as long as TCP does. oslo.cache builds the region from a [cache]
# section against a local server that accepts and never answers: the connect
# and read timeouts come from socket_timeout (1.0 s by default), ignore_exc is
# set, and a read misses within 3 s. A refused port misses at once, and so
# does every call with socket_timeout = 0. With distributed_lock, ignore_exc
# stays off and the read raises TimeoutError within 3 s, where a miss would
# leave dogpile's MemcachedLock looping on add(). dogpile.cache used without
# oslo.cache keeps pymemcache's unbounded defaults and its errors. A 10 s
# alarm ends a call that blocks, so an unbounded client fails the check
# without hanging the job. Stderr is echoed on failure so the reason is
# named. Kept here so the check is edited once instead of once per image.

# assert_pymemcache_backend_bounded
# Assert that [cache] socket_timeout bounds the pymemcache client of $IMAGE
# and that its errors are misses. Needs $IMAGE and the caller's PASS/FAIL
# counters.
assert_pymemcache_backend_bounded() {
  local exit_code=0 err=""
  err=$(docker run --rm "$IMAGE" \
    /var/lib/openstack/bin/python -c \
    'import signal
import socket
import sys
import threading
import time

from dogpile.cache import make_region
from dogpile.cache.api import NO_VALUE
from oslo_cache import core
from oslo_config import cfg


def still_blocked(signum, frame):
    sys.exit("a cache call was still blocked after 10 s: the pymemcache "
             "client has no socket timeout")


signal.signal(signal.SIGALRM, still_blocked)
signal.alarm(10)

# A server that accepts connections and never answers. Every accepted
# socket stays referenced: a closed one would answer with EOF at once.
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen(8)
SILENT = "127.0.0.1:%d" % listener.getsockname()[1]
accepted = []


def accept():
    while True:
        accepted.append(listener.accept()[0])


threading.Thread(target=accept, daemon=True).start()


def oslo_region(server, socket_timeout=None, backend_argument=None):
    conf = cfg.ConfigOpts()
    core.configure(conf)
    conf([], default_config_files=[])
    conf.set_override("enabled", True, group="cache")
    conf.set_override("backend", "dogpile.cache.pymemcache", group="cache")
    conf.set_override("memcache_servers", [server], group="cache")
    if socket_timeout is not None:
        conf.set_override("socket_timeout", socket_timeout, group="cache")
    if backend_argument is not None:
        conf.set_override("backend_argument", backend_argument, group="cache")
    region = core.create_region()
    core.configure_cache_region(conf, region)
    return region


def assert_quick_miss(region, limit, case):
    start = time.monotonic()
    value = region.get("k")
    took = time.monotonic() - start
    if value is not NO_VALUE:
        sys.exit("%s: get returned %r, expected a miss" % (case, value))
    if took >= limit:
        sys.exit("%s: the miss took %.2f s, expected under %s s"
                 % (case, took, limit))


def bounds(client):
    return (client.default_kwargs["connect_timeout"],
            client.default_kwargs["timeout"], client.ignore_exc)


region = oslo_region(SILENT)
found = bounds(region.backend.client)
if found != (1.0, 1.0, True):
    sys.exit("connect_timeout, timeout and ignore_exc are %r, expected "
             "(1.0, 1.0, True) from [cache] socket_timeout" % (found,))
assert_quick_miss(region, 3, "silent server")
assert_quick_miss(oslo_region("127.0.0.1:1"), 1, "refused port")
assert_quick_miss(oslo_region(SILENT, socket_timeout=0), 0.5,
                  "silent server with socket_timeout = 0")

locked = oslo_region(SILENT, backend_argument=["distributed_lock:true"])
found = bounds(locked.backend.client)
if found != (1.0, 1.0, False):
    sys.exit("with distributed_lock, connect_timeout, timeout and "
             "ignore_exc are %r, expected (1.0, 1.0, False)" % (found,))
start = time.monotonic()
try:
    value = locked.get_or_create("k", lambda: "created")
except TimeoutError:
    took = time.monotonic() - start
    if took >= 3:
        sys.exit("with distributed_lock, the TimeoutError took %.2f s, "
                 "expected under 3 s" % took)
else:
    sys.exit("with distributed_lock, get_or_create returned %r, expected "
             "a TimeoutError" % (value,))

plain = make_region().configure("dogpile.cache.pymemcache",
                                arguments={"url": "127.0.0.1:1"})
found = bounds(plain.backend.client)
if found != (None, None, False):
    sys.exit("without oslo.cache, connect_timeout, timeout and ignore_exc "
             "are %r, expected (None, None, False)" % (found,))' \
    2>&1 > /dev/null) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $err"

  assert_eq "[cache] socket_timeout bounds the pymemcache client and errors are misses" "0" "$exit_code"
}
