// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

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

// agentCABundleDefaultKey is the Secret data key holding the Nova metadata CA
// bundle when spec.novaMetadata.caBundleSecretRef.key is empty, the case of a CR
// that bypassed the defaulting webhook. It mirrors the value that webhook fills
// in.
const agentCABundleDefaultKey = "ca.crt"

// reconcileAgentSecrets gates on the credentials the agent pods consume and
// returns the SHA-256 digests of the transport URL and of the Nova metadata
// shared secret, in that order. The DaemonSet step stamps each into a
// pod-template annotation so a rotated broker credential or shared secret rolls
// the pods: both are env-var-consumed, so they only take effect on a pod
// restart.
//
// Both blocks it gates on are optional. An agent without spec.novaMetadata
// proxies nowhere, and one without spec.messaging opens no broker connection, so
// a CR that sets neither reports SecretsAvailable without reading anything.
// Inside spec.novaMetadata it gates on the shared secret and then on the CA
// bundle named by caBundleSecretRef, each only while it is referenced.
//
// The Secrets are read and written through the children client: they are
// materialised beside the pods that consume them.
func (r *NeutronMetadataAgentReconciler) reconcileAgentSecrets(ctx context.Context, children client.Client,
	cr *neutronv1alpha1.NeutronMetadataAgent,
) (ctrl.Result, string, string, error) {
	var gates []secrets.CredentialGateSpec
	if ref := agentSharedSecretRef(cr); ref != nil {
		// The secret the agent signs forwarded requests with. Nova rejects an
		// unsigned request when it carries a secret of its own, so a pod started
		// without it would answer every instance with a 403 from Nova.
		gates = append(gates, secrets.CredentialGateSpec{
			Key:          client.ObjectKey{Namespace: cr.Namespace, Name: ref.Name},
			Reason:       "WaitingForNovaSharedSecret",
			Noun:         "Nova metadata shared secret",
			WaitingMsg:   "Waiting for the Nova metadata shared secret to be synced",
			ExpectedKeys: []string{agentSharedSecretKey(cr)},
		})
	}
	if ref := agentNovaMetadataCARef(cr); ref != nil {
		// The bundle the agent verifies the Nova metadata API's certificate
		// with. The DaemonSet projects the key as a file, and a pod started
		// without the Secret never leaves ContainerCreating. The noun names the
		// key because the gate's own messages do not.
		key := agentNovaMetadataCAKey(cr)
		gates = append(gates, secrets.CredentialGateSpec{
			Key:          client.ObjectKey{Namespace: cr.Namespace, Name: ref.Name},
			Reason:       "WaitingForNovaMetadataCA",
			Noun:         fmt.Sprintf("Nova metadata CA bundle (key %q)", key),
			WaitingMsg:   "Waiting for the CA bundle the agent verifies the Nova metadata API with",
			ExpectedKeys: []string{key},
		})
	}
	if len(gates) > 0 {
		ready, err := secrets.GateCredentials(ctx, children, gates, &cr.Status.Conditions, cr.Generation, "SecretsReady")
		if err != nil {
			return ctrl.Result{}, "", "", err
		}
		if !ready {
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, "", "", nil
		}
	}

	// A rotated shared secret has to reach the running pods too: the agent
	// would keep signing with the old value, and Nova would reject every
	// request it proxies.
	var sharedSecretDigest string
	if ref := agentSharedSecretRef(cr); ref != nil {
		value, err := secrets.GetSecretValue(ctx, children,
			client.ObjectKey{Namespace: cr.Namespace, Name: ref.Name}, agentSharedSecretKey(cr))
		if err != nil {
			return ctrl.Result{}, "", "", fmt.Errorf("reading the Nova metadata shared secret value: %w", err)
		}
		sharedSecretDigest = secrets.AdminPasswordDigest(value)
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
			return result, "", "", err
		}
		digest = transportDigest
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               "SecretsReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             "SecretsAvailable",
	})
	return ctrl.Result{}, digest, sharedSecretDigest, nil
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

// agentNovaMetadataCARef returns the Secret reference holding the CA bundle the
// agent verifies the Nova metadata API with, or nil when the agent names none.
func agentNovaMetadataCARef(cr *neutronv1alpha1.NeutronMetadataAgent) *commonv1.SecretRefSpec {
	if cr.Spec.NovaMetadata == nil {
		return nil
	}
	return cr.Spec.NovaMetadata.CABundleSecretRef
}

// agentNovaMetadataCAKey returns the Secret data key the CA bundle is read from,
// defaulting to agentCABundleDefaultKey. The credential gate and the DaemonSet
// volume resolve it through this one function, so a pod never projects a key
// the gate did not check.
func agentNovaMetadataCAKey(cr *neutronv1alpha1.NeutronMetadataAgent) string {
	if ref := agentNovaMetadataCARef(cr); ref != nil && ref.Key != "" {
		return ref.Key
	}
	return agentCABundleDefaultKey
}
