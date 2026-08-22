// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// keystoneServicePushContentHashAnnotation stamps the assembled clouds.yaml
// content hash onto the registration's PushSecret, forcing an immediate re-push
// on a rotation. It is a separate key from the ControlPlane's own push-hash
// annotations so the two mechanisms cannot overwrite each other's value on a
// PushSecret while both exist.
const keystoneServicePushContentHashAnnotation = "c5c3.io/keystoneservice-push-hash" //nolint:gosec // G101 false positive: annotation key, not a credential.

// ensureAccount projects the declared account block: the Keystone identity (a
// domain handle, a project, a probe-gated user with an operator-generated
// password, and the role assignments) and the delivery of its credentials into
// the CR's own namespace through OpenBao.
//
// It is behavior-matched to the ControlPlane's ensureServiceAccount, with two
// differences that follow from the CR shape rather than from a change of policy:
// the owner is the KeystoneService, and a CR carries exactly ONE account, so the
// inline machinery's per-account precedence pass collapses into early returns in
// the same order — collision, terminal error, pending probe, bounded wait.
func (r *KeystoneServiceReconciler) ensureAccount(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference,
) (ctrl.Result, error) {
	fail := keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)
	account := ks.Spec.Account
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}

	// LastPasswordRotation survives a steady-state pass: it is refreshed only on
	// an actual rotation, so carry the recorded value forward before the status is
	// re-projected.
	status := &c5c3v1alpha1.KeystoneServiceAccountStatus{
		SecretName: keystoneServiceCredentialsSecretName(ks),
	}
	if ks.Status.Account != nil {
		status.LastPasswordRotation = ks.Status.Account.LastPasswordRotation
	}
	ks.Status.Account = status

	// Gate on the store the credentials round-trip through, so an ESO or OpenBao
	// outage surfaces promptly rather than at the next hourly refresh.
	storeReady, err := r.keystoneServiceStoreReady(ctx, ks, cp)
	if err != nil {
		fail(reasonServiceAccountError, err.Error())
		return ctrl.Result{}, err
	}
	if !storeReady {
		storeRef := effectiveControlPlaneStoreRef(cp)
		fail(reasonServiceAccountStoreNotReady, fmt.Sprintf(
			"%s %q in namespace %q is not ready; upstream secret backend unreachable",
			storeRef.Kind, storeRef.Name, ks.Namespace,
		))
		return requeue, nil
	}

	userName := keystoneServiceUserName(ks)
	domain := keystoneServiceDomainName(cp, ks)

	// (a) Domain handle.
	domainRef, err := r.ensureKeystoneServiceDomain(ctx, ks, cp, credRef)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("ensuring domain import: %v", err))
		return ctrl.Result{}, err
	}

	// (b) Project, probe-gated when the registration creates it.
	project, proceed, err := r.ensureKeystoneServiceProject(ctx, ks, cp, credRef, managedCredRef, domainRef)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("ensuring project: %v", err))
		return ctrl.Result{}, err
	}
	if !proceed {
		return requeue, nil
	}
	if project.Status.ID != nil {
		status.ProjectID = *project.Status.ID
	}

	// (c) User collision gate.
	proceed, err = r.keystoneServiceUserCollisionGate(ctx, ks, cp, credRef, userName, domainRef)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("probing for a pre-existing user: %v", err))
		return ctrl.Result{}, err
	}
	if !proceed {
		return requeue, nil
	}

	// (d) Managed user with its generation-scoped password.
	user, gen, created, rotatedAt, err := r.ensureKeystoneServiceUser(ctx, ks, cp, managedCredRef, domainRef)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("ensuring the managed user: %v", err))
		return ctrl.Result{}, err
	}
	status.PasswordGeneration = gen
	if user.Status.ID != nil {
		status.UserID = *user.Status.ID
	}
	if rotatedAt != nil {
		status.LastPasswordRotation = rotatedAt
	}
	if created && account.Rotation != nil && account.Rotation.Mode == c5c3v1alpha1.ServiceAccountRotationModeScheduled {
		if r.Recorder != nil {
			r.Recorder.Event(ks, "Normal", "ScheduledRotationDeferred",
				"rotation.mode Scheduled is accepted but not yet implemented; rotate on demand via a CredentialRotation CR")
		}
	}

	// A terminal K-ORC error on either handle fails loud: K-ORC has stopped
	// retrying, so a bounded wait would never resolve.
	for _, obj := range []orcv1alpha1.ObjectWithConditions{project, user} {
		if termErr := orcv1alpha1.GetTerminalError(obj); termErr != nil {
			fail(reasonServiceAccountsFailed, fmt.Sprintf(
				"K-ORC reported a terminal error on the service account: %v", termErr,
			))
			return requeue, nil
		}
	}

	passwordRefName := keystoneServicePasswordSecretName(ks, gen)
	appliedCurrent := user.Status.Resource != nil && user.Status.Resource.AppliedPasswordRef == passwordRefName
	if !korcAvailableUpToDate(project) || !orcv1alpha1.IsAvailable(user) || !appliedCurrent {
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
			reasonWaitingForServiceAccounts, keystoneServiceAccountWaitMessage(project, user),
			pendingServiceAccountObjs(project, user)...)
		return requeue, nil
	}

	// (e) Role assignments, folded into readiness so the account is never reported
	// provisioned before the roles it needs are bound.
	rolesReady, err := r.ensureKeystoneServiceRoles(ctx, ks, cp, credRef, managedCredRef)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("ensuring role assignments: %v", err))
		return ctrl.Result{}, err
	}
	if !rolesReady {
		return requeue, nil
	}

	// (f) Publish: assemble, mirror to OpenBao, materialize, and gate on the
	// materialized password matching the generation K-ORC applied.
	published, err := r.publishKeystoneServiceAccount(ctx, ks, cp, userName, account.Project.Name, domain, gen)
	if err != nil {
		fail(reasonServiceAccountError, fmt.Sprintf("delivering the account credentials: %v", err))
		return ctrl.Result{}, err
	}
	if !published {
		fail(reasonWaitingForServiceAccounts,
			"the service account is provisioned in Keystone; awaiting the OpenBao round-trip and the materialized Secret")
		return requeue, nil
	}

	keystoneServiceSetTrue(ks, conditionTypeKeystoneServiceAccountReady, reasonKeystoneServiceAccountProvisioned,
		fmt.Sprintf("service account %q is provisioned in domain %q and its credentials are materialized in Secret %q",
			userName, domain, keystoneServiceCredentialsSecretName(ks)))
	return ctrl.Result{}, nil
}

