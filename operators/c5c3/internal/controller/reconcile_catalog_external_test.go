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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
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

// --- the explicit opt-in ---

// optInControlPlane returns an External-mode ControlPlane declaring one managed
// catalog entry with a single public endpoint.
func optInControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := externalCatalogControlPlane()
	cp.Spec.Services.Keystone.External.Catalog = &c5c3v1alpha1.ExternalCatalogSpec{
		ManagedEntries: []c5c3v1alpha1.ExternalCatalogEntrySpec{{
			Type: "image",
			Name: "glance",
			Endpoints: []c5c3v1alpha1.ExternalCatalogEndpointSpec{
				{Interface: c5c3v1alpha1.ExternalEndpointTypePublic, URL: "https://glance.example.com"},
			},
		}},
	}
	return cp
}

// TestReconcileCatalogExternal_OptInCreatesExactlyDeclaredEntry proves the opt-in
// creates the declared entry and nothing else: the identity imports stay unmanaged
// and no undeclared interface is registered.
func TestReconcileCatalogExternal_OptInCreatesExactlyDeclaredEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := optInControlPlane()
	_, c := reconcileCatalogFor(t, cp)

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)}, svc)).
		To(Succeed())
	g.Expect(svc.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(svc.Spec.Import).To(BeNil())
	g.Expect(svc.Spec.Resource.Type).To(Equal("image"))
	g.Expect(svc.Spec.Resource.Name).To(HaveValue(Equal(orcv1alpha1.OpenStackName("glance"))))
	g.Expect(metav1.IsControlledBy(svc, cp)).To(BeTrue())

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name:      catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
		Namespace: childNamespace(cp),
	}, ep)).To(Succeed())
	g.Expect(ep.Spec.ManagementPolicy).To(Equal(orcv1alpha1.ManagementPolicyManaged))
	g.Expect(ep.Spec.Resource.URL).To(Equal("https://glance.example.com"))
	g.Expect(ep.Spec.Resource.ServiceRef).To(Equal(orcv1alpha1.KubernetesNameRef(catalogEntryServiceName(cp, "image"))))

	// Exactly one managed Service and one managed Endpoint; nothing else.
	var services orcv1alpha1.ServiceList
	g.Expect(c.List(ctx, &services, client.InNamespace(childNamespace(cp)))).To(Succeed())
	managed := 0
	for _, item := range services.Items {
		if item.Spec.ManagementPolicy == orcv1alpha1.ManagementPolicyManaged {
			managed++
		}
	}
	g.Expect(managed).To(Equal(1))

	var endpoints orcv1alpha1.EndpointList
	g.Expect(c.List(ctx, &endpoints, client.InNamespace(childNamespace(cp)))).To(Succeed())
	g.Expect(endpoints.Items).To(HaveLen(len(externalCatalogInterfaces) + 1))
	err := c.Get(ctx, client.ObjectKey{
		Name:      catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypeAdmin),
		Namespace: childNamespace(cp),
	}, &orcv1alpha1.Endpoint{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an undeclared interface must not be registered")
}

// TestReconcileCatalogExternal_OptInEntriesAuthenticateWithThePasswordCloud pins
// the credential split the teardown depends on. The entry CRs are Managed, so
// K-ORC must reach the external Keystone to DELETE them — while the teardown sweep
// concurrently revokes the ApplicationCredential that the spec's clouds.yaml
// (k-orc-clouds-yaml) carries. Authenticating the entries through the operator-owned
// password cloud instead removes the ordering the unsequenced sweep cannot provide.
// The read-only imports keep the spec's clouds.yaml: their CR delete never calls
// OpenStack, so a revoked credential cannot strand them.
func TestReconcileCatalogExternal_OptInEntriesAuthenticateWithThePasswordCloud(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := optInControlPlane()
	_, c := reconcileCatalogFor(t, cp)

	specSecret := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName

	svc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)}, svc)).
		To(Succeed())
	g.Expect(svc.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)),
		"a managed entry must not authenticate with the application credential the teardown sweep revokes")
	g.Expect(svc.Spec.CloudCredentialsRef.CloudName).To(Equal(cloudName))

	ep := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name:      catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
		Namespace: childNamespace(cp),
	}, ep)).To(Succeed())
	g.Expect(ep.Spec.CloudCredentialsRef.SecretName).To(Equal(adminPasswordCloudSecretName(cp)))
	g.Expect(ep.Spec.CloudCredentialsRef.CloudName).To(Equal(cloudName))

	// The unmanaged identity imports stay on the spec's clouds.yaml.
	imported := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)}, imported)).
		To(Succeed())
	g.Expect(imported.Spec.CloudCredentialsRef.SecretName).To(Equal(specSecret))
	g.Expect(specSecret).NotTo(Equal(adminPasswordCloudSecretName(cp)),
		"the fixture must keep the two credentials distinct for this test to mean anything")
}

