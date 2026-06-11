// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// Requeue intervals used by the ControlPlane sub-reconcilers while waiting on a
// projected child CR to converge (CC-0110, REQ-008, REQ-009). Centralised here
// so the wait cadence is consistent and tunable in one place.
const (
	// infraRequeueAfter is the backoff used while a managed MariaDB/Memcached
	// child is still converging to Ready.
	infraRequeueAfter = 15 * time.Second

	// dbCredentialsRequeueAfter is the backoff the DB-credentials sub-reconciler
	// uses while waiting for the per-ControlPlane DB-credential ExternalSecret to
	// sync to Ready (CC-0116, REQ-001, REQ-008). DECISION: a dedicated named
	// constant (rather than reusing korcRequeueAfter) matches the
	// per-sub-reconciler naming convention already established here, so the wait
	// cadence of each sub-reconciler is independently documented and tunable.
	dbCredentialsRequeueAfter = 10 * time.Second

	// adminPasswordRequeueAfter is the backoff the AdminPassword sub-reconciler
	// uses while waiting for the per-ControlPlane admin-password ExternalSecret to
	// sync to Ready (CC-0117, REQ-003). DECISION: a dedicated named constant
	// (rather than reusing korcRequeueAfter) matches the per-sub-reconciler naming
	// convention already established here, so the wait cadence of each
	// sub-reconciler is independently documented and tunable.
	adminPasswordRequeueAfter = 10 * time.Second

	// keystoneInfraGateRequeueAfter is the short backoff used while the Keystone
	// sub-reconciler is gated on InfrastructureReady; it is small so the Keystone
	// CR is projected promptly once the infrastructure converges.
	keystoneInfraGateRequeueAfter = 5 * time.Second

	// korcRequeueAfter is the backoff used by the K-ORC / admin-credential /
	// catalog sub-reconcilers while waiting on a gate (KORCReady,
	// AdminCredentialReady, the K-ORC clouds.yaml ExternalSecret) or on a K-ORC
	// child CR (ApplicationCredential/Service/Endpoint) to converge, and while a
	// missing K-ORC CRD keeps the sub-reconciler from making progress
	// (CC-0110, REQ-010, REQ-011, REQ-014).
	korcRequeueAfter = 10 * time.Second

	// credentialRotationWaitInterval is the short backoff the CredentialRotation
	// reconciler uses while waiting for the ControlPlane reconciler to mint the
	// admin ApplicationCredential CR (Bootstrap) or for a ControlPlane / admin
	// password Secret to appear (CC-0110, REQ-015).
	credentialRotationWaitInterval = 10 * time.Second

	// remintStallTimeout bounds how long the admin ApplicationCredential may stay
	// Terminating during a re-mint before reconcileKORC escalates KORCReady from
	// the transient "ReMinting" reason to "ReMintStalled". A stuck finalizer (e.g.
	// K-ORC cannot reach Keystone to revoke the old credential) otherwise loops on
	// "ReMinting" indefinitely with no operator-visible signal. The window is
	// generous so a slow-but-progressing revoke is not flagged as stalled.
	remintStallTimeout = 5 * time.Minute
)
