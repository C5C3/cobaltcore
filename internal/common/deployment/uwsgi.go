// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strconv"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// UWSGILogFormat is the uWSGI --log-format literal every operator's API
// container emits so request lines reach stderr in the same shape.
const UWSGILogFormat = "%(method) %(uri) => generated %(rsize) bytes in %(msecs) msecs (%(proto) %(status))"

// UWSGICommandParams assembles the uWSGI command of a service operator's API
// container. The caller-supplied tokens are emitted verbatim in the order
// documented on BuildUWSGICommand, which also documents the flags the builder
// owns on its own.
type UWSGICommandParams struct {
	// UWSGI is the CR's uWSGI knob block. Nil falls back to the shared defaults
	// in the commonv1 package, the same values the defaulting webhooks apply, so
	// a CR without the block renders the documented command.
	UWSGI *commonv1.UWSGISpec

	// Bind is the --http bind, ":5000" for an API every in-cluster client may
	// reach or "127.0.0.1:5000" for one only a sidecar may reach.
	Bind string

	// ArgsAfterHTTP are raw tokens emitted directly after --http <Bind>, for the
	// transport flags a service needs on its HTTP socket. Nil or empty adds
	// nothing.
	ArgsAfterHTTP []string

	// WSGIFilePath is the --wsgi-file value: the entry script the service image
	// ships.
	WSGIFilePath string

	// Module is the --module value: the "package.module:callable" import path of
	// the WSGI application, for a service image that ships no entry script.
	// Exactly one of WSGIFilePath and Module must be non-empty; Module renders
	// --module in the position --wsgi-file occupies.
	Module string

	// TrailingArgs are raw tokens emitted after the --threads value and before
	// --harakiri, for the per-service flags that configure the WSGI application
	// itself. Nil or empty adds nothing.
	TrailingArgs []string
}

// EffectiveUWSGIConcurrency resolves the worker topology of the rendered uWSGI
// command: the process and thread counts after the same default resolution
// BuildUWSGICommand applies. A nil spec yields the commonv1 defaults; a
// non-positive count in a non-nil spec falls back to the same defaults, as
// defense in depth for a spec that reached the builder without the CRD
// defaults — an adopting operator whose spec omits the +kubebuilder:default
// markers, or an in-process caller that never round-trips the API server. (In
// the command, a zero count would render "--processes 0", a master that binds
// the socket, forks no worker, and serves nothing.)
//
// Callers sizing resources that scale with request concurrency — a database
// user's connection cap, most prominently — resolve the counts here so their
// arithmetic cannot drift from the command the container actually runs.
func EffectiveUWSGIConcurrency(spec *commonv1.UWSGISpec) (processes, threads int32) {
	processes = commonv1.DefaultUWSGIProcesses
	threads = commonv1.DefaultUWSGIThreads
	if spec != nil {
		if spec.Processes >= 1 {
			processes = spec.Processes
		}
		if spec.Threads >= 1 {
			threads = spec.Threads
		}
	}
	return processes, threads
}

// UWSGIGracefulShutdownArgs returns the uWSGI flags that make SIGTERM finish
// the requests in flight and exit. BuildUWSGICommand appends them after
// --need-app, and the horizon-operator appends them to its own command.
//
//   - --hook-master-start "unix_signal:15 gracefully_kill_them_all" replaces
//     the master's SIGTERM handler, a brutal reload by default, with the
//     graceful shutdown: every worker finishes its current request and exits,
//     then the master exits 0 after logging "goodbye to uWSGI.". An 8 s request
//     in flight at SIGTERM answered 200 with it, and an empty reply without it.
//   - --shutdown-sockets makes the first idle worker shut down the socket the
//     HTTP router forwards to, so a request that arrives after SIGTERM fails
//     at once. Without it, such a request was held 6 s, until the slowest
//     request in flight ended, and then got an empty reply.
//   - --threaded-logger moves the log reader into a master thread. The master
//     blocks while it waits for the workers to exit, and without the thread
//     only 275 of 4,000 log lines written during the shutdown, and no
//     "goodbye to uWSGI.", reached the container log.
//
// The master does not enforce --harakiri while it waits for the workers, so
// only the kubelet's SIGKILL at the end of the termination grace period bounds
// a request still running then. uWSGI resolves gracefully_kill_them_all with
// dlsym, and a binary that does not export it exits at start with
// `unable to find function "gracefully_kill_them_all"`.
//
// Each call returns a new slice, so a caller may modify the result.
func UWSGIGracefulShutdownArgs() []string {
	return []string{
		"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
		"--shutdown-sockets",
		"--threaded-logger",
	}
}

