// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// rcFileName holds the exit codes the nova-manage stub returns, one per call,
// as the fields of a single line. The contract phase loops until nova-manage
// reports it is done, so a fixed code could not describe that walk.
const rcFileName = "exit-codes"

// upgradeStubPrelude lays down the nova-status and nova-manage stubs the two
// upgrade scripts call and puts them first on PATH. nova-status exits with
// $STATUS_RC, the severity the expand phase has to classify; nova-manage exits
// with the n-th field of $RC_FILE, n being how often it has been called, which
// is what drives the contract loop. Both are written by the shell rather than by
// the test process because a PATH entry has to be executable, and neither can be
// shadowed by a shell function: POSIX rejects a hyphen in a function name, and
// /bin/sh is the interpreter the Jobs use.
const upgradeStubPrelude = `cat > nova-status <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$STATUS_LOG"
exit "$STATUS_RC"
STUB
cat > nova-manage <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$ARGV_LOG"
printf '%s\n' "${OS_DATABASE__CONNECTION:-}" >> "$CONNECTION_LOG"
exit "$(awk -v n="$(wc -l < "$ARGV_LOG")" '{print $n}' "$RC_FILE")"
STUB
chmod 0700 nova-status nova-manage
PATH="$PWD:$PATH"
export PATH
`

// statusLogName is where the nova-status stub records that it ran, which is what
// tells a failed expand phase apart from one that never reached the check.
const statusLogName = "status.log"

// connectionLogName is where the nova-manage stub records the cell database
// connection each call ran with, which is what tells the cell pass of the
// contract phase apart from the cell0 pass.
const connectionLogName = "connection.log"

// testCellConnection is the cell database connection the migration Jobs run
// with, TLS parameters included, as the derived connection Secret spells it.
const testCellConnection = "mysql+pymysql://nova:pw@nova-db:3306/nova?ssl_ca=/etc/nova-db-tls/cell/ca.crt"

// upgradeEnv writes the nova-manage exit-code sequence into dir and returns the
// environment the two stubs read.
func upgradeEnv(t *testing.T, dir string, statusRC int, exitCodes string) []string {
	t.Helper()
	writeScriptFile(t, dir, rcFileName, exitCodes+"\n")
	return []string{
		"STATUS_LOG=" + filepath.Join(dir, statusLogName),
		"STATUS_RC=" + strconv.Itoa(statusRC),
		"ARGV_LOG=" + filepath.Join(dir, argvLogName),
		"RC_FILE=" + filepath.Join(dir, rcFileName),
		"CONNECTION_LOG=" + filepath.Join(dir, connectionLogName),
		"OS_DATABASE__CONNECTION=" + testCellConnection,
	}
}

// callCount returns how often a stub logged a call, treating a missing log as
// "never called": a stub that was never reached writes no file at all.
func callCount(t *testing.T, dir, logName string) int {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(filepath.Join(dir, logName)))
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// TestUpgradeExpandScript_ToleratesWarnings covers the two severities that must
// not stop the phase. nova-status reports 0 for a clean check and 1 for
// warnings, and a deployment keeps warnings: the checks report on integrations
// it may not run at all. Both leave the real code in the termination log, which
// is the only place reportUpgradeCheck reads it back from.
func TestUpgradeExpandScript_ToleratesWarnings(t *testing.T) {
	for _, statusRC := range []int{0, 1} {
		t.Run(fmt.Sprintf("nova-status exits %d", statusRC), func(t *testing.T) {
			g := NewGomegaWithT(t)
			dir := scriptDir(t)
			run := upgradeStubPrelude + upgradeExpandScriptHead + terminationLogName + upgradeExpandScriptTail

			code := runScript(t, dir, run, upgradeEnv(t, dir, statusRC, "0 0")...)

			g.Expect(code).To(Equal(0), "a check severity is not a reason to stop the upgrade")
			g.Expect(readScriptFile(t, dir, terminationLogName)).To(Equal(
				fmt.Sprintf("nova-status upgrade check exit %d\n", statusRC)))
			g.Expect(novaManageVerbs(t, dir)).To(Equal([]string{"api_db sync", "db sync"}),
				"the expand phase migrates both schemas behind the check")
		})
	}
}

