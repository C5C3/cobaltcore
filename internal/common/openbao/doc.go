// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package openbao is a minimal HTTP client for the OpenBao paths the
// c5c3-operator calls itself: the Kubernetes-auth login, the database-engine
// roles, lease revocation by prefix and the token's revoke-self. Everything
// else the operators read from OpenBao goes through External Secrets Operator
// objects; this package is not a general SDK.
//
// Login presents the caller's ServiceAccount token and, over mTLS, the client
// certificate the OpenBao listener requires. The returned Client carries the
// issued token on every later request, and Close revokes it.
package openbao
