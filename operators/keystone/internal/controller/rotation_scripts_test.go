// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// rotationShPath is the interpreter the rotation CronJobs run their scripts
// with. The tests run the same bytes through it, so a machine without it skips
// rather than exercising a different shell than production does.
const rotationShPath = "/bin/sh"

// rotationStubPrelude lays down the keystone-manage and python3 stubs the
// rotation scripts call instead of the real binaries and puts them first on
// PATH.
//
// keystone-manage logs its last argument, the command, with the primary key it
// sees, the highest numeric file name in $KEY_DIR. It exits with the n-th field
// of $KM_RCS, n being how often it has been called, and 0 once the fields run
// out. fernet_rotate and credential_rotate rename key 0 to the next index and,
// only when they exit 0, write a new key 0 holding new-<n>, so a failing rotate
// leaves the half-rotated directory a crash would leave. The stub purges no
// key: the tests count rotations, not keystone's pruning.
//
// python3 stands in for the PATCH block. It discards the script on stdin,
// appends a "--- patch" line and one <name>=<content> line per file of $KEY_DIR
// to $PATCH_LOG, and exits with the m-th field of $PATCH_RCS, m being the number
// of PATCHes so far, its own included.
//
// The stubs are written by the shell rather than by the test process because a
// PATH entry has to be executable, and keystone-manage cannot be shadowed by a
// shell function: POSIX rejects a hyphen in a function name.
const rotationStubPrelude = `cat > keystone-manage <<'STUB'
#!/bin/sh
for verb in "$@"; do :; done
highest=$(ls "$KEY_DIR" | awk '$0 ~ /^[0-9]+$/ && (m == "" || $0 + 0 > m + 0) {m = $0} END {print m}')
printf '%s primary=%s\n' "$verb" "$highest" >> "$ARGV_LOG"
n=$(awk 'END {print NR}' "$ARGV_LOG")
rc=$(printf '%s\n' "${KM_RCS:-0}" | awk -v n="$n" '{print ($n == "" ? 0 : $n)}')
case "$verb" in
fernet_rotate | credential_rotate)
	mv "$KEY_DIR/0" "$KEY_DIR/$((highest + 1))"
	if [ "$rc" -eq 0 ]; then
		printf 'new-%s' "$n" > "$KEY_DIR/0"
	fi
	;;
esac
exit "$rc"
STUB
cat > python3 <<'STUB'
#!/bin/sh
cat > /dev/null
printf '%s\n' '--- patch' >> "$PATCH_LOG"
for name in $(ls "$KEY_DIR"); do
	printf '%s=%s\n' "$name" "$(cat "$KEY_DIR/$name")" >> "$PATCH_LOG"
done
m=$(awk '$0 == "--- patch" {m++} END {print m}' "$PATCH_LOG")
exit "$(printf '%s\n' "${PATCH_RCS:-0}" | awk -v n="$m" '{print ($n == "" ? 0 : $n)}')"
STUB
chmod 0700 keystone-manage python3
PATH="$PWD:$PATH"
export PATH
`

// sourceKeys are the production keys of the read-only source mount.
var sourceKeys = map[string]string{"0": "k0", "1": "k1", "2": "k2"}

// otherRotation is the production key set after another Job committed its own
// rotation of sourceKeys: its key 0 is one this pod never wrote.
var otherRotation = map[string]string{"0": "other", "1": "k1", "2": "k2", "3": "k0"}

// rotatedOnce is the key set one rotation of sourceKeys leaves when the rotate
// was the n-th keystone-manage call: key 0 moved to 3 and a new key 0.
func rotatedOnce(n int) map[string]string {
	return map[string]string{"0": fmt.Sprintf("new-%d", n), "1": "k1", "2": "k2", "3": "k0"}
}

// writeKeys writes one file per entry of keys into path, replacing any file of
// that name.
func writeKeys(t *testing.T, path string, keys map[string]string) {
	t.Helper()
	g := NewGomegaWithT(t)
	for name, content := range keys {
		g.Expect(os.WriteFile(filepath.Join(path, name), []byte(content), 0o600)).To(Succeed())
	}
}

