// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the KeystoneService catalog block: the adopt-gated collision probe,
// the projected service and endpoint rows, and the readiness they gate.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v3/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// runKSCatalog drives ensureCatalog once and returns the resulting CatalogReady
// condition.
func runKSCatalog(
	t *testing.T, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object,
) (*metav1.Condition, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)
	s := korcTestScheme(t)
	all := append([]client.Object{cp, ks}, objs...)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(all...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	return ksCatalogCondition(ks), c
}

// ksWithCatalog returns a CR declaring the minimal catalog block.
func ksWithCatalog() *c5c3v1alpha1.KeystoneService {
	ks := keystoneServiceCR()
	ks.Spec.Catalog = ksCatalogSpec()
	return ks
}

// ksAvailableEndpoint seeds one endpoint row as K-ORC reports it once registered.
func ksAvailableEndpoint(ks *c5c3v1alpha1.KeystoneService, iface c5c3v1alpha1.ExternalEndpointType, id string) *orcv1alpha1.Endpoint {
	return &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogEndpointRef(ks, iface),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.EndpointStatus{Conditions: availableImportConditions(), ID: ptr.To(id)},
	}
}

// --- the collision probe ---

func TestKSCatalog_CollisionFailsLoudly(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	// The probe resolved: a service row of that type and name already exists.
	probe := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.ServiceStatus{Conditions: availableImportConditions()},
	}

	cond, c := runKSCatalog(t, ks, cp, probe)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogCollision))
	g.Expect(cond.Message).To(ContainSubstring("catalog.adopt=true"))
	g.Expect(cond.Message).To(ContainSubstring("image"))

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"a managed create would silently adopt the existing row, so nothing may be created")
}

func TestKSCatalog_ProbePendingHoldsTheRegistration(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	cond, c := runKSCatalog(t, ks, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonProbingForCollision))

	probe := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace}, probe)).To(Succeed())
	g.Expect(probe.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"the probe only reads the catalog; it must never write to it")
	g.Expect(*probe.Spec.Import.Filter.Type).To(Equal("image"))
	g.Expect(string(*probe.Spec.Import.Filter.Name)).To(Equal(ksTestName),
		"the filter must ask the question K-ORC's own adoption asks: type AND name")

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestKSCatalog_ServiceCreatedOnceTheProbeReportsAbsent(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	probe := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace},
		Status:     orcv1alpha1.ServiceStatus{Conditions: pendingImportConditions(0)},
	}

	_, c := runKSCatalog(t, ks, cp, probe)

	service := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, service)).To(Succeed())
	g.Expect(service.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the resolved probe stops polling Keystone")
}

func TestKSCatalog_AdoptSkipsTheProbeEntirely(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Adopt = true

	_, c := runKSCatalog(t, ks, cp)

	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace},
		&orcv1alpha1.Service{})).To(Succeed())
	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no probe is created under adopt")
}

func TestKSCatalog_ExistingManagedServiceSkipsTheProbe(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	_, c := runKSCatalog(t, ks, cp, ksConvergedCatalog(ks)...)

	err := c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceProbeRef(ks), Namespace: ks.Namespace}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a Service we already own settles the verdict")
}

// --- the projected rows ---

func TestKSCatalog_ServiceRowAloneReachesReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = nil

	cond, c := runKSCatalog(t, ks, cp, ksConvergedCatalog(ks)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))

	var endpoints orcv1alpha1.EndpointList
	g.Expect(c.List(context.Background(), &endpoints, client.InNamespace(ks.Namespace))).To(Succeed())
	g.Expect(endpoints.Items).To(BeEmpty(), "an entry with no endpoints registers the service row alone")

	g.Expect(ks.Status.Catalog).NotTo(BeNil())
	g.Expect(ks.Status.Catalog.ServiceID).To(Equal("ks-service-id"))
	g.Expect(ks.Status.Catalog.Endpoints).To(BeEmpty())
}

func TestKSCatalog_EndpointsAreProjectedPerInterface(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
	}

	seeded := append(ksConvergedCatalog(ks),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypePublic, "ep-public-id"),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal, "ep-internal-id"))

	cond, c := runKSCatalog(t, ks, cp, seeded...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	public := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
		Namespace: ks.Namespace,
	}, public)).To(Succeed())
	g.Expect(public.Spec.Resource.URL).To(Equal("https://image.example/public"))
	g.Expect(public.Spec.Resource.Interface).To(Equal("public"))
	g.Expect(string(public.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceCatalogServiceRef(ks)))

	g.Expect(ks.Status.Catalog.Endpoints).To(HaveLen(2))
	g.Expect(ks.Status.Catalog.Endpoints[0].Interface).To(Equal(c5c3v1alpha1.ExternalEndpointTypePublic))
	g.Expect(ks.Status.Catalog.Endpoints[0].ID).To(Equal("ep-public-id"))
}

