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
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// reconcileTransportURLSecret materialises the rabbit:// transport URL into the
// derived <neutron.Name>-transport-url Secret and reports the two values the
// later steps need: the SHA-256 digest of the URL, which the deployment step
// stamps into a pod-template annotation so a rotated broker credential rolls the
// pods, and the broker's TCP port, which the networkpolicy step opens as an
// egress peer.
//
// The shared flow reads the RabbitmqCluster (managed mode) or the brownfield
// Secret and writes the derived Secret through the children client, in the CR's
// own namespace. A placed Neutron whose bus lives on the management cluster
// therefore has to use spec.messaging.secretRef: the helper is same-namespace,
// same-client by design, and the CRD field documents it.
//
// The egress port is derived from the transport URL the flow returns. Both it and
// the digest are zero on the waiting path, where no derived Secret was
// materialised.
func (r *NeutronReconciler) reconcileTransportURLSecret(ctx context.Context, children client.Client,
	neutron *neutronv1alpha1.Neutron,
) (ctrl.Result, string, int32, error) {
	result, transportURL, digest, err := messaging.ReconcileTransportURLSecret(ctx, messaging.TransportURLSecretFlowParams{
		Client:        children,
		Scheme:        r.Scheme,
		Owner:         neutron,
		InstanceName:  neutron.Name,
		Namespace:     neutron.Namespace,
		Messaging:     &neutron.Spec.Messaging,
		Conditions:    &neutron.Status.Conditions,
		Generation:    neutron.Generation,
		ConditionType: "SecretsReady",
		RequeueAfter:  commonreconcile.RequeueSecretPolling,
	})
	if err != nil || digest == "" {
		return result, digest, 0, err
	}

	return result, digest, messaging.EgressPort(transportURL), nil
}
