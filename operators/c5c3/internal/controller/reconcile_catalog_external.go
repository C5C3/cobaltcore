// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// CatalogReady reasons only the External branch produces.
const (
	// conditionReasonCatalogImported is the External-mode success reason: the
	// external identity catalog is visible as resolved imports. It is deliberately
	// NOT CatalogRegistered — nothing was registered, and conflating the two would
	// make "did this ControlPlane write to my catalog?" unanswerable from status.
	conditionReasonCatalogImported = "CatalogImported"

	// conditionReasonImportError reports a Kubernetes-level failure reconciling one
	// of the unmanaged import CRs (not a K-ORC/OpenStack failure).
	conditionReasonImportError = "ImportError"

	// conditionReasonCatalogEntryError reports a Kubernetes-level failure
	// reconciling (or garbage-collecting) an opt-in managed catalog entry.
	conditionReasonCatalogEntryError = "CatalogEntryError"
)

// externalCatalogInterfaces are the catalog interfaces imported in External mode.
//
// ALL THREE are imported, not just the one the control plane authenticates
// against: catalog rows are listable through the identity API regardless of
// whether the endpoint they advertise is reachable from this cluster, so full
// visibility costs nothing and is the foundation a later declarative endpoint
// cutover builds on.
//
// Only ONE of them is REQUIRED to resolve, though — see catalogImport.required.
// A brownfield Keystone is free not to publish an interface at all (kolla-ansible
// stopped registering the identity `admin` endpoint after Zed; a devstack whose
// bootstrap set only a public URL publishes nothing else), and adopting exactly
// those installations is what External mode exists for. Gating readiness on an
// interface the installation never published would hold Ready False forever, so
// the other two are imported for visibility and reported through
// status.catalog.imports — informational, as the entries for interfaces this
// cluster cannot dial always were.
var externalCatalogInterfaces = []c5c3v1alpha1.ExternalEndpointType{
	c5c3v1alpha1.ExternalEndpointTypePublic,
	c5c3v1alpha1.ExternalEndpointTypeInternal,
	c5c3v1alpha1.ExternalEndpointTypeAdmin,
}

