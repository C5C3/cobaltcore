// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the admin-credential sub-reconciler's KORCReady gate, in particular
// the External-mode drift escalation (P6): drift in the external installation is
// surfaced on the existing sub-condition with a dedicated reason, never fought.
package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
)

// reconcileAdminCredentialAtKORCGate runs reconcileAdminCredential against a CR
// whose KORCReady condition is False with the given reason, and returns the
// resulting AdminCredentialReady condition. The gate returns before any client
// call beyond the condition read, so a bare scheme suffices.
func reconcileAdminCredentialAtKORCGate(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, korcReason, korcMessage string,
) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             korcReason,
		Message:            korcMessage,
	})

	s := korcTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	res, err := r.reconcileAdminCredential(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	return conditions.GetCondition(cp.Status.Conditions, conditionTypeAdminCredentialReady)
}

// TestReconcileAdminCredential_ExternalModeDriftEscalatesReason asserts that both
// drift classes on KORCReady — a stale admin password (AuthenticationFailed) and a
// stale resolve-once import id (CredentialDrift) — escalate the gate from the
// opaque WaitingForKORC to the documented CredentialDrift reason, naming the
// Secret the operator reads and stating that it never remediates the external
// installation.
func TestReconcileAdminCredential_ExternalModeDriftEscalatesReason(t *testing.T) {
	for _, korcReason := range []string{
		conditionReasonAuthenticationFailed,
		conditionReasonCredentialDrift,
	} {
		t.Run(korcReason, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := korcExternalControlPlane()

			cond := reconcileAdminCredentialAtKORCGate(t, cp, korcReason, "got 401 instead")

			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonCredentialDrift))
			g.Expect(cond.Message).To(ContainSubstring(`"keystone-admin"`),
				"the message must name the admin-password Secret the operator reads")
			g.Expect(cond.Message).To(ContainSubstring("never remediates"))
			g.Expect(cond.Message).To(ContainSubstring("got 401 instead"),
				"K-ORC's message must be carried through from KORCReady")
		})
	}
}

// TestReconcileAdminCredential_ExternalModeOtherKORCFailuresStayWaitingForKORC is
// the discrimination guard: drift is a statement about the CREDENTIAL. An
// unreachable endpoint, a TLS failure, a catalog mismatch or a stalled import are
// not drift, so the gate must keep its ordinary WaitingForKORC reason.
func TestReconcileAdminCredential_ExternalModeOtherKORCFailuresStayWaitingForKORC(t *testing.T) {
	for _, korcReason := range []string{
		conditionReasonEndpointUnreachable,
		conditionReasonTLSVerificationFailed,
		conditionReasonCatalogEndpointMismatch,
		conditionReasonImportStalled,
	} {
		t.Run(korcReason, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := korcExternalControlPlane()

			cond := reconcileAdminCredentialAtKORCGate(t, cp, korcReason, "some failure")

			g.Expect(cond.Reason).To(Equal("WaitingForKORC"))
			g.Expect(cond.Reason).NotTo(Equal(conditionReasonCredentialDrift))
		})
	}
}

// TestReconcileAdminCredential_ManagedModeGateUnchanged is the AC-2 regression
// guard: a managed ControlPlane never picks up the External-mode drift vocabulary,
// even when its KORCReady carries a reason that would escalate in External mode.
func TestReconcileAdminCredential_ManagedModeGateUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := korcControlPlane()

	cond := reconcileAdminCredentialAtKORCGate(t, cp, conditionReasonAuthenticationFailed, "got 401 instead")

	g.Expect(cond.Reason).To(Equal("WaitingForKORC"))
}
