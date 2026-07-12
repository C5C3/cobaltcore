// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package watch provides the generic watch scaffolding shared by the service
// operators: the CR update predicate that filters a controller's own
// status-only writes (CRUpdatePredicate), the field-indexed Secret→CR event
// mapper with an optional owner-reference leg (SecretToOwnersMapper) plus its
// indexer registration helper (RegisterSecretNameIndex), and the secret-store
// fan-out that enqueues the CRs whose effective store ref matches a changed
// ClusterSecretStore or namespaced SecretStore (StoreRefFanOut).
//
// Operator-specific mappers with a single consumer (e.g. keystone's MariaDB
// clusterRef mapper and its PushSecret name mapper/predicate) deliberately
// stay in their operator — the rule of two is not met for them.
package watch
