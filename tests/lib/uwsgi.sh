#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Shared checks of the uWSGI in a Nix-built service image, for the verify
# scripts under tests/container-images/: that it imports the service
# package, and that SIGTERM drains a streamed response before uWSGI exits.
#
# The operators launch the API of every service through the venv's uwsgi with
# --module or --wsgi-file, so the import has to succeed inside uWSGI's
# embedded interpreter, not only under /var/lib/openstack/bin/python. The two
# differ: uWSGI derives the program name of its interpreter from
# readlink /proc/self/exe, and Python finds the venv (its pyvenv.cfg, and
# with it the site-packages that hold the service) beside that path or not at
# all. A bin/uwsgi that links into the uwsgi package's store path therefore
# starts with the base interpreter's sys.prefix, and `uwsgi --version` still
# exits 0 while every application import fails with ModuleNotFoundError.
#
# --pyrun runs a script in that interpreter: uWSGI exits with the status a
# SystemExit in the script carries, so the probe exits 1 with the exception
# and sys.prefix on failure. Kept here so the probe is edited once instead of
# once per image.
#
# The operators run uWSGI with the graceful-shutdown flags of
# UWSGIGracefulShutdownArgs (internal/common/deployment/uwsgi.go). They need
# the binary to export gracefully_kill_them_all, or uWSGI exits at start, and
# a streamed response needs nix/lib/patches/uwsgi-2.0.31-retry-write-poll-on-eintr.patch,
# or the SIGHUP the master sends each worker cuts it. The last log lines need
# nix/lib/patches/uwsgi-2.0.31-flush-logs-under-threaded-logger-lock.patch, or
# the atexit log drain races the --threaded-logger thread and writes heap
# memory in their place.

# assert_uwsgi_imports <module>
# Assert that `uwsgi --pyrun` imports <module>. Needs $IMAGE and the caller's
# PASS/FAIL counters.
assert_uwsgi_imports() {
  local module="$1" output exit_code=0
  local probe
  probe=$(
    cat <<EOF
import sys
try:
    import ${module}
except ImportError as exc:
    sys.exit("uwsgi cannot import ${module}: %s (sys.prefix=%s)" % (exc, sys.prefix))
EOF
  )
  # The probe file goes through /tmp because --pyrun takes a path; the
  # heredoc text travels in an environment variable so no quoting layer of
  # docker run or sh -c touches it.
  output=$(docker run --rm -e UWSGI_PROBE="$probe" "$IMAGE" sh -c \
    'printf "%s\n" "$UWSGI_PROBE" > /tmp/uwsgi-probe.py && uwsgi --pyrun /tmp/uwsgi-probe.py' 2>&1) || exit_code=$?
  [ "$exit_code" -eq 0 ] || echo "    $(grep -E 'uwsgi cannot import|Error' <<<"$output" | tail -3)"
  assert_eq "uwsgi --pyrun imports ${module}" "0" "$exit_code"
}