// TestUpgradeExpandScript_FailsOnRC2 covers the severity that does stop it. Exit
// 2 means a check failed, typically because the cell mapping is not there yet,
// and migrating against a half-set-up deployment is worse than a failed Job.
func TestUpgradeExpandScript_FailsOnRC2(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)
	run := upgradeStubPrelude + upgradeExpandScriptHead + terminationLogName + upgradeExpandScriptTail

	code := runScript(t, dir, run, upgradeEnv(t, dir, 2, "0 0")...)

	g.Expect(code).To(Equal(2), "the Job fails with the check's own code")
	g.Expect(readScriptFile(t, dir, terminationLogName)).To(Equal("nova-status upgrade check exit 2\n"))
	g.Expect(callCount(t, dir, argvLogName)).To(BeZero(), "neither schema may be migrated after a failed check")
	g.Expect(callCount(t, dir, statusLogName)).To(Equal(1))
}

// TestUpgradeContractScript_LoopsWhileRC1 covers the backfill walk. nova runs
// one bounded batch of online data migrations per call and reports "there is
// more to do" with exit code 1, so a pass is only complete once a call returns
// 0. The cell schema is walked first, then cell0, which online_data_migrations
// does not reach on its own: its connection is the running one with "_cell0"
// appended to the schema and the query kept.
func TestUpgradeContractScript_LoopsWhileRC1(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)

	code := runScript(t, dir, upgradeStubPrelude+upgradeContractScript, upgradeEnv(t, dir, 0, "1 1 0 1 0")...)

	g.Expect(code).To(Equal(0))
	g.Expect(callCount(t, dir, argvLogName)).To(Equal(5), "each pass runs until a batch reports nothing left")
	g.Expect(novaManageVerbs(t, dir)).To(HaveEach("db online_data_migrations"))

	cell0 := "mysql+pymysql://nova:pw@nova-db:3306/nova_cell0?ssl_ca=/etc/nova-db-tls/cell/ca.crt"
	g.Expect(strings.Split(strings.TrimSpace(readScriptFile(t, dir, connectionLogName)), "\n")).To(Equal([]string{
		testCellConnection, testCellConnection, testCellConnection, cell0, cell0,
	}))
}

// TestUpgradeContractScript_Cell0ConnectionWithoutAQuery covers a connection
// that carries no TLS parameters: "_cell0" still lands on the schema, at the end
// of the URL.
func TestUpgradeContractScript_Cell0ConnectionWithoutAQuery(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)

	code := runScript(t, dir, upgradeStubPrelude+upgradeContractScript,
		append(upgradeEnv(t, dir, 0, "0 0"), "OS_DATABASE__CONNECTION=mysql+pymysql://nova:pw@nova-db:3306/nova")...)

	g.Expect(code).To(Equal(0))
	g.Expect(strings.Split(strings.TrimSpace(readScriptFile(t, dir, connectionLogName)), "\n")).To(Equal([]string{
		"mysql+pymysql://nova:pw@nova-db:3306/nova", "mysql+pymysql://nova:pw@nova-db:3306/nova_cell0",
	}))
}

// TestUpgradeContractScript_ExitsOnRC2 covers the failure the loop must not
// swallow: anything above 1 is a broken migration rather than progress, and
// retrying it forever would wedge the phase instead of failing the Job.
func TestUpgradeContractScript_ExitsOnRC2(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)

	code := runScript(t, dir, upgradeStubPrelude+upgradeContractScript, upgradeEnv(t, dir, 0, "2 0")...)

	g.Expect(code).To(Equal(2))
	g.Expect(callCount(t, dir, argvLogName)).To(Equal(1), "a failed batch stops the loop at once")

	t.Run("in the cell0 pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		dir := scriptDir(t)

		code := runScript(t, dir, upgradeStubPrelude+upgradeContractScript, upgradeEnv(t, dir, 0, "0 2 0")...)

		g.Expect(code).To(Equal(2))
		g.Expect(callCount(t, dir, argvLogName)).To(Equal(2))
	})
}
