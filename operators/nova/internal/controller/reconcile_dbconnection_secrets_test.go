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

	"github.com/c5c3/cobaltcore/internal/common/database"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// derivedDSN reads the connection URL out of a derived <instance>-db-connection
// Secret, failing the test when the Secret is not there.
func derivedDSN(t *testing.T, r *NovaReconciler, instanceName string) string {
	t.Helper()
	g := NewGomegaWithT(t)
	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: database.ConnectionSecretName(instanceName)}
	g.Expect(r.Get(context.Background(), key, &derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveKey(database.ConnectionSecretKey))
	return string(derived.Data[database.ConnectionSecretKey])
}

// TestReconcileDBConnectionSecrets_DerivesBothSecretsAndDistinctDigests covers
// the ready path: Nova splits its state across two schemas, so one pass has to
// leave two derived Secrets behind, each naming its own schema, and two digests
// the deployment steps can stamp independently.
func TestReconcileDBConnectionSecrets_DerivesBothSecretsAndDistinctDigests(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, novaAPIDBSecret(), novaCellDBSecret())

	res, digests, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	apiDSN := derivedDSN(t, r, apiInstanceName(nova))
	cellDSN := derivedDSN(t, r, nova.Name)
	g.Expect(apiDSN).To(ContainSubstring("/nova_api?"))
	g.Expect(cellDSN).To(ContainSubstring("/nova?"))

	g.Expect(digests.api).NotTo(BeEmpty())
	g.Expect(digests.cell).NotTo(BeEmpty())
	g.Expect(digests.api).NotTo(Equal(digests.cell),
		"each schema rolls its own readers, so the two digests must not collapse")

	// Both digests are stable across passes: they drive the pod roll, so a value
	// that moved without the credentials moving would roll every pass.
	_, second, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(second).To(Equal(digests))
}

// TestReconcileDBConnectionSecrets_APIBlockNotReadyStopsBeforeCell covers the
// GitOps ordering in which only the cell credentials have synced: the nova_api
// block returns its own requeue, and the cell Secret is not derived behind it.
// Every process that reads the cell schema reads nova_api too, so deriving one
// half would publish a DSN nothing can use yet.
func TestReconcileDBConnectionSecrets_APIBlockNotReadyStopsBeforeCell(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, novaCellDBSecret())

	res, digests, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digests).To(Equal(dsnDigests{}))

	cond := novaCondition(nova, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDBCredentials))

	var derived corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: database.ConnectionSecretName(nova.Name)}
	g.Expect(r.Get(context.Background(), key, &derived)).NotTo(Succeed(),
		"the cell Secret is not derived while the nova_api credentials are missing")
}

// TestReconcileDBConnectionSecrets_CellBlockNotReadyWaits covers the GitOps
// ordering in which only the nova_api credentials have synced. The API block
// derives its Secret, but the step must still report the wait and requeue: a
// zero result here would let the pipeline go on with an empty cell digest and
// start processes whose cell connection URL does not exist yet, while
// SecretsReady says False.
func TestReconcileDBConnectionSecrets_CellBlockNotReadyWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	r := newNovaTestReconciler(nova, novaAPIDBSecret())

	res, digests, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digests).To(Equal(dsnDigests{}),
		"no digest is handed on while one of the two connection URLs is missing")

	cond := novaCondition(nova, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(database.ReasonWaitingForDBCredentials))

	apiKey := client.ObjectKey{Namespace: testNamespace, Name: "nova-api-db-connection"}
	g.Expect(r.Get(context.Background(), apiKey, &corev1.Secret{})).To(Succeed(),
		"the API block ran to completion ahead of the cell block")
	cellKey := client.ObjectKey{Namespace: testNamespace, Name: "nova-db-connection"}
	g.Expect(r.Get(context.Background(), cellKey, &corev1.Secret{})).NotTo(Succeed())
}

// TestReconcileDBConnectionSecrets_CredentialsReadFailureNamesTheSchema covers a
// read of the upstream credentials that fails outright rather than finding
// nothing. Both schemas go through the same shared flow, so the prefix is the
// only thing telling an operator which of the two Secrets the API server
// refused.
func TestReconcileDBConnectionSecrets_CredentialsReadFailureNamesTheSchema(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		wrap   string
	}{
		{name: "nova_api", secret: testAPIDBSecret, wrap: "api database connection secret:"},
		{name: "cell", secret: testCellDBSecret, wrap: "cell database connection secret:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			boom := errors.New("etcd unavailable")
			failingKey := client.ObjectKey{Namespace: testNamespace, Name: tc.secret}
			c := novaFakeClientBuilder(nova, novaAPIDBSecret(), novaCellDBSecret()).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
						opts ...client.GetOption,
					) error {
						if _, ok := obj.(*corev1.Secret); ok && key == failingKey {
							return boom
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

			res, digests, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)

			g.Expect(err).To(MatchError(boom))
			g.Expect(err).To(MatchError(HavePrefix(tc.wrap)))
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(digests).To(Equal(dsnDigests{}))
		})
	}
}

// TestReconcileDBConnectionSecrets_TLSMountPathsDiffer covers the two schemas
// carrying their own client keypair: each DSN has to point at its own mount
// directory, since a shared one could only project one of the two keypairs.
func TestReconcileDBConnectionSecrets_TLSMountPathsDiffer(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.APIDatabase.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "nova-api-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "nova-api-db-cert"},
	}
	nova.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "verify-full",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "nova-db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "nova-db-cert"},
	}
	r := newNovaTestReconciler(nova, novaAPIDBSecret(), novaCellDBSecret())

	res, _, err := r.reconcileDBConnectionSecrets(context.Background(), r.Client, nova)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	apiPaths := database.TLSFilePaths(apiDBTLSMountPath)
	cellPaths := database.TLSFilePaths(cellDBTLSMountPath)

	apiDSN := derivedDSN(t, r, apiInstanceName(nova))
	g.Expect(apiDSN).To(ContainSubstring("ssl_ca=" + apiPaths.CA))
	g.Expect(apiDSN).To(ContainSubstring("ssl_key=" + apiPaths.Key))
	g.Expect(apiDSN).NotTo(ContainSubstring(cellDBTLSMountPath))

	cellDSN := derivedDSN(t, r, nova.Name)
	g.Expect(cellDSN).To(ContainSubstring("ssl_ca=" + cellPaths.CA))
	g.Expect(cellDSN).To(ContainSubstring("ssl_key=" + cellPaths.Key))
	g.Expect(cellDSN).NotTo(ContainSubstring(apiDBTLSMountPath))
}
