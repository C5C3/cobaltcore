// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esmetav1 "github.com/external-secrets/external-secrets/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

const (
	// esoTenantStoreName is the namespaced SecretStore the operator provisions
	// per ControlPlane and defaults the control plane onto. It MUST match the
	// name set by deploy/openbao/bootstrap/setup-eso-tenant.sh (the manual
	// onboarding path for standalone Keystone/Horizon) so both provisioning
	// routes converge on the same store, and it is the name standalone CRs set in
	// spec.secretStoreRef.
	esoTenantStoreName = "openbao-tenant-store"
	// esoTenantServiceAccountName is the fixed name of the ServiceAccount the
	// tenant SecretStore authenticates as. The eso-tenant OpenBao role binds this
	// SA name in ANY namespace (setup-auth.sh); the eso-tenant templated policy
	// then confines the token to the caller's OWN namespace, so a fixed name is
	// safe under the one-ControlPlane-per-namespace admission contract.
	esoTenantServiceAccountName = "eso-tenant-auth" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
	// esoTenantClientCertName is the fixed name of the per-ControlPlane
	// cert-manager Certificate / Secret carrying the mTLS client keypair (plus the
	// CA under ca.crt) the tenant SecretStore presents to the OpenBao listener.
	// Fixed to match setup-eso-tenant.sh.
	esoTenantClientCertName = "eso-tenant-client-tls" //nolint:gosec // G101 false positive: cert name, not a credential.
	// esoTenantVaultRole is the OpenBao Kubernetes-auth role the tenant store
	// authenticates against (see setup-auth.sh); it is bound to the eso-tenant
	// templated policy scoping every readable/writable path to the caller's own
	// namespace.
	esoTenantVaultRole = "eso-tenant"
	// esoTenantKVMountPath is the KV-v2 secrets-engine mount the tenant store
	// reads from, matching the shared cluster store (deploy/eso/clustersecretstore.yaml).
	esoTenantKVMountPath = "kv-v2"
	// esoTenantClientCertDuration / renewBefore reuse the DB-credential client
	// certificate lifetimes (dbCredentialClientCert*) so client-cert rotation
	// cadences stay aligned by construction rather than by two literals that can
	// silently drift.
	esoTenantClientCertDuration    = dbCredentialClientCertDuration
	esoTenantClientCertRenewBefore = dbCredentialClientCertRenewBefore
)

// effectiveControlPlaneStoreRef resolves the store the ControlPlane's own
// ExternalSecrets/PushSecrets — and the ref projected onto its Keystone/Horizon
// children — route through. An explicit spec.secretStoreRef is an override and
// wins (normalised via secrets.EffectiveStoreRef so an empty kind resolves to
// ClusterSecretStore); when omitted the operator DEFAULTS to the per-tenant
// namespaced SecretStore (openbao-tenant-store) it provisions in the child
// namespace, so a control plane reaches OpenBao as its own tenant identity
// rather than the shared cluster store. This deliberately differs from
// secrets.EffectiveStoreRef, whose nil default stays the shared cluster store —
// that default is for standalone Keystone/Horizon, which have no operator above
// them to provision a tenant store and onboard the per-tenant identity manually.
func effectiveControlPlaneStoreRef(cp *c5c3v1alpha1.ControlPlane) commonv1.SecretStoreRefSpec {
	if cp.Spec.SecretStoreRef != nil {
		return secrets.EffectiveStoreRef(cp.Spec.SecretStoreRef)
	}
	return commonv1.SecretStoreRefSpec{
		Kind: commonv1.SecretStoreKindNamespaced,
		Name: esoTenantStoreName,
	}
}

// effectiveControlPlaneStoreRefPtr returns effectiveControlPlaneStoreRef as an
// independent pointer suitable for projection onto a child CR's
// spec.secretStoreRef. The projected ref is always concrete (never nil), so a
// child never falls back to its own shared-store default — the control plane's
// store selection is authoritative.
func effectiveControlPlaneStoreRefPtr(cp *c5c3v1alpha1.ControlPlane) *commonv1.SecretStoreRefSpec {
	ref := effectiveControlPlaneStoreRef(cp)
	return &ref
}

// esoTenantServiceAccount builds the ServiceAccount the tenant SecretStore in
// namespace authenticates as. PURE builder: the reconciler claims ownership in
// the apply path.
//
// One is provisioned per namespace the ControlPlane occupies. The fixed name is
// safe because the eso-tenant OpenBao role binds it in ANY namespace and the
// templated policy then confines the token to the caller's OWN namespace — and
// because a namespace belongs to at most one ControlPlane (the webhook's
// namespace-claim check), so two ControlPlanes can never contend for this name.
func esoTenantServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantServiceAccountName, Namespace: namespace},
	}
}

