#!/bin/bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/c5c3/full-controlplane-keystone/horizon-login.sh: the dashboard
# login of Link 2c, which Link 8e runs again after the release hop.
#
# The step 1 script of chainsaw-test.yaml sources this file among its
# helpers:
#
#   . ./horizon-login.sh
#
# Chainsaw hands a script's content to bash as one argument, and Linux caps
# one argument at 128 KiB (MAX_ARG_STRLEN). With this check inline the step 1
# script passes that cap and bash never starts. Sourced, the function runs in
# the step's own shell: it reads KEYSTONE_CR, HORIZON_CR, CP_NS and
# LOGIN_CLIENT_IMAGE from it, runs under its set -euo pipefail, and a failed
# check ends the step.

# Link 2c's dashboard login, run again by Link 8e after the release hop.
# A real login POST is the only check that exercises the dashboard's
# server-side Keystone path end-to-end: Django authenticates against
# OPENSTACK_KEYSTONE_URL and then lists the user's projects via the
# catalog interface selected by OPENSTACK_ENDPOINT_TYPE. A login-page
# GET renders without a live Keystone and misses both (which is how
# an unreachable keystoneEndpoint once passed CI). Success is the 302
# redirect plus a sessionid cookie; a failed login re-renders the
# form with HTTP 200.
horizon_login_round_trip() {
  echo "== Horizon login POST (expect 302 + sessionid) =="
  ADMIN_PW="$(kubectl get secret "${KEYSTONE_CR}-admin-credentials" -n "${CP_NS}" \
    -o 'jsonpath={.data.password}' | base64 -d)"
  kubectl run cp-horizon-login --image="${LOGIN_CLIENT_IMAGE}" --rm -i --restart=Never \
    -n "${CP_NS}" \
    --env "HORIZON_BASE=http://${HORIZON_CR}.${CP_NS}.svc:8080" \
    --env "OS_PASSWORD=${ADMIN_PW}" \
    --command -- python3 - <<'PYEOF'
import http.cookiejar
import os
import re
import urllib.error
import urllib.parse
import urllib.request

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
print('OK: dashboard login round-trip succeeded (302 + sessionid)')
PYEOF
  echo "OK: Horizon authenticated against Keystone through the cluster-local endpoint"
}
