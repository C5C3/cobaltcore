// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package webhook holds the scaffolding every CobaltCore admission webhook
// repeats verbatim: the never-invoked ValidateDelete that satisfies the
// Validator interface. The defaulting and validation logic itself stays with
// each operator's API package, where it genuinely differs.
package webhook