// ensureKeystoneServiceDomain ensures the Domain handle the user and project
// resolve against, and returns the CR name to reference. When the effective
// domain is the ControlPlane's admin domain it reuses the admin import and
// creates nothing.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceDomain(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef orcv1alpha1.CloudCredentialsReference,
) (string, error) {
	name := keystoneServiceDomainRef(cp, ks)
	if name == adminDomainRef(cp) {
		return name, nil
	}
	if err := ensureAccountDomainImport(ctx,
		name, keystoneServiceChildNamespace(cp), keystoneServiceDomainName(cp, ks), credRef,
		r.keystoneServiceEnsure(ks)); err != nil {
		return "", fmt.Errorf("registration Domain import %q: %w", name, err)
	}
	return name, nil
}

// ensureKeystoneServiceProject ensures the project handle. project.create=false
// REFERENCES a pre-existing project through an unmanaged import the operator
// never creates or deletes; create=true CREATES a managed Project, gated by the
// same fail-loudly probe as the user, because K-ORC's managed create would
// silently adopt a same-named project.
//
// proceed is false — with the blocking condition already written — when a
// collision or an unresolved probe stops the account.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceProject(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference, domainRef string,
) (*orcv1alpha1.Project, bool, error) {
	name := keystoneServiceProjectRef(ks)
	account := ks.Spec.Account
	domainName := keystoneServiceDomainName(cp, ks)

	if !account.Project.Create {
		project := unmanagedProjectImport(name, keystoneServiceChildNamespace(cp),
			account.Project.Name, domainRef, credRef)
		if err := r.ensureKeystoneServiceChild(ctx, ks, project); err != nil {
			return nil, false, fmt.Errorf("registration referenced Project import %q: %w", name, err)
		}
		return project, true, nil
	}

	// create:true — probe before creating a managed Project, unless we already own
	// one, in which case the verdict is settled. adopt is never set here: project
	// takeover is expressed as account.project.create=false.
	probe := unmanagedProjectImport(keystoneServiceProjectProbeRef(ks), keystoneServiceChildNamespace(cp),
		account.Project.Name, domainRef, credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:        "Project",
		managed:     &orcv1alpha1.Project{},
		managedName: name,
		namespace:   keystoneServiceChildNamespace(cp),
		probe:       probe,
		ensure:      r.keystoneServiceEnsure(ks),
	})
	if err != nil {
		return nil, false, err
	}
	if !proceed {
		switch verdict {
		case probeResolved:
			keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountCollision, fmt.Sprintf(
				"the registration wants to CREATE project %q in domain %q, but a project of that name already exists "+
					"in Keystone; the operator will not silently adopt it — set account.project.create=false to "+
					"reference the existing project instead",
				account.Project.Name, domainName,
			))
		case probePending:
			r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
				reasonProbingForCollision,
				fmt.Sprintf("probing whether project %q already exists in Keystone before creating it", account.Project.Name),
				probe)
		case probeAbsent:
			// Unreachable: an absent probe proceeds.
		}
		return probe, false, nil
	}

	project := managedProjectChild(name, keystoneServiceChildNamespace(cp),
		account.Project.Name, domainRef, managedCredRef)
	if err := r.ensureKeystoneServiceChild(ctx, ks, project); err != nil {
		return nil, false, fmt.Errorf("registration managed Project %q: %w", name, err)
	}
	return project, true, nil
}