// applyESOTenantCertificateSpec sets the desired spec fields on the tenant mTLS
// client Certificate, mirroring applyDBCredentialCertificateSpec. Extracted so
// the CreateOrUpdate mutate closure can re-assert the spec on an existing object
// without clobbering cert-manager-managed status.
func applyESOTenantCertificateSpec(u *unstructured.Unstructured, namespace string) {
	// SetNested* only errors on a type conflict at an existing path; on a
	// freshly-built or Certificate-typed object the writes cannot fail, so the
	// errors are intentionally ignored here.
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertName, "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertDuration, "spec", "duration")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertRenewBefore, "spec", "renewBefore")
	_ = unstructured.SetNestedStringSlice(u.Object, []string{"client auth"}, "spec", "usages")
	_ = unstructured.SetNestedField(u.Object, esoTenantClientCertName+"."+namespace+".svc", "spec", "commonName")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"name": openBaoCAIssuerName,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
}

// esoTenantSecretStore builds the namespaced per-ControlPlane SecretStore that
// authenticates to OpenBao as the eso-tenant role with the eso-tenant-auth SA.
// All Secret references are same-namespace (a namespaced SecretStore resolves
// them locally): the client cert/key and CA all come from the tenant client
// Certificate's Secret. server and mountPath are the OpenBao connection resolved
// from the SHARED cluster store (the tenant store cannot describe its own
// bootstrap). Mirrors deploy/openbao/bootstrap/setup-eso-tenant.sh.
func esoTenantSecretStore(namespace, server, mountPath string) *esov1.SecretStore {
	return &esov1.SecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: esoTenantStoreName, Namespace: namespace},
		Spec: esov1.SecretStoreSpec{
			Provider: &esov1.SecretStoreProvider{
				Vault: &esov1.VaultProvider{
					Server: server,
					Path:   ptr.To(esoTenantKVMountPath),
					// Version has no omitempty, so leaving it unset serializes as
					// "" and the ESO CRD enum (v1|v2) rejects the object — the CRD
					// default only applies to ABSENT fields. Pin the ESO default.
					Version: esov1.VaultKVStoreV2,
					Auth: &esov1.VaultAuth{
						Kubernetes: &esov1.VaultKubernetesAuth{
							Path:              mountPath,
							Role:              esoTenantVaultRole,
							ServiceAccountRef: &esmetav1.ServiceAccountSelector{Name: esoTenantServiceAccountName},
						},
					},
					ClientTLS: esov1.VaultClientTLS{
						CertSecretRef: &esmetav1.SecretKeySelector{Name: esoTenantClientCertName},
						KeySecretRef:  &esmetav1.SecretKeySelector{Name: esoTenantClientCertName},
					},
					CAProvider: &esov1.CAProvider{
						Type: esov1.CAProviderTypeSecret,
						Name: esoTenantClientCertName,
						Key:  "ca.crt",
					},
				},
			},
		},
	}
}

