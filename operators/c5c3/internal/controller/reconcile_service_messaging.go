// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// serviceMessagingCAKey is the data key the CA mirror carries the broker's CA
// bundle under, and the key the projected child's messaging TLS block names.
const serviceMessagingCAKey = "ca.crt"

// serviceMessagingTarget names the consumer the ControlPlane delivers the
// shared bus to.
type serviceMessagingTarget struct {
	Service       string // "Neutron": the display name in the failure reason and the wrapped texts
	ChildName     string // the projected child the Secrets are named after, e.g. neutronName(cp)
	Namespace     string // where the Secrets are written; childrenClientFor resolves its cluster
	ConditionType string // the ControlPlane condition every arm reports on, e.g. conditionTypeNeutronReady
}

// serviceMessagingSecretName returns t.ChildName + "-messaging".
func serviceMessagingSecretName(t serviceMessagingTarget) string {
	return t.ChildName + "-messaging"
}

// serviceMessagingCASecretName returns t.ChildName + "-messaging-ca".
func serviceMessagingCASecretName(t serviceMessagingTarget) string {
	return t.ChildName + "-messaging-ca"
}

// errorReason returns t.Service + "MessagingError".
func (t serviceMessagingTarget) errorReason() string {
	return t.Service + "MessagingError"
}

// reconcileServiceMessaging delivers the ControlPlane-wide message bus
// (spec.infrastructure.messaging) into the namespace the consumer t names. halt
// and the conditions keep their meaning across consumers: every condition is
// written on t.ConditionType, the failure reason is t.errorReason(), and the
// three wrapped texts read "writing the <service> messaging Secret %s/%s: %w",
// "writing the <service> messaging CA Secret %s/%s: %w" and
// "deleting the stale <service> messaging CA Secret %s/%s: %w" with
// strings.ToLower(t.Service).
//
// The bus is declared in the ControlPlane's own namespace and read there, on the
// management cluster. The consumer may run somewhere else: in a namespace of its
// own, or on a target cluster. The service operator resolves spec.messaging in
// the child's own namespace on the child's own cluster and would find nothing
// there, so the ControlPlane resolves the bus itself and hands the child a
// brownfield secretRef pointing at serviceMessagingSecretName(t), written on the
// client that namespace resolves to. A managed clusterRef and a brownfield
// secretRef both arrive as the same one Secret.
//
// A placed consumer receives the URL the bus declares, whatever cluster it runs
// on. Reaching a broker across a cluster boundary is the bus operator's concern,
// and the neutron operator's own D11 rule keeps a pure-OVN Neutron from dialing
// the URL at all.
//
// halt=true means the caller returns res and err as they are and projects no
// child: either the material is not there yet (res requeues, err is nil) or the
// write failed (err is non-nil). halt=false means the URL was delivered. Every
// condition this pass writes is on t.ConditionType.
func (r *ControlPlaneReconciler) reconcileServiceMessaging(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, t serviceMessagingTarget,
) (res ctrl.Result, halt bool, err error) {
	failed := conditionFailer(cp, t.ConditionType)
	service := strings.ToLower(t.Service)

	// The caller only calls this for a ControlPlane that declares a bus, so a
	// missing block is a programming error rather than a state to wait out.
	if cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		err := errors.New("spec.infrastructure.messaging is nil")
		failed(t.errorReason(), err.Error())
		return ctrl.Result{}, true, err
	}

	transportURL, _, waitMessage, err := messaging.ResolveTransportURL(ctx, messaging.TransportURLSecretFlowParams{
		Client:    r.Client,
		Namespace: cp.Namespace,
		Messaging: cp.Spec.Infrastructure.Messaging,
	})
	if err != nil {
		failed(t.errorReason(), err.Error())
		return ctrl.Result{}, true, fmt.Errorf("resolving the shared bus transport URL: %w", err)
	}
	if waitMessage != "" {
		// The RabbitmqCluster, its default-user Secret or the brownfield Secret is
		// not there yet. Nothing is written, so the child never sees a partial URL.
		failed(messaging.ReasonWaitingForMessagingCredentials, waitMessage)
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	namespace := t.Namespace
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	name := serviceMessagingSecretName(t)
	if serr := r.ensureOwnedSecret(ctx, children, cp, name, namespace, func(secret *corev1.Secret) error {
		secret.Data[commonv1.DefaultTransportURLSecretKey] = []byte(transportURL)
		return nil
	}); serr != nil {
		failed(t.errorReason(), serr.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the %s messaging Secret %s/%s: %w",
			service, namespace, name, serr)
	}

	tls := cp.Spec.Infrastructure.Messaging.TLS
	if tls == nil {
		// A plaintext bus leaves no mirror behind, but the mirror is NOT reaped
		// here: the live child still names it, and the projection that drops that
		// pointer is several halting gates further down the pass.
		// pruneServiceMessagingCA runs on the far side of it.
		return ctrl.Result{}, false, nil
	}

	bundleName := tls.CABundleSecretRef.Name
	bundleKey := tls.CABundleSecretRef.Key
	if bundleKey == "" {
		bundleKey = c5c3v1alpha1.DefaultCABundleSecretKey
	}

	// The bundle is read beside the bus it belongs to, on the management cluster,
	// and mirrored to wherever the consumer runs.
	bundleSecret := &corev1.Secret{}
	switch gerr := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: bundleName}, bundleSecret); {
	case apierrors.IsNotFound(gerr):
		failed("WaitingForMessagingCABundle", fmt.Sprintf(
			"messaging CA bundle Secret %s/%s (key %q) named by spec.infrastructure.messaging.tls.caBundleSecretRef "+
				"does not exist", cp.Namespace, bundleName, bundleKey))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	case gerr != nil:
		failed(t.errorReason(), gerr.Error())
		return ctrl.Result{}, true, fmt.Errorf("reading messaging CA bundle Secret %s/%s: %w",
			cp.Namespace, bundleName, gerr)
	}

	// An empty key is the ordinary transient of a two-step "create the Secret,
	// then populate it" flow, so it waits exactly as a missing Secret does rather
	// than mirroring an empty trust anchor.
	bundle := bundleSecret.Data[bundleKey]
	if len(bundle) == 0 {
		failed("WaitingForMessagingCABundle", fmt.Sprintf(
			"messaging CA bundle Secret %s/%s carries no data under key %q", cp.Namespace, bundleName, bundleKey))
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	caName := serviceMessagingCASecretName(t)
	if serr := r.ensureOwnedSecret(ctx, children, cp, caName, namespace, func(secret *corev1.Secret) error {
		secret.Data[serviceMessagingCAKey] = bundle
		return nil
	}); serr != nil {
		failed(t.errorReason(), serr.Error())
		return ctrl.Result{}, true, fmt.Errorf("writing the %s messaging CA Secret %s/%s: %w",
			service, namespace, caName, serr)
	}

	return ctrl.Result{}, false, nil
}

