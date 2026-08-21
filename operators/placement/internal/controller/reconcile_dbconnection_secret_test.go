// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
)

func TestReconcileDBConnectionSecret_DerivesSecretAndDigest(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	r := newPlacementTestReconciler(placement, placementDBSecret())

	res, digest, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())

	// The derived <placement>-db-connection Secret is materialised with the DSN.
	var derived corev1.Secret
	key := client.ObjectKey{Namespace: "default", Name: "test-placement-db-connection"}
	g.Expect(key.Name).To(Equal(database.ConnectionSecretName(placement.Name)))
	g.Expect(r.Get(context.Background(), key, &derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveKey("connection"))
	g.Expect(database.ConnectionSecretKey).To(Equal("connection"))
	g.Expect(string(derived.Data["connection"])).To(HavePrefix("mysql+pymysql://"))

	// The digest is stable across passes (it drives the deployment pod-roll).
	_, digest2, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, placement)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}

func TestReconcileDBConnectionSecret_MissingUpstreamWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()
	// No upstream DB credentials Secret: no derived Secret is materialised.
	r := newPlacementTestReconciler(placement)

	res, digest, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, placement)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := conditions.GetCondition(placement.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDBCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: "default", Name: database.ConnectionSecretName(placement.Name)}
	err = r.Get(context.Background(), key, &derived)
	g.Expect(err).To(HaveOccurred(), "no derived Secret is written on the waiting path")
}

// TestReconcileDBConnectionSecret_UpstreamReadErrorPropagates covers the
// difference between "not there yet" and "the API server said no": a NotFound
// waits, any other read failure is an error the caller retries with backoff
// rather than a waiting condition that looks like a healthy first install.
func TestReconcileDBConnectionSecret_UpstreamReadErrorPropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := testPlacement()

	boom := errors.New("etcd unavailable")
	upstreamKey := client.ObjectKey{Namespace: "default", Name: "placement-db"}
	c := placementFakeClientBuilder(placement, placementDBSecret()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret && key == upstreamKey {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &PlacementReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, digest, err := r.reconcileDBConnectionSecret(context.Background(), r.Client, placement)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading database credentials Secret default/placement-db:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())
}
