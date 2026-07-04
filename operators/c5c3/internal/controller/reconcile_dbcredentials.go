// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	esmetav1 "github.com/external-secrets/external-secrets/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// dbCredentialSecretNameSuffix is appended to keystoneName(cp) to derive the
// deterministic, collision-free name of the per-ControlPlane service DB-credential
// Secret/ExternalSecret, mirroring the *Suffix discipline used
// by the K-ORC admin-credential names so a single namespace can host the DB
// credential of multiple ControlPlanes.
const dbCredentialSecretNameSuffix = "-db-credentials" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

const (
	// dbDynamicVaultRole is the OpenBao Kubernetes-auth role the generator
	// authenticates against (see deploy/openbao/bootstrap/setup-auth.sh); it is
	// bound to the keystone-db-dynamic policy scoping reads to the per-tenant
	// creds path.
	dbDynamicVaultRole = "keystone-db"
	// dbCredentialServiceAccountName is the fixed name of the per-ControlPlane
	// ServiceAccount whose token the VaultDynamicSecret generator presents to
	// OpenBao. A fixed name is safe because the one-ControlPlane-per-namespace
	// webhook guarantees a single ControlPlane per childNamespace.
	dbCredentialServiceAccountName = "keystone-db-creds" //nolint:gosec // G101 false positive: ServiceAccount name, not a credential.
	// dbCredentialClientCertSuffix names the per-ControlPlane cert-manager
	// Certificate / Secret carrying the mTLS client keypair the generator uses to
	// satisfy the OpenBao listener's require-and-verify-client-cert gate.
	dbCredentialClientCertSuffix = "-db-openbao-client" //nolint:gosec // G101 false positive: cert name suffix, not a credential.
	// openBaoDefaultServer / openBaoDefaultKubernetesMount are the fallbacks used
	// when the openbao-cluster-store's provider config cannot be read; they match
	// deploy/eso/clustersecretstore.yaml.
	openBaoDefaultServer          = "https://openbao.openbao-system.svc:8200"
	openBaoDefaultKubernetesMount = "kubernetes/management"
	// openBaoCAIssuerName is the cluster-scoped cert-manager CA issuer that signs
	// the per-ControlPlane client certificate (deploy/flux-system/infrastructure/openbao-ca-issuer.yaml).
	openBaoCAIssuerName = "openbao-ca-issuer"
	// dbCredentialClientCertDuration / renewBefore mirror the shared openbao TLS
	// leaves (openbao-client-tls-cert.yaml) so client-cert rotation cadences align.
	dbCredentialClientCertDuration    = "8760h"
	dbCredentialClientCertRenewBefore = "720h"
)

// dbCredentialRefreshInterval is how often ESO re-issues the engine-issued
// credential (a dynamic engine mints a fresh username+password on every read,
// not a renewal of the in-use lease). Because the materialised credential — and
// therefore the Keystone DSN — changes on every re-issue, and the DSN is
// env-var-consumed (a rotated credential only takes effect on a Pod restart —
// see reconcile_deployment.go), every refresh rolls the entire Keystone
// Deployment. The interval is therefore deliberately large (24h) so a stateless
// API is not recycled every couple of hours.
//
// It MUST stay below the engine role's default_ttl (48h, set in
// setup-database-tenant.sh) so a fresh lease is materialised before the previous
// one expires. The default_ttl − refreshInterval gap (48h − 24h = 24h) is the
// window the keystone operator has to roll the pods onto a fresh credential
// before the previous, still-in-use lease is revoked. Sizing that gap to a full
// day means a stalled rollout (bad image, resource pressure) persists long enough
// to page on-call before it can become a hard identity outage — the previous 2h
// gap could silently arm one.
const dbCredentialRefreshInterval = 24 * time.Hour

// certificateGVK is the cert-manager Certificate GroupVersionKind. The
// per-ControlPlane client Certificate is built and Owned as an
// *unstructured.Unstructured (mirroring memcachedGVK) so the c5c3 operator does
// not take a cert-manager Go dependency.
var certificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