// BuildUWSGICommand renders the uWSGI container command shared by every service
// operator's API container. It owns exactly the parts the operators would
// otherwise each repeat:
//
//   - default resolution: a nil p.UWSGI yields DefaultUWSGIProcesses,
//     DefaultUWSGIThreads, and DefaultUWSGIHTTPKeepAlive, while a non-nil spec
//     supplies Processes and Threads verbatim — falling back to the same two
//     defaults for a count left at or below zero — and keeps the keep-alive
//     default only while its nil-preserving HTTPKeepAlive pointer is unset;
//   - the keep-alive token: the bare --http-keepalive, or --http-keepalive=<n>
//     when the spec bounds the idle timeout with a value above 1;
//   - the request logging --log-master and --log-format UWSGILogFormat, emitted
//     in every configuration so request lines reach stderr even with keep-alive
//     disabled;
//   - the fixed run --master --lazy-apps --need-app, the graceful-shutdown
//     flags of UWSGIGracefulShutdownArgs, then --processes and --threads;
//   - the --harakiri cap when the spec sets a positive one, last in the command.
//
// Everything else comes from p and is emitted verbatim: the bind, the launch
// mode, and the tokens in ArgsAfterHTTP and TrailingArgs. A service that needs
// no extra flags gets none.
//
// The launch mode is a single flag: --module p.Module when that field is set,
// otherwise --wsgi-file p.WSGIFilePath. Neither flag is emitted with an empty
// value, so params that set neither field render neither flag.
//
// The timeout is dropped with its parent flag when keep-alive resolves false,
// even with HTTPKeepAliveTimeout set: the timeout has no meaning without its
// parent feature. The webhook rejects that combination at admission, so only a
// CR that bypassed the webhook reaches this path.
//
// The timeout is likewise dropped at or below 1, and --harakiri at or below
// zero. This is the same defense in depth the process and thread counts get,
// for the same reason: a value the API contract already forbids must not reach
// the container. uWSGI reads --http-keepalive=0 as keep-alive off and
// --http-keepalive=1 as keep-alive without an idle timer, so neither bounds
// anything, and "--harakiri 0" disables the per-request kill the operator asked
// for.
//
// BuildUWSGICommand is a total function over valid params: it performs no I/O
// and has no error paths.
func BuildUWSGICommand(p UWSGICommandParams) []string {
	processes, threads := EffectiveUWSGIConcurrency(p.UWSGI)
	httpKeepAlive := commonv1.DefaultUWSGIHTTPKeepAlive

	if p.UWSGI != nil {
		// HTTPKeepAlive is a nil-preserving *bool: nil means "unset", which keeps
		// the default (true); an explicit true/false is honored verbatim.
		if p.UWSGI.HTTPKeepAlive != nil {
			httpKeepAlive = *p.UWSGI.HTTPKeepAlive
		}
	}

	cmd := []string{
		"uwsgi",
		"--http", p.Bind,
	}
	cmd = append(cmd, p.ArgsAfterHTTP...)
	if httpKeepAlive {
		// The idle timeout is the value of the keep-alive flag itself; uWSGI binds
		// the optional value only in the attached form. A value below
		// MinUWSGIHTTPKeepAliveTimeout renders the bare flag, the same defense in
		// depth the counts get above: =1 is the bare flag's own meaning, =0 would
		// turn keep-alive off, and a negative value would make uWSGI exit. Such a
		// value comes from a spec stored under the earlier Minimum=1, which the API
		// server keeps on an update that leaves the field unchanged, or from one
		// that bypassed admission.
		if p.UWSGI != nil && p.UWSGI.HTTPKeepAliveTimeout != nil && *p.UWSGI.HTTPKeepAliveTimeout >= commonv1.MinUWSGIHTTPKeepAliveTimeout {
			cmd = append(cmd, "--http-keepalive="+strconv.Itoa(int(*p.UWSGI.HTTPKeepAliveTimeout)))
		} else {
			cmd = append(cmd, "--http-keepalive")
		}
	}
	// Unconditional: makes uWSGI master logging always-on so request lines reach
	// stderr in every configuration, regardless of keep-alive.
	cmd = append(
		cmd,
		"--log-master",
		"--log-format", UWSGILogFormat,
	)
	// A service image ships either an entry script or an importable module, never
	// both, and an empty value would make uWSGI fail to load any application at
	// all — so each flag is emitted only with a value behind it.
	switch {
	case p.Module != "":
		cmd = append(cmd, "--module", p.Module)
	case p.WSGIFilePath != "":
		cmd = append(cmd, "--wsgi-file", p.WSGIFilePath)
	}
	cmd = append(
		cmd,
		"--master",
		"--lazy-apps",
		"--need-app",
	)
	cmd = append(cmd, UWSGIGracefulShutdownArgs()...)
	cmd = append(
		cmd,
		"--processes", strconv.Itoa(int(processes)),
		"--threads", strconv.Itoa(int(threads)),
	)
	cmd = append(cmd, p.TrailingArgs...)
	// A zero cap is no cap, so the flag is dropped rather than emitted as
	// "--harakiri 0".
	if p.UWSGI != nil && p.UWSGI.Harakiri != nil && *p.UWSGI.Harakiri > 0 {
		cmd = append(cmd, "--harakiri", strconv.Itoa(int(*p.UWSGI.Harakiri)))
	}
	return cmd
}
