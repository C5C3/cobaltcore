// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/record"
)

// expectEvent asserts that the FakeRecorder received an event containing the
// given substring (e.g. "Normal DatabaseSynced").
func expectEvent(g Gomega, r *KeystoneReconciler, substring string) {
	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	g.Expect(fakeRecorder.Events).To(Receive(ContainSubstring(substring)))
}

// expectNoEvent asserts that no event was emitted to the FakeRecorder.
func expectNoEvent(g Gomega, r *KeystoneReconciler) {
	fakeRecorder := r.Recorder.(*record.FakeRecorder)
	g.Expect(fakeRecorder.Events).ToNot(Receive())
}