// rotationScriptDir returns a directory laid out as the rotation pod is when
// its rotate container first starts: src/ holds the production keys, and keys/
// and state/ are the empty working and rotation-state emptyDirs. It skips the
// test when the interpreter or a binary the scripts and stubs call is
// unavailable.
func rotationScriptDir(t *testing.T) string {
	t.Helper()
	g := NewGomegaWithT(t)

	for _, binary := range []string{rotationShPath, "awk", "install", "ls", "sha256sum"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is unavailable; the rotation scripts cannot be exercised here: %v", binary, err)
		}
	}
	dir := t.TempDir()
	for _, sub := range []string{"src", "keys", "state"} {
		g.Expect(os.Mkdir(filepath.Join(dir, sub), 0o750)).To(Succeed())
	}
	writeKeys(t, filepath.Join(dir, "src"), sourceKeys)
	return dir
}

// runRotationScript runs script behind the stub prelude with /bin/sh inside dir
// and returns its exit code and its combined stdout and stderr. The script
// travels through a file rather than through "sh -c" so the command line stays
// free of interpolated input. KEY_DIR, SRC_DIR and STATE_DIR point at the
// directories of rotationScriptDir, the stub logs live in dir, and env entries
// come last, so they win. A second call in the same dir is the container the
// kubelet restarts after a failure: every directory and log keeps its content.
func runRotationScript(t *testing.T, dir, script string, env ...string) (int, string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(os.WriteFile(filepath.Join(dir, "run.sh"), []byte(rotationStubPrelude+script), 0o600)).To(Succeed())

	cmd := exec.CommandContext(t.Context(), rotationShPath, "-eu", "run.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"KEY_DIR="+filepath.Join(dir, "keys"),
		"SRC_DIR="+filepath.Join(dir, "src"),
		"STATE_DIR="+filepath.Join(dir, "state"),
		"ARGV_LOG="+filepath.Join(dir, "argv.log"),
		"PATCH_LOG="+filepath.Join(dir, "patch.log"),
	)
	cmd.Env = append(cmd.Env, env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
			"the script must fail by exiting, not by failing to start: %v (%s)", err, out)
		return exitErr.ExitCode(), string(out)
	}
	return 0, string(out)
}

// readLogLines returns the lines of the log name in dir, nil when the log does
// not exist.
func readLogLines(t *testing.T, dir, name string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// readArgvLog returns one "<command> primary=<key>" line per keystone-manage
// call, nil when keystone-manage was never called.
func readArgvLog(t *testing.T, dir string) []string {
	t.Helper()
	return readLogLines(t, dir, "argv.log")
}

// readPatchLog returns the key set of every PATCH in order, empty when nothing
// was PATCHed.
func readPatchLog(t *testing.T, dir string) []map[string]string {
	t.Helper()
	g := NewGomegaWithT(t)
	var snapshots []map[string]string
	for _, line := range readLogLines(t, dir, "patch.log") {
		if line == "--- patch" {
			snapshots = append(snapshots, map[string]string{})
			continue
		}
		name, content, ok := strings.Cut(line, "=")
		g.Expect(ok && len(snapshots) > 0).To(BeTrue(), "unexpected PATCH log line %q", line)
		snapshots[len(snapshots)-1][name] = content
	}
	return snapshots
}

// readKeyDir returns the content and the permission bits of every file in path.
func readKeyDir(t *testing.T, path string) (map[string]string, map[string]os.FileMode) {
	t.Helper()
	g := NewGomegaWithT(t)
	entries, err := os.ReadDir(path)
	g.Expect(err).NotTo(HaveOccurred())
	contents := map[string]string{}
	modes := map[string]os.FileMode{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Clean(filepath.Join(path, entry.Name())))
		g.Expect(err).NotTo(HaveOccurred())
		info, err := entry.Info()
		g.Expect(err).NotTo(HaveOccurred())
		contents[entry.Name()] = string(content)
		modes[entry.Name()] = info.Mode().Perm()
	}
	return contents, modes
}

