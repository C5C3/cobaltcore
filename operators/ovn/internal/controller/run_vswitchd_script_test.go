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

// runVswitchdScriptRun is run-vswitchd.sh, unmodified, behind shell functions
// that stand in for ovs-vsctl and ovs-vswitchd and record every invocation to
// CALL_LOG. The builtin exec looks its command up on PATH only and would never
// reach the ovs-vswitchd stub, so a function shadows exec too and runs its
// command in place.
const runVswitchdScriptRun = `ovs-vsctl() { printf 'ovs-vsctl %s\n' "$*" >> "$CALL_LOG"; }
ovs-vswitchd() { printf 'ovs-vswitchd %s\n' "$*" >> "$CALL_LOG"; }
exec() { "$@"; }
` + runVswitchdScript

// runVswitchdDaemonStart is the invocation that starts the daemon.
const runVswitchdDaemonStart = "ovs-vswitchd unix:/run/openvswitch/db.sock " +
	"--pidfile=/run/openvswitch/ovs-vswitchd.pid --unixctl=/run/openvswitch/ovs-vswitchd.ctl"

// runRunVswitchdScript runs the script with env on top of the base environment
// and returns its exit code and the invocations it made. A non-zero exit is an
// expected outcome here, not a test failure.
func runRunVswitchdScript(t *testing.T, env ...string) (int, []string) {
	t.Helper()
	g := NewGomegaWithT(t)

	if _, err := exec.LookPath(bashPath); err != nil {
		t.Skipf("%s is unavailable; run-vswitchd.sh cannot be exercised here: %v", bashPath, err)
	}
	callLog := filepath.Join(t.TempDir(), "calls")

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", runVswitchdScriptRun)
	cmd.Env = append(os.Environ(), "CALL_LOG="+callLog)
	cmd.Env = append(cmd.Env, env...)

	runErr := cmd.Run()
	calls, err := os.ReadFile(filepath.Clean(callLog))
	g.Expect(err).NotTo(HaveOccurred(), "the script must at least wait for the database")
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")

	if runErr == nil {
		return 0, lines
	}
	var exitErr *exec.ExitError
	g.Expect(errors.As(runErr, &exitErr)).To(BeTrue(),
		"the script must fail by exiting, not by failing to start: %v", runErr)
	return exitErr.ExitCode(), lines
}

// A pod the OVS DaemonSet rendered, which carries OVS_REVALIDATOR_THREADS, pins
// the count between the init and the daemon start.
func TestRunVswitchdScript_PinsTheCountBeforeTheDaemonStarts(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runRunVswitchdScript(t, "OVS_REVALIDATOR_THREADS=2")

	g.Expect(code).To(Equal(0), "%v", calls)
	g.Expect(calls).To(Equal([]string{
		"ovs-vsctl --timeout=5 --no-wait show",
		"ovs-vsctl --no-wait init",
		"ovs-vsctl --no-wait set open . other_config:n-revalidator-threads=2",
		runVswitchdDaemonStart,
	}))
}

// A pod created from a template older than the pin has no
// OVS_REVALIDATOR_THREADS, yet the kubelet refreshes the scripts volume under
// it. When its ovs-vswitchd container restarts, the script must still start
// the daemon and leave the count to OVS until the pod is replaced: a failed
// start takes the node's datapath down, and under OnDelete nothing replaces
// the pod.
func TestRunVswitchdScript_WithoutTheVariableStillStartsTheDaemon(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runRunVswitchdScript(t)

	g.Expect(code).To(Equal(0), "%v", calls)
	g.Expect(calls).To(Equal([]string{
		"ovs-vsctl --timeout=5 --no-wait show",
		"ovs-vsctl --no-wait init",
		runVswitchdDaemonStart,
	}), "a pod without the variable writes no count")
}
