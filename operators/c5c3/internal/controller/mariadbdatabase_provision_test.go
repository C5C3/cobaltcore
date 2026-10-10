// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the MariaDBDatabase provision leg: the MariaDB and schema gates,
// the shared OpenBao identity, the OpenBao role, the Database CR and the
// generator that issues the credential.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/openbao"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// mdbGetDatabase reads the order's Database CR from "default" on c.
func mdbGetDatabase(t *testing.T, c client.Client, order *c5c3v1alpha1.MariaDBDatabase) *mariadbv1alpha1.Database {
	t.Helper()
	db := &mariadbv1alpha1.Database{}
	key := types.NamespacedName{Namespace: "default", Name: mariaDBDatabaseChildName(order, "")}
	if err := c.Get(context.Background(), key, db); err != nil {
		t.Fatalf("reading the Database CR %s: %v", key, err)
	}
	return db
}

// mdbMarkDatabaseReady reports the order's Database CR Ready, as the
// mariadb-operator does once the schema exists.
func mdbMarkDatabaseReady(t *testing.T, c client.Client, order *c5c3v1alpha1.MariaDBDatabase) {
	t.Helper()
	db := mdbGetDatabase(t, c, order)
	db.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created", LastTransitionTime: metav1.Now(),
	}}
	if err := c.Status().Update(context.Background(), db); err != nil {
		t.Fatalf("marking the Database CR Ready: %v", err)
	}
}

// TestMariaDBDatabase_Provision_ProjectsTheIdentityTheRoleAndTheDatabase walks
// a fresh order through the provision leg: the shared identity, one role
// write, the Database CR, then the generator and its ExternalSecret.
func TestMariaDBDatabase_Provision_ProjectsTheIdentityTheRoleAndTheDatabase(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbControlPlane("")
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, cp, mdbReadyMariaDB(), mdbClientCertSecret())

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	got := mdbGet(t, h)
	expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse, "WaitingForDatabase")
	expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseWaitingForCredentials)

	// The shared identity carries no order labels.
	sa := &corev1.ServiceAccount{}
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "order-db-creds"}, sa)).To(Succeed())
	g.Expect(sa.Labels).NotTo(HaveKey(mariaDBDatabaseNameLabel))
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	g.Expect(h.mgmt.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "order-db-openbao-client"}, cert)).To(Succeed())
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	g.Expect(issuer).To(Equal(openBaoCAIssuerName))
	g.Expect(cert.GetLabels()).NotTo(HaveKey(mariaDBDatabaseNameLabel))
	g.Expect(cert.GetOwnerReferences()).To(BeEmpty())

	// One role write, with the schema's underscore escaped in the GRANT.
	roleName := mariaDBDatabaseRoleName("default", order, "")
	g.Expect(roleName).To(MatchRegexp(`^order\.default\.app-db-[0-9a-f]{8}-database$`))
	g.Expect(h.bao.count("write")).To(Equal(1))
	g.Expect(h.bao.recorded()).To(ContainElement("write database/mariadb " + roleName))
	g.Expect(h.bao.writes[0]).To(Equal(openbao.DatabaseRole{
		DBName: "keystone-default",
		CreationStatements: []string{
			"CREATE USER '{{name}}'@'%' IDENTIFIED BY '{{password}}';",
			"GRANT ALL PRIVILEGES ON `app\\_db`.* TO '{{name}}'@'%';",
		},
		RevocationStatements: []string{"DROP USER IF EXISTS '{{name}}'@'%';"},
		DefaultTTL:           48 * time.Hour,
		MaxTTL:               72 * time.Hour,
	}))
	g.Expect(h.bao.configs[0].Role).To(Equal("c5c3-operator"))
	g.Expect(h.bao.configs[0].KubernetesMount).To(Equal(openBaoDefaultKubernetesMount))
	g.Expect(h.bao.configs[0].CACert).To(Equal([]byte("openbao-ca")))
	g.Expect(h.bao.count("close")).To(Equal(1))
	g.Expect(got.Status.RoleName).To(Equal(roleName))

	// The Database CR beside the MariaDB.
	db := mdbGetDatabase(t, h.mgmt, order)
	g.Expect(db.Spec.Name).To(Equal("app_db"))
	g.Expect(db.Spec.MariaDBRef.Name).To(Equal("openstack-db"))
	g.Expect(db.Spec.MariaDBRef.WaitForIt).To(BeTrue())
	g.Expect(db.Spec.CleanupPolicy).To(HaveValue(Equal(mariadbv1alpha1.CleanupPolicySkip)))
	g.Expect(db.Labels).To(Equal(mariaDBDatabaseRef(order, "").childLabels()))
	g.Expect(labelledIn(t, h.mgmt, &esov1.ExternalSecretList{}, mariaDBDatabaseNameLabel, mdbTestName)).To(BeZero(),
		"nothing reads the role before the schema exists")

	mdbMarkDatabaseReady(t, h.mgmt, order)
	result, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseWaitingForCredentials)
	g.Expect(h.bao.count("write")).To(Equal(1), "an equal role is not rewritten")
	g.Expect(h.bao.count("close")).To(Equal(2), "every pass that dialled closes")

	key := types.NamespacedName{Namespace: "default", Name: mariaDBDatabaseGeneratorName(order, "")}
	vds := &esgenv1alpha1.VaultDynamicSecret{}
	g.Expect(h.mgmt.Get(context.Background(), key, vds)).To(Succeed())
	g.Expect(vds.Labels).To(Equal(mariaDBDatabaseRef(order, "").childLabels()))
	g.Expect(vds.Spec.Path).To(Equal("database/mariadb/creds/" + roleName))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.Role).To(Equal("order-db"))
	g.Expect(vds.Spec.Provider.Auth.Kubernetes.ServiceAccountRef.Name).To(Equal("order-db-creds"))
	g.Expect(vds.Spec.Provider.ClientTLS.CertSecretRef.Name).To(Equal("order-db-openbao-client"))
	es := &esov1.ExternalSecret{}
	g.Expect(h.mgmt.Get(context.Background(), key, es)).To(Succeed())
	g.Expect(es.Labels).To(Equal(mariaDBDatabaseRef(order, "").childLabels()))
	g.Expect(es.Spec.RefreshInterval.Duration).To(Equal(dbCredentialRefreshInterval))
	g.Expect(es.Spec.Target.Template).NotTo(BeNil())
	g.Expect(es.Spec.Target.Template.Metadata.Labels).To(Equal(mariaDBDatabaseRef(order, "").childLabels()))
}

