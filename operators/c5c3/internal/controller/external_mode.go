// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// Condition reasons that only External keystone mode produces. They are the
// single source of truth for the External-mode status contract: call sites MUST
// reference these constants rather than inline string literals, mirroring the
// conditionType* block in controlplane_controller.go.
//
// The vocabulary deliberately keeps three "nothing was projected" reasons apart,
// because each answers a different operator question:
//
//   - ExternallyManaged — the ControlPlane manages identity against a
//     pre-existing Keystone, so this sub-reconciler has nothing to project.
//   - KeystoneNotManaged — spec.services.keystone is unset: there is no identity
//     plane at all (staged adoption).
//   - BrownfieldUserSuppliedCredential — the ControlPlane owns a Keystone but
//     the *database* is brownfield, so the user supplies its credential Secret.
//
// Collapsing them would make "why is nothing deployed?" unanswerable from
// `kubectl describe` alone.
const (
	// conditionReasonExternallyManaged marks a sub-reconciler that skipped its
	// projection because the ControlPlane's Keystone is externally operated. The
	// condition still reports Status=True so the condition schema — and therefore
	// subConditionTypes, setReadyCondition and the condition_type drift guard —
	// is identical across modes.
	conditionReasonExternallyManaged = "ExternallyManaged"

	// conditionReasonAuthenticationFailed reports that the external Keystone
	// rejected the operator's admin credentials (HTTP 401). In practice this is
	// the "admin password rotated out-of-band, passwordSecretRef is stale" drift.
	conditionReasonAuthenticationFailed = "AuthenticationFailed"

	// conditionReasonEndpointUnreachable reports that the external Keystone's
	// authURL could not be dialled (DNS failure, connection refused, timeout).
	conditionReasonEndpointUnreachable = "EndpointUnreachable"

	// conditionReasonTLSVerificationFailed reports that the external Keystone's
	// certificate did not verify against the client's trust store — typically a
	// private CA that spec.services.keystone.external.caBundleSecretRef must
	// supply.
	conditionReasonTLSVerificationFailed = "TLSVerificationFailed"

	// conditionReasonCatalogEndpointMismatch reports that authentication
	// succeeded but the requested interface/region is absent from the external
	// Keystone's service catalog — a wrong spec.services.keystone.external.
	// endpointType or spec.region. It fails loud rather than silently importing
	// nothing.
	conditionReasonCatalogEndpointMismatch = "CatalogEndpointMismatch"

	// conditionReasonImportStalled reports the silent-empty hazard: a K-ORC
	// import that has been waiting to be "created externally" beyond
	// externalImportStallGrace. In External mode every import target pre-exists
	// by definition, so a persistent wait is a misconfiguration signal, never a
	// legitimate wait. The reason is shared vocabulary with the catalog
	// import-first work.
	conditionReasonImportStalled = "ImportStalled"

	// conditionReasonCredentialDrift reports that the operator's view of the
	// external Keystone's admin identity no longer matches reality — the admin
	// user was recreated behind a resolve-once import id, or the admin password
	// changed without the referenced Secret following. Drift is SURFACED, never
	// fought: the operator does not write to the external installation.
	conditionReasonCredentialDrift = "CredentialDrift"

	// conditionReasonInfrastructureNotConfigured reports a non-External
	// ControlPlane that reached reconcileInfrastructure with no
	// spec.infrastructure block. The validating webhook requires the block
	// outside External mode, so this fails closed for a webhook-bypassed CR
	// rather than dereferencing the nil pointer.
	conditionReasonInfrastructureNotConfigured = "InfrastructureNotConfigured"
)

// korcImportPendingExternalMarker is the message K-ORC stamps on an unmanaged
// import's Available=False condition while the OpenStack resource it filters for
// does not (yet) exist. It is the substring korcImportStalled keys the
// silent-empty detector on.
const korcImportPendingExternalMarker = "Waiting for OpenStack resource to be created externally"

// externalKeystoneAuthURL returns the external Keystone's identity endpoint, or
// "" when the ControlPlane is not in External mode (or the block is absent, which
// admission forbids). It is nil-safe on every level so the ExternallyManaged and
// drift messages can name the endpoint without a guard at each call site.
//
// This helper is MESSAGE-ONLY. The mode-switching auth-URL resolver the
// clouds.yaml builders consume — the one that decides whether K-ORC dials the
// in-cluster Service DNS or this external endpoint — is a separate concern and
// lands with the External-mode K-ORC/AdminCredential work.
func externalKeystoneAuthURL(cp *c5c3v1alpha1.ControlPlane) string {
	ks := cp.Spec.Services.Keystone
	if ks == nil || ks.External == nil {
		return ""
	}
	return ks.External.AuthURL
}

