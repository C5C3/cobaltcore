// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// shPath is the interpreter the migration Jobs run their scripts with. The tests
// run the same bytes through it, so a machine without it skips rather than
// exercising a different shell than production does.
const shPath = "/bin/sh"

// The file names a script run works with, all relative to the run directory so
// the scripts under test stay byte-identical to the shipped ones except for the
// termination-log destination. The production destination is
// /dev/termination-log, which belongs to the kubelet and no test process may
// write.
const (
	scriptFileName     = "script.sh"
	terminationLogName = "termination-log"
	argvLogName        = "argv.log"
	cellsTableName     = "cells-table"
)

// dbSyncStubPrelude lays down the nova-manage stub the script calls instead of
// the real binary and puts it first on PATH. It logs its argument list, fails
// the call whose ordinal is $FAIL_CALL (none when unset), and answers a
// "cell_v2 list_cells" with the table named by $CELLS_TABLE. The stub is written
// by the shell rather than by the test process because a PATH entry has to be
// executable, and nova-manage cannot be shadowed by a shell function: POSIX
// rejects a hyphen in a function name, and /bin/sh is the interpreter the Job
// uses.
const dbSyncStubPrelude = `cat > nova-manage <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$ARGV_LOG"
if [ "$(( $(wc -l < "$ARGV_LOG") ))" = "${FAIL_CALL:-0}" ]; then
  exit 1
fi
case "$*" in
*list_cells*) cat "$CELLS_TABLE" ;;
esac
STUB
chmod 0700 nova-manage
PATH="$PWD:$PATH"
export PATH
`

// listCellsTable is what "nova-manage cell_v2 list_cells" prints once both cells
// are mapped: three header rows, one row per cell with the name in column 2 and
// the UUID in column 3, and a bottom rule that carries no pipe at all.
const listCellsTable = `+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
|  Name |                 UUID                 |           Transport URL            |               Database Connection               | Disabled |
+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
| cell0 | 00000000-0000-0000-0000-000000000000 |               none:/               | mysql+pymysql://nova:****@nova-db:3306/nova_cell0 |  False   |
| cell1 | 2683878f-66d5-4512-bac3-9d70555bdd23 | rabbit://nova:****@rabbitmq:5672/  |    mysql+pymysql://nova:****@nova-db:3306/nova    |  False   |
+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
`

// listCellsTableWithoutCell1 is the same table one pass earlier, when map_cell0
// has run and the real cell has not been created yet.
const listCellsTableWithoutCell1 = `+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
|  Name |                 UUID                 |           Transport URL            |               Database Connection               | Disabled |
+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
| cell0 | 00000000-0000-0000-0000-000000000000 |               none:/               | mysql+pymysql://nova:****@nova-db:3306/nova_cell0 |  False   |
+-------+--------------------------------------+------------------------------------+-------------------------------------------------+----------+
`

// listCellsTableWithCell1InTheURLs is the table one pass before the real cell is
// created, for a deployment whose names carry "cell1" as a word: the MariaDB user
// is nova-cell1, the host cell1-db and the broker vhost region-cell1. cell0's row
// shows its expanded connection with only the password masked, so every one of
// them is in the table although no cell is named cell1.
const listCellsTableWithCell1InTheURLs = `+-------+--------------------------------------+------------------------------------------+------------------------------------------------------------+----------+
|  Name |                 UUID                 |              Transport URL               |                    Database Connection                     | Disabled |
+-------+--------------------------------------+------------------------------------------+------------------------------------------------------------+----------+
| cell0 | 00000000-0000-0000-0000-000000000000 |                  none:/                  | mysql+pymysql://nova-cell1:****@cell1-db:3306/nova_cell0?ssl_ca=/etc/nova-db-tls/cell/ca.crt |  False   |
+-------+--------------------------------------+------------------------------------------+------------------------------------------------------------+----------+
`