// TestMariaDBDatabase_Provision_RewritesARoleThatDiffers rewrites a stored role
// whose GRANT does not escape the schema's underscore.
func TestMariaDBDatabase_Provision_RewritesARoleThatDiffers(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbControlPlane("")
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order, cp}, mdbConverged(order, "")...)...)
	roleName := mariaDBDatabaseRoleName("default", order, "")
	stale := mariaDBDatabaseOpenBaoRole(cp, "app_db")
	stale.CreationStatements[1] = "GRANT ALL PRIVILEGES ON `app_db`.* TO '{{name}}'@'%';"
	h.bao.roles[roleName] = stale

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.bao.count("write")).To(Equal(1))
	repaired, ok := h.bao.role(roleName)
	g.Expect(ok).To(BeTrue())
	g.Expect(repaired).To(Equal(mariaDBDatabaseOpenBaoRole(cp, "app_db")))

	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.bao.count("write")).To(Equal(1), "the repaired role is not rewritten")
}

func TestMariaDBDatabase_Provision_ClusterNotReady(t *testing.T) {
	cases := []struct {
		name    string
		mariadb func() []client.Object
	}{
		{name: "the MariaDB is not Ready", mariadb: func() []client.Object {
			m := mdbReadyMariaDB()
			m.Status.Conditions[0].Status = metav1.ConditionFalse
			return []client.Object{m}
		}},
		{name: "the MariaDB does not exist", mariadb: func() []client.Object { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := append([]client.Object{mariaDBDatabaseCR(), mdbControlPlane(""), mdbClientCertSecret()}, tc.mariadb()...)
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
			cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				"ClusterNotReady")
			g.Expect(cond.Message).To(Equal(`MariaDB cluster "openstack-db" in namespace "default" is not ready`))
			g.Expect(h.bao.recorded()).To(BeEmpty())
		})
	}
}

