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
	// OpenBao. A fixed name is safe because a namespace belongs to at most one
	// ControlPlane: the one-ControlPlane-per-namespace webhook guarantees it for the
	// ControlPlane's own namespace, and the namespace-claim webhook guarantees it
	// for every service namespace.
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
//
// It is keyed on the KEYSTONE service namespace, not the ControlPlane's: the
// ExternalSecret that reads it is materialised beside the Keystone child and
// routes through THAT namespace's tenant store, whose templated OpenBao policy
// only grants paths under its own namespace.
//
// NOTHING SEEDS THIS PATH. The per-ControlPlane static seed was retired when
// managed mode moved to engine-issued credentials
// (deploy/openbao/bootstrap/write-bootstrap-secrets.sh), so a ControlPlane on the
// Static branch — the explicit opt-out on the shared database, and every DEDICATED
// managed database, which is Static-only — must have the path seeded out-of-band
// (username, password) before ESO can sync the credential. Until then the
// ExternalSecret cannot go Ready; dbCredentialNotReadyMessage names the path in the
// condition so the requirement is visible from `kubectl describe controlplane`
// rather than only in the migration guide.
func dbCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "openstack/keystone/" + cp.KeystoneNamespace() + "/" + cp.Name + "/db"
}

// dbCredentialNotReadyMessage explains an unsynced DB-credential ExternalSecret.
//
// In Dynamic mode ESO mints the credential from the OpenBao database engine, so
// an unready ExternalSecret means no more than "ESO has not synced yet".
//
// In Static mode the ExternalSecret READS a KV path nothing seeds (see
// dbCredentialRemoteKeyFor), so an unready ExternalSecret is, in the common case,
// a missing manual seed rather than a transient sync — and it is reached without
// the user asking for it, because a dedicated managed database is materialized
// onto Static on their behalf. The message names the exact path so the condition
// tells the operator what to do instead of leaving them to infer it.
func dbCredentialNotReadyMessage(cp *c5c3v1alpha1.ControlPlane) string {
	if dbCredentialsDynamicEnabled(cp) {
		return "DB credential ExternalSecret is not yet Ready"
	}
	return fmt.Sprintf("DB credential ExternalSecret is not yet Ready: credentialsMode Static reads the "+
		"credential from OpenBao KV key %q, which neither the operator nor the bootstrap seeds; seed it "+
		"(username, password) out-of-band before this ControlPlane can reach Ready",
		dbCredentialRemoteKeyFor(cp))
}

// dbDynamicRoleFor returns the per-tenant OpenBao database-engine role name for
// this ControlPlane. It is keyed on the KEYSTONE SERVICE NAMESPACE alone — the
// namespace the database and the generator that reads from it actually live in.
// A namespace is a unique tenant key (at most one ControlPlane occupies it), so
// no name component is needed, and cluster-unique namespaces make the role name
// collision-free (a hyphen-joined <namespace>-<name> would be ambiguous — e.g.
// ns=a-b/name=c and ns=a/name=b-c both flatten to keystone-a-b-c).
// Namespace-only keying is also what lets the keystone-db-dynamic policy scope
// reads by the caller's service_account_namespace with an EXACT match (no
// over-matching wildcard that would leak another namespace's creds path) — and
// that caller is the generator's ServiceAccount in the Keystone namespace, so
// keying on the ControlPlane's namespace instead would put the role outside the
// policy's reach. It MUST stay in sync with the role-name derivation in
// deploy/openbao/bootstrap/setup-database-tenant.sh — the operator reads
// credentials from a role that script provisions.
func dbDynamicRoleFor(cp *c5c3v1alpha1.ControlPlane) string {
	return "keystone-" + cp.KeystoneNamespace()
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
		ObjectMeta: metav1.ObjectMeta{Name: dbCredentialServiceAccountName, Namespace: cp.KeystoneNamespace()},
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
	_ = unstructured.SetNestedField(u.Object, name+"."+cp.KeystoneNamespace()+".svc", "spec", "commonName")
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
		ObjectMeta: metav1.ObjectMeta{Name: dbCredentialSecretName(cp), Namespace: cp.KeystoneNamespace()},
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
// materialises the engine-issued username+password into the KEYSTONE service
// namespace (beside the child that consumes it) via the per-CP VaultDynamicSecret
// generator. It carries no static Data refs and no
// SecretStoreRef — the generatorRef is the sole source. RefreshInterval is below
// the engine role's default_ttl so ESO renews the lease before it expires.
func dbCredentialGeneratorExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cp.KeystoneNamespace()},
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
// the store the ControlPlane selected (default: the shared openbao-cluster-store).
func dbCredentialStaticExternalSecret(cp *c5c3v1alpha1.ControlPlane) *esov1.ExternalSecret {
	name := dbCredentialSecretName(cp)
	remoteKey := dbCredentialRemoteKeyFor(cp)
	return &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cp.KeystoneNamespace()},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  secrets.ESOSecretStoreRef(effectiveControlPlaneStoreRef(cp)),
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{SecretKey: "username", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "username"}},
				{SecretKey: "password", RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: "password"}},
			},
		},
	}
}