// expectSavedKeySet checks that the state directory holds want as the saved
// rotated key set, every file read-only for its owner, and no half-written
// copy.
func expectSavedKeySet(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	g := NewGomegaWithT(t)
	saved, modes := readKeyDir(t, filepath.Join(dir, "state", "rotated"))
	g.Expect(saved).To(Equal(want))
	g.Expect(modes).To(HaveEach(Equal(os.FileMode(0o400))))
	g.Expect(filepath.Join(dir, "state", "rotated.tmp")).NotTo(BeAnExistingFile())
}

// expectEmptySourceFailsBeforeRotating runs script with no production key to
// copy: the copy fails, so the script stops before keystone-manage, the PATCH
// and the save.
func expectEmptySourceFailsBeforeRotating(t *testing.T, script string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)
	for name := range sourceKeys {
		g.Expect(os.Remove(filepath.Join(dir, "src", name))).To(Succeed())
	}

	code, out := runRotationScript(t, dir, script)
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(BeEmpty())
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	g.Expect(filepath.Join(dir, "state", "rotated")).NotTo(BeAnExistingFile())
}

// expectMissingStateDirFailsBeforeRotating runs script without the
// rotation-state mount: without it a retry could not tell a rotated key set
// from the production keys, so the script exits 1 before it touches the keys.
func expectMissingStateDirFailsBeforeRotating(t *testing.T, script string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)
	// Keys in the working copy show whether the script reset it.
	writeKeys(t, filepath.Join(dir, "keys"), sourceKeys)
	absent := filepath.Join(dir, "absent")

	code, out := runRotationScript(t, dir, script, "STATE_DIR="+absent)
	g.Expect(code).To(Equal(1), out)
	g.Expect(out).To(ContainSubstring("ERROR: rotation state directory " + absent + " does not exist"))
	g.Expect(readArgvLog(t, dir)).To(BeEmpty())
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	keys, _ := readKeyDir(t, filepath.Join(dir, "keys"))
	g.Expect(keys).To(Equal(sourceKeys), "the working keys must stay untouched")
}

// expectCopiedKeysOwnerReadOnly runs script with a keystone-manage that fails
// its first call, so the working copy holds only the keys the script copied
// from the source mount, and checks that every copy is read-only for its owner.
// writeKeys writes the sources at 0o600, so a copy that kept their mode
// would fail. A world-readable key makes upstream Keystone log
// "key_repository is world readable".
func expectCopiedKeysOwnerReadOnly(t *testing.T, script string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, script, "KM_RCS=1")
	g.Expect(code).NotTo(Equal(0), out)
	_, modes := readKeyDir(t, filepath.Join(dir, "keys"))
	g.Expect(modes).To(HaveEach(Equal(os.FileMode(0o400))))
}

// expectLeftoverTmpIsDiscarded runs script with a rotated.tmp that a container
// killed while saving left behind. The script must discard it and save and
// PATCH want, the set of its own rotation; a script that kept it would fail
// its mkdir on every restart and never rotate.
func expectLeftoverTmpIsDiscarded(t *testing.T, script string, want map[string]string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)
	tmp := filepath.Join(dir, "state", "rotated.tmp")
	g.Expect(os.Mkdir(tmp, 0o750)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(tmp, "9"), []byte("stale"), 0o600)).To(Succeed())

	code, out := runRotationScript(t, dir, script)
	g.Expect(code).To(Equal(0), out)
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{want}))
	expectSavedKeySet(t, dir, want)
}

// expectRetryAfterProductionChangeStartsOver runs script with a refused first
// PATCH and then commits otherRotation to the source mount, as a Job created
// with kubectl create job --from does while this pod retries. The saved set
// lacks key 0 of otherRotation, so the retry must discard it and rotate the
// new production keys once. retryCalls are the keystone-manage calls of that
// rotation, and want is the key set it saves and PATCHes.
func expectRetryAfterProductionChangeStartsOver(t *testing.T, script string, retryCalls []string, want map[string]string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, script, "PATCH_RCS=1 0")
	g.Expect(code).NotTo(Equal(0), out)
	calls := readArgvLog(t, dir)
	patches := readPatchLog(t, dir)
	writeKeys(t, filepath.Join(dir, "src"), otherRotation)

	code, out = runRotationScript(t, dir, script, "PATCH_RCS=1 0")
	g.Expect(code).To(Equal(0), out)
	g.Expect(out).To(ContainSubstring("keys changed since this pod rotated; starting over"))
	g.Expect(readArgvLog(t, dir)).To(Equal(append(calls, retryCalls...)))
	g.Expect(readPatchLog(t, dir)).To(Equal(append(patches, want)))
	expectSavedKeySet(t, dir, want)
}

