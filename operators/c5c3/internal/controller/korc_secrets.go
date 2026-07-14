// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// adminAppCredentialSecretSuffix names the operator-owned Secret that K-ORC
// writes the minted application credential into (Resource.SecretRef). It is the
// push source for the OpenBao PushSecret.
const adminAppCredentialSecretSuffix = "-admin-app-credential" //nolint:gosec // G101 false positive: Secret name suffix, not a credential.

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

// appCredSecretValueKey is the Secret data key K-ORC reads the application
// credential's secret from (the actuator reads Secret.Data["value"]).
const appCredSecretValueKey = "value"

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

// ensureOwnedSecret create-or-updates an operator-owned corev1.Secret named
// `name` in childNamespace(cp), with cp set as the controller owner reference.
// The Secret's Data map is guaranteed non-nil before `mutate` runs, so callers
// only set the keys they own; `mutate` may return an error to abort the write
// (e.g. when generating a random value fails). It stays read-modify-write (not
// Server-Side Apply) precisely because `mutate` reads the LIVE Secret's Data to
// preserve generated-once random values across reconciles, which cannot be a
// pure projection. It centralises the four
// near-identical owned-Secret CreateOrUpdate wrappers (ensureAppCredentialSecret,
// ensureAdminPasswordCloud, seedBootstrapCloudsYAML and
// regenerateAppCredentialSecretValue); each keeps its own error wrapping so the
// failure context stays specific.
func (r *ControlPlaneReconciler) ensureOwnedSecret(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, name string, mutate func(*corev1.Secret) error,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: childNamespace(cp),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if err := mutate(secret); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cp, secret, r.Scheme)
	})
	return err
}

// setCACertKey projects the external private-CA bundle into an operator-owned
// K-ORC credentials Secret under the inline "cacert" key K-ORC reads natively —
// or deletes the key when no bundle is configured, so REMOVING
// spec.services.keystone.external.caBundleSecretRef converges the Secret instead
// of leaving a stale trust anchor behind.
//
// DOCUMENTED CONSTRAINT (K-ORC provider-client cache aliasing): K-ORC keys its
// provider-client cache on the PARSED CLOUD STRUCT only — "cacert" is NOT part of
// the key (internal/scope/provider.go). The cache TTL is the token lifetime / 2
// (~30 min at Keystone defaults), so a rotated or removed CA bundle only takes
// effect once the cached client expires: the Secret converges immediately, the
// trust store does not. Nothing in this operator can shorten that window.
func setCACertKey(secret *corev1.Secret, caBundle string) {
	if caBundle == "" {
		delete(secret.Data, korcCACertKey)
		return
	}
	secret.Data[korcCACertKey] = []byte(caBundle)
}

// caCertPushTrigger derives the PushSecret annotation value that tracks the
// "cacert" state setCACertKey just wrote into the app-credential source Secret.
//
// It hashes the RESOLVED bundle — the EMPTY string included — so every transition
// changes it exactly once: adding a bundle, rotating it, removing it, and adding
// the same bundle back later. Hashing only non-empty bundles would leave the
// annotation pinned at the old value across a remove/re-add cycle, so the re-added
// key would never be pushed and the read-back would be declared against a property
// the intervening whole-Secret push had already dropped.
func caCertPushTrigger(caBundle string) string {
	sum := sha256.Sum256([]byte(caBundle))
	return hex.EncodeToString(sum[:])
}

// readExternalCABundle reads the private-CA bundle an External-mode ControlPlane
// references, from the ControlPlane's own namespace (mirroring readAdminPassword).
// It returns "" — with no error and no API read — when no bundle is configured,
// which is both the managed-mode and the publicly-trusted-endpoint case. The data
// key defaults to DefaultCABundleSecretKey ("ca.crt") for a webhook-bypassed CR.
//
// A missing Secret/key surfaces as a secrets.IsMissingSecretOrKey error so the
// caller can defer (KORCReady=False/WaitingForCABundle) rather than mint against
// an endpoint it cannot verify. A present-but-EMPTY key is the same non-verifiable
// state — the normal transient of a two-step "create the Secret, then populate it"
// flow (cert-manager, CI templating) — but GetSecretValue reports it as a successful
// ("", nil) read, indistinguishable from "no bundle configured". Mapping it onto
// ErrKeyNotFound keeps the returned bundle non-empty whenever a ref is set, so
// setCACertKey and ensureKORCCloudsYAMLExternalSecret can share one predicate.
func readExternalCABundle(ctx context.Context, c client.Client, cp *c5c3v1alpha1.ControlPlane) (string, error) {
	ref := externalCABundleRef(cp)
	if ref == nil {
		return "", nil
	}
	key := ref.Key
	if key == "" {
		key = c5c3v1alpha1.DefaultCABundleSecretKey
	}
	name := types.NamespacedName{Namespace: cp.Namespace, Name: ref.Name}
	bundle, err := secrets.GetSecretValue(ctx, c, name, key)
	if err != nil {
		return "", err
	}
	if bundle == "" {
		return "", fmt.Errorf("%w: key %q in Secret %s/%s is present but empty",
			secrets.ErrKeyNotFound, key, name.Namespace, name.Name)
	}
	return bundle, nil
}