// TestReconcileCatalogExternal_OptInGatesCatalogReady proves readiness waits for
// the declared entry too: a registered-but-not-yet-Available entry is not Ready.
func TestReconcileCatalogExternal_OptInGatesCatalogReady(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := optInControlPlane()
	cond, _ := reconcileCatalogFor(t, cp, resolvedIdentityCatalog(cp)...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring(catalogEntryServiceName(cp, "image")))
}

// TestReconcileCatalogExternal_OptInTerminalErrorFailsLoud covers the error path of
// the opt-in: K-ORC could not create the declared entry.
func TestReconcileCatalogExternal_OptInTerminalErrorFailsLoud(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := optInControlPlane()
	broken := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)},
		Status:     orcv1alpha1.ServiceStatus{Conditions: terminalImportConditions("quota exceeded")},
	}
	objs := append(resolvedIdentityCatalog(cp), broken)
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("quota exceeded"))
	g.Expect(cond.Message).To(ContainSubstring(catalogEntryServiceName(cp, "image")))
}

// TestReconcileCatalogExternal_OptInEntryFailureNamesTheCause is the write-path half
// of the silent-empty contract. K-ORC collapses every hard failure against the
// OpenStack API into the same non-terminal TransientError, so nothing an entry can
// realistically fail with is terminal: a hardened brownfield cloud whose adopted
// account is a domain-admin returns HTTP 403 on POST /v3/services, and the entry then
// only ever reaches the bounded wait. The wait must relay K-ORC's message — it is the
// one place the 403 exists — rather than reporting "not yet Available" forever.
func TestReconcileCatalogExternal_OptInEntryFailureNamesTheCause(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := optInControlPlane()
	denied := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)},
		Status: orcv1alpha1.ServiceStatus{Conditions: transientEntryConditions(
			"You are not authorized to perform the requested action: identity:create_service. (HTTP 403)",
		)},
	}
	objs := append(resolvedIdentityCatalog(cp), denied)
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("identity:create_service"),
		"the wait must relay the only description of what actually failed")
	g.Expect(cond.Message).To(ContainSubstring("HTTP 403"))
}

// TestReconcileCatalogExternal_OptInEntryClassifiableFailureSurfaced proves the write
// path is classified exactly like the imports: a message that identifies a failure
// CLASS is reported with that class, not collapsed into a generic wait.
func TestReconcileCatalogExternal_OptInEntryClassifiableFailureSurfaced(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := optInControlPlane()
	unauthorized := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)},
		Status:     orcv1alpha1.ServiceStatus{Conditions: transientEntryConditions("401 Unauthorized")},
	}
	objs := append(resolvedIdentityCatalog(cp), unauthorized)
	cond, _ := reconcileCatalogFor(t, cp, objs...)

	g.Expect(cond.Reason).To(Equal(conditionReasonAuthenticationFailed))
	g.Expect(cond.Message).To(ContainSubstring("401 Unauthorized"))
}

