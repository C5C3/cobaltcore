// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
	spdystream "k8s.io/streaming/pkg/httpstream/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testTunnelNamespace = "openstack"
	testTunnelService   = "openbao"
	testTunnelPod       = "openbao-0"

	// testTunnelServicePort is the port the caller dials; testTunnelTargetPort is
	// the container port behind it. They differ on purpose, so a stream asking for
	// the dialled port instead of the resolved one fails the assertion.
	testTunnelServicePort = 8200
	testTunnelTargetPort  = 8300
)

// portForwardServer stands in for a target cluster's API server serving the
// pods/portforward subresource: it upgrades the request to SPDY, accepts the
// error/data stream pair, and echoes back whatever is written to the data
// stream. It records the request paths and the port headers it saw, so a test
// can assert which pod was addressed and which port the stream asked for.
type portForwardServer struct {
	*httptest.Server

	// refusal is what the server writes on the error stream instead of serving
	// the forward, which is how a real API server reports a forward it cannot
	// carry — the pod listening on no such port, above all. Empty serves the
	// forward.
	refusal string

	mu    sync.Mutex
	paths []string
	ports []string
}

func newPortForwardServer(t *testing.T) *portForwardServer {
	t.Helper()
	return newRefusingPortForwardServer(t, "")
}

// newRefusingPortForwardServer is newPortForwardServer with an error-stream
// message: the data stream is accepted and then left silent, exactly as the API
// server leaves it once it has said it cannot serve the forward.
func newRefusingPortForwardServer(t *testing.T, refusal string) *portForwardServer {
	t.Helper()

	s := &portForwardServer{refusal: refusal}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.record(&s.paths, req.URL.Path)

		if _, err := httpstream.Handshake(req, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
			return
		}

		conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, req,
			func(stream httpstream.Stream, _ <-chan struct{}) error {
				s.record(&s.ports, stream.Headers().Get(corev1.PortHeader))
				if stream.Headers().Get(corev1.StreamType) != corev1.StreamTypeData {
					if s.refusal != "" {
						go func() {
							_, _ = io.WriteString(stream, s.refusal)
							_ = stream.Close()
						}()
					}
					return nil
				}
				// A served forward echoes; a refused one is closed without a byte,
				// which is the unexplained EOF the error stream's message explains.
				if s.refusal != "" {
					go func() { _ = stream.Close() }()
					return nil
				}
				go func() { _, _ = io.Copy(stream, stream) }()
				return nil
			})
		if conn == nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// The handler owns the hijacked connection, so it has to outlive the
		// request and end only when the client hangs up.
		<-conn.CloseChan()
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *portForwardServer) record(into *[]string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*into = append(*into, value)
}

func (s *portForwardServer) recorded(from *[]string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), *from...)
}

// tunnelReader builds the target cluster's reader over the given objects.
func tunnelReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the target cluster scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// tunnelService is the Service the tunnel resolves, mapping the dialled port
// onto a different container port.
func tunnelService() *corev1.Service {
	return tunnelServiceTargeting(intstr.FromInt32(testTunnelTargetPort))
}

// tunnelServiceTargeting is tunnelService with the targetPort of the caller's
// choosing, so a test can drive the resolution of a target the dialer does not
// follow to a container port.
func tunnelServiceTargeting(targetPort intstr.IntOrString) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: testTunnelNamespace, Name: testTunnelService},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Port:       testTunnelServicePort,
			TargetPort: targetPort,
		}}},
	}
}

// tunnelEndpointSlice is the Service's EndpointSlice, backed by one pod whose
// readiness the caller decides.
func tunnelEndpointSlice(ready bool) *discoveryv1.EndpointSlice {
	return tunnelEndpointSliceOf(tunnelEndpoint(ptr.To(ready), testTunnelPod))
}

// tunnelEndpointSliceOf is the Service's EndpointSlice over the endpoints the
// caller assembles, in the order it lists them.
func tunnelEndpointSliceOf(endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testTunnelNamespace,
			Name:      testTunnelService + "-abcde",
			Labels:    map[string]string{discoveryv1.LabelServiceName: testTunnelService},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

// tunnelEndpoint is one endpoint of that slice. A nil ready leaves the condition
// unset, which the EndpointSlice contract tells consumers to read as ready; an
// empty pod leaves the TargetRef nil, which is an endpoint no port-forward can
// address because the subresource is on the pod and not on the address.
func tunnelEndpoint(ready *bool, pod string) discoveryv1.Endpoint {
	endpoint := discoveryv1.Endpoint{
		Addresses:  []string{"10.244.0.7"},
		Conditions: discoveryv1.EndpointConditions{Ready: ready},
	}
	if pod != "" {
		endpoint.TargetRef = &corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: testTunnelNamespace,
			Name:      pod,
		}
	}
	return endpoint
}

// tunnelServiceAddr is the address a caller dialling the Service produces.
func tunnelServiceAddr() string {
	return "openbao.openstack.svc:8200"
}

