// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the MariaDBDatabase delivery leg: the address per cluster, the CA
// bundle, the delivered Secret, its refresh and its repair.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// mdbDelivered reads the delivered Secret from the order's cluster, or nil.
func mdbDelivered(t *testing.T, h *mdbHarness) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: kuTestNamespace, Name: mariaDBDatabaseCredentialsSecretName(mariaDBDatabaseCR())}
	if err := h.order.Get(context.Background(), key, secret); err != nil {
		return nil
	}
	return secret
}

// mdbTLSControlPlane runs the shared database with tls.mode require and the CA
// bundle in the Secret db-ca.
func mdbTLSControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := mdbControlPlane("")
	cp.Spec.Infrastructure.Database.TLS = &commonv1.DatabaseTLSSpec{
		Mode:                "require",
		CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
		ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
	}
	return cp
}

func mdbCABundle() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-ca", Namespace: "default"},
		Data:       map[string][]byte{"ca.crt": []byte("database-ca")},
	}
}

func TestMariaDBDatabase_Delivery_WritesTheKeys(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, mdbControlPlane("")}, mdbConverged(order, "")...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	got := mdbGet(t, h)
	expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionTrue, reasonMariaDBDatabaseProvisioned)
	cond := expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionTrue, reasonMariaDBDatabaseDelivered)
	g.Expect(cond.Message).To(Equal(`credentials are delivered in Secret "app-db-credentials" (keys host, port, ` +
		`database, username, password) in namespace "tenant-a" on the management cluster`))
	g.Expect(conditions.AllTrue(got.Status.Conditions, "Ready")).To(BeTrue())

	secret := mdbDelivered(t, h)
	g.Expect(secret).NotTo(BeNil())
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(metav1.IsControlledBy(secret, got)).To(BeTrue())
	g.Expect(secret.Labels).To(Equal(map[string]string{
		mariaDBDatabaseNameLabel: mdbTestName, mariaDBDatabaseNamespaceLabel: kuTestNamespace,
	}), "no cluster label: the Secret is not a labelled child")
	g.Expect(secret.Data).To(Equal(map[string][]byte{
		"host":     []byte("openstack-db.default.svc"),
		"port":     []byte("3306"),
		"database": []byte("app_db"),
		"username": []byte("v-order-app-db-1"),
		"password": []byte("pw-1"),
	}), "no ca.crt without a tls block")

	g.Expect(got.Status.SecretName).To(Equal("app-db-credentials"))
	g.Expect(got.Status.SecretKeys).To(Equal([]string{"host", "port", "database", "username", "password"}))
	g.Expect(got.Status.Host).To(Equal("openstack-db.default.svc"))
	g.Expect(got.Status.Port).To(Equal(int32(3306)))
	g.Expect(got.Status.DatabaseName).To(Equal("app_db"))
	g.Expect(got.Status.CredentialsRefreshedAt).NotTo(BeNil())
	g.Expect(got.Status.CredentialsRefreshedAt.Equal(&mdbTestRefresh)).To(BeTrue())
}

// TestMariaDBDatabase_Delivery_CarriesTheCABundleUnderTLS waits for the CA
// bundle, delivers it under ca.crt, and drops it once TLS is turned off.
func TestMariaDBDatabase_Delivery_CarriesTheCABundleUnderTLS(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbTLSControlPlane()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append([]client.Object{order, cp}, mdbConverged(order, "")...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseWaitingForCABundle)
	g.Expect(cond.Message).To(Equal("the database CA bundle Secret default/db-ca does not carry ca.crt yet"))
	g.Expect(mdbDelivered(t, h)).To(BeNil(), "nothing is delivered without the CA")

	g.Expect(h.mgmt.Create(context.Background(), mdbCABundle())).To(Succeed())
	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	got := mdbGet(t, h)
	expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionTrue, reasonMariaDBDatabaseDelivered)
	g.Expect(mdbDelivered(t, h).Data).To(HaveKeyWithValue("ca.crt", []byte("database-ca")))
	g.Expect(got.Status.SecretKeys).To(Equal([]string{"host", "port", "database", "username", "password", "ca.crt"}))

	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(context.Background(), client.ObjectKeyFromObject(cp), live)).To(Succeed())
	live.Spec.Infrastructure.Database.TLS = nil
	g.Expect(h.mgmt.Update(context.Background(), live)).To(Succeed())
	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mdbDelivered(t, h).Data).NotTo(HaveKey("ca.crt"), "the field manager drops the key it owned")
	g.Expect(mdbGet(t, h).Status.SecretKeys).NotTo(ContainElement("ca.crt"))
}

