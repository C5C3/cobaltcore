// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strconv"

	commonv1 "github.com/c5c3/forge/internal/common/types"
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

// BuildUWSGICommand renders the uWSGI container command shared by every service
// operator's API container. It owns exactly the parts the operators would
// otherwise each repeat:
//
//   - default resolution: a nil p.UWSGI yields DefaultUWSGIProcesses,
//     DefaultUWSGIThreads, and DefaultUWSGIHTTPKeepAlive, while a non-nil spec
//     supplies Processes and Threads verbatim — falling back to the same two
//     defaults for a count left at or below zero — and keeps the keep-alive
//     default only while its nil-preserving HTTPKeepAlive pointer is unset;
//   - the keep-alive block --http-keepalive plus --http-keepalive-timeout, the
//     latter only when the spec bounds the idle timeout with a positive value;
//   - the request logging --log-master and --log-format UWSGILogFormat, emitted
//     in every configuration so request lines reach stderr even with keep-alive
//     disabled;
//   - the fixed run --master --lazy-apps --need-app with --processes and
//     --threads;
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
// Both keep-alive flags are dropped when keep-alive resolves false, even with
// HTTPKeepAliveTimeout set: the timeout has no meaning without its parent
// feature. The webhook rejects that combination at admission, so only a CR that
// bypassed the webhook reaches this path.
//
// The two optional caps are likewise dropped at or below zero — the same
// defense in depth the process and thread counts get, for the same reason: a
// value the API contract already forbids must not reach the container. Neither
// bounds anything at zero, so emitting it would be strictly worse than omitting
// the flag: "--http-keepalive-timeout 0" is the unbounded idle timeout the
// UWSGISpec field docs reject outright, and "--harakiri 0" disables the
// per-request kill the operator asked for.
//
// BuildUWSGICommand is a total function over valid params: it performs no I/O
// and has no error paths.
func BuildUWSGICommand(p UWSGICommandParams) []string {
	processes := commonv1.DefaultUWSGIProcesses
	threads := commonv1.DefaultUWSGIThreads
	httpKeepAlive := commonv1.DefaultUWSGIHTTPKeepAlive

	if p.UWSGI != nil {
		processes = p.UWSGI.Processes
		threads = p.UWSGI.Threads
		// Defense in depth for a spec that reached the builder without the CRD
		// defaults — an adopting operator whose spec omits the
		// +kubebuilder:default markers, or an in-process caller that never
		// round-trips the API server. A zero count renders "--processes 0", a
		// master that binds the socket, forks no worker, and serves nothing.
		if processes < 1 {
			processes = commonv1.DefaultUWSGIProcesses
		}
		if threads < 1 {
			threads = commonv1.DefaultUWSGIThreads
		}
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
		cmd = append(cmd, "--http-keepalive")
		// The > 0 arm is the same defense in depth the counts get above: the
		// Minimum=1 marker rejects zero at admission, and only keystone's webhook
		// re-checks it, so a spec that bypassed both must not turn into the
		// unbounded idle timeout.
		if p.UWSGI != nil && p.UWSGI.HTTPKeepAliveTimeout != nil && *p.UWSGI.HTTPKeepAliveTimeout > 0 {
			cmd = append(cmd, "--http-keepalive-timeout", strconv.Itoa(int(*p.UWSGI.HTTPKeepAliveTimeout)))
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
		"--processes", strconv.Itoa(int(processes)),
		"--threads", strconv.Itoa(int(threads)),
	)
	cmd = append(cmd, p.TrailingArgs...)
	// Same > 0 guard as the keep-alive timeout: a zero cap is no cap, so the flag
	// is dropped rather than emitted as "--harakiri 0".
	if p.UWSGI != nil && p.UWSGI.Harakiri != nil && *p.UWSGI.Harakiri > 0 {
		cmd = append(cmd, "--harakiri", strconv.Itoa(int(*p.UWSGI.Harakiri)))
	}
	return cmd
}
