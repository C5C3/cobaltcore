// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// provisionUser creates the order's Keystone user through K-ORC in the
// ControlPlane's namespace and writes UserReady. ok is true once K-ORC reports
// the user Available with the current password generation applied; the
// generation is then in status.passwordGeneration. A false ok carries the
// condition explaining it and the result to return.
//
// The user is created in the ControlPlane's admin domain with no project. The
// outcomes are a chain of early returns: a reserved name, the store, the
// collision probe, a terminal K-ORC error, the wait.
func (r *KeystoneUserReconciler) provisionUser(
	ctx context.Context, order *c5c3v1alpha1.KeystoneUser, cp *c5c3v1alpha1.ControlPlane, cluster string,
) (bool, ctrl.Result, error) {
	fail := keystoneUserFail(order, conditionTypeKeystoneUserUserReady)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	userName := keystoneUserName(order)
	domain := adminDomainName(cp)

	// An order may never resolve to a user the ControlPlane creates in the admin
	// domain itself. For a live account the managed User would take it over,
	// rotate its password into the order's Secret, and delete it from Keystone at
	// teardown; a name taken before its service is enabled fails that service's
	// registration on ServiceAccountCollision. The domain is always the admin
	// domain, so the user name alone decides. Only an edit of the ControlPlane
	// clears the admin case, so there is no requeue.
	if slices.ContainsFunc(keystoneUserReservedNames(cp), func(name string) bool {
		return keystoneNameKey(name) == keystoneNameKey(userName)
	}) {
		fail(reasonServiceAccountCollision, fmt.Sprintf(
			"the order resolves to Keystone user %q in domain %q, which ControlPlane %s/%s reserves for its "+
				"admin identity or a built-in service account; it is never provisioned",
			userName, domain, cp.Namespace, cp.Name))
		return false, ctrl.Result{}, nil
	}

	// The password is backed up through the ControlPlane namespace's own store,
	// so an ESO or OpenBao outage surfaces here rather than at the delivery.
	storeRef := effectiveControlPlaneStoreRef(cp)
	ready, err := secrets.IsStoreRefReady(ctx, r.Client, storeRef, cp.Namespace)
	if err != nil {
		err = fmt.Errorf("checking %s %q in namespace %q: %w", storeRef.Kind, storeRef.Name, cp.Namespace, err)
		fail(reasonServiceAccountError, err.Error())
		return false, ctrl.Result{}, err
	}
	if !ready {
		fail(reasonServiceAccountStoreNotReady, fmt.Sprintf(
			"%s %q in namespace %q is not ready; the password cannot be backed up to OpenBao",
			storeRef.Kind, storeRef.Name, cp.Namespace))
		return false, requeue, nil
	}

	// K-ORC's managed create takes over a same-named user instead of failing, so
	// a probe decides first. adopt is never set: an order never takes a user
	// over.
	credRef, managedCredRef := keystoneServiceCredentialRefs(cp)
	probe := unmanagedUserImport(keystoneUserUserProbeRef(order, cluster), cp.Namespace,
		userName, adminDomainRef(cp), credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:             "User",
		managed:          &orcv1alpha1.User{},
		managedName:      keystoneUserUserRef(order, cluster),
		namespace:        cp.Namespace,
		probe:            probe,
		dropProbeOnOwned: true,
		ensure:           r.keystoneUserEnsure(order, cluster),
	})
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("probing for a pre-existing user: %v", err))
		return false, ctrl.Result{}, err
	}
	if !proceed {
		switch verdict {
		case probeResolved:
			fail(reasonServiceAccountCollision, fmt.Sprintf(
				"Keystone user %q in domain %q already exists; the order never takes over a user it did not "+
					"create; pick another userName or delete the order", userName, domain))
		case probePending:
			waitOrClassifyCondition(cp, fail, reasonProbingForCollision,
				fmt.Sprintf("probing whether Keystone user %q already exists before creating it", userName), probe)
		case probeAbsent:
			// Unreachable: an absent probe proceeds.
		}
		return false, requeue, nil
	}

	user, gen, _, rotatedAt, err := ensureManagedAccountUser(ctx, r.Client, managedAccountUserInput{
		name:           keystoneUserUserRef(order, cluster),
		namespace:      cp.Namespace,
		userName:       userName,
		domainRef:      adminDomainRef(cp),
		managedCredRef: managedCredRef,
		passwordSecretNameFor: func(gen int64) string {
			return keystoneUserPasswordSecretName(order, cluster, gen)
		},
		ensurePasswordSecret: func(ctx context.Context, gen int64) error {
			name := keystoneUserPasswordSecretName(order, cluster, gen)
			if err := r.ensureKeystoneUserSecret(ctx, order, cluster, name, cp.Namespace,
				generatedPasswordMutator); err != nil {
				return fmt.Errorf("ensuring order password Secret %q: %w", name, err)
			}
			return nil
		},
		passwordSecretPrefix:   keystoneUserPasswordSecretPrefix(order, cluster),
		passwordSecretSelector: keystoneUserChildLabels(order, cluster),
		ownsChild:              func(obj client.Object) bool { return ownsKeystoneUserChild(obj, order, cluster) },
		claim: func(obj client.Object) error {
			claimKeystoneUserChild(obj, order, cluster)
			return nil
		},
		desiredGeneration: keystoneUserPasswordGeneration(order),
	})
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("ensuring the managed user: %v", err))
		return false, ctrl.Result{}, err
	}
	if user.Status.ID != nil {
		order.Status.UserID = *user.Status.ID
	}
	// The rotation time is recorded on the pass that re-points the password, which
	// is ahead of the one that sees it applied.
	if rotatedAt != nil {
		order.Status.LastPasswordRotation = rotatedAt
	}

	// A latched transport error is handed back to K-ORC first; korc_unlatch.go
	// states the policy.
	if err := unlatchKORCTransportErrors(ctx, r.Client, user); err != nil {
		fail(conditionReasonTransportErrorRetryFailed, err.Error())
		return false, ctrl.Result{}, err
	}
	if termErr := orcv1alpha1.GetTerminalError(user); termErr != nil {
		fail(reasonServiceAccountsFailed, fmt.Sprintf("K-ORC reported a terminal error on the user: %v", termErr))
		return false, requeue, nil
	}

	applied := user.Status.Resource != nil &&
		user.Status.Resource.AppliedPasswordRef == keystoneUserPasswordSecretName(order, cluster, gen)
	if !orcv1alpha1.IsAvailable(user) || !applied {
		message := "the user is registered but not yet Available"
		if orcv1alpha1.IsAvailable(user) {
			message = fmt.Sprintf("awaiting K-ORC to apply password generation %d to the user", gen)
		}
		waitOrClassifyCondition(cp, fail, reasonWaitingForServiceAccounts, message,
			pendingServiceAccountObjs(user)...)
		return false, requeue, nil
	}

	order.Status.PasswordGeneration = gen
	keystoneUserSetTrue(order, conditionTypeKeystoneUserUserReady, reasonKeystoneUserProvisioned, fmt.Sprintf(
		"Keystone user %q is provisioned in domain %q at password generation %d", userName, domain, gen))
	return true, ctrl.Result{}, nil
}

// keystoneUserReservedNames are the Keystone users a ControlPlane creates in its
// admin domain, where every order's user is created as well: the admin identity
// and the accounts of the built-in service registrations. A service's account is
// reserved whether or not the service is enabled yet.
func keystoneUserReservedNames(cp *c5c3v1alpha1.ControlPlane) []string {
	return []string{
		adminUserName(cp),
		c5c3v1alpha1.GlanceServiceAccountName,
		c5c3v1alpha1.PlacementServiceAccountName,
		c5c3v1alpha1.BarbicanServiceAccountName,
		c5c3v1alpha1.NeutronServiceAccountName,
		c5c3v1alpha1.CinderServiceAccountName,
		c5c3v1alpha1.NovaServiceAccountName,
		c5c3v1alpha1.NeutronNovaNotifierAccountName,
		c5c3v1alpha1.NovaHypervisorOperatorAccountName,
	}
}
