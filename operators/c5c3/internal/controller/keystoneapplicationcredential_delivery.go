// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// deliverCredential writes the Secret beside the order and DeliveryReady.
// Nothing is delivered yet.
func (r *KeystoneApplicationCredentialReconciler) deliverCredential(
	_ context.Context, _ client.Client, order *c5c3v1alpha1.KeystoneApplicationCredential,
	_ *c5c3v1alpha1.ControlPlane, _ string,
) (ctrl.Result, error) {
	keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialDeliveryReady)(
		reasonKeystoneApplicationCredentialWaiting, "the credential is not minted yet; nothing is delivered")
	return ctrl.Result{}, nil
}
