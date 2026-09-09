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
	"strconv"
	"testing"

	. "github.com/onsi/gomega"
)

// shPath is the interpreter the migrate phase's Job runs the check script with.
// The tests run the script through it directly, so a machine without it skips
// rather than exercising a different shell than production does.
const shPath = "/bin/sh"

// upgradeCheckLogName is where the run below writes the termination log. The
// production destination is /dev/termination-log, which belongs to the kubelet
// and no test process may write, so the run is assembled from the script's own
// two halves with this relative name in between. It is relative because the run
// has to stay a compile-time constant, and it lands in cmd.Dir.
const upgradeCheckLogName = "termination-log"

// upgradeCheckRun is the script as the tests run it: a prelude that lays down a
// cinder-status exiting with the code STUB_RC names and puts it on PATH, then
// the shipped script with its destination swapped. The stub is written by the
// shell rather than by the test process because a PATH entry has to be
// executable, and because that keeps the whole command line a constant.
//
// cinder-status cannot be shadowed by a shell function: POSIX rejects a hyphen
// in a function name, and /bin/sh is the interpreter the Job uses.
const upgradeCheckRun = "printf '#!/bin/sh\\nexit %s\\n' \"$STUB_RC\" > cinder-status; " +
	"chmod 0700 cinder-status; PATH=\"$PWD:$PATH\"; " +
	upgradeCheckScriptHead + upgradeCheckLogName + upgradeCheckScriptTail

// TestUpgradeCheckScript_PinsTheCommandLine pins the script the migrate Job
// runs. It is a string the operator ships rather than code it compiles, so this
// pin is what a review of a change to it reads.
func TestUpgradeCheckScript_PinsTheCommandLine(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(upgradeCheckScript).To(Equal(
		`rc=0; cinder-status --config-dir /etc/cinder/cinder.conf.d upgrade check || rc=$?; ` +
			`echo "cinder-status upgrade check exit $rc" > /dev/termination-log; ` +
			`case "$rc" in 0|1|2) exit 0;; *) exit "$rc";; esac`))
}

// TestUpgradeCheckScript_NormalisesTheCheckSeverities runs the script for each
// severity cinder-status reports. 0 is clean, 1 carries warnings and 2 means a
// check failed; none of the three may wedge the upgrade, so all three have to
// leave the Job complete. Anything else is a broken binary rather than a
// verdict, and fails the Job with its own code. Every run records the real code
// in the termination log, which is the only place the operator reads it back
// from.
func TestUpgradeCheckScript_NormalisesTheCheckSeverities(t *testing.T) {
	if _, err := exec.LookPath(shPath); err != nil {
		t.Skipf("%s is unavailable; the upgrade-check script cannot be exercised here: %v", shPath, err)
	}

	cases := []struct {
		checkExit int
		jobExit   int
		because   string
	}{
		{checkExit: 0, jobExit: 0, because: "a clean check completes the phase"},
		{checkExit: 1, jobExit: 0, because: "warnings are reported, not a reason to stop the upgrade"},
		{
			checkExit: 2, jobExit: 0,
			because: "a failed check (typically a volume whose service_uuid is still NULL) must not wedge the upgrade",
		},
		{checkExit: 3, jobExit: 3, because: "an exit code that is not a verdict fails the Job with its own code"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cinder-status exits %d", tc.checkExit), func(t *testing.T) {
			g := NewGomegaWithT(t)
			dir := t.TempDir()

			cmd := exec.CommandContext(t.Context(), shPath, "-eu", "-c", upgradeCheckRun)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "STUB_RC="+strconv.Itoa(tc.checkExit))

			exitCode := 0
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
					"the script must fail by exiting, not by failing to start: %v", err)
				exitCode = exitErr.ExitCode()
			}
			g.Expect(exitCode).To(Equal(tc.jobExit), tc.because)

			written, err := os.ReadFile(filepath.Clean(filepath.Join(dir, upgradeCheckLogName)))
			g.Expect(err).NotTo(HaveOccurred(), "every run must leave the exit code behind")
			g.Expect(string(written)).To(Equal(
				fmt.Sprintf("cinder-status upgrade check exit %d\n", tc.checkExit)))
		})
	}
}
