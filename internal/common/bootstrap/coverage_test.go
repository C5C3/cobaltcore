// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// flushRecorder stands in for runtime/coverage: it records every write and
// clear in order and fails them on demand.
type flushRecorder struct {
	mu       sync.Mutex
	events   []string
	dirs     []string
	writeErr error
	clearErr error
}

func (r *flushRecorder) write(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "write")
	r.dirs = append(r.dirs, dir)
	return r.writeErr
}

func (r *flushRecorder) clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "clear")
	return r.clearErr
}

func (r *flushRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *flushRecorder) count(event string) int {
	n := 0
	for _, e := range r.snapshot() {
		if e == event {
			n++
		}
	}
	return n
}

func newTestFlusher(r *flushRecorder, interval time.Duration) *coverageFlusher {
	return &coverageFlusher{
		dir:      "/coverage",
		interval: interval,
		write:    r.write,
		clear:    r.clear,
		log:      logr.Discard(),
	}
}

// startFlusher runs f.Start in a goroutine and returns the channel its result
// arrives on.
func startFlusher(ctx context.Context, f *coverageFlusher) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()
	return done
}

func waitForStart(t *testing.T, done <-chan error, within time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(within):
		t.Fatalf("Start did not return within %s", within)
	}
}

func TestCoverageFlusher_WritesThenClearsOnEveryTick(t *testing.T) {
	r := &flushRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startFlusher(ctx, newTestFlusher(r, 10*time.Millisecond))

	deadline := time.Now().Add(2 * time.Second)
	for r.count("write") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d writes within 2s, want at least 3", r.count("write"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	waitForStart(t, done, time.Second)

	events := r.snapshot()
	if len(events)%2 != 0 {
		t.Fatalf("events = %v, want write/clear pairs", events)
	}
	for i, e := range events {
		want := "write"
		if i%2 == 1 {
			want = "clear"
		}
		if e != want {
			t.Fatalf("event %d = %q, want %q (events %v)", i, e, want, events)
		}
	}
	for _, dir := range r.dirs {
		if dir != "/coverage" {
			t.Fatalf("write got dir %q, want /coverage", dir)
		}
	}
}

func TestCoverageFlusher_FlushesOnceMoreWhenCancelled(t *testing.T) {
	r := &flushRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := newTestFlusher(r, time.Hour).Start(ctx); err != nil {
		t.Fatalf("Start returned %v, want nil", err)
	}
	if got := r.snapshot(); len(got) != 2 || got[0] != "write" || got[1] != "clear" {
		t.Fatalf("events = %v, want [write clear]", got)
	}
}

func TestCoverageFlusher_StopsAfterAWriteError(t *testing.T) {
	r := &flushRecorder{writeErr: errors.New(
		"WriteCountersDir invoked for program built with -covermode=<invalid> (please use -covermode=atomic)")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitForStart(t, startFlusher(ctx, newTestFlusher(r, 10*time.Millisecond)), time.Second)
	if ctx.Err() != nil {
		t.Fatal("Start returned only after the context was cancelled")
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.count("write"); got != 1 {
		t.Fatalf("write called %d times, want 1", got)
	}
	if got := r.count("clear"); got != 0 {
		t.Fatalf("clear called %d times after a failed write, want 0", got)
	}
}

func TestCoverageFlusher_StopsAfterAClearError(t *testing.T) {
	r := &flushRecorder{clearErr: errors.New("ClearCounters invoked for program built with -covermode=set")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitForStart(t, startFlusher(ctx, newTestFlusher(r, 10*time.Millisecond)), time.Second)
	if ctx.Err() != nil {
		t.Fatal("Start returned only after the context was cancelled")
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.count("write"); got != 1 {
		t.Fatalf("write called %d times, want 1", got)
	}
	if got := r.count("clear"); got != 1 {
		t.Fatalf("clear called %d times, want 1", got)
	}
}

func TestCoverageFlusherFromEnv(t *testing.T) {
	tests := []struct {
		name       string
		gocoverdir string
		wantNil    bool
	}{
		{name: "unset", gocoverdir: "", wantNil: true},
		{name: "set", gocoverdir: "/coverage", wantNil: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOCOVERDIR", tt.gocoverdir)
			f := coverageFlusherFromEnv()
			if tt.wantNil {
				if f != nil {
					t.Fatalf("got flusher %+v, want nil", f)
				}
				return
			}
			if f == nil {
				t.Fatal("got nil, want a flusher")
			}
			if f.dir != tt.gocoverdir {
				t.Errorf("dir = %q, want %q", f.dir, tt.gocoverdir)
			}
			if f.interval != coverageFlushInterval {
				t.Errorf("interval = %s, want %s", f.interval, coverageFlushInterval)
			}
			if f.write == nil || f.clear == nil {
				t.Error("write and clear must be set")
			}
		})
	}
}

func TestCoverageFlusher_NeedLeaderElection(t *testing.T) {
	if (&coverageFlusher{}).NeedLeaderElection() {
		t.Fatal("NeedLeaderElection() = true, want false")
	}
}
