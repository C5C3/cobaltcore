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

// evacuateScriptRun is the evacuation script, unmodified, behind a shell
// function that stands in for ovn-nbctl. A function shadows the PATH lookup, so
// the script reaches the stub with its arguments intact and no executable has to
// be laid down for it.
//
// The stub records every invocation to CALL_LOG and answers the queries out of
// the environment: the write is the invocation that carries the removals, and it
// is the recording that carries the assertions, because a drain is judged by
// what it asked the database to do.
//
// The queries answer from the database the run has left behind rather than from
// the one it started with, because the script closes by asking what still names
// the chassis and a stub that replayed the seed regardless would answer that
// question for a database nobody wrote to.
const evacuateScriptRun = `ovn-nbctl() {
  printf '%s\n' "$*" >> "$CALL_LOG"
  case " $* " in
  *" find Gateway_Chassis "*) surviving "${FIND_GATEWAY_CHASSIS:-}"; return "${FIND_RC:-0}" ;;
  *" find HA_Chassis "*) surviving "${FIND_HA_CHASSIS:-}" ;;
  *" list Logical_Router_Port "*) printf '%s' "${LIST_LOGICAL_ROUTER_PORT:-}" ;;
  *" remove "*) printf '%s\n' "$*" >> "$REMOVED" ;;
  esac
}
# surviving replays a find payload without the rows the run has already removed.
# A row named by STICKY survives its own removal: that is the removal whose
# record ovn-nbctl could not resolve and skipped for --if-exists.
surviving() {
  local uuid removed=""
  if [ -s "$REMOVED" ]; then removed="$(cat "$REMOVED")"; fi
  while read -r uuid; do
    if [ -z "$uuid" ]; then continue; fi
    if [[ $removed != *"$uuid"* || $uuid == "${STICKY:-}" ]]; then printf '%s\n\n' "$uuid"; fi
  done <<< "$1"
}
` + evacuateScript

// The Northbound the stub serves: two Gateway_Chassis rows name the drained
// chassis, and two logical router ports hold them. The second port also holds a
// binding of another chassis, which the drain has to leave alone.
//
// Both payloads are what ovn-nbctl 26.03.2 prints for these queries: --bare
// separates records with a blank line, and the CSV writer wraps a set of more
// than one member in double quotes because its rendering holds a comma.
const (
	evacuateChassis = "worker-3"

	evacuateGatewayChassisRowA = "e04f3b33-60d7-467a-b45c-220803d8a47a"
	evacuateGatewayChassisRowB = "615a5540-6819-4342-a44c-50f527a01368"
	evacuateOtherChassisRow    = "7bc0c68f-a6f8-4dce-80e8-6d63b9386e0f"
	evacuateRouterPortA        = "6851b0b4-8c8d-41fe-8f87-2ea09e517206"
	evacuateRouterPortB        = "0a977973-8108-4924-92ad-db0e99611038"

	evacuateFindPayload = evacuateGatewayChassisRowA + "\n\n" + evacuateGatewayChassisRowB + "\n\n"
	evacuateListPayload = evacuateRouterPortA + ",[" + evacuateGatewayChassisRowA + "]\n" +
		evacuateRouterPortB + ",\"[" + evacuateGatewayChassisRowB + ", " + evacuateOtherChassisRow + "]\"\n"
)

// runEvacuateScript runs the script with env on top of the base environment and
// returns its exit code and the ovn-nbctl invocations it made. A non-zero exit
// is an expected outcome here, not a test failure.
func runEvacuateScript(t *testing.T, env ...string) (int, string) {
	t.Helper()
	g := NewGomegaWithT(t)

	if _, err := exec.LookPath(bashPath); err != nil {
		t.Skipf("%s is unavailable; the evacuation script cannot be exercised here: %v", bashPath, err)
	}
	// The script keeps the rows it collected in an associative array, which the
	// bash 3.2 macOS ships as /bin/bash does not have. The image that runs the
	// Job carries bash 5, and so does CI.
	probe := exec.CommandContext(t.Context(), bashPath, "-c", `[ "${BASH_VERSINFO[0]}" -ge 4 ]`)
	if err := probe.Run(); err != nil {
		t.Skipf("%s is older than bash 4; the evacuation script cannot be exercised here", bashPath)
	}
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", evacuateScriptRun)
	cmd.Env = append(os.Environ(),
		"NB_ADDR=ssl:10.96.0.11:6641",
		"CHASSIS="+evacuateChassis,
		"CALL_LOG="+callLog,
		"REMOVED="+filepath.Join(dir, "removed"),
	)
	cmd.Env = append(cmd.Env, env...)

	runErr := cmd.Run()
	calls, err := os.ReadFile(callLog)
	if errors.Is(err, os.ErrNotExist) {
		calls = nil
	} else {
		g.Expect(err).NotTo(HaveOccurred())
	}

	if runErr == nil {
		return 0, string(calls)
	}
	var exitErr *exec.ExitError
	g.Expect(errors.As(runErr, &exitErr)).To(BeTrue(),
		"the script must fail by exiting, not by failing to start: %v", runErr)
	return exitErr.ExitCode(), string(calls)
}