// TestMariaDBDatabase_Delivery_PublishedEndpointOnAnotherCluster delivers the
// published endpoint to an order off the database's cluster, and refuses it
// while none is published.
func TestMariaDBDatabase_Delivery_PublishedEndpointOnAnotherCluster(t *testing.T) {
	cases := []struct {
		name      string
		published string
		wantHost  string
		wantPort  string
	}{
		{name: "a host name", published: "db.example.test:13306", wantHost: "db.example.test", wantPort: "13306"},
		{name: "a bracketed IPv6 literal", published: "[fd00::1]:3306", wantHost: "fd00::1", wantPort: "3306"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mariaDBDatabaseCR()
			cp := mdbControlPlane(kuTestCluster)
			cp.Spec.Infrastructure.PublishedDatabaseEndpoint = tc.published
			h := newMDBHarness(t, kuTestCluster, nil, nil,
				append([]client.Object{order, cp}, mdbConverged(order, kuTestCluster)...)...)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			got := mdbGet(t, h)
			cond := expectMDBCondition(t, got, conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionTrue,
				reasonMariaDBDatabaseDelivered)
			g.Expect(cond.Message).To(HaveSuffix(`on target cluster "edge-1"`))
			secret := mdbDelivered(t, h)
			g.Expect(string(secret.Data["host"])).To(Equal(tc.wantHost))
			g.Expect(string(secret.Data["port"])).To(Equal(tc.wantPort))
			g.Expect(got.Status.Host).To(Equal(tc.wantHost))
			// The Secret is on the target cluster, not at home.
			g.Expect(h.mgmt.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})).NotTo(Succeed())
		})
	}

	t.Run("nothing is published", func(t *testing.T) {
		g := NewGomegaWithT(t)
		order := mariaDBDatabaseCR()
		h := newMDBHarness(t, kuTestCluster, nil, nil,
			append([]client.Object{order, mdbControlPlane(kuTestCluster)}, mdbConverged(order, kuTestCluster)...)...)

		result, err := h.reconcile(context.Background())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "a zero leg result comes back on the refresh")
		cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
			reasonMariaDBDatabaseNotPublished)
		g.Expect(cond.Message).To(Equal(`the order lives on target cluster "edge-1", which cannot reach the in-cluster ` +
			`MariaDB Service openstack-db.default.svc:3306; set spec.infrastructure.publishedDatabaseEndpoint on ` +
			`ControlPlane default/cp`))
		g.Expect(mdbDelivered(t, h)).To(BeNil())
	})
}