// dbCredentialRemoteKeyFor returns the per-ControlPlane OpenBao KV path the
// stage-(a) STATIC service DB credential is read from. It is retained only for
// the Static opt-out / brownfield-migration path; the default managed mode
// reads engine-issued credentials from dbDynamicCredsPathFor instead.
func dbCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/keystone/" + cp.Namespace + "/" + cp.Name + "/db"
}

// dbDynamicRoleFor returns the per-tenant OpenBao database-engine role name for
// this ControlPlane. It is keyed on the ControlPlane NAMESPACE alone: the
// one-ControlPlane-per-namespace admission contract makes the namespace a unique
// tenant key, so no name component is needed, and cluster-unique namespaces make
// the role name collision-free (a hyphen-joined <namespace>-<name> would be
// ambiguous — e.g. ns=a-b/name=c and ns=a/name=b-c both flatten to keystone-a-b-c).
// Namespace-only keying is also what lets the keystone-db-dynamic policy scope
// reads by the caller's service_account_namespace with an EXACT match (no
// over-matching wildcard that would leak another namespace's creds path). It MUST
// stay in sync with the role-name derivation in
// deploy/openbao/bootstrap/setup-database-tenant.sh — the operator reads
// credentials from a role that script provisions.
func dbDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "keystone-" + cp.Namespace
}

// dbDynamicCredsPathFor returns the OpenBao path the VaultDynamicSecret generator
// reads short-lived credentials from (database/mariadb/creds/<role>).
func dbDynamicCredsPathFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "database/mariadb/creds/" + dbDynamicRoleFor(cp)
}

// dbCredentialSecretName returns the deterministic name of the per-ControlPlane
// DB-credential Secret/ExternalSecret (see dbCredentialSecretNameSuffix). It is
// derived from keystoneName(cp) so it tracks the projected Keystone CR.
func dbCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneName(cp) + dbCredentialSecretNameSuffix
}

// dbCredentialClientCertName returns the name of the per-ControlPlane
// cert-manager Certificate and the Secret it materialises (client mTLS keypair
// plus the CA under ca.crt).
func dbCredentialClientCertName(cp *c5c3v1alpha1.ControlPlane) string {
	return keystoneName(cp) + dbCredentialClientCertSuffix
}

// dbCredentialServiceAccount builds the per-ControlPlane ServiceAccount whose
// token the VaultDynamicSecret generator presents to OpenBao. PURE builder: the
// reconciler sets the owner reference in the CreateOrUpdate mutate closure.
func dbCredentialServiceAccount(cp *c5c3v1alpha1.ControlPlane) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: dbCredentialServiceAccountName, Namespace: childNamespace(cp)},
	}
}

// applyDBCredentialCertificateSpec sets the desired spec fields on the client
// Certificate. Extracted so the CreateOrUpdate mutate closure can re-assert the
// spec on an existing object without clobbering cert-manager-managed status.
func applyDBCredentialCertificateSpec(u *unstructured.Unstructured, cp *c5c3v1alpha1.ControlPlane) {
	name := dbCredentialClientCertName(cp)
	// SetNested* only errors on a type conflict at an existing path; on a
	// freshly-built or Certificate-typed object the writes cannot fail, so the
	// errors are intentionally ignored here.
	_ = unstructured.SetNestedField(u.Object, name, "spec", "secretName")
	_ = unstructured.SetNestedField(u.Object, dbCredentialClientCertDuration, "spec", "duration")
	_ = unstructured.SetNestedField(u.Object, dbCredentialClientCertRenewBefore, "spec", "renewBefore")
	_ = unstructured.SetNestedStringSlice(u.Object, []string{"client auth"}, "spec", "usages")
	_ = unstructured.SetNestedField(u.Object, name+"."+childNamespace(cp)+".svc", "spec", "commonName")
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"name": openBaoCAIssuerName,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
}

