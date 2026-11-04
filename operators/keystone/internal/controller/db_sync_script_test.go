// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// dbSyncShPath is the shell the db-sync container runs dbSyncScript with.
const dbSyncShPath = "/bin/sh"

// dbSyncStubURL is the database URL the reset cases run with. Its password is
// the needle of the leak check.
const dbSyncStubURL = "mysql+pymysql://keystone:stub-pw@db.invalid:3306/keystone"

// dbSyncCall is the argument line keystone-manage receives from dbSyncScript.
const dbSyncCall = "--config-dir=/etc/keystone/keystone.conf.d/ db_sync"

// dbSyncStubPrelude lays down the keystone-manage stub and puts it first on
// PATH. The stub appends its arguments to $KM_LOG, and a keystone-manage line
// to the pymysql stub's log, so that log shows the order of statements and
// call. The shell writes it because a PATH entry has to be executable.
const dbSyncStubPrelude = `mkdir -p bin
cat > bin/keystone-manage <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$KM_LOG"
printf 'keystone-manage %s\n' "$*" >> "$PYMYSQL_STUB_LOG"
STUB
chmod 0700 bin/keystone-manage
PATH="$PWD/bin:$PATH"
export PATH
`

// dbSyncStubPymysqlErr stands in for pymysql.err.
const dbSyncStubPymysqlErr = `class Error(Exception):
    pass


class OperationalError(Error):
    pass


class ProgrammingError(Error):
    pass
`

// dbSyncStubPymysql stands in for pymysql. It answers the statements of
// db_sync_reset.py and bootstrap_db_seed.py from the case in the JSON file
// $PYMYSQL_STUB and appends a line per call to $PYMYSQL_STUB_LOG: the connect keyword arguments as JSON,
// each statement with its arguments, and each time.sleep, which it replaces so
// a retry returns at once. Any statement it does not know fails the script.
// chr(96) is the backtick a Go raw string cannot hold.
const dbSyncStubPymysql = `import json
import os
import time

from . import err

BT = chr(96)

with open(os.environ["PYMYSQL_STUB"], encoding="utf-8") as f:
    CASE = json.load(f)
TABLES = CASE["tables"] or {}
ALEMBIC = CASE["alembic"]
CONNECT_ERRORS = list(CASE["connect_errors"] or [])
DROP_ERRORS = list(CASE["drop_errors"] or [])


def log(line):
    with open(os.environ["PYMYSQL_STUB_LOG"], "a", encoding="utf-8") as f:
        f.write(line + "\n")


def sleep(seconds):
    log("sleep %s" % seconds)


time.sleep = sleep


def quote(name):
    return BT + name.replace(BT, BT + BT) + BT


def connect(**kwargs):
    if CONNECT_ERRORS:
        raise err.OperationalError(*CONNECT_ERRORS.pop(0))
    log("connect " + json.dumps(kwargs, sort_keys=True))
    return Connection()


class Connection:
    def cursor(self):
        return Cursor()

    def commit(self):
        pass

    def close(self):
        pass


class Cursor:
    rows = []

    def execute(self, sql, args=None):
        log("execute " + sql + ("" if args is None else "\t" + repr(args)))
        if sql.startswith("DROP TABLE IF EXISTS ") and DROP_ERRORS:
            raise err.OperationalError(*DROP_ERRORS.pop(0))
        self.rows = self.answer(sql, args)

    def answer(self, sql, args):
        if sql == (
            "SELECT TABLE_NAME FROM information_schema.TABLES"
            " WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'"
        ):
            names = list(TABLES) + ([] if ALEMBIC is None else ["alembic_version"])
            return [(name,) for name in names]
        if sql == "SELECT version_num FROM alembic_version":
            if ALEMBIC is None:
                raise err.ProgrammingError(1146, "Table 'keystone.alembic_version' doesn't exist")
            return [(rev,) for rev in ALEMBIC]
        if sql == "SELECT 1 FROM " + quote("project") + " WHERE id <> %s LIMIT 1":
            return [(1,) for row in TABLES["project"] if row != args[0]]
        for name in TABLES:
            if sql == "SELECT 1 FROM " + quote(name) + " LIMIT 1":
                return [(1,) for row in TABLES[name]]
        if sql.startswith(("DROP TABLE IF EXISTS ", "INSERT IGNORE INTO region ")) or sql in (
            "SET FOREIGN_KEY_CHECKS = 0",
            "SET FOREIGN_KEY_CHECKS = 1",
        ):
            return []
        raise AssertionError("unexpected statement: " + sql)

    def fetchall(self):
        return self.rows

    def fetchone(self):
        return self.rows[0] if self.rows else None
`