// A client that sets region_name (every nova client section, the neutron
// notifier) resolves only a row registered in that region: keystoneauth skips a
// region-less row and answers EndpointNotFound.
func TestKSCatalog_EndpointsAreRegisteredInTheControlPlaneRegion(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.Region = "RegionTwo"
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
	}

	seeded := append(ksConvergedCatalog(ks),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypePublic, "ep-public-id"),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal, "ep-internal-id"))

	cond, c := runKSCatalog(t, ks, cp, seeded...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(ContainSubstring(`in region "RegionTwo"`))

	region := &orcv1alpha1.Region{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name: keystoneServiceCatalogRegionRef(ks), Namespace: ks.Namespace,
	}, region)).To(Succeed())
	g.Expect(region.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"the region belongs to the keystone bootstrap; a registration only reads it")
	g.Expect(region.Spec.Import).NotTo(BeNil())
	g.Expect(region.Spec.Import.Filter).NotTo(BeNil())
	g.Expect(region.Spec.Import.Filter.Name).To(Equal(ptr.To(orcv1alpha1.OpenStackName("RegionTwo"))))
	credRef, _ := keystoneServiceCredentialRefs(cp)
	g.Expect(region.Spec.CloudCredentialsRef).To(Equal(credRef))

	for _, iface := range []c5c3v1alpha1.ExternalEndpointType{
		c5c3v1alpha1.ExternalEndpointTypePublic, c5c3v1alpha1.ExternalEndpointTypeInternal,
	} {
		endpoint := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(context.Background(), types.NamespacedName{
			Name: keystoneServiceCatalogEndpointRef(ks, iface), Namespace: ks.Namespace,
		}, endpoint)).To(Succeed())
		g.Expect(endpoint.Spec.Resource.RegionRef).To(Equal(ptr.To(orcv1alpha1.KubernetesNameRef(region.Name))),
			"the %s row must be registered in the plane's region", iface)
	}
}

func TestKSCatalog_WaitsWhileTheRegionIsNotResolved(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	seeded := ksConvergedCatalog(ks)
	seeded[1].(*orcv1alpha1.Region).Status = orcv1alpha1.RegionStatus{Conditions: pendingImportConditions(time.Minute)}
	seeded = append(seeded, ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypePublic, "ep-public-id"))

	cond, _ := runKSCatalog(t, ks, cp, seeded...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(`the Keystone region "RegionOne"`))
}

// ksLegacyEndpoint seeds the region-less row an earlier version registered,
// holding K-ORC's finalizer the way a live one does.
func ksLegacyEndpoint(ks *c5c3v1alpha1.KeystoneService, iface c5c3v1alpha1.ExternalEndpointType) *orcv1alpha1.Endpoint {
	return &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceLegacyCatalogEndpointRef(ks, iface),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
			Finalizers:      []string{"openstack.k-orc.cloud/endpoint"},
		},
		Status: orcv1alpha1.EndpointStatus{Conditions: availableImportConditions(), ID: ptr.To("legacy-id")},
	}
}

// Until the regioned row is Available the region-less one is the only row a
// client finds, so neither the catalog block nor the sweep may remove it.
func TestKSCatalog_LegacyEndpointStaysUntilItsReplacementIsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
	}
	legacyName := keystoneServiceLegacyCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)

	cond, c := runKSCatalog(t, ks, cp,
		append(ksConvergedCatalog(ks), ksLegacyEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal))...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Message).To(ContainSubstring(keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)))

	legacy := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: legacyName, Namespace: ks.Namespace}, legacy)).To(Succeed())
	g.Expect(legacy.DeletionTimestamp).To(BeNil())
	g.Expect(keystoneServiceDeclaredChildNames(ks)).To(HaveKey(legacyName),
		"the per-pass sweep must not remove the row the catalog still relies on")
}