// dbCredentialVaultDynamicSecret builds the per-ControlPlane ESO generator that
// issues short-lived DB credentials from the OpenBao database engine. All Secret
// references are same-namespace (the generator is Namespaced, so cross-namespace
// refs are not permitted): the client cert/key and CA come from the per-CP
// Certificate's Secret, and the SA token from the per-CP ServiceAccount. server
// and mountPath are copied from the live openbao-cluster-store so the connection
// config cannot drift from the store the rest of the stack uses.
func dbCredentialVaultDynamicSecret(cp *c5c3v1alpha1.ControlPlane, server, mountPath string) *esgenv1alpha1.VaultDynamicSecret {
	certSecret := dbCredentialClientCertName(cp)
	return &esgenv1alpha1.VaultDynamicSecret{
		ObjectMeta: metav1.ObjectMeta{Name: dbCredentialSecretName(cp), Namespace: childNamespace(cp)},
		Spec: esgenv1alpha1.VaultDynamicSecretSpec{
			Path:   dbDynamicCredsPathFor(cp),
			Method: "GET",
			Provider: &esov1.VaultProvider{
				Server: server,
				// Version has no omitempty, so leaving it unset serializes as ""
				// and the ESO CRD enum (v1|v2) rejects the object — the CRD
				// default only applies to ABSENT fields. The value itself is
				// inert here: KV versioning does not affect the database-engine
				// read on spec.path, so pin the ESO default.
				Version: esov1.VaultKVStoreV2,
				Auth: &esov1.VaultAuth{
					Kubernetes: &esov1.VaultKubernetesAuth{
						Path:              mountPath,
						Role:              dbDynamicVaultRole,
						ServiceAccountRef: &esmetav1.ServiceAccountSelector{Name: dbCredentialServiceAccountName},
					},
				},
				CAProvider: &esov1.CAProvider{
					Type: esov1.CAProviderTypeSecret,
					Name: certSecret,
					Key:  "ca.crt",
				},
				ClientTLS: esov1.VaultClientTLS{
					CertSecretRef: &esmetav1.SecretKeySelector{Name: certSecret},
					KeySecretRef:  &esmetav1.SecretKeySelector{Name: certSecret},
				},
			},
		},
	}
}

// dbCredentialGeneratorExternalSecret builds the Dynamic-mode ExternalSecret that
// materialises the engine-issued username+password into childNamespace(cp) via
// the per-CP VaultDynamicSecret generator. It carries no static Data refs and no
// SecretStoreRef — the generatorRef is the sole source. RefreshInterval is below
// the engine role's default_ttl so ESO renews the lease before it expires.
func dbCredentialGeneratorExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: dbCredentialRefreshInterval},
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			DataFrom: []esov1.ExternalSecretDataFromRemoteRef{
				{SourceRef: &esov1.StoreGeneratorSourceRef{
					GeneratorRef: &esov1.GeneratorRef{
						APIVersion: "generators.external-secrets.io/v1alpha1",
						Kind:       "VaultDynamicSecret",
						Name:       name,
					},
				}},
			},
		},
	}
}

// dbCredentialStaticExternalSecret builds the stage-(a) STATIC, KV-backed
// ExternalSecret used by the Static opt-out branch (migration staging /
// brownfield). It reads username+password from the per-ControlPlane KV path via
// the openbao-cluster-store, exactly as stage (a) did.
func dbCredentialStaticExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	remoteKey := dbCredentialRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: childNamespace(cp)},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  esov1.SecretStoreRef{Kind: "ClusterSecretStore", Name: openBaoClusterStoreName},
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{SecretKey: "username", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "username"}},
				{SecretKey: "password", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"}},
			},
		},
	}
}

// dbCredentialsDynamicEnabled reports the effective credentials mode: Dynamic
// (engine-issued) is the default for managed mode; a ControlPlane opts out by
// setting spec.infrastructure.database.credentialsMode: Static (migration
// staging / brownfield).
func dbCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	return cp.Spec.Infrastructure.Database.CredentialsMode != commonv1.CredentialsModeStatic
}

