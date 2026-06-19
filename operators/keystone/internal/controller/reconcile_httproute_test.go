// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	commonv1 "github.com/c5c3/forge/internal/common/types"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

func hrTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return s
}

func hrTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-keystone",
			Namespace:  "default",
			UID:        "ks-uid",
			Generation: 1,
		},
		Spec: keystonev1alpha1.KeystoneSpec{
			Replicas: 3,
			Image:    commonv1.ImageSpec{Repository: "ghcr.io/c5c3/keystone", Tag: "2025.2"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "keystone",
				SecretRef: commonv1.SecretRefSpec{Name: "keystone-db-credentials"},
			},
			Cache: commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			Bootstrap: keystonev1alpha1.BootstrapSpec{
				AdminUser:              "admin",
				AdminPasswordSecretRef: commonv1.SecretRefSpec{Name: "keystone-admin"},
				Region:                 "RegionOne",
			},
		},
	}
}

func hrTestGateway() *keystonev1alpha1.GatewaySpec {
	return &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{
			Name: "public-gateway",
		},
		Hostname: "keystone.example.com",
	}
}

func newHRTestReconciler(s *runtime.Scheme, objs ...client.Object) *KeystoneReconciler {
	cb := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	// HTTPRoute carries a status subresource so the operator's main-resource
	// apply never clobbers the Gateway-controller-written Accepted condition.
	cb = cb.WithStatusSubresource(&keystonev1alpha1.Keystone{}, &gatewayv1.HTTPRoute{})
	// The fake client's default typed converter cannot apply an unstructured
	// HTTPRoute; the deduced converter applies it uniformly. Server-Side Apply
	// against a real API server is exercised by the internal/common envtest
	// suites.
	cb = cb.WithTypeConverters(managedfields.NewDeducedTypeConverter())
	return &KeystoneReconciler{
		Client:   cb.Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
		// Every test in this file exercises the Gateway API code paths —
		// the scheme includes gatewayv1 and the fake client supports
		// HTTPRoute. Simulate a cluster where the CRD is installed so the
		// runtime CRD-availability guard in reconcileHTTPRoute does not
		// short-circuit the tests.
		gatewayAPIAvailable: true,
	}
}

// --- keystoneStatusEndpoint unit tests ---

func TestKeystoneStatusEndpoint_GatewayNil_ReturnsClusterLocal(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	// Gateway is nil by default.

	endpoint := keystoneStatusEndpoint(ks)

	g.Expect(endpoint).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"),
		"spec.gateway unset must produce the in-cluster Service DNS endpoint")
}

func TestKeystoneStatusEndpoint_GatewaySet_ReturnsHTTPSHostnameEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	endpoint := keystoneStatusEndpoint(ks)

	g.Expect(endpoint).To(Equal("https://keystone.example.com/v3"),
		"spec.gateway.Hostname must drive the public HTTPS endpoint")
}

func TestKeystoneStatusEndpoint_PublicEndpointSet_OverridesHostnameDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	// External port stems from kind extraPortMappings or an edge proxy and is
	// not captured anywhere in the Keystone or Gateway spec — only
	// publicEndpoint can express it.
	ks.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com:8443/v3"

	endpoint := keystoneStatusEndpoint(ks)

	g.Expect(endpoint).To(Equal("https://keystone.example.com:8443/v3"),
		"spec.bootstrap.publicEndpoint must take precedence over the synthesised https://{hostname}/v3 default")
}

func TestKeystoneStatusEndpoint_PublicEndpointEmpty_FallsBackToHostnameDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	// PublicEndpoint left empty (the default for CRs that don't republish
	// the listener on a non-443 host port). Status must continue to fall
	// back to the synthesised URL so behaviour is unchanged for existing
	// CRs.
	ks.Spec.Bootstrap.PublicEndpoint = ""

	endpoint := keystoneStatusEndpoint(ks)

	g.Expect(endpoint).To(Equal("https://keystone.example.com/v3"),
		"empty publicEndpoint must fall back to https://{hostname}/v3")
}

func TestKeystoneStatusEndpoint_PublicEndpointWithoutGateway_ReturnsClusterLocal(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	// spec.gateway nil: even when publicEndpoint is set, the operator did
	// not create an HTTPRoute, so status.endpoint must continue to expose
	// the in-cluster Service DNS (the only address whose readiness the
	// operator's health-check actually verifies).
	ks.Spec.Bootstrap.PublicEndpoint = "https://keystone.example.com/v3"

	endpoint := keystoneStatusEndpoint(ks)

	g.Expect(endpoint).To(Equal("http://test-keystone.default.svc.cluster.local:5000/v3"),
		"publicEndpoint without spec.gateway must not override the cluster-local fallback")
}

