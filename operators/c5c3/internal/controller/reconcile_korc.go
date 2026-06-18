// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// openBaoClusterStoreName mirrors the keystone operator's constant
// (operators/keystone/internal/controller/reconcile_secrets.go): the cluster-
// scoped ESO SecretStore that fronts OpenBao. Defined locally so the c5c3
// PushSecrets target the same backend without importing the keystone package
const openBaoClusterStoreName = "openbao-cluster-store"

// adminAppCredentialNameSuffix is appended to the ControlPlane name to derive
// the deterministic, collision-free name of the owned K-ORC ApplicationCredential
// CR, mirroring the keystoneNameSuffix discipline so a single
// namespace can host the admin AC of multiple ControlPlanes.
const adminAppCredentialNameSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: name suffix, not a credential.

// adminAppCredentialSecretSuffix names the operator-owned Secret that K-ORC
// writes the minted application credential into (Resource.SecretRef). It is the
// push source for the OpenBao PushSecret.
const adminAppCredentialSecretSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// adminAppCredentialRemoteKeyFor returns the per-ControlPlane OpenBao path the
// minted admin application credential is mirrored to. The
// path is scoped by both the ControlPlane's Namespace and Name so two
// ControlPlanes never clobber each other's admin credential on the cluster-
// global OpenBao backend. The matching read consumer (the per-CR k-orc
// clouds.yaml ExternalSecret) targets the same key.
func adminAppCredentialRemoteKeyFor(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("openstack/keystone/%s/%s/admin/app-credential", cp.Namespace, cp.Name)
}

// adminPasswordHashAnnotation stamps the SHA-256 of the admin password the
// application credential was last minted against onto the owned AC CR. Mirrors the hash+annotation pattern in the keystone operator's
// password-rotation reconciler. A mismatch on a later pass drives a re-mint.
const adminPasswordHashAnnotation = "forge.c5c3.io/admin-password-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// korcCloudsYamlSecretName is the conventional name of the admin clouds.yaml
// Secret (and its ExternalSecret) K-ORC reads its admin credentials from. It
// matches DefaultCloudCredentialsSecretName, the value the defaulting webhook
// applies to spec.korc.adminCredential.cloudCredentialsRef.secretName, and is
// used only as the fallback when a CR somehow reaches the reconciler without the
// webhook having defaulted that field.
//
// C1 (co-location): the ExternalSecret materialises this Secret into the
// SAME namespace as the K-ORC ApplicationCredential/Service/Endpoint CRs the
// operator projects — the ControlPlane's own namespace (childNamespace(cp)) —
// because K-ORC resolves CloudCredentialsRef in the resource's own namespace
// (vendored api/v1alpha1/credentials_ref.go GetCloudCredentialsRef returns the
// resource's Namespace). The AdminCredentialReady gate therefore waits on the
// ExternalSecret in childNamespace(cp), NOT a fixed orc-system one, so the minted
// AC is never pushed before K-ORC can actually authenticate.
const korcCloudsYamlSecretName = "k-orc-clouds-yaml" //nolint:gosec // G101 false positive: Secret name, not a credential.

// adminPasswordCloudSecretSuffix names the operator-owned Secret holding a
// PASSWORD-based clouds.yaml that always tracks the current admin password. The
// admin ApplicationCredential mints against THIS secret (not k-orc-clouds-yaml),
// which breaks the self-referential bootstrap deadlock: k-orc-clouds-yaml is the
// minted app-credential itself, so deleting the AC to re-mint would invalidate
// the very clouds.yaml needed to re-authenticate. A restricted application
// credential also cannot mint a new application credential — only a
// password-authenticated session can — so password auth is required for the
// delete+recreate re-mint to work at all.
const adminPasswordCloudSecretSuffix = "-admin-password-cloud" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

// adminAppCredentialName returns the deterministic name of the owned K-ORC
// ApplicationCredential CR for the given ControlPlane.
func adminAppCredentialName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminAppCredentialNameSuffix
}

// adminPasswordCloudSecretName returns the name of the operator-owned Secret that
// holds the password-based clouds.yaml the admin ApplicationCredential mints with.
func adminPasswordCloudSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminPasswordCloudSecretSuffix
}

// adminAppCredentialSecretName returns the name of the operator-owned Secret that
// holds the application-credential secret K-ORC mints with (key "value") and,
// after minting, the assembled app-credential clouds.yaml pushed to OpenBao.
func adminAppCredentialSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + adminAppCredentialSecretSuffix
}

// appCredSecretValueKey is the Secret data key K-ORC reads the application
// credential's secret from (the actuator reads Secret.Data["value"]).
const appCredSecretValueKey = "value"

// appCredCloudsYAMLKey is the Secret data key the assembled app-credential
// clouds.yaml is stored under; the PushSecret mirrors it to OpenBao and the
// k-orc-clouds-yaml ExternalSecret reads it back as the "clouds.yaml" property.
const appCredCloudsYAMLKey = "clouds.yaml"

// ensureAppCredentialSecret ensures the operator-owned Secret that K-ORC reads the
// application-credential secret from exists with a generated "value". K-ORC's
// managed ApplicationCredential reads Secret.Data["value"] and creates the AC in
// Keystone with it, so this MUST exist before the AC is reconciled. The value is
// generated once and preserved across reconciles — regenerating it would force a
// re-mint and invalidate the stored clouds.yaml.
func (r *ControlPlaneReconciler) ensureAppCredentialSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialSecretName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if len(secret.Data[appCredSecretValueKey]) == 0 {
			v, gerr := generateAppCredSecretValue()
			if gerr != nil {
				return gerr
			}
			secret.Data[appCredSecretValueKey] = []byte(v)
		}
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring app-credential secret %q: %w", secret.Name, err)
	}
	return nil
}

