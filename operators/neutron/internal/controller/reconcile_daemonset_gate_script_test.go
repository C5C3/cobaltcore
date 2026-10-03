// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// shPath is the interpreter the init container runs the gate with. The tests
// run the script through it directly, so a machine without it skips rather
// than exercising a different shell than production does.
const shPath = "/bin/sh"

// gateStubPrelude lays down an ovsdb-client stub in the working directory and
// puts it first on PATH. A call whose arguments name a unix: remote is the
// local Open vSwitch query, and every other call is the Southbound query. Each
// kind appends its argument list to <kind>.calls, prints <kind>.reply when that
// file exists, and exits with the code in <kind>.rc, 0 when there is none. The
// files are the test's handle on both databases: it writes the replies and
// codes and reads the calls back.
//
// The stub is written by the shell rather than by the test process because a
// PATH entry has to be executable, and because that keeps the whole command
// line a constant. ovsdb-client cannot be shadowed by a shell function: POSIX
// rejects a hyphen in a function name.
const gateStubPrelude = `cat > ovsdb-client <<'STUB'
#!/bin/sh
case " $* " in
*" unix:"*) kind=local ;;
*) kind=sb ;;
esac
printf '%s\n' "$*" >> "$kind.calls"
if [ -f "$kind.reply" ]; then cat "$kind.reply"; fi
if [ -f "$kind.rc" ]; then exit "$(cat "$kind.rc")"; fi
STUB
chmod 0700 ovsdb-client
PATH="$PWD:$PATH"
export PATH
`

// gateRun is the gate as the tests run it: the stub prelude, then the shipped
// script unchanged.
const gateRun = gateStubPrelude + waitForChassisScript

// The system-ids the cases use: the one a reused database still holds, and the
// one the current OVNChassis renders for the node.
const (
	gateStaleID = "5a1b3c4d-6e7f-4a8b-9c0d-1e2f3a4b5c6d"
	gateNewID   = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b"
)

// The replies ovsdb-client transact prints that carry no id.
const (
	emptyMapLocalReply = `[{"rows":[{"external_ids":["map",[]]}]}]`
	noRowsSBReply      = `[{"rows":[]}]`
	errorSBReply       = `[{"details":"no table named Chassis_Private","error":"syntax error"}]`
)

// The lines the gate prints for its two wait states.
const localWaitLine = "waiting for the chassis to write its system-id into the local Open vSwitch database"

func sbWaitLine(id string) string {
	return "waiting for chassis " + id + " to register in the Southbound database"
}

func registeredLine(id string) string {
	return "chassis " + id + " is registered in the Southbound database"
}

// localReply is the local database's answer for a node whose Open_vSwitch row
// carries system-id id.
func localReply(id string) string {
	return `[{"rows":[{"external_ids":["map",[["hostname","node-1"],["system-id","` + id + `"]]]}]}]`
}

// sbReply is the Southbound database's answer when a Chassis_Private row named
// id exists.
func sbReply(id string) string {
	return `[{"rows":[{"name":"` + id + `"}]}]`
}

// writeGateFile writes one of the stub's files by rename, so a stub reading it
// while the gate loops sees either the old content or the new one.
func writeGateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatalf("renaming %s: %v", tmp, err)
	}
}

// gateDir returns a fresh working directory holding the stub's files.
func gateDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		writeGateFile(t, dir, name, content)
	}
	return dir
}

// readGateFile returns the content of one of the stub's files and whether it
// exists.
func readGateFile(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(content), true
}

// gateResult is what one run of the gate left behind. err is nil when the gate
// exited 0, context.DeadlineExceeded when it was still looping at its deadline,
// and an *exec.ExitError when it exited non-zero.
type gateResult struct {
	err    error
	stdout string
	stderr string
}

// startGate starts the gate in dir with env on top of the test's environment,
// which has OVN_SB_CONNECTION removed, and returns the function that waits for
// it. The gate is killed at timeout. A killed run returns up to 2 seconds
// later, because the orphaned sleep holds the output pipe open.
func startGate(t *testing.T, dir string, timeout time.Duration, env ...string) func() gateResult {
	t.Helper()
	if _, err := exec.LookPath(shPath); err != nil {
		t.Skipf("%s is unavailable; the gate cannot be exercised here: %v", shPath, err)
	}

	environ := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, sbConnectionEnvVarName+"=")
	})
	environ = append(environ, env...)

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	cmd := exec.CommandContext(ctx, shPath, "-c", gateRun)
	cmd.Dir = dir
	cmd.Env = environ
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting the gate: %v", err)
	}
	return func() gateResult {
		defer cancel()
		err := cmd.Wait()
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		return gateResult{err: err, stdout: stdout.String(), stderr: stderr.String()}
	}
}

