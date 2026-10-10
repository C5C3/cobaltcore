// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// provisionCredential keeps the order's credential minted and rotated and
// writes CredentialReady. It returns the result of the pass, which carries the
// time to the next rotation and to the end of a running grace period.
//
// Each generation N is one K-ORC ApplicationCredential, credential-v<N>, with
// the Secret secret-v<N> K-ORC reads the credential's secret from. K-ORC mints
// and deletes it authenticated as the ordered user through the mint document,
// because Keystone creates a user's application credentials only for the
// token's own user, scoped to the token's project with the token's roles.
// K-ORC never updates a credential, so a rotation is a new generation: the
// successor is minted, status switches to it once it is Available, the delivery
// leg rewrites the Secret in the same pass, and the superseded generation is
// deleted after the grace period.
func (r *KeystoneApplicationCredentialReconciler) provisionCredential(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cp *c5c3v1alpha1.ControlPlane,
	cluster string, user *c5c3v1alpha1.KeystoneUser, project *c5c3v1alpha1.KeystoneProject,
) (ctrl.Result, error) {
	fail := keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialCredentialReady)
	now := r.now()
	interval, grace := keystoneApplicationCredentialRotation(order)

	if ok, result, err := r.ensureMintCloud(ctx, order, cp, cluster, user, project); err != nil || !ok {
		return result, err
	}

	credentials, secretsByGen, err := r.credentialChildren(ctx, order, cluster, cp.Namespace)
	if err != nil {
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return ctrl.Result{}, err
	}
	cur := order.Status.CredentialGeneration
	desired := desiredGeneration(ctx, order, interval, grace, credentials, secretsByGen, now)
	if err := r.pruneStrayGenerations(ctx, order, credentials, secretsByGen, desired); err != nil {
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return ctrl.Result{}, err
	}

	var result ctrl.Result
	if desired > cur {
		result, err = r.mintGeneration(ctx, order, cp, cluster, user, project, desired, now)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		keystoneApplicationCredentialSetTrue(order, conditionTypeKeystoneApplicationCredentialCredentialReady,
			reasonKeystoneApplicationCredentialMinted, mintedMessage(order, user, project))
	}

	if err := r.sweepGrace(ctx, order, cluster, cp.Namespace, credentials, now); err != nil {
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return ctrl.Result{}, err
	}

	order.Status.NextRotation = nextRotation(order, interval, grace)
	return commonreconcile.ShortestRequeue(result,
		requeueAt(order.Status.NextRotation, now),
		requeueAt(order.Status.PreviousCredentialDeleteAt, now),
		requeueAt(order.Status.CredentialExpiresAt, now)), nil
}