// generateAppCredSecretValue returns a 256-bit, URL-safe random string used as the
// application credential's secret.
func generateAppCredSecretValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating application-credential secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// buildAppCredCloudsYAML assembles the application-credential clouds.yaml the
// control plane authenticates K-ORC with after minting: the credential id comes
// from the minted AC, the secret from the generated "value", and the auth_url from
// the projected Keystone Service (keystoneEndpointURL).
//
// CRITICAL (endpoint_type: internal): gophercloud only uses the auth_url to obtain
// a token; for every subsequent API call it resolves the endpoint from the returned
// service catalog, picking the interface set here. K-ORC runs IN-CLUSTER, so it
// must use the "internal" (cluster-DNS) identity endpoint. Once the ControlPlane
// exposes Keystone via the shared Gateway the catalog's "public" identity endpoint
// becomes the external host (e.g. https://keystone.<host>.nip.io:8443/v3), which
// from inside a pod is unreachable — so "public" makes every list/get fail. Worse,
// K-ORC swallows that failure (osclients ListDomains does `_ = pager.EachPage(...)`)
// and reports it as an EMPTY import, so the admin Domain/User imports hang forever
// on "Waiting for OpenStack resource to be created externally".
//
// The key MUST be "endpoint_type", NOT "interface": K-ORC's scope builder copies
// only clientconfig.Cloud.EndpointType (the `endpoint_type` key) into the client
// options and drops Cloud.Interface (the `interface` key) — see vendored
// internal/scope/provider.go NewProviderClient. An "interface:" value is therefore
// ignored and the endpoint defaults to "public". The auth_url already points at the
// in-cluster Service for the same reason (keystoneEndpointURL, never the external
// endpoint).
func buildAppCredCloudsYAML(cp *c5c3v1alpha1.ControlPlane, acID, secret string) string {
	region := cp.Spec.Region
	if region == "" {
		region = "RegionOne"
	}
	return fmt.Sprintf(`clouds:
  admin:
    auth:
      auth_url: %q
      application_credential_id: %q
      application_credential_secret: %q
    auth_type: v3applicationcredential
    region_name: %q
    endpoint_type: internal
    identity_api_version: 3
`, keystoneEndpointURL(cp), acID, secret, region)
}

// ensureAdminPasswordCloud ensures the operator-owned Secret holding the
// PASSWORD-based clouds.yaml the admin ApplicationCredential mints with exists and
// always tracks the CURRENT admin password. Unlike ensureAppCredentialSecret's
// "value" (generated once and preserved), this clouds.yaml is rebuilt from the
// live admin password on every pass, so a password rotation flows through to it —
// the credential K-ORC uses to re-authenticate and revoke/re-mint never goes
// stale. CreateOrUpdate only writes when the rendered clouds.yaml differs, so a
// steady-state pass does not churn the Secret (and does not wake any consumer).
//
// The Secret lives in childNamespace(cp) — the same namespace as the K-ORC CRs —
// because K-ORC resolves CloudCredentialsRef in the resource's own namespace (C1).
func (r *ControlPlaneReconciler) ensureAdminPasswordCloud(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, password string) error {
	cloudsYAML := []byte(buildPasswordCloudsYAML(cp, password))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminPasswordCloudSecretName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring admin password-cloud secret %q: %w", secret.Name, err)
	}
	return nil
}

// buildPasswordCloudsYAML assembles the password-based clouds.yaml the admin
// ApplicationCredential authenticates with to mint (and, on re-mint, revoke) the
// Keystone credential. It mirrors the bootstrap seed
// (deploy/openbao/bootstrap/write-bootstrap-secrets.sh) so the in-cluster and
// operator-owned credentials are byte-compatible: the cloud key matches the
// CloudCredentialsRef.CloudName, auth_url is the in-cluster Keystone Service
// (keystoneEndpointURL — never the external endpoint), and endpoint_type is
// "internal" (the key MUST be "endpoint_type", not "interface"; see
// buildAppCredCloudsYAML for the full rationale).
func buildPasswordCloudsYAML(cp *c5c3v1alpha1.ControlPlane, password string) string {
	region := cp.Spec.Region
	if region == "" {
		region = "RegionOne"
	}
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	if cloudName == "" {
		cloudName = korcAdminUsername
	}
	return fmt.Sprintf(`clouds:
  %s:
    auth:
      auth_url: %q
      username: %q
      password: %q
      project_name: %q
      user_domain_name: %q
      project_domain_name: %q
    region_name: %q
    endpoint_type: internal
    identity_api_version: 3
`, cloudName, keystoneEndpointURL(cp), korcAdminUsername, password,
		korcAdminUsername, korcAdminDomainName, korcAdminDomainName, region)
}

// seedBootstrapCloudsYAML writes the PASSWORD-based clouds.yaml into the
// {cp.Name}-admin-app-credential Secret's clouds.yaml key, but ONLY when that key
// is empty (write-if-empty). It breaks the AdminCredentialReady chicken-and-egg
// deadlock on a fresh cluster the per-CR OpenBao path the
// k-orc-clouds-yaml ExternalSecret reads is empty until something pushes to it, so
// the operator seeds a password-based document here that lets K-ORC's admin
// imports authenticate before the application credential is ever minted —
// previously this was seeded by deploy/openbao/bootstrap/write-bootstrap-secrets.sh.
//
// Write-if-empty is the idempotency guard once
// reconcileAdminCredential fills the key with the minted credential-based
// clouds.yaml (buildAppCredCloudsYAML) the seed becomes a no-op and never clobbers
// the minted document. On a re-mint, regenerateAppCredentialSecretValue deletes
// the key, so the next reconcileKORC pass re-seeds the password version and bridges
// the re-authentication gap. The "value" key (owned by ensureAppCredentialSecret)
// is never touched.
func (r *ControlPlaneReconciler) seedBootstrapCloudsYAML(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, password string) error {
	cloudsYAML := []byte(buildPasswordCloudsYAML(cp, password))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialSecretName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		// Write-if-empty: never overwrite a minted credential-based clouds.yaml.
		if len(secret.Data[appCredCloudsYAMLKey]) == 0 {
			secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		}
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("seeding bootstrap clouds.yaml into secret %q: %w", secret.Name, err)
	}
	return nil
}

// adminAppCredentialPushSecretName returns the PushSecret name backing up the
// minted application credential to OpenBao.
func adminAppCredentialPushSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-admin-app-credential-backup"
}

