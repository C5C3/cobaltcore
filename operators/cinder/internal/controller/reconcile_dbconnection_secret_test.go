// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
)

func TestReconcileDBConnectionSecret_DerivesSecretAndDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	r := newCinderTestReconciler(cinder, cinderDBSecret())

	res, digest, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())

	// The derived <cinder>-db-connection Secret is materialised with the DSN.
	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: database.ConnectionSecretName(cinder.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveKey(database.ConnectionSecretKey))
	g.Expect(derived.Data[database.ConnectionSecretKey]).NotTo(BeEmpty())

	// The digest is stable across passes (it drives the deployment pod-roll).
	_, digest2, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, cinder)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}

func TestReconcileDBConnectionSecret_MissingUpstreamWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := validCinder()
	// No upstream DB credentials Secret: no derived Secret is materialised.
	r := newCinderTestReconciler(cinder)

	res, digest, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, cinder)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := cinderCondition(cinder, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDBCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: database.ConnectionSecretName(cinder.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).NotTo(Succeed(),
		"no derived Secret is written on the waiting path")
}
