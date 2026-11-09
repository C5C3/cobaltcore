// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the import-first External-mode catalog branch reconcileCatalogExternal.
package controller

import (
	"context"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v3/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// --- fixtures ---

// externalCatalogControlPlane returns an External-mode ControlPlane whose
// AdminCredentialReady gate is already satisfied, so reconcileCatalog forks
// straight into the import branch.
func externalCatalogControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcExternalControlPlane()
	setAdminCredentialReady(cp)
	return cp
}

// reconcileCatalogFor runs reconcileCatalog against cp with the given seeded K-ORC
// CRs and returns the resulting CatalogReady condition together with the client,
// so tests can assert on what was (and was not) created.
func reconcileCatalogFor(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object,
) (*metav1.Condition, client.Client) {
	t.Helper()
	g := NewGomegaWithT(t)

	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, objs...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady), c
}

// availableImportConditions stamps the Available=True condition K-ORC reports once
// an import matched a live catalog entry. Its ObservedGeneration is left at zero to
// match the generation the fake client assigns, which is what korcAvailableUpToDate
// compares against.
func availableImportConditions() []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		Message:            "resolved",
		LastTransitionTime: metav1.Now(),
	}}
}

// pendingImportConditions stamps the silent-empty state: Available=False on the
// "created externally" marker, transitioned age ago.
func pendingImportConditions(age time.Duration) []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonProgressing,
		Message:            korcImportPendingExternalMarker,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
	}}
}

// waitingOnDependencyConditions stamps the state K-ORC gives an import whose
// referenced CR is not Available yet: Progressing=True and Available=False, both
// on reason Progressing with K-ORC's "Waiting for <kind>/<name> to be ready"
// message. Both transitioned longer ago than externalImportStallGrace, so a
// stall detector that took the wait for the pending-external marker would fire.
func waitingOnDependencyConditions(kind, name string) []metav1.Condition {
	message := "Waiting for " + kind + "/" + name + " to be ready"
	since := metav1.NewTime(time.Now().Add(-(externalImportStallGrace + time.Minute)))
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonProgressing,
		Message:            message,
		LastTransitionTime: since,
	}, {
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonProgressing,
		Message:            message,
		LastTransitionTime: since,
	}}
}

// terminalImportConditions stamps a terminal K-ORC failure: Progressing=False with
// the InvalidConfiguration reason, which is what GetTerminalError keys on.
func terminalImportConditions(msg string) []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonInvalidConfiguration,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	}}
}

// transportLatchedConditions stamps the latch this package unlatches: an
// InvalidConfiguration Progressing=False whose message is the verbatim dial error
// K-ORC recorded when Keystone was not listening. Its ObservedGeneration is left
// at zero to match the generation the fake client assigns, which is what
// korcTerminalReason compares against. The transition time is cosmetic: the
// backoff reads korcTransportUnlatchedAtAnnotation, never this.
func transportLatchedConditions() []metav1.Condition {
	return []metav1.Condition{{
		Type:   orcv1alpha1.ConditionProgressing,
		Status: metav1.ConditionFalse,
		Reason: orcv1alpha1.ConditionReasonInvalidConfiguration,
		Message: `invalid configuration creating resource: Post ` +
			`"http://keystone.brownfield-keystone.svc:5000/v3/projects": ` +
			`dial tcp 10.96.227.194:5000: connect: connection refused`,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
	}}
}

// unrecoverableImportConditions stamps K-ORC's OTHER terminal reason: not "the user
// must fix the configuration" but "this can never succeed". The import branch keys
// its optional-import tolerance on the reason, so the two are not interchangeable.
func unrecoverableImportConditions(msg string) []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonUnrecoverableError,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	}}
}

// transientEntryConditions stamps the shape EVERY hard failure against the external
// Keystone takes on a KeystoneService child: a non-terminal Progressing=True with
// reason=TransientError, carrying the only description of what actually went wrong.
func transientEntryConditions(msg string) []metav1.Condition {
	return []metav1.Condition{{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonTransientError,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	}}
}

func importedIdentityService(cp *c5c3v1alpha1.ControlPlane, conds []metav1.Condition, id string) *orcv1alpha1.Service {
	svc := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)},
		Status:     orcv1alpha1.ServiceStatus{Conditions: conds},
	}
	if id != "" {
		svc.Status.ID = ptr.To(id)
	}
	return svc
}

// importedIdentityRegion returns the Region import of spec.region the identity
// Endpoint imports name as their regionRef.
func importedIdentityRegion(cp *c5c3v1alpha1.ControlPlane, conds []metav1.Condition, id string) *orcv1alpha1.Region {
	region := &orcv1alpha1.Region{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneRegionName(cp), Namespace: childNamespace(cp)},
		Status:     orcv1alpha1.RegionStatus{Conditions: conds},
	}
	if id != "" {
		region.Status.ID = ptr.To(id)
	}
	return region
}