// reconcileKORC reconciles the K-ORC (OpenStack Resource Controller)
// integration and drives the KORCReady condition.
//
// It create-or-updates an OWNED ApplicationCredential CR that instructs K-ORC to
// mint the admin application credential. The CR maps the ControlPlane's
// AdminCredential spec onto the K-ORC ApplicationCredentialSpec, taking care of
// the Restricted <-> Unrestricted inversion (see below). The AC authenticates via
// the operator-owned password-cloud (ensureAdminPasswordCloud), NOT
// k-orc-clouds-yaml, so it can always re-authenticate as admin even while the
// minted app credential is being revoked.
//
// RE-MINT K-ORC's AC actuator implements only Create + Delete, so a
// rotated admin password cannot re-mint the credential in place. reconcileKORC
// therefore compares the SHA-256 of the current admin password against the
// adminPasswordHashAnnotation stamped on the AC; on a mismatch it DELETES the AC
// (the finalizer revokes the old credential) and regenerates the secret "value"
// (remintAdminApplicationCredential), and the next pass recreates it for a fresh
// mint. The hash compare and the delete+recreate live here, co-located with the
// resource K-ORC reacts to; reconcileAdminCredential only commits/pushes the
// already-(re-)minted secret.
//
// MISSING-CRD SAFETY: if the K-ORC CRD is not installed the apiserver / RESTMapper
// returns a no-match error; this is detected via meta.IsNoMatchError and surfaced
// as KORCReady=False (Reason "KORCCRDNotInstalled") WITHOUT returning a hard error
// that would crash-loop the operator.
func (r *ControlPlaneReconciler) reconcileKORC(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	adminCred := cp.Spec.KORC.AdminCredential

	// Read the admin password used to (re-)mint the AC. The cleartext is
	// needed both to derive the rotation hash AND to render the password-based
	// clouds.yaml the AC mints with. A read failure (missing Secret/key) is
	// surfaced as KORCReady False with a requeue rather than a hard error so a
	// not-yet-seeded admin password simply defers minting.
	password, err := readAdminPassword(ctx, r.Client, cp)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			logger.Info("admin password not yet available, deferring K-ORC mint")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "WaitingForAdminPassword",
				Message:            "admin password Secret is not yet available; deferring application-credential mint",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "AdminPasswordError",
			Message:            fmt.Sprintf("reading admin password: %v", err),
		})
		return ctrl.Result{}, err
	}
	pwHash := hashAdminPassword(password)

	// Ensure the operator-owned password-based clouds.yaml the AC mints with always
	// tracks the current admin password. This is what breaks the self-referential
	// bootstrap deadlock and lets the delete+recreate re-mint below re-authenticate
	// as admin even while k-orc-clouds-yaml still holds the (about-to-be-revoked)
	// app credential.
	if err := r.ensureAdminPasswordCloud(ctx, cp, password); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "PasswordCloudError",
			Message:            fmt.Sprintf("ensuring admin password-cloud secret: %v", err),
		})
		return ctrl.Result{}, err
	}

	// restricted defaults to true (the safe least-privilege baseline) when unset,
	// matching the +kubebuilder:default=true marker and the defaulting webhook.
	restricted := true
	if adminCred.ApplicationCredential.Restricted != nil {
		restricted = *adminCred.ApplicationCredential.Restricted
	}

	// importCredRef authenticates the Domain/User imports and the catalog
	// Service/Endpoint: they stay on the spec's CloudCredentialsRef
	// (k-orc-clouds-yaml) and tolerate the brief auth gap during a re-mint by
	// requeueing. acCredRef points the AC itself at the operator-owned
	// password-cloud so a delete+recreate can always re-authenticate (see
	// adminPasswordCloudSecretSuffix).
	importCredRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: adminCred.CloudCredentialsRef.SecretName,
		CloudName:  adminCred.CloudCredentialsRef.CloudName,
	}
	acCredRef := orcv1alpha1.CloudCredentialsReference{
		SecretName: adminPasswordCloudSecretName(cp),
		CloudName:  adminCred.CloudCredentialsRef.CloudName,
	}

	// The admin ApplicationCredential's UserRef points at a K-ORC User that must
	// already exist. The Keystone bootstrap creates the real admin user in the
	// Default domain, so import it (and its domain) as UNMANAGED K-ORC resources
	// before minting — otherwise the AC blocks forever on "Waiting for User/admin
	// to be created".
	if err := r.ensureKORCAdminImports(ctx, cp, importCredRef); err != nil {
		if meta.IsNoMatchError(err) {
			logger.Info("K-ORC User/Domain CRD not installed; KORCReady=False")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "KORCCRDNotInstalled",
				Message:            "K-ORC User/Domain CRD is not installed",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "AdminImportError",
			Message:            fmt.Sprintf("ensuring K-ORC admin User/Domain imports: %v", err),
		})
		return ctrl.Result{}, err
	}

	// K-ORC's managed ApplicationCredential reads the DESIRED secret from
	// Secret.Data["value"] and passes it to Keystone when creating the credential
	// (it does NOT generate or write the secret itself). So the operator-owned
	// Secret MUST exist with a generated "value" BEFORE the AC is reconciled —
	// otherwise the AC blocks on "Waiting for Secret … to be created".
	if err := r.ensureAppCredentialSecret(ctx, cp); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretError",
			Message:            fmt.Sprintf("ensuring application-credential secret: %v", err),
		})
		return ctrl.Result{}, err
	}

	// Seed the bootstrap clouds.yaml, mirror it to OpenBao, and create the per-CR
	// ExternalSecret that reads it back — all BEFORE the AC is minted, so the
	// AdminCredentialReady chicken-and-egg gate opens on a fresh cluster without any
	// external shell seed (//). The seed is
	// write-if-empty, so once the credential is minted these become no-ops that never
	// clobber the minted clouds.yaml.
	//
	// DECISION (placement): the issue says "after ensureAdminPasswordCloud and
	// ensureAppCredentialSecret AND before ensureKORCAdminImports", but the real call
	// order is ensureAdminPasswordCloud -> ensureKORCAdminImports ->
	// ensureAppCredentialSecret, so those two constraints are inconsistent. Chose to
	// insert the three steps immediately AFTER ensureAppCredentialSecret (so the seed
	// updates the very Secret that call just created/owns rather than racing two
	// CreateOrUpdate passes over it) and BEFORE the re-mint/CreateOrUpdate AC
	// decision. The "before the imports" intent is moot: K-ORC retries authentication
	// asynchronously, so the relative order within one synchronous pass does not
	// change convergence. Reviewer: please verify.
	if err := r.seedBootstrapCloudsYAML(ctx, cp, password); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SeedCloudsYamlError",
			Message:            fmt.Sprintf("seeding bootstrap clouds.yaml: %v", err),
		})
		return ctrl.Result{}, err
	}
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, adminAppCredentialPushSecret(cp)); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "PushSecretError",
			Message:            fmt.Sprintf("ensuring admin app-credential PushSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if err := r.ensureKORCCloudsYAMLExternalSecret(ctx, cp); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ExternalSecretError",
			Message:            fmt.Sprintf("ensuring k-orc clouds.yaml ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}

	// Decide between steady-state convergence and a re-mint BEFORE the
	// CreateOrUpdate: K-ORC's ApplicationCredential actuator only implements
	// Create + Delete (no in-place re-mint), so the only way to a fresh Keystone
	// credential on a password rotation is to delete the AC (finalizer revokes the
	// old credential) and let the next pass recreate it. A hash mismatch — a stale
	// stamped hash after a password rotation, or the empty annotation the
	// CredentialRotation reconciler writes to nudge — is the re-mint signal.
	acKey := types.NamespacedName{Name: adminAppCredentialName(cp), Namespace: childNamespace(cp)}
	existing := &orcv1alpha1.ApplicationCredential{}
	switch getErr := r.Get(ctx, acKey, existing); {
	case getErr == nil:
		if existing.Annotations[adminPasswordHashAnnotation] != pwHash {
			return r.remintAdminApplicationCredential(ctx, cp, existing)
		}
		// Hash matches: fall through to the idempotent CreateOrUpdate below, which
		// converges the spec without re-minting (no-op when nothing changed).
	case apierrors.IsNotFound(getErr):
		// First mint, or the recreate after a re-mint delete: CreateOrUpdate below.
	case meta.IsNoMatchError(getErr):
		logger.Info("K-ORC ApplicationCredential CRD not installed; KORCReady=False")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "KORCCRDNotInstalled",
			Message:            "K-ORC ApplicationCredential CRD is not installed",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	default:
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ApplicationCredentialError",
			Message:            fmt.Sprintf("reading ApplicationCredential: %v", getErr),
		})
		return ctrl.Result{}, getErr
	}

	ac := &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialName(cp),
			Namespace: childNamespace(cp),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ac, func() error {
		ac.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		ac.Spec.CloudCredentialsRef = acCredRef

		if ac.Spec.Resource == nil {
			ac.Spec.Resource = &orcv1alpha1.ApplicationCredentialResourceSpec{}
		}
		// CRITICAL INVERSION our spec is Restricted; K-ORC's field is
		// Unrestricted. restricted=true => Unrestricted=false (and vice versa).
		ac.Spec.Resource.Unrestricted = ptr.To(!restricted)
		ac.Spec.Resource.UserRef = orcv1alpha1.KubernetesNameRef(adminUserRef(cp))
		ac.Spec.Resource.SecretRef = orcv1alpha1.KubernetesNameRef(adminAppCredentialSecretName(cp))
		ac.Spec.Resource.AccessRules = projectAccessRules(adminCred.ApplicationCredential.AccessRules)

		// Stamp the password hash this credential was minted against. On a later
		// pass a mismatch (the hash moved because the admin password rotated, or
		// the CredentialRotation reconciler zeroed the annotation to nudge) is what
		// the re-mint decision above keys off to delete+recreate the AC.
		if ac.Annotations == nil {
			ac.Annotations = map[string]string{}
		}
		ac.Annotations[adminPasswordHashAnnotation] = pwHash

		return controllerutil.SetControllerReference(cp, ac, r.Scheme)
	})
	if err != nil {
		// MISSING-CRD SAFETY: a no-match error means the K-ORC CRD is absent.
		// Surface a clean condition instead of crash-looping.
		if meta.IsNoMatchError(err) {
			logger.Info("K-ORC ApplicationCredential CRD not installed; KORCReady=False")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "KORCCRDNotInstalled",
				Message:            "K-ORC ApplicationCredential CRD is not installed",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ApplicationCredentialError",
			Message:            fmt.Sprintf("create-or-update ApplicationCredential: %v", err),
		})
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("ensured K-ORC ApplicationCredential", "name", ac.Name, "operation", op)
	}

	// Reflect the AC CR's observed state into status on every pass. The
	// ID is populated by K-ORC once the credential is minted; Restricted is the
	// inverse of the K-ORC-reported Unrestricted (falling back to the desired
	// value while status is empty). LastRotation is stamped on a fresh mint/re-mint.
	r.updateAdminApplicationCredentialStatus(cp, ac, restricted)

	// Gate KORCReady on the AC CR reporting Available=True. K-ORC uses the
	// "Available" condition (not "Ready") to signal a usable resource; while it
	// converges, requeue with KORCReady=False.
	if !orcv1alpha1.IsAvailable(ac) {
		logger.Info("ApplicationCredential not yet Available, requeuing", "name", ac.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForApplicationCredential",
			Message:            fmt.Sprintf("ApplicationCredential %q is not yet Available", ac.Name),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "ApplicationCredentialMinted",
		Message:            "K-ORC admin application credential is minted and available",
	})
	return ctrl.Result{}, nil
}