// keystoneServiceUserCollisionGate implements the fail-loudly default for a
// pre-existing Keystone user. K-ORC's managed create silently adopts a same-named
// user, so a short-lived unmanaged probe decides exists/absent before any managed
// User is created.
//
// proceed is true when the managed User may be created: the operator already owns
// it, adoption was requested, or the probe confirmed the user is absent.
func (r *KeystoneServiceReconciler) keystoneServiceUserCollisionGate(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef orcv1alpha1.CloudCredentialsReference, userName, domainRef string,
) (bool, error) {
	probe := unmanagedUserImport(keystoneServiceUserProbeRef(ks), keystoneServiceChildNamespace(cp),
		userName, domainRef, credRef)
	proceed, verdict, err := managedChildProbeGate(ctx, r.Client, managedChildProbeInput{
		kind:             "User",
		managed:          &orcv1alpha1.User{},
		managedName:      keystoneServiceUserRef(ks),
		namespace:        keystoneServiceChildNamespace(cp),
		adopt:            ks.Spec.Account.Adopt,
		probe:            probe,
		dropProbeOnOwned: true,
		ensure:           r.keystoneServiceEnsure(ks),
	})
	if err != nil || proceed {
		return proceed, err
	}
	switch verdict {
	case probeResolved:
		keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountCollision, fmt.Sprintf(
			"the registration resolves to Keystone user %q in domain %q, which already exists; the operator fails "+
				"loudly rather than take over an account it did not create — set account.adopt=true to take over "+
				"the account (and rotate its password), or remove the account block",
			userName, keystoneServiceDomainName(cp, ks),
		))
	case probePending:
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
			reasonProbingForCollision,
			fmt.Sprintf("probing whether Keystone user %q already exists before creating the service account", userName),
			probe)
	case probeAbsent:
		// Unreachable: an absent probe proceeds.
	}
	return false, nil
}