// listCellsTableBrownfield is the table of a deployment another tool set up: the
// real cell is mapped onto the cell schema under a name of its own, with TLS
// parameters on the connection.
const listCellsTableBrownfield = `+---------+--------------------------------------+------------------------------------+-------------------------------------------------------------------+----------+
|   Name  |                 UUID                 |           Transport URL            |                        Database Connection                        | Disabled |
+---------+--------------------------------------+------------------------------------+-------------------------------------------------------------------+----------+
|  cell0  | 00000000-0000-0000-0000-000000000000 |               none:/               |         mysql+pymysql://nova:****@nova-db:3306/nova_cell0         |  False   |
| default | 5d3b8a61-0c55-4e3a-9f38-2a6f4a4a2d17 | rabbit://nova:****@rabbitmq:5672/  | mysql+pymysql://nova:****@nova-db:3306/nova?ssl_ca=/etc/ca.crt |  False   |
+---------+--------------------------------------+------------------------------------+-------------------------------------------------------------------+----------+
`

// scriptDir returns a directory to run a script in, skipping the test when the
// interpreter or the awk the report stage pipes through is unavailable.
func scriptDir(t *testing.T) string {
	t.Helper()

	for _, binary := range []string{shPath, "awk"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is unavailable; the shipped scripts cannot be exercised here: %v", binary, err)
		}
	}
	return t.TempDir()
}

// writeScriptFile writes one file of a script run into dir. The mode is the
// read-write-for-the-owner the repository's security profile allows; the stubs
// that have to be executable are created by the prelude inside the run itself.
func writeScriptFile(t *testing.T, dir, name, content string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)).To(Succeed())
}

// readScriptFile returns the contents of a file a script run left behind.
func readScriptFile(t *testing.T, dir, name string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	content, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
	g.Expect(err).NotTo(HaveOccurred(), "the run was expected to leave %s behind", name)
	return string(content)
}

// runScript runs script with /bin/sh inside dir and returns its exit code. The
// script travels through a file rather than through "sh -c" so the command line
// stays free of interpolated input, and env carries the per-case parameters the
// stubs read.
func runScript(t *testing.T, dir, script string, env ...string) int {
	t.Helper()
	g := NewGomegaWithT(t)
	writeScriptFile(t, dir, scriptFileName, script)

	cmd := exec.CommandContext(t.Context(), shPath, "-eu", scriptFileName)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
			"the script must fail by exiting, not by failing to start: %v (%s)", err, stderr.String())
		return exitErr.ExitCode()
	}
	return 0
}

// dbSyncRun assembles the shipped db-sync script with its termination-log
// destination swapped for a path the test may write, behind the stub prelude.
func dbSyncRun() string {
	return dbSyncStubPrelude + dbSyncScriptHead(validNova()) + terminationLogName
}

// dbSyncEnv is the environment the stub reads: where to log its argument lists
// and which list_cells table to answer with.
func dbSyncEnv(dir string) []string {
	return []string{
		"ARGV_LOG=" + filepath.Join(dir, argvLogName),
		"CELLS_TABLE=" + filepath.Join(dir, cellsTableName),
	}
}

// novaManageVerbs reduces the stub's argument log to the nova-manage verb of
// each call, which is what the ordering assertions read.
func novaManageVerbs(t *testing.T, dir string) []string {
	t.Helper()
	var verbs []string
	for _, line := range strings.Split(strings.TrimSpace(readScriptFile(t, dir, argvLogName)), "\n") {
		// Every call is "--config-dir <dir> <verb...>"; the two leading words are
		// the same on all of them.
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		verbs = append(verbs, strings.Join(fields[2:4], " "))
	}
	return verbs
}