// ensureAppCredentialSecret ensures the operator-owned Secret that K-ORC reads the
// application-credential secret from exists with a generated "value". K-ORC's
// managed ApplicationCredential reads Secret.Data["value"] and creates the AC in
// Keystone with it, so this MUST exist before the AC is reconciled. The value is
// generated once and preserved across reconciles — regenerating it would force a
// re-mint and invalidate the stored clouds.yaml.
//
// The external CA bundle (empty in managed mode) is projected alongside as
// "cacert". This Secret is the PushSecret's source and ESO pushes it WHOLE, so the
// bundle reaches OpenBao next to the assembled clouds.yaml with no extra plumbing.
func (r *ControlPlaneReconciler) ensureAppCredentialSecret(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, caBundle string) error {
	if err := r.ensureOwnedSecret(ctx, cp, adminAppCredentialSecretName(cp), func(secret *corev1.Secret) error {
		if len(secret.Data[appCredSecretValueKey]) == 0 {
			v, gerr := generateAppCredSecretValue()
			if gerr != nil {
				return gerr
			}
			secret.Data[appCredSecretValueKey] = []byte(v)
		}
		setCACertKey(secret, caBundle)
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring app-credential secret %q: %w", adminAppCredentialSecretName(cp), err)
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
//
// The external CA bundle (empty in managed mode) is projected alongside as
// "cacert": this is the credential the ApplicationCredential authenticates with
// DIRECTLY, so without the bundle a private-CA endpoint fails TLS verification on
// every mint and re-mint.
func (r *ControlPlaneReconciler) ensureAdminPasswordCloud(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, password, caBundle string) error {
	if err := r.ensureOwnedSecret(ctx, cp, adminPasswordCloudSecretName(cp), func(secret *corev1.Secret) error {
		secret.Data[appCredCloudsYAMLKey] = []byte(buildPasswordCloudsYAML(cp, password))
		setCACertKey(secret, caBundle)
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring admin password-cloud secret %q: %w", adminPasswordCloudSecretName(cp), err)
	}
	return nil
}

// seedBootstrapCloudsYAML writes the PASSWORD-based clouds.yaml into the
// {cp.Name}-admin-app-credential Secret's clouds.yaml key, but ONLY when that key
// is empty (write-if-empty). It breaks the AdminCredentialReady chicken-and-egg
// deadlock on a fresh cluster: the per-CR OpenBao path the
// k-orc-clouds-yaml ExternalSecret reads is empty until something pushes to it, so
// the operator seeds a password-based document here that lets K-ORC's admin
// imports authenticate before the application credential is ever minted —
// in MANAGED mode this was previously seeded by
// deploy/openbao/bootstrap/write-bootstrap-secrets.sh.
//
// The seed NEVER invents a password. It renders the cleartext reconcileKORC read
// from the EFFECTIVE admin-password Secret (readAdminPassword ->
// effectiveAdminPasswordSecretRef): the operator-owned, OpenBao-projected Secret
// in managed mode, and the USER-SUPPLIED Secret in External and brownfield mode.
// An absent user Secret is not a seeding opportunity — reconcileKORC defers with
// KORCReady=False/WaitingForAdminPassword before this is ever called, so the key
// simply stays unseeded. A generated password would never authenticate against a
// pre-existing Keystone anyway.
//
// Write-if-empty is the idempotency guard: once
// reconcileAdminCredential fills the key with the minted credential-based
// clouds.yaml (buildAppCredCloudsYAML) the seed becomes a no-op and never clobbers
// the minted document. On a re-mint, regenerateAppCredentialSecretValue deletes
// the key, so the next reconcileKORC pass re-seeds the password version and bridges
// the re-authentication gap. The "value" key (owned by ensureAppCredentialSecret)
// is never touched.
func (r *ControlPlaneReconciler) seedBootstrapCloudsYAML(ctx context.Context, cp *c5c3v1alpha1.ControlPlane, password string) error {
	if err := r.ensureOwnedSecret(ctx, cp, adminAppCredentialSecretName(cp), func(secret *corev1.Secret) error {
		// Write-if-empty: never overwrite a minted credential-based clouds.yaml.
		if len(secret.Data[appCredCloudsYAMLKey]) == 0 {
			secret.Data[appCredCloudsYAMLKey] = []byte(buildPasswordCloudsYAML(cp, password))
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seeding bootstrap clouds.yaml into secret %q: %w", adminAppCredentialSecretName(cp), err)
	}
	return nil
}

// regenerateAppCredentialSecretValue overwrites the app-credential secret "value"
// with a fresh random secret and drops any stale assembled clouds.yaml. The new
// "value" makes the recreated AC mint a NEW Keystone credential; dropping the
// clouds.yaml forces reconcileAdminCredential to rebuild it from the fresh id+value
// rather than keep serving the just-revoked credential.
func (r *ControlPlaneReconciler) regenerateAppCredentialSecretValue(ctx context.Context, cp *c5c3v1alpha1.ControlPlane) error {
	if err := r.ensureOwnedSecret(ctx, cp, adminAppCredentialSecretName(cp), func(secret *corev1.Secret) error {
		v, gerr := generateAppCredSecretValue()
		if gerr != nil {
			return gerr
		}
		secret.Data[appCredSecretValueKey] = []byte(v)
		delete(secret.Data, appCredCloudsYAMLKey)
		return nil
	}); err != nil {
		return fmt.Errorf("regenerating app-credential secret value: %w", err)
	}
	return nil
}

// computeAdminPasswordHash reads the admin password from the ControlPlane's
// configured PasswordSecretRef and returns its SHA-256 as a lowercase hex string.
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
// effectiveAdminPasswordSecretRef. It is read from
// effectiveAdminPasswordSecretNamespace: the KEYSTONE service namespace in managed
// mode (where the operator materialises it, beside the child that consumes it) and
// the ControlPlane's own namespace when the Secret is the user's.
// reconcileKORC needs the cleartext — not just
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
		types.NamespacedName{Namespace: effectiveAdminPasswordSecretNamespace(cp), Name: ref.Name}, key)
}

// hashAdminPassword returns the SHA-256 of the admin password as a lowercase hex
// string — the value stamped onto the AC CR's adminPasswordHashAnnotation.
func hashAdminPassword(pw string) string {
	sum := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(sum[:])
}
