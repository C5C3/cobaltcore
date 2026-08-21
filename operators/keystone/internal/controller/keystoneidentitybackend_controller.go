// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/c5c3/cobaltcore/internal/common/bootstrap"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	"github.com/c5c3/cobaltcore/internal/common/watch"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/keystone/internal/identity"
)

// Condition reason constants for the per-backend DomainReady and
// ConfigProjected conditions.
const (
	// DomainReady reasons.
	conditionReasonKeystoneNotFound       = "KeystoneNotFound"
	conditionReasonWaitingForKeystoneAPI  = "WaitingForKeystoneAPI"
	conditionReasonAdminSecretUnavailable = "AdminSecretUnavailable"
	conditionReasonDomainProvisioned      = "DomainProvisioned"
	conditionReasonDomainAdopted          = "DomainAdopted"
	conditionReasonDomainNotFound         = "DomainNotFound"
	conditionReasonDomainAlreadyExists    = "DomainAlreadyExists"
	conditionReasonIdentityAPIError       = "IdentityAPIError"

	// ConfigProjected reasons. conditionReasonWaitingForRollout (declared with
	// the shared identity-backend vocabulary in reconcile_identitybackends.go)
	// is the third: the config is mounted but the Deployment has not finished
	// rolling it out.
	conditionReasonConfigProjected      = "ConfigProjected"
	conditionReasonWaitingForProjection = "WaitingForProjection"
)

// configProjectionState is inspectConfigProjection's three-valued answer to
// "does the Keystone workload run this backend's config?".
type configProjectionState int

const (
	// configNotProjected: the Deployment's pod template does not reference
	// this backend's rendered config (volume absent, Secret absent, or the
	// backend's data key missing).
	configNotProjected configProjectionState = iota
	// configRolloutPending: the pod template mounts the config, but replicas
	// of a previous template still serve.
	configRolloutPending
	// configRunning: every replica runs the template that mounts the config.
	configRunning
)

// identityBackendSubConditionTypesFor returns the per-backend sub-conditions
// the aggregate Ready is derived from. The set is per backend type:
// SetAggregateReady requires every listed type present-and-True, so listing
// the federation conditions for an LDAP backend would strand it Ready=False
// forever.
func identityBackendSubConditionTypesFor(backend *keystonev1alpha1.KeystoneIdentityBackend) []string {
	if backend.IsFederationType() {
		return []string{
			conditionTypeDomainReady,
			conditionTypeFederationObjectsReady,
			conditionTypeMappingsReady,
			conditionTypeConfigProjected,
		}
	}
	return []string{
		conditionTypeDomainReady,
		conditionTypeConfigProjected,
	}
}

// KeystoneIdentityBackendReconciler owns the KeystoneIdentityBackend CR
// lifecycle: finalizer, domain provisioning through the minimal identity
// client, and the per-backend DomainReady / ConfigProjected / Ready
// conditions. It is the SINGLE writer of KeystoneIdentityBackend status; the
// keystone-side identitybackends sub-reconciler only reads it and writes the
// aggregated IdentityBackendsReady condition onto the Keystone CR instead
// (the two-controller split of the Phase-0 decisions).
type KeystoneIdentityBackendReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// IdentityClientFactory is the injectable seam for the identity client —
	// tests bind it to the httptest-backed fake identity server; nil falls
	// back to the production identity.NewHTTPClient. Mirrors the HTTPClient
	// HTTPDoer seam on KeystoneReconciler.
	IdentityClientFactory func(endpoint string, creds identity.Credentials) identity.Client

	// Resolver resolves the target cluster the referenced Keystone names in
	// spec.targetClusterRef. Only the federation-objects teardown consults it:
	// a placed Keystone keeps both its admin-password Secret and its API on
	// that cluster, so both have to be reached there before the finalizer is
	// released. Nil means always-local, which is what single-cluster tests and
	// deployments want.
	Resolver commonmulticluster.ClusterResolver

	// MaxConcurrentReconciles bounds how many backend CRs reconcile
	// concurrently; see KeystoneReconciler.MaxConcurrentReconciles.
	MaxConcurrentReconciles int
}

// identityClient builds the identity client for the given endpoint and
// credentials, honoring the injectable factory. doer is the transport the
// production client runs on — the target cluster's service proxy for a placed
// Keystone; nil dials the endpoint directly, which is what an unplaced one
// wants.
func (r *KeystoneIdentityBackendReconciler) identityClient(endpoint string, creds identity.Credentials, doer identity.HTTPDoer) identity.Client {
	if r.IdentityClientFactory != nil {
		return r.IdentityClientFactory(endpoint, creds)
	}
	return identity.NewHTTPClient(endpoint, creds, doer)
}