func TestKSCatalog_LegacyEndpointIsRetiredOnceItsReplacementIsAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
	}
	legacyName := keystoneServiceLegacyCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)

	cond, c := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal, "ep-internal-id"),
		ksLegacyEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal))...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"the registration is not done while K-ORC is still removing the region-less row")
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(legacyName))

	legacy := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: legacyName, Namespace: ks.Namespace}, legacy)).To(Succeed())
	g.Expect(legacy.DeletionTimestamp).NotTo(BeNil(), "the region-less row is deleted once its replacement is Available")

	// K-ORC removes the Keystone row and releases its finalizer.
	legacy.Finalizers = nil
	g.Expect(c.Update(context.Background(), legacy)).To(Succeed())

	r := &KeystoneServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ksCatalogCondition(ks).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ks.Status.Catalog.Endpoints[0].ID).To(Equal("ep-internal-id"))
}

// An interface the spec stopped declaring has nothing to hand over to, so its
// region-less row goes with the regioned one on the next sweep.
func TestKSCatalog_LegacyEndpointOfAnUndeclaredInterfaceIsSwept(t *testing.T) {
	g := NewGomegaWithT(t)

	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	keep := keystoneServiceDeclaredChildNames(ks)
	g.Expect(keep).To(HaveKey(keystoneServiceCatalogRegionRef(ks)))
	g.Expect(keep).NotTo(HaveKey(keystoneServiceLegacyCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)))
	g.Expect(keep).NotTo(HaveKey(keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)))
}

func TestKSCatalog_ManagedChildrenUseThePasswordCloud(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Adopt = true

	_, c := runKSCatalog(t, ks, cp)

	service := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, service)).To(Succeed())
	// The application credential is revoked by its own finalizer at teardown, so a
	// managed row authenticating through it could not delete itself.
	g.Expect(service.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)))
}

func TestKSCatalog_ServiceNameFallsBackToTheCRName(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Adopt = true

	_, c := runKSCatalog(t, ks, cp)

	service := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, service)).To(Succeed())
	// Never nil: K-ORC would then name the row after the child CR, whose name
	// carries this registration's uniqueness suffix.
	g.Expect(service.Spec.Resource.Name).NotTo(BeNil())
	g.Expect(string(*service.Spec.Resource.Name)).To(Equal(ksTestName))
}

func TestKSCatalog_ExplicitServiceNameIsRegisteredVerbatim(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Adopt = true
	ks.Spec.Catalog.ServiceName = "glance"

	_, c := runKSCatalog(t, ks, cp)

	service := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		types.NamespacedName{Name: keystoneServiceCatalogServiceRef(ks), Namespace: ks.Namespace}, service)).To(Succeed())
	g.Expect(string(*service.Spec.Resource.Name)).To(Equal("glance"))
	g.Expect(service.Spec.Resource.Type).To(Equal("image"))
}

// --- failure reporting ---

func TestKSCatalog_TerminalServiceErrorIsReportedBeforeItsEndpoints(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogServiceRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.ServiceStatus{Conditions: terminalImportConditions("service create rejected")},
	}
	endpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.EndpointStatus{Conditions: terminalImportConditions("endpoint blocked on its service")},
	}

	cond, _ := runKSCatalog(t, ks, cp, service, endpoint)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("service create rejected"),
		"the root stuck dependency must surface, not the endpoint merely blocked on it")
	g.Expect(cond.Message).NotTo(ContainSubstring("endpoint blocked on its service"))
}

func TestKSCatalog_WaitsWhileAnEndpointIsNotAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	// The Service resolved; its endpoint has not.
	cond, _ := runKSCatalog(t, ks, cp, ksConvergedCatalog(ks)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("Endpoint"))
}

func TestKSCatalog_WaitsWhileTheServiceIsNotAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	// Owned but not yet resolved, so the probe is skipped and the wait is on the
	// registration itself.
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogServiceRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
	}

	cond, _ := runKSCatalog(t, ks, cp, service)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("Service"))
}

func TestKSCatalog_ExternalModeClassifiesTheKORCMessage(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeExternal
	cp.Spec.Services.Keystone.External = &c5c3v1alpha1.ExternalKeystoneSpec{
		AuthURL: "https://keystone.example.com/v3",
	}
	ks := ksWithCatalog()

	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogServiceRef(ks),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.ServiceStatus{
			Conditions: transientEntryConditions("Post \"https://keystone.example.com/v3/services\": dial tcp: i/o timeout"),
		},
	}

	cond, _ := runKSCatalog(t, ks, cp, service)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonEndpointUnreachable),
		"the classified cause must beat the generic wait reason")
	g.Expect(cond.Message).To(ContainSubstring("https://keystone.example.com/v3"))
}

