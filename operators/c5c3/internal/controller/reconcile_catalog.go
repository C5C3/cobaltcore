// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// CatalogReady reasons shared by BOTH branches of reconcileCatalog. The
// managed-only reasons stay inline at their single call site; these two are
// stamped from reconcile_catalog_external.go as well, so a literal in each file
// would be a drift hazard.
const (
	// conditionReasonCatalogFailed reports a TERMINAL K-ORC failure on a catalog
	// child CR: an unrecoverable/invalid-configuration error K-ORC will not retry.
	conditionReasonCatalogFailed = "CatalogFailed"

	// conditionReasonWaitingForCatalog reports that the catalog children are
	// reconciled but not yet Available for their current generation.
	conditionReasonWaitingForCatalog = "WaitingForCatalog"
)

// reconcileCatalog drives the CatalogReady condition. What it does depends on
// the Keystone mode, and the two postures are opposites:
//
//   - Managed — the ControlPlane OWNS the catalog. It registers the OpenStack
//     service catalog entries for Keystone (an identity Service plus its public
//     Endpoint) as managed K-ORC CRs, which K-ORC creates in Keystone.
//   - External — the catalog belongs to the pre-existing installation, so the
//     ControlPlane IMPORTS it instead (reconcileCatalogExternal). Creating
//     entries against a populated catalog would duplicate rows Keystone does not
//     deduplicate, so it never happens by default.
//
// Both branches are GATED on AdminCredentialReady: the admin credential must be
// available before K-ORC can talk to Keystone at all. Child CRs are
// create-or-updated idempotently. K-ORC is a hard CRD dependency (see
// reconcileKORC), so a missing Service/Endpoint CRD never reaches here — no-match
// errors fall through to the generic error returns below (#476).
func (r *ControlPlaneReconciler) reconcileCatalog(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fail := conditionFailer(cp, conditionTypeCatalogReady)

	// Gate on AdminCredentialReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		logger.Info("AdminCredential not ready, deferring catalog registration")
		fail("WaitingForAdminCredential", "AdminCredentialReady is not True; catalog registration deferred")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	secretName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	// Fall back to the conventional name when SecretName is empty, matching
	// reconcileAdminCredential and ensureKORCCloudsYAMLExternalSecret so a
	// webhook-bypass CR resolves to the same clouds.yaml Secret name everywhere
	// (#476). Without this the catalog Service/Endpoint would reference an empty
	// CloudCredentialsRef.SecretName.
	if secretName == "" {
		secretName = korcCloudsYamlSecretName
	}
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: secretName, CloudName: cloudName}

	// Fork on the mode discriminator. Everything above is mode-agnostic (the gate
	// and the admin credential K-ORC authenticates with); everything below owns
	// the catalog and therefore only runs in Managed mode.
	if cp.IsExternalKeystone() {
		return r.reconcileCatalogExternal(ctx, cp, credRef)
	}

	// 1. Identity (Keystone) Service. The desired spec is a pure projection of
	// cp.Spec, so it is applied via Server-Side Apply under the shared field
	// manager rather than read-modify-write.
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: orcv1alpha1.ServiceSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.ServiceResourceSpec{
				Type:    "identity",
				Name:    ptr.To(orcv1alpha1.OpenStackName("keystone")),
				Enabled: ptr.To(true),
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, service, apply.FieldManager); err != nil {
		fail("ServiceError", fmt.Sprintf("applying identity Service: %v", err))
		return ctrl.Result{}, err
	}

	// 2. Public Endpoint for the Keystone API.
	//
	// DECISION (Endpoint URL): K-ORC's EndpointResourceSpec.URL is REQUIRED.
	// When the ControlPlane exposes Keystone externally (a gateway or explicit
	// publicEndpoint is set) we register that public URL so the catalog matches
	// what Keystone's own bootstrap advertises; otherwise we fall back to the
	// in-cluster Keystone Service URL derived from the PROJECTED Keystone
	// Service — keystoneName(cp) = "{cp.Name}-keystone" in the ControlPlane
	// namespace — which is the Service the keystone-operator actually exposes.
	endpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneEndpointName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: orcv1alpha1.EndpointSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: credRef,
			Resource: &orcv1alpha1.EndpointResourceSpec{
				Interface:  "public",
				URL:        keystoneCatalogURL(cp),
				ServiceRef: orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp)),
				Enabled:    ptr.To(true),
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, cp, endpoint, apply.FieldManager); err != nil {
		fail("EndpointError", fmt.Sprintf("applying identity Endpoint: %v", err))
		return ctrl.Result{}, err
	}

	// Gate CatalogReady on BOTH child CRs reporting Available, and surface a TERMINAL
	// K-ORC failure distinctly: registering the Service/Endpoint CRs only instructs
	// K-ORC to create the catalog entries — it does not mean the entries exist in
	// Keystone. The documented failure class (wrong clouds.yaml endpoint, K-ORC
	// swallowing list errors, an import hung on "created externally") otherwise lets
	// CatalogReady (and the aggregate Ready) report True while the catalog is empty.
	if termErr := orcv1alpha1.GetTerminalError(service); termErr != nil {
		return r.catalogTerminalError(cp, "identity Service", service.Name, termErr), nil
	}
	if termErr := orcv1alpha1.GetTerminalError(endpoint); termErr != nil {
		return r.catalogTerminalError(cp, "identity Endpoint", endpoint.Name, termErr), nil
	}
	if !korcAvailableUpToDate(service) || !korcAvailableUpToDate(endpoint) {
		logger.Info("catalog Service/Endpoint not yet Available, requeuing")
		fail(conditionReasonWaitingForCatalog, "Keystone identity Service and Endpoint are registered but not yet Available")
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "CatalogRegistered",
		Message:            "Keystone identity Service and Endpoint are registered and Available",
	})
	return ctrl.Result{}, nil
}

