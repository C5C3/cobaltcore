// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Nova reconciler.
package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// subConditionTypes lists the condition types set by the individual Nova
// sub-reconcilers. The aggregate Ready condition is True only when all of these
// are True. Every parallel-group member (HPA, NetworkPolicy and the three
// HTTPRoutes) always sets its condition, configured-ready, NotRequired, or
// waiting, so a gateway-less or autoscaling-less cluster still resolves the
// aggregate (the NotRequired paths report True), exactly as the sibling
// operators aggregate their optional conditions.
//
// ExtraConfigHealthy stays out: it reports on an overlay the user owns, is
// informational, and must not depool a Nova whose API serves fine.
var subConditionTypes = []string{
	"SecretsReady",
	"ComputeConfigReady",
	"DatabaseReady",
	"ConductorReady",
	"SchedulerReady",
	"MetadataReady",
	"ConsoleProxyReady",
	"DeploymentReady",
	"DBArchiveReady",
	"NovaAPIReady",
	"HPAReady",
	"NetworkPolicyReady",
	"HTTPRouteReady",
	"MetadataHTTPRouteReady",
	"ConsoleHTTPRouteReady",
}

// novaSkeleton bundles the shared controller-skeleton glue (Ready aggregation,
// no-op-skipping status writes, config-failure marking) with nova's
// sub-condition vocabulary and status accessor. The wrapper helper below
// delegates to it.
var novaSkeleton = commonreconcile.Skeleton[*novav1alpha1.Nova, novav1alpha1.NovaStatus]{
	SubConditionTypes: subConditionTypes,
	Conditions:        func(n *novav1alpha1.Nova) *[]metav1.Condition { return &n.Status.Conditions },
}

// NovaReconciler reconciles a Nova object: it drives the sub-reconciler chain
// that projects the API, metadata, scheduler, conductor and console-proxy
// workloads together with the Secrets, config and database children they read.
type NovaReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Nova API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Nova CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to the
	// controller's controller.Options in SetupWithManager. A value <= 0 falls back
	// to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// Resolver resolves the target cluster a Nova CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the management
	// cluster, which is what single-cluster tests and deployments want.
	Resolver commonmulticluster.ClusterResolver
}

// setReadyCondition sets the aggregate Ready condition based on all
// sub-conditions, delegating to the shared skeleton with nova's sub-condition
// vocabulary.
func setReadyCondition(nova *novav1alpha1.Nova) {
	novaSkeleton.SetReady(nova)
}