func TestMariaDBDatabase_Provision_ReservedName(t *testing.T) {
	for _, name := range []string{"mysql", "MySQL", "information_schema", "sys"} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mariaDBDatabaseCR()
			order.Spec.DatabaseName = name
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
				mdbReadyMariaDB(), mdbClientCertSecret())

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "only an edit lifts the refusal")
			got := mdbGet(t, h)
			cond := expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseNameReserved)
			g.Expect(cond.Message).To(ContainSubstring("is a MariaDB system schema"))
			expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseWaitingForCredentials)
			g.Expect(h.bao.recorded()).To(BeEmpty())
			g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero())
		})
	}
}

// TestMariaDBDatabase_Provision_InvalidName refuses a name that is not a MySQL
// identifier before anything is written. The CRD's pattern rejects each of
// them; the fake client applies no schema, as a target cluster with an altered
// CRD would not.
func TestMariaDBDatabase_Provision_InvalidName(t *testing.T) {
	for _, name := range []string{"%", "app`db", "app.db", "app-db", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mariaDBDatabaseCR()
			order.Spec.DatabaseName = name
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
				mdbReadyMariaDB(), mdbClientCertSecret())

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "only an edit lifts the refusal")
			cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseNameInvalid)
			g.Expect(cond.Message).To(ContainSubstring("is not a MySQL identifier"))
			g.Expect(h.bao.recorded()).To(BeEmpty(), "no role grants on the name")
			g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "no Database CR creates the schema")
		})
	}
}

// TestMariaDBDatabase_Provision_CollisionOnAForeignDatabase refuses a schema
// another Database CR on the same MariaDB carries, before anything is written,
// and admits the same name on another MariaDB.
func TestMariaDBDatabase_Provision_CollisionOnAForeignDatabase(t *testing.T) {
	foreign := func(mariadb, name string) *mariadbv1alpha1.Database {
		return &mariadbv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "default"},
			Spec: mariadbv1alpha1.DatabaseSpec{
				MariaDBRef: mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: mariadb}},
				Name:       name,
			},
		}
	}

	t.Run("the same MariaDB", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := mariaDBDatabaseCR()
		h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
			mdbReadyMariaDB(), mdbClientCertSecret(), foreign("openstack-db", "app_db"))

		result, logs, err := h.reconcileLogged()

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
		cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
			reasonMariaDBDatabaseCollision)
		// The status names neither the other Database CR nor where the MariaDB runs.
		g.Expect(cond.Message).To(Equal(`database "app_db" is already in use on the ControlPlane's shared MariaDB; ` +
			`the order never takes over a database it did not create; delete and re-create the order with ` +
			`another databaseName`))
		g.Expect(logs).To(ContainSubstring(`"takenBy"="billing"`), "the log names the other Database CR")
		g.Expect(h.bao.recorded()).To(BeEmpty())
		g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "no Database CR is applied")
	})

	// Two orders whose names default to the same schema: the other order's
	// Database CR carries its labels, which are not this order's.
	otherNamespace := mariaDBDatabaseCR()
	otherNamespace.Namespace, otherNamespace.UID = "tenant-b", "other-uid"
	for name, other := range map[string]*mariadbv1alpha1.Database{
		"another namespace's order of the same name": mdbReadyDatabase(otherNamespace, ""),
		"the same order on another cluster":          mdbReadyDatabase(mariaDBDatabaseCR(), kuTestCluster),
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mariaDBDatabaseCR(), mdbControlPlane(""),
				mdbReadyMariaDB(), mdbClientCertSecret(), other)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseCollision)
			g.Expect(h.bao.recorded()).To(BeEmpty(), "no role grants on the other order's schema")
		})
	}

	t.Run("the metadata.name of a Database without spec.name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := mariaDBDatabaseCR()
		order.Spec.DatabaseName = "billing"
		h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
			mdbReadyMariaDB(), mdbClientCertSecret(), foreign("openstack-db", ""))

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
			reasonMariaDBDatabaseCollision)
	})

	t.Run("another MariaDB", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := mariaDBDatabaseCR()
		h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
			mdbReadyMariaDB(), mdbClientCertSecret(), foreign("other-db", "app_db"))

		_, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
			"WaitingForDatabase")
	})
}