// ensureMintCloud writes the mint document, mint-cloud: the project-scoped
// password clouds.yaml of the ordered user, rebuilt on every pass from the
// password of the user's status.passwordGeneration, so a password rotation
// reaches it and K-ORC can always mint the successor and delete the superseded
// credential. It carries the Keystone CA bundle K-ORC verifies the endpoint
// with. ok is false when a wait or an error was written on CredentialReady.
func (r *KeystoneApplicationCredentialReconciler) ensureMintCloud(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cp *c5c3v1alpha1.ControlPlane,
	cluster string, user *c5c3v1alpha1.KeystoneUser, project *c5c3v1alpha1.KeystoneProject,
) (bool, ctrl.Result, error) {
	fail := keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialCredentialReady)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}

	if project.Status.ProjectName == "" {
		fail(reasonKeystoneProjectWaiting, fmt.Sprintf("KeystoneProject %q reports no project name yet", project.Name))
		return false, requeue, nil
	}

	gen := user.Status.PasswordGeneration
	key := types.NamespacedName{Namespace: cp.Namespace, Name: keystoneUserPasswordSecretName(user, cluster, gen)}
	pwSecret := &corev1.Secret{}
	if err := r.Get(ctx, key, pwSecret); client.IgnoreNotFound(err) != nil {
		err = fmt.Errorf("reading the password Secret %s of KeystoneUser %q: %w", key, user.Name, err)
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return false, ctrl.Result{}, err
	}
	password := pwSecret.Data[serviceAccountPasswordKey]
	if len(password) == 0 {
		fail(reasonKeystoneRoleAssignmentWaitingForUser, fmt.Sprintf(
			"the password of generation %d of KeystoneUser %q is not available yet", gen, user.Name))
		return false, requeue, nil
	}

	caBundle, err := readKeystoneCABundle(ctx, r.Client, cp)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			fail(reasonKeystoneApplicationCredentialWaitingForCABundle, fmt.Sprintf(
				"the CA bundle Secret referenced by %s is not yet available; the credential is not minted",
				keystoneCABundleField(cp)))
			return false, requeue, nil
		}
		err = fmt.Errorf("reading the Keystone CA bundle: %w", err)
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return false, ctrl.Result{}, err
	}

	name := keystoneApplicationCredentialMintCloudName(order, cluster)
	cloudsYAML := buildServiceAccountCloudsYAML(cp, keystoneUserName(user), project.Status.ProjectName,
		adminDomainName(cp), string(password), nil)
	if err := ensureOrderSecret(ctx, r.Client, keystoneApplicationCredentialRef(order, cluster), name, cp.Namespace,
		func(secret *corev1.Secret) error {
			secret.Data[appCredCloudsYAMLKey] = []byte(cloudsYAML)
			setCACertKey(secret, caBundle)
			return nil
		}); err != nil {
		err = fmt.Errorf("ensuring the mint document %s/%s: %w", cp.Namespace, name, err)
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

// credentialChildren lists the order's ApplicationCredentials and Secrets in
// childNS and keys the generation children by their generation. A labelled
// object outside the order's prefix is not its child and is left out.
func (r *KeystoneApplicationCredentialReconciler) credentialChildren(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster, childNS string,
) (map[int64]*orcv1alpha1.ApplicationCredential, map[int64]*corev1.Secret, error) {
	ref := keystoneApplicationCredentialRef(order, cluster)
	selector := client.MatchingLabels(ref.childLabels())

	var credentialList orcv1alpha1.ApplicationCredentialList
	if err := r.List(ctx, &credentialList, client.InNamespace(childNS), selector); err != nil {
		return nil, nil, fmt.Errorf("listing the order's ApplicationCredentials: %w", err)
	}
	credentials := map[int64]*orcv1alpha1.ApplicationCredential{}
	credentialPrefix := keystoneApplicationCredentialCredentialPrefix(order, cluster)
	for i := range credentialList.Items {
		ac := &credentialList.Items[i]
		if gen, ok := credentialGenerationOf(ac.Name, credentialPrefix); ok && ownsOrderChild(ac, order, ref) {
			credentials[gen] = ac
		}
	}

	var secretList corev1.SecretList
	if err := r.List(ctx, &secretList, client.InNamespace(childNS), selector); err != nil {
		return nil, nil, fmt.Errorf("listing the order's Secrets: %w", err)
	}
	secretsByGen := map[int64]*corev1.Secret{}
	secretPrefix := keystoneApplicationCredentialSecretPrefix(order, cluster)
	for i := range secretList.Items {
		secret := &secretList.Items[i]
		if gen, ok := credentialGenerationOf(secret.Name, secretPrefix); ok && ownsOrderChild(secret, order, ref) {
			secretsByGen[gen] = secret
		}
	}
	return credentials, secretsByGen, nil
}

// desiredGeneration decides the generation the pass works toward. Before the
// first mint it is the declared generation. After it, a credential child above
// the live generation is a rotation in flight, and the highest one is
// continued. One K-ORC failed terminally is given up once a higher generation
// is declared, so raising spec.credentialGeneration replaces a failed
// successor. Otherwise a rotation starts, toward the declared generation or the
// next one, whichever is higher, when the declared generation is above the live
// one, the schedule is due, the live credential has expired, or its secret is
// lost from the cluster; the Keystone secret cannot be recovered, so a lost one
// is rotated. No rotation starts while a grace period runs, so the order never
// holds more than two credentials.
func desiredGeneration(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, interval, grace time.Duration,
	credentials map[int64]*orcv1alpha1.ApplicationCredential, secretsByGen map[int64]*corev1.Secret, now time.Time,
) int64 {
	declared := keystoneApplicationCredentialGeneration(order)
	cur := order.Status.CredentialGeneration
	if cur == 0 {
		return declared
	}
	inFlight := int64(0)
	for gen, ac := range credentials {
		if gen > cur && (orcv1alpha1.GetTerminalError(ac) == nil || gen >= declared) {
			inFlight = max(inFlight, gen)
		}
	}
	if inFlight > 0 {
		return inFlight
	}
	if order.Status.PreviousCredentialDeleteAt != nil {
		return cur
	}

	due := func(t *metav1.Time) bool { return t != nil && !now.Before(t.Time) }
	rotate := declared > cur ||
		(interval > 0 && due(nextRotation(order, interval, grace))) ||
		due(order.Status.CredentialExpiresAt)
	if secret := secretsByGen[cur]; secret == nil || len(secret.Data[appCredSecretValueKey]) == 0 {
		log.FromContext(ctx).Info(fmt.Sprintf("the secret of credential generation %d is lost; rotating", cur))
		rotate = true
	}
	if !rotate {
		return cur
	}
	return max(declared, cur+1)
}

// nextRotation is when the schedule rotates the live credential: the earlier
// of the last rotation plus the interval and the credential's expiry minus the
// grace period, so a lengthened interval never lets the delivered credential
// run into its Keystone expiry. It is nil with the schedule off and before the
// first mint.
func nextRotation(order *c5c3v1alpha1.KeystoneApplicationCredential, interval, grace time.Duration) *metav1.Time {
	last := order.Status.LastRotation
	if interval <= 0 || last == nil {
		return nil
	}
	next := last.Add(interval)
	if expires := order.Status.CredentialExpiresAt; expires != nil && expires.Add(-grace).Before(next) {
		next = expires.Add(-grace)
	}
	return &metav1.Time{Time: next}
}

// requeueAt returns the result that wakes the order at t, or an empty one when t
// is unset or not after now.
func requeueAt(t *metav1.Time, now time.Time) ctrl.Result {
	if t == nil || !t.After(now) {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: t.Sub(now)}
}

// pruneStrayGenerations deletes every credential and secret generation other
// than the live one, the superseded one while its grace period runs, and the
// one the pass works toward, so a pass that was interrupted leaves nothing
// behind.
func (r *KeystoneApplicationCredentialReconciler) pruneStrayGenerations(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential,
	credentials map[int64]*orcv1alpha1.ApplicationCredential, secretsByGen map[int64]*corev1.Secret, desired int64,
) error {
	keep := func(gen int64) bool {
		return gen == order.Status.CredentialGeneration || gen == desired ||
			(order.Status.PreviousCredentialDeleteAt != nil && gen == order.Status.PreviousCredentialGeneration)
	}
	var stray []client.Object
	for gen, ac := range credentials {
		if !keep(gen) {
			stray = append(stray, ac)
		}
	}
	for gen, secret := range secretsByGen {
		if !keep(gen) {
			stray = append(stray, secret)
		}
	}
	for _, obj := range stray {
		if obj.GetDeletionTimestamp() != nil {
			continue
		}
		log.FromContext(ctx).Info("removing a stray credential generation", "name", obj.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting stray credential generation %s: %w", obj.GetName(), err)
		}
	}
	return nil
}

// mintGeneration ensures generation gen's Secret and ApplicationCredential and
// switches status to it once K-ORC reports it Available with an id. A
// credential minted with the schedule on expires in Keystone at the mint time
// plus the interval plus the grace period, so one the operator fails to delete
// dies on its own.
func (r *KeystoneApplicationCredentialReconciler) mintGeneration(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cp *c5c3v1alpha1.ControlPlane,
	cluster string, user *c5c3v1alpha1.KeystoneUser, project *c5c3v1alpha1.KeystoneProject, gen int64, now time.Time,
) (ctrl.Result, error) {
	fail := keystoneApplicationCredentialFail(order, conditionTypeKeystoneApplicationCredentialCredentialReady)
	requeue := ctrl.Result{RequeueAfter: korcRequeueAfter}
	ref := keystoneApplicationCredentialRef(order, cluster)
	interval, grace := keystoneApplicationCredentialRotation(order)
	cur := order.Status.CredentialGeneration

	secretName := keystoneApplicationCredentialSecretName(order, cluster, gen)
	if err := ensureOrderSecret(ctx, r.Client, ref, secretName, cp.Namespace, generatedAppCredValueMutator); err != nil {
		err = fmt.Errorf("ensuring the secret of credential generation %d: %w", gen, err)
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return ctrl.Result{}, err
	}

	var expiresAt *metav1.Time
	if interval > 0 {
		expiresAt = &metav1.Time{Time: now.Add(interval + grace)}
	}
	ac, err := r.ensureCredentialGeneration(ctx, order, cp, cluster, user, gen, expiresAt)
	if err != nil {
		fail(reasonKeystoneApplicationCredentialError, err.Error())
		return ctrl.Result{}, err
	}

	// A latched transport error is handed back to K-ORC first; korc_unlatch.go
	// states the policy. A latch the operator cannot clear is reported and
	// retried on the K-ORC cadence like every other wait of the mint.
	if err := unlatchKORCTransportErrors(ctx, r.Client, ac); err != nil {
		fail(conditionReasonTransportErrorRetryFailed, err.Error())
		return requeue, nil
	}
	if termErr := orcv1alpha1.GetTerminalError(ac); termErr != nil {
		fail(reasonKeystoneApplicationCredentialFailed, fmt.Sprintf(
			"K-ORC reported a terminal error on credential generation %d: %v", gen, termErr))
		return requeue, nil
	}
	if !orcv1alpha1.IsAvailable(ac) || ac.Status.ID == nil {
		if cur == 0 {
			waitOrClassifyCondition(cp, fail, reasonKeystoneApplicationCredentialWaiting,
				fmt.Sprintf("credential generation %d is registered but not yet Available", gen), ac)
			return requeue, nil
		}
		// The delivered credential is valid while its successor is minted, so a
		// scheduled rotation does not read as a failure.
		keystoneApplicationCredentialSetTrue(order, conditionTypeKeystoneApplicationCredentialCredentialReady,
			reasonKeystoneApplicationCredentialMinted,
			fmt.Sprintf("credential generation %d is delivered; generation %d is being minted", cur, gen))
		return requeue, nil
	}

	if cur > 0 {
		order.Status.PreviousCredentialID = order.Status.CredentialID
		order.Status.PreviousCredentialGeneration = cur
		order.Status.PreviousCredentialDeleteAt = &metav1.Time{Time: now.Add(grace)}
	}
	order.Status.CredentialID = *ac.Status.ID
	order.Status.CredentialGeneration = gen
	order.Status.LastRotation = &metav1.Time{Time: now}
	// K-ORC reports the expiry Keystone holds; the CR's own is the fallback
	// while its status does not carry one.
	order.Status.CredentialExpiresAt = nil
	if res := ac.Status.Resource; res != nil && res.ExpiresAt != nil {
		order.Status.CredentialExpiresAt = res.ExpiresAt.DeepCopy()
	} else if spec := ac.Spec.Resource; spec != nil {
		order.Status.CredentialExpiresAt = spec.ExpiresAt.DeepCopy()
	}
	keystoneApplicationCredentialSetTrue(order, conditionTypeKeystoneApplicationCredentialCredentialReady,
		reasonKeystoneApplicationCredentialMinted, mintedMessage(order, user, project))
	return ctrl.Result{}, nil
}

// ensureCredentialGeneration returns generation gen's ApplicationCredential,
// creating it when it does not exist. A live one is never applied again: K-ORC
// keeps its resource block immutable, and its expiry was fixed at the mint.
func (r *KeystoneApplicationCredentialReconciler) ensureCredentialGeneration(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cp *c5c3v1alpha1.ControlPlane,
	cluster string, user *c5c3v1alpha1.KeystoneUser, gen int64, expiresAt *metav1.Time,
) (*orcv1alpha1.ApplicationCredential, error) {
	ref := keystoneApplicationCredentialRef(order, cluster)
	key := types.NamespacedName{Namespace: cp.Namespace, Name: keystoneApplicationCredentialCredentialName(order, cluster, gen)}
	ac := &orcv1alpha1.ApplicationCredential{}
	switch err := r.Get(ctx, key, ac); {
	case err == nil:
		if !isOrderChild(ac, order, ref) {
			return nil, fmt.Errorf("refusing to adopt pre-existing ApplicationCredential %s: it was not created by this order", key)
		}
		return ac, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading ApplicationCredential %s: %w", key, err)
	}

	ac = &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: orcv1alpha1.ApplicationCredentialSpec{
			ManagementPolicy: orcv1alpha1.ManagementPolicyManaged,
			CloudCredentialsRef: orcv1alpha1.CloudCredentialsReference{
				SecretName: keystoneApplicationCredentialMintCloudName(order, cluster),
				CloudName:  korcCloudName(cp),
			},
			Resource: &orcv1alpha1.ApplicationCredentialResourceSpec{
				Description: ptr.To(fmt.Sprintf("KeystoneApplicationCredential %s/%s generation %d",
					order.Namespace, order.Name, gen)),
				UserRef:      orcv1alpha1.KubernetesNameRef(keystoneUserUserRef(user, cluster)),
				Unrestricted: ptr.To(false),
				SecretRef:    orcv1alpha1.KubernetesNameRef(keystoneApplicationCredentialSecretName(order, cluster, gen)),
				ExpiresAt:    expiresAt,
			},
		},
	}
	if err := ensureOrderChild(ctx, r.Client, r.Scheme, order, ref, ac); err != nil {
		return nil, fmt.Errorf("ensuring credential generation %d: %w", gen, err)
	}
	return ac, nil
}

