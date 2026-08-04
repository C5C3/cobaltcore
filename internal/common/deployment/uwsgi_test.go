// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	commonv1 "github.com/c5c3/forge/internal/common/types"
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

// A CR without a uwsgi block must still get the documented command: the shared
// defaults, keep-alive on with no timeout, and no harakiri cap at all (the
// builder injects no hidden default for the optional flags).
func TestBuildUWSGICommand_NilSpecDefaults(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{Bind: ":5000", WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi"})

	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("2"))
	g.Expect(valueAfter(cmd, "--threads")).To(gomega.Equal("1"))
	g.Expect(cmd).To(gomega.ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive-timeout"))
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
	g.Expect(valueAfter(cmd, "--http-keepalive-timeout")).To(gomega.Equal("15"))
	g.Expect(valueAfter(cmd, "--harakiri")).To(gomega.Equal("30"))

	// A spec that leaves the nil-preserving keep-alive pointer unset keeps the
	// default (true) while its process and thread counts still apply.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI:        &commonv1.UWSGISpec{Processes: 4, Threads: 8},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(cmd).To(gomega.ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive-timeout"))
	g.Expect(valueAfter(cmd, "--processes")).To(gomega.Equal("4"))
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

// The optional caps get the same zero guard as the counts: a spec that reached
// the builder past the Minimum=1 markers must not render
// "--http-keepalive-timeout 0" (the unbounded idle timeout the field docs
// reject) or "--harakiri 0" (no per-request kill at all). Only keystone's
// webhook re-checks these two, so glance and placement reach the builder with
// the CRD schema as their only guard.
func TestBuildUWSGICommand_ZeroCapsDropTheirFlags(t *testing.T) {
	g := gomega.NewWithT(t)

	cmd := BuildUWSGICommand(UWSGICommandParams{
		UWSGI: &commonv1.UWSGISpec{
			HTTPKeepAliveTimeout: ptr.To(int32(0)),
			Harakiri:             ptr.To(int32(0)),
		},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(cmd).To(gomega.ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive-timeout"))
	g.Expect(cmd).NotTo(gomega.ContainElement("--harakiri"))

	// A negative cap is dropped on the same arm, and the two caps are guarded
	// independently: a positive harakiri alongside a rejected timeout still lands.
	cmd = BuildUWSGICommand(UWSGICommandParams{
		UWSGI: &commonv1.UWSGISpec{
			HTTPKeepAliveTimeout: ptr.To(int32(-1)),
			Harakiri:             ptr.To(int32(30)),
		},
		Bind:         ":5000",
		WSGIFilePath: "/var/lib/openstack/bin/svc-wsgi",
	})

	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive-timeout"))
	g.Expect(valueAfter(cmd, "--harakiri")).To(gomega.Equal("30"))
}

// A CR that bypassed the admission webhook can carry a timeout with keep-alive
// disabled. The timeout has no meaning without its parent feature, so both
// flags are dropped, while the request logging stays on.
func TestBuildUWSGICommand_KeepAliveFalseDropsBothFlags(t *testing.T) {
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

	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive"))
	g.Expect(cmd).NotTo(gomega.ContainElement("--http-keepalive-timeout"))
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
