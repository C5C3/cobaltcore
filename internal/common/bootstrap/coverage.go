// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"os"
	"runtime/coverage"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// coverageFlushInterval is how often a coverage-instrumented manager writes its
// counters to GOCOVERDIR.
const coverageFlushInterval = time.Minute

// coverageFlusher writes the Go coverage counters of a binary built with -cover
// to a directory at a fixed interval. Such a binary writes its counters only
// when it returns from main or calls os.Exit, and an operator pod killed with
// grace period 0, as a chaos pod-kill does, does neither. Every flush clears the
// counters after writing them, so each counter file holds the delta since the
// previous flush and `go tool covdata` sums the files back together.
// ClearCounters works only under -covermode=atomic.
type coverageFlusher struct {
	dir      string
	interval time.Duration
	write    func(dir string) error
	clear    func() error
	log      logr.Logger
}

// coverageFlusherFromEnv returns a flusher for the GOCOVERDIR directory, or nil
// when GOCOVERDIR is unset, as it is in every production pod.
func coverageFlusherFromEnv() *coverageFlusher {
	dir := os.Getenv("GOCOVERDIR")
	if dir == "" {
		return nil
	}
	return &coverageFlusher{
		dir:      dir,
		interval: coverageFlushInterval,
		write:    coverage.WriteCountersDir,
		clear:    coverage.ClearCounters,
		log:      ctrl.Log.WithName("coverage"),
	}
}

// Start flushes on every tick and once more when ctx is done. A failed flush
// ends the loop. Start never returns an error, so a coverage problem cannot stop
// the manager.
func (f *coverageFlusher) Start(ctx context.Context) error {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			f.flush()
			return nil
		case <-ticker.C:
			if !f.flush() {
				return nil
			}
		}
	}
}

// flush writes the counters and then clears them. It reports false when either
// step fails; a failed write skips the clear.
func (f *coverageFlusher) flush() bool {
	if err := f.write(f.dir); err != nil {
		f.log.Error(err, "coverage flush failed; no further flushes", "dir", f.dir)
		return false
	}
	if err := f.clear(); err != nil {
		f.log.Error(err, "coverage flush failed; no further flushes", "dir", f.dir)
		return false
	}
	return true
}

// NeedLeaderElection returns false: every replica runs code worth measuring,
// including one that does not hold the lease.
func (f *coverageFlusher) NeedLeaderElection() bool {
	return false
}

var (
	_ manager.Runnable               = (*coverageFlusher)(nil)
	_ manager.LeaderElectionRunnable = (*coverageFlusher)(nil)
)
