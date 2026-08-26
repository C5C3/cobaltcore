// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
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

// chassisAppName is the app.kubernetes.io/name label value carried by every
// child of an OVNChassis. It is the CR kind in lower case, which is what keeps
// the children of a chassis distinguishable from the control-plane children an
// OVNCentral of the same name projects into the same namespace.
const chassisAppName = "ovnchassis"

// conditionTypeMaintenanceReady is the condition the maintenance step reports
// under: the Jobs that evacuate a gateway node and deregister a leaving
// chassis. The other four chassis condition types are declared in the file of
// the step that sets them.
const conditionTypeMaintenanceReady = "MaintenanceReady"

// chassisSubConditionTypes lists the condition types set by the individual
// OVNChassis sub-reconcilers. The aggregate Ready condition is True only when
// all of them are True, so every sub-reconciler has to set its condition on
// every path it takes, including the ones where it creates nothing.
//
// The order is the pipeline's: the OVNCentral the chassis attach to comes
// first, because its Southbound address and client Secret are what every later
// step is parameterised by, the per-node values next, then the two DaemonSets
// that mount them, and the maintenance Jobs last, since they act on nodes the
// node step has already marked as leaving or as evacuating.
var chassisSubConditionTypes = []string{
	conditionTypeCentralReady,
	conditionTypeNodesReady,
	conditionTypeOVSReady,
	conditionTypeControllerReady,
	conditionTypeMaintenanceReady,
}

// chassisSkeleton bundles the shared controller-skeleton glue (Ready
// aggregation, no-op-skipping status writes, config-failure marking) with the
// OVNChassis sub-condition vocabulary and status accessor.
var chassisSkeleton = commonreconcile.Skeleton[*ovnv1alpha1.OVNChassis, ovnv1alpha1.OVNChassisStatus]{
	SubConditionTypes: chassisSubConditionTypes,
	Conditions:        func(c *ovnv1alpha1.OVNChassis) *[]metav1.Condition { return &c.Status.Conditions },
}

// OVNChassisRemoteChildKinds are the kinds an OVNChassis CR projects into the
// namespace of the target cluster it names, and the kinds the deletion sweep
// selects by ownership label when that CR is deleted. Nothing on the target
// cluster collects them, so a kind missing from this list is a kind that keeps
// running after its CR is gone.
//
// The list is short because a chassis owns no state of its own: the two
// DaemonSets, the two ConfigMaps that carry the per-node values and the scripts
// the pods run, and the maintenance Jobs that evacuate a gateway node and
// deregister a leaving chassis.
var OVNChassisRemoteChildKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
}

// OVNChassisReconciler reconciles an OVNChassis object. Its fields mirror the
// OVNCentral reconciler's core set, minus the cert-manager probe: a chassis
// requests no certificate of its own, it mounts the client Secret the
// OVNCentral it names publishes.
type OVNChassisReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds how many OVNChassis CRs reconcile
	// concurrently. It is threaded from the --max-concurrent-reconciles flag and
	// applied to the controller's controller.Options in SetupWithManager. A value
	// <= 0 falls back to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// Resolver resolves the target cluster an OVNChassis CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the
	// management cluster, which is what single-cluster tests and deployments
	// want.
	Resolver commonmulticluster.ClusterResolver
}