// TestMariaDBDatabase_KeystoneOutsideThePlanesNamespace runs an order through
// the provision, the delivery and the teardown against a Keystone outside the
// ControlPlane's namespace: placed on a target cluster, or in a namespace of
// its own at home. The MariaDB, the Database CR and the CA bundle are read and
// written in Keystone's namespace through Keystone's cluster client, and the
// ESO objects stay in the plane's namespace at home.
func TestMariaDBDatabase_KeystoneOutsideThePlanesNamespace(t *testing.T) {
	cases := []struct {
		name string
		// placedOn is Keystone's target cluster, empty for the management cluster.
		placedOn string
		wantHost string
	}{
		{name: "placed on a target cluster", placedOn: "edge-db", wantHost: "db.example.test"},
		{name: "in a namespace of its own at home", wantHost: "openstack-db.identity.svc"},
	}
	for _, tc := range cases {
		// build seeds the plane's children at home and the MariaDB, the CA bundle
		// and keystoneObjs in "identity" on Keystone's cluster, and returns the
		// harness and Keystone's cluster client.
		build := func(t *testing.T, keystoneObjs ...client.Object) (*mdbHarness, client.Client) {
			t.Helper()
			order := mariaDBDatabaseCR()
			cp := mdbTLSControlPlane()
			cp.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
				Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
			}
			if tc.placedOn != "" {
				cp.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: tc.placedOn}
				cp.Spec.Infrastructure.PublishedDatabaseEndpoint = "db.example.test:13306"
			}
			mariadb, caBundle := mdbReadyMariaDB(), mdbCABundle()
			mariadb.Namespace, caBundle.Namespace = "identity", "identity"
			home := []client.Object{
				order, cp, mdbClientCertSecret(), mdbReadyExternalSecret(order, "", mdbTestRefresh),
				mdbMaterialisedSecret(order, "", "v-order-app-db-1", "pw-1"),
			}
			keystoneObjs = append(keystoneObjs, mariadb, caBundle)
			if tc.placedOn == "" {
				h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append(home, keystoneObjs...)...)
				return h, h.mgmt
			}
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, home...)
			keystone := orderFakeClient(t, nil, keystoneObjs...)
			h.reconciler().Resolver = &childrenResolver{children: keystone}
			return h, keystone
		}
		order := mariaDBDatabaseCR()
		dbKey := types.NamespacedName{Namespace: "identity", Name: mariaDBDatabaseChildName(order, "")}

		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h, keystone := build(t)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				"WaitingForDatabase")
			db := &mariadbv1alpha1.Database{}
			g.Expect(keystone.Get(context.Background(), dbKey, db)).To(Succeed(), "the Database CR is beside the MariaDB")
			if tc.placedOn != "" {
				g.Expect(labelledIn(t, h.mgmt, &mariadbv1alpha1.DatabaseList{}, mariaDBDatabaseNameLabel, mdbTestName)).
					To(BeZero(), "no Database CR at home")
			}

			db.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created", LastTransitionTime: metav1.Now(),
			}}
			g.Expect(keystone.Status().Update(context.Background(), db)).To(Succeed())
			_, err = h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionTrue,
				reasonMariaDBDatabaseDelivered)
			secret := mdbDelivered(t, h)
			g.Expect(string(secret.Data["host"])).To(Equal(tc.wantHost))
			g.Expect(secret.Data).To(HaveKeyWithValue("ca.crt", []byte("database-ca")), "the CA bundle beside Keystone")

			g.Expect(h.order.Delete(context.Background(), mdbGet(t, h))).To(Succeed())
			_, err = h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			err = keystone.Get(context.Background(), dbKey, &mariadbv1alpha1.Database{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the teardown deletes the Database CR beside the MariaDB")
		})

		t.Run(tc.name+", a foreign Database CR beside the MariaDB", func(t *testing.T) {
			g := NewGomegaWithT(t)
			foreign := &mariadbv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "identity"},
				Spec: mariadbv1alpha1.DatabaseSpec{
					MariaDBRef: mariadbv1alpha1.MariaDBRef{ObjectReference: mariadbv1alpha1.ObjectReference{Name: "openstack-db"}},
					Name:       "app_db",
				},
			}
			h, _ := build(t, foreign)

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDatabaseReady, metav1.ConditionFalse,
				reasonMariaDBDatabaseCollision)
			g.Expect(h.bao.recorded()).To(BeEmpty())
		})
	}
}

