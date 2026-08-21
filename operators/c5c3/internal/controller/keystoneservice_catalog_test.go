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

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
