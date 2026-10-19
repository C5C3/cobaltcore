// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

func TestIsGVKAvailable(t *testing.T) {
	g := gomega.NewWithT(t)

	withMapping := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: gatewayv1.GroupVersion.Group, Version: gatewayv1.GroupVersion.Version}})
	withMapping.Add(httpRouteGVK, meta.RESTScopeNamespace)
	ok, err := IsGVKAvailable(withMapping, httpRouteGVK)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ok).To(gomega.BeTrue())

	// A missing mapping (CRD not installed) is a definite "not served".
	empty := meta.NewDefaultRESTMapper(nil)
	ok, err = IsGVKAvailable(empty, httpRouteGVK)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ok).To(gomega.BeFalse())

	// A nil mapper cannot answer, so it is an error a caller fails setup on.
	ok, err = IsGVKAvailable(nil, httpRouteGVK)
	g.Expect(err).To(gomega.MatchError("no RESTMapper to probe for HTTPRoute.gateway.networking.k8s.io/v1"))
	g.Expect(ok).To(gomega.BeFalse())
}

// scriptedMapper answers RESTMapping with errs in order, repeating the last
// entry once the script runs out; a nil entry yields a mapping. calls counts
// the RESTMapping calls, each of which stands for a request to the API server.
type scriptedMapper struct {
	meta.RESTMapper
	errs  []error
	calls int
}

func (m *scriptedMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	err := m.errs[min(m.calls, len(m.errs)-1)]
	m.calls++
	if err != nil {
		return nil, err
	}
	return &meta.RESTMapping{}, nil
}

// zeroProbeBackoff removes the retry sleep for the duration of a test so the
// retry path runs instantly, restoring the production value afterwards.
func zeroProbeBackoff(t *testing.T) {
	t.Helper()
	prev := probeBackoff
	probeBackoff = 0
	t.Cleanup(func() { probeBackoff = prev })
}

func TestIsGVKAvailableNoMatchIsNotAnError(t *testing.T) {
	noMatches := map[string]error{
		"NoKindMatchError":             &meta.NoKindMatchError{},
		"NoResourceMatchError":         &meta.NoResourceMatchError{},
		"wrapped NoKindMatchError":     fmt.Errorf("x: %w", &meta.NoKindMatchError{}),
		"wrapped NoResourceMatchError": fmt.Errorf("x: %w", &meta.NoResourceMatchError{}),
	}
	for name, noMatch := range noMatches {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			mapper := &scriptedMapper{errs: []error{noMatch}}

			ok, err := IsGVKAvailable(mapper, httpRouteGVK)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ok).To(gomega.BeFalse())
			g.Expect(mapper.calls).To(gomega.Equal(1), "a no-match is an answer and is not retried")
		})
	}
}

func TestIsGVKAvailableReportsADiscoveryFailureAfterRetrying(t *testing.T) {
	g := gomega.NewWithT(t)
	zeroProbeBackoff(t)
	mapper := &scriptedMapper{errs: []error{fmt.Errorf("failed to get server groups: %w", context.DeadlineExceeded)}}

	ok, err := IsGVKAvailable(mapper, httpRouteGVK)
	g.Expect(ok).To(gomega.BeFalse())
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
	g.Expect(meta.IsNoMatchError(err)).To(gomega.BeFalse())
	g.Expect(err.Error()).To(gomega.ContainSubstring("probing the RESTMapper for HTTPRoute.gateway.networking.k8s.io/v1"))
	g.Expect(mapper.calls).To(gomega.Equal(probeAttempts))
}

func TestIsGVKAvailableRecoversOnALaterAttempt(t *testing.T) {
	zeroProbeBackoff(t)
	boom := errors.New("boom")

	t.Run("served after a failure", func(t *testing.T) {
		g := gomega.NewWithT(t)
		mapper := &scriptedMapper{errs: []error{boom, nil}}

		ok, err := IsGVKAvailable(mapper, httpRouteGVK)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		g.Expect(mapper.calls).To(gomega.Equal(2))
	})

	t.Run("not served after a failure", func(t *testing.T) {
		g := gomega.NewWithT(t)
		mapper := &scriptedMapper{errs: []error{boom, &meta.NoKindMatchError{}}}

		ok, err := IsGVKAvailable(mapper, httpRouteGVK)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		g.Expect(mapper.calls).To(gomega.Equal(2))
	})
}