// hostKeystoneProbe exits 0 when the host's own python3 finds a keystone
// package, which would stand in for the one a case leaves out.
const hostKeystoneProbe = `import importlib.util, sys; sys.exit(0 if importlib.util.find_spec("keystone") else 1)`

// dbSyncState is the database a reset case connects to, in the shape the
// pymysql stub reads.
type dbSyncState struct {
	// ConnectErrors are raised as OperationalError(code, message), one per
	// connect, before a connect succeeds.
	ConnectErrors [][]any `json:"connect_errors"`
	// Tables maps every base table but alembic_version to the ids of its rows.
	Tables map[string][]string `json:"tables"`
	// Alembic are the rows of alembic_version; nil means the table is absent.
	Alembic []string `json:"alembic"`
	// DropErrors are raised as OperationalError(code, message), one per
	// DROP TABLE, before a DROP TABLE succeeds.
	DropErrors [][]any `json:"drop_errors"`
}

// dbSyncHeads selects the keystone package a reset case puts on PYTHONPATH.
type dbSyncHeads int

const (
	// bothHeads writes the package with EXPAND_HEAD and CONTRACT_HEAD.
	bothHeads dbSyncHeads = iota
	// noKeystonePackage writes no keystone package.
	noKeystonePackage
	// expandHeadOnly writes the package without CONTRACT_HEAD.
	expandHeadOnly
)

// dbSyncRun is what one run of dbSyncScript left behind.
type dbSyncRun struct {
	code   int
	stdout string
	stderr string
	// log holds the pymysql stub's lines and the keystone-manage calls.
	log []string
	// km holds the argument line of every keystone-manage call.
	km []string
}

// linesWithPrefix returns the log lines that start with one of prefixes, in
// log order.
func (r dbSyncRun) linesWithPrefix(prefixes ...string) []string {
	var lines []string
	for _, line := range r.log {
		for _, prefix := range prefixes {
			if strings.HasPrefix(line, prefix) {
				lines = append(lines, line)
				break
			}
		}
	}
	return lines
}

// connectKwargs returns the keyword arguments of the successful connect.
func (r dbSyncRun) connectKwargs(g Gomega) map[string]any {
	connects := r.linesWithPrefix("connect ")
	g.Expect(connects).To(HaveLen(1))
	var kwargs map[string]any
	g.Expect(json.Unmarshal([]byte(strings.TrimPrefix(connects[0], "connect ")), &kwargs)).To(Succeed())
	return kwargs
}

// requireDBSyncTools skips the test when the shell or python3 is missing. A
// GitHub-hosted runner ships both, so there it fails instead, and a green CI
// test job means the cases ran.
func requireDBSyncTools(t *testing.T) {
	t.Helper()
	for _, binary := range []string{dbSyncShPath, "python3"} {
		if _, err := exec.LookPath(binary); err != nil {
			if os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted" {
				t.Fatalf("%s is unavailable on a GitHub-hosted runner: %v", binary, err)
			}
			t.Skipf("%s is unavailable; dbSyncScript cannot be exercised here: %v", binary, err)
		}
	}
}

// writeDBSyncFile writes content to the path rel under dir, creating its
// directories.
func writeDBSyncFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	g := NewGomegaWithT(t)
	path := filepath.Join(dir, rel)
	g.Expect(os.MkdirAll(filepath.Dir(path), 0o750)).To(Succeed())
	g.Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
}