// resolvedIdentityRegion is the Region import K-ORC resolved to spec.region; a
// Keystone region id is its name.
func resolvedIdentityRegion(cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.Region {
	return importedIdentityRegion(cp, availableImportConditions(), korcRegion(cp))
}

func importedIdentityEndpoint(
	cp *c5c3v1alpha1.ControlPlane, iface c5c3v1alpha1.ExternalEndpointType, conds []metav1.Condition, id string,
) *orcv1alpha1.Endpoint {
	ep := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp)},
		Status:     orcv1alpha1.EndpointStatus{Conditions: conds},
	}
	if id != "" {
		ep.Status.ID = ptr.To(id)
	}
	return ep
}

// resolvedIdentityCatalog returns the five import CRs (Service, Region and three
// Endpoints) all reporting Available with a resolved id — the converged
// External-mode catalog. The Service stays at index 0 and the Region at index 1.
func resolvedIdentityCatalog(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
	}
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, importedIdentityEndpoint(cp, iface, availableImportConditions(), "ep-"+string(iface)))
	}
	return objs
}

// --- the default posture: import everything, create nothing ---

// TestReconcileCatalogExternal_ImportsServiceAndAllEndpointInterfaces is the
// headline acceptance criterion: pointed at a populated catalog, External mode
// creates ZERO catalog entries and instead imports the identity Service, the
// region spec.region names, and all three endpoint interfaces in that region as
// unmanaged, read-only K-ORC CRs.
func TestReconcileCatalogExternal_ImportsServiceAndAllEndpointInterfaces(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	cond, c := reconcileCatalogFor(t, cp)

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, svc)).To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"the identity Service must be imported, never created")
	g.Expect(svc.Spec.Resource).To(BeNil(), "an unmanaged import must declare no desired resource")
	g.Expect(svc.Spec.Import).NotTo(BeNil())
	g.Expect(svc.Spec.Import.Filter.Type).To(HaveValue(Equal(c5c3v1alpha1.IdentityCatalogServiceType)))
	g.Expect(svc.Spec.Import.Filter.Name).To(BeNil(), "no disambiguation filter is configured")
	g.Expect(metav1.IsControlledBy(svc, cp)).To(BeTrue())

	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(context.Background(),
			client.ObjectKey{Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp)}, ep)).
			To(Succeed(), "the %q endpoint interface must be imported", iface)
		g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged))
		g.Expect(ep.Spec.Resource).To(BeNil())
		g.Expect(ep.Spec.Import.Filter.Interface).To(Equal(string(iface)))
		g.Expect(ep.Spec.Import.Filter.ServiceRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp)))))
		g.Expect(ep.Spec.Import.Filter.RegionRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))),
			"the %q import must select the row in spec.region", iface)
		g.Expect(metav1.IsControlledBy(ep, cp)).To(BeTrue())
	}

	// No managed CR of either kind exists: zero catalog entries were created.
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(context.Background(), &services, client.InNamespace(childNamespace(cp)))).To(Succeed())
	for _, item := range services.Items {
		g.Expect(item.Spec.ManagementPolicy).NotTo(Equal(orcv1alpha1.ManagementPolicyManaged),
			"External mode must create no managed Service by default")
	}
	var endpoints orcv1alpha1.EndpointList
	g.Expect(c.List(context.Background(), &endpoints, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(endpoints.Items).To(HaveLen(len(externalCatalogInterfaces)))
	for _, item := range endpoints.Items {
		g.Expect(item.Spec.ManagementPolicy).NotTo(Equal(orcv1alpha1.ManagementPolicyManaged),
			"External mode must create no managed Endpoint by default")
	}
	var regions orcv1alpha1.RegionList
	g.Expect(c.List(context.Background(), &regions, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(regions.Items).To(HaveLen(1), "External mode imports exactly the region spec.region names")
	g.Expect(regions.Items[0].Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyUnmanaged),
		"External mode must create no managed Region")

	// Freshly created imports carry no status yet, so the catalog is not Ready.
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
}

// TestReconcileCatalogExternal_ResolvedImportsFlipCatalogImported proves the
// success path reports the dedicated CatalogImported reason (never
// CatalogRegistered — nothing was registered) and projects every resolved import.
func TestReconcileCatalogExternal_ResolvedImportsFlipCatalogImported(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	cond, _ := reconcileCatalogFor(t, cp, resolvedIdentityCatalog(cp)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	g.Expect(cond.Message).To(ContainSubstring(`3 of 3 Endpoint interface(s) in region "RegionOne"`))

	g.Expect(cp.Status.Catalog).NotTo(BeNil())
	g.Expect(cp.Status.Catalog.Imports).To(HaveLen(2 + len(externalCatalogInterfaces)))

	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	svc := byName[keystoneServiceName(cp)]
	g.Expect(svc.Kind).To(Equal("Service"))
	g.Expect(svc.Resolved).To(BeTrue())
	g.Expect(svc.ID).To(Equal("svc-id"))
	g.Expect(svc.Interface).To(BeEmpty(), "the Service import carries no interface")

	g.Expect(byName[keystoneRegionName(cp)]).To(Equal(c5c3v1alpha1.CatalogImportStatus{
		Name: keystoneRegionName(cp), Kind: "Region", Interface: "", Resolved: true, ID: "RegionOne",
	}))

	for _, iface := range externalCatalogInterfaces {
		ep := byName[keystoneEndpointImportName(cp, iface)]
		g.Expect(ep.Kind).To(Equal("Endpoint"))
		g.Expect(ep.Interface).To(Equal(iface))
		g.Expect(ep.Resolved).To(BeTrue())
		g.Expect(ep.ID).To(Equal("ep-" + string(iface)))
	}
}

// TestReconcileCatalogExternal_IdentityServiceNameProjectsFilter proves the
// disambiguation filter reaches K-ORC.
func TestReconcileCatalogExternal_IdentityServiceNameProjectsFilter(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
		IdentityServiceName: "keystone-legacy",
	}
	_, c := reconcileCatalogFor(t, cp)

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(context.Background(),
		client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, svc)).To(Succeed())
	g.Expect(svc.Spec.Import.Filter.Name).To(HaveValue(Equal(orcv1alpha1.OpenStackName("keystone-legacy"))))
	g.Expect(svc.Spec.Import.Filter.Type).To(HaveValue(Equal(c5c3v1alpha1.IdentityCatalogServiceType)))
}

func TestReconcileCatalogExternal_GatedOnAdminCredential(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := korcExternalControlPlane() // AdminCredentialReady absent
	cond, c := reconcileCatalogFor(t, cp)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForAdminCredential"))

	// The gate fires before ANY import is reconciled — a ControlPlane that cannot
	// authenticate must not leave K-ORC CRs behind.
	err := c.Get(context.Background(),
		client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, &orcv1alpha1.Service{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(cp.Status.Catalog).To(BeNil())
}

// --- fail loudly: 0 matches (silent-empty) and >1 matches (ambiguous) ---

// TestReconcileCatalogExternal_StalledImportSurfacesImportStalled covers the
// silent-empty hazard the spike characterized: an import that matches nothing sits
// on the pending-external marker forever, indistinguishable by conditions from an
// import that is about to resolve. Past the grace window it must fail loud.
func TestReconcileCatalogExternal_StalledImportSurfacesImportStalled(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	stalled := importedIdentityService(cp, pendingImportConditions(externalImportStallGrace+time.Minute), "")
	cond, _ := reconcileCatalogFor(t, cp, stalled)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImportStalled))
	g.Expect(cond.Message).To(ContainSubstring("endpointType"), "the message must name the likely cause")
	g.Expect(cond.Message).To(ContainSubstring("spec.region"), "the message must name the likely cause")
	g.Expect(cond.Message).To(ContainSubstring(keystoneServiceName(cp)), "the message must name the stuck import")

	// The import is still projected, reported as unresolved rather than omitted.
	g.Expect(cp.Status.Catalog.Imports[0].Resolved).To(BeFalse())
	g.Expect(cp.Status.Catalog.Imports[0].ID).To(BeEmpty())
}

// TestReconcileCatalogExternal_StalledEndpointNamesMissingInterface proves an
// Endpoint import that matched nothing names the third possibility no spec edit
// can fix: the external catalog publishes no such interface. Only the
// authenticating interface is gated on, so that is the one stalled here.
func TestReconcileCatalogExternal_StalledEndpointNamesMissingInterface(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{importedIdentityService(cp, availableImportConditions(), "svc-id"), resolvedIdentityRegion(cp)}
	objs = append(
		objs,
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			pendingImportConditions(externalImportStallGrace+time.Minute), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal, availableImportConditions(), "ep-internal"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, availableImportConditions(), "ep-admin"),
	)
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonImportStalled))
	g.Expect(cond.Message).To(ContainSubstring(`no "public" endpoint for the identity service in region "RegionOne"`))
}

