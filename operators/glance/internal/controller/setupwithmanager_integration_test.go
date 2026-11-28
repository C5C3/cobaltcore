// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package controller

import (
	"testing"

	. "github.com/onsi/gomega"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	"github.com/c5c3/cobaltcore/operators/glance/internal/testutil"
)

// TestSetupWithManager_AllControllersStart registers the production
// GlanceReconciler and GlanceBackendReconciler via their real SetupWithManager
// methods against an envtest manager that serves every watched CRD (the
// operator CRDs plus the shared fake CRDs), asserts the RESTMapper probes
// latched, and then waits for each controller to stamp
// status.observedGeneration on a CR of its kind.
func TestSetupWithManager_AllControllersStart(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	var glance *GlanceReconciler
	c, ctx, _ := setupEnvTestWithController(t, false, func(r *GlanceReconciler) { glance = r })
	g.Expect(glance.gatewayAPIAvailable).To(BeTrue(),
		"SetupWithManager must detect the Gateway API HTTPRoute CRD served from fake_crds/gateway-api")
	g.Expect(glance.vpaAvailable).To(BeTrue(),
		"SetupWithManager must detect the VerticalPodAutoscaler CRD served from fake_crds/vertical-pod-autoscaler")
	g.Expect(glance.memcachedAvailable).To(BeTrue(),
		"SetupWithManager must detect the Memcached CRD served from fake_crds/memcached-operator")

	ns := createTestNamespace(t, ctx, c)

	cr := integrationGlance("glance", ns)
	g.Expect(c.Create(ctx, cr)).To(Succeed(), "create Glance fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, cr,
		func() int64 { return cr.Status.ObservedGeneration }, "Glance", eventuallyTimeout, pollInterval)

	glanceBackend := integrationBackend("store", ns, "glance", true)
	g.Expect(c.Create(ctx, glanceBackend)).To(Succeed(), "create GlanceBackend fixture")
	commonenvtest.WaitForObservedGeneration(t, ctx, c, glanceBackend,
		func() int64 { return glanceBackend.Status.ObservedGeneration }, "GlanceBackend", eventuallyTimeout, pollInterval)
}