// expectRetryAfterCommittedPatchExits runs script with a first PATCH whose
// response is lost: the stub fails it, and the test commits the PATCHed set
// to the source mount, as the operator does. The retry finds its saved set in
// production and exits 0 without a keystone-manage call or a second PATCH.
func expectRetryAfterCommittedPatchExits(t *testing.T, script string) {
	t.Helper()
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, script, "PATCH_RCS=1")
	g.Expect(code).NotTo(Equal(0), out)
	calls := readArgvLog(t, dir)
	patches := readPatchLog(t, dir)
	g.Expect(patches).To(HaveLen(1))
	writeKeys(t, filepath.Join(dir, "src"), patches[0])

	code, out = runRotationScript(t, dir, script)
	g.Expect(code).To(Equal(0), out)
	g.Expect(out).To(ContainSubstring("keys this pod rotated; nothing to PATCH"))
	g.Expect(readArgvLog(t, dir)).To(Equal(calls))
	g.Expect(readPatchLog(t, dir)).To(Equal(patches))
}

// TestFernetRotateScript_FirstAttemptRotatesOnceAndPatches covers the first
// start of the rotate container: it rotates the production keys once, saves
// the rotated set, and PATCHes that set (S1).
func TestFernetRotateScript_FirstAttemptRotatesOnceAndPatches(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, fernetRotateScript)
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{"fernet_rotate primary=2"}))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(1)}))
	expectSavedKeySet(t, dir, rotatedOnce(1))
}

// TestFernetRotateScript_RetryAfterFailedPatchDoesNotRotateAgain covers the
// container the kubelet restarts after a refused PATCH: it restores the saved
// set and PATCHes it again. A second rotation would promote a key no API pod
// holds as staged.
func TestFernetRotateScript_RetryAfterFailedPatchDoesNotRotateAgain(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, fernetRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).NotTo(Equal(0), "the refused PATCH must fail the first attempt: %s", out)

	code, out = runRotationScript(t, dir, fernetRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{"fernet_rotate primary=2"}), "the retry must not rotate again")
	g.Expect(out).To(ContainSubstring("Retry: restoring the Fernet keys this pod rotated earlier; not rotating again"))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(1), rotatedOnce(1)}))
}

// TestFernetRotateScript_RetryClearsStrayWorkingKeys covers a retry whose
// working emptyDir holds a file the saved set lacks: the script clears the
// working copy before it restores the saved set, so the PATCH carries the
// saved set alone, without rotating again.
func TestFernetRotateScript_RetryClearsStrayWorkingKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, fernetRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(os.WriteFile(filepath.Join(dir, "keys", "4"), []byte("stray"), 0o600)).To(Succeed())

	code, out = runRotationScript(t, dir, fernetRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{"fernet_rotate primary=2"}))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(1), rotatedOnce(1)}))
}

// TestFernetRotateScript_RetryAfterProductionChangeStartsOver covers a retry
// that finds the production keys of another Job's rotation: it rotates them
// once instead of PATCHing a saved set that would drop that Job's keys.
func TestFernetRotateScript_RetryAfterProductionChangeStartsOver(t *testing.T) {
	expectRetryAfterProductionChangeStartsOver(t, fernetRotateScript,
		[]string{"fernet_rotate primary=3"},
		map[string]string{"0": "new-2", "1": "k1", "2": "k2", "3": "k0", "4": "other"})
}

// TestFernetRotateScript_RetryAfterCommittedPatchExits covers a retry after a
// PATCH the operator committed although its response was lost.
func TestFernetRotateScript_RetryAfterCommittedPatchExits(t *testing.T) {
	expectRetryAfterCommittedPatchExits(t, fernetRotateScript)
}

