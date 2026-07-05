// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package naming defines the label and resource-name conventions shared by
// every service operator's workloads (Deployment, Service, PDB, HPA,
// NetworkPolicy, HTTPRoute). It is the single source of truth for the
// app.kubernetes.io selector-label keys and the sub-resource naming scheme,
// so the webhook TSC validation, the workload builders, and every consumer
// that derives a service address by convention agree by construction.
//
// DECISION (cross-service endpoint discovery): the naming convention carried
// by this package IS the cross-service endpoint contract. Consumers such as
// the c5c3 ControlPlane operator derive service URLs by convention —
// http://<name>.<namespace>.svc.cluster.local:<port> over the Service named
// SubResourceName(<cr name>) — rather than reading the producing CR's
// status.endpoint. Keystone keeps publishing Status.Endpoint for human
// consumers (kubectl printcolumns), but nothing machine-consumes it; a
// status-based resolve helper plus cross-CR watch is deliberately NOT built
// until a consumer needs endpoint shapes the convention cannot express.
package naming