// lastLine is the last line a run printed.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestWaitForChassisScript runs the gate of the metadata agent against a stub
// ovsdb-client. It passes only once the system-id of the local database has a
// Chassis_Private row in the Southbound database, so an id a reused database
// still holds keeps the agent waiting until the node's current id registers.
func TestWaitForChassisScript(t *testing.T) {
	sbEnv := sbConnectionEnvVarName + "=" + testSouthboundAddress

	t.Run("exits once the local system-id is registered in the Southbound database", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		dir := gateDir(t, map[string]string{"local.reply": localReply(gateNewID), "sb.reply": sbReply(gateNewID)})

		got := startGate(t, dir, 20*time.Second, sbEnv)()

		g.Expect(got.err).NotTo(HaveOccurred(), "stderr: %s", got.stderr)
		g.Expect(lastLine(got.stdout)).To(Equal(registeredLine(gateNewID)))
	})

	t.Run("asks every Southbound remote with the client identity and without the leader", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		dir := gateDir(t, map[string]string{"local.reply": localReply(gateNewID), "sb.reply": sbReply(gateNewID)})
		remotes := "ssl:10.96.0.21:6642,ssl:10.96.0.22:6642"

		got := startGate(t, dir, 20*time.Second, sbConnectionEnvVarName+"="+remotes)()

		g.Expect(got.err).NotTo(HaveOccurred(), "stderr: %s", got.stderr)
		calls, ok := readGateFile(t, dir, "sb.calls")
		g.Expect(ok).To(BeTrue(), "the gate must have asked the Southbound database")
		for _, want := range []string{
			"--timeout=5",
			"--no-leader-only",
			"-p /etc/ovn/tls/tls.key",
			"-c /etc/ovn/tls/tls.crt",
			"-C /etc/ovn/tls/ca.crt",
			" transact " + remotes + " ",
			"OVN_Southbound",
			"Chassis_Private",
			gateNewID,
		} {
			g.Expect(calls).To(ContainSubstring(want))
		}
	})

	t.Run("keeps waiting on a system-id the Southbound database has no row for", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		dir := gateDir(t, map[string]string{"local.reply": localReply(gateStaleID), "sb.reply": noRowsSBReply})

		got := startGate(t, dir, 5*time.Second, sbEnv)()

		g.Expect(errors.Is(got.err, context.DeadlineExceeded)).To(BeTrue(),
			"the gate must still be looping when it is killed, got %v", got.err)
		calls, _ := readGateFile(t, dir, "sb.calls")
		g.Expect(strings.Count(calls, "\n")).To(BeNumerically(">=", 2),
			"the gate must have made a second pass for the print-once check to mean anything")
		g.Expect(strings.Count(got.stdout, sbWaitLine(gateStaleID))).To(Equal(1),
			"a message is printed once, not on every pass")
	})

	t.Run("follows the system-id from a stale one to the registered one", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		dir := gateDir(t, map[string]string{"local.reply": localReply(gateStaleID), "sb.reply": sbReply(gateNewID)})

		wait := startGate(t, dir, 20*time.Second, sbEnv)
		g.Eventually(func() string {
			calls, _ := readGateFile(t, dir, "sb.calls")
			return calls
		}).WithTimeout(10*time.Second).WithPolling(100*time.Millisecond).Should(ContainSubstring(gateStaleID),
			"the gate must ask the Southbound database for the stale id first")
		writeGateFile(t, dir, "local.reply", localReply(gateNewID))
		got := wait()

		g.Expect(got.err).NotTo(HaveOccurred(), "stderr: %s", got.stderr)
		g.Expect(got.stdout).To(ContainSubstring(sbWaitLine(gateStaleID)))
		g.Expect(lastLine(got.stdout)).To(Equal(registeredLine(gateNewID)))
	})

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{name: "keeps waiting on a local row without system-id", files: map[string]string{
			"local.reply": emptyMapLocalReply,
		}},
		{name: "keeps waiting while the local query fails", files: map[string]string{
			"local.rc": "1",
		}},
		{name: "keeps waiting on a local system-id that is not a UUID", files: map[string]string{
			"local.reply": localReply("node-1"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			tc.files["sb.reply"] = sbReply("node-1")
			dir := gateDir(t, tc.files)

			got := startGate(t, dir, 5*time.Second, sbEnv)()

			g.Expect(errors.Is(got.err, context.DeadlineExceeded)).To(BeTrue(),
				"the gate must still be looping when it is killed, got %v", got.err)
			calls, _ := readGateFile(t, dir, "local.calls")
			g.Expect(strings.Count(calls, "\n")).To(BeNumerically(">=", 2),
				"the gate must have made a second pass for the print-once check to mean anything")
			g.Expect(strings.Count(got.stdout, localWaitLine)).To(Equal(1))
			_, asked := readGateFile(t, dir, "sb.calls")
			g.Expect(asked).To(BeFalse(), "without a UUID system-id there is nothing to ask the Southbound database")
		})
	}

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{name: "keeps waiting while the Southbound query fails", files: map[string]string{
			"sb.rc": "1",
		}},
		{name: "keeps waiting on a Southbound error reply", files: map[string]string{
			"sb.reply": errorSBReply,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			tc.files["local.reply"] = localReply(gateNewID)
			dir := gateDir(t, tc.files)

			got := startGate(t, dir, 5*time.Second, sbEnv)()

			g.Expect(errors.Is(got.err, context.DeadlineExceeded)).To(BeTrue(),
				"the gate must still be looping when it is killed, got %v", got.err)
			calls, _ := readGateFile(t, dir, "sb.calls")
			g.Expect(strings.Count(calls, "\n")).To(BeNumerically(">=", 2),
				"the gate must have made a second pass for the print-once check to mean anything")
			g.Expect(strings.Count(got.stdout, sbWaitLine(gateNewID))).To(Equal(1))
		})
	}

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "fails without a Southbound address"},
		{name: "fails on an empty Southbound address", env: []string{sbConnectionEnvVarName + "="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			dir := gateDir(t, map[string]string{"local.reply": localReply(gateNewID), "sb.reply": sbReply(gateNewID)})

			got := startGate(t, dir, 20*time.Second, tc.env...)()

			var exitErr *exec.ExitError
			g.Expect(errors.As(got.err, &exitErr)).To(BeTrue(),
				"the gate must fail by exiting, got %v", got.err)
			g.Expect(exitErr.ExitCode()).To(Equal(1), "no wait repairs a rendering fault")
			g.Expect(got.stderr).To(ContainSubstring("OVN_SB_CONNECTION is not set"))
			for _, calls := range []string{"local.calls", "sb.calls"} {
				_, called := readGateFile(t, dir, calls)
				g.Expect(called).To(BeFalse(), "ovsdb-client must not be called")
			}
		})
	}
}