// reconcileDBCredentials projects (in managed mode) the per-ControlPlane service
// DB-credential ExternalSecret and drives the DBCredentialsReady condition.
//
// Brownfield CONTROL: when the ControlPlane supplies its own database connection
// (Database.ClusterRef == nil), the user owns the DB credential Secret
// out-of-band, so the operator projects nothing and reports DBCredentialsReady
// True immediately.
//
// Managed CONTROL: the ClusterSecretStore is gated first so an ESO/OpenBao
// outage surfaces as DBCredentialsReady=False. The effective mode then decides
// the projection: Dynamic (default) provisions the ESO VaultDynamicSecret
// generator plus its ServiceAccount and mTLS client Certificate and an
// ExternalSecret that draws from the generator; Static (opt-out) projects the
// stage-(a) KV-backed ExternalSecret and tears down any dynamic-mode objects.
// Both paths converge on the WaitForExternalSecret condition flow.
func (r *ControlPlaneReconciler) reconcileDBCredentials(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Brownfield early-exit: the user supplies their own DB credential Secret, so
	// there is nothing for the operator to project or reference in OpenBao.
	if cp.Spec.Infrastructure.Database.ClusterRef == nil {
		logger.Info("brownfield database (user-supplied credential), skipping DB credential ExternalSecret projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             "BrownfieldUserSuppliedCredential",
			Message:            "brownfield database: the user supplies the DB credential Secret out-of-band; no ExternalSecret is projected",
		})
		return ctrl.Result{}, nil
	}

	// Check the OpenBao-backed ClusterSecretStore first so an ESO/OpenBao outage
	// surfaces as DBCredentialsReady=False even while a per-ExternalSecret cache
	// still reports Ready=True from its last successful sync (mirrors
	// reconcile_secrets.go in the keystone operator).
	storeReady, err := secrets.IsClusterSecretStoreReady(ctx, r.Client, openBaoClusterStoreName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("ClusterSecretStore not ready, requeuing DB credential projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
				openBaoClusterStoreName),
		})
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, nil
	}

	if dbCredentialsDynamicEnabled(cp) {
		if err := r.ensureDynamicDBCredentialObjects(ctx, cp); err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeDBCredentialsReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "GeneratorError",
				Message:            fmt.Sprintf("ensuring dynamic DB credential objects: %v", err),
			})
			return ctrl.Result{}, err
		}
	} else {
		// Static opt-out: tear down any dynamic-mode objects left from a prior
		// Dynamic deployment, then project the stage-(a) KV-backed ExternalSecret.
		r.deleteDynamicDBCredentialObjects(ctx, cp)
		desired := dbCredentialStaticExternalSecret(cp)
		es := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
			es.Spec = desired.Spec
			return controllerutil.SetControllerReference(cp, es, r.Scheme)
		}); err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeDBCredentialsReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "ExternalSecretError",
				Message:            fmt.Sprintf("ensuring DB credential ExternalSecret: %v", err),
			})
			return ctrl.Result{}, err
		}
	}

	return r.waitDBCredentialExternalSecret(ctx, cp)
}