// Reconcile drives one KeystoneIdentityBackend CR: finalizer installation,
// domain provisioning (Manage) or resolution (Adopt), the ConfigProjected
// observation, and the deletion policy on teardown.
func (r *KeystoneIdentityBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backend keystonev1alpha1.KeystoneIdentityBackend
	if err := r.Get(ctx, req.NamespacedName, &backend); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).V(1).Info("KeystoneIdentityBackend not found; likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching KeystoneIdentityBackend: %w", err)
	}

	if backend.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, &backend)
	}

	// Install the finalizer before any provisioning so a deletion issued
	// between now and the next pass funnels through reconcileDelete.
	if added, err := commonreconcile.EnsureFinalizer(ctx, r.Client, &backend, identityBackendFinalizerName); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	statusBefore := backend.Status.DeepCopy()
	result, err := r.reconcileNormal(ctx, &backend)
	return r.updateStatus(ctx, &backend, statusBefore, result, err)
}

// reconcileNormal drives the non-deleting path: resolve the Keystone, gate on
// its API health, ensure the domain, and observe the config projection.
func (r *KeystoneIdentityBackendReconciler) reconcileNormal(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	keystone, ready, result := r.resolveKeystone(ctx, backend)
	if keystone == nil || !ready {
		return result, nil
	}

	creds, err := r.adminCredentials(ctx, r.Client, keystone)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonAdminSecretUnavailable,
				fmt.Sprintf("Admin password Secret %s/%s is missing or has no password key",
					keystone.Namespace, keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name))
			return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
		}
		return ctrl.Result{}, err
	}

	idc := r.identityClient(internalAPIURL(keystone), creds, nil)
	if result, err := r.ensureDomain(ctx, backend, idc); !result.IsZero() || err != nil {
		return result, err
	}

	// Federation objects require the domain: identity providers are
	// domain-scoped and the declarative groups live inside it. A zero-result
	// ensureDomain pass can still leave DomainReady=False (e.g. a foreign
	// same-named domain), so gate on the condition, not just on control flow.
	if backend.IsFederationType() {
		domainReady := conditions.GetCondition(backend.Status.Conditions, conditionTypeDomainReady)
		if domainReady != nil && domainReady.Status == metav1.ConditionTrue && backend.Status.DomainID != "" {
			if result, err := r.ensureFederation(ctx, backend, idc); !result.IsZero() || err != nil {
				return result, err
			}
		}
	}

	return r.observeConfigProjected(ctx, keystone, backend)
}

// resolveKeystone fetches the referenced Keystone CR and gates on its
// KeystoneAPIReady condition. It returns (nil, false, zeroResult) with the
// DomainReady condition set when the backend must wait; the Keystone watch
// (keystoneToIdentityBackendsMapper) re-enqueues this backend on any Keystone
// event, including status flips, so no requeue timer is needed here.
func (r *KeystoneIdentityBackendReconciler) resolveKeystone(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) (*keystonev1alpha1.Keystone, bool, ctrl.Result) {
	var keystone keystonev1alpha1.Keystone
	key := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.KeystoneRef.Name}
	if err := r.Get(ctx, key, &keystone); err != nil {
		if apierrors.IsNotFound(err) {
			r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonKeystoneNotFound,
				fmt.Sprintf("Keystone %s not found in namespace %s", key.Name, key.Namespace))
			return nil, false, ctrl.Result{}
		}
		// Transient client failure: let the workqueue back off via the error
		// path in Reconcile by treating it as a hard wait with a short poll.
		log.FromContext(ctx).Error(err, "fetching referenced Keystone", "keystone", key)
		r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonKeystoneNotFound,
			fmt.Sprintf("fetching Keystone %s: %v", key.Name, err))
		return nil, false, ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}
	}

	apiReady := conditions.GetCondition(keystone.Status.Conditions, conditionTypeKeystoneAPIReady)
	if apiReady == nil || apiReady.Status != metav1.ConditionTrue {
		r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonWaitingForKeystoneAPI,
			fmt.Sprintf("Keystone %s API is not ready yet", keystone.Name))
		return &keystone, false, ctrl.Result{}
	}
	return &keystone, true, ctrl.Result{}
}