// classifyKORCMessage maps a K-ORC condition message onto an External-mode
// condition reason, returning "" when the message matches no known failure class.
//
// SPIKE CONSTRAINT: K-ORC collapses EVERY hard failure against the OpenStack API
// — a 401, a DNS/dial error, a TLS verification failure, a catalog mismatch —
// into the same non-terminal Progressing condition with reason=TransientError.
// Nothing in the observed inventory is terminal. The failure CLASS is therefore
// only distinguishable from the free-text message, so the ControlPlane matches on
// message substrings and relays K-ORC's message verbatim alongside the reason.
//
// The order below is the documented precedence, most specific first, and is
// pinned by TestClassifyKORCMessage:
//
//  1. catalog mismatch  — the full gophercloud sentence, unambiguous
//  2. credential drift  — a 403 naming the application-credential policy rule,
//     which is what an AC create against a stale user id yields
//  3. TLS              — "x509", which a dial error may also mention
//  4. authentication   — 401 / Unauthorized
//  5. reachability     — DNS / dial / timeout markers
func classifyKORCMessage(msg string) string {
	switch {
	case strings.Contains(msg, "No suitable endpoint could be found in the service catalog"):
		return conditionReasonCatalogEndpointMismatch
	case strings.Contains(msg, "identity:create_application_credential"):
		return conditionReasonCredentialDrift
	case strings.Contains(msg, "x509"):
		return conditionReasonTLSVerificationFailed
	case strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized"):
		return conditionReasonAuthenticationFailed
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "i/o timeout"):
		return conditionReasonEndpointUnreachable
	default:
		return ""
	}
}

// classifyKORCObject returns the External-mode reason for the first condition on
// obj whose message classifies, together with that message VERBATIM. An
// unclassifiable object yields ("", "").
//
// The message is relayed byte-for-byte rather than reworded: K-ORC's text is the
// only place the underlying gophercloud/OpenStack error survives, so truncating
// or paraphrasing it would destroy the very signal the classification points at.
func classifyKORCObject(obj orcv1alpha1.ObjectWithConditions) (reason, rawMessage string) {
	if obj == nil {
		return "", ""
	}
	for _, cond := range obj.GetConditions() {
		if reason := classifyKORCMessage(cond.Message); reason != "" {
			return reason, cond.Message
		}
	}
	return "", ""
}

// classifyExternalKORCFailure walks objs in order and returns the first
// classifiable failure. Call sites pass the admin Domain, then the admin User,
// then the ApplicationCredential — the same dependency order ensureKORCAdminImports
// uses for its status fragment, so the ROOT dependency is reported rather than the
// downstream resource that merely blocked on it.
func classifyExternalKORCFailure(objs ...orcv1alpha1.ObjectWithConditions) (reason, rawMessage string) {
	for _, obj := range objs {
		if reason, rawMessage := classifyKORCObject(obj); reason != "" {
			return reason, rawMessage
		}
	}
	return "", ""
}

// korcImportStalled reports whether a K-ORC import has been waiting for its
// OpenStack resource to appear for longer than grace — the silent-empty detector.
//
// In External mode every import target (the admin Domain, the admin User) exists
// in the external Keystone BY DEFINITION: the installation predates the
// ControlPlane. An import that stays Available=False on
// korcImportPendingExternalMarker therefore never resolves on its own. It means
// K-ORC looked in the wrong place — a catalog interface or region that resolves to
// a different Keystone, or an authURL pointing at an empty deployment — and the
// only honest signal is a loud condition, not an eternal wait.
//
// A missing or True Available condition, a different message, or a transition
// inside the grace window all read as "not stalled".
func korcImportStalled(obj orcv1alpha1.ObjectWithConditions, grace time.Duration) bool {
	if obj == nil {
		return false
	}
	for _, cond := range obj.GetConditions() {
		if cond.Type != orcv1alpha1.ConditionAvailable {
			continue
		}
		return cond.Status == metav1.ConditionFalse &&
			strings.Contains(cond.Message, korcImportPendingExternalMarker) &&
			time.Since(cond.LastTransitionTime.Time) > grace
	}
	return false
}