// TestFernetRotateScript_LeftoverTmpFromKilledSaveIsDiscarded covers a
// container killed between creating and renaming rotated.tmp.
func TestFernetRotateScript_LeftoverTmpFromKilledSaveIsDiscarded(t *testing.T) {
	expectLeftoverTmpIsDiscarded(t, fernetRotateScript, rotatedOnce(1))
}

// TestFernetRotateScript_FailedRotationStartsOverFromProductionKeys covers a
// rotate that failed halfway: nothing was saved, so the restarted container
// resets the working copy from the production keys and rotates once, rather
// than rotating the half-rotated directory again.
func TestFernetRotateScript_FailedRotationStartsOverFromProductionKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, fernetRotateScript, "KM_RCS=1")
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	g.Expect(filepath.Join(dir, "state", "rotated")).NotTo(BeAnExistingFile())

	code, out = runRotationScript(t, dir, fernetRotateScript, "KM_RCS=1")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{"fernet_rotate primary=2", "fernet_rotate primary=2"}),
		"the second attempt must rotate the production keys, not the half-rotated copy")
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(2)}))
}

// TestFernetRotateScript_CopiesKeysOwnerReadOnly covers the mode of the
// working copy.
func TestFernetRotateScript_CopiesKeysOwnerReadOnly(t *testing.T) {
	expectCopiedKeysOwnerReadOnly(t, fernetRotateScript)
}

// TestFernetRotateScript_EmptySourceFailsBeforeRotating covers a source mount
// without keys.
func TestFernetRotateScript_EmptySourceFailsBeforeRotating(t *testing.T) {
	expectEmptySourceFailsBeforeRotating(t, fernetRotateScript)
}

// TestFernetRotateScript_MissingStateDirFailsBeforeRotating covers a pod
// without the rotation-state mount.
func TestFernetRotateScript_MissingStateDirFailsBeforeRotating(t *testing.T) {
	expectMissingStateDirFailsBeforeRotating(t, fernetRotateScript)
}

// TestCredentialRotateScript_FirstAttemptMigratesRotatesMigrates covers the
// first start of the rotate container: it migrates under the production
// primary, rotates, migrates under the new primary, and PATCHes the rotated
// set (C1).
func TestCredentialRotateScript_FirstAttemptMigratesRotatesMigrates(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, credentialRotateScript)
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{
		"credential_migrate primary=2",
		"credential_rotate primary=2",
		"credential_migrate primary=3",
	}))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(2)}))
	expectSavedKeySet(t, dir, rotatedOnce(2))
}

// TestCredentialRotateScript_RetryAfterFailedPatchMigratesWithoutRotating
// covers the container the kubelet restarts after a refused PATCH: it restores
// the saved set, migrates under its primary again, and PATCHes it again. A
// second rotate would re-encrypt the database under a key that exists only
// inside the Job pod.
func TestCredentialRotateScript_RetryAfterFailedPatchMigratesWithoutRotating(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, credentialRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).NotTo(Equal(0), "the refused PATCH must fail the first attempt: %s", out)
	calls := readArgvLog(t, dir)

	code, out = runRotationScript(t, dir, credentialRotateScript, "PATCH_RCS=1 0")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal(append(calls, "credential_migrate primary=3")),
		"the retry must only migrate under the saved primary")
	g.Expect(out).To(ContainSubstring("Retry: restoring the credential keys this pod rotated earlier; not rotating again"))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(2), rotatedOnce(2)}))
}

// TestCredentialRotateScript_RetryAfterFailedMigrationOnlyMigrates covers a
// second migration that failed after the save: the restarted container
// restores the saved set and repeats only that migration before the PATCH.
func TestCredentialRotateScript_RetryAfterFailedMigrationOnlyMigrates(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, credentialRotateScript, "KM_RCS=0 0 1")
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(filepath.Join(dir, "state", "rotated")).To(BeADirectory())
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	calls := readArgvLog(t, dir)

	code, out = runRotationScript(t, dir, credentialRotateScript, "KM_RCS=0 0 1")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal(append(calls, "credential_migrate primary=3")))
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(2)}))
}