// TestDBSyncScript_PinsTheCommandLine pins the script the db-sync Job runs. It
// is a string the operator ships rather than code it compiles, so this pin is
// what a review of a change to it reads.
func TestDBSyncScript_PinsTheCommandLine(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(dbSyncScript(validNova())).To(Equal(
		`nova-manage --config-dir /etc/nova/nova.conf.d api_db sync && ` +
			`nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 map_cell0 ` +
			`--database_connection '{scheme}://{username}:{password}@{hostname}:{port}/nova_cell0?{query}' && ` +
			`cells="$(nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 list_cells)" && ` +
			`(printf '%s\n' "$cells" | awk -F'|' -v name='cell1' -v schema='/nova' ` +
			`'NR>3 {n=$2; d=$5; gsub(/ /,"",n); gsub(/ /,"",d); sub(/[?].*/,"",d); ` +
			`if (n == name || (length(d) > length(schema) && substr(d, length(d)-length(schema)+1) == schema)) f=1} ` +
			`END {exit !f}' || ` +
			`nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 create_cell --name cell1 ` +
			`--transport-url '{scheme}://{username}:{password}@{hostname}:{port}/{path}?{query}' ` +
			`--database_connection '{scheme}://{username}:{password}@{hostname}:{port}/nova?{query}') && ` +
			`nova-manage --config-dir /etc/nova/nova.conf.d db sync && ` +
			`cells="$(nova-manage --config-dir /etc/nova/nova.conf.d cell_v2 list_cells)" && ` +
			`printf '%s\n' "$cells" | ` +
			`awk -F'|' 'NR>3 && $2 !~ /^ *-/ {gsub(/ /,"",$2); gsub(/ /,"",$3); if ($2 != "") print $2"="$3}' ` +
			`> /dev/termination-log`))
}

// TestDBSyncScript_SkipsCreateCellWhenListed covers the repeat run, which is
// every pass after the first: a second create_cell with the same templates does
// not fail and does not update the existing row, it maps a second cell under the
// same name. The guard is what keeps the Job idempotent.
func TestDBSyncScript_SkipsCreateCellWhenListed(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	writeScriptFile(t, dir, cellsTableName, listCellsTable)

	code := runScript(t, dir, dbSyncRun(), dbSyncEnv(dir)...)

	g.Expect(code).To(Equal(0))
	g.Expect(novaManageVerbs(t, dir)).To(Equal([]string{
		"api_db sync",
		"cell_v2 map_cell0",
		"cell_v2 list_cells",
		"db sync",
		"cell_v2 list_cells",
	}), "the nova_api schema is migrated and cell0 mapped before the cell schema is touched")
}

// TestDBSyncScript_CreatesCellWhenAbsent covers the first run: cell0 is mapped,
// the real cell is missing from the listing, and create_cell writes it with both
// templates before the cell schema is migrated.
func TestDBSyncScript_CreatesCellWhenAbsent(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	writeScriptFile(t, dir, cellsTableName, listCellsTableWithoutCell1)

	code := runScript(t, dir, dbSyncRun(), dbSyncEnv(dir)...)

	g.Expect(code).To(Equal(0))
	g.Expect(novaManageVerbs(t, dir)).To(Equal([]string{
		"api_db sync",
		"cell_v2 map_cell0",
		"cell_v2 list_cells",
		"cell_v2 create_cell",
		"db sync",
		"cell_v2 list_cells",
	}))

	argv := readScriptFile(t, dir, argvLogName)
	g.Expect(argv).To(ContainSubstring("create_cell --name cell1 " +
		"--transport-url {scheme}://{username}:{password}@{hostname}:{port}/{path}?{query} " +
		"--database_connection {scheme}://{username}:{password}@{hostname}:{port}/nova?{query}"))
	g.Expect(strings.Count(argv, "create_cell")).To(Equal(1), "the cell is created once, not once per pass")
}

