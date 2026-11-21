#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/c5c3/full-controlplane-keystone/horizon-login.sh: the dashboard
# login of Link 2c, which Link 8e runs again after the release hop, and Link
# 8f, which moves the dashboard to cache sessions and logs in a third time.
#
# The step 1 script of chainsaw-test.yaml sources this file among its
# helpers:
#
#   . ./horizon-login.sh
#
# Chainsaw hands a script's content to bash as one argument, and Linux caps
# one argument at 128 KiB (MAX_ARG_STRLEN). With this check inline the step 1
# script passes that cap and bash never starts. Sourced, the function runs in
# the step's own shell: it reads KEYSTONE_CR, HORIZON_CR, CP_NAME, CP_NS and
# LOGIN_CLIENT_IMAGE from it, runs under its set -euo pipefail, and a failed
# check ends the step.

# Link 2c's dashboard login, run again by Link 8e after the release hop.
# A real login POST is the only check that exercises the dashboard's
# server-side Keystone path end-to-end: Django authenticates against
# OPENSTACK_KEYSTONE_URL and then lists the user's projects via the
# catalog interface selected by OPENSTACK_ENDPOINT_TYPE. A login-page
# GET renders without a live Keystone and misses both (which is how
# an unreachable keystoneEndpoint once passed CI). The login succeeds
# with the 302 redirect plus a sessionid cookie; a failed login
# re-renders the form with HTTP 200.
#
# With that session the check requests /project/api_access/ and
# expects HTTP 200. The page proves that Horizon accepts the session
# the login set. This plane's cookies stay far under uWSGI's default
# buffer of 4096 bytes, so the page does not test --buffer-size; the
# 6000- and 60000-byte cookie requests of
# tests/e2e/horizon/basic-deployment/ and its 2026.2 twin do. The
# page's view builds the table from request.user.service_catalog alone
# (IndexView.get_data), so the page renders at Link 2c with Keystone
# as the only service.
# Last, the names and values of the cookies the page request sends
# must sum to less than Horizon's SESSION_COOKIE_MAX_SIZE of 4093
# bytes. The check is red where Horizon would log its "Total Cookie
# size" ERROR line for this plane, and the sizes it prints record the
# trend in every run. The sizes are those of the cookies the login set:
# the view replaces the service ids in the session's catalog with list
# indexes, so the cookie its response sets is smaller.
#
# HORIZON_SESSION_BACKEND names the session store the dashboard runs,
# SignedCookies unless set. With Cache the sessionid value must be the
# 32-character key of Django's cache session engine; the cookie bound
# stays, since the browser sends every other cookie too.
horizon_login_round_trip() {
  echo "== Horizon login POST with ${HORIZON_SESSION_BACKEND:-SignedCookies} sessions (expect 302 + sessionid) =="
  ADMIN_PW="$(kubectl get secret "${KEYSTONE_CR}-admin-credentials" -n "${CP_NS}" \
    -o 'jsonpath={.data.password}' | base64 -d)"
  kubectl run cp-horizon-login --image="${LOGIN_CLIENT_IMAGE}" --rm -i --restart=Never \
    -n "${CP_NS}" \
    --env "HORIZON_BASE=http://${HORIZON_CR}.${CP_NS}.svc:8080" \
    --env "OS_PASSWORD=${ADMIN_PW}" \
    --env "HORIZON_SESSION_BACKEND=${HORIZON_SESSION_BACKEND:-SignedCookies}" \
    --command -- python3 - <<'PYEOF'
import http.client
import http.cookiejar
import os
import re
import urllib.error
import urllib.parse
import urllib.request

# Horizon's SESSION_COOKIE_MAX_SIZE (horizon/defaults.py), read from the
# 2026.1 and 2026.2 images (Horizon 25.7.0 and 27.0.0).
# HorizonMiddleware._process_request (horizon/middleware/base.py) sums
# len(name) + len(value) over request.COOKIES and logs "Total Cookie size"
# at ERROR once the sum reaches it.
SESSION_COOKIE_MAX_SIZE = 4093

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

base = os.environ['HORIZON_BASE']
login_url = base + '/auth/login/'
jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(
    urllib.request.HTTPCookieProcessor(jar), NoRedirect())
body = opener.open(login_url).read().decode()
token = re.search('name="csrfmiddlewaretoken" value="([^"]+)"', body).group(1)
form = urllib.parse.urlencode({
    'csrfmiddlewaretoken': token,
    'region': 'default',
    'domain': 'Default',
    'username': 'admin',
    'password': os.environ['OS_PASSWORD'],
}).encode()
req = urllib.request.Request(login_url, data=form,
                             headers={'Referer': login_url})
try:
    status = opener.open(req).status
except urllib.error.HTTPError as err:
    status = err.code
assert status == 302, (
    'login POST returned HTTP %s (expected 302); the dashboard could '
    'not authenticate against Keystone' % status)
assert any(c.name == 'sessionid' for c in jar), \
    'no sessionid cookie after the login redirect'
if os.environ['HORIZON_SESSION_BACKEND'] == 'Cache':
    session_key = next(c.value for c in jar if c.name == 'sessionid')
    assert len(session_key) == 32, (
        'sessionid value has %d characters, not the 32 of a cache session '
        'key; the dashboard still keeps the session in the cookie'
        % len(session_key))
    print('OK: sessionid is a 32-character cache session key')
print('OK: dashboard login round-trip succeeded (302 + sessionid)')

# The cookies the page request sends, the sum Horizon's middleware takes.
total = sum(len(c.name) + len(c.value) for c in jar)
session_size = next(len(c.name) + len(c.value) for c in jar if c.name == 'sessionid')

page = '/project/api_access/'
location = None
try:
    resp = opener.open(base + page)
    resp.read()
    status = resp.status
except urllib.error.HTTPError as err:
    status = err.code
    location = err.headers.get('Location')
except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
    raise SystemExit(
        'logged-in page %s got no response (%r); uWSGI drops a request '
        'whose line and headers exceed its --buffer-size' % (page, exc)) from exc
if status == 302:
    raise SystemExit(
        'logged-in page %s returned HTTP 302 to %s; Horizon did not accept '
        'the session cookie' % (page, location))
if status != 200:
    raise SystemExit(
        'logged-in page %s returned HTTP %s (expected 200)' % (page, status))
if total >= SESSION_COOKIE_MAX_SIZE:
    raise SystemExit(
        'cookies sum to %d bytes (sessionid %d), at or over the %d at which '
        'Horizon logs "Total Cookie size"; the catalog no longer fits a '
        'signed-cookie session' % (total, session_size, SESSION_COOKIE_MAX_SIZE))
print("OK: logged-in page %s served (HTTP 200); sessionid cookie %d bytes, "
      "all cookies %d bytes, under Horizon's %d"
      % (page, session_size, total, SESSION_COOKIE_MAX_SIZE))
PYEOF
  echo "OK: Horizon authenticated against Keystone, served a logged-in page, and its cookies fit a browser"
}