// externalCatalogSpec returns the External-mode catalog block, or nil when the
// ControlPlane is managed, has no external block, or left the block at its
// conservative default. Nil-safe on every level so callers branch on the result.
func externalCatalogSpec(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.ExternalCatalogSpec {
	ks := cp.Spec.Services.Keystone
	if ks == nil || ks.External == nil {
		return nil
	}
	return ks.External.Catalog
}

// externalIdentityServiceName returns the configured identity-service
// disambiguation filter, or "" when the import filters on type alone.
func externalIdentityServiceName(cp *c5c3v1alpha1.ControlPlane) string {
	if catalog := externalCatalogSpec(cp); catalog != nil {
		return catalog.IdentityServiceName
	}
	return ""
}

// externalManagedCatalogEntries returns the explicitly opted-in catalog entries,
// or nil — the default — in which case External mode creates nothing.
func externalManagedCatalogEntries(cp *c5c3v1alpha1.ControlPlane) []c5c3v1alpha1.ExternalCatalogEntrySpec {
	if catalog := externalCatalogSpec(cp); catalog != nil {
		return catalog.ManagedEntries
	}
	return nil
}

// keystoneEndpointImportName is the deterministic name of the unmanaged Endpoint
// import CR for one catalog interface. It extends the managed-mode Endpoint name
// with the interface, so the two never collide (modes cannot transition, but the
// deletion sweep enumerates both).
func keystoneEndpointImportName(cp *c5c3v1alpha1.ControlPlane, iface c5c3v1alpha1.ExternalEndpointType) string {
	return keystoneEndpointName(cp) + "-" + string(iface)
}

// catalogEntryNamePrefix is the name prefix of every CR belonging to an opt-in
// managed catalog entry. Together with the controller reference it scopes the
// removal sweep: only CRs this ControlPlane created for a declared entry are
// candidates for deletion. It cannot collide with the "-identity-" import names.
func catalogEntryNamePrefix(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-catalog-"
}

func catalogEntryServiceName(cp *c5c3v1alpha1.ControlPlane, entryType string) string {
	return catalogEntryNamePrefix(cp) + entryType
}

func catalogEntryEndpointName(cp *c5c3v1alpha1.ControlPlane, entryType string, iface c5c3v1alpha1.ExternalEndpointType) string {
	return catalogEntryServiceName(cp, entryType) + "-" + string(iface)
}

// catalogImport is one unmanaged import CR carrying its live status, plus the
// metadata the status projection and the failure messages need.
type catalogImport struct {
	kind  string
	name  string
	iface c5c3v1alpha1.ExternalEndpointType // empty for the Service import
	id    string                            // the resolved OpenStack id, "" while unresolved
	obj   orcv1alpha1.ObjectWithConditions

	// required marks the imports CatalogReady is gated on: the identity Service,
	// and the Endpoint of the interface spec.services.keystone.external.
	// endpointType selects. Those two must resolve — the control plane already
	// authenticates through that interface, so a catalog that does not publish it
	// is not the catalog K-ORC was pointed at. Every other interface is
	// best-effort: see externalCatalogInterfaces.
	required bool
}

func (i catalogImport) resolved() bool { return korcAvailableUpToDate(i.obj) }

func (i catalogImport) describe() string { return fmt.Sprintf("%s %q", i.kind, i.name) }

// korcTerminalReason returns the reason of obj's TERMINAL Progressing condition, or
// "" when K-ORC has not terminally failed on it. It is the machine-readable half of
// orcv1alpha1.GetTerminalError, which surfaces only the free-text message — and the
// reason is the part that survives a K-ORC rewording.
func korcTerminalReason(obj orcv1alpha1.ObjectWithConditions) string {
	cond := apimeta.FindStatusCondition(obj.GetConditions(), orcv1alpha1.ConditionProgressing)
	if cond == nil || cond.ObservedGeneration != obj.GetGeneration() ||
		!orcv1alpha1.IsConditionReasonTerminal(cond.Reason) {
		return ""
	}
	return cond.Reason
}

// korcStatusMessage returns the message K-ORC last stamped on obj — the Progressing
// condition's, else the Available condition's — or "" when neither carries one.
//
// It is the only place a hard failure classifyKORCMessage has no arm for survives. A
// domain-admin account adopting a brownfield cloud gets HTTP 403 on POST /v3/services;
// K-ORC reports that as a non-terminal TransientError, so a bounded wait that dropped
// the message would say "registered but not yet Available" forever with nothing in the
// condition naming the 403.
func korcStatusMessage(obj orcv1alpha1.ObjectWithConditions) string {
	for _, condType := range []string{orcv1alpha1.ConditionProgressing, orcv1alpha1.ConditionAvailable} {
		if cond := apimeta.FindStatusCondition(obj.GetConditions(), condType); cond != nil && cond.Message != "" {
			return cond.Message
		}
	}
	return ""
}

// reconcileCatalogExternal is the import-first catalog branch. It NEVER creates a
// catalog entry unless the spec explicitly declares one.
//
// Pointed at a populated catalog, a managed registration would duplicate rows —
// Keystone enforces no uniqueness on service names — so the default posture is to
// make the existing identity service and its endpoint interfaces VISIBLE as
// unmanaged K-ORC imports and to create nothing at all.
//
// That inverts the failure modes, and the detection story is the point of this
// branch. A K-ORC import that matches nothing does not error: it waits forever on
// "Waiting for OpenStack resource to be created externally", by conditions
// indistinguishable from a resource that is about to appear. For a REQUIRED
// import (see catalogImport.required) the target pre-exists BY DEFINITION, so
// past a grace window that wait is a misconfiguration signal, surfaced as
// ImportStalled naming endpointType and region — never quiet success. An import
// that matches SEVERAL entries is terminal in K-ORC itself, and is relayed with a
// hint at the disambiguation filter.
//
// Precedence, most specific cause first:
//
//  1. a classifiable K-ORC message on an unresolved import, entry or removal (auth,
//     TLS, reachability, catalog mismatch) — relayed verbatim, the failure class is
//     only recoverable from the message text
//  2. a terminal K-ORC error on an import, Service before Endpoints so the ROOT is
//     reported. Unlike the waits below this covers every import, required or not:
//     K-ORC has given up on it, which is loud, actionable and never a property of
//     the external catalog merely omitting an interface. The lone exception is an
//     InvalidConfiguration on a non-required import, which no spec edit can repair —
//     see the rationale at the loop
//  3. a REQUIRED import stalled past externalImportStallGrace — the silent-empty
//     hazard
//  4. a terminal K-ORC error on an opt-in managed entry, or on one being removed
//  5. a REQUIRED import, any declared entry, or any removal not yet complete — a
//     legitimate, bounded wait
func (r *ControlPlaneReconciler) reconcileCatalogExternal(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeCatalogReady)

	imports, err := r.ensureExternalCatalogImports(ctx, cp, credRef)
	if err != nil {
		fail(conditionReasonImportError, fmt.Sprintf("reconciling the external catalog imports: %v", err))
		return ctrl.Result{}, err
	}
	// Project the observed imports before any early return, so an operator can see
	// which rows resolved even while the condition reports a failure.
	cp.Status.Catalog = &c5c3v1alpha1.CatalogStatus{Imports: catalogImportStatus(imports)}

	entries, pruning, err := r.ensureManagedCatalogEntries(ctx, cp, entryCredentialsRef(cp, credRef))
	if err != nil {
		fail(conditionReasonCatalogEntryError, fmt.Sprintf("reconciling the managed catalog entries: %v", err))
		return ctrl.Result{}, err
	}

	// 1. A classifiable message on an UNRESOLVED import. A resolved import is never
	// re-classified: K-ORC leaves the last transient attempt's message on the
	// Progressing condition, and classifying that would flip a converged catalog to
	// a failure it has already recovered from (mirrors classifyExternalKORCState).
	var pending []orcv1alpha1.ObjectWithConditions
	for _, imp := range imports {
		if !imp.resolved() {
			pending = append(pending, imp.obj)
		}
	}
	// The WRITE path is classified alongside the imports. Nothing K-ORC reports on a
	// managed entry is terminal (classifyKORCMessage documents why: a 401, a dial
	// error, a TLS failure and a catalog mismatch all collapse into the same
	// non-terminal TransientError), so without this every realistic entry failure
	// would fall through to the unbounded step-5 wait and read as "not yet Available"
	// — the silent-empty hazard the import branch exists to eliminate, reintroduced.
	for _, entry := range entries {
		if !korcAvailableUpToDate(entry.obj) {
			pending = append(pending, entry.obj)
		}
	}
	// A removal is in flight by construction, so its message always describes the
	// DELETE K-ORC is retrying against the external catalog.
	for _, entry := range pruning {
		pending = append(pending, entry.obj)
	}
	if reason, rawMessage := classifyExternalKORCFailure(pending...); reason != "" {
		message := fmt.Sprintf("external Keystone at %s: %s", externalKeystoneAuthURL(cp), rawMessage)
		if reason == conditionReasonCatalogEndpointMismatch {
			message += "; " + catalogEndpointMismatchHint(cp)
		}
		fail(reason, message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// 2. Terminal K-ORC errors, in dependency order. The >1-match half of the
	// ambiguity contract lands here: K-ORC refuses to guess and stops retrying.
	//
	// A terminal error gates CatalogReady for every import, required or not — with
	// ONE exception: an InvalidConfiguration on an import that is not required. That
	// reason is K-ORC's machine-readable "the user must fix the configuration", and a
	// non-required import has no configuration a user CAN fix: its filter is entirely
	// operator-derived (an interface plus the identity Service reference), and K-ORC's
	// EndpointFilter carries no region (see ambiguityHint), so a catalog publishing an
	// interface once per region is not spec-disambiguable at all. Gating on it would
	// hold CatalogReady False forever, with no remediation, over an interface nothing
	// in this control plane depends on. That is the same brownfield asymmetry step 3
	// already tolerates for the 0-match case, so tolerate it here too and surface it
	// through status.catalog.imports as unresolved. The same import required — the
	// interface the control plane authenticates through — still fails loud, and so
	// does an UnrecoverableError on any import.
	//
	// The exception is keyed on the REASON, never on korcImportMultipleMatchesMarker:
	// that marker is coupled to K-ORC's literal wording, so keying the tolerate branch
	// on it would turn a K-ORC rewording into a permanent CatalogReady=False on a
	// healthy multi-region control plane. The marker selects the HINT only, where a
	// rewording degrades to "the terminal error is surfaced without the hint".
	for _, imp := range imports {
		termErr := orcv1alpha1.GetTerminalError(imp.obj)
		if termErr == nil {
			continue
		}
		if !imp.required && korcTerminalReason(imp.obj) == orcv1alpha1.ConditionReasonInvalidConfiguration {
			logger.Info("unrepairable terminal error on an optional catalog import; not gating CatalogReady",
				"import", imp.name, "interface", imp.iface, "error", termErr)
			continue
		}
		message := fmt.Sprintf("K-ORC reported a terminal error importing the identity %s: %v", imp.describe(), termErr)
		if strings.Contains(termErr.Error(), korcImportMultipleMatchesMarker) {
			message += "; " + imp.ambiguityHint(cp)
		}
		fail(conditionReasonCatalogFailed, message)
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// 3. The silent-empty hazard: a REQUIRED import that matched nothing. An
	// optional interface the external catalog simply does not publish stalls on the
	// same marker forever and is not a failure — it is the brownfield posture.
	for _, imp := range imports {
		if imp.required && korcImportStalled(imp.obj, externalImportStallGrace) {
			logger.Info("external catalog import stalled", "import", imp.name, "kind", imp.kind)
			fail(conditionReasonImportStalled, imp.stallMessage(cp))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}

	// 4. Terminal K-ORC errors on the entries this ControlPlane itself created — and
	// on the ones it is removing. A removal K-ORC gave up on leaves the row live in a
	// catalog this ControlPlane does not own, so it can never be fire-and-forget.
	for _, entry := range entries {
		if termErr := orcv1alpha1.GetTerminalError(entry.obj); termErr != nil {
			return r.catalogTerminalError(cp, entry.kind, entry.name, termErr), nil
		}
	}
	for _, entry := range pruning {
		if termErr := orcv1alpha1.GetTerminalError(entry.obj); termErr != nil {
			fail(conditionReasonCatalogFailed, fmt.Sprintf(
				"K-ORC reported a terminal error removing the %s %q from the external catalog: %v",
				entry.kind, entry.name, termErr,
			))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}

	// 5. Bounded waits.
	for _, imp := range imports {
		if imp.required && !imp.resolved() {
			logger.Info("external catalog import not yet resolved, requeuing", "import", imp.name)
			fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
				"the identity %s is imported but K-ORC has not resolved it against the external catalog yet",
				imp.describe(),
			))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	for _, entry := range entries {
		// An entry re-declared while its earlier removal is still in flight is NOT
		// Available, whatever its conditions say. controllerutil.CreateOrUpdate finds
		// the Terminating CR, projects a byte-identical spec and updates nothing, so
		// no generation bump invalidates the Available=True K-ORC left on it — while
		// K-ORC is deleting that exact row. korcAvailableUpToDate is generation-aware,
		// not deletion-aware, so the guard belongs here.
		if entry.obj.GetDeletionTimestamp() != nil {
			fail(conditionReasonWaitingForCatalog, fmt.Sprintf(
				"the declared catalog entry %s %q is still being removed from the external catalog by an "+
					"earlier deletion; it is re-registered once K-ORC releases it", entry.kind, entry.name,
			))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		if !korcAvailableUpToDate(entry.obj) {
			fail(conditionReasonWaitingForCatalog, entryWaitMessage(entry, fmt.Sprintf(
				"the declared catalog entry %s %q is registered but not yet Available", entry.kind, entry.name,
			)))
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
	}
	// Removal gates readiness exactly as registration does. Until K-ORC's finalizer
	// has taken the row out of the external catalog the ControlPlane still owns it,
	// and reporting CatalogReady=True meanwhile would make a removal that never
	// completes (a 403 on DELETE /v3/endpoints, a flapping WAN link) invisible — and
	// its CR a candidate for the teardown stall escape, which orphans the row.
	for _, entry := range pruning {
		fail(conditionReasonWaitingForCatalog, entryWaitMessage(entry, fmt.Sprintf(
			"the undeclared catalog entry %s %q is still being removed from the external catalog",
			entry.kind, entry.name,
		)))
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// The count is of RESOLVED endpoint interfaces, not of imported CRs: an
	// external catalog that publishes fewer than all three is Ready, and a message
	// claiming otherwise would hide the very asymmetry status.catalog.imports
	// exists to expose.
	resolvedInterfaces := 0
	for _, imp := range imports {
		if imp.iface != "" && imp.resolved() {
			resolvedInterfaces++
		}
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             conditionReasonCatalogImported,
		Message: fmt.Sprintf(
			"imported the external identity Service and %d of %d Endpoint interface(s) as unmanaged K-ORC CRs "+
				"(see status.catalog.imports); %d declared catalog entry/entries registered",
			resolvedInterfaces, len(externalCatalogInterfaces), len(externalManagedCatalogEntries(cp)),
		),
	})
	return ctrl.Result{}, nil
}

// ambiguityHint renders the remediation for a >1-match import failure.
//
// The identity Service import filters on type (and optionally name), both of
// which the spec controls, so the hint names the disambiguation field. An
// Endpoint import filters on interface and its owning Service — K-ORC's
// EndpointFilter carries no region — so a catalog publishing the same interface
// per region cannot be disambiguated from the spec at all; say so rather than
// pointing at a field that would not help.
func (i catalogImport) ambiguityHint(cp *c5c3v1alpha1.ControlPlane) string {
	if i.iface != "" {
		return fmt.Sprintf(
			"the external catalog publishes more than one %q endpoint for the identity service "+
				"(commonly one per region); K-ORC's endpoint import filter cannot select among them",
			i.iface,
		)
	}
	filter := "type=" + c5c3v1alpha1.IdentityCatalogServiceType
	if name := externalIdentityServiceName(cp); name != "" {
		filter += fmt.Sprintf(", name=%q", name)
	}
	return fmt.Sprintf(
		"the identity Service import filter (%s) matched more than one catalog entry; "+
			"set spec.services.keystone.external.catalog.identityServiceName to disambiguate",
		filter,
	)
}

// stallMessage renders the ImportStalled message: what is stuck, where it was
// looked for, and the two spec fields that decide where K-ORC looks. For an
// Endpoint import it also names the third possibility — the external catalog
// simply does not publish that interface, which no spec edit can fix.
func (i catalogImport) stallMessage(cp *c5c3v1alpha1.ControlPlane) string {
	message := fmt.Sprintf(
		"catalog import %s has been waiting to be created externally in %s for longer than %s; "+
			"in External mode the import target already exists, so this is a misconfiguration — "+
			"check spec.services.keystone.external.endpointType and spec.region",
		i.describe(), externalKeystoneAuthURL(cp), externalImportStallGrace,
	)
	if i.iface != "" {
		message += fmt.Sprintf(", or the external catalog publishes no %q endpoint for the identity service", i.iface)
	} else if name := externalIdentityServiceName(cp); name != "" {
		message += fmt.Sprintf(", or the external catalog holds no identity service named %q "+
			"(spec.services.keystone.external.catalog.identityServiceName)", name)
	}
	return message
}

// catalogImportStatus projects the observed imports onto status.catalog.imports.
func catalogImportStatus(imports []catalogImport) []c5c3v1alpha1.CatalogImportStatus {
	out := make([]c5c3v1alpha1.CatalogImportStatus, 0, len(imports))
	for _, imp := range imports {
		out = append(out, c5c3v1alpha1.CatalogImportStatus{
			Name:      imp.name,
			Kind:      imp.kind,
			Interface: imp.iface,
			Resolved:  imp.resolved(),
			ID:        imp.id,
		})
	}
	return out
}

// ensureExternalCatalogImports create-or-updates the UNMANAGED K-ORC CRs that
// import the external identity service and each of its endpoint interfaces, and
// returns them carrying live status in dependency order (Service first, so a
// classifier reports the root stuck dependency rather than an Endpoint merely
// blocked on it), each flagged with whether CatalogReady is gated on it.
//
// ManagementPolicyUnmanaged with Spec.Import and no Spec.Resource is what makes
// these read-only: K-ORC resolves them against the existing catalog, writes
// nothing, and on CR deletion removes only the Kubernetes object.
func (r *ControlPlaneReconciler) ensureExternalCatalogImports(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
) ([]catalogImport, error) {
	ns := childNamespace(cp)

	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: ns},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
		service.Spec.CloudCredentialsRef = credRef
		service.Spec.Resource = nil
		filter := &orcv1alpha1.ServiceFilter{Type: ptr.To(c5c3v1alpha1.IdentityCatalogServiceType)}
		if name := externalIdentityServiceName(cp); name != "" {
			filter.Name = ptr.To(orcv1alpha1.OpenStackName(name))
		}
		service.Spec.Import = &orcv1alpha1.ServiceImport{Filter: filter}
		return controllerutil.SetControllerReference(cp, service, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("identity Service import %q: %w", service.Name, err)
	}

	imports := []catalogImport{{
		kind:     "Service",
		name:     service.Name,
		id:       ptr.Deref(service.Status.ID, ""),
		obj:      service,
		required: true,
	}}

	authInterface := korcEndpointType(cp)
	for _, iface := range externalCatalogInterfaces {
		endpoint := &orcv1alpha1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneEndpointImportName(cp, iface), Namespace: ns},
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
			endpoint.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
			endpoint.Spec.CloudCredentialsRef = credRef
			endpoint.Spec.Resource = nil
			endpoint.Spec.Import = &orcv1alpha1.EndpointImport{
				Filter: &orcv1alpha1.EndpointFilter{
					Interface:  string(iface),
					ServiceRef: ptr.To(orcv1alpha1.KubernetesNameRef(service.Name)),
				},
			}
			return controllerutil.SetControllerReference(cp, endpoint, r.Scheme)
		}); err != nil {
			return nil, fmt.Errorf("identity Endpoint import %q: %w", endpoint.Name, err)
		}
		imports = append(imports, catalogImport{
			kind:     "Endpoint",
			name:     endpoint.Name,
			iface:    iface,
			id:       ptr.Deref(endpoint.Status.ID, ""),
			obj:      endpoint,
			required: string(iface) == authInterface,
		})
	}

	return imports, nil
}

// entryCredentialsRef returns the credentials the opt-in catalog entries
// authenticate with: the operator-owned password cloud, NOT the spec's
// clouds.yaml (which carries the minted application credential).
//
// The entries are ManagementPolicyManaged, so K-ORC must reach the external
// Keystone to DELETE them — and the teardown sweep (deleteORCResources) issues
// every Delete in one unsequenced pass. The ApplicationCredential's K-ORC
// finalizer REVOKES the credential at the Keystone level, so an entry still
// authenticating through it would get a 404 and stay Terminating until the stall
// escape strips its finalizer, orphaning the row in a third-party catalog. The
// admin password outlives the revocation, so pointing the entries at the same
// document the ApplicationCredential itself mints with removes the dependency the
// sweep cannot order. The read-only identity imports keep credRef: deleting an
// unmanaged CR never calls OpenStack.
func entryCredentialsRef(
	cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
) orcv1alpha1.CloudCredentialsReference {
	return orcv1alpha1.CloudCredentialsReference{
		SecretName: adminPasswordCloudSecretName(cp),
		CloudName:  credRef.CloudName,
	}
}

// managedCatalogEntry is one MANAGED K-ORC CR backing a declared catalog entry, or
// one being removed because the spec stopped declaring it.
type managedCatalogEntry struct {
	kind string
	name string
	obj  orcv1alpha1.ObjectWithConditions
}

// entryWaitMessage appends K-ORC's own message to a bounded-wait message, so the
// failure class the wait is actually blocked on is visible in `kubectl describe`
// even when classifyKORCMessage cannot name it. See korcStatusMessage.
func entryWaitMessage(entry managedCatalogEntry, message string) string {
	if korcMessage := korcStatusMessage(entry.obj); korcMessage != "" {
		return message + "; K-ORC reports: " + korcMessage
	}
	return message
}

// ensureManagedCatalogEntries projects spec.services.keystone.external.catalog.
// managedEntries onto managed K-ORC Service/Endpoint CRs and then garbage-collects
// the entry CRs this ControlPlane owns that the spec no longer declares. It returns
// the projected entries and, separately, the removals K-ORC has not completed yet.
//
// The sweep is what makes "removing the opt-in deletes only that managed entry"
// true: it considers only CRs that are BOTH controller-owned by this ControlPlane
// AND carry the catalog-entry name prefix, so the unmanaged identity imports (and
// any foreign CR sharing the namespace) can never be caught by it. With no
// declared entries the desired set is empty and every previously-declared entry is
// removed — the default posture creates nothing and leaves nothing behind.
func (r *ControlPlaneReconciler) ensureManagedCatalogEntries(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference,
) (reconciled, pruning []managedCatalogEntry, err error) {
	ns := childNamespace(cp)

	for _, entry := range externalManagedCatalogEntries(cp) {
		service := &orcv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: catalogEntryServiceName(cp, entry.Type), Namespace: ns},
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
			service.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
			service.Spec.CloudCredentialsRef = credRef
			service.Spec.Import = nil
			if service.Spec.Resource == nil {
				service.Spec.Resource = &orcv1alpha1.ServiceResourceSpec{}
			}
			service.Spec.Resource.Type = entry.Type
			service.Spec.Resource.Name = nil
			if entry.Name != "" {
				service.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName(entry.Name))
			}
			service.Spec.Resource.Enabled = ptr.To(true)
			return controllerutil.SetControllerReference(cp, service, r.Scheme)
		}); err != nil {
			return nil, nil, fmt.Errorf("catalog entry Service %q: %w", service.Name, err)
		}
		reconciled = append(reconciled, managedCatalogEntry{kind: "Service", name: service.Name, obj: service})

		for _, ep := range entry.Endpoints {
			endpoint := &orcv1alpha1.Endpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name:      catalogEntryEndpointName(cp, entry.Type, ep.Interface),
					Namespace: ns,
				},
			}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
				endpoint.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
				endpoint.Spec.CloudCredentialsRef = credRef
				endpoint.Spec.Import = nil
				if endpoint.Spec.Resource == nil {
					endpoint.Spec.Resource = &orcv1alpha1.EndpointResourceSpec{}
				}
				endpoint.Spec.Resource.Interface = string(ep.Interface)
				endpoint.Spec.Resource.URL = ep.URL
				endpoint.Spec.Resource.ServiceRef = orcv1alpha1.KubernetesNameRef(service.Name)
				endpoint.Spec.Resource.Enabled = ptr.To(true)
				return controllerutil.SetControllerReference(cp, endpoint, r.Scheme)
			}); err != nil {
				return nil, nil, fmt.Errorf("catalog entry Endpoint %q: %w", endpoint.Name, err)
			}
			reconciled = append(reconciled, managedCatalogEntry{kind: "Endpoint", name: endpoint.Name, obj: endpoint})
		}
	}

	pruning, err = r.pruneManagedCatalogEntries(ctx, cp, reconciled)
	if err != nil {
		return nil, nil, err
	}
	return reconciled, pruning, nil
}

