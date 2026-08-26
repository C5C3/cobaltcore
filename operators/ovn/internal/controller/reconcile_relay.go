// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"path"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeRelayReady is the condition the relay step reports under. It is
// set on every path, the one where spec.relay is absent included: the aggregate
// Ready is True only when every sub-condition is, so a step that created
// nothing still has to say so.
const conditionTypeRelayReady = "RelayReady"

// The condition reasons of the relay step that the two Deployment steps do not
// share.
const (
	conditionReasonRelayNotRequired = "RelayNotRequired"
	conditionReasonServicePending   = "ServicePending"
)

// componentRelay is the component-label value and the name suffix of the relay
// children.
const componentRelay = "sb-relay"

// reconcileRelay projects the ovsdb-server relays in front of the Southbound
// database, and removes them again when spec.relay is cleared.
//
// A relay is a read-through cache: it holds an open connection to the Raft
// cluster, serves every chassis read from its own copy, and forwards only the
// writes. That is what takes the per-chassis connection load off the leader,
// which past a few hundred nodes is what limits the cluster.
func (r *OVNCentralReconciler) reconcileRelay(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	if cr.Spec.Relay == nil {
		if err := r.deleteRelay(ctx, children, cr); err != nil {
			centralSkeleton.MarkFailed(cr, conditionTypeRelayReady, conditionReasonDeploymentError, err)
			return ctrl.Result{}, err
		}
		// The address goes with the relays. A chassis handed a cluster IP whose
		// Service no longer exists waits out its own timeout instead of falling
		// back to the database it can still reach.
		cr.Status.RelayAddress = ""
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRelayReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonRelayNotRequired,
			Message:            "spec.relay is not set; clients connect to the Southbound database directly",
		})
		return ctrl.Result{}, nil
	}

	if cr.Status.Southbound.InternalDbAddress == "" {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRelayReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonWaitingForEndpoints,
			Message:            "Waiting for the Southbound database address to be published",
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	deploy := buildRelayDeployment(cr)
	ready, err := deployment.EnsureDeployment(ctx, children, r.Scheme, cr, deploy)
	if err != nil {
		err = fmt.Errorf("ensuring sb-relay Deployment: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeRelayReady, conditionReasonDeploymentError, err)
		return ctrl.Result{}, err
	}

	// Both children report under DeploymentError, whichever of the two failed:
	// the step is one unit of work, and a reason per object would put whichever
	// happened to fail first into a field consumers match on.
	svc := buildRelayService(cr)
	if err := deployment.EnsureService(ctx, children, r.Scheme, cr, svc); err != nil {
		err = fmt.Errorf("ensuring sb-relay Service: %w", err)
		centralSkeleton.MarkFailed(cr, conditionTypeRelayReady, conditionReasonDeploymentError, err)
		return ctrl.Result{}, err
	}

	// The cluster IP is read back live rather than taken from the applied
	// object: the API server assigns it, the builder never sets it, and a cache
	// that has not caught up would report the Service as unassigned.
	live := &corev1.Service{}
	if err := commonmulticluster.LiveReader(children).Get(ctx, client.ObjectKeyFromObject(svc), live); err != nil {
		err = fmt.Errorf("reading sb-relay Service %s: %w", svc.Name, err)
		centralSkeleton.MarkFailed(cr, conditionTypeRelayReady, conditionReasonDeploymentError, err)
		return ctrl.Result{}, err
	}
	if live.Spec.ClusterIP == "" {
		// Reported ahead of the Deployment's own state: without an address the
		// relays are unreachable however many of them are running, and that is
		// the more useful thing to name.
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRelayReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonServicePending,
			Message:            fmt.Sprintf("Waiting for Service %s to be assigned a cluster IP", svc.Name),
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}
	cr.Status.RelayAddress = fmt.Sprintf("ssl:%s:%d", live.Spec.ClusterIP, southboundDB(cr).clientPort)

	if !ready {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeRelayReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonDeploymentProgressing,
			Message:            "Waiting for the sb-relay Deployment to become available",
		})
		return ctrl.Result{RequeueAfter: commonreconcile.RequeueDeploymentPolling}, nil
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeRelayReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonDeploymentReady,
		Message:            fmt.Sprintf("The sb-relay Deployment is available at %s", cr.Status.RelayAddress),
	})
	return ctrl.Result{}, nil
}

// deleteRelay removes the relay children of a CR that no longer asks for them,
// the Certificate the TLS step requested for the tier included: a keypair with
// nothing left to authenticate is one more copy of the CA's trust lying around.
func (r *OVNCentralReconciler) deleteRelay(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) error {
	if err := deleteRelayChild(ctx, children, r.Scheme, cr, &appsv1.Deployment{}, "Deployment"); err != nil {
		return err
	}
	if err := deleteRelayChild(ctx, children, r.Scheme, cr, &corev1.Service{}, "Service"); err != nil {
		return err
	}
	return deleteRelayChild(ctx, children, r.Scheme, cr, &certmanagerv1.Certificate{}, "Certificate")
}