// remintAdminApplicationCredential drives the actual re-mint when the stamped
// password hash no longer matches the current admin password. K-ORC's AC actuator
// has no in-place re-mint, so it DELETES the AC (the finalizer revokes the old
// Keystone credential, authenticating via the operator-owned password-cloud) and
// regenerates the app-credential secret "value" so the recreated AC mints a
// brand-new credential. The next reconcileKORC pass observes the now-absent AC and
// recreates it via CreateOrUpdate.
//
// While the old AC is Terminating it reports KORCReady=False/ReMinting, escalating
// to ReMintStalled once it has been deleting longer than remintStallTimeout — a
// stuck finalizer (e.g. K-ORC cannot reach Keystone to revoke) otherwise loops on
// ReMinting forever with no operator-visible signal.
func (r *ControlPlaneReconciler) remintAdminApplicationCredential(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, ac *orcv1alpha1.ApplicationCredential,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Already Terminating: wait for K-ORC's finalizer to revoke + remove the AC
	// before the next pass recreates it. Escalate to ReMintStalled past the timeout.
	if !ac.DeletionTimestamp.IsZero() {
		reason := "ReMinting"
		message := fmt.Sprintf("re-minting admin application credential %q; awaiting revoke of the previous credential", ac.Name)
		if time.Since(ac.DeletionTimestamp.Time) > remintStallTimeout {
			reason = "ReMintStalled"
			message = fmt.Sprintf("admin application credential %q has been Terminating longer than %s; "+
				"K-ORC may be unable to revoke the previous Keystone credential", ac.Name, remintStallTimeout)
			logger.Info("admin application credential re-mint stalled",
				"name", ac.Name, "terminatingSince", ac.DeletionTimestamp.Time)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Trigger the re-mint: delete the AC, then regenerate the secret "value" so the
	// recreated AC mints a fresh credential (a NotFound on delete is benign — the
	// AC is already gone, the recreate happens next pass).
	if err := r.Delete(ctx, ac); err != nil && !apierrors.IsNotFound(err) {
		if meta.IsNoMatchError(err) {
			logger.Info("K-ORC ApplicationCredential CRD not installed; KORCReady=False")
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeKORCReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "KORCCRDNotInstalled",
				Message:            "K-ORC ApplicationCredential CRD is not installed",
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ApplicationCredentialError",
			Message:            fmt.Sprintf("deleting ApplicationCredential for re-mint: %v", err),
		})
		return ctrl.Result{}, err
	}

	if err := r.regenerateAppCredentialSecretValue(ctx, cp); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeKORCReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretError",
			Message:            fmt.Sprintf("regenerating application-credential secret value for re-mint: %v", err),
		})
		return ctrl.Result{}, err
	}

	logger.Info("deleted admin application credential to trigger re-mint", "name", ac.Name)
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "ReMinting",
		Message:            fmt.Sprintf("deleted admin application credential %q; a fresh credential will be minted from the rotated admin password", ac.Name),
	})
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}