// TestMariaDBDatabase_Provision_CollisionOnARefusedAdopt refuses a Database CR
// of the child's name that the order did not create.
func TestMariaDBDatabase_Provision_CollisionOnARefusedAdopt(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	stranger := mdbReadyDatabase(order, "")
	stranger.Labels = nil
	stranger.Spec.Name = "unrelated"
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""),
		mdbReadyMariaDB(), mdbClientCertSecret(), stranger)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter))
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseCollision)
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt pre-existing *v1alpha1.Database"))
	g.Expect(mdbGetDatabase(t, h.mgmt, order).Spec.Name).To(Equal("unrelated"), "the stranger is not reshaped")
}

func TestMariaDBDatabase_Provision_WaitsForTheClientCertificate(t *testing.T) {
	incomplete := mdbClientCertSecret()
	delete(incomplete.Data, "ca.crt")
	cases := []struct {
		name string
		objs []client.Object
	}{
		{name: "the Secret does not exist"},
		{name: "the Secret carries no ca.crt", objs: []client.Object{incomplete}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := append([]client.Object{mariaDBDatabaseCR(), mdbControlPlane(""), mdbReadyMariaDB()}, tc.objs...)
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
			cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseWaitingForClientCertificate)
			g.Expect(cond.Message).To(Equal("the OpenBao client certificate default/order-db-openbao-client is not issued yet"))
			g.Expect(mdbProjectedIdentity(t, h.mgmt)).To(BeTrue(), "the identity it waits on is projected")
			g.Expect(h.bao.recorded()).To(BeEmpty())
		})
	}
}

// TestMariaDBDatabase_Provision_OpenBaoError reports a failing login or role
// write on DatabaseReady and returns an error, so the pass backs off and the
// error is counted. A 403 names the bootstrap scripts.
func TestMariaDBDatabase_Provision_OpenBaoError(t *testing.T) {
	denied := &openbao.APIError{
		StatusCode: 403, Method: "POST", Path: "/v1/auth/kubernetes/management/login",
		Errors: []string{"permission denied"},
	}
	cases := []struct {
		name       string
		setup      func(h *mdbHarness)
		wantPrefix bool
		wantCalls  []string
	}{
		{
			name:       "the login is denied",
			setup:      func(h *mdbHarness) { h.bao.dialErr = denied },
			wantPrefix: true,
			wantCalls:  []string{"dial"},
		},
		{
			name: "the role write fails",
			setup: func(h *mdbHarness) {
				h.bao.failOn = "write"
				h.bao.err = &openbao.APIError{StatusCode: 400, Method: "POST", Errors: []string{`"order.default.x" is not an allowed role`}}
			},
			wantCalls: []string{"dial", "read", "write", "close"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mariaDBDatabaseCR(), mdbControlPlane(""),
				mdbReadyMariaDB(), mdbClientCertSecret())
			tc.setup(h)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(ContainSubstring(`ensuring OpenBao database role "order.default.app-db-`)))
			cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseOpenBaoError)
			if tc.wantPrefix {
				g.Expect(cond.Message).To(HavePrefix("the operator's OpenBao login or policy is missing; re-run " +
					"deploy/openbao/bootstrap/setup-auth.sh, setup-policies.sh and setup-database-tenant.sh: openbao: 403"))
			} else {
				g.Expect(cond.Message).To(HavePrefix("openbao: 400"))
			}
			calls := h.bao.recorded()
			g.Expect(calls).To(HaveLen(len(tc.wantCalls)))
			for i, want := range tc.wantCalls {
				g.Expect(calls[i]).To(HavePrefix(want))
			}
			g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "no Database CR before the role exists")
		})
	}
}

