// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strings"
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// indexOf returns the position of tok in cmd, or -1 when it is absent.
func indexOf(cmd []string, tok string) int {
	for i, arg := range cmd {
		if arg == tok {
			return i
		}
	}
	return -1
}

// valueAfter returns the token following flag, or "" when the flag is absent or
// ends the command. A flag whose value went missing therefore fails the same
// assertion as a flag that was never emitted.
func valueAfter(cmd []string, flag string) string {
	i := indexOf(cmd, flag)
	if i < 0 || i+1 >= len(cmd) {
		return ""
	}
	return cmd[i+1]
}

// countOf returns how many times tok appears in cmd.
func countOf(cmd []string, tok string) int {
	n := 0
	for _, arg := range cmd {
		if arg == tok {
			n++
		}
	}
	return n
}

// gracefulShutdownTokens is the literal UWSGIGracefulShutdownArgs returns,
// spelled out so a change to the shared flags fails here first.
var gracefulShutdownTokens = []string{
	"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
	"--shutdown-sockets",
	"--threaded-logger",
}

// expectGracefulShutdownFlags asserts that cmd carries each graceful-shutdown
// token once, contiguous, directly after --need-app and directly before
// --processes.
func expectGracefulShutdownFlags(t *testing.T, cmd []string) {
	t.Helper()
	g := gomega.NewWithT(t)

	for _, tok := range gracefulShutdownTokens {
		g.Expect(countOf(cmd, tok)).To(gomega.Equal(1), "token %q", tok)
	}
	i := indexOf(cmd, "--need-app")
	g.Expect(i).To(gomega.BeNumerically(">=", 0))
	end := i + 1 + len(gracefulShutdownTokens)
	g.Expect(len(cmd)).To(gomega.BeNumerically(">", end))
	g.Expect(cmd[i+1 : end]).To(gomega.Equal(gracefulShutdownTokens))
	g.Expect(cmd[end]).To(gomega.Equal("--processes"))
}

// keepAliveTokens returns every token with the prefix --http-keepalive: the bare
// flag, the --http-keepalive=<n> form, and any stray flag that shares the prefix.
func keepAliveTokens(cmd []string) []string {
	var tokens []string
	for _, arg := range cmd {
		if strings.HasPrefix(arg, "--http-keepalive") {
			tokens = append(tokens, arg)
		}
	}
	return tokens
}

// A CR without a uwsgi block must still get the documented command: the shared
// defaults, keep-alive on with no timeout, and no harakiri cap at all (the
// builder injects no hidden default for the optional flags).
func TestBuildUWSGICommand_NilSpecDefaults(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{Bind: ":5000", WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi"})

	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("2"))
	g.Expect(valueAfter(cmd, "--threads")).To(gomega.Equal("1"))
	g.Expect(keepAliveTokens(cmd)).To(gomega.Equal([]string{"--http-keepalive"}))
	g.Expect(cmd).NotTo(gomega.ContainElement("--harakiri"))
}

// An explicit spec is honored verbatim, and the optional flags appear only once
// their fields are set.
func TestBuildUWSGICommand_ExplicitKnobsHonored(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI: &commonv1.UWSGISpec{
			Processes:            4,
			Threads:              8,
			HTTPKeepAlive:        ptr.To(true),
			HTTPKeepAliveTimeout: ptr.To(int32(15)),
			Harakiri:             ptr.To(int32(30)),
		},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("4"))
	g.Expect(valueAfter(cmd, "--threads")).To(gomega.Equal("8"))
	g.Expect(keepAliveTokens(cmd)).To(gomega.Equal([]string{"--http-keepalive=15"}))
	g.Expect(indexOf(cmd, "--log-master")).To(gomega.Equal(indexOf(cmd, "--http-keepalive=15") + 1))
	g.Expect(valueAfter(cmd, "--harakiri")).To(gomega.Equal("30"))

	// A spec that leaves the nil-preserving keep-alive pointer unset keeps the
	// default (true) while its process and thread counts still apply.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Processes: 4, Threads: 8},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(keepAliveTokens(cmd)).To(gomega.Equal([]string{"--http-keepalive"}))
	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("4"))

	// The unset keep-alive pointer resolves true, so a timeout without it still
	// renders as the value of the keep-alive flag.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Processes: 4, Threads: 8, HTTPKeepAliveTimeout: ptr.To(int32(15))},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(keepAliveTokens(cmd)).To(gomega.Equal([]string{"--http-keepalive=15"}))
}

