// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// The replies an Open vSwitch database answers the gate's select with.
const (
	registeredReply = `{"id":0,"result":[{"rows":[{"external_ids":["map",[["hostname","node-1"],["system-id","5a1b"]]]}]}],"error":null}`
	emptyMapReply   = `{"id":0,"result":[{"rows":[{"external_ids":["map",[]]}]}],"error":null}`
	errorReply      = `{"id":0,"result":null,"error":"unknown database"}`
	echoRequest     = `{"id":"echo","method":"echo","params":[]}`
)

// gateSocket serves a unix socket that answers every connection's request with
// reply, and returns its path. The directory is short on purpose: a unix
// socket path is bounded to about a hundred bytes, and the per-test temp
// directory on macOS is longer than that.
func gateSocket(t *testing.T, reply string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ovs")
	if err != nil {
		t.Fatalf("creating the socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "db.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var request map[string]any
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					return
				}
				_, _ = conn.Write([]byte(reply))
				// Hold the connection open briefly, the way a server that keeps
				// the session does, so the gate decides from the reply alone.
				time.Sleep(200 * time.Millisecond)
			}()
		}
	}()
	return path
}

// runGate runs the gate script against socket and returns its error: nil when
// it exited 0, a context deadline when it was still looping after timeout.
func runGate(t *testing.T, socket string, timeout time.Duration) error {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", waitForChassisScript)
	cmd.Env = append(os.Environ(), "OVSDB_SOCKET="+socket)
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func TestWaitForChassisScript(t *testing.T) {
	t.Run("exits once the chassis is registered", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		g.Expect(runGate(t, gateSocket(t, registeredReply), 20*time.Second)).To(Succeed())
	})

	t.Run("skips an unsolicited echo before the reply", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		g.Expect(runGate(t, gateSocket(t, echoRequest+registeredReply), 20*time.Second)).To(Succeed())
	})

	for _, tc := range []struct {
		name   string
		socket func(t *testing.T) string
	}{
		{name: "keeps waiting on a row without system-id", socket: func(t *testing.T) string { return gateSocket(t, emptyMapReply) }},
		{name: "keeps waiting on an error reply", socket: func(t *testing.T) string { return gateSocket(t, errorReply) }},
		{name: "keeps waiting while the socket is missing", socket: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "absent.sock")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			err := runGate(t, tc.socket(t), 5*time.Second)
			g.Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(),
				"the gate must still be looping when it is killed, got %v", err)
		})
	}
}
