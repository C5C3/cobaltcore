// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// dbArchiveStubPrelude lays down the nova-manage and sleep stubs the script calls
// instead of the real binaries and puts them first on PATH. nova-manage logs its
// argument list and exits with the n-th field of $STUB_RCS, n being how often it
// has been called, and 0 once the fields run out: the script reads each code as
// a severity and loops on it. sleep logs its argument instead of sleeping, so a
// run of several batches takes no time. The stubs are written by the shell
// rather than by the test process because a PATH entry has to be executable, and
// nova-manage cannot be shadowed by a shell function: POSIX rejects a hyphen in
// a function name, and /bin/sh is the interpreter the CronJob uses.
const dbArchiveStubPrelude = `cat > nova-manage <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$ARGV_LOG"
exit "$(printf '%s\n' "${STUB_RCS:-0}" | awk -v n="$(( $(wc -l < "$ARGV_LOG") ))" '{print ($n == "" ? 0 : $n)}')"
STUB
cat > sleep <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$SLEEP_LOG"
STUB
chmod 0700 nova-manage sleep
PATH="$PWD:$PATH"
export PATH
`

// sleepLogName is where the sleep stub records the pauses the loop took.
const sleepLogName = "sleep.log"

// dbArchiveRun assembles the shipped archive script behind the stub prelude. The
// script itself is byte-identical to the one the CronJob runs.
func dbArchiveRun() string {
	return dbArchiveStubPrelude + dbArchiveScript
}

// dbArchiveEnv is the environment the CronJob container carries, with the
// stubs' logs and exit codes on top. Extra entries (RETENTION_DAYS, a different
// STUB_RCS) are appended by the caller and win, because the later assignment is
// the one the process ends up with.
func dbArchiveEnv(dir string, extra ...string) []string {
	return append([]string{
		"ARGV_LOG=" + filepath.Join(dir, argvLogName),
		"SLEEP_LOG=" + filepath.Join(dir, sleepLogName),
		"MAX_ROWS=1000",
		"SLEEP=1",
	}, extra...)
}

// skipWithoutGNUDate skips a case that needs "date -d", the GNU coreutils
// arithmetic the python-base Debian image ships and the BSD date on macOS does
// not have.
func skipWithoutGNUDate(t *testing.T) {
	t.Helper()
	if err := exec.CommandContext(t.Context(), "date", "-u", "-d", "-1 days", "+%Y-%m-%d").Run(); err != nil {
		t.Skipf("date -d is unavailable here, so the retention window cannot be exercised: %v", err)
	}
}

// TestDBArchiveScript_LoopsWhileRowsWereArchived covers the batch loop: nova-manage
// answers 1 when a batch archived rows, so the run starts another batch after a
// pause, and 0 once a batch finds nothing left, which ends the run successfully.
// A run whose only batch archives nothing takes no pause at all.
func TestDBArchiveScript_LoopsWhileRowsWereArchived(t *testing.T) {
	for _, tc := range []struct {
		rcs        string
		wantCalls  int
		wantSleeps int
	}{
		{rcs: "0", wantCalls: 1, wantSleeps: 0},
		{rcs: "1 1 0", wantCalls: 3, wantSleeps: 2},
	} {
		t.Run("nova-manage exits "+tc.rcs, func(t *testing.T) {
			g := NewGomegaWithT(t)
			dir := scriptDir(t)

			code := runScript(t, dir, dbArchiveRun(), dbArchiveEnv(dir, "STUB_RCS="+tc.rcs)...)

			g.Expect(code).To(Equal(0), "exits 0 and 1 are outcomes of a healthy batch")
			g.Expect(callCount(t, dir, argvLogName)).To(Equal(tc.wantCalls))
			g.Expect(callCount(t, dir, sleepLogName)).To(Equal(tc.wantSleeps))
			if tc.wantSleeps > 0 {
				g.Expect(readScriptFile(t, dir, sleepLogName)).To(HavePrefix("1\n"),
					"the pause between batches is spec.dbArchive.sleep")
			}
		})
	}
}

