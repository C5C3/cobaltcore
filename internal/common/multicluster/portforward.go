// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PortForwardDialer opens a TCP path to a cluster-local Service on a target
// cluster, tunnelled through that cluster's API server over the pods/portforward
// subresource and under the credentials the cluster is registered with.
//
// It exists for the connections the service proxy cannot carry: the ones whose
// TLS has to terminate at the workload. An operator that provisions OpenBao
// dials https://<name>.<namespace>.svc:8200 and verifies the answer against that
// instance's own CA bundle, and that URL is the SAN the instance's certificate
// carries. Routed through the API server's services/proxy, TLS would terminate
// at the API server instead, and both the trust chain and the SAN would be
// wrong.
//
// So only the TCP path moves. The caller keeps building the Service URL it
// always built, the handshake still runs end to end against the workload's
// certificate, and the SNI is still the Service DNS name; the dialer merely
// hands the bytes to a port-forward stream instead of to a resolver that cannot
// answer for a name living on another cluster.
type PortForwardDialer struct {
	apiServer *url.URL
	reader    client.Reader
	transport http.RoundTripper
	upgrader  spdy.Upgrader

	// requestID numbers the stream pairs this dialer opens. Each connection
	// carries exactly one pair, so the value only has to be stable across the two
	// streams of the same connection; it counts up rather than staying constant so
	// two tunnels can be told apart in an API server audit log.
	requestID atomic.Uint64
}

// NewPortForwardDialer builds the dialer for the cluster cfg addresses. The
// Service and its EndpointSlices are read through reader, which is meant to be
// the target cluster's uncached reader: a pod that moved must not be dialled
// from a cache that still names the old one.
func NewPortForwardDialer(cfg *rest.Config, reader client.Reader) (*PortForwardDialer, error) {
	if cfg == nil {
		return nil, errors.New("building a port-forward dialer: the target cluster has no REST config")
	}
	if reader == nil {
		return nil, errors.New("building a port-forward dialer: the target cluster has no reader")
	}

	apiServer, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing the API server URL %q of the target cluster: %w", cfg.Host, err)
	}
	if apiServer.Host == "" {
		return nil, fmt.Errorf("the API server URL %q of the target cluster names no host", cfg.Host)
	}

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the SPDY transport for the target cluster API server: %w", err)
	}

	return &PortForwardDialer{apiServer: apiServer, reader: reader, transport: transport, upgrader: upgrader}, nil
}

// DialContext opens a connection to addr, for an http.Transport to use in place
// of its own dialer.
//
// A cluster-local Service address — <name>.<namespace>.svc:<port> or its fully
// qualified form — is tunnelled through the target cluster's API server.
// Anything else is dialled directly: a brownfield CR may name a server that is
// not a Service of that cluster at all, and such an address resolves from here
// perfectly well.
//
// The returned connection carries raw bytes in both directions, so whatever the
// caller layers on top of it runs against the workload itself.
func (d *PortForwardDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	name, namespace, isService := splitServiceHost(host)
	if !isService {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	// A bit size of 32 bounds the value to the int32 a Service port is.
	servicePort, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("dialling %q: %q is not a port number: %w", addr, port, err)
	}

	targetPort, err := d.targetPort(ctx, namespace, name, int32(servicePort))
	if err != nil {
		return nil, err
	}
	pod, err := d.readyPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return d.forward(ctx, namespace, pod, targetPort, addr)
}

// targetPort maps the port the caller dialled onto the container port behind it.
// A port-forward reaches a POD, and a Service port is only the name the client
// dialled, so the two have to be told apart before a stream can ask for one.
//
// A named targetPort resolves to the Service port instead of to the container
// port it points at: resolving the name would mean reading the pod spec on every
// dial, and every Service this dialer is pointed at is operator-projected with a
// numeric target. A Service whose targetPort is a name that does not match its
// port number is therefore not supported.
func (d *PortForwardDialer) targetPort(ctx context.Context, namespace, name string, port int32) (int32, error) {
	var svc corev1.Service
	if err := d.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		return 0, fmt.Errorf("reading Service %s/%s on the target cluster: %w", namespace, name, err)
	}

	for _, servicePort := range svc.Spec.Ports {
		if servicePort.Port != port {
			continue
		}
		if servicePort.TargetPort.Type == intstr.Int && servicePort.TargetPort.IntVal != 0 {
			return servicePort.TargetPort.IntVal, nil
		}
		return servicePort.Port, nil
	}
	return 0, fmt.Errorf("service %s/%s serves no port %d", namespace, name, port)
}

// readyPod names the pod a connection to the Service is forwarded to. The
// EndpointSlices are the same list kube-proxy load balances over, so a pod that
// is not ready — mid-rollout, or failing its probes — is never dialled.
//
// The first ready endpoint wins rather than a random one. A dial that fails is
// retried by the caller's next reconcile anyway, and choosing deterministically
// keeps a failing tunnel reproducible instead of intermittent.
func (d *PortForwardDialer) readyPod(ctx context.Context, namespace, service string) (string, error) {
	var slices discoveryv1.EndpointSliceList
	if err := d.reader.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: service}); err != nil {
		return "", fmt.Errorf("listing the EndpointSlices of Service %s/%s on the target cluster: %w",
			namespace, service, err)
	}

	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			// An unset Ready is the unknown state the API contract tells consumers to
			// read as ready; only an explicit false is a pod to pass over.
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			// An endpoint that names no pod cannot be port-forwarded to: the
			// subresource is on the pod, not on the address.
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Name == "" {
				continue
			}
			return endpoint.TargetRef.Name, nil
		}
	}
	return "", fmt.Errorf("no ready endpoints for service %s/%s", namespace, service)
}

