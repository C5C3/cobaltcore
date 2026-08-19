// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/forge/internal/common/apply"
	"github.com/c5c3/forge/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
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
	user, gen, created, rotatedAt, err := r.ensureKeystoneServiceUser(ctx, ks, managedCredRef, domainRef)
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
	domain := &orcv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace},
		Spec: orcv1alpha1.DomainSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.DomainImport{
				Filter: &orcv1alpha1.DomainFilter{Name: ptr.To(orcv1alpha1.KeystoneName(keystoneServiceDomainName(cp, ks)))},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, domain, apply.FieldManager); err != nil {
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
		project := &orcv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace},
			Spec: orcv1alpha1.ProjectSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
				CloudCredentialsRef: credRef,
				Import: &orcv1alpha1.ProjectImport{
					Filter: &orcv1alpha1.ProjectFilter{
						Name:      ptr.To(orcv1alpha1.KeystoneName(account.Project.Name)),
						DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
					},
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, project, apply.FieldManager); err != nil {
			return nil, false, fmt.Errorf("registration referenced Project import %q: %w", name, err)
		}
		return project, true, nil
	}

	// create:true — probe before creating a managed Project, unless we already own
	// one, in which case the verdict is settled and the probe is dropped.
	managed := &orcv1alpha1.Project{}
	switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ks.Namespace}, managed); {
	case err == nil:
		// Already ours: converge below.
	case apierrors.IsNotFound(err):
		probe := &orcv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: keystoneServiceProjectProbeRef(ks), Namespace: ks.Namespace},
			Spec: orcv1alpha1.ProjectSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
				CloudCredentialsRef: credRef,
				Import: &orcv1alpha1.ProjectImport{
					Filter: &orcv1alpha1.ProjectFilter{
						Name:      ptr.To(orcv1alpha1.KeystoneName(account.Project.Name)),
						DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
					},
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, probe, apply.FieldManager); err != nil {
			return nil, false, fmt.Errorf("registration Project probe %q: %w", probe.Name, err)
		}
		switch interpretProbe(probe) {
		case probeResolved:
			keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountCollision, fmt.Sprintf(
				"the registration wants to CREATE project %q in domain %q, but a project of that name already exists "+
					"in Keystone; the operator will not silently adopt it — set account.project.create=false to "+
					"reference the existing project instead",
				account.Project.Name, domainName,
			))
			return probe, false, nil
		case probePending:
			r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
				reasonProbingForCollision,
				fmt.Sprintf("probing whether project %q already exists in Keystone before creating it", account.Project.Name),
				probe)
			return probe, false, nil
		case probeAbsent:
			// Safe to create; drop the probe and fall through.
		}
		if err := deleteRegistrationChild(ctx, r.Client, &orcv1alpha1.Project{}, keystoneServiceProjectProbeRef(ks), ks.Namespace, "registration"); err != nil {
			return nil, false, err
		}
	default:
		return nil, false, fmt.Errorf("reading managed Project %q: %w", name, err)
	}

	project := &orcv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace},
		Spec: orcv1alpha1.ProjectSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: managedCredRef,
			Resource: &orcv1alpha1.ProjectResourceSpec{
				Name:      ptr.To(orcv1alpha1.KeystoneName(account.Project.Name)),
				DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, project, apply.FieldManager); err != nil {
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
	probeName := keystoneServiceUserProbeRef(ks)

	existing := &orcv1alpha1.User{}
	switch err := r.Get(ctx, types.NamespacedName{Name: keystoneServiceUserRef(ks), Namespace: ks.Namespace}, existing); {
	case err == nil:
		// The managed User is already ours; no probe is needed any more.
		return true, deleteRegistrationChild(ctx, r.Client, &orcv1alpha1.User{}, probeName, ks.Namespace, "registration")
	case apierrors.IsNotFound(err):
	default:
		return false, fmt.Errorf("reading managed User %q: %w", keystoneServiceUserRef(ks), err)
	}

	// adopt: take over the pre-existing user (and rotate its password) without
	// probing at all.
	if ks.Spec.Account.Adopt {
		return true, deleteRegistrationChild(ctx, r.Client, &orcv1alpha1.User{}, probeName, ks.Namespace, "registration")
	}

	probe := &orcv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: probeName, Namespace: ks.Namespace},
		Spec: orcv1alpha1.UserSpec{
			ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
			CloudCredentialsRef: credRef,
			Import: &orcv1alpha1.UserImport{
				Filter: &orcv1alpha1.UserFilter{
					Name:      ptr.To(orcv1alpha1.OpenStackName(userName)),
					DomainRef: ptr.To(orcv1alpha1.KubernetesNameRef(domainRef)),
				},
			},
		},
	}
	if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, probe, apply.FieldManager); err != nil {
		return false, fmt.Errorf("registration User probe %q: %w", probeName, err)
	}

	switch interpretProbe(probe) {
	case probeResolved:
		keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountCollision, fmt.Sprintf(
			"the registration resolves to Keystone user %q in domain %q, which already exists; the operator fails "+
				"loudly rather than take over an account it did not create — set account.adopt=true to take over "+
				"the account (and rotate its password), or remove the account block",
			userName, keystoneServiceDomainName(cp, ks),
		))
		return false, nil
	case probePending:
		r.keystoneServiceWaitOrClassify(ks, cp, conditionTypeKeystoneServiceAccountReady,
			reasonProbingForCollision,
			fmt.Sprintf("probing whether Keystone user %q already exists before creating the service account", userName),
			probe)
		return false, nil
	case probeAbsent:
		// The user does not exist; drop the probe and create the managed User.
	}
	return true, deleteRegistrationChild(ctx, r.Client, &orcv1alpha1.User{}, probeName, ks.Namespace, "registration")
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
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService,
	managedCredRef orcv1alpha1.CloudCredentialsReference, domainRef string,
) (*orcv1alpha1.User, int64, bool, *metav1.Time, error) {
	name := keystoneServiceUserRef(ks)
	userName := keystoneServiceUserName(ks)

	existing := &orcv1alpha1.User{}
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ks.Namespace}, existing)
	exists := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, 0, false, nil, fmt.Errorf("reading managed User %q: %w", name, getErr)
	}

	var currentGen int64
	if exists && existing.Spec.Resource != nil && existing.Spec.Resource.PasswordRef != nil {
		if g, ok := parseServiceAccountGeneration(string(*existing.Spec.Resource.PasswordRef)); ok {
			currentGen = g
		}
	}
	if currentGen < 1 {
		currentGen = 1
	}
	// An empty generation annotation is the CredentialRotation reconciler's
	// rotation nudge.
	rotating := false
	if exists {
		if v, present := existing.Annotations[serviceAccountPasswordGenerationAnnotation]; present && v == "" {
			rotating = true
		}
	}
	desiredGen := currentGen
	if !exists {
		desiredGen = 1
	} else if rotating {
		desiredGen = currentGen + 1
	}

	if err := r.ensureKeystoneServicePasswordSecret(ctx, ks, desiredGen); err != nil {
		return nil, 0, false, nil, err
	}
	passwordRefName := keystoneServicePasswordSecretName(ks, desiredGen)

	user := &orcv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, user, func() error {
		user.Spec.ManagementPolicy = orcv1alpha1.ManagementPolicyManaged
		user.Spec.Import = nil
		user.Spec.CloudCredentialsRef = managedCredRef
		if user.Spec.Resource == nil {
			user.Spec.Resource = &orcv1alpha1.UserResourceSpec{}
		}
		user.Spec.Resource.Name = ptr.To(orcv1alpha1.OpenStackName(userName))
		user.Spec.Resource.DomainRef = ptr.To(orcv1alpha1.KubernetesNameRef(domainRef))
		user.Spec.Resource.DefaultProjectRef = ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServiceProjectRef(ks)))
		user.Spec.Resource.PasswordRef = ptr.To(orcv1alpha1.KubernetesNameRef(passwordRefName))
		if user.Annotations == nil {
			user.Annotations = map[string]string{}
		}
		// Stamp the generation on a fresh create or a rotation. In the steady state
		// stamp only when the key is ABSENT (re-derive): a present-but-empty value is
		// a rotation nudge this pass did not observe, so leaving it makes the NEXT
		// pass rotate rather than losing the request.
		if _, present := user.Annotations[serviceAccountPasswordGenerationAnnotation]; user.CreationTimestamp.IsZero() || rotating || !present {
			user.Annotations[serviceAccountPasswordGenerationAnnotation] = strconv.FormatInt(desiredGen, 10)
		}
		return controllerutil.SetControllerReference(ks, user, r.Scheme)
	})
	if err != nil {
		return nil, 0, false, nil, fmt.Errorf("registration managed User %q: %w", name, err)
	}

	var rotatedAt *metav1.Time
	if rotating {
		now := metav1.Now()
		rotatedAt = &now
	}
	// Prune superseded generations only once K-ORC confirms the current one is
	// applied, and only the ones that actually exist: a blind delete of v1..v(N-1)
	// on every steady-state pass would issue a growing stream of NotFound round
	// trips that never converges.
	if user.Status.Resource != nil && user.Status.Resource.AppliedPasswordRef == passwordRefName {
		var pwSecrets corev1.SecretList
		if err := r.List(ctx, &pwSecrets, client.InNamespace(ks.Namespace)); err != nil {
			return nil, 0, false, nil, fmt.Errorf("listing superseded password Secrets: %w", err)
		}
		prefix := keystoneServiceChildPrefix(ks) + "password-v"
		for i := range pwSecrets.Items {
			secretName := pwSecrets.Items[i].Name
			if g, ok := parseServiceAccountGeneration(secretName); ok &&
				strings.HasPrefix(secretName, prefix) && g < desiredGen &&
				ownsKeystoneServiceChild(ks, &pwSecrets.Items[i]) {
				if err := deleteRegistrationChild(ctx, r.Client, &corev1.Secret{}, secretName, ks.Namespace, "registration"); err != nil {
					return nil, 0, false, nil, err
				}
			}
		}
	}
	return user, desiredGen, op == controllerutil.OperationResultCreated, rotatedAt, nil
}