// A spec that reached the builder without the CRD defaults — an adopting
// operator whose spec omits the +kubebuilder:default markers, or an in-process
// caller that never round-trips the API server — must not render
// "--processes 0": a master that forks no worker accepts connections and serves
// none, so the API container stays Running while every probe times out.
func TestBuildUWSGICommand_ZeroCountsFallBackToDefaults(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("2"))
	g.Expect(valueAfter(cmd, "--threads")).To(gomega.Equal("1"))

	// The two counts fall back independently: a spec that sets only one keeps it.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Processes: 4},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("4"))
	g.Expect(valueAfter(cmd, "--threads")).To(gomega.Equal("1"))
}

// A timeout that reached the builder past the Minimum=2 marker renders the bare
// flag: uWSGI reads --http-keepalive=1 as the bare flag, =0 as keep-alive off,
// and exits on a negative value. Only keystone's webhook re-checks this bound,
// so the other operators reach the builder with the CRD schema as their only
// guard.
func TestBuildUWSGICommand_KeepAliveTimeoutLowerBound(t *testing.T) {
	cases := []struct {
		name    string
		timeout int32
		want    []string
	}{
		{name: "two is the lowest bound", timeout: 2, want: []string{"--http-keepalive=2"}},
		{name: "one", timeout: 1, want: []string{"--http-keepalive"}},
		{name: "zero", timeout: 0, want: []string{"--http-keepalive"}},
		{name: "negative", timeout: -1, want: []string{"--http-keepalive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			cmd := BuildUWSGICommand(UWSGICommandParams{
				UWSGI:        &commonv1.UWSGISpec{HTTPKeepAliveTimeout: ptr.To(tc.timeout)},
				Bind:         ":5000",
				WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
			})

			g.Expect(keepAliveTokens(cmd)).To(gomega.Equal(tc.want))
		})
	}
}

// Harakiri gets the same zero guard as the counts: "--harakiri 0" would mean no
// per-request kill at all, so a zero cap drops the flag. Only keystone's webhook
// re-checks the Minimum=1 marker, so the other operators reach the builder with
// the CRD schema as their only guard.
func TestBuildUWSGICommand_ZeroHarakiriDropsFlag(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Harakiri: ptr.To(int32(0))},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(cmd).NotTo(gomega.ContainElement("--harakiri"))

	// The harakiri guard is independent of the keep-alive timeout guard: a
	// positive harakiri alongside a timeout below 2 still lands.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI: &commonv1.UWSGISpec{
			HTTPKeepAliveTimeout: ptr.To(int32(-1)),
			Harakiri:             ptr.To(int32(30)),
		},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(keepAliveTokens(cmd)).To(gomega.Equal([]string{"--http-keepalive"}))
	g.Expect(valueAfter(cmd, "--harakiri")).To(gomega.Equal("30"))
}

// A CR that bypassed the admission webhook can carry a timeout with keep-alive
// disabled. The timeout has no meaning without its parent feature, so no
// keep-alive token is rendered, while the request logging stays on.
func TestBuildUWSGICommand_KeepAliveFalseDropsKeepAliveToken(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI: &commonv1.UWSGISpec{
			Processes:            2,
			Threads:              1,
			HTTPKeepAlive:        ptr.To(false),
			HTTPKeepAliveTimeout: ptr.To(int32(15)),
		},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(keepAliveTokens(cmd)).To(gomega.BeEmpty())
	g.Expect(cmd).To(gomega.ContainElement("--log-master"))
	g.Expect(valueAfter(cmd, "--log-format")).To(gomega.Equal(UWSGILogFormat))
}

// The harakiri cap closes the command: a service's trailing tokens must not end
// up between the flag and its value.
func TestBuildUWSGICommand_HarakiriIsLast(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Processes: 2, Threads: 1, Harakiri: ptr.To(int32(30))},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
		TrailingArgs: []string{"--pyargv=--config-dir=/etc/svc/svc.conf.d/"},
	})

	g.Expect(cmd[len(cmd)-3:]).To(gomega.Equal([]string{"--pyargv=--config-dir=/etc/svc/svc.conf.d/", "--harakiri", "30"}))
}

