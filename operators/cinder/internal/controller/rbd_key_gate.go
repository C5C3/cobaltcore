// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// gateRBDKeySecret maintains CredentialsReady on conds for the RBD key Secret
// key and reports whether it carries a usable cephx key. Both satellite
// controllers gate on it: an RBD volume backend and an RBD backup target follow
// one credential contract.
//
// The materialized-Secret-then-ExternalSecret ladder is secrets.GateCredential's,
// which sets the False condition with a precise message; the value is then read
// and checked for shape, because a present but empty or malformed key would be
// copied into the projected keyring and fail every connection the driver opens.
// A client error is propagated so the workqueue backs off without demoting a
// standing True; a failed value read is wrapped with subject, which names the
// satellite ("backend \"rbd1\"").
//
// The caller passes the children client: the pods mount the projected keyring
// on the cluster they run on.
func gateRBDKeySecret(ctx context.Context, children client.Client, key client.ObjectKey,
	conds *[]metav1.Condition, generation int64, subject string,
) (bool, error) {
	ready, err := secrets.GateCredential(ctx, children, secrets.CredentialGateSpec{
		Key:          key,
		Reason:       conditionReasonWaitingForCredentials,
		Noun:         "RBD key",
		WaitingMsg:   "waiting for the RBD key Secret to carry the " + cinderv1alpha1.RBDKeySecretDataKey + " data key",
		ExpectedKeys: []string{cinderv1alpha1.RBDKeySecretDataKey},
	}, conds, generation, conditionTypeCredentialsReady)
	if err != nil || !ready {
		return false, err
	}

	value, err := secrets.GetSecretValue(ctx, children, key, cinderv1alpha1.RBDKeySecretDataKey)
	if err != nil {
		return false, fmt.Errorf("reading the RBD key of %s: %w", subject, err)
	}
	if fault := cephxKeyFault(key.Name, strings.TrimSpace(value)); fault != "" {
		conditions.SetCondition(conds, metav1.Condition{
			Type:               conditionTypeCredentialsReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			Reason:             conditionReasonWaitingForCredentials,
			Message:            fault,
		})
		return false, nil
	}
	conditions.SetCondition(conds, metav1.Condition{
		Type:               conditionTypeCredentialsReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             conditionReasonCredentialsAvailable,
		Message: fmt.Sprintf("RBD key Secret %q carries the %s data key",
			key.Name, cinderv1alpha1.RBDKeySecretDataKey),
	})
	return true, nil
}
