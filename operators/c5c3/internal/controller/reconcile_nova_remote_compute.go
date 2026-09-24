// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The NovaReady reasons of the remote bus delivery.
const (
	// reasonWaitingForRemoteMessaging reports a handed Secret, or the key inside
	// it, that is not there yet.
	reasonWaitingForRemoteMessaging = "WaitingForRemoteMessaging"
	// reasonNovaRemoteMessagingError reports a handed URL that cannot be used,
	// and a failed write or delete of the delivered Secret.
	reasonNovaRemoteMessagingError = "NovaRemoteMessagingError"
)

// novaRemoteMessagingSecretName returns the name of the Secret the ControlPlane
// delivers the handed external bus URL under, beside the Nova child
// ("<cp>-nova-remote-messaging"). The name is the ControlPlane's own, like
// novaMessagingSecretName, so no controller in that namespace claims it.
func novaRemoteMessagingSecretName(cp *c5c3v1alpha1.ControlPlane) string {
	return novaName(cp) + "-remote-messaging"
}

// novaRemoteComputeSpec returns the spec.remoteCompute the Nova child receives:
// nil while services.nova.remoteCompute is unset, and otherwise the public
// Keystone URL the ControlPlane registers in the catalog, paired with the Secret
// reconcileNovaRemoteMessaging delivers.
//
// The Keystone URL is empty only for a ControlPlane that bypassed the webhook
// with no Keystone publication. The Nova CRD's MinLength marker then refuses the
// child, which NovaProjectionRejected reports.
func novaRemoteComputeSpec(cp *c5c3v1alpha1.ControlPlane) *novav1alpha1.NovaRemoteComputeSpec {
	if cp.Spec.Services.Nova == nil || cp.Spec.Services.Nova.RemoteCompute == nil {
		return nil
	}
	return &novav1alpha1.NovaRemoteComputeSpec{
		KeystoneEndpoint: keystonePublicEndpoint(cp.Spec.Services.Keystone),
		TransportURLSecretRef: commonv1.SecretRefSpec{
			Name: novaRemoteMessagingSecretName(cp),
			Key:  commonv1.DefaultTransportURLSecretKey,
		},
	}
}

// reconcileNovaRemoteMessaging carries the handed external bus URL into the
// Nova namespace. The handed Secret lives in the ControlPlane's namespace on the
// management cluster, while the nova operator reads spec.remoteCompute in the
// Nova's own namespace on the Nova's own cluster. So the ControlPlane reads the
// URL and writes it beside the child, the way reconcileServiceMessaging delivers
// the shared bus.
//
// halt has the meaning it has there: true means the caller returns res and err
// and projects no child, false means the URL was delivered or nothing was asked
// for. Every condition this writes is on NovaReady.
func (r *ControlPlaneReconciler) reconcileNovaRemoteMessaging(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane,
) (res ctrl.Result, halt bool, err error) {
	nv := cp.Spec.Services.Nova
	if nv == nil || nv.RemoteCompute == nil {
		return ctrl.Result{}, false, nil
	}
	failed := conditionFailer(cp, conditionTypeNovaReady)

	ref := nv.RemoteCompute.TransportURLSecretRef
	transportURL, _, waitMessage, err := messaging.ResolveTransportURL(ctx, messaging.TransportURLSecretFlowParams{
		Client:    r.Client,
		Namespace: cp.Namespace,
		Messaging: &commonv1.MessagingSpec{SecretRef: &ref},
	})
	if err != nil {
		// The resolver never quotes the URL, which carries the broker password.
		failed(reasonNovaRemoteMessagingError, err.Error())
		return ctrl.Result{}, true, fmt.Errorf("resolving the nova remote transport URL: %w", err)
	}
	if waitMessage != "" {
		failed(reasonWaitingForRemoteMessaging, "services.nova.remoteCompute.transportURLSecretRef: "+waitMessage)
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	namespace := cp.NovaNamespace()
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	name := novaRemoteMessagingSecretName(cp)
	if serr := r.ensureOwnedSecret(ctx, children, cp, name, namespace, func(secret *corev1.Secret) error {
		secret.Data[commonv1.DefaultTransportURLSecretKey] = []byte(transportURL)
		return nil
	}); serr != nil {
		failed(reasonNovaRemoteMessagingError, serr.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the nova remote messaging Secret %s/%s: %w",
			namespace, name, serr)
	}
	return ctrl.Result{}, false, nil
}

// reapNovaRemoteMessaging removes the delivered remote bus Secret once
// services.nova.remoteCompute is gone. Like pruneServiceMessagingCA it runs past
// the child's readiness and waits for the child's own verdict: until the nova
// operator has reconciled the generation that dropped spec.remoteCompute, it
// still reads the Secret on every pass. A same-named Secret this ControlPlane
// never wrote is left alone.
func (r *ControlPlaneReconciler) reapNovaRemoteMessaging(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, nv *novav1alpha1.Nova,
) (res ctrl.Result, halt bool, err error) {
	if cp.Spec.Services.Nova.RemoteCompute != nil || nv.Status.ObservedGeneration < nv.Generation {
		return ctrl.Result{}, false, nil
	}
	failed := conditionFailer(cp, conditionTypeNovaReady)

	namespace := cp.NovaNamespace()
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	name := novaRemoteMessagingSecretName(cp)
	if derr := commonreconcile.DeleteOrphanedChildFunc(ctx, children, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}, func(live client.Object) bool { return isControlPlaneChild(live, cp) }); derr != nil {
		failed(reasonNovaRemoteMessagingError, derr.Error())
		return ctrl.Result{}, true, fmt.Errorf("deleting the stale nova remote messaging Secret %s/%s: %w",
			namespace, name, derr)
	}
	return ctrl.Result{}, false, nil
}