// The two raw-token slices land at their documented anchors: ArgsAfterHTTP
// directly behind the bind (ahead of the keep-alive block) and TrailingArgs
// directly behind the thread count.
func TestBuildUWSGICommand_ArgsPlacement(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		Bind:          "127.0.0.1:5000",
		ArgsAfterHTTP: []string{"--buffer-size", "65535"},
		WSGIFilePath:  "/var/lib/openstack/bin/svc-wsgi",
		TrailingArgs:  []string{"--pyargv", "--config-dir /etc/svc/svc.conf.d/"},
	})

	g.Expect(cmd[:6]).To(gomega.Equal([]string{
		"uwsgi", "--http", "127.0.0.1:5000", "--buffer-size", "65535", "--http-keepalive",
	}))

	threads := indexOf(cmd, "--threads")
	g.Expect(threads).To(gomega.BeNumerically(">", 0))
	g.Expect(cmd[threads+2:]).To(gomega.Equal([]string{"--pyargv", "--config-dir /etc/svc/svc.conf.d/"}))
}

// Nil and empty raw-token slices are the same absence: neither may leave a gap
// or an empty token in the command.
func TestBuildUWSGICommand_NilAndEmptySlicesAddNothing(t *testing.T) {
	g := gomega.NewWithT(t)

	nilSlices := BuildUWSGICommand(UWSGICommandParams{
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})
	emptySlices := BuildUWSGICommand(UWSGICommandParams{
		Bind:          ":5000",
		ArgsAfterHTTP: []string{},
		WSGIFilePath:  "/var/lib/openstack/bin/svc-wsgi",
		TrailingArgs:  []string{},
	})

	g.Expect(emptySlices).To(gomega.Equal(nilSlices))
}

// SIGTERM starts uWSGI's graceful shutdown in every API container, so every
// params value renders the three shutdown flags in one place, and none
// switches them off. Each call of UWSGIGracefulShutdownArgs hands out a slice
// of its own: a caller that changes its copy changes no later command.
func TestBuildUWSGICommand_GracefulShutdownFlags(t *testing.T) {
	t.Run("nil spec", func(t *testing.T) {
		expectGracefulShutdownFlags(t, BuildUWSGICommand(UWSGICommandParams{
			Bind:         ":5000",
			WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
		}))
	})

	t.Run("full spec with module and raw tokens", func(t *testing.T) {
		g := gomega.NewWithT(t)

		cmd := BuildUWSGICommand(UWSGICommandParams{
			UWSGI: &commonv1.UWSGISpec{
				Processes:            4,
				Threads:              8,
				HTTPKeepAlive:        ptr.To(true),
				HTTPKeepAliveTimeout: ptr.To(int32(15)),
				Harakiri:             ptr.To(int32(30)),
			},
			Bind:          ":9311",
			ArgsAfterHTTP: []string{"--buffer-size", "65535"},
			Module:        "barbican.wsgi.api:application",
			TrailingArgs:  []string{"--pyargv", "--config-dir /etc/svc/svc.conf.d/"},
		})

		expectGracefulShutdownFlags(t, cmd)
		g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("4"))
		g.Expect(cmd[len(cmd)-2:]).To(gomega.Equal([]string{"--harakiri", "30"}))
	})

	t.Run("empty raw-token slices", func(t *testing.T) {
		expectGracefulShutdownFlags(t, BuildUWSGICommand(UWSGICommandParams{
			Bind:          ":5000",
			ArgsAfterHTTP: []string{},
			WSGIFilePath:  "/var/lib/openstack/bin/svc-wsgi",
			TrailingArgs:  []string{},
		}))
	})

	t.Run("new slice on every call", func(t *testing.T) {
		g := gomega.NewWithT(t)

		args := UWSGIGracefulShutdownArgs()
		g.Expect(args).To(gomega.Equal(gracefulShutdownTokens))

		args[0] = "--mutated"

		g.Expect(UWSGIGracefulShutdownArgs()).To(gomega.Equal(gracefulShutdownTokens))
		expectGracefulShutdownFlags(t, BuildUWSGICommand(UWSGICommandParams{
			Bind:         ":5000",
			WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
		}))
	})
}