// --- buildKeystoneHTTPRoute unit tests ---

func TestBuildKeystoneHTTPRoute_NameAndNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Name).To(Equal("test-keystone"))
	g.Expect(route.Namespace).To(Equal("default"))
}

func TestBuildKeystoneHTTPRoute_Labels(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Labels).To(HaveKeyWithValue("app.kubernetes.io/name", "keystone"))
	g.Expect(route.Labels).To(HaveKeyWithValue("app.kubernetes.io/instance", "test-keystone"))
	g.Expect(route.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "keystone-operator"))
}

func TestBuildKeystoneHTTPRoute_ParentRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.ParentRefs).To(HaveLen(1))
	g.Expect(route.Spec.ParentRefs[0].Name).To(Equal(gatewayv1.ObjectName("public-gateway")))
	g.Expect(route.Spec.ParentRefs[0].Namespace).To(BeNil(),
		"Namespace should be nil when GatewayParentRefSpec.Namespace is empty so the Route's own namespace is used")
	g.Expect(route.Spec.ParentRefs[0].SectionName).To(BeNil(),
		"SectionName should be nil when GatewayParentRefSpec.SectionName is empty")
}

func TestBuildKeystoneHTTPRoute_ParentRefWithOptionalFields(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = &keystonev1alpha1.GatewaySpec{
		ParentRef: keystonev1alpha1.GatewayParentRefSpec{
			Name:        "public-gateway",
			Namespace:   "gateway-system",
			SectionName: "https",
		},
		Hostname: "keystone.example.com",
	}

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.ParentRefs).To(HaveLen(1))
	g.Expect(route.Spec.ParentRefs[0].Name).To(Equal(gatewayv1.ObjectName("public-gateway")))
	g.Expect(route.Spec.ParentRefs[0].Namespace).NotTo(BeNil())
	g.Expect(*route.Spec.ParentRefs[0].Namespace).To(Equal(gatewayv1.Namespace("gateway-system")))
	g.Expect(route.Spec.ParentRefs[0].SectionName).NotTo(BeNil())
	g.Expect(*route.Spec.ParentRefs[0].SectionName).To(Equal(gatewayv1.SectionName("https")))
}

func TestBuildKeystoneHTTPRoute_Hostname(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("keystone.example.com")))
}

func TestBuildKeystoneHTTPRoute_DefaultPath(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Path = ""

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches[0].Path).NotTo(BeNil())
	g.Expect(route.Spec.Rules[0].Matches[0].Path.Type).NotTo(BeNil())
	g.Expect(*route.Spec.Rules[0].Matches[0].Path.Type).To(Equal(gatewayv1.PathMatchPathPrefix))
	g.Expect(route.Spec.Rules[0].Matches[0].Path.Value).NotTo(BeNil())
	g.Expect(*route.Spec.Rules[0].Matches[0].Path.Value).To(Equal("/"))
}

func TestBuildKeystoneHTTPRoute_CustomPath(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Path = "/identity"

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches[0].Path).NotTo(BeNil())
	g.Expect(*route.Spec.Rules[0].Matches[0].Path.Type).To(Equal(gatewayv1.PathMatchPathPrefix))
	g.Expect(*route.Spec.Rules[0].Matches[0].Path.Value).To(Equal("/identity"))
}

// TestBuildKeystoneHTTPRoute_PathWithoutLeadingSlash_Normalized verifies that a
// path like "identity" (missing the leading slash) is normalized to "/identity"
// so the resulting HTTPRoute is not rejected by Gateway controllers that
// require HTTPPathMatch.Value to begin with "/".
func TestBuildKeystoneHTTPRoute_PathWithoutLeadingSlash_Normalized(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Path = "identity"

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].Matches[0].Path).NotTo(BeNil())
	g.Expect(*route.Spec.Rules[0].Matches[0].Path.Value).To(Equal("/identity"),
		"missing leading slash must be normalized")
}

func TestBuildKeystoneHTTPRoute_BackendRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].BackendRefs).To(HaveLen(1))

	backend := route.Spec.Rules[0].BackendRefs[0].BackendObjectReference
	g.Expect(backend.Name).To(Equal(gatewayv1.ObjectName("test-keystone")))
	g.Expect(backend.Port).NotTo(BeNil())
	g.Expect(*backend.Port).To(Equal(gatewayv1.PortNumber(5000)))

	// Kind defaults to Service when nil; if set it must be Service.
	if backend.Kind != nil {
		g.Expect(*backend.Kind).To(Equal(gatewayv1.Kind("Service")))
	}
}

