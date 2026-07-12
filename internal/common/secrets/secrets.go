// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/apply"
)

// ErrKeyNotFound is returned (wrapped) by GetSecretValue when the requested
// data key is absent from the Secret. Callers use errors.Is to distinguish
// this recoverable condition (e.g. wait-for-credentials) from transport or
// permission errors.
var ErrKeyNotFound = errors.New("key not found in Secret")

// IsMissingSecretOrKey reports whether err indicates either an absent upstream
// Secret (apierrors.IsNotFound through the wrap chain) or a missing data key.
// GetSecretValue wraps the IsNotFound from c.Get with %w so apierrors.IsNotFound
// walks the chain, and wraps ErrKeyNotFound when the requested data key is
// absent so errors.Is walks the chain for the missing-data-key case
func IsMissingSecretOrKey(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, ErrKeyNotFound)
}

// AdminPasswordDigest returns the SHA-256 of the admin password as a lowercase
// hex string. This is the single source of truth for the cross-operator
// admin-password digest contract: the keystone operator gates the bootstrap
// Job re-run on it (forge.c5c3.io/admin-password-hash), while the c5c3 operator
// stamps the same digest onto the application-credential CR annotation. Both
// must compute it identically — a drift between the two would break the
// bootstrap re-run / re-mint gate, so the derivation lives here rather than
// being reimplemented per operator.
func AdminPasswordDigest(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

// WaitForExternalSecret reports the sync status of the ExternalSecret
// identified by key as a tri-state: exists distinguishes "the ExternalSecret
// does not exist yet" from "it exists but is not yet Ready", and ready is true
// only when it has a Ready condition with status True. The return tuple is
// (exists, ready, err):
//
//   - (false, false, nil) — the ExternalSecret was not found.
//   - (true, false, nil)  — it exists but has not synced (no Ready=True).
//   - (true, true, nil)   — it exists and is Ready.
//   - (false, false, err) — an unexpected client failure.
//
// Callers can use exists to surface a clearer status — "ExternalSecret not
// found yet" versus "waiting for ESO to sync" — instead of collapsing both
// into a single "not ready" signal.
func WaitForExternalSecret(ctx context.Context, c client.Client, key client.ObjectKey) (exists, ready bool, err error) {
	es := &esov1.ExternalSecret{}
	if err := c.Get(ctx, key, es); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("getting ExternalSecret %s/%s: %w", key.Namespace, key.Name, err)
	}

	for _, cond := range es.Status.Conditions {
		if cond.Type == esov1.ExternalSecretReady && cond.Status == corev1.ConditionTrue {
			return true, true, nil
		}
	}
	return true, false, nil
}

// IsClusterSecretStoreReady checks whether the ClusterSecretStore identified
// by name currently reports a Ready condition with status True. It returns
// (false, nil) when the store does not exist or is not ready, and (false,
// error) on unexpected client failures. Consumers use this to flip their own
// *Ready conditions when the upstream secret backend is unreachable — ESO
// only re-syncs ExternalSecrets at their refreshInterval (default 1h), so
// relying on ExternalSecret Ready alone would miss short-lived outages.
func IsClusterSecretStoreReady(ctx context.Context, c client.Client, name string) (bool, error) {
	store := &esov1.ClusterSecretStore{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, store); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting ClusterSecretStore %s: %w", name, err)
	}

	for _, cond := range store.Status.Conditions {
		if cond.Type == esov1.SecretStoreReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// IsSecretStoreReady checks whether the namespaced SecretStore identified by
// name and namespace currently reports a Ready condition with status True. It
// is the namespaced twin of IsClusterSecretStoreReady: (false, nil) when the
// store does not exist or is not ready, (false, error) on unexpected client
// failures. A per-tenant SecretStore is resolved in the consuming CR's own
// namespace, so callers pass that namespace here.
func IsSecretStoreReady(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	store := &esov1.SecretStore{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, store); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting SecretStore %s/%s: %w", namespace, name, err)
	}

	for _, cond := range store.Status.Conditions {
		if cond.Type == esov1.SecretStoreReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// IsSecretReady checks whether a Kubernetes Secret exists at the given key and
// contains all expectedKeys in its Data field. When no expectedKeys are
// provided, it only checks for Secret existence. It returns (true, nil) when
// the Secret exists and all expected keys are present, (false, nil) when the
// Secret is not found or is missing expected keys, and (false, error) on
// unexpected failures.
func IsSecretReady(ctx context.Context, c client.Client, key client.ObjectKey, expectedKeys ...string) (bool, error) {
	secret := &corev1.Secret{}
	err := c.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	for _, k := range expectedKeys {
		if _, ok := secret.Data[k]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// GetSecretValue retrieves the value of a specific data key from the Secret
// identified by key. It returns an error if the Secret is not found or if the
// data key is not present.
func GetSecretValue(ctx context.Context, c client.Client, key client.ObjectKey, dataKey string) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("getting Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	val, ok := secret.Data[dataKey]
	if !ok {
		return "", fmt.Errorf("%w: key %q in Secret %s/%s", ErrKeyNotFound, dataKey, key.Namespace, key.Name)
	}
	return string(val), nil
}

// EnsurePushSecret creates a PushSecret if it does not exist or applies its
// desired state via Server-Side Apply if it already exists. An owner reference
// is set on the PushSecret so that it is garbage-collected when the owning
// resource is deleted. Because the field manager owns only the fields the
// builder sets, server-defaulted fields (updatePolicy, refreshInterval) are
// never claimed and a converged PushSecret is applied without a write, so ESO
// is not woken to re-push an unchanged credential.
func EnsurePushSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, ps *esov1alpha1.PushSecret) error {
	return apply.EnsureObject(ctx, c, scheme, owner, ps, apply.FieldManager)
}
