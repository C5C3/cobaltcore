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
	"time"

	. "github.com/onsi/gomega"
)

// bashPath is the interpreter the backup script declares in its shebang. The
// tests run the script through it directly, so a machine without it skips
// rather than exercising a different shell than production does.
const bashPath = "/bin/bash"

// The two runs of the backup script the tests exercise. Each is the script
// itself, unmodified, behind a shell function that stands in for ovsdb-client: a
// function shadows the PATH lookup, so the script reaches the stub with its
// arguments and its redirect intact and no executable has to be laid down for
// it.
//
// They are constants because the whole command line is then a constant too,
// which is what keeps a test that shells out from reading as one that hands
// untrusted input to a subprocess.
const (
	backupScriptFailingRun    = "ovsdb-client() { return 1; }\n" + backupScript
	backupScriptSucceedingRun = "ovsdb-client() { echo 'ovsdb backup payload'; }\n" + backupScript
	// The stub a full volume produces: ovsdb-client exits zero having written
	// nothing, which is the one failure the ".tmp" rename cannot catch.
	backupScriptEmptyRun = "ovsdb-client() { return 0; }\n" + backupScript
)

// The addresses and the retention the backup CronJob hands the script through
// the environment. The stubs ignore the addresses; they are here so the error
// message a failing run prints can be matched against the input that produced
// it.
const (
	scriptNBAddr        = "ssl:10.96.0.11:6641"
	scriptSBAddr        = "ssl:10.96.0.21:6642"
	scriptRetentionDays = "14"
)

// runBackupScript runs cmd against backupDir and returns its exit code and
// stderr. A non-zero exit is an expected outcome here, not a test failure.
//
// BACKUP_DIR is the one knob the script reads for its destination, and pointing
// it at a temp directory is what makes the script testable without a container
// and without the root privileges a bind mount over /backup would take.
func runBackupScript(t *testing.T, cmd *exec.Cmd, backupDir string) (int, string) {
	t.Helper()
	g := NewGomegaWithT(t)

	cmd.Env = append(os.Environ(),
		"BACKUP_DIR="+backupDir,
		"NB_ADDR="+scriptNBAddr,
		"SB_ADDR="+scriptSBAddr,
		"RETENTION_DAYS="+scriptRetentionDays,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var exitErr *exec.ExitError
	g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
		"the script must fail by exiting, not by failing to start: %v", err)
	return exitErr.ExitCode(), stderr.String()
}

// backupScriptDir returns a temp directory to run the script against, skipping
// the test when the interpreter the script is written for is unavailable.
func backupScriptDir(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath(bashPath); err != nil {
		t.Skipf("%s is unavailable; the backup script cannot be exercised here: %v", bashPath, err)
	}
	return t.TempDir()
}

// backupDirEntries returns the names of the files in dir.
func backupDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	g := NewGomegaWithT(t)

	entries, err := os.ReadDir(dir)
	g.Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A run whose ovsdb-client fails must leave nothing behind. Without the ".tmp"
// indirection the shell redirect alone would create a file named like a
// snapshot, and a restore reaching for the newest one would find an empty file
// where the database it is trying to recover should be.
func TestBackupScript_FailedOvsdbClientLeavesNoArtefact(t *testing.T) {
	g := NewGomegaWithT(t)
	backupDir := backupScriptDir(t)

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", backupScriptFailingRun)
	code, stderr := runBackupScript(t, cmd, backupDir)

	g.Expect(code).To(Equal(1), "a failed snapshot must fail the run, so the Job reports it")
	g.Expect(stderr).To(ContainSubstring("backup of OVN_Northbound at " + scriptNBAddr + " failed"))
	g.Expect(backupDirEntries(t, backupDir)).To(BeEmpty(),
		"neither the .tmp file nor a renamed snapshot may survive a failed run")
}

