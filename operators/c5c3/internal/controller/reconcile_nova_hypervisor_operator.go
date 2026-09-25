// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// spec.services.nova.hypervisorOperator provisions the Keystone account
// openstack-hypervisor-operator runs as (desiredNovaHypervisorOperatorRegistration)
// and delivers its credentials as one Secret, "<cp>-nova-hypervisor-operator-auth":
// beside the Nova, and on every compute cluster a NovaCompute of it runs on,
// the same targets the compute contract is mirrored to.

const (
	// reasonWaitingForHypervisorOperatorCredentials is the bounded wait while the
	// registration's consumer Secret carries no password on the Nova's cluster yet.
	reasonWaitingForHypervisorOperatorCredentials = "WaitingForHypervisorOperatorCredentials"
	// reasonHypervisorOperatorError reports a Kubernetes-level failure reading
	// the credentials, or writing or deleting the auth Secret or one of its
	// mirrors.
	reasonHypervisorOperatorError = "HypervisorOperatorError"
)

// novaHypervisorOperatorAuthSecretName returns the name of the auth Secret,
// "<cp>-nova-hypervisor-operator-auth". It is the Nova child's name plus the
// suffix the nova operator's reap derives the same name with.
func novaHypervisorOperatorAuthSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + novav1alpha1.HypervisorOperatorAuthSecretSuffix
}

// novaHypervisorOperatorCredentialsSecretName returns the name of the consumer
// Secret the hypervisor operator's registration delivers,
// "<cp>-nova-hypervisor-operator-credentials", key "password".
func novaHypervisorOperatorCredentialsSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaHypervisorOperatorRegistrationName(cp) + keystoneServiceCredentialsSecretSuffix
}

// desiredNovaHypervisorOperatorAuthSecret builds the auth Secret in the Nova
// namespace. PURE builder: the caller applies it. Each key feeds one value of
// the openstack-hypervisor-operator chart:
//
//	auth_url             controllerManager.manager.env.osAuthUrl
//	username             controllerManager.manager.env.osUsername
//	user_domain_name     controllerManager.manager.env.osUserDomainName
//	project_name         controllerManager.manager.env.osProjectName
//	project_domain_name  controllerManager.manager.env.osProjectDomainName
//	region_name          controllerManager.manager.env.osRegionName
//	password             secret.servicePassword
//
// auth_url is the Keystone URL the catalog advertises: the public one once
// Keystone is published, the in-cluster one before.
func desiredNovaHypervisorOperatorAuthSecret(cp *c5c3v1alpha1.ControlPlane, password []byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      novaHypervisorOperatorAuthSecretName(cp),
			Namespace: cp.NovaNamespace(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"auth_url":            []byte(keystoneCatalogURL(cp)),
			"username":            []byte(c5c3v1alpha1.NovaHypervisorOperatorAccountName),
			"user_domain_name":    []byte(adminDomainName(cp)),
			"project_name":        []byte(c5c3v1alpha1.NovaHypervisorOperatorProjectName),
			"project_domain_name": []byte(adminDomainName(cp)),
			"region_name":         []byte(korcRegion(cp)),
			"password":            password,
		},
	}
	stampControlPlaneChildLabels(secret, cp)
	return secret
}