func TestBuildKeystoneHTTPRoute_Annotations(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Annotations = map[string]string{
		"konghq.com/plugins":   "rate-limiting",
		"nginx.ingress.k8s.io": "test",
	}

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Annotations).To(HaveKeyWithValue("konghq.com/plugins", "rate-limiting"))
	g.Expect(route.Annotations).To(HaveKeyWithValue("nginx.ingress.k8s.io", "test"))
}

func TestBuildKeystoneHTTPRoute_NoAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Annotations = nil

	route := buildKeystoneHTTPRoute(ks)

	// No user-provided annotations — operator does not inject any annotations
	// beyond what the user specifies.
	g.Expect(route.Annotations).To(BeEmpty())
}

// --- reconcileHTTPRoute lifecycle unit tests ---

// --- Path 1: gateway set — create HTTPRoute ---

func TestReconcileHTTPRoute_GatewaySet_CreatesHTTPRoute(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	r := newHRTestReconciler(s, ks)

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	// No Accepted status yet on the fresh HTTPRoute: expect a requeue so the
	// operator re-checks parent status.
	g.Expect(result.RequeueAfter).NotTo(BeZero())

	var route gatewayv1.HTTPRoute
	g.Expect(r.Client.Get(context.Background(), types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())

	g.Expect(route.OwnerReferences).To(HaveLen(1))
	g.Expect(route.OwnerReferences[0].Name).To(Equal("test-keystone"))

	// Verify HTTPRouteReady condition is set False/HTTPRouteNotAccepted while
	// parents have not yet reported Accepted.
	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))
}

// --- Path 2: gateway nil — delete HTTPRoute ---

func TestReconcileHTTPRoute_GatewayNil_NoExisting_SetsNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	// gateway is nil by default.
	r := newHRTestReconciler(s, ks)

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))
}

func TestReconcileHTTPRoute_GatewayNil_ExistingRoute_DeletesHTTPRoute(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()

	// Pre-create an HTTPRoute as if gateway was previously enabled.
	existingRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
	}
	r := newHRTestReconciler(s, ks, existingRoute)
	ctx := context.Background()

	// Verify HTTPRoute exists before reconcile.
	var route gatewayv1.HTTPRoute
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())

	// reconcileHTTPRoute with nil gateway should delete the route.
	result, err := r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	// Verify HTTPRoute was deleted.
	err = r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))
}

// --- ObservedGeneration and idempotency ---

func TestReconcileHTTPRoute_GatewaySet_ConditionObservedGeneration(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()

	ks := hrTestKeystone()
	ks.Generation = 9
	ks.Spec.Gateway = hrTestGateway()
	r := newHRTestReconciler(s, ks)

	_, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.ObservedGeneration).To(Equal(int64(9)))

	// Also verify ObservedGeneration for the not-required path.
	ks2 := hrTestKeystone()
	ks2.Generation = 14
	r2 := newHRTestReconciler(s, ks2)

	_, err = r2.reconcileHTTPRoute(context.Background(), ks2)
	g.Expect(err).NotTo(HaveOccurred())

	cond2 := meta.FindStatusCondition(ks2.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond2).NotTo(BeNil())
	g.Expect(cond2.ObservedGeneration).To(Equal(int64(14)))
}

func TestReconcileHTTPRoute_GatewaySet_HTTPRouteUpdated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	r := newHRTestReconciler(s, ks)
	ctx := context.Background()

	// First reconcile creates the HTTPRoute.
	_, err := r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	// Change hostname and re-reconcile — the HTTPRoute should be updated.
	ks.Spec.Gateway.Hostname = "keystone.new-example.com"
	_, err = r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var route gatewayv1.HTTPRoute
	g.Expect(r.Client.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())

	g.Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("keystone.new-example.com")))
}

// TestReconcileHTTPRoute_RepeatedReconcileIsIdempotent verifies that reconciling
// an unchanged spec twice leaves exactly one HTTPRoute with the desired spec.
// Server-Side Apply converges without a write on a real API server; that
// no-write property is exercised by the internal/common envtest convergence
// suites (the fake client bumps resourceVersion on every apply).
func TestReconcileHTTPRoute_RepeatedReconcileIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	r := newHRTestReconciler(s, ks)
	ctx := context.Background()

	_, err := r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	list := &gatewayv1.HTTPRouteList{}
	g.Expect(r.List(ctx, list, client.InNamespace("default"))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("keystone.example.com")))
}

// --- Error scenarios ---