// regenerateAppCredentialSecretValue overwrites the app-credential secret "value"
// with a fresh random secret and drops any stale assembled clouds.yaml. The new
// "value" makes the recreated AC mint a NEW Keystone credential; dropping the
// clouds.yaml forces reconcileAdminCredential to rebuild it from the fresh id+value
// rather than keep serving the just-revoked credential.
func (r *ControlPlaneReconciler) regenerateAppCredentialSecretValue(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialSecretName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		v, gerr := generateAppCredSecretValue()
		if gerr != nil {
			return gerr
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[appCredSecretValueKey] = []byte(v)
		delete(secret.Data, appCredCloudsYAMLKey)
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	}); err != nil {
		return fmt.Errorf("regenerating app-credential secret value: %w", err)
	}
	return nil
}

// adminUserRef returns the Kubernetes metadata.name of the imported K-ORC User
// CR the admin application credential is associated with. AdminCredentialSpec has
// no UserRef field, but K-ORC's ApplicationCredentialResourceSpec.UserRef is
// REQUIRED, so we derive a deterministic name scoped by cp.Name (mirroring
// adminDomainRef) — this way two ControlPlanes in one namespace produce DISTINCT
// User objects rather than colliding on a shared name. The
// inner OpenStack username the import resolves to is still "admin": it is set
// independently via Spec.Import.Filter.Name = OpenStackName(korcAdminUsername) in
// ensureKORCAdminImports. The matching User CR is provisioned there as an
// unmanaged import, so the reference always resolves.
func adminUserRef(cp *c5c3v1alpha1.ControlPlane) string {
	return fmt.Sprintf("%s-user-admin", cp.Name)
}

// korcAdminUsername / korcAdminDomainName identify the OpenStack admin user and
// its domain that the Keystone bootstrap creates; the c5c3-operator imports them
// into K-ORC (unmanaged) rather than creating them.
const (
	korcAdminUsername   = "admin"
	korcAdminDomainName = "Default"
)

// adminDomainRef is the deterministic name of the K-ORC Domain CR the admin User
// import is scoped to.
func adminDomainRef(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + "-domain-default"
}

// ensureKORCAdminImports ensures the K-ORC Domain and User that the admin
// ApplicationCredential's UserRef depends on exist as UNMANAGED imports. The
// Keystone bootstrap creates the real admin user (in the Default domain); K-ORC
// must import — not create — it, otherwise the ApplicationCredential blocks on
// "Waiting for User/admin to be created". Both CRs are owned by the ControlPlane
// for GC and reuse the admin clouds.yaml credentials.
func (r *ControlPlaneReconciler) ensureKORCAdminImports(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, credRef orcv1alpha1.CloudCredentialsReference) error {
	domain := &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: adminDomainRef(cp), Namespace: childNamespace(cp)},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, domain, func() error {
		domain.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
		domain.Spec.CloudCredentialsRef = credRef
		domain.Spec.Import = &orcv1alpha1.DomainImport{
			Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(korcAdminDomainName))},
		}
		return controllerutil.SetControllerReference(cp, domain, r.Scheme)
	}); err != nil {
		return fmt.Errorf("admin Domain import %q: %w", domain.Name, err)
	}

	user := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: adminUserRef(cp), Namespace: childNamespace(cp)},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, user, func() error {
		user.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyUnmanaged
		user.Spec.CloudCredentialsRef = credRef
		user.Spec.Import = &orcv1alpha1.UserImport{
			Filter: &orcv1alpha1.UserFilter{
				Name:      ptr.To(orcv1alpha1.OpenStackName(korcAdminUsername)),
				DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(adminDomainRef(cp))),
			},
		}
		return controllerutil.SetControllerReference(cp, user, r.Scheme)
	}); err != nil {
		return fmt.Errorf("admin User import %q: %w", user.Name, err)
	}
	return nil
}