// adminCredentials reads the bootstrap admin password through c and assembles
// the identity-client credentials (bootstrap admin user, admin project, Default
// user domain — BootstrapSpec has no domain knob). c is the management client
// for a Keystone deployed here and the target cluster's client for a placed
// one, whose admin-password Secret lives there.
func (r *KeystoneIdentityBackendReconciler) adminCredentials(ctx context.Context, c client.Client, keystone *keystonev1alpha1.Keystone) (identity.Credentials, error) {
	key := client.ObjectKey{Namespace: keystone.Namespace, Name: keystone.Spec.Bootstrap.AdminPasswordSecretRef.Name}
	password, err := secrets.GetSecretValue(ctx, c, key, "password")
	if err != nil {
		return identity.Credentials{}, err
	}
	return identity.Credentials{
		Username: keystone.Spec.Bootstrap.AdminUser,
		Password: password,
	}, nil
}

// ensureDomain provisions (Manage) or resolves (Adopt) the backend's domain
// and maintains DomainReady + Status.DomainID.
func (r *KeystoneIdentityBackendReconciler) ensureDomain(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, idc identity.Client) (ctrl.Result, error) {
	domainName := backend.Spec.Domain.Name

	existing, err := idc.GetDomainByName(ctx, domainName)
	if err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
			fmt.Sprintf("looking up domain %q: %v", domainName, err))
		return ctrl.Result{}, fmt.Errorf("looking up domain %q: %w", domainName, err)
	}

	if backend.Spec.Domain.Mode == keystonev1alpha1.DomainModeAdopt {
		if existing == nil {
			r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonDomainNotFound,
				fmt.Sprintf("domain %q not found; Adopt mode never creates domains", domainName))
			return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
		}
		// Adopt resolves by name and NEVER mutates the domain.
		if backend.Status.DomainID == "" {
			r.Recorder.Eventf(backend, corev1.EventTypeNormal, "DomainAdopted",
				"Adopted pre-existing domain %q (id %s)", domainName, existing.ID)
		}
		backend.Status.DomainID = existing.ID
		r.setDomainReady(backend, metav1.ConditionTrue, conditionReasonDomainAdopted,
			fmt.Sprintf("domain %q adopted (id %s)", domainName, existing.ID))
		return ctrl.Result{}, nil
	}

	// Manage mode.
	if existing == nil {
		created, err := idc.CreateDomain(ctx, identity.Domain{
			Name:        domainName,
			Description: backend.Spec.Domain.Description,
			Enabled:     ptr.To(true),
		})
		if err != nil {
			r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("creating domain %q: %v", domainName, err))
			return ctrl.Result{}, fmt.Errorf("creating domain %q: %w", domainName, err)
		}
		backend.Status.DomainID = created.ID
		r.Recorder.Eventf(backend, corev1.EventTypeNormal, "DomainCreated",
			"Created domain %q (id %s)", domainName, created.ID)
		r.setDomainReady(backend, metav1.ConditionTrue, conditionReasonDomainProvisioned,
			fmt.Sprintf("domain %q provisioned (id %s)", domainName, created.ID))
		return ctrl.Result{}, nil
	}

	if backend.Status.DomainID == "" || backend.Status.DomainID != existing.ID {
		// A same-named domain this CR did not create: never silently seize a
		// foreign domain — the user opts into that explicitly via Adopt.
		r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonDomainAlreadyExists,
			fmt.Sprintf("domain %q already exists (id %s) but was not created by this backend; use domain.mode Adopt to attach to it", domainName, existing.ID))
		return ctrl.Result{}, nil
	}

	// Our domain: reconcile description and keep it enabled. Only write on
	// drift to avoid unconditional identity-API churn.
	wantDescription := backend.Spec.Domain.Description
	enabled := existing.Enabled == nil || *existing.Enabled
	if existing.Description != wantDescription || !enabled {
		var desc *string
		if existing.Description != wantDescription {
			desc = &wantDescription
		}
		var en *bool
		if !enabled {
			en = ptr.To(true)
		}
		if err := idc.UpdateDomain(ctx, existing.ID, en, desc); err != nil {
			r.setDomainReady(backend, metav1.ConditionFalse, conditionReasonIdentityAPIError,
				fmt.Sprintf("updating domain %q: %v", domainName, err))
			return ctrl.Result{}, fmt.Errorf("updating domain %q: %w", domainName, err)
		}
	}
	r.setDomainReady(backend, metav1.ConditionTrue, conditionReasonDomainProvisioned,
		fmt.Sprintf("domain %q provisioned (id %s)", domainName, existing.ID))
	return ctrl.Result{}, nil
}

