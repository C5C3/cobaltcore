// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package job manages Kubernetes Jobs and CronJobs for CobaltCore operators.
// It provides functions for creating one-shot Jobs, ensuring CronJobs exist,
// and checking Job completion status, and it resolves and applies the pod
// settings (resources, priority class, node placement) of every Job and
// CronJob pod.
package job
