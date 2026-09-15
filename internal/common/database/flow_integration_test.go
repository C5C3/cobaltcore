// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package database

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	envtestutil "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// TestIntegration_ReconcileProvision drives the managed provisioning flow end to
// end against a real API server: the cluster gate, the Database ensure, and the
// User/Grant ensure each gate on the simulated readiness of the underlying CR.
func TestIntegration_ReconcileProvision(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-flow-provision"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "provision-owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, mdb)).To(Succeed())

	spec := &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	var conds []metav1.Condition
	params := ProvisionFlowParams{
		Client: c, Scheme: scheme, Owner: owner,
		InstanceName: "keystone", Namespace: ns.Name,
		Database: spec, Conditions: &conds, Generation: 1,
		ConditionType: "DatabaseReady", RequeueAfter: time.Second,
	}

	// Cluster not ready yet.
	res, err := ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonClusterNotReady))

	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Name: "mariadb", Namespace: ns.Name}, 1)).To(Succeed())

	// Database applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonWaitingForDatabase))

	dbKey := client.ObjectKey{Name: "keystone", Namespace: ns.Name}
	g.Expect(simulators.SimulateDatabaseReady(ctx, c, dbKey)).To(Succeed())

	// Database ready; User applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.SimulateUserReady(ctx, c, dbKey)).To(Succeed())

	// User ready; Grant applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.SimulateGrantReady(ctx, c, dbKey)).To(Succeed())

	// All three Ready: provisioning is complete.
	res, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
}