// deleteRelayChild deletes one relay child when it exists and this CR controls
// it. obj is an empty instance of the child's kind and is read into; kind names
// it in the error messages.
//
// The ownership check is what keeps the delete off an object somebody else
// provisioned under the name this CR's relay Service happens to want. On a
// target cluster no owner reference exists to answer that question, which is why
// Controls is asked rather than the reference read directly.
func deleteRelayChild(ctx context.Context, children client.Client, scheme *runtime.Scheme,
	cr *ovnv1alpha1.OVNCentral, obj client.Object, kind string,
) error {
	key := client.ObjectKey{Namespace: cr.Namespace, Name: relayName(cr)}
	switch err := commonmulticluster.LiveReader(children).Get(ctx, key, obj); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading sb-relay %s %s before deleting it: %w", kind, key.Name, err)
	}

	owned, err := commonmulticluster.Controls(scheme, cr, obj)
	if err != nil {
		return fmt.Errorf("checking ownership of sb-relay %s %s: %w", kind, key.Name, err)
	}
	if !owned {
		return nil
	}

	if err := client.IgnoreNotFound(children.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("deleting sb-relay %s %s: %w", kind, key.Name, err)
	}
	return nil
}

// relayName names the relay Deployment and the Service in front of it.
func relayName(cr *ovnv1alpha1.OVNCentral) string {
	return cr.Name + "-" + componentRelay
}

// effectiveRelayDeployment resolves the deployment knobs of the relay: the two
// the relay spec carries, with the shared defaults applied on top. The relay has
// no spec.deployment block of its own because it is a stateless cache with
// nothing to drain and no rollout ordering to respect.
func effectiveRelayDeployment(cr *ovnv1alpha1.OVNCentral) commonv1.DeploymentSpec {
	spec := commonv1.DeploymentSpec{
		Replicas:  cr.Spec.Relay.Replicas,
		Resources: cr.Spec.Relay.Resources,
	}
	spec.Default()
	return spec
}

// buildRelayDeployment builds the relay Deployment.
//
// The relays listen with a keypair of their own rather than with the Southbound
// database's: ovn-ctl configures a relay with one certificate for both
// directions, so whatever it listens with it also dials the Raft cluster with,
// and the database's server key in a tier every chassis in the fleet connects
// to would make one relay pod the database's identity. There is no liveness
// probe, for the reason northd has none either.
func buildRelayDeployment(cr *ovnv1alpha1.OVNCentral) *appsv1.Deployment {
	sb := southboundDB(cr)
	knobs := effectiveRelayDeployment(cr)

	return deployment.BuildWorkload(deployment.WorkloadParams{
		Namespace:      cr.Namespace,
		Name:           relayName(cr),
		Labels:         naming.ComponentLabels(centralAppName, cr.Name, componentRelay),
		SelectorLabels: componentSelectorLabels(cr, componentRelay),
		Deployment:     &knobs,
		Container: deployment.ContainerParams{
			Name:  "relay",
			Image: effectiveImage(cr.Spec.Image).Reference(),
			// The relay listens on the connection row the Southbound database
			// already carries (pssl:6642:0.0.0.0), which it reads through the
			// remote it is pointed at, so no listener option is passed here.
			Command: []string{
				"/usr/share/ovn/scripts/ovn-ctl",
				"--db-sb-relay-remote=" + cr.Status.Southbound.InternalDbAddress,
				"--ovn-sb-relay-db-ssl-key=" + path.Join(ovnTLSDir, "tls.key"),
				"--ovn-sb-relay-db-ssl-cert=" + path.Join(ovnTLSDir, "tls.crt"),
				"--ovn-sb-relay-db-ssl-ca-cert=" + path.Join(ovnTLSDir, "ca.crt"),
				"run_sb_relay_ovsdb",
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{
						"ovsdb-client",
						"-p", path.Join(ovnTLSDir, "tls.key"),
						"-c", path.Join(ovnTLSDir, "tls.crt"),
						"-C", path.Join(ovnTLSDir, "ca.crt"),
						"list-dbs",
						fmt.Sprintf("ssl:127.0.0.1:%d", sb.clientPort),
					}},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      5,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: runVolumeName, MountPath: ovnRunDir},
				{Name: logVolumeName, MountPath: ovnLogDir},
				{Name: tmpVolumeName, MountPath: "/tmp"},
				{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true},
			},
		},
		Volumes: append(ovnScratchVolumes(), corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: relayName(cr)},
			},
		}),
	})
}

// buildRelayService builds the Service the chassis reach the relays through. It
// is a plain load-balanced Service, unlike the per-member Services of the
// database: every relay serves the same cached copy, so any of them can answer.
func buildRelayService(cr *ovnv1alpha1.OVNCentral) *corev1.Service {
	port := southboundDB(cr).clientPort
	return deployment.BuildService(cr.Namespace, relayName(cr),
		naming.ComponentLabels(centralAppName, cr.Name, componentRelay),
		componentSelectorLabels(cr, componentRelay), port, port)
}