func TestReconcileHTTPRoute_EnsureError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				return fmt.Errorf("simulated HTTPRoute apply error")
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:              c,
		Scheme:              s,
		Recorder:            record.NewFakeRecorder(10),
		gatewayAPIAvailable: true,
	}

	_, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("ensuring HTTPRoute"))
	g.Expect(err.Error()).To(ContainSubstring("simulated HTTPRoute apply error"))
}

func TestReconcileHTTPRoute_DeleteError_Propagated(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	// gateway is nil — triggers delete path.

	existingRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ks, existingRoute).
		WithStatusSubresource(&keystonev1alpha1.Keystone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*gatewayv1.HTTPRoute); ok {
					return fmt.Errorf("simulated HTTPRoute deletion error")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &KeystoneReconciler{
		Client:              c,
		Scheme:              s,
		Recorder:            record.NewFakeRecorder(10),
		gatewayAPIAvailable: true,
	}

	_, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("deleting HTTPRoute"))
	g.Expect(err.Error()).To(ContainSubstring("simulated HTTPRoute deletion error"))
}

// --- annotation/label removal via Server-Side Apply field ownership ---

func TestReconcileHTTPRoute_AnnotationRemoval_RemovesKeyFromLiveRoute(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()
	ks.Spec.Gateway.Annotations = map[string]string{
		"konghq.com/plugins":   "rate-limiting",
		"nginx.ingress.k8s.io": "test",
	}
	r := newHRTestReconciler(s, ks)
	ctx := context.Background()

	// First reconcile creates the HTTPRoute with both annotations.
	_, err := r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	var route gatewayv1.HTTPRoute
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())
	g.Expect(route.Annotations).To(HaveKeyWithValue("konghq.com/plugins", "rate-limiting"))
	g.Expect(route.Annotations).To(HaveKeyWithValue("nginx.ingress.k8s.io", "test"))

	// Remove konghq.com/plugins from spec.gateway.annotations and re-reconcile.
	// Server-Side Apply relinquishes the no-longer-set key, so the API server
	// removes it without any sentinel bookkeeping.
	ks.Spec.Gateway.Annotations = map[string]string{
		"nginx.ingress.k8s.io": "test",
	}
	_, err = r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())
	g.Expect(route.Annotations).NotTo(HaveKey("konghq.com/plugins"),
		"annotation removed from spec.gateway.annotations must be removed from the live HTTPRoute")
	g.Expect(route.Annotations).To(HaveKeyWithValue("nginx.ingress.k8s.io", "test"),
		"annotations still present in spec.gateway.annotations must be preserved")

	// Remove all annotations and re-reconcile — all previously-owned keys go.
	ks.Spec.Gateway.Annotations = nil
	_, err = r.reconcileHTTPRoute(ctx, ks)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: "test-keystone", Namespace: "default",
	}, &route)).To(Succeed())
	g.Expect(route.Annotations).NotTo(HaveKey("nginx.ingress.k8s.io"),
		"clearing spec.gateway.annotations must remove all previously-managed annotation keys")
}

// Cross-manager annotation preservation (a third party's annotation surviving
// the operator dropping its own) depends on the API server's granular ownership
// of the metadata.annotations map, which the fake client's deduced type
// converter treats atomically. It is therefore covered by the envtest suite:
// TestIntegration_HTTPRoute_PreservesThirdPartyAnnotation in integration_test.go.

// --- Status tests: Accepted condition tracking ---

func TestReconcileHTTPRoute_AcceptedCondition_True(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	// Pre-create an HTTPRoute that already reports Accepted=True on its parent,
	// simulating a Gateway controller that has accepted the attachment
	acceptedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "public-gateway"}},
			},
			Hostnames: []gatewayv1.Hostname{"keystone.example.com"},
		},
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{
					{
						ParentRef:      gatewayv1.ParentReference{Name: "public-gateway"},
						ControllerName: "example.net/gateway-controller",
						Conditions: []metav1.Condition{
							{
								Type:   string(gatewayv1.RouteConditionAccepted),
								Status: metav1.ConditionTrue,
								Reason: string(gatewayv1.RouteReasonAccepted),
							},
						},
					},
				},
			},
		},
	}
	r := newHRTestReconciler(s, ks, acceptedRoute)

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	// No requeue once Accepted=True is observed.
	g.Expect(result.RequeueAfter).To(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteAccepted))
}