// TestPortForwardDialerTunnelsAServiceAddress is the happy path: a cluster-local
// Service address resolves to a ready pod, the stream asks for the container
// port behind the Service port, and the connection carries bytes both ways.
func TestPortForwardDialerTunnelsAServiceAddress(t *testing.T) {
	g := gomega.NewWithT(t)

	server := newPortForwardServer(t)
	dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
		tunnelReader(t, tunnelService(), tunnelEndpointSlice(true)))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	conn, err := dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("ping"))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	echoed := make([]byte, 4)
	_, err = io.ReadFull(conn, echoed)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(echoed)).To(gomega.Equal("ping"))

	g.Expect(server.recorded(&server.paths)).To(gomega.Equal(
		[]string{"/api/v1/namespaces/openstack/pods/openbao-0/portforward"}),
		"the tunnel addresses the pod behind the Service, not the Service")
	g.Expect(server.recorded(&server.ports)).To(gomega.Equal([]string{"8300", "8300"}),
		"both streams ask for the container port the Service targets, not the port that was dialled")
}

// TestPortForwardDialerWithoutAReadyEndpoint covers the two states in which
// there is no pod to forward to. Both report the same message, because both mean
// the same thing to the caller: the workload is not up yet, and the CR waits
// rather than failing.
func TestPortForwardDialerWithoutAReadyEndpoint(t *testing.T) {
	tests := []struct {
		name string
		objs []client.Object
	}{
		{
			name: "no EndpointSlice at all",
			objs: []client.Object{tunnelService()},
		},
		{
			name: "the only endpoint is not ready",
			objs: []client.Object{tunnelService(), tunnelEndpointSlice(false)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			server := newPortForwardServer(t)
			dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL}, tunnelReader(t, tc.objs...))
			g.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
			g.Expect(err).To(gomega.MatchError("no ready endpoints for service openstack/openbao"))
			g.Expect(server.recorded(&server.paths)).To(gomega.BeEmpty(),
				"nothing is asked of the API server before a pod is known")
		})
	}
}