// catalogTerminalError records a terminal K-ORC catalog failure: it sets
// CatalogReady=False/CatalogFailed naming the failing child CR. It requeues so a
// fixed configuration (e.g. a corrected clouds.yaml) is re-evaluated rather than
// leaving the catalog wedged.
func (r *ControlPlaneReconciler) catalogTerminalError(cp *c5c3v1alpha1.ControlPlane, kind, name string, termErr error) ctrl.Result {
	conditionFailer(cp, conditionTypeCatalogReady)(conditionReasonCatalogFailed,
		fmt.Sprintf("K-ORC reported a terminal error registering the %s %q: %v", kind, name, termErr))
	return ctrl.Result{RequeueAfter: korcRequeueAfter}
}

// korcAvailableUpToDate reports whether a K-ORC resource is Available for its
// CURRENT generation: the Available condition exists, is True, AND its
// ObservedGeneration matches the object's generation. Unlike orcv1alpha1.IsAvailable
// — which is generation-blind — it refuses to treat a stale Available condition
// (left over from before a spec edit, e.g. a changed publicEndpoint/region that
// moved keystoneCatalogURL, that K-ORC has not yet re-reconciled) as a live result.
// This mirrors the generation gate orcv1alpha1.GetTerminalError already applies via
// its Progressing check, so a gate cannot flip True advertising a value K-ORC has
// not yet applied.
func korcAvailableUpToDate(obj orcv1alpha1.ObjectWithConditions) bool {
	for _, c := range obj.GetConditions() {
		if c.Type == orcv1alpha1.ConditionAvailable {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration == obj.GetGeneration()
		}
	}
	return false
}

// keystoneServiceName / keystoneEndpointName return the deterministic names of
// the owned K-ORC Service/Endpoint CRs registering the identity catalog entry.
func keystoneServiceName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-service"
}

func keystoneEndpointName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-identity-endpoint"
}

// keystoneEndpointURL derives the in-cluster Keystone identity URL from the
// projected Keystone Service — keystoneName(cp) = "{cp.Name}-keystone" — in the
// ControlPlane namespace (see DECISION on Endpoint URL in reconcileCatalog). It
// must NOT hard-code "keystone": the keystone-operator names the Service after
// the projected Keystone CR, so a fixed name would not resolve. This is the URL
// K-ORC authenticates against (the seeded clouds.yaml auth_url): K-ORC runs
// in-cluster, so it must always use the Service DNS, never the external endpoint.
func keystoneEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("http://%s.%s.svc:5000/v3", keystoneName(cp), childNamespace(cp))
}

// keystoneCatalogURL returns the URL registered for the K-ORC identity catalog
// Endpoint. It prefers the externally routable publicEndpoint (keystonePublicEndpoint)
// so the catalog matches what Keystone's own bootstrap advertises when exposed
// via a Gateway; absent external exposure it falls back to the in-cluster
// Service URL (keystoneEndpointURL).
func keystoneCatalogURL(cp *c5c3v1alpha1.ControlPlane) string {
	if pe := keystonePublicEndpoint(cp.Spec.Services.Keystone); pe != "" {
		return pe
	}
	return keystoneEndpointURL(cp)
}