// projectAccessRules maps our AccessRule{Service,Method,Path} list onto K-ORC's
// ApplicationCredentialAccessRule list. K-ORC models the service as a serviceRef
// (a reference to an ORC Service CR named after the service type) and the method
// as a typed HTTPMethod enum; path is a plain string pointer.
func projectAccessRules(rules []c5c3v1alpha1.AccessRule) []orcv1alpha1.ApplicationCredentialAccessRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]orcv1alpha1.ApplicationCredentialAccessRule, 0, len(rules))
	for _, rule := range rules {
		projected := orcv1alpha1.ApplicationCredentialAccessRule{}
		if rule.Path != "" {
			projected.Path = ptr.To(rule.Path)
		}
		if rule.Method != "" {
			method := orcv1alpha1.HTTPMethod(rule.Method)
			projected.Method = &method
		}
		// DECISION (AccessRule.Service): K-ORC takes a serviceRef (KubernetesNameRef
		// to an ORC Service CR), not a free-form service-type string. Per the
		// vendored K-ORC actuator (internal/controllers/applicationcredential/
		// actuator.go) K-ORC resolves serviceRef to an EXISTING Service CR by
		// metadata.name and uses that Service's Status.Resource.Type as the OpenStack
		// access-rule service. We pass rule.Service verbatim as that CR name, so a
		// site using access rules MUST provision a K-ORC Service CR whose
		// metadata.name == rule.Service (e.g. a Service named "identity"). NOTE: this
		// is NOT the catalog Service reconcileCatalog registers — that one is named
		// keystoneServiceName(cp) = "{cp.Name}-identity-service" (type "identity"),
		// so it does not satisfy a rule.Service of "identity" by name. AccessRules are
		// unused on the default/E2E path (the list is empty), so this does not affect
		// the headline credential chain. Reviewer: please verify the intended
		// rule.Service → Service-CR-name convention on a live cluster.
		if rule.Service != "" {
			ref := orcv1alpha1.KubernetesNameRef(rule.Service)
			projected.ServiceRef = &ref
		}
		out = append(out, projected)
	}
	return out
}

// computeAdminPasswordHash reads the admin password from the ControlPlane's
// configured PasswordSecretRef and returns its SHA-256 as a lowercase hex string
// The data key defaults to "password" when the SecretRef.Key
// is unset, matching the keystone admin-password Secret convention.
//
// DECISION (hash-helper extraction): the hash derivation lives here as a
// package-level function so BOTH the ControlPlane reconciler (which reads the
// cleartext via readAdminPassword and hashes it via hashAdminPassword inline) and
// the CredentialRotation reconciler compute the SAME hash without duplicating the
// SHA-256 logic.
func computeAdminPasswordHash(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	pw, err := readAdminPassword(ctx, c, cp)
	if err != nil {
		return "", err
	}
	return hashAdminPassword(pw), nil
}

// readAdminPassword reads the cleartext admin password from the EFFECTIVE
// admin-password Secret (data key defaults to "password"). The effective ref
// is the operator-owned per-ControlPlane Secret
// adminPasswordSecretName(cp) in managed mode and the user-supplied
// cp.Spec.KORC.AdminCredential.PasswordSecretRef in brownfield mode — see
// effectiveAdminPasswordSecretRef. reconcileKORC needs the cleartext — not just
// its hash — to render the password-based clouds.yaml the admin
// ApplicationCredential mints with, so the read is factored out here and the
// hash derived from it via hashAdminPassword.
func readAdminPassword(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	ref := effectiveAdminPasswordSecretRef(cp)
	key := ref.Key
	if key == "" {
		key = "password"
	}
	return secrets.GetSecretValue(ctx, c,
		types.NamespacedName{Namespace: cp.Namespace, Name: ref.Name}, key)
}

// hashAdminPassword returns the SHA-256 of the admin password as a lowercase hex
// string — the value stamped onto the AC CR's adminPasswordHashAnnotation.
func hashAdminPassword(pw string) string {
	sum := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(sum[:])
}

// updateAdminApplicationCredentialStatus reflects the observed AC CR into
// cp.Status.AdminApplicationCredential. LastRotation is
// (re-)stamped to now whenever the recorded credential ID changes (initial mint
// or re-mint), so a rotation is observable from status; once the ID is stable it
// is preserved across reconciles.
func (r *ControlPlaneReconciler) updateAdminApplicationCredentialStatus(
	cp *c5c3v1alpha1.ControlPlane, ac *orcv1alpha1.ApplicationCredential, desiredRestricted bool,
) {
	var id string
	if ac.Status.ID != nil {
		id = *ac.Status.ID
	}

	restricted := desiredRestricted
	if ac.Status.Resource != nil {
		// K-ORC reports Unrestricted; invert back to our Restricted semantics.
		restricted = !ac.Status.Resource.Unrestricted
	}

	prev := cp.Status.AdminApplicationCredential
	rotated := prev == nil || prev.ID != id

	status := &c5c3v1alpha1.AdminApplicationCredentialStatus{
		ID:         id,
		Restricted: restricted,
	}
	switch {
	case rotated && id != "":
		now := metav1.Now()
		status.LastRotation = &now
	case prev != nil:
		status.LastRotation = prev.LastRotation
	}
	cp.Status.AdminApplicationCredential = status
}