// The launch mode is a single flag with a value behind it: --module for a
// service image that ships an importable WSGI application, --wsgi-file for one
// that ships an entry script. Params that set both fall to --module, params that
// set neither get neither flag, and the wsgi-file shape the existing callers
// pass renders token-for-token as it did before --module existed.
func TestBuildUWSGICommand_LaunchMode(t *testing.T) {
	cases := []struct {
		name   string
		params UWSGICommandParams
		want   []string
	}{
		{
			name: "module only",
			params: UWSGICommandParams{
				Bind:   ":9311",
				Module: "barbican.wsgi.api:application",
			},
			want: []string{
				"uwsgi",
				"--http", ":9311",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--module", "barbican.wsgi.api:application",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
			},
		},
		{
			// Byte-identity pin for the shape the three existing callers pass: a
			// zero-value Module must leave their command untouched.
			name: "wsgi file only",
			params: UWSGICommandParams{
				Bind:         ":5000",
				WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
				TrailingArgs: []string{"--pyargv=--config-dir=/etc/svc/svc.conf.d/"},
			},
			want: []string{
				"uwsgi",
				"--http", ":5000",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--wsgi-file", "/var/lib/openstack/bin/svc-wsgi",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
				"--pyargv=--config-dir=/etc/svc/svc.conf.d/",
			},
		},
		{
			// Neither flag may reach the container with an empty value: uWSGI would
			// start a master that loads no application and answers every request 500.
			name:   "neither set",
			params: UWSGICommandParams{Bind: ":5000"},
			want: []string{
				"uwsgi",
				"--http", ":5000",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
			},
		},
		{
			// The field docs allow only one of the two, so this pins what a params
			// struct that ignores the contract renders: --module wins, --wsgi-file is
			// dropped rather than emitted alongside it.
			name: "both set",
			params: UWSGICommandParams{
				Bind:         ":9311",
				WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
				Module:       "barbican.wsgi.api:application",
			},
			want: []string{
				"uwsgi",
				"--http", ":9311",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--module", "barbican.wsgi.api:application",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(BuildUWSGICommand(tc.params)).To(gomega.Equal(tc.want))
		})
	}
}

// The four parameterizations the service operators pass today, pinned
// token-for-token so a change to the emission order shows up here before it
// reaches a running API container.
func TestBuildUWSGICommand_RealParameterizations(t *testing.T) {
	cases := []struct {
		name   string
		params UWSGICommandParams
		want   []string
	}{
		{
			name: "keystone default",
			params: UWSGICommandParams{
				Bind:         ":5000",
				WSGIFilePath: "/var/lib/openstack/bin/keystone-wsgi-public",
				TrailingArgs: []string{"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/"},
			},
			want: []string{
				"uwsgi",
				"--http", ":5000",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--wsgi-file", "/var/lib/openstack/bin/keystone-wsgi-public",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
				"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/",
			},
		},
		{
			// With federation active keystone binds to loopback so only the
			// header-stripping sidecar reaches uWSGI, and raises the buffer size
			// for the larger claim headers.
			name: "keystone federation",
			params: UWSGICommandParams{
				Bind:          "127.0.0.1:5000",
				ArgsAfterHTTP: []string{"--buffer-size", "65535"},
				WSGIFilePath:  "/var/lib/openstack/bin/keystone-wsgi-public",
				TrailingArgs:  []string{"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/"},
			},
			want: []string{
				"uwsgi",
				"--http", "127.0.0.1:5000",
				"--buffer-size", "65535",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--wsgi-file", "/var/lib/openstack/bin/keystone-wsgi-public",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
				"--pyargv=--config-dir=/etc/keystone/keystone.conf.d/",
			},
		},
		{
			// Glance streams image uploads and downloads chunked, and hands its
			// two config-dir roots to the entry script as a single --pyargv value.
			name: "glance",
			params: UWSGICommandParams{
				Bind:          ":9292",
				ArgsAfterHTTP: []string{"--http-auto-chunked", "--http-chunked-input"},
				WSGIFilePath:  "/var/lib/openstack/bin/glance-wsgi-api",
				TrailingArgs: []string{
					"--pyargv",
					"--config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/",
				},
			},
			want: []string{
				"uwsgi",
				"--http", ":9292",
				"--http-auto-chunked",
				"--http-chunked-input",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--wsgi-file", "/var/lib/openstack/bin/glance-wsgi-api",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
				"--pyargv",
				"--config-dir /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/",
			},
		},
		{
			// Placement passes its config location in an env var, so it needs
			// neither extra transport flags nor a --pyargv.
			name: "placement",
			params: UWSGICommandParams{
				Bind:         ":8778",
				WSGIFilePath: "/var/lib/openstack/bin/placement-api",
			},
			want: []string{
				"uwsgi",
				"--http", ":8778",
				"--http-keepalive",
				"--log-master",
				"--log-format", UWSGILogFormat,
				"--wsgi-file", "/var/lib/openstack/bin/placement-api",
				"--master",
				"--lazy-apps",
				"--need-app",
				"--hook-master-start", "unix_signal:15 gracefully_kill_them_all",
				"--shutdown-sockets",
				"--threaded-logger",
				"--processes", "2",
				"--threads", "1",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(BuildUWSGICommand(tc.params)).To(gomega.Equal(tc.want))
		})
	}
}
