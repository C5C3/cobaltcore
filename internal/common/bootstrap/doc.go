// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap provides shared controller-runtime manager setup used by
// all CobaltCore operators. Centralising flag parsing, logging, metrics, and
// health-probe wiring here prevents drift when these concerns change.
//
// Feature: CC-0001
package bootstrap
