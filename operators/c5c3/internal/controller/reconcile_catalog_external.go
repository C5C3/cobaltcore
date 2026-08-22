// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
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

// keystoneEndpointImportName is the deterministic name of the unmanaged Endpoint
// import CR for one catalog interface. It extends the managed-mode Endpoint name
// with the interface, so the two never collide (modes cannot transition, but the
// deletion sweep enumerates both).
func keystoneEndpointImportName(cp *c5c3v1alpha1.ControlPlane, iface c5c3v1alpha1.ExternalEndpointType) string {
	return keystoneEndpointName(cp) + "-" + string(iface)
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

// reconcileCatalogExternal is the import-first catalog branch. It creates no
// catalog entry.
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
//  1. a classifiable K-ORC message on an unresolved import (auth, TLS,
//     reachability, catalog mismatch) — relayed verbatim, the failure class is
//     only recoverable from the message text
//  2. a terminal K-ORC error on an import, Service before Endpoints so the ROOT is
//     reported. Unlike the waits below this covers every import, required or not:
//     K-ORC has given up on it, which is loud, actionable and never a property of
//     the external catalog merely omitting an interface. The lone exception is an
//     InvalidConfiguration on a non-required import, which no spec edit can repair —
//     see the rationale at the loop
//  3. a REQUIRED import stalled past externalImportStallGrace — the silent-empty
//     hazard
//  4. a REQUIRED import not yet resolved — a legitimate, bounded wait
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

	// 4. Bounded waits.
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
				"(see status.catalog.imports)",
			resolvedInterfaces, len(externalCatalogInterfaces),
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

	// The desired import spec is a pure projection of cp.Spec, so it is applied
	// via Server-Side Apply under the shared field manager.
	serviceFilter := &orcv1alpha1.ServiceFilter{Type: ptr.To(c5c3v1alpha1.IdentityCatalogServiceType)}
	if name := externalIdentityServiceName(cp); name != "" {
		serviceFilter.Name = ptr.To(orcv1alpha1.OpenStackName(name))
	}
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceName(cp), Namespace: ns},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import:              &orcv1alpha1.ServiceImport{Filter: serviceFilter},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, service, apply.FieldManager); err != nil {
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
			Spec: orcv1alpha1.EndpointSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
				CloudCredentialsRef: credRef,
				Import: &orcv1alpha1.EndpointImport{
					Filter: &orcv1alpha1.EndpointFilter{
						Interface:  string(iface),
						ServiceRef: ptr.To(orcv1alpha1.KubernetesNameRef(service.Name)),
					},
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, endpoint, apply.FieldManager); err != nil {
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

// entryCredentialsRef returns the credentials the MANAGED K-ORC children
// authenticate with (the KeystoneService catalog rows, the service-account users
// and their role assignments): the operator-owned password cloud, NOT the spec's
// clouds.yaml (which carries the minted application credential). It resolves the
// cloud name the same way the identity imports do, only against another Secret.
//
// Those children are ManagementPolicyManaged, so K-ORC must reach the external
// Keystone to DELETE them, and the teardown sweep (deleteORCResources) issues
// every Delete in one unsequenced pass. The ApplicationCredential's K-ORC
// finalizer REVOKES the credential at the Keystone level, so a child still
// authenticating through it would get a 404 and stay Terminating until the stall
// escape strips its finalizer, orphaning the resource behind it. The admin
// password outlives the revocation, so pointing the managed children at the same
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