// httpRouteGroupVersionPath is the path the dynamic mapper asks for HTTPRoute
// when discovery comes in the non-aggregated format and /apis lists no gateway
// group.
const httpRouteGroupVersionPath = "/apis/gateway.networking.k8s.io/v1"

// The answers the discovery server gives about HTTPRoute.
const (
	routeServed int32 = iota
	routeAbsent
	routeFailing
)

// serveLegacyDiscovery answers in the non-aggregated v1 format: /api and /apis
// list only the groups, so the mapper asks the HTTPRoute group-version path on
// its own, and a 404 there is the "not served" answer.
func serveLegacyDiscovery(_ *testing.T, w http.ResponseWriter, r *http.Request, served bool) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api":
		_, _ = io.WriteString(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
	case "/apis":
		// An empty group list is an error to the mapper, so list one
		// unrelated group.
		_, _ = io.WriteString(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apps",`+
			`"versions":[{"groupVersion":"apps/v1","version":"v1"}],`+
			`"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
	case httpRouteGroupVersionPath:
		if !served {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"APIResourceList","apiVersion":"v1",`+
			`"groupVersion":"gateway.networking.k8s.io/v1","resources":[{"name":"httproutes",`+
			`"singularName":"httproute","namespaced":true,"kind":"HTTPRoute","verbs":["get","list","watch"]}]}`)
	default:
		http.NotFound(w, r)
	}
}

// The groups an aggregated /apis document lists: apps always, since an empty
// group list is an error to the mapper, and the gateway group when the server
// serves HTTPRoute.
const (
	aggregatedAppsGroup = `{"metadata":{"name":"apps"},"versions":[{"version":"v1","freshness":"Current",` +
		`"resources":[{"resource":"deployments","responseKind":{"group":"apps","version":"v1","kind":"Deployment"},` +
		`"scope":"Namespaced","verbs":["get","list","watch"]}]}]}`
	aggregatedGatewayGroup = `{"metadata":{"name":"gateway.networking.k8s.io"},"versions":[{"version":"v1","freshness":"Current",` +
		`"resources":[{"resource":"httproutes","responseKind":{"group":"gateway.networking.k8s.io","version":"v1","kind":"HTTPRoute"},` +
		`"scope":"Namespaced","singularResource":"httproute","verbs":["get","list","watch"]}]}]}`
)

// serveAggregatedDiscovery answers in the aggregated v2 format every supported
// API server serves. /apis carries every group with its resources, so the
// mapper asks no group-version path, and it reports a group /apis leaves out as
// an apiutil.ErrResourceDiscoveryFailed that wraps a NoResourceMatchError. Like
// the API server, it answers in this format only to a client that asks for it.
func serveAggregatedDiscovery(t *testing.T, w http.ResponseWriter, r *http.Request, served bool) {
	if !strings.Contains(r.Header.Get("Accept"), "g=apidiscovery.k8s.io;v=v2") {
		t.Errorf("the discovery request for %s does not accept the aggregated format: %q", r.URL.Path, r.Header.Get("Accept"))
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
		return
	}
	w.Header().Set("Content-Type", "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList")
	switch r.URL.Path {
	case "/api":
		_, _ = io.WriteString(w, `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[]}`)
	case "/apis":
		groups := []string{aggregatedAppsGroup}
		if served {
			groups = append(groups, aggregatedGatewayGroup)
		}
		_, _ = io.WriteString(w, `{"kind":"APIGroupDiscoveryList","apiVersion":"apidiscovery.k8s.io/v2","items":[`+
			strings.Join(groups, ",")+`]}`)
	default:
		http.NotFound(w, r)
	}
}

// newDynamicMapper builds controller-runtime's lazy dynamic mapper, the one
// every operator's manager hands the probe, against the API server at host.
// Each probe gets its own mapper: a mapper caches a successful mapping, which
// would answer the next probe without a request.
func newDynamicMapper(t *testing.T, host string) meta.RESTMapper {
	t.Helper()
	cfg := &rest.Config{Host: host}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("building the HTTP client: %v", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		t.Fatalf("building the dynamic RESTMapper: %v", err)
	}
	return mapper
}