// reconcileESOTenantStore provisions the in-cluster half of the ControlPlane's
// per-tenant OpenBao identity — the eso-tenant-auth ServiceAccount, the tenant
// mTLS client Certificate, and the namespaced openbao-tenant-store SecretStore —
// and drives ESOTenantStoreReady from that SecretStore's Ready condition. It is
// the enforced default: every ControlPlane that omits spec.secretStoreRef routes
// its (and its children's) secret traffic through this per-tenant store, so
// OpenBao itself enforces cross-tenant isolation via the templated eso-tenant
// policy rather than a naming convention. Registered ahead of the store-consuming
// sub-reconcilers (DBCredentials, AdminPassword, Keystone, ...) so the store
// exists before they gate on it.
//
// OVERRIDE: a ControlPlane that sets an explicit spec.secretStoreRef opts out of
// the operator-provisioned store — it owns the referenced store's lifecycle — so
// the operator provisions nothing and reports ESOTenantStoreReady=True with the
// StoreRefOverridden reason. The store-consuming sub-reconcilers still gate on
// the selected store's own readiness.
func (r *ControlPlaneReconciler) reconcileESOTenantStore(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if cp.Spec.SecretStoreRef != nil {
		ref := secrets.EffectiveStoreRef(cp.Spec.SecretStoreRef)
		logger.Info("explicit secretStoreRef set; not provisioning the per-tenant store",
			"kind", ref.Kind, "name", ref.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeESOTenantStoreReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "StoreRefOverridden",
			Message: fmt.Sprintf("explicit spec.secretStoreRef (%s %q) overrides the operator-provisioned per-tenant "+
				"store; its readiness is gated by the store-consuming sub-reconcilers", ref.Kind, ref.Name),
		})
		return ctrl.Result{}, nil
	}

	if err := r.ensureESOTenantStoreObjects(ctx, cp); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeESOTenantStoreReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ProvisioningError",
			Message:            fmt.Sprintf("ensuring per-tenant secret store objects: %v", err),
		})
		return ctrl.Result{}, err
	}

	// EVERY store must be Ready, not just the one in the ControlPlane's namespace:
	// a service placed in a namespace of its own materialises its secret material
	// through THAT namespace's store, so a store still issuing its client cert
	// there is as load-bearing as the one at home. The first unready store is named
	// with its namespace so the condition says which one to look at.
	namespaces := controlPlaneNamespaces(cp)
	for _, ns := range namespaces {
		ready, err := secrets.IsSecretStoreReady(ctx, r.Client, esoTenantStoreName, ns)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			logger.Info("per-tenant secret store not ready yet, requeuing", "namespace", ns)
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeESOTenantStoreReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "SecretStoreNotReady",
				Message: fmt.Sprintf("per-tenant SecretStore %q in namespace %q is not ready yet; waiting on cert "+
					"issuance and the OpenBao backend", esoTenantStoreName, ns),
			})
			return ctrl.Result{RequeueAfter: esoTenantStoreRequeueAfter}, nil
		}
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeESOTenantStoreReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "ESOTenantStoreReady",
		Message: fmt.Sprintf("per-tenant SecretStore %q is Ready in all %d namespace(s) of this ControlPlane",
			esoTenantStoreName, len(namespaces)),
	})
	return ctrl.Result{}, nil
}

// ensureESOTenantStoreObjects create-or-updates the ServiceAccount, the mTLS
// client Certificate, and the namespaced SecretStore — in EVERY namespace the
// ControlPlane occupies. Ordering (SA → Certificate → SecretStore) makes the auth
// identity and TLS material exist before the store that references them,
// mirroring ensureDynamicDBCredentialObjects.
//
// SECRET DISTRIBUTION ACROSS NAMESPACES. An ESO SecretStore and the Secrets it
// materialises are namespace-local: a store in the ControlPlane's namespace
// cannot deliver anything into a service namespace. So each namespace hosting a
// service gets its OWN tenant store — its own ServiceAccount, its own client
// certificate, its own SecretStore object. That needs no OpenBao-side change: the
// eso-tenant role binds the SA name in ANY namespace and its templated policy
// scopes every path to the caller's own namespace, so each store authenticates as
// its own tenant identity and reaches only its own paths.
//
// In the ControlPlane's own namespace the objects are owner-referenced and the GC
// cascade reaps them; elsewhere they carry the ownership labels and the teardown
// deletes them explicitly.
func (r *ControlPlaneReconciler) ensureESOTenantStoreObjects(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	// server/mountPath come from the SHARED cluster store, not the tenant stores
	// this method is building — a tenant store cannot describe its own OpenBao
	// connection. openBaoConnection falls back to the documented defaults when the
	// shared store is unreadable, which match the tenant stores' connection anyway.
	// Every tenant store copies the same connection by construction, so it is
	// resolved once for all of them.
	server, mountPath := r.openBaoConnection(ctx, cp, secrets.EffectiveStoreRef(nil))

	for _, ns := range controlPlaneNamespaces(cp) {
		if err := r.ensureUnownedOrOwned(ctx, cp, esoTenantServiceAccount(ns)); err != nil {
			return fmt.Errorf("ensuring per-tenant ServiceAccount in namespace %q: %w", ns, err)
		}

		// The cert-manager Certificate is handled as an unstructured object (no Go
		// module ships its type), and apply.EnsureObject converts typed structs, not
		// unstructured inputs — so this projection stays read-modify-write while the
		// typed sibling objects around it use Server-Side Apply.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(certificateGVK)
		live.SetName(esoTenantClientCertName)
		live.SetNamespace(ns)
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
			if err := refuseForeignAdoption(cp, live, r.Scheme); err != nil {
				return err
			}
			applyESOTenantCertificateSpec(live, ns)
			return claimChildOwnership(cp, live, r.Scheme)
		}); err != nil {
			return fmt.Errorf("ensuring per-tenant client Certificate in namespace %q: %w", ns, err)
		}

		if err := r.ensureUnownedOrOwned(ctx, cp, esoTenantSecretStore(ns, server, mountPath)); err != nil {
			return fmt.Errorf("ensuring per-tenant SecretStore in namespace %q: %w", ns, err)
		}
	}
	return nil
}