// TestMariaDBDatabase_Delivery_RefreshRewritesTheSecret follows an ESO refresh:
// the delivered Secret carries the new credential, credentialsRefreshedAt the
// ExternalSecret's refresh time, and the log notes the refresh once.
func TestMariaDBDatabase_Delivery_RefreshRewritesTheSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, mdbControlPlane("")}, mdbConverged(order, "")...)...)
	_, logs, err := h.reconcileLogged()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(logs).NotTo(ContainSubstring("delivered a refreshed database credential"), "the first delivery is no refresh")

	// ESO re-issues: a new user, a new password, a later refresh time.
	refreshed := metav1.NewTime(mdbTestRefresh.Add(24 * time.Hour))
	key := types.NamespacedName{Namespace: "default", Name: mariaDBDatabaseGeneratorName(order, "")}
	materialised := &corev1.Secret{}
	g.Expect(h.mgmt.Get(context.Background(), key, materialised)).To(Succeed())
	materialised.Data = map[string][]byte{"username": []byte("v-order-app-db-2"), "password": []byte("pw-2")}
	g.Expect(h.mgmt.Update(context.Background(), materialised)).To(Succeed())
	es := &esov1.ExternalSecret{}
	g.Expect(h.mgmt.Get(context.Background(), key, es)).To(Succeed())
	es.Status.RefreshTime = refreshed
	g.Expect(h.mgmt.Status().Update(context.Background(), es)).To(Succeed())

	// The materialised Secret carries the order's labels, so its event maps back.
	g.Expect(orderChildToRequests(mariaDBDatabaseLabelKeys)(context.Background(), materialised)).To(HaveLen(1))

	_, logs, err = h.reconcileLogged()

	g.Expect(err).NotTo(HaveOccurred())
	secret := mdbDelivered(t, h)
	g.Expect(string(secret.Data["username"])).To(Equal("v-order-app-db-2"))
	g.Expect(string(secret.Data["password"])).To(Equal("pw-2"))
	got := mdbGet(t, h)
	g.Expect(got.Status.CredentialsRefreshedAt.Equal(&refreshed)).To(BeTrue())
	g.Expect(strings.Count(logs, "delivered a refreshed database credential")).To(Equal(1))
	g.Expect(logs).To(ContainSubstring(`"username"="v-order-app-db-2"`))

	_, logs, err = h.reconcileLogged()

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(logs).NotTo(ContainSubstring("delivered a refreshed database credential"), "an unchanged credential is no refresh")
}

// TestMariaDBDatabase_Delivery_RepairsAnEditedAndADeletedSecret rewrites an
// edited key, keeps a key somebody else added, and re-creates a deleted
// Secret.
func TestMariaDBDatabase_Delivery_RepairsAnEditedAndADeletedSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, mdbControlPlane("")}, mdbConverged(order, "")...)...)
	_, err := h.reconcile(context.Background())
	g.Expect(err).NotTo(HaveOccurred())

	secret := mdbDelivered(t, h)
	secret.Data["password"] = []byte("edited")
	secret.Data["note"] = []byte("added by the owner")
	g.Expect(h.order.Update(context.Background(), secret)).To(Succeed())
	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	secret = mdbDelivered(t, h)
	g.Expect(string(secret.Data["password"])).To(Equal("pw-1"), "the edited password is repaired")
	g.Expect(string(secret.Data["note"])).To(Equal("added by the owner"), "a key somebody else added stays")

	g.Expect(h.order.Delete(context.Background(), secret)).To(Succeed())
	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	secret = mdbDelivered(t, h)
	g.Expect(secret).NotTo(BeNil(), "the deleted Secret is re-created")
	g.Expect(metav1.IsControlledBy(secret, mdbGet(t, h))).To(BeTrue())
}

func TestMariaDBDatabase_Delivery_RefusesASecretItDoesNotControl(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	stranger := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-db-credentials", Namespace: kuTestNamespace},
		Data:       map[string][]byte{"password": []byte("theirs")},
	}
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, mdbControlPlane(""), stranger}, mdbConverged(order, "")...)...)

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "a zero leg result comes back on the refresh")
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseDeliveryRefused)
	g.Expect(cond.Message).To(Equal("Secret tenant-a/app-db-credentials exists and is not owned by this order; " +
		"delete it or rename the order"))
	g.Expect(string(mdbDelivered(t, h).Data["password"])).To(Equal("theirs"))
}