// dbCredentialsDynamicEnabled reports the effective credentials mode of the
// database Keystone actually connects to: Dynamic (engine-issued) is the default
// for a managed SHARED database; a ControlPlane opts out by setting
// credentialsMode: Static (migration staging / brownfield).
//
// A DEDICATED database is never Dynamic. The OpenBao database engine carries one
// connection and one role per NAMESPACE (deploy/openbao/bootstrap/
// setup-database-tenant.sh), bootstrapped against the SHARED cluster, so no
// engine role exists that could issue credentials for a dedicated instance — it
// takes the Static branch, the same documented contract the shared block's own
// Static opt-out carries. The validating webhook rejects an explicit Dynamic
// there; keying the decision on the dedicated declaration rather than only on the
// stored mode keeps a webhook-bypassed CR failing closed onto Static rather than
// projecting a generator that could never sync.
func dbCredentialsDynamicEnabled(cp *c5c3v1alpha1.ControlPlane) bool {
	if cp.DedicatedKeystoneDatabase() != nil {
		return false
	}
	// The effective database is nil for an External-mode (or webhook-bypassed) CR,
	// and brownfield when it carries no ClusterRef: neither has a managed database
	// to issue credentials for.
	db := effectiveKeystoneDatabase(cp)
	return db != nil && db.ClusterRef != nil && db.CredentialsMode != commonv1.CredentialsModeStatic
}