// runDBSyncScript runs script, a container program such as dbSyncScript, behind
// the stub prelude with /bin/sh in a fresh directory, against state and the
// keystone package heads selects. The script travels through a file rather than
// through "sh -c", so the command line stays free of interpolated input. env
// entries come last, so they win; OS_DATABASE__CONNECTION is dbSyncStubURL
// unless env sets it or unsetURL is true.
func runDBSyncScript(t *testing.T, script string, state dbSyncState, heads dbSyncHeads, unsetURL bool, env ...string) dbSyncRun {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := t.TempDir()

	writeDBSyncFile(t, dir, "py/pymysql/__init__.py", dbSyncStubPymysql)
	writeDBSyncFile(t, dir, "py/pymysql/err.py", dbSyncStubPymysqlErr)
	if heads != noKeystonePackage {
		versions := "py/keystone/common/sql/migrations/versions"
		writeDBSyncFile(t, dir, "py/keystone/__init__.py", "")
		writeDBSyncFile(t, dir, versions+"/EXPAND_HEAD", "742c857f1dfb\n")
		if heads != expandHeadOnly {
			writeDBSyncFile(t, dir, versions+"/CONTRACT_HEAD", "c88cdce8f248\n")
		}
	}
	stateJSON, err := json.Marshal(state)
	g.Expect(err).NotTo(HaveOccurred())
	writeDBSyncFile(t, dir, "case.json", string(stateJSON))
	writeDBSyncFile(t, dir, "run.sh", dbSyncStubPrelude+script)

	cmd := exec.CommandContext(t.Context(), dbSyncShPath, "-eu", "run.sh")
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains([]string{"OS_DATABASE__CONNECTION", "PYTHONPATH", "PYMYSQL_STUB", "PYMYSQL_STUB_LOG", "KM_LOG"}, name) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"PYTHONPATH="+filepath.Join(dir, "py"),
		"PYMYSQL_STUB="+filepath.Join(dir, "case.json"),
		"PYMYSQL_STUB_LOG="+filepath.Join(dir, "stub.log"),
		"KM_LOG="+filepath.Join(dir, "km.log"),
	)
	if !unsetURL {
		cmd.Env = append(cmd.Env, "OS_DATABASE__CONNECTION="+dbSyncStubURL)
	}
	cmd.Env = append(cmd.Env, env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	run := dbSyncRun{}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
			"the script must fail by exiting, not by failing to start: %v (%s)", err, stderr.String())
		run.code = exitErr.ExitCode()
	}
	run.stdout = stdout.String()
	run.stderr = stderr.String()
	run.log = readLogLines(t, dir, "stub.log")
	run.km = readLogLines(t, dir, "km.log")
	return run
}

// TestDBSyncScript_Command pins the db-sync container command: the reset
// pre-step through a 'PY' quoted heredoc, then keystone-manage db_sync.
func TestDBSyncScript_Command(t *testing.T) {
	g := NewGomegaWithT(t)

	job := buildDBSyncJob(brownfieldKeystone(), "keystone-config-abc123", "")

	container := findContainerByName(job.Spec.Template.Spec.Containers, "db-sync")
	g.Expect(container).NotTo(BeNil())
	g.Expect(container.Command).To(Equal([]string{"/bin/sh", "-eu", "-c", dbSyncScript}))

	g.Expect(dbSyncResetScript).NotTo(BeEmpty(), "check the go:embed directive")
	g.Expect(dbSyncResetScript).To(HaveSuffix("\n"), "the heredoc terminator must start a line")
	g.Expect(strings.Split(dbSyncResetScript, "\n")).NotTo(ContainElement("PY"),
		"a line reading PY alone would end the heredoc inside the script")
	g.Expect(dbSyncScript).To(HavePrefix("python3 - <<'PY'\n"))
	g.Expect(dbSyncScript).To(ContainSubstring(dbSyncResetScript + "PY\n"))
	g.Expect(dbSyncScript).To(HaveSuffix("\nexec keystone-manage --config-dir=/etc/keystone/keystone.conf.d/ db_sync\n"))
}