// TestReconcileCatalogExternal_StalledRegionImportSurfacesImportStalled proves a
// Region import that matched nothing fails loud instead of leaving every Endpoint
// import waiting on it: the external Keystone has no region of the name
// spec.region carries. The Endpoint imports carry what K-ORC gives them then, a
// wait on the Region, and the condition names the Region rather than any of them.
func TestReconcileCatalogExternal_StalledRegionImportSurfacesImportStalled(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		importedIdentityRegion(cp, pendingImportConditions(externalImportStallGrace+time.Minute), ""),
	}
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, importedIdentityEndpoint(cp, iface,
			waitingOnDependencyConditions("Region", keystoneRegionName(cp)), ""))
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonImportStalled))
	g.Expect(cond.Message).To(ContainSubstring(keystoneRegionName(cp)), "the message must name the stuck import")
	g.Expect(cond.Message).To(ContainSubstring("spec.region"))
	g.Expect(cond.Message).To(ContainSubstring(`no region "RegionOne"`))
	for _, iface := range externalCatalogInterfaces {
		g.Expect(cond.Message).NotTo(ContainSubstring(keystoneEndpointImportName(cp, iface)),
			"an Endpoint import merely waiting on the Region is not the root")
	}
}

// TestReconcileCatalogExternal_UnpublishedInterfacesDoNotBlockReady is the
// brownfield posture External mode exists to adopt: a Keystone that publishes
// only the interface the control plane authenticates against. kolla-ansible
// stopped registering the identity `admin` endpoint after Zed, and a devstack
// bootstrapped with only a public URL publishes neither of the other two — so
// their imports stall on the pending-external marker forever, by design. Gating
// CatalogReady on them would hold the aggregate Ready False for the two most
// common brownfield deployment tools.
func TestReconcileCatalogExternal_UnpublishedInterfacesDoNotBlockReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	stalled := pendingImportConditions(externalImportStallGrace + time.Minute)
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal, stalled, ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, stalled, ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	g.Expect(cond.Message).To(ContainSubstring("1 of 3 Endpoint interface(s)"),
		"the message must report what resolved, not claim all three did")

	// The unpublished interfaces are surfaced, not hidden: they are projected as
	// unresolved so an operator can see the asymmetry the condition tolerates.
	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	g.Expect(byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypePublic)].Resolved).To(BeTrue())
	g.Expect(byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypeInternal)].Resolved).To(BeFalse())
	g.Expect(byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin)].Resolved).To(BeFalse())
}