// terminatingEntryCRs returns the CRs of the declared `image` entry exactly as
// ensureManagedCatalogEntries projects them — byte-identical specs, so
// controllerutil.CreateOrUpdate finds them and updates NOTHING — but Terminating
// behind K-ORC's finalizers and still carrying the Available=True K-ORC left on them.
func terminatingEntryCRs(
	t *testing.T, s *runtime.Scheme, cp *c5c3v1alpha1.ControlPlane,
) (*orcv1alpha1.Service, *orcv1alpha1.Endpoint) {
	t.Helper()
	g := NewGomegaWithT(t)

	credRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: adminPasswordCloudSecretName(cp),
		CloudName:  cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName,
	}
	deletion := metav1.Now()
	svc := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              catalogEntryServiceName(cp, "image"),
			Namespace:         childNamespace(cp),
			Finalizers:        []string{"openstack.k-orc.cloud/service"},
			DeletionTimestamp: &deletion,
		},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.ServiceResourceSpec{
				Type:    "image",
				Name:    ptr.To(orcv1alpha1.OpenStackName("glance")),
				Enabled: ptr.To(true),
			},
		},
		Status: orcv1alpha1.ServiceStatus{Conditions: availableImportConditions()},
	}
	ep := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace:         childNamespace(cp),
			Finalizers:        []string{"openstack.k-orc.cloud/endpoint"},
			DeletionTimestamp: &deletion,
		},
		Spec: orcv1alpha1.EndpointSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.EndpointResourceSpec{
				Interface:  string(c5c3v1alpha1.ExternalEndpointTypePublic),
				URL:        "https://glance.example.com",
				ServiceRef: orcv1alpha1.KubernetesNameRef(catalogEntryServiceName(cp, "image")),
				Enabled:    ptr.To(true),
			},
		},
		Status: orcv1alpha1.EndpointStatus{Conditions: availableImportConditions()},
	}
	g.Expect(controllerutil.SetControllerReference(cp, svc, s)).To(Succeed())
	g.Expect(controllerutil.SetControllerReference(cp, ep, s)).To(Succeed())
	return svc, ep
}

// TestReconcileCatalogExternal_ReAddedEntryIsNotReadyWhileTerminating covers the
// remove-then-re-add race. Inside the window where K-ORC is removing the rows,
// controllerutil.CreateOrUpdate finds the still-Terminating CRs, projects a
// byte-identical spec and issues no Update — so no generation bump invalidates the
// retained Available=True, and korcAvailableUpToDate (generation-aware, not
// deletion-aware) reports both entries live. CatalogReady must not be stamped True
// over rows K-ORC is deleting: once the finalizers clear the CRs are recreated with
// fresh OpenStack ids, and a client that cached the endpoint id sees it vanish and
// reappear under a new one.
func TestReconcileCatalogExternal_ReAddedEntryIsNotReadyWhileTerminating(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := optInControlPlane() // the `image` entry is declared again
	s := korcTestScheme(t)
	svc, ep := terminatingEntryCRs(t, s, cp)

	objs := append(resolvedIdentityCatalog(cp), svc, ep)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, objs...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// Both fixtures must reach the reconciler untouched, otherwise the guard under
	// test is masked by a generation bump rather than exercised.
	gotSvc := &orcv1alpha1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc)).To(Succeed())
	g.Expect(gotSvc.DeletionTimestamp).NotTo(BeNil())
	g.Expect(korcAvailableUpToDate(gotSvc)).To(BeTrue(),
		"the retained Available condition is what would flip CatalogReady True without the deletion guard")
	gotEp := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ep), gotEp)).To(Succeed())
	g.Expect(korcAvailableUpToDate(gotEp)).To(BeTrue())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("still being removed"))
	g.Expect(cond.Message).To(ContainSubstring(catalogEntryServiceName(cp, "image")))
}

// stuckPrunedEntry returns the entry CRs of an `image` entry the spec no longer
// declares, owned by cp and held Terminating by a K-ORC finalizer once deleted — the
// removal K-ORC cannot complete (a 403 on DELETE /v3/endpoints, an endpoint deleted
// by hand, a flapping WAN link). conds is stamped on the Endpoint.
func stuckPrunedEntry(
	t *testing.T, s *runtime.Scheme, cp *c5c3v1alpha1.ControlPlane, conds []metav1.Condition,
) (*orcv1alpha1.Service, *orcv1alpha1.Endpoint) {
	t.Helper()
	g := NewGomegaWithT(t)

	svc := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       catalogEntryServiceName(cp, "image"),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/service"},
		},
		Spec: orcv1alpha1.ServiceSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
	}
	ep := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:       catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace:  childNamespace(cp),
			Finalizers: []string{"openstack.k-orc.cloud/endpoint"},
		},
		Spec:   orcv1alpha1.EndpointSpec{ManagementPolicy: orcv1alpha1.ManagementPolicyManaged},
		Status: orcv1alpha1.EndpointStatus{Conditions: conds},
	}
	g.Expect(controllerutil.SetControllerReference(cp, svc, s)).To(Succeed())
	g.Expect(controllerutil.SetControllerReference(cp, ep, s)).To(Succeed())
	return svc, ep
}