// ensureDynamicDBCredentialObjects create-or-updates, all owner-referenced to the
// ControlPlane, the ServiceAccount, mTLS client Certificate, VaultDynamicSecret
// generator, and generator-backed ExternalSecret that materialise engine-issued
// DB credentials. Ordering (SA → Certificate → generator → ExternalSecret) makes
// the auth identity and TLS material exist before the generator that references
// them.
func (r *ControlPlaneReconciler) ensureDynamicDBCredentialObjects(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	server, mountPath := r.openBaoConnection(ctx)

	sa := dbCredentialServiceAccount(cp)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(cp, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring DB credential ServiceAccount: %w", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(certificateGVK)
	live.SetName(dbCredentialClientCertName(cp))
	live.SetNamespace(childNamespace(cp))
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		applyDBCredentialCertificateSpec(live, cp)
		return controllerutil.SetControllerReference(cp, live, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring DB credential Certificate: %w", err)
	}

	vds := dbCredentialVaultDynamicSecret(cp, server, mountPath)
	liveVDS := &esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{Name: vds.Name, Namespace: vds.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, liveVDS, func() error {
		liveVDS.Spec = vds.Spec
		return controllerutil.SetControllerReference(cp, liveVDS, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring VaultDynamicSecret generator: %w", err)
	}

	desired := dbCredentialGeneratorExternalSecret(cp)
	es := &esov1.ExternalSecret{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec = desired.Spec
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring DB credential ExternalSecret: %w", err)
	}
	return nil
}

// deleteDynamicDBCredentialObjects best-effort deletes the dynamic-mode
// ServiceAccount, client Certificate, and VaultDynamicSecret so a Dynamic→Static
// flip leaves no orphaned generator. Uses minimal-object Delete + IgnoreNotFound
// (the Get-less delete pattern); a non-NotFound error is logged rather than
// blocking the Static reconcile of a superseded object. The generator-backed
// ExternalSecret is not deleted — it is create-or-updated in place to the static
// shape (same name).
func (r *ControlPlaneReconciler) deleteDynamicDBCredentialObjects(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) {
	logger := log.FromContext(ctx)
	ns := childNamespace(cp)

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(dbCredentialClientCertName(cp))
	cert.SetNamespace(ns)

	objs := []client.Object{
		&esgenv1alpha1.VaultDynamicSecret{ObjectMeta: metav1.ObjectMeta{Name: dbCredentialSecretName(cp), Namespace: ns}},
		cert,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dbCredentialServiceAccountName, Namespace: ns}},
	}
	for _, obj := range objs {
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			logger.V(1).Info("best-effort delete of dynamic DB credential object failed",
				"kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName(), "error", err.Error())
		}
	}
}

// openBaoConnection returns the OpenBao server URL and Kubernetes-auth mount
// path, copied from the openbao-cluster-store's Vault provider so the generator
// cannot drift from the store the rest of the stack uses. Falls back to the
// documented defaults when the store or the fields are unreadable.
func (r *ControlPlaneReconciler) openBaoConnection(ctx context.Context) (server, mountPath string) {
	server = openBaoDefaultServer
	mountPath = openBaoDefaultKubernetesMount

	store := &esov1.ClusterSecretStore{}
	if err := r.Get(ctx, client.ObjectKey{Name: openBaoClusterStoreName}, store); err != nil {
		return server, mountPath
	}
	if p := store.Spec.Provider; p != nil && p.Vault != nil {
		if p.Vault.Server != "" {
			server = p.Vault.Server
		}
		if p.Vault.Auth != nil && p.Vault.Auth.Kubernetes != nil && p.Vault.Auth.Kubernetes.Path != "" {
			mountPath = p.Vault.Auth.Kubernetes.Path
		}
	}
	return server, mountPath
}

// waitDBCredentialExternalSecret mirrors the per-CP DB-credential ExternalSecret's
// Ready status into DBCredentialsReady, requeuing while ESO has not yet synced.
// Shared by the Dynamic and Static projection branches.
func (r *ControlPlaneReconciler) waitDBCredentialExternalSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	exists, ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: dbCredentialSecretName(cp)})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("checking DB credential ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		// The reconciler ensures this ExternalSecret just above, so exists is
		// almost always true here; a false value indicates a transient cache
		// lag and is surfaced in the log rather than the user-facing status.
		logger.Info("DB credential ExternalSecret not ready, requeuing", "exists", exists)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForDBCredentialSecret",
			Message:            "DB credential ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: dbCredentialsRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeDBCredentialsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "DBCredentialsReady",
		Message:            "DB credential ExternalSecret is Ready",
	})
	return ctrl.Result{}, nil
}