// TestIntegration_ReconcileProvision_additionalSchemas drives a two-schema block
// through the same ladder: the additional Database gates the User, and the
// additional Grant gates completion. The finalizer then sweeps all five CRs.
func TestIntegration_ReconcileProvision_additionalSchemas(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-flow-provision-cells"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cells-owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())
	mdb := &mariadbv1alpha1.MariaDB{ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, mdb)).To(Succeed())

	spec := &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "nova",
		SecretRef:  commonv1.SecretRefSpec{Name: "nova-db"},
	}
	var conds []metav1.Condition
	params := ProvisionFlowParams{
		Client: c, Scheme: scheme, Owner: owner,
		InstanceName: "nova", Namespace: ns.Name,
		Database: spec, AdditionalDatabaseNames: []string{"nova_cell0"},
		Conditions: &conds, Generation: 1,
		ConditionType: "DatabaseReady", RequeueAfter: time.Second,
	}

	primaryKey := client.ObjectKey{Name: "nova", Namespace: ns.Name}
	cellKey := client.ObjectKey{Name: "nova-nova-cell0", Namespace: ns.Name}

	// Cluster not ready yet.
	res, err := ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonClusterNotReady))

	g.Expect(simulators.SimulateMariaDBReady(ctx, c, client.ObjectKey{Name: "mariadb", Namespace: ns.Name}, 1)).To(Succeed())

	// Primary Database applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonWaitingForDatabase))
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Message).To(Equal("MariaDB Database CR is not ready"))

	g.Expect(simulators.SimulateDatabaseReady(ctx, c, primaryKey)).To(Succeed())

	// Additional Database applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Message).
		To(Equal(`MariaDB Database CR for schema "nova_cell0" is not ready`))

	g.Expect(simulators.SimulateDatabaseReady(ctx, c, cellKey)).To(Succeed())

	// Both schemas ready; User applied; not ready. The additional Grant waits
	// for the User.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Message).To(Equal("MariaDB User or Grant CR is not ready"))
	g.Expect(apierrors.IsNotFound(c.Get(ctx, cellKey, &mariadbv1alpha1.Grant{}))).To(BeTrue())
	g.Expect(simulators.SimulateUserReady(ctx, c, primaryKey)).To(Succeed())

	// User ready; primary Grant applied; not ready. The additional Grant waits
	// for the primary Grant.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Message).To(Equal("MariaDB User or Grant CR is not ready"))
	g.Expect(apierrors.IsNotFound(c.Get(ctx, cellKey, &mariadbv1alpha1.Grant{}))).To(BeTrue())
	g.Expect(simulators.SimulateGrantReady(ctx, c, primaryKey)).To(Succeed())

	// Primary Grant ready; additional Grant applied; not ready.
	_, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Message).
		To(Equal(`MariaDB Grant CR for schema "nova_cell0" is not ready`))

	g.Expect(simulators.SimulateGrantReady(ctx, c, cellKey)).To(Succeed())

	// All five Ready: provisioning is complete.
	res, err = ReconcileProvision(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())

	// Two schemas on one SQL user: two Databases, two Grants, one User.
	dbs := &mariadbv1alpha1.DatabaseList{}
	g.Expect(c.List(ctx, dbs, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(dbs.Items).To(HaveLen(2))
	users := &mariadbv1alpha1.UserList{}
	g.Expect(c.List(ctx, users, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(users.Items).To(HaveLen(1))
	grants := &mariadbv1alpha1.GrantList{}
	g.Expect(c.List(ctx, grants, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(grants.Items).To(HaveLen(2))
	for _, grant := range grants.Items {
		g.Expect(grant.Spec.Username).To(Equal("nova"))
	}

	// The finalizer sweeps the additional schema's CRs along with the primary
	// ones.
	g.Expect(FinalizeResources(ctx, c, primaryKey, "nova_cell0")).To(Succeed())
	swept := []struct {
		obj client.Object
		key client.ObjectKey
	}{
		{&mariadbv1alpha1.Database{}, primaryKey},
		{&mariadbv1alpha1.Database{}, cellKey},
		{&mariadbv1alpha1.User{}, primaryKey},
		{&mariadbv1alpha1.Grant{}, primaryKey},
		{&mariadbv1alpha1.Grant{}, cellKey},
	}
	for _, s := range swept {
		g.Expect(apierrors.IsNotFound(c.Get(ctx, s.key, s.obj))).
			To(BeTrue(), "%T %s should be gone", s.obj, s.key)
	}
}

// TestIntegration_ReconcileSyncJobs drives the db-sync then schema-check
// sequencing against a real API server, using SimulateJobComplete to advance
// each Job to its terminal state.
func TestIntegration_ReconcileSyncJobs(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-flow-sync"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sync-owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	var conds []metav1.Condition
	installed := ""
	params := SyncFlowParams{
		Client: c, Scheme: scheme, Owner: owner,
		Jobs: JobSetParams{
			InstanceName:       "keystone",
			Namespace:          ns.Name,
			Image:              "registry.example.com/keystone:2026.1",
			ConfigMapName:      "keystone-config",
			ConfigMountPath:    "/etc/keystone/keystone.conf.d/",
			SyncCommand:        []string{"keystone-manage", "db_sync"},
			SchemaCheckCommand: []string{"keystone-manage", "db_sync", "--check"},
		},
		Conditions: &conds, Generation: 1, ConditionType: "DatabaseReady",
		RequeueAfter: time.Second, InstalledRelease: &installed, ImageTag: "2026.1",
	}

	// db-sync created and in progress.
	res, err := ReconcileSyncJobs(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonDBSyncInProgress))

	g.Expect(simulators.SimulateJobComplete(ctx, c, client.ObjectKey{Name: "keystone-db-sync", Namespace: ns.Name})).To(Succeed())

	// db-sync complete; schema-check created.
	_, err = ReconcileSyncJobs(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Reason).To(Equal(ReasonSchemaCheckInProgress))

	g.Expect(simulators.SimulateJobComplete(ctx, c, client.ObjectKey{Name: "keystone-schema-check", Namespace: ns.Name})).To(Succeed())

	// Both complete: DatabaseSynced and the installed release promoted.
	res, err = ReconcileSyncJobs(ctx, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(meta.FindStatusCondition(conds, "DatabaseReady").Status).To(Equal(metav1.ConditionTrue))
	g.Expect(installed).To(Equal("2026.1"))
}

// TestIntegration_ReconcileConnectionSecret materialises the derived
// db-connection Secret from a real upstream credentials Secret.
func TestIntegration_ReconcileConnectionSecret(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-flow-conn"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "conn-owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())
	upstream := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keystone-db", Namespace: ns.Name},
		Data:       map[string][]byte{"password": []byte("s3cr3t")},
	}
	g.Expect(c.Create(ctx, upstream)).To(Succeed())

	spec := &commonv1.DatabaseSpec{
		ClusterRef: &corev1.LocalObjectReference{Name: "mariadb"},
		Database:   "keystone",
		SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
	}
	var conds []metav1.Condition
	res, digest, err := ReconcileConnectionSecret(ctx, ConnectionSecretFlowParams{
		Client: c, Scheme: scheme, Owner: owner,
		InstanceName: "keystone", Namespace: ns.Name,
		Database: spec, TLSMountPath: "/etc/keystone/db-tls/",
		Conditions: &conds, Generation: 1, ConditionType: "SecretsReady",
		RequeueAfter: time.Second,
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())

	derived := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "keystone-db-connection", Namespace: ns.Name}, derived)).To(Succeed())
	g.Expect(derived.Data).To(HaveKey(ConnectionSecretKey))
	g.Expect(derived.OwnerReferences).To(HaveLen(1))
}