// TestDBSyncScript_Reset runs dbSyncScript against a pymysql stub and pins
// what the pre-step drops, what it leaves untouched, and when keystone-manage
// db_sync runs after it.
func TestDBSyncScript_Reset(t *testing.T) {
	requireDBSyncTools(t)

	noSetOrDrop := func(g Gomega, run dbSyncRun) {
		g.Expect(run.linesWithPrefix("execute SET", "execute DROP")).To(BeEmpty())
	}
	ranDBSync := func(g Gomega, run dbSyncRun) {
		g.Expect(run.code).To(Equal(0), run.stderr)
		g.Expect(run.km).To(Equal([]string{dbSyncCall}))
	}
	failedBeforeDBSync := func(g Gomega, run dbSyncRun) {
		g.Expect(run.code).NotTo(Equal(0), run.stdout)
		g.Expect(run.km).To(BeEmpty())
	}
	droppedInOrder := func(g Gomega, run dbSyncRun, drop string) {
		g.Expect(run.linesWithPrefix("execute SET", "execute DROP", "keystone-manage")).To(Equal([]string{
			"execute SET FOREIGN_KEY_CHECKS = 0",
			"execute " + drop,
			"execute SET FOREIGN_KEY_CHECKS = 1",
			"keystone-manage " + dbSyncCall,
		}))
	}
	const projectQuery = "execute SELECT 1 FROM `project` WHERE id <> %s LIMIT 1\t('<<keystone.domain.root>>',)"

	type resetCase struct {
		name     string
		state    dbSyncState
		heads    dbSyncHeads
		unsetURL bool
		check    func(g Gomega, run dbSyncRun)
	}
	cases := []resetCase{
		{
			name: "empty schema",
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal("db-sync-reset: schema is empty, nothing to reset\n"))
				noSetOrDrop(g, run)
				g.Expect(run.connectKwargs(g)).To(Equal(map[string]any{
					"host": "db.invalid", "port": float64(3306), "user": "keystone", "password": "stub-pw",
					"database": "keystone", "charset": "utf8",
					"autocommit": true, "init_command": "SET SESSION lock_wait_timeout = 30",
				}))
			},
		},
		{
			name: "interrupted initial revision",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}, "access_rule": {}},
				Alembic: []string{},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				droppedInOrder(g, run, "DROP TABLE IF EXISTS `access_rule`, `alembic_version`, `application_credential`")
				g.Expect(run.stdout).To(Equal("db-sync-reset: dropped 3 tables an interrupted first db_sync left" +
					" (recorded revisions: none): access_rule, alembic_version, application_credential\n"))
			},
		},
		{
			name: "root project row",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}, "project": {"<<keystone.domain.root>>"}},
				Alembic: []string{},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.log).To(ContainElement(projectQuery))
				droppedInOrder(g, run, "DROP TABLE IF EXISTS `alembic_version`, `application_credential`, `project`")
			},
		},
		{
			name: "project row besides the root",
			state: dbSyncState{
				Tables:  map[string][]string{"project": {"<<keystone.domain.root>>", "default"}},
				Alembic: []string{},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.log).To(ContainElement(projectQuery))
				g.Expect(run.stdout).To(Equal("db-sync-reset: table project holds rows, schema left untouched\n"))
				noSetOrDrop(g, run)
			},
		},
		{
			name: "interrupted later revision",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}, "project": {"<<keystone.domain.root>>"}},
				Alembic: []string{"27e647c0fad4"},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				droppedInOrder(g, run, "DROP TABLE IF EXISTS `alembic_version`, `application_credential`, `project`")
				g.Expect(run.stdout).To(Equal("db-sync-reset: dropped 3 tables an interrupted first db_sync left" +
					" (recorded revisions: 27e647c0fad4): alembic_version, application_credential, project\n"))
			},
		},
		{
			name: "at heads",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}, "project": {"<<keystone.domain.root>>"}},
				Alembic: []string{"742c857f1dfb", "c88cdce8f248"},
			},
			heads: bothHeads,
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal(
					"db-sync-reset: schema is at the image heads (742c857f1dfb, c88cdce8f248), nothing to reset\n"))
				g.Expect(run.linesWithPrefix("execute SELECT 1 ")).To(BeEmpty(), "a schema at the heads needs no row check")
				noSetOrDrop(g, run)
			},
		},
		{
			name: "rows present",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}, "user": {"u1"}},
				Alembic: []string{},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal("db-sync-reset: table user holds rows, schema left untouched\n"))
				noSetOrDrop(g, run)
			},
		},
		{
			name:  "legacy schema",
			state: dbSyncState{Tables: map[string][]string{"migrate_version": {}, "user": {}}},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal(
					"db-sync-reset: schema carries migrate_version (sqlalchemy-migrate), left untouched\n"))
				noSetOrDrop(g, run)
			},
		},
		{
			name: "heads unreadable/revision recorded",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}},
				Alembic: []string{"27e647c0fad4"},
			},
			heads: noKeystonePackage,
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal(
					"db-sync-reset: image heads unreadable and the schema records 27e647c0fad4, left untouched\n"))
				noSetOrDrop(g, run)
			},
		},
		{
			name: "heads unreadable/no revision",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}},
				Alembic: []string{},
			},
			heads: noKeystonePackage,
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				droppedInOrder(g, run, "DROP TABLE IF EXISTS `alembic_version`, `application_credential`")
			},
		},
		{
			name: "heads unreadable/contract head missing",
			state: dbSyncState{
				Tables:  map[string][]string{"application_credential": {}},
				Alembic: []string{"27e647c0fad4"},
			},
			heads: expandHeadOnly,
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal(
					"db-sync-reset: image heads unreadable and the schema records 27e647c0fad4, left untouched\n"))
				noSetOrDrop(g, run)
			},
		},
		{
			name: "database comes back",
			state: dbSyncState{ConnectErrors: [][]any{
				{2003, "Can't connect to MySQL server"},
				{2003, "Can't connect to MySQL server"},
			}},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				retry := "db-sync-reset: database not reachable (2003 Can't connect to MySQL server), retrying in 5 s\n"
				g.Expect(run.stdout).To(Equal(retry + retry + "db-sync-reset: schema is empty, nothing to reset\n"))
				g.Expect(run.linesWithPrefix("sleep ", "connect ")).To(HaveExactElements(
					"sleep 5", "sleep 5", HavePrefix("connect ")))
			},
		},
		{
			name:  "connect error without a code fails the attempt",
			state: dbSyncState{ConnectErrors: [][]any{{}}},
			check: func(g Gomega, run dbSyncRun) {
				failedBeforeDBSync(g, run)
				g.Expect(run.stderr).To(ContainSubstring("OperationalError"))
				g.Expect(run.linesWithPrefix("sleep ")).To(BeEmpty(), "only connection errors are retried")
			},
		},
		{
			name:  "access denied fails the attempt",
			state: dbSyncState{ConnectErrors: [][]any{{1045, "Access denied for user 'keystone'"}}},
			check: func(g Gomega, run dbSyncRun) {
				failedBeforeDBSync(g, run)
				g.Expect(run.stderr).To(ContainSubstring("1045"))
				g.Expect(run.linesWithPrefix("sleep ")).To(BeEmpty(), "only connection errors are retried")
			},
		},
		{
			name: "drop denied",
			state: dbSyncState{
				Tables:     map[string][]string{"application_credential": {}},
				Alembic:    []string{},
				DropErrors: [][]any{{1142, "DROP command denied"}},
			},
			check: func(g Gomega, run dbSyncRun) {
				failedBeforeDBSync(g, run)
				g.Expect(run.stderr).To(ContainSubstring("1142"))
			},
		},
		{
			name: "drop waits on a metadata lock",
			state: dbSyncState{
				Tables:     map[string][]string{"application_credential": {}},
				Alembic:    []string{},
				DropErrors: [][]any{{1205, "Lock wait timeout exceeded; try restarting transaction"}},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.stdout).To(Equal("db-sync-reset: a statement waited 30 s for a metadata lock another" +
					" session holds, checking the schema again in 5 s\n" +
					"db-sync-reset: dropped 2 tables an interrupted first db_sync left" +
					" (recorded revisions: none): alembic_version, application_credential\n"))
				drop := "execute DROP TABLE IF EXISTS `alembic_version`, `application_credential`"
				g.Expect(run.linesWithPrefix("execute SELECT TABLE_NAME ", "execute DROP", "sleep ", "keystone-manage")).
					To(HaveExactElements(
						HavePrefix("execute SELECT TABLE_NAME "), drop, "sleep 5",
						HavePrefix("execute SELECT TABLE_NAME "), drop, "keystone-manage "+dbSyncCall,
					), "the wait reads the schema again before the next DROP")
			},
		},
		{
			name:     "URL absent",
			unsetURL: true,
			check: func(g Gomega, run dbSyncRun) {
				failedBeforeDBSync(g, run)
				g.Expect(run.stderr).To(ContainSubstring("configparser.NoSectionError"))
			},
		},
		{
			name: "quoting",
			state: dbSyncState{
				Tables: map[string][]string{"application_credential": {}, "odd`name": {}},
			},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				droppedInOrder(g, run, "DROP TABLE IF EXISTS `application_credential`, `odd``name`")
			},
		},
	}
	// Each connect error RETRY_CODES lists makes the pre-step wait, not fail.
	for _, code := range []int{1047, 2002, 2003, 2006, 2013} {
		cases = append(cases, resetCase{
			name:  fmt.Sprintf("connect error %d is retried", code),
			state: dbSyncState{ConnectErrors: [][]any{{code, "connection error"}}},
			check: func(g Gomega, run dbSyncRun) {
				ranDBSync(g, run)
				g.Expect(run.linesWithPrefix("sleep ", "connect ")).To(HaveExactElements(
					"sleep 5", HavePrefix("connect ")))
			},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.heads == noKeystonePackage {
				if exec.CommandContext(t.Context(), "python3", "-E", "-c", hostKeystoneProbe).Run() == nil {
					t.Skip("the host's python3 finds a keystone package of its own")
				}
			}
			if tc.unsetURL {
				if _, err := os.Stat("/etc/keystone/keystone.conf.d"); err == nil {
					t.Skip("/etc/keystone/keystone.conf.d exists here and would supply a URL")
				}
			}
			g := NewGomegaWithT(t)

			run := runDBSyncScript(t, dbSyncScript, tc.state, tc.heads, tc.unsetURL)

			tc.check(g, run)
			g.Expect(run.stdout).NotTo(ContainSubstring("stub-pw"), "stdout must not carry the password")
			g.Expect(run.stderr).NotTo(ContainSubstring("stub-pw"), "stderr must not carry the password")
		})
	}
}