// ensureKeystoneServicePasswordSecret ensures the generation-scoped, operator-owned
// Secret holding the K-ORC-facing password. The value is generated ONCE and
// preserved: a new generation is a new Secret name, never an in-place edit, which
// is what lets K-ORC detect the rotation at all.
func (r *KeystoneServiceReconciler) ensureKeystoneServicePasswordSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, gen int64,
) error {
	name := keystoneServicePasswordSecretName(ks, gen)
	if err := r.ensureKeystoneServiceSecret(ctx, ks, name, func(secret *corev1.Secret) error {
		if len(secret.Data[serviceAccountPasswordKey]) == 0 {
			v, gerr := generateAppCredSecretValue()
			if gerr != nil {
				return gerr
			}
			secret.Data[serviceAccountPasswordKey] = []byte(v)
		}
		return nil
	}); err != nil {
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
		roleImportName := keystoneServiceRoleImportRef(ks, role)
		assignmentName := keystoneServiceRoleAssignmentRef(ks, role)

		roleObj := &orcv1alpha1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: roleImportName, Namespace: ks.Namespace},
			Spec: orcv1alpha1.RoleSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyUnmanaged,
				CloudCredentialsRef: credRef,
				Import: &orcv1alpha1.RoleImport{
					Filter: &orcv1alpha1.RoleFilter{Name: ptr.To(orcv1alpha1.KeystoneName(role))},
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, roleObj, apply.FieldManager); err != nil {
			return false, fmt.Errorf("registration Role import %q: %w", roleImportName, err)
		}

		assignment := &orcv1alpha1.RoleAssignment{
			ObjectMeta: metav1.ObjectMeta{Name: assignmentName, Namespace: ks.Namespace},
			Spec: orcv1alpha1.RoleAssignmentSpec{
				ManagementPolicy:    orcv1alpha1.ManagementPolicyManaged,
				CloudCredentialsRef: managedCredRef,
				Resource: &orcv1alpha1.RoleAssignmentResourceSpec{
					RoleRef:    orcv1alpha1.KubernetesNameRef(roleImportName),
					UserRef:    ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServiceUserRef(ks))),
					ProjectRef: ptr.To(orcv1alpha1.KubernetesNameRef(keystoneServiceProjectRef(ks))),
				},
			},
		}
		if err := apply.EnsureObject(ctx, r.Client, r.Scheme, ks, assignment, apply.FieldManager); err != nil {
			return false, fmt.Errorf("registration RoleAssignment %q: %w", assignmentName, err)
		}

		for _, obj := range []orcv1alpha1.ObjectWithConditions{roleObj, assignment} {
			if termErr := orcv1alpha1.GetTerminalError(obj); termErr != nil {
				keystoneServiceFail(ks, conditionTypeKeystoneServiceAccountReady)(reasonServiceAccountsFailed, fmt.Sprintf(
					"K-ORC reported a terminal error on role %q: %v", role, termErr,
				))
				return false, nil
			}
		}

		if !korcAvailableUpToDate(roleObj) {
			ready = false
			pending = append(pending, roleObj)
			if waitMessage == "" {
				waitMessage = fmt.Sprintf("the Role import for role %q is registered but not yet Available", role)
				if korcImportStalled(roleObj, externalImportStallGrace) {
					waitMessage += fmt.Sprintf("; role %q may not exist in Keystone — create it there or fix the spelling", role)
				}
			}
			continue
		}
		if !korcAvailableUpToDate(assignment) {
			ready = false
			pending = append(pending, assignment)
			if waitMessage == "" {
				waitMessage = fmt.Sprintf("the RoleAssignment for role %q is registered but not yet Available", role)
			}
		}
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
	pwSecret := &corev1.Secret{}
	pwKey := types.NamespacedName{Name: keystoneServicePasswordSecretName(ks, gen), Namespace: ks.Namespace}
	if err := r.Get(ctx, pwKey, pwSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading the registration password Secret: %w", err)
	}
	password := pwSecret.Data[serviceAccountPasswordKey]
	if len(password) == 0 {
		return false, nil
	}

	// nil target-cluster ref: a registration is delivered on the management
	// cluster, so the document resolves the in-cluster auth URL.
	cloudsYAML := []byte(buildServiceAccountCloudsYAML(cp, userName, projectName, domain, string(password), nil))
	sourceName := keystoneServiceSourceSecretName(ks)
	if err := r.ensureKeystoneServiceSecret(ctx, ks, sourceName, func(secret *corev1.Secret) error {
		secret.Data[serviceAccountPasswordKey] = password
		secret.Data["username"] = []byte(userName)
		secret.Data["project_name"] = []byte(projectName)
		secret.Data["user_domain_name"] = []byte(domain)
		secret.Data["project_domain_name"] = []byte(domain)
		secret.Data["auth_url"] = []byte(korcAuthURL(cp, nil))
		secret.Data["region_name"] = []byte(korcRegion(cp))
		secret.Data[appCredCloudsYAMLKey] = cloudsYAML
		return nil
	}); err != nil {
		return false, fmt.Errorf("assembling the registration source Secret %q: %w", sourceName, err)
	}

	sum := sha256.Sum256(cloudsYAML)
	contentHash := hex.EncodeToString(sum[:])

	ps := keystoneServicePushSecret(ks, cp)
	if err := secrets.EnsurePushSecret(ctx, r.Client, r.Scheme, ks, ps); err != nil {
		return false, fmt.Errorf("ensuring the registration PushSecret: %w", err)
	}
	if err := repushPushSecret(ctx, r.Client, ks.Namespace, ps.Name, keystoneServicePushContentHashAnnotation, contentHash); err != nil {
		return false, err
	}
	pushed := &esov1alpha1.PushSecret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ks.Namespace}, pushed); err != nil {
		return false, fmt.Errorf("reading the registration PushSecret: %w", err)
	}
	if !pushSecretReady(pushed) {
		return false, nil
	}

	if err := r.ensureKeystoneServiceExternalSecret(ctx, ks, cp); err != nil {
		return false, err
	}
	// The completed push is part of the trigger: without it a re-sync could
	// materialize the value that was in OpenBao BEFORE this generation's push.
	syncTrigger := contentHash + "/" + pushed.Status.SyncedResourceVersion
	if err := resyncExternalSecret(ctx, r.Client, ks.Namespace, keystoneServiceCredentialsSecretName(ks), syncTrigger); err != nil {
		return false, err
	}

	materialized := &corev1.Secret{}
	credKey := types.NamespacedName{Name: keystoneServiceCredentialsSecretName(ks), Namespace: ks.Namespace}
	if err := r.Get(ctx, credKey, materialized); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading the materialized registration Secret: %w", err)
	}
	return bytes.Equal(materialized.Data[serviceAccountPasswordKey], password), nil
}

