// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the reconcilers of the OVN operator.
package controller

import (
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// centralSubConditionTypes lists the condition types set by the individual
// OVNCentral sub-reconcilers. The aggregate Ready condition is True only when
// all of them are True, so every sub-reconciler has to set its condition on
// every path it takes, including the ones where it creates nothing.
//
// The order is the pipeline's: the certificates come first because every OVN
// connection is authenticated with them, the two Raft databases next, their
// published addresses after that, and northd, the relay and the backup last,
// since each of those consumes an address the endpoint step publishes.
//
// Every entry references the owning sub-reconciler's own constant, so a rename
// cannot leave a stale literal behind here.
var centralSubConditionTypes = []string{
	conditionTypeTLSReady,
	conditionTypeNorthboundReady,
	conditionTypeSouthboundReady,
	conditionTypeEndpointsReady,
	conditionTypeNorthdReady,
	conditionTypeRelayReady,
	conditionTypeBackupReady,
}

// centralSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with the
// OVNCentral sub-condition vocabulary and status accessor.
var centralSkeleton = commonreconcile.Skeleton[*ovnv1alpha1.OVNCentral, ovnv1alpha1.OVNCentralStatus]{
	SubConditionTypes: centralSubConditionTypes,
	Conditions:        func(c *ovnv1alpha1.OVNCentral) *[]metav1.Condition { return &c.Status.Conditions },
}

// certificateGVK identifies the cert-manager Certificate kind every OVN
// certificate is requested through. cert-manager owns the Secrets those
// Certificates write, which is why the Secret kind stays off the remote-child
// list below.
var certificateGVK = schema.GroupVersionKind{
	Group:   certmanagerv1.SchemeGroupVersion.Group,
	Version: certmanagerv1.SchemeGroupVersion.Version,
	Kind:    "Certificate",
}

// OVNCentralReconciler reconciles an OVNCentral object. Its fields mirror the
// sibling service reconcilers' core set.
type OVNCentralReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many OVNCentral CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// certManagerAvailable is set during SetupWithManager from the management
	// cluster's RESTMapper and says whether the cert-manager.io/v1 Certificate
	// CRD is installed there. commonmulticluster.ChildrenServeKind answers with
	// it for local children while probing the target cluster's RESTMapper for
	// remote ones, and reconcileTLS turns a negative verdict into a
	// TLSReady=False/CertManagerUnavailable wait instead of applying a
	// Certificate the cluster would reject with "no matches for kind
	// Certificate".
	certManagerAvailable bool

	// Resolver resolves the target cluster an OVNCentral CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver
}

// OVNCentralRemoteChildKinds are the kinds an OVNCentral CR projects into the
// namespace of the target cluster it names, and the kinds the deletion sweep
// selects by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// Secret is deliberately absent: every Secret the control plane mounts is a
// cert-manager Certificate's output, and cert-manager deletes it when the
// Certificate goes. The database PersistentVolumeClaims are absent for the same
// reason at one remove (the StatefulSet controller removes them through the
// persistentVolumeClaimRetentionPolicy the volumeClaimTemplates carry), while
// PersistentVolumeClaim itself stays on the list for the backup volume, which no
// controller owns.
var OVNCentralRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	corev1.SchemeGroupVersion.WithKind("Service"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	certificateGVK,
}