func TestKSCatalog_TransportLatchedEndpointIsRetried(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
	}

	// K-ORC gave up on the endpoint row a minute ago because Keystone was not
	// listening, and stopped retrying.
	latched := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace:       ks.Namespace,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.EndpointStatus{Conditions: transportLatchedConditions()},
	}

	// Both K-ORC kinds carry a status subresource here: the fake client answers a
	// status write on a kind it does not carry with NotFound, which the unlatch
	// helper reads as "the child vanished" and skips.
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(ksConvergedCatalog(ks), cp, ks, latched)...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}, &orcv1alpha1.Service{}, &orcv1alpha1.Endpoint{}).
		Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	live := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{
		Name:      keystoneServiceCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypePublic),
		Namespace: ks.Namespace,
	}, live)).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(live.Status.Conditions, orcv1alpha1.ConditionProgressing)).To(BeNil(),
		"the latch must be gone from the live child so K-ORC reconciles it again")

	cond := ksCatalogCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog),
		"a cleared child is waited on, not reported as a terminal failure")
	g.Expect(cond.Message).To(ContainSubstring("Endpoint"))
}

func TestKSCatalog_KubernetesErrorIsReportedAndReturned(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithCatalog()

	s := korcTestScheme(t)
	boom := errors.New("services.openstack.k-orc.cloud is forbidden")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, ks).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*orcv1alpha1.Service); ok && key.Name == keystoneServiceCatalogServiceRef(ks) {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	cond := ksCatalogCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogError))
}

// --- registered limits ---

// ksRegisteredLimit seeds the RegisteredLimit of one declared entry as K-ORC
// reports it: Available for observedGeneration against the object's generation.
func ksRegisteredLimit(
	ks *c5c3v1alpha1.KeystoneService, resourceName string, generation, observedGeneration int64,
) *orcv1alpha1.RegisteredLimit {
	conds := availableImportConditions()
	conds[0].ObservedGeneration = observedGeneration
	return &orcv1alpha1.RegisteredLimit{
		ObjectMeta: metav1.ObjectMeta{
			Name:            keystoneServiceRegisteredLimitRef(ks, resourceName),
			Namespace:       ks.Namespace,
			Generation:      generation,
			OwnerReferences: ownedByKS(ks),
		},
		Status: orcv1alpha1.RegisteredLimitStatus{Conditions: conds, ID: ptr.To("limit-id")},
	}
}

// ksWithRegisteredLimit returns a catalog-only CR declaring one registered limit
// on a service row without endpoints.
func ksWithRegisteredLimit(resourceName string, defaultLimit int32) *c5c3v1alpha1.KeystoneService {
	ks := ksWithCatalog()
	ks.Spec.Catalog.RegisteredLimits = []c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec{
		{ResourceName: resourceName, DefaultLimit: defaultLimit},
	}
	return ks
}

// A declared registered limit is projected as one managed RegisteredLimit on the
// catalog Service, in the region the endpoints use, through the password cloud,
// and CatalogReady waits until K-ORC reports it Available.
func TestKSCatalog_RegisteredLimitIsProjectedAndGatesReadiness(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)
	name := keystoneServiceRegisteredLimitRef(ks, "r1")

	cond, c := runKSCatalog(t, ks, cp, ksConvergedCatalog(ks)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(Equal(`the registered limit "` + name + `" is registered but not yet Available`))

	var limits orcv1alpha1.RegisteredLimitList
	g.Expect(c.List(context.Background(), &limits, client.InNamespace(cp.Namespace))).To(Succeed())
	g.Expect(limits.Items).To(HaveLen(1))
	limit := limits.Items[0]
	g.Expect(limit.Name).To(Equal(keystoneServiceChildPrefix(ks) + "limit-" + serviceAccountRoleSlug("r1")))
	g.Expect(limit.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	_, managedCredRef := keystoneServiceCredentialRefs(cp)
	g.Expect(limit.Spec.CloudCredentialsRef).To(Equal(managedCredRef),
		"a managed row authenticates through the password cloud so its teardown delete outlives the app credential")
	g.Expect(limit.Spec.Resource).NotTo(BeNil())
	g.Expect(string(limit.Spec.Resource.ServiceRef)).To(Equal(keystoneServiceCatalogServiceRef(ks)))
	g.Expect(limit.Spec.Resource.RegionRef).To(Equal(ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServiceCatalogRegionRef(ks)))))
	g.Expect(limit.Spec.Resource.ResourceName).To(Equal("r1"))
	g.Expect(limit.Spec.Resource.DefaultLimit).To(Equal(ptr.To(int32(10))))
	g.Expect(limit.Spec.Resource.Description).To(BeNil())

	// K-ORC registers the limit. The fake client carries no status subresource
	// for the kind here, so a plain Update writes the status.
	limit.Status = orcv1alpha1.RegisteredLimitStatus{Conditions: availableImportConditions()}
	limit.Status.Conditions[0].ObservedGeneration = limit.Generation
	g.Expect(c.Update(context.Background(), &limit)).To(Succeed())

	r := &KeystoneServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(20)}
	credRef, _ := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	cond = ksCatalogCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
	g.Expect(cond.Message).To(ContainSubstring(`with 0 endpoint(s) and 1 registered limit(s) in region "RegionOne"`))
}