// ensureKeystoneServiceUser create-or-updates the managed K-ORC User with the
// current-generation password, and flips the passwordRef to a fresh generation
// when a CredentialRotation has cleared the generation annotation. K-ORC's user
// actuator re-applies the password only when the passwordRef NAME changes, so a
// rotation is a Secret-name flip and never an in-place edit.
//
// It stays read-modify-write rather than Server-Side Apply because the mutation
// reads the LIVE User's CreationTimestamp and annotation to decide whether to
// re-stamp the generation, which no pure projection of the spec can express.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceUser(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	managedCredRef orcv1alpha1.CloudCredentialsReference, domainRef string,
) (*orcv1alpha1.User, int64, bool, *metav1.Time, error) {
	return ensureManagedAccountUser(ctx, r.Client, managedAccountUserInput{
		name:           keystoneServiceUserRef(ks),
		namespace:      keystoneServiceChildNamespace(cp),
		userName:       keystoneServiceUserName(ks),
		domainRef:      domainRef,
		projectRef:     keystoneServiceProjectRef(ks),
		managedCredRef: managedCredRef,
		passwordSecretNameFor: func(gen int64) string {
			return keystoneServicePasswordSecretName(ks, gen)
		},
		ensurePasswordSecret: func(ctx context.Context, gen int64) error {
			return r.ensureKeystoneServicePasswordSecret(ctx, ks, cp, gen)
		},
		passwordSecretPrefix: keystoneServiceChildPrefix(ks) + "password-v",
		ownsChild:            func(obj client.Object) bool { return ownsKeystoneServiceChild(ks, obj) },
		claim: func(obj client.Object) error {
			return claimKeystoneServiceChild(ks, obj, r.Scheme)
		},
	})
}

// ensureKeystoneServicePasswordSecret ensures the generation-scoped, operator-owned
// Secret holding the K-ORC-facing password. The value is generated ONCE and
// preserved: a new generation is a new Secret name, never an in-place edit, which
// is what lets K-ORC detect the rotation at all.
func (r *KeystoneServiceReconciler) ensureKeystoneServicePasswordSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane, gen int64,
) error {
	name := keystoneServicePasswordSecretName(ks, gen)
	if err := r.ensureKeystoneServiceSecret(ctx, ks, name,
		keystoneServiceChildNamespace(cp), generatedPasswordMutator); err != nil {
		return fmt.Errorf("ensuring registration password Secret %q: %w", name, err)
	}
	return nil
}

// ensureKeystoneServiceRoles projects the declared roles and reports whether every
// projected object is Available. Per role it applies an UNMANAGED Role import
// filtered by name (Keystone roles are global, so the import carries no domain and
// reading one never mutates it) and a MANAGED RoleAssignment binding that role to
// this account's user on its project.
//
// Unlike the ControlPlane's inline accounts, which share one Role import per
// ControlPlane, the imports here are per CR: registrations are independently
// owned and deleted, so a shared import would have no single owner to tear it
// down and the first CR deleted would pull it out from under the others.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceRoles(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	credRef, managedCredRef orcv1alpha1.CloudCredentialsReference,
) (bool, error) {
	ready := true
	var pending []orcv1alpha1.ObjectWithConditions
	waitMessage := ""

	for _, role := range ks.Spec.Account.Roles {
		out, err := applyAccountRole(ctx,
			keystoneServiceRoleImportRef(ks, role), keystoneServiceRoleAssignmentRef(ks, role),
			keystoneServiceChildNamespace(cp), role,
			credRef, managedCredRef, keystoneServiceUserRef(ks), keystoneServiceProjectRef(ks),
			r.keystoneServiceEnsure(ks))
		if err != nil {
			return false, err
		}
		if termErr := out.terminalErr; termErr != nil {
			keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountsFailed, fmt.Sprintf(
				"K-ORC reported a terminal error on role %q: %v", role, termErr,
			))
			return false, nil
		}
		if out.blocked == nil {
			continue
		}
		ready = false
		pending = append(pending, out.blocked)
		if waitMessage != "" {
			continue
		}
		if _, isImport := out.blocked.(*orcv1alpha1.Role); isImport {
			waitMessage = fmt.Sprintf("the Role import for role %q is registered but not yet Available", role)
			if out.stalled {
				waitMessage += fmt.Sprintf("; role %q may not exist in Keystone — create it there or fix the spelling", role)
			}
			continue
		}
		waitMessage = fmt.Sprintf("the RoleAssignment for role %q is registered but not yet Available", role)
	}

	if !ready {
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
			reasonWaitingForServiceAccounts, waitMessage, pending...)
	}
	return ready, nil
}