# assert_uwsgi_graceful_shutdown
# Assert that SIGTERM, sent while uWSGI streams a 128 MiB response to a
# client reading at 64 MiB/s, lets the response finish and makes uWSGI exit 0
# after logging "goodbye to uWSGI." once, with no interrupted write, no reload
# and no byte outside printable ASCII, tab and newline in the log. A
# request sent 0.2 s after SIGTERM, on a new connection and on a kept-alive
# one, has to end before the stream is due to end at 64 MiB/s:
# --shutdown-sockets fails it at once, where without the flag it is held
# until the drain ends. Needs $IMAGE and the caller's PASS/FAIL counters.
assert_uwsgi_graceful_shutdown() {
  local size=$((128 << 20)) app client script output head log
  local client_exit uwsgi_exit late_new late_keepalive nontext goodbye
  local eintr=no reload=no
  app=$(
    cat <<'EOF'
import os

CHUNK = b"x" * (1 << 20)
SIZE = int(os.environ["UWSGI_STREAM_SIZE"])


def application(environ, start_response):
    if environ["PATH_INFO"] == "/small":
        start_response("200 OK", [("Content-Length", "2")])
        yield b"ok"
        return
    start_response("200 OK", [
        ("Content-Type", "application/octet-stream"),
        ("Content-Length", str(SIZE)),
    ])
    for _ in range(SIZE // len(CHUNK)):
        yield CHUNK
EOF
  )
  client=$(
    cat <<'EOF'
import os
import signal
import socket
import sys
import threading
import time

SIZE = int(os.environ["UWSGI_STREAM_SIZE"])
CHUNK = 1 << 20
RATE = 64 << 20  # bytes per second
SMALL = b"GET /small HTTP/1.1\r\nHost: localhost\r\n\r\n"


def wait_ready():
    """Wait up to 30 s for a worker that loaded the app and for the port."""
    # The router binds the port before the workers load the app, so a
    # connect alone also succeeds for a uWSGI that --need-app stops.
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            with open("/tmp/uwsgi.log", errors="replace") as log:
                loaded = "WSGI app 0 (mountpoint='') ready" in log.read()
            if loaded:
                socket.create_connection(("127.0.0.1", 5000), timeout=1).close()
                return 0
        except OSError:
            pass
        time.sleep(0.2)
    return 1


def keep_alive():
    """Return a connection that finished a request and stays open."""
    sock = socket.create_connection(("127.0.0.1", 5000), timeout=10)
    sock.sendall(SMALL)
    reply = b""
    while not reply.endswith(b"\r\n\r\nok"):
        data = sock.recv(1024)
        if not data:
            raise OSError("keep-alive connection closed after %r" % reply)
        reply += data
    return sock


def late_request(sock, result):
    """Send SMALL on sock, or on a new connection when sock is None."""
    # The wait lets the idle worker exit, which with --shutdown-sockets shuts
    # down the socket the router forwards to.
    time.sleep(0.2)
    try:
        if sock is None:
            sock = socket.create_connection(("127.0.0.1", 5000), timeout=10)
        sock.sendall(SMALL)
        result["outcome"] = "%d-byte reply" % len(sock.recv(1024))
    except OSError as exc:
        result["outcome"] = "error %r" % exc
    finally:
        if sock is not None:
            sock.close()
        result["end"] = time.monotonic()


def stream(master):
    """Read the response at RATE; SIGTERM the master 0.5 s into the body.

    Right after the SIGTERM, two threads send the late requests. Each is
    timed against the end the stream is due at RATE, so a cut stream does not
    fail them as well.
    """
    body = 0
    first = 0.0
    sigterm = 0.0
    due = 0.0
    head = b""
    in_body = False
    signalled = False
    late = {}
    sock = socket.create_connection(("127.0.0.1", 5000), timeout=30)
    try:
        idle = keep_alive()
        sock.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\n"
                     b"Connection: close\r\n\r\n")
        while True:
            data = sock.recv(CHUNK)
            if not data:
                break
            if not in_body:
                head += data
                if b"\r\n\r\n" not in head:
                    continue
                in_body = True
                data = head.partition(b"\r\n\r\n")[2]
                if not data:
                    continue
            if not body:
                first = time.monotonic()
            body += len(data)
            if not signalled and time.monotonic() - first >= 0.5:
                os.kill(master, signal.SIGTERM)
                sigterm = time.monotonic()
                due = first + SIZE / RATE
                signalled = True
                for name, late_sock in (("new", None), ("keepalive", idle)):
                    result = {}
                    thread = threading.Thread(target=late_request,
                                              args=(late_sock, result))
                    thread.start()
                    late[name] = (thread, result)
            ahead = first + body / RATE - time.monotonic()
            if ahead > 0:
                time.sleep(ahead)
    except OSError as exc:
        print("client error: %s" % exc)
    finally:
        sock.close()
        # A short body still shuts uWSGI down, so the script does not wait
        # for its exit watchdog.
        if not signalled:
            os.kill(master, signal.SIGTERM)
    print("body=%d" % body)
    for name, (thread, result) in late.items():
        # The socket timeouts bound the thread.
        thread.join()
        print("late %s request: %s at +%.2f s, stream due at +%.2f s" % (
            name, result.get("outcome", "no outcome"),
            result["end"] - sigterm, due - sigterm))
        print("late_%s=%s" % (name, "yes" if result["end"] < due else "no"))
    return 0 if body == SIZE else 1


if sys.argv[1] == "wait":
    sys.exit(wait_ready())
sys.exit(stream(int(sys.argv[1])))
EOF
  )
  script=$(
    cat <<'EOF'
printf '%s\n' "$UWSGI_STREAM_APP" > /tmp/stream.py
printf '%s\n' "$UWSGI_STREAM_CLIENT" > /tmp/client.py
# --hook-master-start, --shutdown-sockets and --threaded-logger mirror
# UWSGIGracefulShutdownArgs in internal/common/deployment/uwsgi.go.
uwsgi --http :5000 --http-keepalive --log-master --wsgi-file /tmp/stream.py \
  --master --lazy-apps --need-app \
  --hook-master-start "unix_signal:15 gracefully_kill_them_all" \
  --shutdown-sockets --threaded-logger --processes 2 --threads 1 \
  > /tmp/uwsgi.log 2>&1 &
pid=$!
if ! /var/lib/openstack/bin/python /tmp/client.py wait; then
  echo "uwsgi did not start"
  tail -n 20 /tmp/uwsgi.log
  exit 1
fi
client_exit=0
/var/lib/openstack/bin/python /tmp/client.py "$pid" || client_exit=$?
# A uWSGI that reloads on SIGTERM instead of exiting runs on; the watchdog
# ends the wait after 30 s.
( sleep 30; echo "uwsgi did not exit"; kill -KILL "$pid" ) &
watchdog=$!
uwsgi_exit=0
wait "$pid" || uwsgi_exit=$?
kill "$watchdog" 2>/dev/null
echo "client_exit=$client_exit"
echo "uwsgi_exit=$uwsgi_exit"
# The bytes outside printable ASCII, tab and newline. The racing atexit log
# drain writes heap memory, NUL bytes included.
echo "nontext=$(tr -d '\t\n\040-\176' < /tmp/uwsgi.log | wc -c)"
echo "--- uwsgi log ---"
# The shell drops NUL bytes from a command substitution with a warning.
tr -d '\000' < /tmp/uwsgi.log
EOF
  )
  # The script reports the client's and uWSGI's exit statuses itself. A
  # docker run that fails before them leaves both empty, and the assertions
  # below fail on that.
  output=$(docker run --rm -e UWSGI_STREAM_APP="$app" -e UWSGI_STREAM_CLIENT="$client" \
    -e UWSGI_STREAM_SIZE="$size" "$IMAGE" sh -c "$script" 2>&1) || true
  client_exit=$(sed -n 's/^client_exit=//p' <<<"$output")
  uwsgi_exit=$(sed -n 's/^uwsgi_exit=//p' <<<"$output")
  late_new=$(sed -n 's/^late_new=//p' <<<"$output")
  late_keepalive=$(sed -n 's/^late_keepalive=//p' <<<"$output")
  nontext=$(sed -n 's/^nontext=//p' <<<"$output")
  head=$(sed '/^--- uwsgi log ---$/,$d' <<<"$output")
  log=$(sed -n '/^--- uwsgi log ---$/,$p' <<<"$output")
  goodbye=$(grep -cF 'goodbye to uWSGI.' <<<"$log" || true)
  if grep -qF 'Interrupted system call' <<<"$log"; then eintr=yes; fi
  if grep -qF 'binary reloading uWSGI' <<<"$log"; then reload=yes; fi
  if [ "$client_exit" != 0 ] || [ "$uwsgi_exit" != 0 ] || [ "$goodbye" != 1 ] ||
    [ "$nontext" != 0 ] ||
    [ "$eintr" != no ] || [ "$reload" != no ] || [ "$late_new" != yes ] ||
    [ "$late_keepalive" != yes ]; then
    sed 's/^/    /' <<<"$head"
    tail -n 20 <<<"$log" | sed 's/^/    /'
  fi
  assert_eq "a $((size >> 20)) MiB response streamed across SIGTERM arrives whole" "0" "$client_exit"
  assert_eq "uwsgi exits 0 after SIGTERM" "0" "$uwsgi_exit"
  assert_eq "uwsgi logs 'goodbye to uWSGI.' once after SIGTERM" "1" "$goodbye"
  assert_eq "the uwsgi log holds no byte outside printable ASCII, tab and newline" "0" "$nontext"
  assert_eq "no write wait fails with 'Interrupted system call'" "no" "$eintr"
  assert_eq "SIGTERM does not reload uwsgi" "no" "$reload"
  assert_eq "a request on a new connection after SIGTERM ends before the stream is due to" "yes" "$late_new"
  assert_eq "a request on a kept-alive connection after SIGTERM ends before the stream is due to" "yes" "$late_keepalive"
}