// TestReconcileCatalogExternal_IncompleteRemovalGatesCatalogReady closes the
// asymmetry the opt-in contract had: registration gated readiness on
// korcAvailableUpToDate, removal gated on nothing. The prune issued its Delete and
// never looked at the outcome, so a row K-ORC could not remove stayed live in the
// customer's catalog behind a Terminating CR while CatalogReady reported True — and,
// once the ControlPlane was deleted, the teardown stall escape stripped the finalizer
// and orphaned the row for good.
func TestReconcileCatalogExternal_IncompleteRemovalGatesCatalogReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := externalCatalogControlPlane() // no managedEntries: the entry was removed
	s := korcTestScheme(t)
	staleSvc, staleEp := stuckPrunedEntry(t, s, cp, transientEntryConditions("cannot reach the external Keystone"))

	objs := append(resolvedIdentityCatalog(cp), staleSvc, staleEp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, objs...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))

	// Both CRs were deleted, and both are held Terminating by K-ORC's finalizer.
	got := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(staleEp), got)).To(Succeed())
	g.Expect(got.DeletionTimestamp).NotTo(BeNil())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"a removal K-ORC has not completed must never report CatalogReady True")
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCatalog))
	g.Expect(cond.Message).To(ContainSubstring("still being removed"))
	g.Expect(cond.Message).To(ContainSubstring(staleEp.Name))
	g.Expect(cond.Message).To(ContainSubstring("cannot reach the external Keystone"))
}

// TestReconcileCatalogExternal_TerminalRemovalErrorFailsLoud is the terminal half:
// K-ORC gave up on the DELETE, so the row is live in a catalog this ControlPlane does
// not own and no retry will take it out.
func TestReconcileCatalogExternal_TerminalRemovalErrorFailsLoud(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := externalCatalogControlPlane()
	s := korcTestScheme(t)
	staleSvc, staleEp := stuckPrunedEntry(t, s, cp, terminalImportConditions("endpoint is referenced elsewhere"))

	objs := append(resolvedIdentityCatalog(cp), staleSvc, staleEp)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, objs...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeCatalogReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonCatalogFailed))
	g.Expect(cond.Message).To(ContainSubstring("removing the Endpoint"))
	g.Expect(cond.Message).To(ContainSubstring("endpoint is referenced elsewhere"))
}

// TestReconcileCatalogExternal_RemovedOptInEntryDeleted is the other half of the
// opt-in contract: removing a declared entry deletes exactly its CRs. The identity
// imports and a foreign CR sharing the namespace must survive, so the sweep is
// proven to be scoped by BOTH the controller reference and the name prefix.
func TestReconcileCatalogExternal_RemovedOptInEntryDeleted(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cp := externalCatalogControlPlane() // no managedEntries: the entry was removed
	staleSvc := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "image"), Namespace: childNamespace(cp)},
	}
	staleEp := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      catalogEntryEndpointName(cp, "image", c5c3v1alpha1.ExternalEndpointTypePublic),
			Namespace: childNamespace(cp),
		},
	}
	// A CR carrying the entry prefix but owned by nobody: prefix alone must not sweep it.
	foreign := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, "compute"), Namespace: childNamespace(cp)},
	}
	s := korcTestScheme(t)
	for _, obj := range []client.Object{staleSvc, staleEp} {
		g.Expect(controllerutil.SetControllerReference(cp, obj, s)).To(Succeed())
	}

	objs := append(resolvedIdentityCatalog(cp), staleSvc, staleEp, foreign)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, objs...)...).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.reconcileCatalog(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(staleSvc), &orcv1alpha1.Service{}))).
		To(BeTrue(), "the undeclared entry Service must be deleted")
	g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(staleEp), &orcv1alpha1.Endpoint{}))).
		To(BeTrue(), "the undeclared entry Endpoint must be deleted")

	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(foreign), &orcv1alpha1.Service{})).
		To(Succeed(), "a CR this ControlPlane does not own must survive the sweep")
	g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneServiceName(cp), Namespace: childNamespace(cp)},
		&orcv1alpha1.Service{})).To(Succeed(), "the unmanaged identity import must survive the sweep")
	for _, iface := range externalCatalogInterfaces {
		g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneEndpointImportName(cp, iface), Namespace: childNamespace(cp)},
			&orcv1alpha1.Endpoint{})).To(Succeed(), "the unmanaged %q endpoint import must survive the sweep", iface)
	}
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