// TestDBSyncScript_ConnectKwargs feeds the DSN database.BuildDSN renders for
// each database TLS mode to the db-sync and the bootstrap container programs.
// The password carries every character url.UserPassword percent-encodes and
// the sub-delims it keeps. Both scripts must hand pymysql the decoded
// credentials and the same ssl dict, so the two copies of the URL resolution
// cannot drift; db_sync_reset.py adds its two session settings.
func TestDBSyncScript_ConnectKwargs(t *testing.T) {
	requireDBSyncTools(t)

	const password = `p@s:s/w?r#d%! *()'$&+,;=`
	paths := database.TLSFilePaths(dbTLSMountPath)
	sslDict := func(verifyMode float64, checkHostname bool) map[string]any {
		return map[string]any{
			"ca": paths.CA, "cert": paths.Cert, "key": paths.Key,
			"verify_mode": verifyMode, "check_hostname": checkHostname,
		}
	}
	bootstrapEnv := []string{
		"BOOTSTRAP_PASSWORD=stub-admin-pw", "BOOTSTRAP_REGION_ID=RegionOne",
		"BOOTSTRAP_ADMIN_URL=http://keystone.invalid", "BOOTSTRAP_INTERNAL_URL=http://keystone.invalid",
		"BOOTSTRAP_PUBLIC_URL=http://keystone.invalid",
	}

	cases := []struct {
		mode string
		ssl  map[string]any
	}{
		{mode: "disabled"},
		// ssl.CERT_NONE is 0 and ssl.CERT_REQUIRED is 2.
		{mode: "require", ssl: sslDict(0, false)},
		{mode: "verify-ca", ssl: sslDict(2, false)},
		{mode: "verify-full", ssl: sslDict(2, true)},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			query := url.Values{}
			query.Set("charset", "utf8")
			g.Expect(database.AppendTLSParams(&commonv1.DatabaseTLSSpec{Mode: tc.mode}, paths, query)).To(Succeed())
			dsn, _ := database.BuildDSN("keystone", password, "db.invalid:3306", "keystone", query)
			want := map[string]any{
				"host": "db.invalid", "port": float64(3306), "user": "keystone", "password": password,
				"database": "keystone", "charset": "utf8",
			}
			if tc.ssl != nil {
				want["ssl"] = tc.ssl
			}

			seed := runDBSyncScript(t, bootstrapScript, dbSyncState{}, bothHeads, false,
				append([]string{"OS_DATABASE__CONNECTION=" + dsn}, bootstrapEnv...)...)
			g.Expect(seed.code).To(Equal(0), seed.stderr)
			g.Expect(seed.connectKwargs(g)).To(Equal(want), "bootstrap_db_seed.py")

			sync := runDBSyncScript(t, dbSyncScript, dbSyncState{}, bothHeads, false, "OS_DATABASE__CONNECTION="+dsn)
			g.Expect(sync.code).To(Equal(0), sync.stderr)
			want["autocommit"] = true
			want["init_command"] = "SET SESSION lock_wait_timeout = 30"
			g.Expect(sync.connectKwargs(g)).To(Equal(want), "db_sync_reset.py")
		})
	}
}