func TestMariaDBDatabase_Provision_TokenReadErrorIsWrapped(t *testing.T) {
	g := NewGomegaWithT(t)

	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mariaDBDatabaseCR(), mdbControlPlane(""),
		mdbReadyMariaDB(), mdbClientCertSecret())
	h.tokenErr = errors.New("open /var/run/secrets/kubernetes.io/serviceaccount/token: no such file or directory")

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(ContainSubstring("reading the operator's ServiceAccount token: open ")))
	expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseOpenBaoError)
	g.Expect(h.bao.recorded()).To(BeEmpty())
}

// TestMariaDBDatabase_Provision_WaitsForCredentials holds the leg until ESO
// has materialised an engine-issued credential.
func TestMariaDBDatabase_Provision_WaitsForCredentials(t *testing.T) {
	order := mariaDBDatabaseCR()
	cases := []struct {
		name   string
		mutate func(objs []client.Object) []client.Object
		want   string
	}{
		{
			name: "the ExternalSecret is not Ready",
			mutate: func(objs []client.Object) []client.Object {
				objs[3].(*esov1.ExternalSecret).Status.Conditions = nil
				return objs
			},
			want: `ESO has not issued a credential from OpenBao path "database/mariadb/creds/order.default.app-db-`,
		},
		{
			name:   "the materialised Secret is missing",
			mutate: func(objs []client.Object) []client.Object { return objs[:4] },
			want:   "does not carry an engine-issued username yet",
		},
		{
			name: "the password is empty",
			mutate: func(objs []client.Object) []client.Object {
				objs[4].(*corev1.Secret).Data["password"] = nil
				return objs
			},
			want: "does not carry an engine-issued username yet",
		},
		{
			name: "the username is a static login",
			mutate: func(objs []client.Object) []client.Object {
				objs[4].(*corev1.Secret).Data["username"] = []byte("app_db_user")
				return objs
			},
			want: "does not carry an engine-issued username yet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := tc.mutate(mdbConverged(order, ""))
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
				append([]client.Object{mariaDBDatabaseCR(), mdbControlPlane("")}, objs...)...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
			got := mdbGet(t, h)
			cond := expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseWaitingForCredentials)
			g.Expect(cond.Message).To(ContainSubstring(tc.want))
			g.Expect(got.Status.SecretName).To(BeEmpty(), "nothing is delivered")
		})
	}
}

// TestMariaDBDatabase_Provision_TargetClusterUnavailable reports a Keystone
// placed on a cluster that does not resolve, where the MariaDB runs.
func TestMariaDBDatabase_Provision_TargetClusterUnavailable(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mdbControlPlane("")
	cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-db"}
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mariaDBDatabaseCR(), cp)
	h.reconciler().Resolver = &childrenResolver{errNames: map[string]error{"edge-db": errors.New("cluster edge-db is not engaged")}}

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
		"TargetClusterUnavailable")
	g.Expect(cond.Message).To(ContainSubstring("edge-db"))
}

// TestMariaDBDatabase_DeletionPolicyFlipReappliesCleanupPolicy pins that a
// changed deletionPolicy reaches the live Database CR on the next pass.
func TestMariaDBDatabase_DeletionPolicyFlipReappliesCleanupPolicy(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	order.Spec.DeletionPolicy = ""
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, mdbControlPlane("")}, mdbConverged(order, "")...)...)
	_, err := h.reconcile(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mdbGetDatabase(t, h.mgmt, order).Spec.CleanupPolicy).To(HaveValue(Equal(mariadbv1alpha1.CleanupPolicySkip)),
		"a stored order without the field reads as Retain")

	live := mdbGet(t, h)
	live.Spec.DeletionPolicy = c5c3v1alpha1.MariaDBDatabaseDeletionPolicyDelete
	g.Expect(h.order.Update(context.Background(), live)).To(Succeed())
	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mdbGetDatabase(t, h.mgmt, order).Spec.CleanupPolicy).To(HaveValue(Equal(mariadbv1alpha1.CleanupPolicyDelete)))
	g.Expect(conditions.AllTrue(mdbGet(t, h).Status.Conditions, "Ready")).To(BeTrue())
}