// TestMariaDBDatabase_Delivery_PortOutOfRangeIsDeliveryError refuses a
// published endpoint whose port no client can connect to, and delivers
// nothing. The fake client applies no schema, as an object stored before the
// CRD's pattern bounded the port would not either.
func TestMariaDBDatabase_Delivery_PortOutOfRangeIsDeliveryError(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbControlPlane(kuTestCluster)
	cp.Spec.Infrastructure.PublishedDatabaseEndpoint = "db.example.test:99999"
	h := newMDBHarness(t, kuTestCluster, nil, nil, append([]client.Object{order, cp}, mdbConverged(order, kuTestCluster)...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred(), "only a ControlPlane edit lifts the refusal")
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseDeliveryError)
	g.Expect(cond.Message).To(Equal(`spec.infrastructure.publishedDatabaseEndpoint "db.example.test:99999" is not ` +
		`host:port: port "99999" is not in 1-65535`))
	g.Expect(mdbDelivered(t, h)).To(BeNil())
}

// TestMariaDBDatabase_Delivery_ApplyErrorIsDeliveryError fails the apply of
// the delivered Secret on the order's target cluster.
func TestMariaDBDatabase_Delivery_ApplyErrorIsDeliveryError(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbControlPlane(kuTestCluster)
	cp.Spec.Infrastructure.PublishedDatabaseEndpoint = "db.example.test:3306"
	h := newMDBHarness(t, kuTestCluster, nil, &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("admission webhook denied the Secret")
		},
	}, append([]client.Object{order, cp}, mdbConverged(order, kuTestCluster)...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(ContainSubstring("delivering Secret tenant-a/app-db-credentials")))
	cond := expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseDeliveryError)
	g.Expect(cond.Message).To(ContainSubstring("admission webhook denied the Secret"))
}

// TestMariaDBDatabase_Delivery_CABundleReadErrorIsDeliveryError fails the read
// of the CA bundle with something other than NotFound.
func TestMariaDBDatabase_Delivery_CABundleReadErrorIsDeliveryError(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "db-ca" {
				return errors.New("etcd timeout")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, nil, append([]client.Object{order, mdbTLSControlPlane(), mdbCABundle()}, mdbConverged(order, "")...)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).To(MatchError(ContainSubstring("reading database CA bundle default/db-ca: etcd timeout")))
	expectMDBCondition(t, mdbGet(t, h), conditionTypeMariaDBDatabaseDeliveryReady, metav1.ConditionFalse,
		reasonMariaDBDatabaseDeliveryError)
}

// TestSplitDatabaseEndpoint pins the split of the address into the host and
// the port the Secret carries, and its refusals.
func TestSplitDatabaseEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	host, port, err := splitDatabaseEndpoint("openstack-db.ns.svc:3306")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(host).To(Equal("openstack-db.ns.svc"))
	g.Expect(port).To(Equal(int32(3306)))

	_, _, err = splitDatabaseEndpoint("db.example.com")
	g.Expect(err).To(MatchError(ContainSubstring("missing port")))
	_, _, err = splitDatabaseEndpoint("db.example.com:http")
	g.Expect(err).To(MatchError(ContainSubstring(`port "http"`)))

	_, port, err = splitDatabaseEndpoint("db.example.com:65535")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(port).To(Equal(int32(65535)))
	for _, endpoint := range []string{"db.example.com:0", "db.example.com:65536", "db.example.com:99999"} {
		_, _, err = splitDatabaseEndpoint(endpoint)
		g.Expect(err).To(MatchError(ContainSubstring("is not in 1-65535")), endpoint)
	}
}