// sweepGrace ends a running grace period once its time has come: it deletes the
// superseded ApplicationCredential, and K-ORC deletes the credential in
// Keystone through the mint document. On the pass that no longer lists it, the
// superseded secret goes too and the previous* fields are cleared. The
// credential child watch brings the order back once K-ORC has let go.
func (r *KeystoneApplicationCredentialReconciler) sweepGrace(
	ctx context.Context, order *c5c3v1alpha1.KeystoneApplicationCredential, cluster, childNS string,
	credentials map[int64]*orcv1alpha1.ApplicationCredential, now time.Time,
) error {
	deleteAt := order.Status.PreviousCredentialDeleteAt
	if deleteAt == nil || now.Before(deleteAt.Time) {
		return nil
	}
	prev := order.Status.PreviousCredentialGeneration
	if ac, listed := credentials[prev]; listed {
		if ac.DeletionTimestamp != nil {
			return nil
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, ac)); err != nil {
			return fmt.Errorf("deleting superseded credential generation %d: %w", prev, err)
		}
		return nil
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: childNS, Name: keystoneApplicationCredentialSecretName(order, cluster, prev),
	}}
	if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
		return fmt.Errorf("deleting the secret of superseded credential generation %d: %w", prev, err)
	}
	log.FromContext(ctx).Info(fmt.Sprintf("deleted superseded credential generation %d (id %s)",
		prev, order.Status.PreviousCredentialID))
	order.Status.PreviousCredentialID = ""
	order.Status.PreviousCredentialGeneration = 0
	order.Status.PreviousCredentialDeleteAt = nil
	return nil
}

// generatedAppCredValueMutator fills the value key K-ORC reads a credential's
// secret from, once. A value already present is preserved: Keystone holds the
// credential with it.
func generatedAppCredValueMutator(secret *corev1.Secret) error {
	if len(secret.Data[appCredSecretValueKey]) > 0 {
		return nil
	}
	v, err := generateAppCredSecretValue()
	if err != nil {
		return err
	}
	secret.Data[appCredSecretValueKey] = []byte(v)
	return nil
}

// mintedMessage is CredentialReady's True message for the live generation.
func mintedMessage(
	order *c5c3v1alpha1.KeystoneApplicationCredential, user *c5c3v1alpha1.KeystoneUser,
	project *c5c3v1alpha1.KeystoneProject,
) string {
	return fmt.Sprintf("credential generation %d (id %s) is minted for user %q on project %q",
		order.Status.CredentialGeneration, order.Status.CredentialID, keystoneUserName(user), keystoneProjectName(project))
}
