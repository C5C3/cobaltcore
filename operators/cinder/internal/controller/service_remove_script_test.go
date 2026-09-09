// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	. "github.com/onsi/gomega"
)

// serviceRemoveTestHost is the host identity the runs below unregister. It has
// to be a literal because the whole command line stays a compile-time constant.
const serviceRemoveTestHost = "cinder@nfs"

// serviceRemoveRun is the script as the tests run it: a prelude that lays down a
// cinder-manage exiting with the code STUB_RC names and puts it on PATH, then
// the shipped script. The stub is written by the shell rather than by the test
// process because a PATH entry has to be executable, and because that keeps the
// whole command line a constant.
//
// cinder-manage cannot be shadowed by a shell function: POSIX rejects a hyphen
// in a function name, and /bin/sh is the interpreter the Job uses.
const serviceRemoveRun = "printf '#!/bin/sh\\nexit %s\\n' \"$STUB_RC\" > cinder-manage; " +
	"chmod 0700 cinder-manage; PATH=\"$PWD:$PATH\"; " +
	serviceRemoveScriptHead + serviceRemoveTestHost + serviceRemoveScriptTail

// TestServiceRemoveScript_PinsTheCommandLine pins the script the detach Job
// runs. It is a string the operator ships rather than code it compiles, so this
// pin is what a review of a change to it reads.
func TestServiceRemoveScript_PinsTheCommandLine(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(serviceRemoveScript(validCinder(), "nfs")).To(Equal(
		`rc=0; cinder-manage --config-dir /etc/cinder/cinder.conf.d ` +
			`service remove cinder-volume cinder@nfs || rc=$?; ` +
			`case "$rc" in 0|2) exit 0;; *) exit "$rc";; esac`))
}

// TestServiceRemoveScript_NormalisesHostNotFound runs the script for the exit
// codes cinder-manage reports. A backend whose cinder-volume never registered
// has no entry to remove, and it must still detach, so "Host not found" leaves
// the Job complete, while a real failure keeps its own code and holds the
// finalizer.
func TestServiceRemoveScript_NormalisesHostNotFound(t *testing.T) {
	if _, err := exec.LookPath(shPath); err != nil {
		t.Skipf("%s is unavailable; the service-remove script cannot be exercised here: %v", shPath, err)
	}

	cases := []struct {
		removeExit int
		jobExit    int
		because    string
	}{
		{removeExit: 0, jobExit: 0, because: "the entry was removed"},
		{
			removeExit: 2, jobExit: 0,
			because: "Host not found: a backend that never registered must still detach",
		},
		{removeExit: 1, jobExit: 1, because: "a failed removal keeps the backend held"},
		{removeExit: 3, jobExit: 3, because: "an unknown code fails the Job with its own"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cinder-manage exits %d", tc.removeExit), func(t *testing.T) {
			g := NewGomegaWithT(t)

			cmd := exec.CommandContext(t.Context(), shPath, "-eu", "-c", serviceRemoveRun)
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "STUB_RC="+strconv.Itoa(tc.removeExit))

			exitCode := 0
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				g.Expect(errors.As(err, &exitErr)).To(BeTrue(),
					"the script must fail by exiting, not by failing to start: %v", err)
				exitCode = exitErr.ExitCode()
			}
			g.Expect(exitCode).To(Equal(tc.jobExit), tc.because)
		})
	}
}
