# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Pre-insert the admin region row before keystone-manage bootstrap runs.

This script resolves the database URL exactly the way Keystone does at
runtime: prefer the OS_DATABASE__CONNECTION env override (set by buildDBConnectionEnvVar from the derived <name>-db-connection Secret) and fall back to keystone.conf only when the env var is
unset. Stdlib configparser has no knowledge of oslo.config env overrides,
so reading the conf file alone would resolve to the placeholder host
introduced by and fail DNS.

: the DSN may carry ssl_ca/ssl_cert/ssl_key plus
ssl_verify_cert/ssl_verify_identity as pymysql-style query parameters.
pymysql.connect() ignores unknown URL query keys, so this script parses
them out of the query string and rebuilds a pymysql ssl={...} dict. The
mapping is:

    ssl_verify_cert=true       -> verify_mode = ssl.CERT_REQUIRED
    ssl_verify_cert absent     -> verify_mode = ssl.CERT_NONE
    ssl_verify_identity=true   -> check_hostname = True
    ssl_verify_identity absent -> check_hostname = False

The ssl= kwarg is only passed when at least one ssl_* parameter was
present so plaintext DSNs (TLS disabled) keep their pre-existing behavior.

Environment inputs:

    OS_DATABASE__CONNECTION  oslo.config env override of [database].connection
                            . When unset the script falls
                             back to /etc/keystone/keystone.conf.d/*.conf.
    BOOTSTRAP_REGION_ID      the region id used for the INSERT IGNORE
                             (matches --bootstrap-region-id passed to
                             keystone-manage bootstrap by the wrapper).
"""

import configparser
import glob
import os
import ssl
from urllib.parse import urlparse, parse_qs

import pymysql

conn_url = os.environ.get("OS_DATABASE__CONNECTION")
if not conn_url:
    conf = configparser.RawConfigParser()
    for f in sorted(glob.glob("/etc/keystone/keystone.conf.d/*.conf")):
        conf.read(f)
    conn_url = conf.get("database", "connection")

url = urlparse(conn_url)
db = url.path.lstrip("/")
qs = parse_qs(url.query)
charset = qs.get("charset", ["utf8"])[0]

ssl_kwargs: dict = {}
if "ssl_ca" in qs:
    ssl_kwargs["ca"] = qs["ssl_ca"][0]
if "ssl_cert" in qs:
    ssl_kwargs["cert"] = qs["ssl_cert"][0]
if "ssl_key" in qs:
    ssl_kwargs["key"] = qs["ssl_key"][0]
if ssl_kwargs:
    verify_cert = qs.get("ssl_verify_cert", ["false"])[0].lower() == "true"
    verify_id = qs.get("ssl_verify_identity", ["false"])[0].lower() == "true"
    ssl_kwargs["verify_mode"] = ssl.CERT_REQUIRED if verify_cert else ssl.CERT_NONE
    ssl_kwargs["check_hostname"] = verify_id

connect_kwargs = dict(
    host=url.hostname,
    port=url.port or 3306,
    user=url.username,
    password=url.password,
    database=db,
    charset=charset,
)
if ssl_kwargs:
    connect_kwargs["ssl"] = ssl_kwargs

conn = pymysql.connect(**connect_kwargs)
try:
    cur = conn.cursor()
    cur.execute(
        "INSERT IGNORE INTO region (id, description, extra) VALUES (%s, %s, %s)",
        (os.environ["BOOTSTRAP_REGION_ID"], "", "{}"),
    )
    conn.commit()
finally:
    conn.close()
