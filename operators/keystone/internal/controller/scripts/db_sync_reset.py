# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Drop the schema an interrupted first keystone-manage db_sync left behind.

MariaDB commits every DDL statement implicitly, while alembic records a
revision with DML that only the next DDL statement commits. A connection
lost inside a migration therefore leaves the revision's tables without its
alembic_version row. The next db_sync starts the revision again and fails
on its first CREATE TABLE with MySQL error 1050 until the Job's backoff
limit. The db-sync Job runs this script before keystone-manage db_sync, so
that db_sync rebuilds the schema instead.

The script reads the database URL from OS_DATABASE__CONNECTION and falls
back to the [database] connection of /etc/keystone/keystone.conf.d/*.conf.
It waits without bound while the connect fails with a connection error,
then decides in this order and prints one line for the outcome:

    no base table                          -> nothing to reset
    migrate_version exists                 -> left untouched
    alembic_version records the heads      -> nothing to reset
    heads unreadable, revisions recorded   -> left untouched
    a table other than alembic_version
    holds a row                            -> left untouched
    otherwise                              -> drop every base table

The heads are the EXPAND_HEAD and CONTRACT_HEAD files of the keystone
package in the image. A row of the project table counts only when its id
is not <<keystone.domain.root>>, the row the initial revision inserts.

The connection runs in autocommit, so its own reads hold no lock between
statements. A statement that waits LOCK_WAIT_SECONDS for a metadata lock
another session holds fails with error 1205; the script then prints a
line, waits RETRY_SECONDS and decides again from a fresh read, without
bound. Every outcome exits 0, and keystone-manage db_sync runs next. Any
other error exits 1, so sh -e stops before db_sync and the Job counts a
failed attempt. No line prints the URL or its password.

dbSyncScript in database_jobs.go pipes this file to python3 through a
quoted heredoc that ends at a line reading PY alone, so no line of this
file may read that.
"""

from __future__ import annotations

import configparser
import glob
import importlib.util
import os
import ssl
import time
from urllib.parse import parse_qs, unquote, urlparse

import pymysql

CONF_GLOB = "/etc/keystone/keystone.conf.d/*.conf"
ROOT_PROJECT_ID = "<<keystone.domain.root>>"
RETRY_SECONDS = 5
# The connect errors oslo.db retries under [database] max_retries = -1:
# oslo_db.sqlalchemy.exc_filters._is_db_connection_error turns these into
# the DBConnectionError that engines._test_connection retries. Any other
# error, such as 1045 for a refused password, fails the attempt the way
# db_sync itself would.
RETRY_CODES = frozenset({1047, 2002, 2003, 2006, 2013})
# ER_LOCK_WAIT_TIMEOUT: a statement waited lock_wait_timeout seconds for a
# lock. MariaDB's default of 86400 s would let a DROP that another session's
# open transaction blocks wait a day without a log line.
LOCK_WAIT_TIMEOUT = 1205
LOCK_WAIT_SECONDS = 30

TABLES_SQL = (
    "SELECT TABLE_NAME FROM information_schema.TABLES"
    " WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'"
)
REVISIONS_SQL = "SELECT version_num FROM alembic_version"
PROJECT_ROWS_SQL = "SELECT 1 FROM `project` WHERE id <> %s LIMIT 1"


def say(message: str) -> None:
    """Print one line of the pre-step's log and flush it at once."""
    print("db-sync-reset: " + message, flush=True)


# resolve_url and connect_kwargs resolve the database the way
# bootstrap_db_seed.py does; keep the two scripts in step.
def resolve_url() -> str:
    """Return the database URL: the env override first, the config second."""
    url = os.environ.get("OS_DATABASE__CONNECTION")
    if url:
        return url
    conf = configparser.RawConfigParser()
    for path in sorted(glob.glob(CONF_GLOB)):
        conf.read(path)
    return conf.get("database", "connection")


def connect_kwargs(url: str) -> dict:
    """Return the pymysql.connect keyword arguments for url.

    The user, password and database are percent-decoded the way SQLAlchemy
    decodes them. The ssl_* query parameters of the URL become a pymysql ssl
    dict, passed only when at least one of ssl_ca, ssl_cert and ssl_key is
    present.
    """
    parsed = urlparse(url)
    qs = parse_qs(parsed.query)

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

    kwargs = dict(
        host=parsed.hostname,
        port=parsed.port or 3306,
        user=None if parsed.username is None else unquote(parsed.username),
        password=None if parsed.password is None else unquote(parsed.password),
        database=unquote(parsed.path.lstrip("/")),
        charset=qs.get("charset", ["utf8"])[0],
    )
    if ssl_kwargs:
        kwargs["ssl"] = ssl_kwargs
    return kwargs


def connect(kwargs: dict) -> pymysql.connections.Connection:
    """Connect, waiting without bound while the database is unreachable."""
    while True:
        try:
            return pymysql.connect(**kwargs)
        except pymysql.err.OperationalError as exc:
            if not exc.args or exc.args[0] not in RETRY_CODES:
                raise
            detail = " ".join(str(arg) for arg in exc.args)
            say(f"database not reachable ({detail}), retrying in {RETRY_SECONDS} s")
            time.sleep(RETRY_SECONDS)


def image_heads() -> tuple[str, str] | None:
    """Return the expand and contract heads of the image's keystone package.

    Return None when the package or either head file is missing.
    """
    spec = importlib.util.find_spec("keystone")
    if spec is None or spec.origin is None:
        return None
    versions = os.path.join(
        os.path.dirname(spec.origin), "common", "sql", "migrations", "versions"
    )
    heads = []
    for name in ("EXPAND_HEAD", "CONTRACT_HEAD"):
        try:
            with open(os.path.join(versions, name), encoding="utf-8") as f:
                heads.append(f.read().strip())
        except FileNotFoundError:
            return None
    return heads[0], heads[1]


def quote(name: str) -> str:
    """Return name as a backtick-quoted identifier, inner backticks doubled."""
    return "`" + name.replace("`", "``") + "`"


def base_tables(cur: pymysql.cursors.Cursor) -> list[str]:
    """Return the base tables of the connection's schema, sorted."""
    cur.execute(TABLES_SQL)
    return sorted(row[0] for row in cur.fetchall())


def recorded_revisions(cur: pymysql.cursors.Cursor, tables: list[str]) -> set[str]:
    """Return the revisions alembic_version records, empty without the table."""
    if "alembic_version" not in tables:
        return set()
    cur.execute(REVISIONS_SQL)
    return {row[0] for row in cur.fetchall()}


def holds_rows(cur: pymysql.cursors.Cursor, name: str) -> bool:
    """Return whether table name holds a row; the root project row does not count."""
    if name == "project":
        cur.execute(PROJECT_ROWS_SQL, (ROOT_PROJECT_ID,))
    else:
        cur.execute(f"SELECT 1 FROM {quote(name)} LIMIT 1")
    return cur.fetchone() is not None


def drop_tables(cur: pymysql.cursors.Cursor, names: list[str]) -> None:
    """Drop the tables names with foreign key checks off."""
    cur.execute("SET FOREIGN_KEY_CHECKS = 0")
    cur.execute("DROP TABLE IF EXISTS " + ", ".join(quote(name) for name in names))
    cur.execute("SET FOREIGN_KEY_CHECKS = 1")


def reset(cur: pymysql.cursors.Cursor, heads: tuple[str, str] | None) -> None:
    """Drop the schema when an interrupted first db_sync left it, else leave it."""
    tables = base_tables(cur)
    if not tables:
        say("schema is empty, nothing to reset")
        return
    if "migrate_version" in tables:
        say("schema carries migrate_version (sqlalchemy-migrate), left untouched")
        return
    revisions = recorded_revisions(cur, tables)
    if heads is not None and revisions == set(heads):
        say(f"schema is at the image heads ({heads[0]}, {heads[1]}), nothing to reset")
        return
    recorded = " ".join(sorted(revisions))
    if heads is None and revisions:
        say(f"image heads unreadable and the schema records {recorded}, left untouched")
        return
    for name in tables:
        if name != "alembic_version" and holds_rows(cur, name):
            say(f"table {name} holds rows, schema left untouched")
            return
    drop_tables(cur, tables)
    say(
        f"dropped {len(tables)} tables an interrupted first db_sync left"
        f" (recorded revisions: {recorded or 'none'}): {', '.join(tables)}"
    )


def reset_after_lock_waits(
    conn: pymysql.connections.Connection, heads: tuple[str, str] | None
) -> None:
    """Run reset, deciding again after each statement a lock wait failed."""
    while True:
        try:
            reset(conn.cursor(), heads)
            return
        except pymysql.err.OperationalError as exc:
            if not exc.args or exc.args[0] != LOCK_WAIT_TIMEOUT:
                raise
            say(
                f"a statement waited {LOCK_WAIT_SECONDS} s for a metadata lock another"
                f" session holds, checking the schema again in {RETRY_SECONDS} s"
            )
            time.sleep(RETRY_SECONDS)


def main() -> None:
    """Run the pre-step."""
    kwargs = connect_kwargs(resolve_url())
    # The session settings stay out of connect_kwargs, which
    # bootstrap_db_seed.py mirrors.
    kwargs["autocommit"] = True
    kwargs["init_command"] = f"SET SESSION lock_wait_timeout = {LOCK_WAIT_SECONDS}"
    conn = connect(kwargs)
    try:
        reset_after_lock_waits(conn, image_heads())
    finally:
        conn.close()


if __name__ == "__main__":
    main()