// reconcileDBCredentials projects (in managed mode) the per-ControlPlane service
// DB-credential ExternalSecret and drives the DBCredentialsReady condition.
//
// External CONTROL: the ControlPlane has no database at all — no managed one to
// issue credentials for, and no brownfield connection to reference. Neither
// OpenBao nor the ClusterSecretStore is consulted; DBCredentialsReady is True
// with the dedicated ExternallyManaged reason.
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

	// External-mode short-circuit, keyed on the MODE discriminator rather than on
	// the database shape: an External-mode ControlPlane has no infrastructure
	// block, so "no managed database" and "no database at all" are different
	// states that must not collapse onto the brownfield reason below.
	if cp.IsExternalKeystone() {
		logger.Info("External keystone mode; no database is managed, skipping DB credential projection",
			"authURL", externalKeystoneAuthURL(cp))
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cp.Generation,
			Reason:             conditionReasonExternallyManaged,
			Message: fmt.Sprintf("External keystone mode: the ControlPlane manages no database "+
				"(external Keystone at %s); no DB-credential ExternalSecret is projected",
				externalKeystoneAuthURL(cp)),
		})
		return ctrl.Result{}, nil
	}

	// Brownfield early-exit: the user supplies their own DB credential Secret, so
	// there is nothing for the operator to project or reference in OpenBao. The
	// decision is made on the EFFECTIVE database — Keystone's dedicated one when it
	// opted in, the shared one otherwise — so a service on a brownfield dedicated
	// database gets the same user-owned-credential contract a brownfield shared one
	// does. A nil effective database on a non-External CR is a webhook-bypass shape;
	// treat it as brownfield rather than dereferencing it.
	if db := effectiveKeystoneDatabase(cp); db == nil || db.ClusterRef == nil {
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

	// Check the selected secret store first so an ESO/OpenBao outage surfaces as
	// DBCredentialsReady=False even while a per-ExternalSecret cache still reports
	// Ready=True from its last successful sync (mirrors reconcile_secrets.go in
	// the keystone operator). The store is the one the ControlPlane selected via
	// spec.secretStoreRef (default: the operator-provisioned per-tenant store); a
	// namespaced store is resolved in the KEYSTONE service namespace, where the
	// credential Secret is materialised and that namespace's own tenant store lives.
	storeRef := effectiveControlPlaneStoreRef(cp)
	storeReady, err := secrets.IsStoreRefReady(ctx, r.Client, storeRef, cp.KeystoneNamespace())
	if err != nil {
		return ctrl.Result{}, err
	}
	if !storeReady {
		logger.Info("secret store not ready, requeuing DB credential projection")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDBCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretStoreNotReady",
			Message: fmt.Sprintf("%s %q is not ready; upstream secret backend unreachable",
				storeRef.Kind, storeRef.Name),
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
		// Static: tear down any dynamic-mode objects left from a prior Dynamic
		// deployment, then project the stage-(a) KV-backed ExternalSecret. It reads
		// a KV path nothing seeds (see dbCredentialRemoteKeyFor), so it only syncs
		// once that path has been seeded out-of-band — dbCredentialNotReadyMessage
		// says so in the condition while it has not.
		r.deleteDynamicDBCredentialObjects(ctx, cp)
		if err := r.ensureUnownedOrOwned(ctx, cp, dbCredentialStaticExternalSecret(cp)); err != nil {
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

// ensureDynamicDBCredentialObjects create-or-updates the ServiceAccount, mTLS
// client Certificate, VaultDynamicSecret generator, and generator-backed
// ExternalSecret that materialise engine-issued DB credentials — all in the
// KEYSTONE service namespace, beside the database they issue against and the child
// that consumes them. Ordering (SA → Certificate → generator → ExternalSecret)
// makes the auth identity and TLS material exist before the generator that
// references them. In the ControlPlane's own namespace they are owner-referenced;
// in a service namespace they carry the ownership labels instead.
func (r *ControlPlaneReconciler) ensureDynamicDBCredentialObjects(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	server, mountPath := r.openBaoConnection(ctx, cp, effectiveControlPlaneStoreRef(cp))

	if err := r.ensureUnownedOrOwned(ctx, cp, dbCredentialServiceAccount(cp)); err != nil {
		return fmt.Errorf("ensuring DB credential ServiceAccount: %w", err)
	}

	// The cert-manager Certificate is handled as an unstructured object (no Go
	// module ships its type), and apply.EnsureObject converts typed structs, not
	// unstructured inputs — so this projection stays read-modify-write while the
	// typed sibling objects around it use Server-Side Apply.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(certificateGVK)
	live.SetName(dbCredentialClientCertName(cp))
	live.SetNamespace(cp.KeystoneNamespace())
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		if err := refuseForeignAdoption(cp, live, r.Scheme); err != nil {
			return err
		}
		applyDBCredentialCertificateSpec(live, cp)
		return claimChildOwnership(cp, live, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring DB credential Certificate: %w", err)
	}

	if err := r.ensureUnownedOrOwned(ctx, cp, dbCredentialVaultDynamicSecret(cp, server, mountPath)); err != nil {
		return fmt.Errorf("ensuring VaultDynamicSecret generator: %w", err)
	}

	if err := r.ensureUnownedOrOwned(ctx, cp, dbCredentialGeneratorExternalSecret(cp)); err != nil {
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
	ns := cp.KeystoneNamespace()

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
// path, copied from the Vault provider of the given store ref (a cluster-scoped
// ClusterSecretStore by name, or a namespaced SecretStore in the child
// namespace) so the caller cannot drift from the store the rest of the stack
// uses. Callers pass the store to resolve against explicitly: the DB-credential
// generator resolves against the control plane's effective store, whereas the
// per-tenant-store sub-reconciler resolves against the SHARED cluster store (the
// tenant store cannot describe its own bootstrap). Falls back to the documented
// defaults when the store or the fields are unreadable.
func (r *ControlPlaneReconciler) openBaoConnection(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ref commonv1.SecretStoreRefSpec) (server, mountPath string) {
	server = openBaoDefaultServer
	mountPath = openBaoDefaultKubernetesMount

	var provider *esov1.SecretStoreProvider
	if ref.Kind == commonv1.SecretStoreKindNamespaced {
		store := &esov1.SecretStore{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: cp.Namespace}, store); err != nil {
			return server, mountPath
		}
		provider = store.Spec.Provider
	} else {
		// Callers pass an already-resolved ref (effectiveControlPlaneStoreRef /
		// secrets.EffectiveStoreRef(nil)), so a non-namespaced kind resolves
		// through the cluster-scoped store by name.
		store := &esov1.ClusterSecretStore{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name}, store); err != nil {
			return server, mountPath
		}
		provider = store.Spec.Provider
	}

	if provider != nil && provider.Vault != nil {
		if provider.Vault.Server != "" {
			server = provider.Vault.Server
		}
		if provider.Vault.Auth != nil && provider.Vault.Auth.Kubernetes != nil && provider.Vault.Auth.Kubernetes.Path != "" {
			mountPath = provider.Vault.Auth.Kubernetes.Path
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
		types.NamespacedName{Namespace: cp.KeystoneNamespace(), Name: dbCredentialSecretName(cp)})
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
			Message:            dbCredentialNotReadyMessage(cp),
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
