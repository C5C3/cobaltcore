// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OpenBaoClusterStoreName is the ClusterSecretStore that fronts the OpenBao
// backend used by deploy/eso. The operators check this store's Ready
// condition on every reconcile so their secret-derived conditions reflect
// upstream backend outages within the ESO store-reconcile interval —
// ExternalSecrets themselves use a 1h refreshInterval and would otherwise
// mask short outages.
const OpenBaoClusterStoreName = "openbao-cluster-store"

// GateState is the outcome of GateSyncedSecret's ExternalSecret-synced →
// keys-present ladder rungs over one ESO-materialized Secret.
type GateState int

const (
	// GateReady: the materialized Secret exists with all expected keys.
	GateReady GateState = iota
	// GateExternalSecretMissing: neither the Secret nor its ExternalSecret
	// exists yet.
	GateExternalSecretMissing
	// GateExternalSecretNotSynced: the ExternalSecret exists but has not
	// synced the Secret yet (Ready is not True).
	GateExternalSecretNotSynced
	// GateSecretKeysMissing: the ExternalSecret reports Ready but the Secret
	// lacks a required key — a status-vs-object race where ESO flipped Ready
	// before committing the Secret, or a genuinely malformed Secret.
	GateSecretKeysMissing
)

// GateSyncedSecret verifies that the ESO-materialized Secret at key is
// usable, checking the Secret before the ExternalSecret to save a read in
// steady state. It returns GateReady when the Secret exists with all
// expectedKeys (an empty expectedKeys list only requires existence). On a
// miss it consults the same-named ExternalSecret solely to attribute the
// cause, so callers can surface a precise condition message per state.
//
// Semantic note: because the materialized Secret is checked first, an
// ExternalSecret whose Ready condition is momentarily False while the Secret
// still holds valid keys does not gate. That matches how pods consume the
// Secret directly; a ClusterSecretStore readiness check
// (IsClusterSecretStoreReady) remains the authoritative backend-outage
// detector callers run before this ladder.
func GateSyncedSecret(ctx context.Context, c client.Client, key client.ObjectKey, expectedKeys ...string) (GateState, error) {
	secretReady, err := IsSecretReady(ctx, c, key, expectedKeys...)
	if err != nil {
		return GateExternalSecretMissing, err
	}
	if secretReady {
		return GateReady, nil
	}

	exists, esReady, err := WaitForExternalSecret(ctx, c, key)
	if err != nil {
		return GateExternalSecretMissing, err
	}
	switch {
	case !exists:
		return GateExternalSecretMissing, nil
	case esReady:
		return GateSecretKeysMissing, nil
	default:
		return GateExternalSecretNotSynced, nil
	}
}
