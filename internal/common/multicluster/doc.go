// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package multicluster resolves the client that carries an operator's child
// reads and writes, based on a CR's optional target-cluster ref. A nil
// resolver or a nil ref selects the local client, the one connected to the
// management cluster the operator runs on.
package multicluster
