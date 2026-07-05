// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package reconcile provides the generic sub-reconciler pipeline and status
// aggregation scaffolding shared by the service operators: the table-driven
// sequential chain (RunPipeline), the errgroup-backed parallel group
// (RunParallelGroup) with per-member CR copies and condition merge on partial
// failure, RequeueAfter-only requeue aggregation (ShortestRequeue), the
// aggregate Ready condition (SetAggregateReady), and the no-op-skipping
// status writer (UpdateStatus).
//
// Per-operator hooks stay in each operator: what to run per step, the
// condition vocabulary, extra status mutations (e.g. c5c3's setServicesStatus
// or keystone's markConfigFailed), and instrumentation wiring — the pipeline
// accepts the operator's instrument function so metrics keep their
// per-operator prefix.
package reconcile