// reconcileNovaHypervisorOperator drives the hypervisor operator's account
// while spec.services.nova.hypervisorOperator is set. It applies the
// registration, waits for its password, writes the auth Secret beside the Nova
// and copies it into every target. targets are the compute-contract mirror
// targets reconcileNova already resolved. While halt is true the caller returns
// res and err verbatim; every condition is on NovaReady.
//
// reconcileNova runs it after the Nova child is Ready, so a stuck account holds
// NovaReady and never the Nova projection.
//
// The source Secret carries no ComputeConfigMirrorLabel: a NovaCompute on the
// Nova's own cluster reads it where it is, and the reap of the last pool on a
// cluster must never delete it. Each mirror carries the label, so that reap
// recognizes it.
func (r *ControlPlaneReconciler) reconcileNovaHypervisorOperator(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, targets []computeConfigMirrorTarget,
) (ctrl.Result, bool, error) {
	fail := conditionFailer(cp, conditionTypeNovaReady)

	if _, res, halt, err := r.reconcileBuiltinRegistration(ctx, cp,
		desiredNovaHypervisorOperatorRegistration(cp), "Nova hypervisor operator", conditionTypeNovaReady); halt {
		return res, true, err
	}

	novaNS := cp.NovaNamespace()
	source, err := r.childrenClientFor(ctx, cp, novaNS)
	if err != nil {
		fail(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	// The registration's consumer Secret, read on the Nova's cluster: the
	// registration delivers it there itself, or ensureBuiltinRegistrationMirror
	// materializes it there for a placed Nova.
	credsName := novaHypervisorOperatorCredentialsSecretName(cp)
	creds := &corev1.Secret{}
	if err := source.Get(ctx, client.ObjectKey{Namespace: novaNS, Name: credsName}, creds); err != nil &&
		!apierrors.IsNotFound(err) {
		fail(reasonHypervisorOperatorError, err.Error())
		return ctrl.Result{}, true, fmt.Errorf("reading the hypervisor operator credentials Secret %s/%s: %w",
			novaNS, credsName, err)
	}
	password := creds.Data["password"]
	if len(password) == 0 {
		fail(reasonWaitingForHypervisorOperatorCredentials, fmt.Sprintf(
			"the credentials Secret %s/%s of the hypervisor operator account has not been materialized yet",
			novaNS, credsName))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	if err := r.ensureUnownedOrOwned(ctx, source, cp, desiredNovaHypervisorOperatorAuthSecret(cp, password)); err != nil {
		fail(reasonHypervisorOperatorError, err.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the hypervisor operator auth Secret into namespace %q: %w",
			novaNS, err)
	}

	for _, target := range targets {
		delivery, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, target.ClusterRef)
		if err != nil {
			fail(commonmulticluster.TargetClusterUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
		}
		mirror := desiredNovaHypervisorOperatorAuthSecret(cp, password)
		mirror.Namespace = target.Namespace
		mirror.Labels[novav1alpha1.ComputeConfigMirrorLabel] = "true"
		if err := r.ensureUnownedOrOwned(ctx, delivery, cp, mirror); err != nil {
			fail(reasonHypervisorOperatorError, err.Error())
			return ctrl.Result{}, true, fmt.Errorf(
				"mirroring the hypervisor operator auth Secret into namespace %q on cluster %q: %w",
				target.Namespace, clusterNameOf(target.ClusterRef), err)
		}
	}
	return ctrl.Result{}, false, nil
}

// pruneNovaHypervisorOperator removes what reconcileNovaHypervisorOperator
// wrote, while spec.services.nova.hypervisorOperator is unset: the
// registration, whose finalizer removes the user and its project from
// Keystone, the credentials ExternalSecret on a placed Nova's cluster, the
// auth Secret beside the Nova, and the mirror in every target. halt has the
// same meaning as there.
//
// Each delete gates on the live object: a same-named object this ControlPlane
// did not create is left alone, a target Secret without ComputeConfigMirrorLabel
// is not a mirror, and an object that is already gone is no error. While the
// block stays unset this costs 2 + len(targets) reads per Nova pass, one more
// for a placed Nova. A read on another cluster goes through its uncached
// reader (liveReadClient), because this runs by default.
func (r *ControlPlaneReconciler) pruneNovaHypervisorOperator(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, targets []computeConfigMirrorTarget,
) (ctrl.Result, bool, error) {
	fail := conditionFailer(cp, conditionTypeNovaReady)
	owned := func(live client.Object) bool { return isControlPlaneChild(live, cp) }
	failed := func(err error) (ctrl.Result, bool, error) {
		fail(reasonHypervisorOperatorError, err.Error())
		return ctrl.Result{}, true, fmt.Errorf("pruning the hypervisor operator account: %w", err)
	}

	novaNS := cp.NovaNamespace()
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, r.Client, &c5c3v1alpha1.KeystoneService{
		ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorRegistrationName(cp), Namespace: novaNS},
	}, owned); err != nil {
		return failed(err)
	}

	source, err := r.childrenClientFor(ctx, cp, novaNS)
	if err != nil {
		fail(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}
	// ensureBuiltinRegistrationMirror materialized the credentials on a placed
	// Nova's cluster through an ExternalSecret. Left behind, its Secret keeps the
	// deleted user's password, and a block set again would deliver that password
	// until ESO next refreshes it.
	if targetClusterRefForNamespace(cp, novaNS) != nil {
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, liveReadClient{source}, &esov1.ExternalSecret{
			ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorCredentialsSecretName(cp), Namespace: novaNS},
		}, owned); err != nil {
			return failed(err)
		}
	}
	if err := commonreconcile.DeleteOrphanedChildFunc(ctx, liveReadClient{source}, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: novaNS},
	}, owned); err != nil {
		return failed(err)
	}

	for _, target := range targets {
		delivery, err := commonmulticluster.ResolveChildrenClient(ctx, r.Resolver, r.Client, target.ClusterRef)
		if err != nil {
			fail(commonmulticluster.TargetClusterUnavailable, err.Error())
			return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
		}
		if err := commonreconcile.DeleteOrphanedChildFunc(ctx, liveReadClient{delivery}, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: target.Namespace},
		}, func(live client.Object) bool {
			return owned(live) && live.GetLabels()[novav1alpha1.ComputeConfigMirrorLabel] == "true"
		}); err != nil {
			return failed(err)
		}
	}
	return ctrl.Result{}, false, nil
}

// liveReadClient is a client whose reads go through
// commonmulticluster.LiveReader: a target cluster's uncached API reader, or
// the local client itself. Every other call goes to the wrapped client. A
// cached read of a Secret on a compute cluster would start an informer over
// every Secret there and hold them all for the life of the process.
type liveReadClient struct{ client.Client }

func (c liveReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return commonmulticluster.LiveReader(c.Client).Get(ctx, key, obj, opts...)
}
