// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the import-first External-mode catalog branch reconcileCatalogExternal.
package controller

import (
	"context"
	"testing"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
// Keystone takes on a managed catalog entry: a non-terminal Progressing=True with
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

// resolvedIdentityCatalog returns the four import CRs all reporting Available with
// a resolved id — the converged External-mode catalog.
func resolvedIdentityCatalog(cp *c5c3v1alpha1.ControlPlane) []client.Object {
	objs := []client.Object{importedIdentityService(cp, availableImportConditions(), "svc-id")}
	for _, iface := range externalCatalogInterfaces {
		objs = append(objs, importedIdentityEndpoint(cp, iface, availableImportConditions(), "ep-"+string(iface)))
	}
	return objs
}

// --- the default posture: import everything, create nothing ---

// TestReconcileCatalogExternal_ImportsServiceAndAllEndpointInterfaces is the
// headline acceptance criterion: pointed at a populated catalog, External mode
// creates ZERO catalog entries and instead imports the identity Service and all
// three endpoint interfaces as unmanaged, read-only K-ORC CRs.
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

	g.Expect(cp.Status.Catalog).NotTo(BeNil())
	g.Expect(cp.Status.Catalog.Imports).To(HaveLen(1 + len(externalCatalogInterfaces)))

	byName := map[string]c5c3v1alpha1.CatalogImportStatus{}
	for _, imp := range cp.Status.Catalog.Imports {
		byName[imp.Name] = imp
	}
	svc := byName[keystoneServiceName(cp)]
	g.Expect(svc.Kind).To(Equal("Service"))
	g.Expect(svc.Resolved).To(BeTrue())
	g.Expect(svc.ID).To(Equal("svc-id"))
	g.Expect(svc.Interface).To(BeEmpty(), "the Service import carries no interface")

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
	objs := []client.Object{importedIdentityService(cp, availableImportConditions(), "svc-id")}
	objs = append(
		objs,
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			pendingImportConditions(externalImportStallGrace+time.Minute), ""),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeInternal, availableImportConditions(), "ep-internal"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypeAdmin, availableImportConditions(), "ep-admin"),
	)
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonImportStalled))
	g.Expect(cond.Message).To(ContainSubstring(`no "public" endpoint`))
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

// TestReconcileCatalogExternal_AmbiguousEndpointNamesRegionLimitation proves a
// multi-match on an ENDPOINT import does not point at identityServiceName (which
// would not help): K-ORC's endpoint filter carries no region.
func TestReconcileCatalogExternal_AmbiguousEndpointNamesRegionLimitation(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane()
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
		importedIdentityEndpoint(cp, c5c3v1alpha1.ExternalEndpointTypePublic,
			terminalImportConditions(korcImportMultipleMatchesMarker), ""),
	}
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("one per region"))
	g.Expect(cond.Message).NotTo(ContainSubstring("identityServiceName"),
		"the endpoint filter is not spec-disambiguable, so do not point at a field that would not help")
}

// TestReconcileCatalogExternal_RewordedAmbiguityOnOptionalInterfaceDoesNotWedge is
// the guard on the marker coupling. korcImportMultipleMatchesMarker is K-ORC's
// literal wording, and a K-ORC bump may reword it at any time. If the tolerate branch
// keyed on that string, the reword would silently promote the per-region ambiguity
// below into a permanent CatalogReady=False — and therefore Ready=False — on a
// healthy multi-region control plane, over an interface nothing depends on and with
// no spec edit able to repair it. Every other test feeds the constant back to itself,
// so only this one would catch the regression. The tolerance is keyed on K-ORC's
// machine-readable InvalidConfiguration reason instead; the message selects the hint.
func TestReconcileCatalogExternal_RewordedAmbiguityOnOptionalInterfaceDoesNotWedge(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
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
// other half of the region limitation the test above names. A Keystone whose
// `public` endpoint resolves cleanly but which registers its `internal` endpoint
// once per region makes K-ORC's region-less EndpointFilter match several rows and
// go terminal — on an interface nothing in this control plane authenticates
// through, and which ambiguityHint itself says no spec edit can disambiguate.
// Gating CatalogReady on it would hold the aggregate Ready False forever with no
// remediation, so it is tolerated exactly like the unpublished interfaces above
// and surfaced through status.catalog.imports instead.
func TestReconcileCatalogExternal_AmbiguousOptionalInterfaceDoesNotBlockReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := externalCatalogControlPlane() // endpointType defaults to public
	objs := []client.Object{
		importedIdentityService(cp, availableImportConditions(), "svc-id"),
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

// --- managed mode is untouched ---

// TestReconcileCatalog_ManagedModeProjectsNoImports is the golden-behavior guard:
// a Managed ControlPlane still registers exactly the two managed CRs it always did,
// creates none of the External-mode import CRs, and leaves status.catalog nil.
func TestReconcileCatalog_ManagedModeProjectsNoImports(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := korcControlPlane()
	setAdminCredentialReady(cp)
	cond, c := reconcileCatalogFor(t, cp, availableCatalogService(cp), availableCatalogEndpoint(cp))

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