// A successful run renames each snapshot into place, prunes what fell out of the
// retention window, and sweeps zero-byte files. The sweep is the backstop for
// the case the rename cannot cover: a full volume lets ovsdb-client exit zero
// with nothing written, and a zero-byte file restores no database.
func TestBackupScript_SuccessPrunesZeroByteBackups(t *testing.T) {
	g := NewGomegaWithT(t)
	backupDir := backupScriptDir(t)

	// A zero-byte leftover of an earlier run, an intact snapshot older than the
	// retention window, and an intact recent one. Only the first two may go.
	empty := filepath.Join(backupDir, "nb-old.backup")
	ancient := filepath.Join(backupDir, "nb-ancient.backup")
	recent := filepath.Join(backupDir, "sb-recent.backup")
	g.Expect(os.WriteFile(empty, nil, 0o600)).To(Succeed())
	g.Expect(os.WriteFile(ancient, []byte("stale but intact"), 0o600)).To(Succeed())
	g.Expect(os.WriteFile(recent, []byte("intact"), 0o600)).To(Succeed())
	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour)
	g.Expect(os.Chtimes(ancient, thirtyDaysAgo, thirtyDaysAgo)).To(Succeed())

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", backupScriptSucceedingRun)
	code, stderr := runBackupScript(t, cmd, backupDir)

	g.Expect(code).To(Equal(0), "stderr was: "+stderr)

	names := backupDirEntries(t, backupDir)
	g.Expect(names).NotTo(ContainElement("nb-old.backup"),
		"a zero-byte file restores no database and must not look like a snapshot")
	g.Expect(names).NotTo(ContainElement("nb-ancient.backup"),
		"a snapshot older than RETENTION_DAYS must be pruned")
	g.Expect(names).To(ContainElement("sb-recent.backup"),
		"a snapshot inside the window must survive")

	var fresh []string
	for _, name := range names {
		g.Expect(name).NotTo(HaveSuffix(".tmp"), "every snapshot must be renamed into place")
		if name != "sb-recent.backup" {
			fresh = append(fresh, name)
		}
		info, err := os.Stat(filepath.Join(backupDir, name))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(info.Size()).To(BeNumerically(">", 0), name)
	}
	g.Expect(fresh).To(HaveLen(2), "one snapshot per database: %v", names)
	g.Expect(strings.Join(fresh, " ")).To(And(ContainSubstring("nb-"), ContainSubstring("sb-")))
}

// A run that produced no bytes must fail. ovsdb-client exits zero on a volume
// that filled up, so without this check the Job completes, BackupReady stays
// True, and the retention prune eats one more day of history on every firing
// while the only path back from a corrupted logical model is being erased.
func TestBackupScript_EmptySnapshotFailsTheRun(t *testing.T) {
	g := NewGomegaWithT(t)
	backupDir := backupScriptDir(t)

	// A snapshot from an earlier, healthy run. It must still be there
	// afterwards: the run fails before it reaches the retention prune.
	previous := filepath.Join(backupDir, "nb-previous.backup")
	g.Expect(os.WriteFile(previous, []byte("intact"), 0o600)).To(Succeed())

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", backupScriptEmptyRun)
	code, stderr := runBackupScript(t, cmd, backupDir)

	g.Expect(code).To(Equal(1), "a run that wrote nothing must fail, so the Job reports it")
	g.Expect(stderr).To(ContainSubstring(
		"backup of OVN_Northbound at " + scriptNBAddr + " produced an empty snapshot"))
	g.Expect(backupDirEntries(t, backupDir)).To(ConsistOf("nb-previous.backup"),
		"the empty snapshot must be removed and the earlier history left alone")
}

// A ".tmp" file left behind by a run the deadline killed matches neither
// retention prune, so it would stay on the volume for good and accumulate one
// full database dump per deadlined firing until the volume is full.
func TestBackupScript_SuccessSweepsStaleTempFiles(t *testing.T) {
	g := NewGomegaWithT(t)
	backupDir := backupScriptDir(t)

	stale := filepath.Join(backupDir, "nb-20260101T020000Z.backup.tmp")
	g.Expect(os.WriteFile(stale, []byte("half-written dump"), 0o600)).To(Succeed())
	twoDaysAgo := time.Now().Add(-48 * time.Hour)
	g.Expect(os.Chtimes(stale, twoDaysAgo, twoDaysAgo)).To(Succeed())

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", backupScriptSucceedingRun)
	code, stderr := runBackupScript(t, cmd, backupDir)

	g.Expect(code).To(Equal(0), "stderr was: "+stderr)
	g.Expect(backupDirEntries(t, backupDir)).NotTo(
		ContainElement("nb-20260101T020000Z.backup.tmp"),
		"a leaked half-written dump must not outlive the retention window")
}
