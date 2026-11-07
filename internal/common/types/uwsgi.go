// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package types

import "k8s.io/utils/ptr"

// UWSGISpec defines the uWSGI application server parameters.
// Exposed as an optional pointer field on the owning operator's spec so that
// existing CRs without it continue to work with hardcoded defaults in the
// reconciler. The cross-field CEL rule mirrors the validating webhooks:
// httpKeepAliveTimeout is only meaningful when httpKeepAlive is true, otherwise
// the keep-alive idle timeout is never rendered.
// +kubebuilder:validation:XValidation:rule="!has(self.httpKeepAliveTimeout) || !has(self.httpKeepAlive) || self.httpKeepAlive",message="httpKeepAliveTimeout may only be set when httpKeepAlive is true"
type UWSGISpec struct {
	// Processes is the number of uWSGI worker processes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Processes int32 `json:"processes,omitempty"`

	// Threads is the number of threads per uWSGI worker process.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Threads int32 `json:"threads,omitempty"`

	// HTTPKeepAlive enables the --http-keepalive flag on the uWSGI process.
	// When false, the flag is omitted from the command. It is a nil-preserving
	// pointer so "unset" is representable: the owning operator's defaulting
	// webhook restores the documented default (true) when the pointer is nil,
	// and the reconciler falls back to the same default for CRs that bypass the
	// webhook. An explicit false is honored verbatim.
	// +optional
	HTTPKeepAlive *bool `json:"httpKeepAlive,omitempty"`

	// Harakiri caps the per-request worker lifetime (seconds) via the uWSGI
	// --harakiri flag. While uWSGI serves, a request blocked longer than this
	// bound is killed, so a single stuck request cannot hold its worker.
	// uWSGI does not enforce it during the graceful shutdown that SIGTERM
	// starts: only the kubelet's SIGKILL at the end of
	// terminationGracePeriodSeconds bounds a request still running then. When
	// nil, the --harakiri flag is omitted from the uWSGI command entirely (no
	// hidden default is injected). The webhook additionally requires
	// harakiri < terminationGracePeriodSeconds - preStopSleepSeconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Harakiri *int32 `json:"harakiri,omitempty"`

	// HTTPKeepAliveTimeout bounds the idle time (seconds) a keep-alive
	// connection may stay open between two requests. It is rendered as the
	// value of the uWSGI keep-alive flag, --http-keepalive=<n>. A bounded
	// timeout forces clients to reconnect through the Service so they never
	// reuse a socket to a removed pod. When nil, the bare --http-keepalive is
	// rendered and an idle connection stays open until the router's 60-second
	// socket timeout. The minimum is 2 because uWSGI reads 1 as keep-alive
	// without an idle timer and 0 as keep-alive off. A value at or below
	// preStopSleepSeconds is recommended so idle sockets have closed before
	// SIGTERM reaches uWSGI.
	// +optional
	// +kubebuilder:validation:Minimum=2
	HTTPKeepAliveTimeout *int32 `json:"httpKeepAliveTimeout,omitempty"`
}

// Default sets the shared-type defaults on a UWSGISpec in place: a zero-valued
// Processes/Threads becomes DefaultUWSGIProcesses/DefaultUWSGIThreads, and a nil
// HTTPKeepAlive is materialized as the documented default so "unset" stays
// distinguishable from an explicit false, which is left untouched. A nil
// receiver is a no-op: the owning operator's uwsgi block is optional and an
// absent block must stay absent so the reconciler falls back to the same
// constants. Operator webhooks call it so the leaf defaults cannot drift across
// operators.
func (u *UWSGISpec) Default() {
	if u == nil {
		return
	}
	if u.Processes == 0 {
		u.Processes = DefaultUWSGIProcesses
	}
	if u.Threads == 0 {
		u.Threads = DefaultUWSGIThreads
	}
	if u.HTTPKeepAlive == nil {
		u.HTTPKeepAlive = ptr.To(DefaultUWSGIHTTPKeepAlive)
	}
}

// DefaultUWSGIProcesses / DefaultUWSGIThreads / DefaultUWSGIHTTPKeepAlive are
// the uWSGI defaults applied by the keystone, glance, and placement defaulting
// webhooks (Processes/Threads/HTTPKeepAlive) and by the shared command builder
// when the uWSGI spec is nil. They live here as the single source of truth so
// the webhooks and the builder cannot drift; the +kubebuilder:default markers
// on UWSGISpec above keep the same literals in sync separately (kubebuilder
// markers cannot reference Go constants).
const (
	// DefaultUWSGIProcesses is the uWSGI worker-process count materialized when
	// the owning operator's uwsgi spec leaves processes zero.
	DefaultUWSGIProcesses int32 = 2
	// DefaultUWSGIThreads is the per-worker thread count materialized when the
	// owning operator's uwsgi spec leaves threads zero.
	DefaultUWSGIThreads int32 = 1
	// DefaultUWSGIHTTPKeepAlive is the --http-keepalive default restored when
	// the owning operator's uwsgi spec leaves httpKeepAlive nil (unset).
	DefaultUWSGIHTTPKeepAlive = true
)

// MinUWSGIHTTPKeepAliveTimeout is the lowest httpKeepAliveTimeout that bounds
// the idle time: uWSGI reads --http-keepalive=1 as keep-alive without an idle
// timer and --http-keepalive=0 as keep-alive off. The keystone webhook and the
// shared command builder read it; the +kubebuilder:validation:Minimum=2 marker
// on UWSGISpec.HTTPKeepAliveTimeout keeps the same literal in sync separately.
const MinUWSGIHTTPKeepAliveTimeout int32 = 2
