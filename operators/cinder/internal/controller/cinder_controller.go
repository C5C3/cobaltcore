// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the Cinder, CinderBackend and
// CinderBackupBackend reconcilers.
package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/healthcheck"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
)

// CinderReconciler reconciles a Cinder object: it drives the sub-reconciler
// chain that projects the API, scheduler, volume and backup workloads together
// with the Secrets, config and database children they read.
type CinderReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace is the Namespace the operator Pod runs in (resolved at
	// startup by bootstrap.DetectOperatorNamespace). The networkpolicy step
	// appends an ingress peer for this Namespace so the operator's own health
	// check can reach the Cinder API. Empty when the namespace could not be
	// determined, in which case no operator-namespace peer is added.
	OperatorNamespace string

	// MaxConcurrentReconciles bounds how many Cinder CRs reconcile concurrently.
	// It is threaded from the --max-concurrent-reconciles flag and applied to the
	// controller's controller.Options in SetupWithManager. A value <= 0 falls back
	// to bootstrap.DefaultMaxConcurrentReconciles inside
	// bootstrap.ControllerOptions, so the zero value is safe.
	MaxConcurrentReconciles int

	// HTTPClient is the health-check client seam. Production leaves it nil so the
	// health check uses http.DefaultClient; tests inject a stub transport.
	HTTPClient healthcheck.HTTPDoer

	// Resolver resolves the target cluster a Cinder CR names in
	// spec.targetClusterRef into the client its children are read and written
	// with. Nil means always-local: every CR keeps its children on the management
	// cluster, which is what single-cluster tests and deployments want.
	Resolver commonmulticluster.ClusterResolver
}