// keystoneServicePushSecret builds the PushSecret mirroring the source Secret to
// the per-CR OpenBao path. DeletionPolicy Delete: the credential dies with the
// registration that owns it.
func keystoneServicePushSecret(ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane) *esov1alpha1.PushSecret {
	return &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{Name: keystoneServicePushSecretName(ks), Namespace: ks.Namespace},
		Spec: esov1alpha1.PushSecretSpec{
			DeletionPolicy:  esov1alpha1.PushSecretDeletionPolicyDelete,
			SecretStoreRefs: secrets.PushSecretStoreRefs(effectiveControlPlaneStoreRef(cp)),
			Selector: esov1alpha1.PushSecretSelector{
				Secret: &esov1alpha1.PushSecretSecret{Name: keystoneServiceSourceSecretName(ks)},
			},
			Data: []esov1alpha1.PushSecretData{{
				Match: esov1alpha1.PushSecretMatch{
					RemoteRef: esov1alpha1.PushSecretRemoteRef{RemoteKey: keystoneServiceRemoteKeyFor(ks)},
				},
			}},
		},
	}
}

// ensureKeystoneServiceExternalSecret create-or-updates the ExternalSecret that
// materializes the consumer Secret from the per-CR OpenBao path. It reads back
// the "password" and "clouds.yaml" properties — the documented consumption
// contract a service reads its credentials through.
func (r *KeystoneServiceReconciler) ensureKeystoneServiceExternalSecret(
	ctx context.Context, ks *c5c3v1alpha1.KeystoneService, cp *c5c3v1alpha1.ControlPlane,
) error {
	name := keystoneServiceCredentialsSecretName(ks)
	remoteKey := keystoneServiceRemoteKeyFor(ks)
	es := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ks.Namespace},
		Spec: esov1.ExternalSecretSpec{
			RefreshInterval: &metav1.Duration{Duration: time.Hour},
			SecretStoreRef:  secrets.ESOSecretStoreRef(effectiveControlPlaneStoreRef(cp)),
			Target:          esov1.ExternalSecretTarget{Name: name, CreationPolicy: esov1.CreatePolicyOwner},
			Data: []esov1.ExternalSecretData{
				{
					SecretKey: serviceAccountPasswordKey,
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: serviceAccountPasswordKey},
				},
				{
					SecretKey: appCredCloudsYAMLKey,
					RemoteRef: esov1.ExternalSecretDataRemoteRef{Key: remoteKey, Property: appCredCloudsYAMLKey},
				},
			},
		},
	}
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