// publishKeystoneServiceAccount assembles the source Secret, mirrors it to the
// per-CR OpenBao path, materializes the consumer Secret, and reports whether the
// materialized password matches the current generation — so a rotated-away
// password never reads ready. It runs only once K-ORC confirms the current
// password is applied to the user.
//
// Everything lands in the CR's own namespace, on the management cluster: a
// registration's consumer is the workload beside it.
func (r *KeystoneServiceReconciler) publishKeystoneServiceAccount(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
	userName, projectName, domain string, gen int64,
) (bool, error) {
	sourceName := keystoneServiceSourceSecretName(ks)
	return publishAccountCredentials(ctx, cp, accountPublication{
		management:         r.Client,
		delivery:           r.Client,
		childNamespace:     keystoneServiceChildNamespace(cp),
		deliveryNamespace:  ks.Namespace,
		passwordSecretName: keystoneServicePasswordSecretName(ks, gen),

		userName:    userName,
		projectName: projectName,
		domain:      domain,
		// nil target-cluster ref: a registration is delivered on the management
		// cluster, so the document resolves the in-cluster auth URL.
		deliveryRef: nil,

		sourceSecretName:      sourceName,
		credentialsSecretName: keystoneServiceCredentialsSecretName(ks),
		pushSecret:            keystoneServicePushSecret(ks, cp),
		pushHashAnnotation:    keystoneServicePushContentHashAnnotation,

		ensureSourceSecret: func(ctx context.Context, mutate func(*corev1.Secret) error) error {
			return r.ensureKeystoneServiceSecret(ctx, ks, sourceName, ks.Namespace, mutate)
		},
		ensurePushSecret: func(ctx context.Context, ps *esov1alpha1.PushSecret) error {
			return secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, ks, ps)
		},
		ensureExternalSecret: func(ctx context.Context) error {
			return r.ensureKeystoneServiceExternalSecret(ctx, ks, cp)
		},
	})
}

// keystoneServicePushSecret builds the PushSecret mirroring the source Secret to
// the per-CR OpenBao path. DeletionPolicy Delete: the credential dies with the
// registration that owns it.
func keystoneServicePushSecret(ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	return accountPushSecretSpec(
		keystoneServicePushSecretName(ks), ks.Namespace,
		keystoneServiceSourceSecretName(ks), keystoneServiceRemoteKeyFor(ks),
		effectiveControlPlaneStoreRef(cp),
	)
}

// ensureKeystoneServiceExternalSecret create-or-updates the ExternalSecret that
// materializes the consumer Secret from the per-CR OpenBao path. It reads back
// the "password" and "clouds.yaml" properties — the documented consumption
// contract a service reads its credentials through.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceExternalSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
) error {
	name := keystoneServiceCredentialsSecretName(ks)
	es := accountExternalSecretSpec(name, ks.Namespace,
		keystoneServiceRemoteKeyFor(ks), effectiveControlPlaneStoreRef(cp))
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, es, apply.FieldManager); err != nil {
		return fmt.Errorf("ensuring the registration ExternalSecret %q: %w", name, err)
	}
	return nil
}

// keystoneServiceAccountWaitMessage names the stuck dependency in dependency
// order, so a bounded wait points at the real blocker rather than at whatever is
// merely waiting behind it.
func keystoneServiceAccountWaitMessage(project *orcv1alpha1.Project, user *orcv1alpha1.User) string {
	switch {
	case project != nil && !korcAvailableUpToDate(project):
		return "the account's project is registered but not yet Available"
	case !orcv1alpha1.IsAvailable(user):
		return "the account's user is registered but not yet Available"
	default:
		return "awaiting K-ORC to apply the current password to the user"
	}
}