func TestReconcileHTTPRoute_AcceptedCondition_False_Requeues(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	// Pre-create an HTTPRoute whose parent reports Accepted=False.
	rejectedRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "public-gateway"}},
			},
			Hostnames: []gatewayv1.Hostname{"keystone.example.com"},
		},
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{
					{
						ParentRef:      gatewayv1.ParentReference{Name: "public-gateway"},
						ControllerName: "example.net/gateway-controller",
						Conditions: []metav1.Condition{
							{
								Type:    string(gatewayv1.RouteConditionAccepted),
								Status:  metav1.ConditionFalse,
								Reason:  string(gatewayv1.RouteReasonNotAllowedByListeners),
								Message: "listener does not allow this route",
							},
						},
					},
				},
			},
		},
	}
	r := newHRTestReconciler(s, ks, rejectedRoute)

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	// Requeue so the operator re-checks parent status.
	g.Expect(result.RequeueAfter).NotTo(BeZero())

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotAccepted))
}

// --- Gateway API CRD missing (production-hardening) ---

// TestReconcileHTTPRoute_GatewayAPIUnavailable_GatewayNil_SetsNotRequired
// verifies that when the Gateway API CRD is not installed, a Keystone CR
// without spec.gateway still reconciles successfully — the controller must
// not attempt to Delete an HTTPRoute whose kind the API server does not know.
func TestReconcileHTTPRoute_GatewayAPIUnavailable_GatewayNil_SetsNotRequired(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	// gateway is nil; CRD not installed.

	r := newHRTestReconciler(s, ks)
	r.gatewayAPIAvailable = false

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonHTTPRouteNotRequired))
}

// TestReconcileHTTPRoute_GatewayAPIUnavailable_GatewaySet_SurfacesCondition
// verifies that when the CRD is missing but the user nonetheless configures
// spec.gateway, the operator surfaces a clear HTTPRouteReady=False condition
// with reason GatewayAPINotInstalled instead of erroring on the Create call.
func TestReconcileHTTPRoute_GatewayAPIUnavailable_GatewaySet_SurfacesCondition(t *testing.T) {
	g := NewGomegaWithT(t)
	s := hrTestScheme()
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	r := newHRTestReconciler(s, ks)
	r.gatewayAPIAvailable = false

	result, err := r.reconcileHTTPRoute(context.Background(), ks)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{}))

	cond := meta.FindStatusCondition(ks.Status.Conditions, conditionTypeHTTPRouteReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonGatewayAPINotInstalled))
	g.Expect(cond.Message).To(ContainSubstring("HTTPRoute CRD is not installed"))
}

// TestBuildKeystoneHTTPRoute_NameAndBackendRefMatchCR pins the HTTPRoute
// ObjectMeta.Name and the BackendRef.Name to the bare CR name. Both must point
// at the Keystone Service emitted at `<cr-name>` (renamed from `<cr-name>-api`): // keystone-api-legacy: pre-rename name referenced for traceability.
// a mismatched BackendRef would silently 503 every
// request that the Gateway routes through the API hostname.
func TestBuildKeystoneHTTPRoute_NameAndBackendRefMatchCR(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()
	ks.Spec.Gateway = hrTestGateway()

	route := buildKeystoneHTTPRoute(ks)

	g.Expect(route.Name).To(Equal(ks.Name),
		"HTTPRoute Name must equal the CR name")
	g.Expect(route.Name).NotTo(HaveSuffix("-api"),
		"HTTPRoute Name must not carry the legacy `-api` suffix")
	g.Expect(route.Spec.Rules).NotTo(BeEmpty())
	g.Expect(route.Spec.Rules[0].BackendRefs).NotTo(BeEmpty())
	backendName := string(route.Spec.Rules[0].BackendRefs[0].Name)
	g.Expect(backendName).To(Equal(ks.Name),
		"BackendRef must point at the Service emitted at `<cr-name>`")
	g.Expect(backendName).NotTo(HaveSuffix("-api"),
		"BackendRef Name must not carry the legacy `-api` suffix")
}

// TestInternalAPIURL_UsesBareCRName pins the cluster-internal Keystone URL —
// the URL the operator hits to verify KeystoneAPIReady — to the bare-CR-name
// Service DNS form. Any drift here would either (a) regress to the legacy
// `<cr-name>-api` host and 503 from the Service rename, or (b) skip the // keystone-api-legacy: pre-rename name referenced for traceability.
// Service entirely and bypass kube-proxy load balancing.
func TestInternalAPIURL_UsesBareCRName(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := hrTestKeystone()

	url := internalAPIURL(ks)

	expected := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v3", ks.Name, ks.Namespace)
	g.Expect(url).To(Equal(expected),
		"internalAPIURL must use the bare CR name")
	g.Expect(url).NotTo(ContainSubstring(ks.Name+"-api."),
		"internalAPIURL must not embed the legacy `-api` suffix in the host segment")
}