// observeConfigProjected derives the ConfigProjected condition from the
// single authoritative pointer: the Keystone Deployment's domains volume and
// the per-domain file inside the Secret it references. A False observation
// carries a commonreconcile.RequeueSecretPolling safety net — the Keystone
// watch normally wakes this controller when the projection lands, but a
// converged Keystone status (no write, no event) must not strand the backend.
func (r *KeystoneIdentityBackendReconciler) observeConfigProjected(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	state, err := r.inspectConfigProjection(ctx, keystone, backend)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch state {
	case configNotProjected:
		conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
			Type:               conditionTypeConfigProjected,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: backend.Generation,
			Reason:             conditionReasonWaitingForProjection,
			Message:            "waiting for the Keystone Deployment to mount this backend's domain config",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case configRolloutPending:
		conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
			Type:               conditionTypeConfigProjected,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: backend.Generation,
			Reason:             conditionReasonWaitingForRollout,
			Message:            "the Keystone Deployment has not finished rolling out this backend's config",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	case configRunning:
		// Fall through to the True observation below.
	}
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               conditionTypeConfigProjected,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: backend.Generation,
		Reason:             conditionReasonConfigProjected,
		Message:            "domain config is mounted in the Keystone Deployment",
	})
	// For a projected SAML backend, publish the stable-named SP metadata export
	// Secret the keystone side created (this controller is the single status
	// writer). Operators register the SP with the IdP from this Secret.
	if backend.Spec.Type == keystonev1alpha1.IdentityBackendTypeSAML {
		backend.Status.SAMLSPMetadataSecretName = samlSPMetadataSecretName(keystone)
	}
	return ctrl.Result{}, nil
}

// inspectConfigProjection reports whether the Keystone Deployment runs this
// backend's rendered config: an LDAP backend's keystone.<domain>.conf inside
// the domains-volume Secret, an OIDC backend's <name>.client document inside
// the federation-metadata-volume Secret, a SAML backend's IdP-metadata document
// inside the federation-mellon-volume Secret. A config that is mounted by the
// pod template but not yet rolled out to every replica reports
// configRolloutPending — the template flips the moment the Keystone side
// writes it, while the pod serving requests lags a rollout behind.
func (r *KeystoneIdentityBackendReconciler) inspectConfigProjection(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (configProjectionState, error) {
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: keystone.Namespace, Name: subResourceName(keystone)}
	if err := r.Get(ctx, deployKey, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return configNotProjected, nil
		}
		return configNotProjected, fmt.Errorf("fetching Deployment %s: %w", deployKey, err)
	}

	volumeName := domainsVolumeName
	dataKey := domainConfFileName(backend.Spec.Domain.Name)
	switch backend.Spec.Type {
	case keystonev1alpha1.IdentityBackendTypeLDAP:
		// The domains-volume defaults set above.
	case keystonev1alpha1.IdentityBackendTypeOIDC:
		volumeName = federationMetadataVolumeName
		dataKey = federationClientKeyName(backend.Name)
	case keystonev1alpha1.IdentityBackendTypeSAML:
		volumeName = federationMellonVolumeName
		dataKey = samlIdPMetadataKeyName(backend.Name)
	}

	secretName := secretNameForVolume(&deploy, volumeName)
	if secretName == "" {
		return configNotProjected, nil
	}

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: keystone.Namespace, Name: secretName}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return configNotProjected, nil
		}
		return configNotProjected, fmt.Errorf("fetching projection Secret %s: %w", secretKey, err)
	}
	if _, ok := secret.Data[dataKey]; !ok {
		return configNotProjected, nil
	}
	if !keystoneDeploymentRolledOut(&deploy) {
		return configRolloutPending, nil
	}
	return configRunning, nil
}

