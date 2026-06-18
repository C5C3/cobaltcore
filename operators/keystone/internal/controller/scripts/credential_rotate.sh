#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
set -eu
keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_rotate
keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_migrate
# NOTE: The Python K8s API PATCH block below MUST stay in sync with fernet_rotate.sh.
python3 << 'PYTHON'
import os, json, base64, glob, ssl, http.client, datetime
data = {}
for f in sorted(glob.glob("/etc/keystone/credential-keys/*")):
    if os.path.isfile(f):
        with open(f, "rb") as fh:
            data[os.path.basename(f)] = base64.b64encode(fh.read()).decode()
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
print("Credential keys staging Secret updated successfully")
PYTHON