// Available for an older generation is not Available: after a defaultLimit edit
// the row in Keystone still carries the old value until K-ORC observes the new
// spec.
func TestKSCatalog_RegisteredLimitAvailableForAnOlderGenerationHolds(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 20)

	cond, _ := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks), ksRegisteredLimit(ks, "r1", 2, 1))...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("registered limit"))
}

// Keystone refuses to update a registered limit while a project limit of its
// resource exists. K-ORC then keeps the RegisteredLimit Available for the new
// generation and reports the refusal on Progressing, so CatalogReady holds and
// relays it instead of reporting a default Keystone has not taken. Once K-ORC
// reports Progressing=False the limit counts as registered.
func TestKSCatalog_RegisteredLimitRefusedUpdateHolds(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 20)
	name := keystoneServiceRegisteredLimitRef(ks, "r1")
	refusal := "Expected HTTP response code [200] when accessing [PATCH /v3/registered_limits/limit-id], but got 403 instead"
	limit := ksRegisteredLimit(ks, "r1", 2, 2)
	limit.Status.Conditions = append(limit.Status.Conditions, metav1.Condition{
		Type: orcv1alpha1.ConditionProgressing, Status: metav1.ConditionTrue,
		Reason: orcv1alpha1.ConditionReasonTransientError, Message: refusal, ObservedGeneration: 2,
	})

	cond, c := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks), limit)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(Equal(`the registered limit "` + name +
		`" has not reached Keystone with its current default yet: ` + refusal))

	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(limit), limit)).To(Succeed())
	apimeta.SetStatusCondition(&limit.Status.Conditions, metav1.Condition{
		Type: orcv1alpha1.ConditionProgressing, Status: metav1.ConditionFalse,
		Reason: orcv1alpha1.ConditionReasonSuccess, ObservedGeneration: limit.Generation,
	})
	g.Expect(c.Update(context.Background(), limit)).To(Succeed())

	r := &KeystoneServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())
	cond = ksCatalogCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogRegistered))
}

// A Progressing=True left over from an older generation does not hold: K-ORC has
// not looked at the current spec through it, and Available for the current
// generation already gates that.
func TestKSCatalog_RegisteredLimitStaleProgressingDoesNotHold(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 20)
	limit := ksRegisteredLimit(ks, "r1", 2, 2)
	limit.Status.Conditions = append(limit.Status.Conditions, metav1.Condition{
		Type: orcv1alpha1.ConditionProgressing, Status: metav1.ConditionTrue,
		Reason: orcv1alpha1.ConditionReasonTransientError, ObservedGeneration: 1,
	})

	cond, _ := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks), limit)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// A registration without registered limits, omitted or empty, projects none and
// keeps the CatalogRegistered message it has always had.
func TestKSCatalog_NoRegisteredLimitsKeepsTheMessage(t *testing.T) {
	for name, limits := range map[string][]c5c3v1alpha1.KeystoneServiceRegisteredLimitSpec{
		"omitted": nil,
		"empty":   {},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			cp := ksControlPlane()
			ks := ksWithCatalog()
			ks.Spec.Catalog.RegisteredLimits = limits
			ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
				{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://image.example/public"},
			}

			cond, c := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks),
				ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypePublic, "ep-public-id"))...)

			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Message).To(Equal(`catalog entry "` + keystoneServiceCatalogName(ks) +
				`" of type "image" is registered with 1 endpoint(s) in region "RegionOne"`))
			var registered orcv1alpha1.RegisteredLimitList
			g.Expect(c.List(context.Background(), &registered)).To(Succeed())
			g.Expect(registered.Items).To(BeEmpty())
		})
	}
}