// reconcileDelete drives the finalizer teardown: wait until the keystone-side
// sub-reconciler has de-projected this backend's config (so keystone never
// runs with config pointing at a dead domain), then apply the domain deletion
// policy, then release the finalizer.
func (r *KeystoneIdentityBackendReconciler) reconcileDelete(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(backend, identityBackendFinalizerName) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	var keystone keystonev1alpha1.Keystone
	key := client.ObjectKey{Namespace: backend.Namespace, Name: backend.Spec.KeystoneRef.Name}
	if err := r.Get(ctx, key, &keystone); err != nil {
		if apierrors.IsNotFound(err) {
			// The whole Keystone is gone: nothing to de-project and no API to
			// clean the domain through. Fail open — a finalizer must never
			// hold the backend hostage to a deployment that no longer exists.
			logger.Info("referenced Keystone gone; releasing identity-backend finalizer", "keystone", key)
			return r.removeFinalizer(ctx, backend)
		}
		return ctrl.Result{}, fmt.Errorf("fetching referenced Keystone %s: %w", key, err)
	}

	// Wait for de-projection: reconcileIdentityBackends skips deleting
	// backends, so the next Keystone pass drops our conf file from the
	// domains Secret and rolls the Deployment. Anything other than
	// configNotProjected — including a rollout still carrying the config —
	// holds the teardown. The backend watch on the Keystone controller wakes
	// it on our DeletionTimestamp flip; the requeue below is the safety net.
	state, err := r.inspectConfigProjection(ctx, &keystone, backend)
	if err != nil {
		return ctrl.Result{}, err
	}
	if state != configNotProjected {
		logger.V(1).Info("waiting for identity-backend config de-projection before domain teardown")
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
	}

	// Federation backends (OIDC and SAML) tear their federation API objects
	// down unconditionally (protocol → mapping → identity provider) BEFORE the
	// domain deletion policy runs: the identity provider is domain-scoped, so a
	// Delete-policy domain teardown would otherwise race its own contents.
	if backend.IsFederationType() {
		if result, err := r.teardownFederationObjects(ctx, &keystone, backend); !result.IsZero() || err != nil {
			return result, err
		}
	}

	if result, err := r.applyDomainDeletionPolicy(ctx, &keystone, backend); !result.IsZero() || err != nil {
		return result, err
	}

	return r.removeFinalizer(ctx, backend)
}