// TestReconcileCatalogExternal_RequiredInterfaceFollowsEndpointType proves the
// gated interface is the one the control plane authenticates through, not a fixed
// "public": stalling `public` is tolerated when endpointType is `internal`, and
// stalling `internal` is not.
func TestReconcileCatalogExternal_RequiredInterfaceFollowsEndpointType(t *testing.T) {
	stalled := func() []metav1.Condition { return pendingImportConditions(externalImportStallGrace + time.Minute) }

	t.Run("the unselected interface may stall", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp := externalCatalogControlPlane()
		cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
		objs := []client.Object{
			importedIdentityService(cp, availableImportConditions(), "svc-id"),
			resolvedIdentityRegion(cp),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, stalled(), ""),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal, availableImportConditions(), "ep-internal"),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, stalled(), ""),
		}
		cond, _ := reconcileCatalogFor(t, cp, objs...)

		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	})

	t.Run("the selected interface may not", func(t *testing.T) {
		g := NewGomegaWithT(t)

		cp := externalCatalogControlPlane()
		cp.Spec.Services.Keystone.External.EndpointType = c5c3v1alpha1.ExternalEndpointTypeInternal
		objs := []client.Object{
			importedIdentityService(cp, availableImportConditions(), "svc-id"),
			resolvedIdentityRegion(cp),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal, stalled(), ""),
			importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, availableImportConditions(), "ep-admin"),
		}
		cond, _ := reconcileCatalogFor(t, cp, objs...)

		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonImportStalled))
		g.Expect(cond.Message).To(ContainSubstring(`no "internal" endpoint`))
	})
}

// TestReconcileCatalogExternal_StalledInsideGraceStaysWaiting proves the grace
// window is honoured: a fresh pending import is a legitimate wait, not a failure.
func TestReconcileCatalogExternal_StalledInsideGraceStaysWaiting(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	fresh := importedIdentityService(cp, pendingImportConditions(time.Second), "")
	cond, _ := reconcileCatalogFor(t, cp, fresh)

	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
}

// TestReconcileCatalogExternal_StaleImportReportsNoID pins the status contract
// that an unresolved import carries no id. A CR whose current spec K-ORC has not
// run yet keeps the status.id of the filter it resolved before, as a re-imported
// Endpoint does while K-ORC still holds the old CR, and projecting that id would
// report a row the current filter may not select.
func TestReconcileCatalogExternal_StaleImportReportsNoID(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	stale := importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal,
		availableImportConditions(), "ep-internal-previous-filter")
	stale.Generation = 1 // availableImportConditions is for generation 0
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
		stale,
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	internal := byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypeInternal)]
	g.Expect(internal.Resolved).To(BeFalse())
	g.Expect(internal.ID).To(BeEmpty(), "an unresolved import must not report the id of its previous filter")
	g.Expect(byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypePublic)].ID).To(Equal("ep-public"))
}

// TestReconcileCatalogExternal_UnresolvedRegionImportWaits proves the Region import
// gates CatalogReady: every Endpoint import names it as its regionRef, so the
// catalog is not Ready until it resolves, even with every other import resolved.
func TestReconcileCatalogExternal_UnresolvedRegionImportWaits(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := resolvedIdentityCatalog(cp)
	objs[1] = importedIdentityRegion(cp, nil, "")
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(keystoneRegionName(cp)))

	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	g.Expect(byName).To(HaveKey(keystoneRegionName(cp)))
	g.Expect(byName[keystoneRegionName(cp)].Resolved).To(BeFalse())
	g.Expect(byName[keystoneRegionName(cp)].ID).To(BeEmpty())
}

// TestReconcileCatalogExternal_AmbiguousImportFailsLoud is the duplicate-name
// catalog: two identity services match the filter, K-ORC refuses to guess and goes
// terminal. The condition must relay that verbatim and point at the disambiguation
// filter — never quiet success, never import-all.
func TestReconcileCatalogExternal_AmbiguousImportFailsLoud(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	ambiguous := importedIdentityService(cp, terminalImportConditions(korcImportMultipleMatchesMarker), "")
	cond, _ := reconcileCatalogFor(t, cp, ambiguous)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(korcImportMultipleMatchesMarker), "K-ORC's message must be relayed verbatim")
	g.Expect(cond.Message).To(ContainSubstring("spec.services.keystone.external.catalog.identityServiceName"))
	g.Expect(cond.Message).To(ContainSubstring(`type=identity`), "the effective filter must be named")
}