// A query that goes unanswered must fail the run. Its result is indistinguishable
// from a chassis that owns nothing, so a run that swallowed it would report a
// drain it never performed: the step writes gatewayEvacuated from the exit code,
// and every router pinned to the node would still name it once the node is gone.
func TestEvacuateScript_UnansweredQueryFailsTheRun(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t, "FIND_RC=1")

	g.Expect(code).NotTo(Equal(0), "an unanswered query must not read as an empty database")
	g.Expect(calls).NotTo(ContainSubstring("remove"),
		"a run that could not read the database must write nothing: "+calls)
}

// A chassis that carries no gateway duties is the ordinary case on an empty
// logical model, and it must cost no write. This is the run the failing one
// above has to stay distinguishable from.
func TestEvacuateScript_ChassisOwningNothingWritesNothing(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t)

	g.Expect(code).To(Equal(0), calls)
	g.Expect(calls).NotTo(ContainSubstring("remove"), calls)
	g.Expect(calls).NotTo(ContainSubstring("list"),
		"nothing owns a row, so no owner table has to be listed: "+calls)
}

// Every removal carries --if-exists. All of them are batched into one
// invocation, and remove resolves its record before the transaction commits: a
// single router port a tenant deleted between the listing and the write would
// otherwise abort the whole batch, discarding the removals of the rows that do
// still exist and leaving the drain stuck on a Job whose backoffLimit is 0.
func TestEvacuateScript_RemovalsCarryIfExists(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t,
		"FIND_GATEWAY_CHASSIS="+evacuateFindPayload,
		"LIST_LOGICAL_ROUTER_PORT="+evacuateListPayload)

	g.Expect(code).To(Equal(0), calls)
	write := writeCall(t, calls)
	g.Expect(strings.Count(write, "--if-exists remove")).To(Equal(2),
		"every removal must tolerate a row that is already gone: "+write)
	g.Expect(write).To(ContainSubstring("remove Logical_Router_Port " + evacuateRouterPortA +
		" gateway_chassis " + evacuateGatewayChassisRowA))
	g.Expect(write).To(ContainSubstring("remove Logical_Router_Port " + evacuateRouterPortB +
		" gateway_chassis " + evacuateGatewayChassisRowB))
	g.Expect(write).NotTo(ContainSubstring(evacuateOtherChassisRow),
		"the binding of another chassis must survive the drain: "+write)
}

// The owner row is addressed by its _uuid. read performs no CSV unquoting, and
// nothing constrains a Logical_Router_Port name to be comma-free, so listing the
// name would split one row across the two fields and send a record identifier
// to remove that no row answers to, which aborts the batch.
func TestEvacuateScript_OwnerRowsAreListedByUUID(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t,
		"FIND_GATEWAY_CHASSIS="+evacuateFindPayload,
		"LIST_LOGICAL_ROUTER_PORT="+evacuateListPayload)

	g.Expect(code).To(Equal(0), calls)
	g.Expect(calls).To(ContainSubstring("--columns=_uuid,gateway_chassis list Logical_Router_Port"),
		"the listing must carry the column remove is handed as the record: "+calls)
	g.Expect(calls).NotTo(ContainSubstring("--columns=name,"), calls)
}

// A drain that removed nothing must fail. args stays empty whenever the parse of
// the owner listing matches none of the rows the find collected — an OVN release
// that renders a set differently, an edit to the strip expression — and an empty
// batch invokes ovn-nbctl not at all. The exit code is the operator's only
// evidence: the step writes gatewayEvacuated from it, the prev.GatewayEvacuated
// guard retires the Job for that node, and every router port would go on naming
// a chassis that has left the cluster.
func TestEvacuateScript_OwnedRowsThatWereNotRemovedFailTheRun(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t, "FIND_GATEWAY_CHASSIS="+evacuateFindPayload)

	g.Expect(code).NotTo(Equal(0),
		"a chassis that still owns its rows has not been drained: "+calls)
	g.Expect(calls).NotTo(ContainSubstring("remove"), calls)
}

// --if-exists buys the batch its tolerance for a row that is already gone, and it
// pays for that with a record identifier no row answers to being skipped instead
// of aborting the invocation. A removal that resolved nothing leaves the binding
// in place behind an exit code of 0, so the run has to confirm the drain against
// the database rather than against the write it submitted.
func TestEvacuateScript_RemovalThatResolvedNoRecordFailsTheRun(t *testing.T) {
	g := NewGomegaWithT(t)

	code, calls := runEvacuateScript(t,
		"FIND_GATEWAY_CHASSIS="+evacuateFindPayload,
		"LIST_LOGICAL_ROUTER_PORT="+evacuateListPayload,
		"STICKY="+evacuateGatewayChassisRowB)

	g.Expect(code).NotTo(Equal(0),
		"a binding the write did not land on must not read as drained: "+calls)
	g.Expect(writeCall(t, calls)).To(ContainSubstring(evacuateGatewayChassisRowB),
		"the run must have attempted the removal it is failed for: "+calls)
}

// writeCall returns the invocation of a run that carries the removals. There is
// exactly one: a process per row would be a TLS handshake and a full Northbound
// schema download per row, which is what the batching buys.
func writeCall(t *testing.T, calls string) string {
	t.Helper()
	g := NewGomegaWithT(t)

	var writes []string
	for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
		if strings.Contains(line, "remove") {
			writes = append(writes, line)
		}
	}
	g.Expect(writes).To(HaveLen(1), "every removal belongs to one invocation: "+calls)
	return writes[0]
}
