// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package simulators

import (
	"testing"

	. "github.com/onsi/gomega"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
)

// Feature: CC-0005

// --- SimulateDatabaseReady integration ---

func TestIntegration_SimulateDatabaseReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the Database resource.
	db := &mariadbv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-database",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.DatabaseSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{},
		},
	}
	g.Expect(c.Create(ctx, db)).To(Succeed())

	key := client.ObjectKey{Name: "test-database", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateDatabaseReady(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &mariadbv1alpha1.Database{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal("Ready"))
	g.Expect(string(cond.Status)).To(Equal("True"))
	g.Expect(cond.Reason).To(Equal("DatabaseReady"))
}

// --- SimulateUserReady integration ---

func TestIntegration_SimulateUserReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-user-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the User resource.
	user := &mariadbv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-user",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.UserSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{},
		},
	}
	g.Expect(c.Create(ctx, user)).To(Succeed())

	key := client.ObjectKey{Name: "test-user", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateUserReady(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &mariadbv1alpha1.User{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal("Ready"))
	g.Expect(string(cond.Status)).To(Equal("True"))
	g.Expect(cond.Reason).To(Equal("UserReady"))
}

// --- SimulateGrantReady integration ---

func TestIntegration_SimulateGrantReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-grant-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the Grant resource.
	grant := &mariadbv1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grant",
			Namespace: ns.Name,
		},
		Spec: mariadbv1alpha1.GrantSpec{
			MariaDBRef: mariadbv1alpha1.MariaDBRef{},
			Privileges: []string{"ALL PRIVILEGES"},
			Username:   "test-user",
		},
	}
	g.Expect(c.Create(ctx, grant)).To(Succeed())

	key := client.ObjectKey{Name: "test-grant", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateGrantReady(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &mariadbv1alpha1.Grant{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal("Ready"))
	g.Expect(string(cond.Status)).To(Equal("True"))
	g.Expect(cond.Reason).To(Equal("GrantReady"))
}

// --- SimulatePushSecretSynced integration ---

func TestIntegration_SimulatePushSecretSynced(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ps-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the PushSecret resource.
	ps := &esov1alpha1.PushSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pushsecret",
			Namespace: ns.Name,
		},
	}
	g.Expect(c.Create(ctx, ps)).To(Succeed())

	key := client.ObjectKey{Name: "test-pushsecret", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulatePushSecretSynced(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.RefreshTime.IsZero()).To(BeFalse())
	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())

	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal(esov1alpha1.PushSecretReady))
	g.Expect(cond.Status).To(Equal(corev1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(esov1alpha1.ReasonSynced))
}

// --- SimulateCertificateReady integration ---

func TestIntegration_SimulateCertificateReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, cancel := envtestutil.SetupEnvTest(t)
	defer cancel()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cert-ns"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	// Create the Certificate resource.
	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certificate",
			Namespace: ns.Name,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: "test-cert-secret",
			IssuerRef: cmmeta.ObjectReference{
				Name: "test-issuer",
			},
		},
	}
	g.Expect(c.Create(ctx, cert)).To(Succeed())

	key := client.ObjectKey{Name: "test-certificate", Namespace: ns.Name}

	// Call the simulator.
	g.Expect(SimulateCertificateReady(ctx, c, key)).To(Succeed())

	// Verify status via a fresh Get.
	updated := &certmanagerv1.Certificate{}
	g.Expect(c.Get(ctx, key, updated)).To(Succeed())

	g.Expect(updated.Status.Conditions).NotTo(BeEmpty())
	cond := updated.Status.Conditions[0]
	g.Expect(cond.Type).To(Equal(certmanagerv1.CertificateConditionReady))
	g.Expect(cond.Status).To(Equal(cmmeta.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("Ready"))
}