// TestReconcileCatalogExternal_AmbiguousImportNamesEffectiveFilter proves the hint
// reports the CONFIGURED filter, so an operator who already set identityServiceName
// learns that even that was not enough (two identically named services).
func TestReconcileCatalogExternal_AmbiguousImportNamesEffectiveFilter(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{IdentityServiceName: "keystone"}
	ambiguous := importedIdentityService(cp, terminalImportConditions(korcImportMultipleMatchesMarker), "")
	cond, _ := reconcileCatalogFor(t, cp, ambiguous)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(`type=identity, name="keystone"`))
}

// TestReconcileCatalogExternal_AmbiguousEndpointNamesDuplicateRowsInRegion proves
// a multi-match on an ENDPOINT import does not point at identityServiceName (which
// would not help): the filter already selects the row in spec.region, so a >1
// match means duplicate rows of one interface inside that region, which only a
// repair of the external catalog removes.
func TestReconcileCatalogExternal_AmbiguousEndpointNamesDuplicateRowsInRegion(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			terminalImportConditions(korcImportMultipleMatchesMarker), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(`in region "RegionOne"`))
	g.Expect(cond.Message).To(ContainSubstring("remove the duplicate"))
	g.Expect(cond.Message).NotTo(ContainSubstring("identityServiceName"),
		"no spec field selects among duplicate rows, so do not point at a field that would not help")
	g.Expect(cond.Message).NotTo(ContainSubstring("one per region"))
	g.Expect(cond.Message).NotTo(ContainSubstring("cannot select"))
}

// TestReconcileCatalogExternal_AmbiguousRegionNamesRegionFilter proves a
// multi-match on the Region import names the Region filter and spec.region, not
// the identity Service filter field.
func TestReconcileCatalogExternal_AmbiguousRegionNamesRegionFilter(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := resolvedIdentityCatalog(cp)
	objs[1] = importedIdentityRegion(cp, terminalImportConditions(korcImportMultipleMatchesMarker), "")
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(`Region import filter (name="RegionOne", spec.region)`))
	g.Expect(cond.Message).NotTo(ContainSubstring("identityServiceName"))
}

// TestReconcileCatalogExternal_RewordedAmbiguityOnOptionalInterfaceDoesNotWedge is
// the guard on the marker coupling. korcImportMultipleMatchesMarker is K-ORC's
// literal wording, and a K-ORC bump may reword it at any time. If the tolerate branch
// keyed on that string, the reword would silently promote the duplicate-row
// ambiguity below into a permanent CatalogReady=False — and therefore Ready=False —
// on a healthy control plane, over an interface nothing depends on and with no spec
// edit able to repair it. Every other test feeds the constant back to itself,
// so only this one would catch the regression. The tolerance is keyed on K-ORC's
// machine-readable InvalidConfiguration reason instead; the message selects the hint.
func TestReconcileCatalogExternal_RewordedAmbiguityOnOptionalInterfaceDoesNotWedge(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal,
			terminalImportConditions("import filter matched multiple OpenStack resources"), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, availableImportConditions(), "ep-admin"),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
		"a reworded K-ORC message must not wedge CatalogReady on an optional import")
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
}

// TestReconcileCatalogExternal_AmbiguousOptionalInterfaceDoesNotBlockReady is the
// other half of the duplicate-row case the test above names. A Keystone whose
// `public` endpoint resolves cleanly but which registers its `internal` endpoint
// twice inside spec.region makes the region-scoped EndpointFilter match several
// rows and go terminal — on an interface nothing in this control plane
// authenticates through, and which ambiguityHint itself says no spec edit can
// disambiguate.
// Gating CatalogReady on it would hold the aggregate Ready False forever with no
// remediation, so it is tolerated exactly like the unpublished interfaces above
// and surfaced through status.catalog.imports instead.
func TestReconcileCatalogExternal_AmbiguousOptionalInterfaceDoesNotBlockReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		resolvedIdentityRegion(cp),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal,
			terminalImportConditions(korcImportMultipleMatchesMarker), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, availableImportConditions(), "ep-admin"),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	g.Expect(cond.Message).To(ContainSubstring("2 of 3 Endpoint interface(s)"))

	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	g.Expect(byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypeInternal)].Resolved).To(BeFalse(),
		"the ambiguous interface must be surfaced as unresolved, not hidden")
}

// TestReconcileCatalogExternal_UnrecoverableErrorOnOptionalInterfaceFailsLoud bounds
// the exception above to the one error class that has no remediation. K-ORC has
// exactly two terminal reasons, and only InvalidConfiguration — "the user must fix
// the configuration" — is tolerated on an optional import, because a non-required
// import has no user-supplied configuration to fix. An UnrecoverableError still gates
// CatalogReady on every import, required or not: K-ORC has given up for a reason that
// is not about the spec, which is loud and actionable.
func TestReconcileCatalogExternal_UnrecoverableErrorOnOptionalInterfaceFailsLoud(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic, availableImportConditions(), "ep-public"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal,
			unrecoverableImportConditions("endpoint is broken"), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("endpoint is broken"))
}

