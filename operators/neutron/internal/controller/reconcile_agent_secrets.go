// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// agentSharedSecretDefaultKey is the Secret data key holding the Nova metadata
// shared secret when spec.novaMetadata.sharedSecretRef.key is empty, which is
// the case of a CR that bypassed the defaulting webhook. It mirrors the value
// that webhook fills in.
const agentSharedSecretDefaultKey = "shared_secret"

// reconcileAgentSecrets gates on the credentials the agent pods consume and
// returns the SHA-256 digest of the transport URL, which the DaemonSet step
// stamps into a pod-template annotation so a rotated broker credential rolls the
// pods: the URL is env-var-consumed, so it only takes effect on a pod restart.
//
// Both blocks it gates on are optional. An agent without spec.novaMetadata
// proxies nowhere, and one without spec.messaging opens no broker connection, so
// a CR that sets neither reports SecretsAvailable without reading anything.
//
// The Secrets are read and written through the children client: they are
// materialised beside the pods that consume them.
func (r *NeutronMetadataAgentReconciler) reconcileAgentSecrets(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) (ctrl.Result, string, error) {
	if ref := agentSharedSecretRef(cr); ref != nil {
		// The secret the agent signs forwarded requests with. Nova rejects an
		// unsigned request when it carries a secret of its own, so a pod started
		// without it would answer every instance with a 403 from Nova.
		ready, err := secrets.GateCredentials(ctx, children, []secrets.CredentialGateSpec{{
			Key:          client.ObjectKey{Namespace: cr.Namespace, Name: ref.Name},
			Reason:       "WaitingForNovaSharedSecret",
			Noun:         "Nova metadata shared secret",
			WaitingMsg:   "Waiting for the Nova metadata shared secret to be synced",
			ExpectedKeys: []string{agentSharedSecretKey(cr)},
		}}, &cr.Status.Conditions, cr.Generation, "SecretsReady")
		if err != nil {
			return ctrl.Result{}, "", err
		}
		if !ready {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", nil
		}
	}

	var digest string
	if cr.Spec.Messaging != nil {
		// The shared flow reads the RabbitmqCluster (managed mode) or the
		// brownfield Secret and writes the derived <name>-transport-url Secret in
		// the CR's own namespace. It reports its own waiting condition under
		// SecretsReady, so a pass that returns a non-zero result leaves the
		// condition it set in place.
		result, _, transportDigest, err := messaging.ReconcileTransportURLSecret(ctx, messaging.TransportURLSecretFlowParams{
			Client:        children,
			Scheme:        r.Scheme,
			Owner:         cr,
			InstanceName:  cr.Name,
			Namespace:     cr.Namespace,
			Messaging:     cr.Spec.Messaging,
			Conditions:    &cr.Status.Conditions,
			Generation:    cr.Generation,
			ConditionType: "SecretsReady",
			RequeueAfter:  commonreconcile.RequeueSecretPolling,
		})
		if err != nil || !result.IsZero() {
			return result, "", err
		}
		digest = transportDigest
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, digest, nil
}

// agentSharedSecretRef returns the Secret reference holding the Nova metadata
// shared secret, or nil when the agent proxies to no Nova metadata API.
func agentSharedSecretRef(cr *neutronv1alpha1.NeutronMetadataAgent) *commonv1.SecretRefSpec {
	if cr.Spec.NovaMetadata == nil {
		return nil
	}
	return cr.Spec.NovaMetadata.SharedSecretRef
}

// agentSharedSecretKey returns the Secret data key the shared secret is read
// from, defaulting to agentSharedSecretDefaultKey. The credential gate and the
// env var the container consumes resolve it through this one function, so a pod
// never sources a key the gate did not check.
func agentSharedSecretKey(cr *neutronv1alpha1.NeutronMetadataAgent) string {
	if ref := agentSharedSecretRef(cr); ref != nil && ref.Key != "" {
		return ref.Key
	}
	return agentSharedSecretDefaultKey
}
