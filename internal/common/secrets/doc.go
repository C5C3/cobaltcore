// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package secrets provides helpers for managing Kubernetes Secrets and
// External Secrets Operator resources (ExternalSecrets, PushSecrets).
// It encapsulates readiness checks and secret value retrieval for use
// by CobaltCore operator reconcilers (CC-0005).
package secrets
