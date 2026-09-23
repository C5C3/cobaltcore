// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi"
)

// The reasons of the NovaReady condition.
const (
	conditionReasonNovaResolved                = "NovaResolved"
	conditionReasonNovaNotFound                = "NovaNotFound"
	conditionReasonWaitingForInstalledRelease  = "WaitingForInstalledRelease"
	conditionReasonComputeConfigNotPublished   = "ComputeConfigNotPublished"
	conditionReasonWaitingForServiceUserSecret = "WaitingForServiceUserSecret"
	conditionReasonNovaError                   = "NovaError"
)

// novaComputeDefaultRepository is the image a NovaCompute runs when spec.image
// is nil, tagged with the Nova's installed release.
const novaComputeDefaultRepository = "ghcr.io/c5c3/nova-compute"

// reconcileNovaComputeNova resolves the Nova the CR names and everything the
// later steps take from it: the image, the compute-contract Secret, and the
// transport and credentials of the API calls. It sets NovaReady.
//
// The Nova is read on the management cluster, where every Nova lives. Its
// service-user Secret and its API are reached on the cluster the Nova is
// placed on, which need not be the pool's.
//
// The default image follows status.installedRelease, not spec.openStackRelease:
// the control plane upgrades first, and a pool rolls to the new release only
// once the Nova has migrated its schemas to it.
func (r *NovaComputeReconciler) reconcileNovaComputeNova(ctx context.Context, cr *novav1alpha1.NovaCompute,
	pass *novaComputePass,
) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.NovaRef.Name}
	nova := &novav1alpha1.Nova{}
	if err := r.Get(ctx, key, nova); err != nil {
		if apierrors.IsNotFound(err) {
			return waitOnNova(cr, conditionReasonNovaNotFound, fmt.Sprintf("Nova %s not found", key))
		}
		err = fmt.Errorf("getting Nova %s: %w", key, err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNovaReady, conditionReasonNovaError, err)
		return ctrl.Result{}, err
	}

	if nova.Status.InstalledRelease == "" {
		return waitOnNova(cr, conditionReasonWaitingForInstalledRelease,
			fmt.Sprintf("Nova %s has not installed a release yet", key))
	}
	if nova.Status.ComputeConfigSecretRef == nil {
		return waitOnNova(cr, conditionReasonComputeConfigNotPublished,
			fmt.Sprintf("Nova %s has not published its compute contract yet", key))
	}

	// An injected HTTPClient wins whenever it is set: it is the test seam, and
	// no binary sets it. Otherwise a placed Nova is reached through its
	// cluster's service proxy, because its Service URLs resolve on that cluster
	// and nowhere else.
	var doer computeapi.Doer = r.HTTPClient
	if r.HTTPClient == nil {
		resolved, err := commonmulticluster.ResolveHTTPDoer(ctx, r.Resolver, nova.Spec.TargetClusterRef, http.DefaultClient)
		if err != nil {
			return waitOnNova(cr, commonmulticluster.TargetClusterUnavailable, err.Error())
		}
		doer = resolved
	}
	novaChildren, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, nova.Spec.TargetClusterRef)
	if err != nil {
		return waitOnNova(cr, commonmulticluster.TargetClusterUnavailable, err.Error())
	}

	secretKey := types.NamespacedName{Namespace: nova.Namespace, Name: nova.Spec.ServiceUser.SecretRef.Name}
	dataKey := effectiveServiceUserKey(nova)
	secret := &corev1.Secret{}
	if err := novaChildren.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return waitOnNova(cr, conditionReasonWaitingForServiceUserSecret,
				fmt.Sprintf("service-user Secret %s of Nova %s not found", secretKey, key))
		}
		err = fmt.Errorf("getting service-user Secret %s: %w", secretKey, err)
		novaComputeSkeleton.MarkFailed(cr, conditionTypeNovaReady, conditionReasonNovaError, err)
		return ctrl.Result{}, err
	}
	password := string(secret.Data[dataKey])
	if password == "" {
		return waitOnNova(cr, conditionReasonWaitingForServiceUserSecret,
			fmt.Sprintf("service-user Secret %s carries no %q key", secretKey, dataKey))
	}

	image := commonv1.ImageSpec{Repository: novaComputeDefaultRepository, Tag: nova.Status.InstalledRelease}
	if cr.Spec.Image != nil {
		image = *cr.Spec.Image
	}

	pass.image = image
	pass.secretName = nova.Status.ComputeConfigSecretRef.Name
	pass.doer = doer
	pass.keystoneURL = nova.Spec.KeystoneEndpoint
	pass.computeURL = internalNovaURL(nova)
	pass.creds = computeapi.Credentials{
		Username:          nova.Spec.ServiceUser.Username,
		Password:          password,
		ProjectName:       nova.Spec.ServiceUser.ProjectName,
		UserDomainName:    nova.Spec.ServiceUser.UserDomainName,
		ProjectDomainName: nova.Spec.ServiceUser.ProjectDomainName,
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNovaReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonNovaResolved,
		Message:            fmt.Sprintf("Nova %s runs release %s", key, nova.Status.InstalledRelease),
	})
	return ctrl.Result{}, nil
}

// waitOnNova sets NovaReady False for a wait on the Nova and requeues.
func waitOnNova(cr *novav1alpha1.NovaCompute, reason, message string) (ctrl.Result, error) {
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNovaReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            message,
	})
	return ctrl.Result{RequeueAfter: commonreconcile.RequeueSecretPolling}, nil
}