// forward opens the port-forward stream pair to pod and presents its data stream
// as a connection. addr is carried along only to name the far end of it.
func (d *PortForwardDialer) forward(
	ctx context.Context, namespace, pod string, port int32, addr string,
) (net.Conn, error) {
	target := &url.URL{
		Scheme: d.apiServer.Scheme,
		Host:   d.apiServer.Host,
		Path: fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward",
			strings.TrimSuffix(d.apiServer.Path, "/"), namespace, pod),
	}
	dialer := spdy.NewDialerForStreaming(d.upgrader,
		&http.Client{Transport: &contextRoundTripper{base: d.transport, ctx: ctx}},
		http.MethodPost, target)
	conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("opening a port-forward to pod %s/%s on the target cluster: %w", namespace, pod, err)
	}

	headers := http.Header{}
	headers.Set(corev1.PortHeader, strconv.Itoa(int(port)))
	headers.Set(corev1.PortForwardRequestIDHeader, strconv.FormatUint(d.requestID.Add(1), 10))

	// The error stream is created first, the order kubectl uses: it is where the
	// API server reports a forward it cannot serve, and a data stream opened
	// before it would race that report.
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	errorStream, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("creating the error stream of the port-forward to pod %s/%s: %w", namespace, pod, err)
	}
	// Half-close: this side never writes on the error stream, and an open write
	// half would hold the stream open past the end of the forward.
	_ = errorStream.Close()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := conn.CreateStream(headers)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("creating the data stream of the port-forward to pod %s/%s: %w", namespace, pod, err)
	}

	tunnel := &portForwardConn{
		conn:   conn,
		stream: dataStream,
		local:  tunnelAddr(namespace + "/" + pod),
		remote: tunnelAddr(addr),
	}
	go tunnel.watchErrorStream(errorStream)
	return tunnel, nil
}

// contextRoundTripper carries a dial's context into the SPDY upgrade request.
// spdy.NewDialer builds that request itself and httpstream.Dialer takes no
// context, so wrapping the transport is the only place the caller's deadline can
// reach the upgrade. Without it, a reconcile that gives up leaves the upgrade
// hanging against the API server.
//
// The context bounds the upgrade alone. The SPDY round tripper uses it to dial
// and to hand shake and then lets go of it, so the tunnel outlives the dial that
// opened it, which is what an http.Transport pooling the connection expects.
type contextRoundTripper struct {
	base http.RoundTripper
	ctx  context.Context
}

func (t *contextRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

var _ net.Conn = (*portForwardConn)(nil)

// portForwardConn is the data stream of one port-forward, presented as a
// net.Conn so an ordinary http.Transport can dial through it.
type portForwardConn struct {
	conn   httpstream.Connection
	stream httpstream.Stream
	local  tunnelAddr
	remote tunnelAddr

	mu         sync.Mutex
	forwardErr error
}

// watchErrorStream drains the forward's error stream. The API server writes to
// it when it cannot serve the forward at all — the pod listening on no such
// port, above all — and then says nothing further, so the data stream alone
// would just look empty. What it reports is raised on the next Read or Write,
// which is where the caller is looking.
//
// A read error is deliberately not raised: it says the connection itself went
// away, which the data stream reports with a real network error, and raising it
// from here too would only mask the better one.
func (c *portForwardConn) watchErrorStream(errorStream httpstream.Stream) {
	defer c.conn.RemoveStreams(errorStream)

	if message, _ := io.ReadAll(errorStream); len(message) > 0 {
		c.setForwardErr(fmt.Errorf("the port-forward was refused: %s", strings.TrimSpace(string(message))))
	}
}

// Read reads from the data stream. A refusal the error stream reported wins over
// what the data stream says, which on a refused forward is an unexplained EOF.
func (c *portForwardConn) Read(p []byte) (int, error) {
	n, err := c.stream.Read(p)
	if forwardErr := c.err(); forwardErr != nil {
		return n, forwardErr
	}
	return n, err
}

// Write writes to the data stream, refusing to write into a forward the API
// server already said it cannot serve.
func (c *portForwardConn) Write(p []byte) (int, error) {
	if forwardErr := c.err(); forwardErr != nil {
		return 0, forwardErr
	}
	return c.stream.Write(p)
}

// Close closes the data stream and the SPDY connection under it. That connection
// carries this one forward and nothing else, so there is nothing left to hold it
// open for.
func (c *portForwardConn) Close() error {
	return errors.Join(c.stream.Close(), c.conn.Close())
}

func (c *portForwardConn) LocalAddr() net.Addr  { return c.local }
func (c *portForwardConn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline and the two halves below are accepted and ignored: a SPDY stream
// carries no deadline to set. Nothing is lost by that here, because every call
// this tunnel carries is already bounded from above — by the caller's context
// and by the HTTP client timeout, both of which end in a Close — while refusing
// them with an error would break an http.Transport that sets a deadline on a
// connection it dialled.
func (c *portForwardConn) SetDeadline(time.Time) error      { return nil }
func (c *portForwardConn) SetReadDeadline(time.Time) error  { return nil }
func (c *portForwardConn) SetWriteDeadline(time.Time) error { return nil }

// setForwardErr records the first refusal the error stream reported.
func (c *portForwardConn) setForwardErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.forwardErr == nil {
		c.forwardErr = err
	}
}

// err returns the recorded refusal, if the error stream reported one.
func (c *portForwardConn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.forwardErr
}

// tunnelAddr names one end of a tunnelled connection. A port-forward has no
// socket address to report, so each end is named by what it stands for: the
// Service address the caller dialled, and the pod it was forwarded to.
type tunnelAddr string

func (a tunnelAddr) Network() string { return "portforward" }
func (a tunnelAddr) String() string  { return string(a) }