// pruneManagedCatalogEntries deletes the opt-in catalog-entry CRs this
// ControlPlane owns which the spec no longer declares. reconciled is the set the
// caller just projected, so an owned, prefixed CR absent from it is undeclared.
// Endpoints go before Services: K-ORC's deletion guard refuses to remove a
// Service an Endpoint still references.
//
// It returns the CRs whose removal has NOT completed — still present after the
// Delete, Terminating behind K-ORC's finalizer while it takes the row out of the
// external catalog. The caller gates CatalogReady on them, symmetrically with
// registration: until the finalizer clears, the row is still live in a catalog this
// ControlPlane does not own, and a removal K-ORC can never complete (a 403 on
// DELETE /v3/endpoints, an endpoint already deleted by hand) must not be reported as
// success.
func (r *ControlPlaneReconciler) pruneManagedCatalogEntries(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, reconciled []managedCatalogEntry,
) ([]managedCatalogEntry, error) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)

	// The kind is compared alongside the name: a Service and an Endpoint can share
	// a name (an entry of type "image-public" and the "public" endpoint of entry
	// "image" both render to "<cp>-catalog-image-public").
	declared := func(kind, name string) bool {
		return slices.ContainsFunc(reconciled, func(e managedCatalogEntry) bool {
			return e.kind == kind && e.name == name
		})
	}

	var pruning []managedCatalogEntry

	var endpoints orcv1alpha1.EndpointList
	if err := r.List(ctx, &endpoints, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing catalog entry Endpoints: %w", err)
	}
	for i := range endpoints.Items {
		endpoint := &endpoints.Items[i]
		if !r.ownsCatalogEntry(cp, endpoint) {
			continue
		}
		if declared("Endpoint", endpoint.Name) {
			continue
		}
		logger.Info("removing an undeclared managed catalog Endpoint", "name", endpoint.Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, endpoint)); err != nil {
			return nil, fmt.Errorf("deleting catalog entry Endpoint %q: %w", endpoint.Name, err)
		}
		// Re-Get so the returned object carries the deletion's live status: K-ORC
		// stamps the failed DELETE onto its conditions.
		fresh := &orcv1alpha1.Endpoint{}
		switch err := r.Get(ctx, client.ObjectKeyFromObject(endpoint), fresh); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("re-checking removed catalog entry Endpoint %q: %w", endpoint.Name, err)
		default:
			pruning = append(pruning, managedCatalogEntry{kind: "Endpoint", name: fresh.Name, obj: fresh})
		}
	}

	var services orcv1alpha1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing catalog entry Services: %w", err)
	}
	for i := range services.Items {
		service := &services.Items[i]
		if !r.ownsCatalogEntry(cp, service) {
			continue
		}
		if declared("Service", service.Name) {
			continue
		}
		logger.Info("removing an undeclared managed catalog Service", "name", service.Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, service)); err != nil {
			return nil, fmt.Errorf("deleting catalog entry Service %q: %w", service.Name, err)
		}
		fresh := &orcv1alpha1.Service{}
		switch err := r.Get(ctx, client.ObjectKeyFromObject(service), fresh); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, fmt.Errorf("re-checking removed catalog entry Service %q: %w", service.Name, err)
		default:
			pruning = append(pruning, managedCatalogEntry{kind: "Service", name: fresh.Name, obj: fresh})
		}
	}

	return pruning, nil
}

// ownsCatalogEntry reports whether obj is a CR this ControlPlane created for a
// declared catalog entry. BOTH the controller reference and the name prefix must
// match: the reference alone would sweep the identity imports, and the prefix
// alone would sweep a same-named CR belonging to somebody else.
func (r *ControlPlaneReconciler) ownsCatalogEntry(cp *c5c3v1alpha1.ControlPlane, obj client.Object) bool {
	return metav1.IsControlledBy(obj, cp) && strings.HasPrefix(obj.GetName(), catalogEntryNamePrefix(cp))
}
