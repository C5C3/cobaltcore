// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
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

// GateClusterStoreReady checks the named ClusterSecretStore's Ready condition
// before the per-Secret gate, so an upstream backend outage surfaces as the
// readiness condition False even while per-ExternalSecret caches still report
// Ready from their last successful sync. On a not-ready store it sets a
// SecretStoreNotReady condition on conds and returns (false, nil); the caller
// requeues. A backend error is propagated as (false, err).
func GateClusterStoreReady(ctx context.Context, c client.Client, storeName string,
	conds *[]metav1.Condition, generation int64, conditionType string,
) (bool, error) {
	ready, err := IsClusterSecretStoreReady(ctx, c, storeName)
	if err != nil {
		return false, err
	}
	if ready {
		return true, nil
	}
	conditions.SetCondition(conds, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		Reason:             "SecretStoreNotReady",
		Message: fmt.Sprintf("ClusterSecretStore %q is not ready; upstream secret backend unreachable",
			storeName),
	})
	return false, nil
}

// CredentialGateSpec describes one credential Secret to gate on: its object
// key, the required keys, and the service-specific condition vocabulary (the
// reason, the human-readable noun, and the generic waiting message).
type CredentialGateSpec struct {
	Key          client.ObjectKey
	Reason       string
	Noun         string
	WaitingMsg   string
	ExpectedKeys []string
}

// GateCredential verifies that a credential Secret is usable, checking the
// materialized Secret before the ExternalSecret to save a read in steady state.
// It returns (true, nil) when the Secret exists with all ExpectedKeys. On a
// miss it consults the ExternalSecret only to produce a precise condition
// message — "ExternalSecret not found yet" vs "waiting to sync" vs "missing
// expected keys" — sets the condition itself with the spec's reason, and
// returns (false, nil). A backend error is propagated as (false, err).
func GateCredential(ctx context.Context, c client.Client, spec CredentialGateSpec,
	conds *[]metav1.Condition, generation int64, conditionType string,
) (bool, error) {
	state, err := GateSyncedSecret(ctx, c, spec.Key, spec.ExpectedKeys...)
	if err != nil {
		return false, err
	}
	if state == GateReady {
		return true, nil
	}

	// The materialized Secret is absent or missing keys; the gate state
	// attributes the cause so the operator surfaces an actionable message.
	msg := spec.WaitingMsg
	switch state {
	case GateExternalSecretMissing:
		msg = fmt.Sprintf("%s ExternalSecret %s/%s not found yet", spec.Noun, spec.Key.Namespace, spec.Key.Name)
	case GateSecretKeysMissing:
		msg = fmt.Sprintf("%s Secret exists but is missing expected keys", spec.Noun)
	case GateExternalSecretNotSynced, GateReady:
		// NotSynced keeps the generic waiting message; Ready returned above.
	}
	conditions.SetCondition(conds, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		Reason:             spec.Reason,
		Message:            msg,
	})
	return false, nil
}

// GateCredentials runs GateCredential over each spec in order. It returns
// (true, nil) only when every credential is ready. On the first not-ready
// credential it returns (false, nil) with the condition already set; on a
// backend error it returns (false, err).
func GateCredentials(ctx context.Context, c client.Client, specs []CredentialGateSpec,
	conds *[]metav1.Condition, generation int64, conditionType string,
) (bool, error) {
	for _, spec := range specs {
		ready, err := GateCredential(ctx, c, spec, conds, generation, conditionType)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}