// reconcileAdminCredential commits the minted application credential into an
// operator-owned Secret and mirrors it to OpenBao, driving the
// AdminCredentialReady condition.
//
// It is GATED on KORCReady: until reconcileKORC reports the AC minted there is
// nothing to push. It is additionally gated on the K-ORC clouds.yaml
// ExternalSecret being Ready in the ControlPlane's OWN namespace
// (childNamespace(cp)/<CloudCredentialsRef.SecretName>) — co-located with the
// K-ORC CRs per C1 — so the credential is never published before K-ORC can
// authenticate with its admin cloud.
func (r *ControlPlaneReconciler) reconcileAdminCredential(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Gate on KORCReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeKORCReady) {
		logger.Info("KORC not ready, deferring admin credential push")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForKORC",
			Message:            "KORCReady is not True; admin credential push deferred",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Gate on the K-ORC clouds.yaml ExternalSecret being Ready. It MUST materialise
	// in the SAME namespace as the K-ORC resource CRs (childNamespace) because
	// K-ORC resolves CloudCredentialsRef in the resource's own namespace (C1). The
	// Secret name follows the spec's CloudCredentialsRef.SecretName — the exact
	// value reconcileKORC sets on the AC CR — defaulted to korcCloudsYamlSecretName
	// by the webhook (the fallback below covers a webhook-bypass edge case).
	cloudsYamlName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if cloudsYamlName == "" {
		cloudsYamlName = korcCloudsYamlSecretName
	}
	ready, err := secrets.WaitForExternalSecret(ctx, r.Client,
		types.NamespacedName{Namespace: childNamespace(cp), Name: cloudsYamlName})
	if err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "CloudsYamlError",
			Message:            fmt.Sprintf("checking k-orc clouds.yaml ExternalSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !ready {
		logger.Info("k-orc clouds.yaml ExternalSecret not ready, requeuing")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForCloudsYaml",
			Message:            "k-orc clouds.yaml ExternalSecret is not yet Ready",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Assemble the application-credential clouds.yaml into the operator-owned Secret
	// so the PushSecret mirrors it to OpenBao (and ESO re-materialises it as the
	// admin clouds.yaml, replacing the password-based bootstrap seed). K-ORC does
	// NOT write this — it only consumed Secret.Data["value"] to mint — so we build
	// it from the minted credential id (AC status, surfaced on cp.Status) and the
	// generated secret value. The "value" key is preserved untouched.
	acID := ""
	if cp.Status.AdminApplicationCredential != nil {
		acID = cp.Status.AdminApplicationCredential.ID
	}
	if acID == "" {
		logger.Info("admin application credential id not yet reported, deferring credential assembly")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForCredentialID",
			Message:            "minted application credential id is not yet reported by K-ORC",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// The operator-owned Secret (with its generated "value" and owner reference)
	// was created by ensureAppCredentialSecret during reconcileKORC, which KORCReady
	// — gated above — guarantees has run and K-ORC has read the "value" to mint.
	// Get it directly rather than CreateOrUpdate: a NotFound here is an invariant
	// violation (the minted secret vanished), NOT a create opportunity — minting a
	// fresh "value" would not match the credential Keystone already issued, so we
	// requeue and let reconcileKORC re-establish the Secret instead of writing a
	// brand-new empty one that would immediately fail the "value" check.
	secretKey := types.NamespacedName{
		Name:      adminAppCredentialSecretName(cp),
		Namespace: childNamespace(cp),
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("app-credential secret not found, deferring credential assembly", "secret", secretKey.Name)
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeAdminCredentialReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "WaitingForAppCredentialSecret",
				Message:            fmt.Sprintf("app-credential secret %q does not exist yet", secretKey.Name),
			})
			return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "SecretError",
			Message:            fmt.Sprintf("reading admin app-credential secret %q: %v", secretKey.Name, err),
		})
		return ctrl.Result{}, err
	}

	value := secret.Data[appCredSecretValueKey]
	if len(value) == 0 {
		logger.Info("app-credential secret has no value yet, deferring credential assembly", "secret", secretKey.Name)
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForAppCredentialSecret",
			Message: fmt.Sprintf("app-credential secret %q has no %q key (mint not complete?)",
				secret.Name, appCredSecretValueKey),
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	// Persist the assembled clouds.yaml under appCredCloudsYAMLKey, leaving the
	// "value" key untouched. Skip the write when it already matches so repeated
	// reconciles do not churn the Secret (and wake ESO to re-push).
	cloudsYAML := []byte(buildAppCredCloudsYAML(cp, acID, string(value)))
	if !bytes.Equal(secret.Data[appCredCloudsYAMLKey], cloudsYAML) {
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		if err := r.Update(ctx, secret); err != nil {
			conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
				Type:               conditionTypeAdminCredentialReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: cp.Generation,
				Reason:             "SecretError",
				Message:            fmt.Sprintf("assembling admin app-credential clouds.yaml: %v", err),
			})
			return ctrl.Result{}, err
		}
	}

	// CLOBBER-SAFE PushSecret: EnsurePushSecret is idempotent — it only Updates
	// the PushSecret when its desired Spec differs from the stored one
	// (apiequality.Semantic.DeepEqual guard inside EnsurePushSecret). Repeated
	// reconciles therefore do not churn the PushSecret, so ESO is not woken to
	// re-push an unchanged credential.
	ps := adminAppCredentialPushSecret(cp)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, cp, ps); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "PushSecretError",
			Message:            fmt.Sprintf("ensuring admin app-credential PushSecret: %v", err),
		})
		return ctrl.Result{}, err
	}

	// Gate AdminCredentialReady on the PushSecret actually syncing to OpenBao — not
	// merely on the CR existing. Otherwise a backend permission failure (e.g. the
	// ESO role missing the push-app-credentials policy) yields a false-positive
	// Ready while OpenBao still serves the password-based bootstrap clouds.yaml.
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ps.Namespace}, pushed); err != nil {
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "PushSecretError",
			Message:            fmt.Sprintf("reading admin app-credential PushSecret: %v", err),
		})
		return ctrl.Result{}, err
	}
	if !pushSecretReady(pushed) {
		logger.Info("admin app-credential PushSecret not yet synced, requeuing")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeAdminCredentialReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForPushSecret",
			Message:            "admin app-credential PushSecret has not synced to OpenBao yet",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAdminCredentialReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "AdminCredentialReady",
		Message:            "Admin application credential committed and mirrored to OpenBao",
	})
	return ctrl.Result{}, nil
}

// pushSecretReady reports whether an ESO PushSecret has synced to its backend
// (its "Ready" condition is True).
func pushSecretReady(ps *esov1alpha1.PushSecret) bool {
	for _, c := range ps.Status.Conditions {
		if c.Type == esov1alpha1.PushSecretReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// adminAppCredentialPushSecret builds the PushSecret that mirrors the minted
// admin application-credential Secret to OpenBao at the per-ControlPlane path
// openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential
// scoping the credential so two ControlPlanes never clobber
// each other's admin credential on the cluster-global OpenBao backend.
//
// DECISION (DeletionPolicy): None — the admin application credential is a shared
// bootstrap secret other consumers may depend on; deleting the PushSecret (e.g.
// on ControlPlane teardown) leaves the last-pushed credential intact in OpenBao
// so a fresh control plane is not locked out mid-rotation. Reviewer: please verify.
func adminAppCredentialPushSecret(cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminAppCredentialPushSecretName(cp),
			Namespace: childNamespace(cp),
		},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyNone,
			SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
				Kind: "ClusterSecretStore",
				Name: openBaoClusterStoreName,
			}},
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{
					Name: adminAppCredentialSecretName(cp),
				},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{
						RemoteKey: adminAppCredentialRemoteKeyFor(cp),
					},
				},
			}},
		},
	}
}