// TestCredentialRotateScript_FailedRotationStartsOverFromProductionKeys covers
// a rotate that failed halfway: nothing was saved, so the restarted container
// resets the working copy from the production keys and runs every step again.
func TestCredentialRotateScript_FailedRotationStartsOverFromProductionKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, credentialRotateScript, "KM_RCS=0 1")
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(filepath.Join(dir, "state", "rotated")).NotTo(BeAnExistingFile())
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	calls := readArgvLog(t, dir)

	code, out = runRotationScript(t, dir, credentialRotateScript, "KM_RCS=0 1")
	g.Expect(code).To(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal(append(calls,
		"credential_migrate primary=2",
		"credential_rotate primary=2",
		"credential_migrate primary=3",
	)), "the second attempt must start from the production keys, whose primary is 2")
	g.Expect(readPatchLog(t, dir)).To(Equal([]map[string]string{rotatedOnce(4)}))
}

// TestCredentialRotateScript_FailedFirstMigrationStopsBeforeRotating covers a
// failed migration under the production keys: credential_rotate would refuse
// anyway, so the script stops before it.
func TestCredentialRotateScript_FailedFirstMigrationStopsBeforeRotating(t *testing.T) {
	g := NewGomegaWithT(t)
	dir := rotationScriptDir(t)

	code, out := runRotationScript(t, dir, credentialRotateScript, "KM_RCS=1")
	g.Expect(code).NotTo(Equal(0), out)
	g.Expect(readArgvLog(t, dir)).To(Equal([]string{"credential_migrate primary=2"}))
	g.Expect(readPatchLog(t, dir)).To(BeEmpty())
	g.Expect(filepath.Join(dir, "state", "rotated")).NotTo(BeAnExistingFile())
}

// TestCredentialRotateScript_RetryAfterProductionChangeStartsOver covers a
// retry that finds the production keys of another Job's rotation: it runs
// every step against them instead of PATCHing a saved set that would drop
// that Job's keys.
func TestCredentialRotateScript_RetryAfterProductionChangeStartsOver(t *testing.T) {
	expectRetryAfterProductionChangeStartsOver(t, credentialRotateScript,
		[]string{"credential_migrate primary=3", "credential_rotate primary=3", "credential_migrate primary=4"},
		map[string]string{"0": "new-5", "1": "k1", "2": "k2", "3": "k0", "4": "other"})
}

// TestCredentialRotateScript_RetryAfterCommittedPatchExits covers a retry
// after a PATCH the operator committed although its response was lost.
func TestCredentialRotateScript_RetryAfterCommittedPatchExits(t *testing.T) {
	expectRetryAfterCommittedPatchExits(t, credentialRotateScript)
}

// TestCredentialRotateScript_LeftoverTmpFromKilledSaveIsDiscarded covers a
// container killed between creating and renaming rotated.tmp.
func TestCredentialRotateScript_LeftoverTmpFromKilledSaveIsDiscarded(t *testing.T) {
	expectLeftoverTmpIsDiscarded(t, credentialRotateScript, rotatedOnce(2))
}

// TestCredentialRotateScript_CopiesKeysOwnerReadOnly covers the mode of the
// working copy.
func TestCredentialRotateScript_CopiesKeysOwnerReadOnly(t *testing.T) {
	expectCopiedKeysOwnerReadOnly(t, credentialRotateScript)
}

// TestCredentialRotateScript_EmptySourceFailsBeforeRotating covers a source
// mount without keys.
func TestCredentialRotateScript_EmptySourceFailsBeforeRotating(t *testing.T) {
	expectEmptySourceFailsBeforeRotating(t, credentialRotateScript)
}

// TestCredentialRotateScript_MissingStateDirFailsBeforeRotating covers a pod
// without the rotation-state mount.
func TestCredentialRotateScript_MissingStateDirFailsBeforeRotating(t *testing.T) {
	expectMissingStateDirFailsBeforeRotating(t, credentialRotateScript)
}
