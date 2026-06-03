// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package fake_crds contains minimal CRD YAML schemas for external operators
// used in envtest integration tests. These are NOT production-grade CRDs — they
// provide just enough schema for envtest to register the types and allow
// creating/reading CRs during testing.
//
// CRDs are grouped by controller in subdirectories:
//
//	cert-manager/        — cert-manager.io CRDs (Certificate, ClusterIssuer)
//	external-secrets/    — external-secrets.io CRDs (ExternalSecret, PushSecret, ClusterSecretStore)
//	k-orc/               — openstack.k-orc.cloud CRDs (ApplicationCredential, Service, Endpoint)
//	mariadb-operator/    — k8s.mariadb.com CRDs (MariaDB, Database, Grant, User)
//	memcached-operator/  — memcached.c5c3.io CRDs (Memcached)
//	rabbitmq-operator/   — rabbitmq.com CRDs (RabbitmqCluster)
//
// Feature: CC-0002, CC-0110
package fake_crds
