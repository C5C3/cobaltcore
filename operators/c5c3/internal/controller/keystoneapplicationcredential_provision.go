// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// provisionCredential mints the order's credential and writes CredentialReady.
// Nothing is minted yet.
func (r *KeystoneApplicationCredentialReconciler) provisionCredential(
	_ context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, _ *c5c3v1alpha1.ControlPlane,
	_ string, _ *c5c3v1alpha1.KeystoneUser, _ *c5c3v1alpha1.KeystoneProject,
) (ctrl.Result, error) {
	keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialCredentialReady)(
		reasonKeystoneApplicationCredentialWaiting, "the credential is not minted yet")
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}