// TestReconcileCatalogExternal_TerminalErrorOnRequiredImportAlwaysFailsLoud pins the
// other side of the reason-keyed tolerance: the SAME InvalidConfiguration that is
// tolerated on an optional interface gates when it lands on the interface the control
// plane authenticates through, because that catalog is not the one K-ORC was pointed at.
func TestReconcileCatalogExternal_TerminalErrorOnRequiredImportAlwaysFailsLoud(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			terminalImportConditions("import filter matched multiple OpenStack resources"), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
}

// TestReconcileCatalogExternal_TransportLatchedRequiredImportIsRetried covers the
// import site. K-ORC latched the required public Endpoint import on a connection
// refused a minute ago and never revisits it, so the pass clears the latch and
// falls through to the bounded wait instead of reporting a terminal failure or
// relaying the dial error as EndpointUnreachable.
func TestReconcileCatalogExternal_TransportLatchedRequiredImportIsRetried(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	latched := importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
		transportLatchedConditions(), "")

	// Both K-ORC kinds carry a status subresource here: the fake client answers a
	// status write on a kind it does not carry with NotFound, which the unlatch
	// helper reads as "the child vanished" and skips.
	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, importedIdentityService(cp, availableImportConditions(), "svc-id"), latched).
		WithStatusSubresource(&orcv1alpha1.Service{}, &orcv1alpha1.Endpoint{}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileCatalog(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	g.Expect(ksLiveProgressing(t, c, &orcv1alpha1.Endpoint{},
		keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypePublic), childNamespace(cp))).To(BeNil(),
		"the latch must be gone from the live child so K-ORC reconciles it again")

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog),
		"a cleared import is waited on, not reported as a terminal failure")
	g.Expect(cond.Reason).NotTo(Equal(conditionReasonEndpointUnreachable))
}

// TestReconcileCatalogExternal_TerminalServiceBeatsTerminalEndpoint pins the
// dependency order: the ROOT failure is reported, not the Endpoint merely blocked
// on the Service it references.
func TestReconcileCatalogExternal_TerminalServiceBeatsTerminalEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := []client.Object{
		importedIdentityService(cp, terminalImportConditions("service is broken"), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			terminalImportConditions("endpoint is broken"), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("service is broken"))
	g.Expect(cond.Message).NotTo(ContainSubstring("endpoint is broken"))
}

// TestReconcileCatalogExternal_TerminalRegionBeatsTerminalEndpoint pins the Region
// between the Service and the Endpoints in the dependency order: a terminal Region
// import is the root an Endpoint import that names it is blocked on.
func TestReconcileCatalogExternal_TerminalRegionBeatsTerminalEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		importedIdentityRegion(cp, unrecoverableImportConditions("region is broken"), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			terminalImportConditions("endpoint is broken"), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring(`Region "` + keystoneRegionName(cp) + `"`))
	g.Expect(cond.Message).To(ContainSubstring("region is broken"))
	g.Expect(cond.Message).NotTo(ContainSubstring("endpoint is broken"))
}

// TestReconcileCatalogExternal_ClassifiableMessageSurfaced proves a K-ORC message
// that identifies a failure CLASS is relayed with that class rather than collapsed
// into a generic wait — the wrong-endpointType hazard cannot look like progress.
func TestReconcileCatalogExternal_ClassifiableMessageSurfaced(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	svc := importedIdentityService(cp, []metav1.Condition{{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             orcv1alpha1.ConditionReasonTransientError,
		Message:            "No suitable endpoint could be found in the service catalog",
		LastTransitionTime: metav1.Now(),
	}}, "")
	cond, _ := reconcileCatalogFor(t, cp, svc)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogEndpointMismatch))
	g.Expect(cond.Message).To(ContainSubstring("No suitable endpoint could be found"))
	g.Expect(cond.Message).To(ContainSubstring(catalogEndpointMismatchHint(cp)))
}

// TestReconcileCatalogExternal_ResolvedImportKeepsStaleMessageQuiet proves a
// converged catalog is never re-classified from a leftover Progressing message
// K-ORC left behind from an attempt it has since recovered from.
func TestReconcileCatalogExternal_ResolvedImportKeepsStaleMessageQuiet(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := resolvedIdentityCatalog(cp)
	svc := objs[0].(*orcv1alpha1.Service)
	svc.Status.Conditions = append(svc.Status.Conditions, metav1.Condition{
		Type:               orcv1alpha1.ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             orcv1alpha1.ConditionReasonSuccess,
		Message:            "401 Unauthorized",
		LastTransitionTime: metav1.Now(),
	})
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
}