// TestIsGVKAvailableAgainstTheDynamicMapper drives the dynamic mapper against a
// discovery server in both wire formats. The two reach the "not served" answer
// through different error types, and only a no-match lets an operator start on
// a cluster without the kind.
func TestIsGVKAvailableAgainstTheDynamicMapper(t *testing.T) {
	zeroProbeBackoff(t)

	formats := []struct {
		name string
		// answerPath is the request whose response says whether the cluster
		// serves HTTPRoute.
		answerPath string
		serve      func(t *testing.T, w http.ResponseWriter, r *http.Request, served bool)
	}{
		{name: "legacy discovery", answerPath: httpRouteGroupVersionPath, serve: serveLegacyDiscovery},
		{name: "aggregated discovery", answerPath: "/apis", serve: serveAggregatedDiscovery},
	}
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			var mode, answerHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == format.answerPath {
					answerHits.Add(1)
					if mode.Load() == routeFailing {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
				}
				format.serve(t, w, r, mode.Load() == routeServed)
			}))
			t.Cleanup(srv.Close)

			probe := func(t *testing.T, routeMode int32) (bool, error) {
				t.Helper()
				mode.Store(routeMode)
				answerHits.Store(0)
				return IsGVKAvailable(newDynamicMapper(t, srv.URL), httpRouteGVK)
			}

			t.Run("served", func(t *testing.T) {
				g := gomega.NewWithT(t)
				ok, err := probe(t, routeServed)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(ok).To(gomega.BeTrue())
			})

			t.Run("absent", func(t *testing.T) {
				g := gomega.NewWithT(t)
				ok, err := probe(t, routeAbsent)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(ok).To(gomega.BeFalse())
				g.Expect(int(answerHits.Load())).To(gomega.Equal(1), "the not-served answer is not retried")
			})

			t.Run("failing", func(t *testing.T) {
				g := gomega.NewWithT(t)
				ok, err := probe(t, routeFailing)
				g.Expect(ok).To(gomega.BeFalse())
				g.Expect(err).To(gomega.HaveOccurred())
				g.Expect(meta.IsNoMatchError(err)).To(gomega.BeFalse())
				g.Expect(int(answerHits.Load())).To(gomega.Equal(probeAttempts))
			})
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		g := gomega.NewWithT(t)
		// Nothing listens on port 1, so every attempt is refused at once.
		ok, err := IsGVKAvailable(newDynamicMapper(t, "http://127.0.0.1:1"), httpRouteGVK)
		g.Expect(ok).To(gomega.BeFalse())
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(meta.IsNoMatchError(err)).To(gomega.BeFalse())
		g.Expect(err.Error()).To(gomega.ContainSubstring("HTTPRoute.gateway.networking.k8s.io/v1"))
	})
}

func testGatewaySpec() *commonv1.GatewaySpec {
	return &commonv1.GatewaySpec{
		Hostname: "keystone.example.com",
		ParentRef: commonv1.GatewayParentRefSpec{
			Name:        "openstack-gw",
			Namespace:   "gateways",
			SectionName: "https",
		},
	}
}

func TestBuildHTTPRoute(t *testing.T) {
	g := gomega.NewWithT(t)

	route := BuildHTTPRoute(testGatewaySpec(), RouteParams{
		Name:           "ks",
		Namespace:      "openstack",
		Labels:         map[string]string{"app.kubernetes.io/name": "keystone"},
		BackendService: "ks",
		BackendPort:    5000,
	})

	g.Expect(route.Name).To(gomega.Equal("ks"))
	g.Expect(route.Namespace).To(gomega.Equal("openstack"))
	g.Expect(route.Spec.ParentRefs).To(gomega.HaveLen(1))
	g.Expect(route.Spec.ParentRefs[0].Name).To(gomega.Equal(gatewayv1.ObjectName("openstack-gw")))
	g.Expect(route.Spec.ParentRefs[0].Namespace).To(gomega.HaveValue(gomega.Equal(gatewayv1.Namespace("gateways"))))
	g.Expect(route.Spec.ParentRefs[0].SectionName).To(gomega.HaveValue(gomega.Equal(gatewayv1.SectionName("https"))))
	g.Expect(route.Spec.Hostnames).To(gomega.ConsistOf(gatewayv1.Hostname("keystone.example.com")))

	rule := route.Spec.Rules[0]
	g.Expect(rule.Matches[0].Path.Value).To(gomega.HaveValue(gomega.Equal("/")),
		"an empty spec.gateway.path must normalize to /")
	backend := rule.BackendRefs[0].BackendObjectReference
	g.Expect(backend.Name).To(gomega.Equal(gatewayv1.ObjectName("ks")))
	g.Expect(backend.Port).To(gomega.HaveValue(gomega.Equal(gatewayv1.PortNumber(5000))))
}

