// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package networkpolicy provides the NetworkPolicy scaffolding shared by the
// service operators: the SSA ensure / delete helpers, the auto-derived egress
// rule builders (DNS, database, cache) and cache-port parsing, the ingress-peer
// assembly over the shared ingress-source type plus the gateway- and
// operator-namespace peers, and the three-path reconcile flow (enabled /
// disabled / fail-closed guard). Each operator keeps only its service-specific
// egress tail (the kube-apiserver, federation, or keystone-endpoint rules) and
// its ingress target port.
package networkpolicy