// preRegionEndpointImport returns the Endpoint import an earlier version created
// for iface: owned by the ControlPlane, with a filter of interface and Service
// only.
func preRegionEndpointImport(
	t *testing.T, s *runtime.Scheme, cp *c5c3v1alpha1.ControlPlane, iface c5c3v1alpha1.ExternalEndpointType,
	conds []metav1.Condition, id string,
) *orcv1alpha1.Endpoint {
	t.Helper()
	ep := importedIdentityEndpoint(cp, iface, conds, id)
	ep.UID = types.UID("pre-region-" + string(iface))
	ep.Spec = orcv1alpha1.EndpointSpec{
		ManagementPolicy: orcv1alpha1.ManagementPolicyUnmanaged,
		Import: &orcv1alpha1.EndpointImport{Filter: &orcv1alpha1.EndpointFilter{
			Interface:  string(iface),
			ServiceRef: ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp))),
		}},
	}
	if err := controllerutil.SetControllerReference(cp, ep, s); err != nil {
		t.Fatalf("owning the pre-region %q import: %v", iface, err)
	}
	return ep
}

// resolveRegionImport stamps what K-ORC writes once the Region import of
// spec.region resolved.
func resolveRegionImport(ctx context.Context, g Gomega, c client.Client, cp *c5c3v1alpha1.ControlPlane) {
	region := &orcv1alpha1.Region{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneRegionName(cp), Namespace: childNamespace(cp)}, region)).To(Succeed())
	region.Status.Conditions = availableImportConditions()
	region.Status.Conditions[0].ObservedGeneration = region.Generation
	region.Status.ID = ptr.To(korcRegion(cp))
	g.Expect(c.Update(ctx, region)).To(Succeed())
}

// TestReconcileCatalogExternal_UpgradeReimportsEndpointsResolvedWithoutRegion is
// the upgrade of a plane whose Endpoint imports an earlier version created with a
// filter of interface and Service only, and which K-ORC resolved against it.
// K-ORC never runs a filter again once status.id is set, so the pass replaces
// each of them with a fresh import under the regioned filter. The external
// catalog here publishes `internal` in another region only: the old filter
// resolved it, the regioned one does not, and the catalog is Ready regardless,
// because internal is not the interface the plane authenticates through.
func TestReconcileCatalogExternal_UpgradeReimportsEndpointsResolvedWithoutRegion(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{cp, importedIdentityService(cp, availableImportConditions(), "svc-id")}
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, preRegionEndpointImport(t, s, cp, iface, availableImportConditions(), "pre-region-"+string(iface)))
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	endpointKey := func(iface c5c3v1alpha1.ExternalEndpointType) client.ObjectKey {
		return client.ObjectKey{Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp)}
	}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(ctx, endpointKey(iface), ep)).To(Succeed())
		g.Expect(ep.UID).NotTo(Equal(types.UID("pre-region-"+string(iface))),
			"the %q import K-ORC resolved without a region must be re-imported, not patched", iface)
		g.Expect(ep.Status.ID).To(BeNil(), "the re-imported %q CR carries no id K-ORC resolved without a region", iface)
		g.Expect(ep.Spec.Import.Filter.RegionRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))))
		g.Expect(metav1.IsControlledBy(ep, cp)).To(BeTrue())
	}
	for _, imp := range cp.Status.Catalog.Imports {
		if imp.Kind == "Endpoint" {
			g.Expect(imp.Resolved).To(BeFalse(), "%s reads as unresolved until K-ORC resolves the new CR", imp.Name)
			g.Expect(imp.ID).To(BeEmpty(), "%s must not report the id resolved without a region", imp.Name)
		}
	}
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))

	// K-ORC resolves the Region and runs the regioned filters: the public and admin
	// rows sit in RegionOne, the only internal row sits in another region.
	resolveRegionImport(ctx, g, c, cp)
	inRegion := map[c5c3v1alpha1.ExternalEndpointType]string{
		c5c3v1alpha1.ExternalEndpointTypePublic: "ep-public-regionone",
		c5c3v1alpha1.ExternalEndpointTypeAdmin:  "ep-admin-regionone",
	}
	for _, iface := range externalCatalogInterfaces {
		ep := &orcv1alpha1.Endpoint{}
		g.Expect(c.Get(ctx, endpointKey(iface), ep)).To(Succeed())
		if id, ok := inRegion[iface]; ok {
			ep.Status.Conditions = availableImportConditions()
			ep.Status.Conditions[0].ObservedGeneration = ep.Generation
			ep.Status.ID = ptr.To(id)
		} else {
			ep.Status.Conditions = pendingImportConditions(externalImportStallGrace + time.Minute)
		}
		g.Expect(c.Update(ctx, ep)).To(Succeed())
	}

	_, err = r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
	g.Expect(cond.Message).To(ContainSubstring(`2 of 3 Endpoint interface(s) in region "RegionOne"`))

	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	public := byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypePublic)]
	g.Expect(public.Resolved).To(BeTrue())
	g.Expect(public.ID).To(Equal("ep-public-regionone"), "a re-imported CR is kept on the next pass")
	internal := byName[keystoneEndpointImportName(cp, c5c3v1alpha1.ExternalEndpointTypeInternal)]
	g.Expect(internal.Resolved).To(BeFalse(), "internal has no row in spec.region")
	g.Expect(internal.ID).To(BeEmpty())
}