// TestDBSyncScript_ReportsCellsFromTheTable covers the last stage: the awk over
// the final listing is what puts the UUIDs nova generated into the termination
// log, which is the only place the operator reads them back from.
func TestDBSyncScript_ReportsCellsFromTheTable(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	writeScriptFile(t, dir, cellsTableName, listCellsTable)

	code := runScript(t, dir, dbSyncRun(), dbSyncEnv(dir)...)

	g.Expect(code).To(Equal(0))
	g.Expect(readScriptFile(t, dir, terminationLogName)).To(Equal(
		"cell0=00000000-0000-0000-0000-000000000000\n" +
			"cell1=2683878f-66d5-4512-bac3-9d70555bdd23\n"))

	// The report is what reportCells parses, so the two halves are pinned against
	// each other rather than only against their own fixtures.
	g.Expect(parseCellsReport(readScriptFile(t, dir, terminationLogName))).To(HaveLen(2))
}

// TestDBSyncScript_GuardMatchesTheCellNotAWordInTheURLs covers the first run of a
// deployment whose user, host or vhost carries "cell1" as a word. cell0's row
// shows its expanded connection, so a guard grepping the table would find the
// word, skip create_cell for good and leave every boot in cell0. The guard reads
// the name column and the schema instead.
func TestDBSyncScript_GuardMatchesTheCellNotAWordInTheURLs(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	writeScriptFile(t, dir, cellsTableName, listCellsTableWithCell1InTheURLs)

	code := runScript(t, dir, dbSyncRun(), dbSyncEnv(dir)...)

	g.Expect(code).To(Equal(0))
	g.Expect(novaManageVerbs(t, dir)).To(ContainElement("cell_v2 create_cell"),
		"no cell is mapped onto the cell schema yet, whatever words the URLs carry")
}

// TestDBSyncScript_SkipsCreateCellForABrownfieldCellOnTheSchema covers a cell
// another tool mapped onto the cell schema under a name of its own. create_cell
// would not recognize it (nova compares the stored URLs against the templates) and
// would map the same schema a second time, listing every instance twice.
func TestDBSyncScript_SkipsCreateCellForABrownfieldCellOnTheSchema(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	writeScriptFile(t, dir, cellsTableName, listCellsTableBrownfield)

	code := runScript(t, dir, dbSyncRun(), dbSyncEnv(dir)...)

	g.Expect(code).To(Equal(0))
	g.Expect(novaManageVerbs(t, dir)).NotTo(ContainElement("cell_v2 create_cell"))
}

// TestDBSyncScript_FailuresStopTheScript covers the three failures the script
// must not step over. A failed nova_api migration must not go on to map cells
// into it; a list_cells that fails in the guard must not read as an empty table,
// which would create the duplicate cell the guard exists to prevent; and a final
// list_cells that fails must fail the Job rather than complete it with an empty
// report.
func TestDBSyncScript_FailuresStopTheScript(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failCall  string
		wantVerbs []string
	}{
		{
			name:      "api_db sync",
			failCall:  "1",
			wantVerbs: []string{"api_db sync"},
		},
		{
			name:      "the guard's list_cells",
			failCall:  "3",
			wantVerbs: []string{"api_db sync", "cell_v2 map_cell0", "cell_v2 list_cells"},
		},
		{
			name:     "the report's list_cells",
			failCall: "5",
			wantVerbs: []string{
				"api_db sync", "cell_v2 map_cell0", "cell_v2 list_cells", "db sync", "cell_v2 list_cells",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			dir := scriptDir(t)
			writeScriptFile(t, dir, cellsTableName, listCellsTable)

			code := runScript(t, dir, dbSyncRun(), append(dbSyncEnv(dir), "FAIL_CALL="+tc.failCall)...)

			g.Expect(code).NotTo(Equal(0))
			g.Expect(novaManageVerbs(t, dir)).To(Equal(tc.wantVerbs))
			_, err := os.Stat(filepath.Join(dir, terminationLogName))
			g.Expect(os.IsNotExist(err)).To(BeTrue(), "no report is written for a run that failed")
		})
	}
}