// TestPortForwardDialerSurfacesAForbiddenUpgrade pins the failure an operator
// meets when its kubeconfig for the target cluster carries no pods/portforward
// grant. The API server refuses the upgrade, and the message naming the missing
// verb has to reach the caller — it is the whole diagnosis, and it ends up in a
// status condition.
func TestPortForwardDialerSurfacesAForbiddenUpgrade(t *testing.T) {
	g := gomega.NewWithT(t)

	const forbidden = `pods "openbao-0" is forbidden: User "system:serviceaccount:c5c3-system:barbican-operator" ` +
		`cannot create resource "pods/portforward" in API group "" in the namespace "openstack"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, forbidden)
	}))
	t.Cleanup(server.Close)

	dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
		tunnelReader(t, tunnelService(), tunnelEndpointSlice(true)))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(forbidden)))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("pod openstack/openbao-0")),
		"the pod the tunnel was refused for is named alongside the API server's message")
}

// TestPortForwardDialerPassesThroughANonServiceAddress covers the brownfield
// case: a CR may name an OpenBao server that is no Service of the target cluster
// at all, and that address resolves from here. Tunnelling it would be wrong, so
// it goes to the plain dialer untouched.
func TestPortForwardDialerPassesThroughANonServiceAddress(t *testing.T) {
	g := gomega.NewWithT(t)

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		accepted, acceptErr := listener.Accept()
		if acceptErr == nil {
			_, _ = io.WriteString(accepted, "external")
			_ = accepted.Close()
		}
	}()

	server := newPortForwardServer(t)
	dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
		tunnelReader(t, tunnelService(), tunnelEndpointSlice(true)))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	conn, err := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { _ = conn.Close() }()

	greeting, err := io.ReadAll(conn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(greeting)).To(gomega.Equal("external"))
	g.Expect(server.recorded(&server.paths)).To(gomega.BeEmpty(),
		"an address that is no cluster-local Service never reaches the API server")
}

// TestPortForwardDialerRejectsAnUnusableCluster covers the constructor's error
// paths. A target cluster the operator holds no REST config for cannot be
// tunnelled into, and saying so is what puts the reason on a status condition
// instead of a nil dereference in the reconcile.
func TestPortForwardDialerRejectsAnUnusableCluster(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *rest.Config
		reader  client.Reader
		wantErr string
	}{
		{
			name:    "no REST config",
			reader:  tunnelReader(t),
			wantErr: "the target cluster has no REST config",
		},
		{
			name:    "no reader",
			cfg:     &rest.Config{Host: "https://api.edge-1.example:6443"},
			wantErr: "the target cluster has no reader",
		},
		{
			name:    "a host that names nothing",
			cfg:     &rest.Config{Host: "api.edge-1.example"},
			reader:  tunnelReader(t),
			wantErr: `the API server URL "api.edge-1.example" of the target cluster names no host`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			_, err := NewPortForwardDialer(tc.cfg, tc.reader)
			g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tc.wantErr)))
		})
	}
}

// TestPortForwardDialerOnAServiceWithoutThePort covers a Service that does not
// serve the port the caller dialled, which is a misconfigured CR rather than a
// workload that is still coming up.
func TestPortForwardDialerOnAServiceWithoutThePort(t *testing.T) {
	g := gomega.NewWithT(t)

	server := newPortForwardServer(t)
	dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
		tunnelReader(t, tunnelService(), tunnelEndpointSlice(true)))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = dialer.DialContext(context.Background(), "tcp", "openbao.openstack.svc:9999")
	g.Expect(err).To(gomega.MatchError("service openstack/openbao serves no port 9999"))
}

// TestPortForwardDialerSurfacesARefusedForward is the reason the tunnel watches
// the error stream at all. The API server accepts the upgrade and both streams,
// then reports on the error stream that it cannot carry the forward and says
// nothing further — so the data stream alone only ever looks empty. The refusal
// has to win over that silence on both halves of the connection, because either
// is where the caller is looking.
func TestPortForwardDialerSurfacesARefusedForward(t *testing.T) {
	const refusal = "unable to do port forwarding: socat not found"

	tests := []struct {
		name string
		use  func(conn net.Conn) error
	}{
		{
			name: "on read, over the data stream's unexplained EOF",
			use: func(conn net.Conn) error {
				_, err := io.ReadAll(conn)
				return err
			},
		},
		{
			name: "on write, before anything is sent into it",
			use: func(conn net.Conn) error {
				_, err := conn.Write([]byte("ping"))
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			server := newRefusingPortForwardServer(t, refusal)
			dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
				tunnelReader(t, tunnelService(), tunnelEndpointSlice(true)))
			g.Expect(err).NotTo(gomega.HaveOccurred())

			conn, err := dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
			g.Expect(err).NotTo(gomega.HaveOccurred(), "the forward is refused after the streams are open")
			defer func() { _ = conn.Close() }()

			// The refusal is raised by the goroutine draining the error stream, so
			// the first call may still race it; every later one carries it.
			g.Eventually(func() error { return tc.use(conn) }).
				Should(gomega.MatchError("the port-forward was refused: " + refusal))
		})
	}
}

// TestPortForwardDialerResolvesATargetPortItCannotFollow covers the documented
// fallback: the dialer never reads a pod spec, so a targetPort it cannot resolve
// to a container port resolves to the Service port instead.
func TestPortForwardDialerResolvesATargetPortItCannotFollow(t *testing.T) {
	tests := []struct {
		name       string
		targetPort intstr.IntOrString
	}{
		{name: "a named targetPort", targetPort: intstr.FromString("api")},
		{name: "no targetPort at all", targetPort: intstr.IntOrString{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			server := newPortForwardServer(t)
			dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
				tunnelReader(t, tunnelServiceTargeting(tc.targetPort), tunnelEndpointSlice(true)))
			g.Expect(err).NotTo(gomega.HaveOccurred())

			conn, err := dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer func() { _ = conn.Close() }()

			g.Expect(server.recorded(&server.ports)).To(gomega.Equal([]string{"8200", "8200"}),
				"a targetPort the dialer cannot follow falls back to the Service port")
		})
	}
}

// TestPortForwardDialerPicksTheFirstAddressableReadyEndpoint covers the two
// endpoints readyPod passes over on its way to one it can forward to. Both are
// ordinary states of a live EndpointSlice, and stopping at either would report
// "no ready endpoints" for a Service that has one.
func TestPortForwardDialerPicksTheFirstAddressableReadyEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []discoveryv1.Endpoint
	}{
		{
			name: "an unset Ready is ready",
			endpoints: []discoveryv1.Endpoint{
				tunnelEndpoint(nil, testTunnelPod),
			},
		},
		{
			name: "an endpoint naming no pod is skipped",
			endpoints: []discoveryv1.Endpoint{
				tunnelEndpoint(ptr.To(true), ""),
				tunnelEndpoint(ptr.To(true), testTunnelPod),
			},
		},
		{
			name: "an endpoint that is not ready is skipped",
			endpoints: []discoveryv1.Endpoint{
				tunnelEndpoint(ptr.To(false), "openbao-1"),
				tunnelEndpoint(ptr.To(true), testTunnelPod),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			server := newPortForwardServer(t)
			dialer, err := NewPortForwardDialer(&rest.Config{Host: server.URL},
				tunnelReader(t, tunnelService(), tunnelEndpointSliceOf(tc.endpoints...)))
			g.Expect(err).NotTo(gomega.HaveOccurred())

			conn, err := dialer.DialContext(t.Context(), "tcp", tunnelServiceAddr())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer func() { _ = conn.Close() }()

			g.Expect(server.recorded(&server.paths)).To(gomega.Equal(
				[]string{"/api/v1/namespaces/openstack/pods/openbao-0/portforward"}))
		})
	}
}
