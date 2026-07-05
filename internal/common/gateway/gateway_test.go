// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

var httpRouteGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupVersion.Group,
	Version: gatewayv1.GroupVersion.Version,
	Kind:    "HTTPRoute",
}

func TestIsGVKAvailable(t *testing.T) {
	g := gomega.NewWithT(t)

	withMapping := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: gatewayv1.GroupVersion.Group, Version: gatewayv1.GroupVersion.Version}})
	withMapping.Add(httpRouteGVK, meta.RESTScopeNamespace)
	g.Expect(IsGVKAvailable(withMapping, httpRouteGVK)).To(gomega.BeTrue())

	// Missing mapping (CRD not installed) and a nil mapper must both report
	// unavailable rather than erroring.
	empty := meta.NewDefaultRESTMapper(nil)
	g.Expect(IsGVKAvailable(empty, httpRouteGVK)).To(gomega.BeFalse())
	g.Expect(IsGVKAvailable(nil, httpRouteGVK)).To(gomega.BeFalse())
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
	backend := rule.BackendRefs[0].BackendRef.BackendObjectReference
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
