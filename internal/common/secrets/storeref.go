// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/c5c3/forge/internal/common/types"
)

// EffectiveStoreRef resolves an optional per-CR SecretStoreRefSpec to the
// concrete store the operators route ExternalSecrets and PushSecrets through.
// A nil ref defaults to today's shared cluster-scoped store
// (ClusterSecretStore/openbao-cluster-store), so a CR that omits the field
// behaves exactly as before this change. An explicit ref whose Kind is empty
// is normalised to ClusterSecretStore — a webhook-bypass safety net matching
// the +kubebuilder:default on SecretStoreRefSpec.Kind — so a store reference
// persisted without a kind never resolves to the empty string.
func EffectiveStoreRef(ref *commonv1.SecretStoreRefSpec) commonv1.SecretStoreRefSpec {
	if ref == nil {
		return commonv1.SecretStoreRefSpec{
			Kind: commonv1.SecretStoreKindCluster,
			Name: OpenBaoClusterStoreName,
		}
	}
	kind := ref.Kind
	if kind == "" {
		kind = commonv1.SecretStoreKindCluster
	}
	return commonv1.SecretStoreRefSpec{Kind: kind, Name: ref.Name}
}

// IsStoreRefReady reports whether the store selected by ref is Ready. It
// dispatches on the (already-resolved) kind: a cluster-scoped store is looked
// up by name alone, a namespaced store in the given namespace (the consuming
// CR's own namespace). An unrecognised kind returns an error rather than
// silently falling back to a store the caller did not select — a wrong-store
// gate would mask misconfiguration. Callers pass EffectiveStoreRef(...) so the
// nil/empty-kind cases are already normalised.
func IsStoreRefReady(ctx context.Context, c client.Client, ref commonv1.SecretStoreRefSpec, namespace string) (bool, error) {
	switch ref.Kind {
	case commonv1.SecretStoreKindCluster:
		return IsClusterSecretStoreReady(ctx, c, ref.Name)
	case commonv1.SecretStoreKindNamespaced:
		return IsSecretStoreReady(ctx, c, ref.Name, namespace)
	default:
		return false, fmt.Errorf("unknown secret store kind %q for store %q", ref.Kind, ref.Name)
	}
}

// ESOSecretStoreRef converts a resolved store reference to the ESO
// ExternalSecret SecretStoreRef the operators stamp onto ExternalSecrets.
func ESOSecretStoreRef(ref commonv1.SecretStoreRefSpec) esov1.SecretStoreRef {
	return esov1.SecretStoreRef{Kind: string(ref.Kind), Name: ref.Name}
}

// PushSecretStoreRefs converts a resolved store reference to the single-element
// ESO PushSecretStoreRef slice the operators stamp onto PushSecrets.
func PushSecretStoreRefs(ref commonv1.SecretStoreRefSpec) []esov1alpha1.PushSecretStoreRef {
	return []esov1alpha1.PushSecretStoreRef{{
		Kind: string(ref.Kind),
		Name: ref.Name,
	}}
}
