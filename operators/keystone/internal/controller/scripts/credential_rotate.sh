#!/bin/sh
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
set -eu
# A failed staging PATCH makes the kubelet restart this container in the same
# pod (restartPolicy OnFailure). The emptyDirs keep their content, so a retry
# must not rotate the keys again. The first attempt saves the digest of the
# production keys it rotates in $STATE_DIR/source and the rotated key set in
# $STATE_DIR/rotated; a retry restores the saved set and repeats only the
# second migration and the PATCH. An attempt without a saved set starts over
# from the production keys.
# A retry starts over as well when the production keys no longer match the
# saved digest: a Job created with kubectl create job --from can commit its
# rotation in the meantime, and the saved set would drop the keys that Job
# added. A retry that finds the saved set in production exits 0, because the
# operator committed a PATCH whose response this pod never received.
# KEY_DIR, SRC_DIR and STATE_DIR default to the mounts of keyRotationCronJob;
# only the script tests set them.
KEY_DIR=${KEY_DIR:-/etc/keystone/credential-keys}
export KEY_DIR
SRC_DIR=${SRC_DIR:-/credential-keys-src}
STATE_DIR=${STATE_DIR:-/rotation-state}
if [ ! -d "$STATE_DIR" ]; then
  echo "ERROR: rotation state directory $STATE_DIR does not exist" >&2
  exit 1
fi
# key_digest prints the SHA-256 and the name of every key in the directory $1.
key_digest() {
  (cd "$1" && sha256sum -- *)
}
if [ -d "$STATE_DIR/rotated" ]; then
  production=$(key_digest "$SRC_DIR")
  if [ "$production" = "$(key_digest "$STATE_DIR/rotated")" ]; then
    echo "Retry: the production Secret already holds the credential keys this pod rotated; nothing to PATCH"
    exit 0
  fi
  if [ "$production" != "$(cat "$STATE_DIR/source")" ]; then
    echo "Retry: the production credential keys changed since this pod rotated; starting over"
    rm -rf "${STATE_DIR:?}/rotated"
  fi
fi
rm -f "${KEY_DIR:?}"/*
if [ -d "$STATE_DIR/rotated" ]; then
  echo "Retry: restoring the credential keys this pod rotated earlier; not rotating again"
  install -m 0400 "$STATE_DIR/rotated"/* "$KEY_DIR"/
else
  install -m 0400 "$SRC_DIR"/* "$KEY_DIR"/
  key_digest "$KEY_DIR" > "$STATE_DIR/source"
  # credential_rotate refuses while any credential is not under the primary
  # key. A Job that gave up after its migration, a replaced pod, or a
  # credential an API pod created under the previous primary while the new
  # key set was still being projected leaves such credentials behind. This
  # migration puts them under the production primary first; with nothing to
  # re-encrypt it writes nothing.
  keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_migrate
  keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_rotate
  rm -rf "${STATE_DIR:?}/rotated.tmp"
  mkdir "$STATE_DIR/rotated.tmp"
  install -m 0400 "$KEY_DIR"/* "$STATE_DIR/rotated.tmp"/
  mv "$STATE_DIR/rotated.tmp" "$STATE_DIR/rotated"
fi
# This migration re-encrypts the credentials under the new primary key. A
# retry runs it again: it re-encrypts only credentials under another key, and
# it fails before the PATCH when a credential is under a key the restored set
# lacks.
keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ credential_migrate
# NOTE: The Python K8s API PATCH block below MUST stay in sync with fernet_rotate.sh.
python3 << 'PYTHON'
import os, json, base64, glob, ssl, http.client, datetime
data = {}
for f in sorted(glob.glob(os.path.join(os.environ["KEY_DIR"], "*"))):
    if os.path.isfile(f):
        with open(f, "rb") as fh:
            data[os.path.basename(f)] = base64.b64encode(fh.read()).decode()
with open("/var/run/secrets/kubernetes.io/serviceaccount/token") as f:
    token = f.read().strip()
ctx = ssl.create_default_context(cafile="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
conn = http.client.HTTPSConnection("kubernetes.default.svc", context=ctx)
completed_at = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
body = {
    "metadata": {"annotations": {"cobaltcore.c5c3.io/rotation-completed-at": completed_at}},
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