// TestReconcileCatalogExternal_UpgradeReimportsEndpointsResolvingWithoutRegion
// is the upgrade of a plane whose region-less Endpoint import K-ORC is resolving
// for the first time while the pass runs: the pass reads the CR without an id,
// and K-ORC then records the row its region-less filter matched, which here sits
// in another region. K-ORC binds that write to the CR it read, so the pass must
// re-import the CR rather than add the regionRef to it, or the regioned import
// would keep the out-of-region row for good.
func TestReconcileCatalogExternal_UpgradeReimportsEndpointsResolvingWithoutRegion(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	internal := preRegionEndpointImport(t, s, cp, c5c3v1alpha1.ExternalEndpointTypeInternal, nil, "")
	internalKey := client.ObjectKeyFromObject(internal)

	// The interceptor plays K-ORC landing its region-less resolution between the
	// pass's read and its apply. Like K-ORC's status apply, the write carries the
	// UID of the CR K-ORC resolved, so it never reaches a re-created CR.
	korcResolved := false
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, importedIdentityService(cp, availableImportConditions(), "svc-id"), internal).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, wc client.WithWatch, ac runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if named, ok := ac.(interface{ GetName() string }); ok && named.GetName() == internalKey.Name && !korcResolved {
					korcResolved = true
					live := &orcv1alpha1.Endpoint{}
					err := wc.Get(ctx, internalKey, live)
					if client.IgnoreNotFound(err) != nil {
						return err
					}
					if err == nil && live.UID == internal.UID {
						live.Status.Conditions = availableImportConditions()
						live.Status.ID = ptr.To("ep-internal-regiontwo")
						if err := wc.Update(ctx, live); err != nil {
							return err
						}
					}
				}
				return wc.Apply(ctx, ac, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(korcResolved).To(BeTrue())

	live := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, internalKey, live)).To(Succeed())
	g.Expect(live.Spec.Import.Filter.RegionRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))))
	g.Expect(live.Status.ID).To(BeNil(), "the regioned import must not carry the row resolved without a region")
	for _, imp := range cp.Status.Catalog.Imports {
		if imp.Name == internalKey.Name {
			g.Expect(imp.Resolved).To(BeFalse())
			g.Expect(imp.ID).To(BeEmpty())
		}
	}
}

// TestReconcileCatalogExternal_UpgradeClearsPerRegionAmbiguity is the upgrade of a
// plane against a catalog that publishes the public identity endpoint in two
// regions. Before it, the region-less public import matched both rows, K-ORC
// failed it terminally, and CatalogReady held CatalogFailed. The pass re-imports
// that CR under the regioned filter, so the terminal error goes with it: the pass
// waits on the Region rather than repeating the old ambiguity, and the catalog is
// Ready once K-ORC runs the regioned filter.
func TestReconcileCatalogExternal_UpgradeClearsPerRegionAmbiguity(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	ambiguous := terminalImportConditions(korcImportMultipleMatchesMarker)
	public := preRegionEndpointImport(t, s, cp, c5c3v1alpha1.ExternalEndpointTypePublic, ambiguous, "")
	publicKey := client.ObjectKeyFromObject(public)

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, importedIdentityService(cp, availableImportConditions(), "svc-id"), public).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(keystoneRegionName(cp)))
	g.Expect(cond.Message).NotTo(ContainSubstring("remove the duplicate"),
		"the ambiguity belongs to the region-less filter K-ORC no longer runs")

	live := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, publicKey, live)).To(Succeed())
	g.Expect(live.UID).NotTo(Equal(public.UID), "a region-less import is re-imported even when it never resolved")
	g.Expect(live.Spec.Import.Filter.RegionRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))))

	// K-ORC resolves the Region and runs the regioned filter, which matches the
	// RegionOne row only.
	resolveRegionImport(ctx, g, c, cp)
	live.Status.Conditions = availableImportConditions()
	live.Status.Conditions[0].ObservedGeneration = live.Generation
	live.Status.ID = ptr.To("ep-public-regionone")
	g.Expect(c.Update(ctx, live)).To(Succeed())

	_, err = r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	cond = conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogImported))
}

// --- managed mode is untouched ---

// TestReconcileCatalog_ManagedModeProjectsNoImports is the golden-behavior guard:
// a Managed ControlPlane still registers exactly the two managed CRs it always did,
// creates none of the External-mode import CRs, and leaves status.catalog nil.
func TestReconcileCatalog_ManagedModeProjectsNoImports(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	cond, c := reconcileCatalogFor(t, cp,
		availableCatalogService(cp), availableCatalogEndpoint(cp), availableCatalogRegion(cp))

	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("CatalogRegistered"), "the managed reason must not change")
	g.Expect(cp.Status.Catalog).To(BeNil(), "status.catalog stays nil in Managed mode")

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, svc)).To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(svc.Spec.Import).To(BeNil())

	for _, iface := range externalCatalogInterfaces {
		err := c.Get(ctx, client.ObjectKey{Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp)},
			&orcv1alpha1.Endpoint{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Managed mode must create no %q endpoint import", iface)
	}
}