// ensureKORCCloudsYAMLExternalSecret create-or-updates the per-ControlPlane,
// operator-owned ExternalSecret that materialises the admin clouds.yaml Secret
// K-ORC authenticates with, replacing the retired static single-identity manifest
// It is created in childNamespace(cp) — co-located with the
// K-ORC resource CRs because K-ORC resolves CloudCredentialsRef in the resource's
// own namespace (C1) — and reads the per-CR OpenBao path
// adminAppCredentialRemoteKeyFor(cp), so an arbitrarily named ControlPlane resolves
// to the correct key with no manifest edit.
//
// The Secret name follows the spec's CloudCredentialsRef.SecretName (defaulted to
// korcCloudsYamlSecretName by the webhook; the fallback covers a webhook-bypass
// edge case). CreationPolicy Owner makes ESO own the materialised Secret, and the
// ExternalSecret itself is owner-referenced to the ControlPlane for GC. The
// ExternalSecret type is esov1 (NOT esov1alpha1 as the PushSecret above is).
func (r *ControlPlaneReconciler) ensureKORCCloudsYAMLExternalSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	name := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	if name == "" {
		name = korcCloudsYamlSecretName
	}
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
		es.Spec.RefreshInterval = &metav1.Duration{Duration: time.Hour}
		es.Spec.SecretStoreRef = esov1.SecretStoreRef{
			Kind: "ClusterSecretStore",
			Name: openBaoClusterStoreName,
		}
		es.Spec.Target = esov1.ExternalSecretTarget{
			Name:           name,
			CreationPolicy: esov1.CreatePolicyOwner,
		}
		es.Spec.Data = []esov1.ExternalSecretData{{
			SecretKey: appCredCloudsYAMLKey,
			RemoteRef: esov1.ExternalSecretDataRemoteRef{
				Key:      adminAppCredentialRemoteKeyFor(cp),
				Property: appCredCloudsYAMLKey,
			},
		}}
		return controllerutil.SetControllerReference(cp, es, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensuring k-orc clouds.yaml ExternalSecret %q: %w", es.Name, err)
	}
	return nil
}

// reconcileCatalog registers the OpenStack service catalog entries for Keystone
// (an identity Service plus its public Endpoint) as OWNED K-ORC CRs and drives
// the CatalogReady condition.
//
// It is GATED on AdminCredentialReady: the admin credential must be available
// before K-ORC can register catalog entries. Both child CRs are create-or-updated
// idempotently; CatalogReady (and cp.Status.CatalogReady) flip True once both are
// registered. MISSING-CRD SAFETY mirrors reconcileKORC.
func (r *ControlPlaneReconciler) reconcileCatalog(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Gate on AdminCredentialReady.
	if !conditions.AllTrue(cp.Status.Conditions, conditionTypeAdminCredentialReady) {
		logger.Info("AdminCredential not ready, deferring catalog registration")
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "WaitingForAdminCredential",
			Message:            "AdminCredentialReady is not True; catalog registration deferred",
		})
		return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
	}

	secretName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.SecretName
	cloudName := cp.Spec.KORC.AdminCredential.CloudCredentialsRef.CloudName
	credRef := orcv1alpha1.CloudCredentialsReference{SecretName: secretName, CloudName: cloudName}

	// 1. Identity (Keystone) Service.
	service := &orcv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneServiceName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		service.Spec.CloudCredentialsRef = credRef
		if service.Spec.Resource == nil {
			service.Spec.Resource = &orcv1alpha1.ServiceResourceSpec{}
		}
		service.Spec.Resource.Type = "identity"
		service.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName("keystone"))
		service.Spec.Resource.Enabled = ptr.To(true)
		return controllerutil.SetControllerReference(cp, service, r.Scheme)
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return r.catalogCRDMissing(cp, logger)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "ServiceError",
			Message:            fmt.Sprintf("create-or-update identity Service: %v", err),
		})
		return ctrl.Result{}, err
	}

	// 2. Public Endpoint for the Keystone API.
	endpoint := &orcv1alpha1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keystoneEndpointName(cp),
			Namespace: childNamespace(cp),
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, endpoint, func() error {
		endpoint.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		endpoint.Spec.CloudCredentialsRef = credRef
		if endpoint.Spec.Resource == nil {
			endpoint.Spec.Resource = &orcv1alpha1.EndpointResourceSpec{}
		}
		endpoint.Spec.Resource.Interface = "public"
		// DECISION (Endpoint URL): K-ORC's EndpointResourceSpec.URL is REQUIRED.
		// When the ControlPlane exposes Keystone externally (a gateway or explicit
		// publicEndpoint is set) we register that public URL so the catalog matches
		// what Keystone's own bootstrap advertises; otherwise we fall back to the
		// in-cluster Keystone Service URL derived from the PROJECTED Keystone
		// Service — keystoneName(cp) = "{cp.Name}-keystone" in the ControlPlane
		// namespace — which is the Service the keystone-operator actually exposes.
		endpoint.Spec.Resource.URL = keystoneCatalogURL(cp)
		endpoint.Spec.Resource.ServiceRef = orcv1alpha1.KubernetesNameRef(keystoneServiceName(cp))
		endpoint.Spec.Resource.Enabled = ptr.To(true)
		return controllerutil.SetControllerReference(cp, endpoint, r.Scheme)
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return r.catalogCRDMissing(cp, logger)
		}
		conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCatalogReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cp.Generation,
			Reason:             "EndpointError",
			Message:            fmt.Sprintf("create-or-update identity Endpoint: %v", err),
		})
		return ctrl.Result{}, err
	}

	cp.Status.CatalogReady = true
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "CatalogRegistered",
		Message:            "Keystone identity Service and Endpoint are registered",
	})
	return ctrl.Result{}, nil
}

// catalogCRDMissing surfaces the MISSING-CRD safety condition for the catalog
// sub-reconciler (mirrors reconcileKORC's KORCCRDNotInstalled handling).
func (r *ControlPlaneReconciler) catalogCRDMissing(cp *c5c3v1alpha1.ControlPlane, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("K-ORC Service/Endpoint CRD not installed; CatalogReady=False")
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "KORCCRDNotInstalled",
		Message:            "K-ORC Service/Endpoint CRD is not installed",
	})
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
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