// applyDomainDeletionPolicy disables and deletes the managed domain when the
// backend opted into deletionPolicy Delete. Retain (the default) and Adopt
// mode always leave the domain in place (D5).
func (r *KeystoneIdentityBackendReconciler) applyDomainDeletionPolicy(ctx context.Context, keystone *keystonev1alpha1.Keystone, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	if backend.Spec.Domain.Mode != keystonev1alpha1.DomainModeManage ||
		backend.Spec.Domain.DeletionPolicy != keystonev1alpha1.DomainDeletionPolicyDelete ||
		backend.Status.DomainID == "" {
		return ctrl.Result{}, nil
	}

	creds, err := r.adminCredentials(ctx, r.Client, keystone)
	if err != nil {
		if secrets.IsMissingSecretOrKey(err) {
			// The admin credential is gone (stack teardown in progress):
			// deleting the domain is impossible and waiting would hold the
			// backend hostage. Fail open and retain the domain, loudly.
			r.Recorder.Eventf(backend, corev1.EventTypeWarning, "DomainDeleteFailed",
				"Retaining domain %q: admin password Secret is unavailable (%v)", backend.Spec.Domain.Name, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	idc := r.identityClient(internalAPIURL(keystone), creds, nil)

	// Disable first — keystone forbids deleting an enabled domain — then
	// delete (the disable-before-delete contract). A domain already gone
	// (deleted out-of-band) counts as deleted; failures warn and retry on a
	// bounded poll so a transient identity-API outage does not strand the CR.
	switch err := idc.UpdateDomain(ctx, backend.Status.DomainID, ptr.To(false), nil); {
	case errors.Is(err, identity.ErrNotFound):
		return ctrl.Result{}, nil
	case err != nil:
		r.Recorder.Eventf(backend, corev1.EventTypeWarning, "DomainDeleteFailed",
			"Disabling domain %q failed: %v", backend.Spec.Domain.Name, err)
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	r.Recorder.Eventf(backend, corev1.EventTypeNormal, "DomainDisabled",
		"Disabled domain %q before deletion", backend.Spec.Domain.Name)

	if err := idc.DeleteDomain(ctx, backend.Status.DomainID); err != nil && !errors.Is(err, identity.ErrNotFound) {
		r.Recorder.Eventf(backend, corev1.EventTypeWarning, "DomainDeleteFailed",
			"Deleting domain %q failed: %v", backend.Spec.Domain.Name, err)
		return ctrl.Result{RequeueAfter: RequeueDatabaseWait}, nil
	}
	r.Recorder.Eventf(backend, corev1.EventTypeNormal, "DomainDeleted",
		"Deleted domain %q (id %s)", backend.Spec.Domain.Name, backend.Status.DomainID)
	return ctrl.Result{}, nil
}

// removeFinalizer releases the identity-backend finalizer and persists the
// update.
func (r *KeystoneIdentityBackendReconciler) removeFinalizer(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(backend, identityBackendFinalizerName)
	if err := r.Update(ctx, backend); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing identity-backend finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// transientObservationReasons classifies the demotion reasons that describe a
// failed OBSERVATION — the Keystone API, the identity API, or the admin
// credential was temporarily unavailable — as opposed to an authoritative
// de-provisioning finding (domain gone, foreign same-named domain, missing
// mapping rules, unresolvable role/project).
var transientObservationReasons = map[string]struct{}{
	conditionReasonWaitingForKeystoneAPI:  {},
	conditionReasonAdminSecretUnavailable: {},
	conditionReasonIdentityAPIError:       {},
}

// upsertBackendCondition upserts one per-backend condition, PRESERVING a
// provisioned (True) condition against transient-observation demotions.
//
// This asymmetry is load-bearing: projecting an OIDC backend rolls the
// Keystone Deployment and switches the Service targetPort to the sidecar, so
// the Keystone API is briefly unreachable on every attach. If that window
// demoted DomainReady, the keystone-side D-gate (reconcileIdentityBackends)
// would de-project the sidecar, roll the Deployment back, re-observe the
// domain once the API returns, re-project — a self-sustaining oscillation
// that re-rolls the Deployment forever (caught by the oidc-federation e2e).
// A True condition therefore only drops on an authoritative observation;
// transient failures surface through the reconcile error/requeue path and
// events instead.
func upsertBackendCondition(backend *keystonev1alpha1.KeystoneIdentityBackend, condType string, status metav1.ConditionStatus, reason, message string) {
	if status != metav1.ConditionTrue {
		if _, transient := transientObservationReasons[reason]; transient {
			current := conditions.GetCondition(backend.Status.Conditions, condType)
			if current != nil && current.Status == metav1.ConditionTrue {
				return
			}
		}
	}
	conditions.SetCondition(&backend.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: backend.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setDomainReady upserts the DomainReady condition.
func (r *KeystoneIdentityBackendReconciler) setDomainReady(backend *keystonev1alpha1.KeystoneIdentityBackend, status metav1.ConditionStatus, reason, message string) {
	upsertBackendCondition(backend, conditionTypeDomainReady, status, reason, message)
}

// updateStatus persists the backend status via the shared helper: the write
// is skipped when the pass left status unchanged, the aggregate Ready is
// re-derived from the backend type's sub-condition set on every persist, and
// ObservedGeneration is stamped.
func (r *KeystoneIdentityBackendReconciler) updateStatus(ctx context.Context, backend *keystonev1alpha1.KeystoneIdentityBackend, statusBefore *keystonev1alpha1.KeystoneIdentityBackendStatus, result ctrl.Result, reconcileErr error) (ctrl.Result, error) {
	return commonreconcile.UpdateStatus(ctx, r.Client, backend, statusBefore, &backend.Status, func() {
		commonreconcile.SetAggregateReady(&backend.Status.Conditions, backend.Generation, identityBackendSubConditionTypesFor(backend))
		backend.Status.ObservedGeneration = backend.Generation
	}, result, reconcileErr)
}

// SetupWithManager registers the KeystoneIdentityBackendReconciler with the
// controller manager. The KeystoneIdentityBackend field indexes are
// registered by KeystoneReconciler.SetupWithManager (the single registration
// site — both controllers run in one manager and that reconciler is set up
// first in main.go and the envtest helper).
func (r *KeystoneIdentityBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(bootstrap.ControllerOptions(r.MaxConcurrentReconciles)).
		// Filter the CR's own status-only updates so Status().Update does not
		// re-wake the controller (see watch.CRUpdatePredicate).
		For(&keystonev1alpha1.KeystoneIdentityBackend{}, builder.WithPredicates(watch.CRUpdatePredicate())).
		// Watch the referenced Keystone WITHOUT a generation predicate:
		// Keystone status flips (KeystoneAPIReady, IdentityBackendsReady) are
		// exactly the wake signals the DomainReady / ConfigProjected gates
		// wait on. No own Secret watch: bind-secret changes re-render via the
		// Keystone side, and the resulting projection flip reaches this
		// controller through the Keystone watch.
		Watches(&keystonev1alpha1.Keystone{}, handler.EnqueueRequestsFromMapFunc(
			keystoneToIdentityBackendsMapper(mgr.GetClient()),
		)).
		Complete(r)
}