// pruneServiceMessagingCA removes the CA mirror of the consumer t names once the
// shared bus no longer declares TLS. A plaintext bus must leave no trust anchor
// behind in the consumer's namespace, and dropping the tls block has to converge
// that namespace rather than pin the last mirrored bundle.
//
// It is deliberately NOT part of reconcileServiceMessaging, which runs BEFORE the
// projection. The live child's spec.messaging.tls names this Secret as a volume
// source, and every gate between the two — the KeystoneService registration mid
// service-account rotation, a Dynamic DB credential mid rotation, a transient API
// error — halts the pass with the pointer still in place. Deleting the mirror
// there would leave the child naming a Secret that no longer exists, so every pod
// of the consumer that restarts in that window wedges on
// CreateContainerConfigError, with nothing in the condition set naming the cause.
//
// Removing the pointer from the CR is not enough either: the workload that mounts
// the mirror is rendered by the service operator, one pass behind the apply. The
// caller therefore runs this only once the child reports having converged on the
// pointer-free spec — see the gate in reconcileNeutron — so the referent never
// outlives its last reference.
//
// A same-named Secret this ControlPlane never wrote is left alone. halt has the
// same meaning as in reconcileServiceMessaging, and every condition is on
// t.ConditionType.
func (r *ControlPlaneReconciler) pruneServiceMessagingCA(
	ctx context.Context, cp *c5c3v1alpha1.ControlPlane, t serviceMessagingTarget,
) (res ctrl.Result, halt bool, err error) {
	failed := conditionFailer(cp, t.ConditionType)
	service := strings.ToLower(t.Service)

	namespace := t.Namespace
	children, err := r.childrenClientFor(ctx, cp, namespace)
	if err != nil {
		failed(commonmulticluster.TargetClusterUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: infraRequeueAfter}, true, nil
	}

	caName := serviceMessagingCASecretName(t)
	if derr := commonreconcile.DeleteOrphanedChildFunc(ctx, children, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caName, Namespace: namespace},
	}, func(live client.Object) bool { return isControlPlaneChild(live, cp) }); derr != nil {
		failed(t.errorReason(), derr.Error())
		return ctrl.Result{}, true, fmt.Errorf("deleting the stale %s messaging CA Secret %s/%s: %w",
			service, namespace, caName, derr)
	}
	return ctrl.Result{}, false, nil
}

// serviceMessagingSpec returns the spec.messaging the projected child receives:
// a brownfield SecretRef {Name: serviceMessagingSecretName(t), Key:
// commonv1.DefaultTransportURLSecretKey} and, only while
// cp.Spec.Infrastructure.Messaging.TLS is non-nil, a TLS block whose
// CABundleSecretRef is {Name: serviceMessagingCASecretName(t), Key: serviceMessagingCAKey}.
func serviceMessagingSpec(cp *c5c3v1alpha1.ControlPlane, t serviceMessagingTarget) commonv1.MessagingSpec {
	spec := commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{
			Name: serviceMessagingSecretName(t),
			Key:  commonv1.DefaultTransportURLSecretKey,
		},
	}
	if cp.Spec.Infrastructure != nil && cp.Spec.Infrastructure.Messaging != nil &&
		cp.Spec.Infrastructure.Messaging.TLS != nil {
		spec.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{
				Name: serviceMessagingCASecretName(t),
				Key:  serviceMessagingCAKey,
			},
		}
	}
	return spec
}

// messagingCAMirrorReleasable reports whether the mirror may be reaped: the
// shared bus declares no tls, child (the applied child's spec.messaging) names
// no TLS block, and observedGeneration >= generation. It reads
// cp.Spec.Infrastructure.Messaging and returns false when that is nil.
func messagingCAMirrorReleasable(
	cp *c5c3v1alpha1.ControlPlane, child commonv1.MessagingSpec, observedGeneration, generation int64,
) bool {
	if cp.Spec.Infrastructure == nil || cp.Spec.Infrastructure.Messaging == nil {
		return false
	}
	return cp.Spec.Infrastructure.Messaging.TLS == nil && child.TLS == nil &&
		observedGeneration >= generation
}

// serviceMessagingSecrets returns the two Secret stubs the teardown paths name,
// transport URL first, in t.Namespace.
func serviceMessagingSecrets(t serviceMessagingTarget) []client.Object {
	return []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: serviceMessagingSecretName(t), Namespace: t.Namespace},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: serviceMessagingCASecretName(t), Namespace: t.Namespace},
		},
	}
}