# Link 8f: the dashboard moves to cache sessions, on the next release, and a
# login works with them. The merge patch sets services.horizon.session on
# the ControlPlane, which projects it onto the Horizon child. The child must
# carry the block, report Ready at its new generation and finish the
# rollout, and its old pods must be gone, so no login reaches a pod that
# still signs its cookies. All of that shares one deadline, settle_seconds
# from the patch on, which the step 1 timeout of chainsaw-test.yaml budgets
# as 5 minutes. The mounted ConfigMap must carry the cache engine, the
# "sessions" alias and its KEY_PREFIX horizon-sessions-e7c8fa627bdb, whose
# hex characters are the start of the SHA-256 of the child's
# "<namespace>/<name>". The login of Link 2c then runs against cache
# sessions. The link runs last, so the signed-cookie bound of Link 2c and
# Link 8e measures the full catalog first.
horizon_cache_sessions_link() {
  echo "== Link 8f: switch the dashboard to cache sessions =="
  kubectl patch controlplane "${CP_NAME}" -n "${CP_NS}" --type merge \
    -p '{"spec":{"services":{"horizon":{"session":{"backend":"Cache"}}}}}'
  local settle_seconds=300
  local deadline state backend gen obs ready terminating cm settings want
  deadline=$(( $(date +%s) + settle_seconds ))
  while :; do
    state="$(kubectl get horizon "${HORIZON_CR}" -n "${CP_NS}" -o \
      "jsonpath={.spec.session.backend}|{.metadata.generation}|{.status.observedGeneration}|{.status.conditions[?(@.type=='Ready')].status}" \
      2>/dev/null || true)"
    IFS='|' read -r backend gen obs ready <<<"${state}"
    if [ "${backend}" = Cache ] && [ -n "${gen}" ] && [ "${gen}" = "${obs}" ] && [ "${ready}" = True ]; then
      break
    fi
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "FAIL: Horizon ${HORIZON_CR} did not settle on cache sessions within ${settle_seconds}s" \
        "(backend|generation|observedGeneration|Ready: ${state})"
      exit 1
    fi
    sleep 5
  done
  kubectl rollout status "deploy/${HORIZON_CR}" -n "${CP_NS}" \
    --timeout="$(( deadline - $(date +%s) > 1 ? deadline - $(date +%s) : 1 ))s"
  while :; do
    terminating="$(kubectl get pods -n "${CP_NS}" -l "app.kubernetes.io/instance=${HORIZON_CR}" \
      -o 'jsonpath={.items[*].metadata.deletionTimestamp}')"
    [ -z "${terminating}" ] && break
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "FAIL: pods of ${HORIZON_CR} still terminating after ${settle_seconds}s"; exit 1
    fi
    sleep 2
  done
  cm="$(kubectl get deploy "${HORIZON_CR}" -n "${CP_NS}" \
    -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')"
  settings="$(kubectl get cm "${cm}" -n "${CP_NS}" -o 'jsonpath={.data.local_settings\.py}')"
  for want in 'SESSION_ENGINE = "django.contrib.sessions.backends.cache"' \
    'SESSION_CACHE_ALIAS = "sessions"' '"KEY_PREFIX": "horizon-sessions-e7c8fa627bdb"'; do
    if ! grep -qF "${want}" <<<"${settings}"; then
      echo "FAIL: ${cm} does not carry the cache session setting ${want}"; exit 1
    fi
  done
  echo "OK: ${HORIZON_CR} runs cache sessions from ${cm}"
  HORIZON_SESSION_BACKEND=Cache horizon_login_round_trip
}
