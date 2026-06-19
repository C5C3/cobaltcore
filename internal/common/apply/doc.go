// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package apply provides a generic Server-Side Apply create-or-update primitive
// for the Ensure* family of helpers. EnsureObject manages only the fields the
// operator's builders actually set, so server-defaulted fields stop
// participating in the diff and reconciliation converges instead of issuing an
// Update on every pass.
package apply
