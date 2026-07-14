// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// webSSOAuthWebssoPath is the path Horizon POSTs a returned WebSSO token to.
// Keystone matches the origin the dashboard sends VERBATIM, including this
// path and its trailing slash, so it is appended to the dashboard's public
// endpoint to form the trusted_dashboard entry.
const webSSOAuthWebssoPath = "/auth/websso/"

// listIdentityBackends returns the live KeystoneIdentityBackend CRs attached to
// the ControlPlane's Keystone child. Backends live beside the Keystone they
// reference (the backend's keystoneRef is same-namespace by CRD contract),
// which is cp.KeystoneNamespace() — the namespace the Keystone service is placed
// in, not necessarily the ControlPlane's; the List is served from the informer cache and
// holds one backend per identity provider plus one per LDAP domain, so the
// keystoneRef filter runs in memory.
//
// A Terminating backend is dropped. Its own reconcileDelete never demotes
// Ready — it parks on a requeue until the Keystone sub-reconciler de-projects
// the backend's config, then tears the federation objects down — so a backend
// stuck in teardown would otherwise keep Ready=True for the whole (unbounded)
// window and stay "attached" to the projection: it would contribute a websso
// choice whose Keystone-side identity provider, mapping and protocol are being
// deleted, it would pin a stale websso block through backendsAwaitingReady, and
// it would collide on choice id with the same-named replacement the backend
// webhook deliberately admits while the old one is Terminating.
func (r *ControlPlaneReconciler) listIdentityBackends(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) ([]keystonev1alpha1.KeystoneIdentityBackend, error) {
	var list keystonev1alpha1.KeystoneIdentityBackendList
	if err := r.List(ctx, &list, client.InNamespace(cp.KeystoneNamespace())); err != nil {
		return nil, fmt.Errorf("listing identity backends for Keystone %s: %w", keystoneName(cp), err)
	}

	var out []keystonev1alpha1.KeystoneIdentityBackend
	for _, b := range list.Items {
		if b.DeletionTimestamp != nil {
			continue
		}
		if b.Spec.KeystoneRef.Name == keystoneName(cp) {
			out = append(out, b)
		}
	}
	return out, nil
}

// readyFederationBackends filters to the OIDC backends that have reached
// Ready, sorted by identity-provider name so the projected websso choices are
// deterministic across reconciles (an unstable order would churn the Horizon
// child's spec and, through it, the rendered settings ConfigMap name).
//
// The Ready gate is what makes the login page honest: a choice for a backend
// whose Keystone-side federation objects are not provisioned yet would render
// an SSO button that dead-ends.
func readyFederationBackends(backends []keystonev1alpha1.KeystoneIdentityBackend) []keystonev1alpha1.KeystoneIdentityBackend {
	var out []keystonev1alpha1.KeystoneIdentityBackend
	for _, b := range backends {
		if b.Spec.Type == keystonev1alpha1.IdentityBackendTypeOIDC && conditions.IsReady(b.Status.Conditions) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].EffectiveIdentityProviderName() < out[j].EffectiveIdentityProviderName()
	})
	return out
}

// backendsAwaitingReady reports whether at least one backend of the given type
// is attached to the Keystone child but none of them has reached Ready.
//
// The Horizon projection uses it to tell "the operator detached the backend"
// apart from "the backend is not healthy right now". A backend's aggregate
// Ready is derived from sub-conditions the backend reconciler can demote on a
// failed observation — the Keystone Deployment briefly not mounting the
// rendered config, for instance. Rebuilding the dashboard's federated-login
// surface from that view would strip the SSO button, re-render
// local_settings.py, roll the Horizon Deployment, and roll it back on recovery
// — all while the Keystone-side federation objects the backend provisioned
// never went away, so the button worked the whole time.
func backendsAwaitingReady(backends []keystonev1alpha1.KeystoneIdentityBackend, backendType keystonev1alpha1.IdentityBackendType) bool {
	attached := false
	for i := range backends {
		b := &backends[i]
		if b.Spec.Type != backendType {
			continue
		}
		if conditions.IsReady(b.Status.Conditions) {
			return false
		}
		attached = true
	}
	return attached
}

