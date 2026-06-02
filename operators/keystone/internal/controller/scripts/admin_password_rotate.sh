#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
set -eu
# Generate a fresh strong admin password and PATCH it onto the staging Secret
# (CC-0109, REQ-006). Unlike fernet_rotate.sh / credential_rotate.sh there is no
# keystone-manage step: admin-password rotation mints a brand-new random secret
# rather than rotating an on-disk key repository, so the CronJob never touches
# Keystone state directly. The operator validates the staged password and
# commits it onto the operator-owned push-source Secret (split-compute-write,
# CC-0081). Only Python standard-library modules are used to avoid image
# dependencies (CC-0013).
# NOTE: The Python K8s API PATCH block below MUST stay in sync with
# fernet_rotate.sh / credential_rotate.sh (CC-0073, CC-0081, W-004).
python3 << 'PYTHON'
import os, json, base64, ssl, http.client, datetime, secrets
# PASSWORD_LENGTH is the number of random bytes of entropy requested.
# secrets.token_urlsafe(nbytes) returns a URL-safe text string of roughly
# 1.3 characters per byte, so the generated password is always at least
# PASSWORD_LENGTH characters long — satisfying the operator-side minimum-length
# check in validateAdminPasswordRotationOutput (CC-0109, REQ-006, REQ-011).
length = int(os.environ.get("PASSWORD_LENGTH", "32"))
password = secrets.token_urlsafe(length)
# Secret .data values are base64-encoded; encode the password the same way the
# fernet/credential scripts encode their key bytes.
data = {"password": base64.b64encode(password.encode()).decode()}
with open("/var/run/secrets/kubernetes.io/serviceaccount/token") as f:
    token = f.read().strip()
ctx = ssl.create_default_context(cafile="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
conn = http.client.HTTPSConnection("kubernetes.default.svc", context=ctx)
completed_at = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
body = {
    "metadata": {"annotations": {"forge.c5c3.io/rotation-completed-at": completed_at}},
    "data": data,
}
conn.request("PATCH",
    "/api/v1/namespaces/{}/secrets/{}".format(os.environ["SECRET_NAMESPACE"], os.environ["SECRET_NAME"]),
    json.dumps(body),
    {"Authorization": "Bearer " + token, "Content-Type": "application/strategic-merge-patch+json"})
resp = conn.getresponse()
if resp.status >= 300:
    raise RuntimeError("Secret update failed: {} {}".format(resp.status, resp.read().decode()))
conn.close()
print("Admin password staging Secret updated successfully")
PYTHON