// While a registered limit is not yet Available the registration is not done, so
// the region-less legacy row an earlier version registered must not be retired
// either.
func TestKSCatalog_PendingRegisteredLimitKeepsTheLegacyEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)
	ks.Spec.Catalog.Endpoints = []c5c3v1alpha1.KeystoneServiceEndpointSpec{
		{Interface: c5c3v1alpha1.ExternalEndpointTypeInternal, URL: "http://glance.svc:9292"},
	}
	legacyName := keystoneServiceLegacyCatalogEndpointRef(ks, c5c3v1alpha1.ExternalEndpointTypeInternal)

	cond, c := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks),
		ksAvailableEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal, "ep-internal-id"),
		ksLegacyEndpoint(ks, c5c3v1alpha1.ExternalEndpointTypeInternal))...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(Equal(`the registered limit "` + keystoneServiceRegisteredLimitRef(ks, "r1") +
		`" is registered but not yet Available`))

	legacy := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: legacyName, Namespace: ks.Namespace}, legacy)).To(Succeed())
	g.Expect(legacy.DeletionTimestamp).To(BeNil(), "the legacy row is retired only once every child is Available")
}

func TestKSCatalog_TerminalRegisteredLimitErrorIsReported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)
	limit := ksRegisteredLimit(ks, "r1", 0, 0)
	limit.Status.Conditions = terminalImportConditions("defaultLimit 0 is not supported")

	cond, _ := runKSCatalog(t, ks, cp, append(ksConvergedCatalog(ks), limit)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(`K-ORC reported a terminal error registering the registered limit "` +
		limit.Name + `": `))
	g.Expect(cond.Message).To(ContainSubstring("defaultLimit 0 is not supported"))
}

func TestKSCatalog_TransportLatchedRegisteredLimitIsRetried(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)
	latched := ksRegisteredLimit(ks, "r1", 0, 0)
	latched.Status.Conditions = transportLatchedConditions()

	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(ksConvergedCatalog(ks), cp, ks, latched)...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}, &orcv1alpha1.Service{}, &orcv1alpha1.RegisteredLimit{}).
		Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	live := &orcv1alpha1.RegisteredLimit{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(latched), live)).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(live.Status.Conditions, orcv1alpha1.ConditionProgressing)).To(BeNil(),
		"the latch must be gone from the live RegisteredLimit so K-ORC reconciles it again")
	cond := ksCatalogCondition(ks)
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog),
		"a cleared child is waited on, not reported as a terminal failure")
}

func TestKSCatalog_RegisteredLimitApplyErrorIsReportedAndReturned(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)

	s := korcTestScheme(t)
	boom := errors.New("registeredlimits.openstack.k-orc.cloud is forbidden")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(ksConvergedCatalog(ks), cp, ks)...).
		WithStatusSubresource(&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if u, ok := obj.(interface{ GetKind() string }); ok && u.GetKind() == "RegisteredLimit" {
					return boom
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := &KeystoneServiceReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)

	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, boom)).To(BeTrue())
	cond := ksCatalogCondition(ks)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonKeystoneServiceCatalogError))
	g.Expect(cond.Message).To(HavePrefix(`applying the "r1" registered limit: `))
	g.Expect(cond.Message).To(ContainSubstring(boom.Error()))
}

// K-ORC updates defaultLimit in place, so an edit re-applies the same CR rather
// than replacing it, which would delete and re-create the Keystone row.
func TestKSCatalog_ChangedDefaultLimitReappliesTheSameCR(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := ksControlPlane()
	ks := ksWithRegisteredLimit("r1", 10)
	_, c := runKSCatalog(t, ks, cp, ksConvergedCatalog(ks)...)

	key := types.NamespacedName{Name: keystoneServiceRegisteredLimitRef(ks, "r1"), Namespace: ks.Namespace}
	before := &orcv1alpha1.RegisteredLimit{}
	g.Expect(c.Get(context.Background(), key, before)).To(Succeed())

	ks.Spec.Catalog.RegisteredLimits[0].DefaultLimit = 20
	r := &KeystoneServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(20)}
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	_, err := r.ensureCatalog(context.Background(), ks, cp, credRef, managedCredRef)
	g.Expect(err).NotTo(HaveOccurred())

	after := &orcv1alpha1.RegisteredLimit{}
	g.Expect(c.Get(context.Background(), key, after)).To(Succeed())
	g.Expect(after.UID).To(Equal(before.UID))
	g.Expect(after.DeletionTimestamp).To(BeNil())
	g.Expect(after.Spec.Resource.DefaultLimit).To(Equal(ptr.To(int32(20))))
	g.Expect(keystoneServiceDeclaredChildNames(ks)).To(HaveKey(key.Name))
}
