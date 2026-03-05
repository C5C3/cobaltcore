// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package database provides helpers for managing MariaDB Operator Database,
// User, and Grant resources, as well as running database synchronisation Jobs.
// It encapsulates create-or-update logic with owner references and readiness
// checks for use by CobaltCore operator reconcilers (CC-0005).
package database