// TestDBArchiveScript_FailsOnRC2 covers the other half of the severity scale: 2
// is an invalid row cap, 3 is a missing API database connection and 4 is an
// unparsable --before. All three are misconfigurations that no later batch
// repairs, so they stop the loop and must reach DBArchiveReady instead of being
// swallowed, also after batches that did archive rows.
func TestDBArchiveScript_FailsOnRC2(t *testing.T) {
	for _, rcs := range []string{"2", "3", "4", "1 2"} {
		t.Run("nova-manage exits "+rcs, func(t *testing.T) {
			g := NewGomegaWithT(t)
			dir := scriptDir(t)

			code := runScript(t, dir, dbArchiveRun(), dbArchiveEnv(dir, "STUB_RCS="+rcs)...)

			g.Expect(code).NotTo(Equal(0), "exit %s must fail the run", rcs)
			g.Expect(callCount(t, dir, argvLogName)).To(Equal(len(strings.Fields(rcs))),
				"a failed batch stops the loop at once")
		})
	}
}

// TestDBArchiveScript_StopsOnceTheBudgetIsSpent covers the backlog one run
// cannot move. Batches keep archiving rows, and the run stops starting new ones
// once the budget is spent, exiting 0: the rows it moved stay moved, and the next
// run continues where it stopped instead of every run failing at the active
// deadline. A date stub jumps past the budget after the first batch.
func TestDBArchiveScript_StopsOnceTheBudgetIsSpent(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := scriptDir(t)

	dateStub := fmt.Sprintf(`cat > date <<'STUB'
#!/bin/sh
count=$(( $(cat "$DATE_COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$count" > "$DATE_COUNT"
if [ "$count" -le 2 ]; then echo 1000; else echo %d; fi
STUB
chmod 0700 date
`, 1000+dbArchiveRunBudgetSeconds)

	code := runScript(t, dir, dbArchiveStubPrelude+dateStub+dbArchiveScript,
		dbArchiveEnv(dir, "STUB_RCS=1 1 1 1", "DATE_COUNT="+filepath.Join(dir, "date-count"))...)

	g.Expect(code).To(Equal(0), "a spent budget ends a healthy run, it does not fail it")
	g.Expect(callCount(t, dir, argvLogName)).To(Equal(1), "no batch starts after the budget is spent")
	g.Expect(dbArchiveRunBudgetSeconds).To(BeNumerically("<", dbArchiveActiveDeadlineSeconds),
		"the budget has to end the run before the active deadline fails it")
}

// TestDBArchiveScript_PassesBeforeOnlyWithRetention pins the argument list the
// script assembles. The retention window is the one argument built at runtime,
// and an empty "$before" must add no argument at all: a stray empty string would
// be read by nova-manage as a positional it does not take. --task-log only ever
// travels with a date, and neither --purge nor --until-complete is ever passed.
func TestDBArchiveScript_PassesBeforeOnlyWithRetention(t *testing.T) {
	t.Run("without a retention window no --before is passed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		dir := scriptDir(t)

		code := runScript(t, dir, dbArchiveRun(), dbArchiveEnv(dir)...)
		g.Expect(code).To(Equal(0))

		argv := strings.TrimSpace(readScriptFile(t, dir, argvLogName))
		g.Expect(argv).To(Equal(
			"--config-dir /etc/nova/nova.conf.d db archive_deleted_rows --all-cells --max_rows 1000"),
			"an unset window must leave the argument list here")
	})

	t.Run("a retention window bounds the run by today minus the window", func(t *testing.T) {
		g := NewGomegaWithT(t)
		skipWithoutGNUDate(t)
		dir := scriptDir(t)

		const day = "2006-01-02"
		// The run straddles a UTC midnight at most once a day; both dates around it
		// are correct answers, so the case names them rather than flaking.
		beforeRun := time.Now().UTC().AddDate(0, 0, -7).Format(day)
		code := runScript(t, dir, dbArchiveRun(), dbArchiveEnv(dir, "RETENTION_DAYS=7")...)
		afterRun := time.Now().UTC().AddDate(0, 0, -7).Format(day)
		g.Expect(code).To(Equal(0))

		argv := strings.TrimSpace(readScriptFile(t, dir, argvLogName))
		g.Expect(argv).To(Or(
			HaveSuffix("--before "+beforeRun+" --task-log"),
			HaveSuffix("--before "+afterRun+" --task-log"),
		), "the window is today minus RETENTION_DAYS, in UTC, and gates the task_log archive")
		g.Expect(argv).To(ContainSubstring("--all-cells --max_rows 1000 --before"),
			"the date is appended to the bound the CronJob sets as environment")
	})

	t.Run("the destructive flags are never passed", func(t *testing.T) {
		g := NewGomegaWithT(t)
		for _, flag := range []string{"--purge", "--until-complete", "--sleep"} {
			g.Expect(dbArchiveScript).NotTo(ContainSubstring(flag))
		}
	})
}