// hasReadyDomainBackend reports whether any LDAP backend has reached Ready.
// One is enough: the login form's domain handling is a single on/off switch,
// and the operator cannot enumerate the domains it does not back (SQL domains
// created out-of-band, the domain an OIDC backend targets), so it never
// narrows the form to a fixed set of domains.
func hasReadyDomainBackend(backends []keystonev1alpha1.KeystoneIdentityBackend) bool {
	for i := range backends {
		b := &backends[i]
		if b.Spec.Type == keystonev1alpha1.IdentityBackendTypeLDAP && conditions.IsReady(b.Status.Conditions) {
			return true
		}
	}
	return false
}

// maxWebSSOChoiceIDLen mirrors the Horizon CRD's websso.choices[].id
// MaxLength=64 marker.
const maxWebSSOChoiceIDLen = 64

// webSSOChoiceID derives the login-form choice id for a federation backend:
// "<identityProvider>_<protocol>". It is the key WEBSSO_IDP_MAPPING is keyed
// on, and the value the form submits as auth_type.
//
// Both source fields are bounded at 64 characters by the
// KeystoneIdentityBackend CRD, so the concatenation reaches 129 — twice the
// Horizon CRD's bound on choices[].id. An over-long id would make the API
// server reject the projected Horizon child, wedging HorizonReady and every
// later dashboard change behind a failing CreateOrUpdate. Truncate and append
// a digest of the full id so distinct backends keep distinct choice ids; the
// id is opaque to the browser, which only ever echoes it back as auth_type.
func webSSOChoiceID(b *keystonev1alpha1.KeystoneIdentityBackend) string {
	id := b.EffectiveIdentityProviderName() + "_" + b.EffectiveProtocolID()
	if len(id) <= maxWebSSOChoiceIDLen {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:4])
	return id[:maxWebSSOChoiceIDLen-len(suffix)] + suffix
}

// horizonPublicEndpoint returns the BROWSER-observed dashboard base URL, with
// any trailing slash trimmed so the derived WebSSO origin carries exactly one.
// An explicit publicEndpoint wins; otherwise, when a gateway is set, it is
// derived as "https://{gateway.hostname}" (the default-443 form). When neither
// is set it returns "" — no dashboard is externally reachable, so there is no
// origin to trust.
//
// It mirrors keystonePublicEndpoint and takes the *ServiceHorizonSpec for the
// same reason: a nil pointer (services.horizon unset) yields "".
func horizonPublicEndpoint(hz *c5c3v1alpha1.ServiceHorizonSpec) string {
	if hz == nil {
		return ""
	}
	if hz.PublicEndpoint != "" {
		return strings.TrimRight(hz.PublicEndpoint, "/")
	}
	if hz.Gateway != nil && hz.Gateway.Hostname != "" {
		return "https://" + hz.Gateway.Hostname
	}
	return ""
}

// identityBackendToControlPlaneMapper maps a KeystoneIdentityBackend event onto
// the ControlPlane whose Keystone child the backend attaches to.
//
// A plain Owns() would never fire: the backends are authored by the operator,
// not projected by the ControlPlane, so they carry no ControlPlane owner
// reference. The List is cluster-wide, not namespace-scoped: a backend attaches
// to the Keystone child, which may be placed in a namespace of its own, so the
// ControlPlane it belongs to lives elsewhere. The match is on the keystone
// NAMESPACE and the child name together, so a backend attached to a hand-rolled
// Keystone beside a ControlPlane still never wakes it.
func (r *ControlPlaneReconciler) identityBackendToControlPlaneMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	backend, ok := obj.(*keystonev1alpha1.KeystoneIdentityBackend)
	if !ok {
		return nil
	}

	var list c5c3v1alpha1.ControlPlaneList
	if err := r.List(ctx, &list); err != nil {
		// Surface the failure rather than silently dropping the event: an
		// unhealthy informer cache would otherwise leave the websso projection
		// stale until the next periodic resync, with no operational signal.
		logger.Error(err, "listing ControlPlanes for identity-backend event",
			"backend", client.ObjectKeyFromObject(backend))
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		cp := &list.Items[i]
		if cp.KeystoneNamespace() == backend.Namespace && keystoneName(cp) == backend.Spec.KeystoneRef.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
		}
	}
	return requests
}
