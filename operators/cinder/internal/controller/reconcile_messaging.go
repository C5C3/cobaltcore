// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// reconcileTransportURLSecret materialises the rabbit:// transport URL into the
// derived <cinder.Name>-transport-url Secret and reports the two values the
// later steps need: the SHA-256 digest of the URL, which the deployment steps
// stamp into a pod-template annotation so a rotated broker credential rolls the
// pods, and the broker's TCP port, which the networkpolicy step opens as an
// egress peer.
//
// The bus carries every volume request past the API: the API hands it to the
// scheduler, the scheduler to a cinder-volume, and the backup service takes its
// jobs the same way, so all four workloads read this one Secret.
//
// The shared flow reads the RabbitmqCluster (managed mode) or the brownfield
// Secret and writes the derived Secret through the children client, in the CR's
// own namespace. A placed Cinder whose bus lives on the management cluster
// therefore has to use spec.messaging.secretRef: the helper is same-namespace,
// same-client by design, and the CRD field documents it.
//
// The egress port is derived from the transport URL the flow returns. Both it
// and the digest are zero on the waiting path, where no derived Secret was
// materialised.
func (r *CinderReconciler) reconcileTransportURLSecret(ctx context.Context, children client.Client,
	cinder *cinderv1alpha1.Cinder,
) (ctrl.Result, string, int32, error) {
	result, transportURL, digest, err := messaging.ReconcileTransportURLSecret(ctx, messaging.TransportURLSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         cinder,
		InstanceName:  cinder.Name,
		Namespace:     cinder.Namespace,
		Messaging:     &cinder.Spec.Messaging,
		Conditions:    &cinder.Status.Conditions,
		Generation:    cinder.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
	if err != nil || digest == "" {
		return result, digest, 0, err
	}

	return result, digest, messaging.EgressPort(transportURL), nil
}