// A path without a leading slash would produce an HTTPRoute the Gateway
// controller rejects; the builder must normalize it.
func TestBuildHTTPRoute_NormalizesPath(t *testing.T) {
	g := gomega.NewWithT(t)

	spec := testGatewaySpec()
	spec.Path = "identity"
	route := BuildHTTPRoute(spec, RouteParams{Name: "ks", Namespace: "ns", BackendService: "ks", BackendPort: 5000})

	g.Expect(route.Spec.Rules[0].Matches[0].Path.Value).To(gomega.HaveValue(gomega.Equal("/identity")))
}

func TestBuildHTTPRoute_PassesAnnotationsThrough(t *testing.T) {
	g := gomega.NewWithT(t)

	spec := testGatewaySpec()
	spec.Annotations = map[string]string{"konghq.com/plugins": "rate-limit"}
	route := BuildHTTPRoute(spec, RouteParams{Name: "ks", Namespace: "ns", BackendService: "ks", BackendPort: 5000})

	g.Expect(route.Annotations).To(gomega.HaveKeyWithValue("konghq.com/plugins", "rate-limit"))
}

// Operators that are fine on the gateway implementation's default must keep
// rendering a route without a timeouts stanza.
func TestBuildHTTPRoute_NoTimeoutsByDefault(t *testing.T) {
	g := gomega.NewWithT(t)

	route := BuildHTTPRoute(testGatewaySpec(), RouteParams{Name: "ks", Namespace: "ns", BackendService: "ks", BackendPort: 5000})

	g.Expect(route.Spec.Rules[0].Timeouts).To(gomega.BeNil(),
		"a nil RequestTimeout must not render a timeouts stanza")
}

func TestBuildHTTPRoute_RequestTimeoutRendered(t *testing.T) {
	g := gomega.NewWithT(t)

	route := BuildHTTPRoute(testGatewaySpec(), RouteParams{
		Name:           "gl",
		Namespace:      "ns",
		BackendService: "gl",
		BackendPort:    9292,
		RequestTimeout: ptr.To(gatewayv1.Duration("0s")),
	})

	timeouts := route.Spec.Rules[0].Timeouts
	g.Expect(timeouts).NotTo(gomega.BeNil())
	g.Expect(timeouts.Request).To(gomega.HaveValue(gomega.Equal(gatewayv1.Duration("0s"))),
		"0s is the Gateway API spelling of a disabled request timeout")
}

func TestIsHTTPRouteAccepted(t *testing.T) {
	g := gomega.NewWithT(t)

	// Empty parents (controller has not observed the route yet) is "not yet
	// accepted", not an error.
	g.Expect(IsHTTPRouteAccepted(&gatewayv1.HTTPRoute{})).To(gomega.BeFalse())

	accepted := &gatewayv1.HTTPRoute{
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{{
					Conditions: []metav1.Condition{{
						Type:   string(gatewayv1.RouteConditionAccepted),
						Status: metav1.ConditionTrue,
					}},
				}},
			},
		},
	}
	g.Expect(IsHTTPRouteAccepted(accepted)).To(gomega.BeTrue())

	rejected := accepted.DeepCopy()
	rejected.Status.Parents[0].Conditions[0].Status = metav1.ConditionFalse
	g.Expect(IsHTTPRouteAccepted(rejected)).To(gomega.BeFalse())
}

func TestDeleteHTTPRoute_ToleratesNotFound(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme, err := newGatewayScheme()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	c := newFakeClient(scheme)

	g.Expect(DeleteHTTPRoute(context.Background(), c, "ns", "absent")).To(gomega.Succeed(),
		"deleting a non-existent HTTPRoute must be a no-op")
}

func TestDeleteHTTPRoute_WrapsADeleteError(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme, err := newGatewayScheme()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	boom := apierrors.NewInternalError(errors.New("boom"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return boom
		},
	}).Build()

	err = DeleteHTTPRoute(context.Background(), c, "ns", "route")
	g.Expect(err).To(gomega.MatchError("deleting HTTPRoute ns/route: Internal error occurred: boom"))
	g.Expect(errors.Is(err, boom)).To(gomega.BeTrue())
	g.Expect(apierrors.IsInternalError(err)).To(gomega.BeTrue(),
		"callers branch on the status error through the wrap")
}
