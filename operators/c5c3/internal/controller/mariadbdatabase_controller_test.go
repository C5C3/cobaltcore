// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the MariaDBDatabase reconciler's lifecycle: the consent gate and
// the freeze, the Dynamic gate, the naming contract, the teardown and the
// watch mappers. The provision and the delivery legs have files of their own.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	"github.com/go-logr/logr/funcr"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/openbao"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// mdbTestName is the fixture order's name. Its default schema name is app_db,
// whose underscore the role's GRANT escapes.
const mdbTestName = "app-db"

// mdbTestRefresh is when ESO last refreshed the fixture credential.
var mdbTestRefresh = metav1.NewTime(time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC))

// mariaDBDatabaseCR returns the order "app-db" in kuTestNamespace, referencing
// the ControlPlane cp in "default" and already carrying the teardown
// finalizer, so a reconcile runs the gates instead of returning on the
// finalizer pass.
func mariaDBDatabaseCR() *c5c3v1alpha1.MariaDBDatabase {
	return &c5c3v1alpha1.MariaDBDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Name:       mdbTestName,
			Namespace:  kuTestNamespace,
			Generation: 1,
			UID:        types.UID("mdb-uid"),
			Finalizers: []string{mariaDBDatabaseFinalizerName},
		},
		Spec: c5c3v1alpha1.MariaDBDatabaseSpec{
			ControlPlaneRef: c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "default"},
			DeletionPolicy:  c5c3v1alpha1.MariaDBDatabaseDeletionPolicyRetain,
		},
	}
}

// mdbControlPlane returns a ControlPlane in "default" that assigns
// kuTestNamespace on cluster, runs its shared managed database openstack-db in
// Dynamic mode and reports DBCredentialsReady. Keystone is unplaced, so the
// MariaDB, the Database CRs and the ControlPlane's children share "default".
func mdbControlPlane(cluster string) *c5c3v1alpha1.ControlPlane {
	cp := keystoneUserControlPlane(assignOn(kuTestNamespace, cluster))
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
		Database: commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
			Database:   "keystone",
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type: conditionTypeDBCredentialsReady, Status: metav1.ConditionTrue, Reason: "Synced",
	})
	return cp
}

// mdbReadyMariaDB is the shared MariaDB, Ready.
func mdbReadyMariaDB() *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: "openstack-db", Namespace: "default"},
		Status: mariadbv1alpha1.MariaDBStatus{Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"},
		}},
	}
}

// mdbClientCertSecret is the Secret cert-manager issues for the shared OpenBao
// client Certificate.
func mdbClientCertSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: orderDBClientCertName, Namespace: "default"},
		Data: map[string][]byte{
			"tls.crt": []byte("client-cert"), "tls.key": []byte("client-key"), "ca.crt": []byte("openbao-ca"),
		},
	}
}

// mdbReadyDatabase is the order's Database CR as the mariadb-operator reports
// it once the schema exists.
func mdbReadyDatabase(order *c5c3v1alpha1.MariaDBDatabase, cluster string) *mariadbv1alpha1.Database {
	db := mariaDBDatabaseChild(order, cluster, "default", "openstack-db", mariaDBDatabaseName(order))
	db.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created"}}
	return db
}

// mdbReadyExternalSecret is the order's ExternalSecret once ESO has synced it
// at refresh.
func mdbReadyExternalSecret(order *c5c3v1alpha1.MariaDBDatabase, cluster string, refresh metav1.Time) *esov1.ExternalSecret {
	es := dbCredentialGeneratorExternalSecret(mdbTarget(order, cluster))
	es.Labels = mariaDBDatabaseRef(order, cluster).childLabels()
	es.Status = esov1.ExternalSecretStatus{
		Conditions:  []esov1.ExternalSecretStatusCondition{{Type: esov1.ExternalSecretReady, Status: corev1.ConditionTrue}},
		RefreshTime: refresh,
	}
	return es
}

// mdbTarget is the dbCredentialTarget the provision leg builds for the order.
func mdbTarget(order *c5c3v1alpha1.MariaDBDatabase, cluster string) dbCredentialTarget {
	roleName := mariaDBDatabaseRoleName("default", order, cluster)
	return dbCredentialTarget{
		namespace: "default", secretName: mariaDBDatabaseGeneratorName(order, cluster),
		certName: orderDBClientCertName, saName: orderDBCredentialServiceAccountName,
		vaultRole: orderDBDynamicVaultRole, credsPath: dbEngineCredsPath(roleName),
		targetLabels: mariaDBDatabaseRef(order, cluster).childLabels(),
	}
}

// mdbMaterialisedSecret is the Secret ESO materialised for the order's
// ExternalSecret.
func mdbMaterialisedSecret(order *c5c3v1alpha1.MariaDBDatabase, cluster, username, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: mariaDBDatabaseGeneratorName(order, cluster), Namespace: "default",
			Labels: mariaDBDatabaseRef(order, cluster).childLabels(),
		},
		Data: map[string][]byte{"username": []byte(username), "password": []byte(password)},
	}
}

// mdbConverged seeds what an order on cluster has at home once OpenBao, the
// mariadb-operator and ESO have converged: the Ready MariaDB, the issued client
// certificate, the Ready Database CR, the synced ExternalSecret and the
// credential it materialised.
func mdbConverged(order *c5c3v1alpha1.MariaDBDatabase, cluster string) []client.Object {
	return []client.Object{
		mdbReadyMariaDB(), mdbClientCertSecret(), mdbReadyDatabase(order, cluster),
		mdbReadyExternalSecret(order, cluster, mdbTestRefresh),
		mdbMaterialisedSecret(order, cluster, "v-order-app-db-1", "pw-1"),
	}
}

// fakeOpenBao records every call the reconciler makes on OpenBao. It is the
// dial seam and the session at once.
type fakeOpenBao struct {
	mu      sync.Mutex
	roles   map[string]openbao.DatabaseRole
	calls   []string
	writes  []openbao.DatabaseRole
	configs []openbao.Config
	// dialErr fails the login; failOn fails the call of that name ("read",
	// "write", "revoke", "delete") with err.
	dialErr error
	failOn  string
	err     error
}

func newFakeOpenBao() *fakeOpenBao { return &fakeOpenBao{roles: map[string]openbao.DatabaseRole{}} }

func (f *fakeOpenBao) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeOpenBao) fails(name string) error {
	if f.failOn == name {
		return f.err
	}
	return nil
}

func (f *fakeOpenBao) dial(_ context.Context, cfg openbao.Config) (openbao.Client, error) {
	f.record("dial")
	f.mu.Lock()
	f.configs = append(f.configs, cfg)
	f.mu.Unlock()
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f, nil
}

func (f *fakeOpenBao) ReadDatabaseRole(_ context.Context, mount, name string) (*openbao.DatabaseRole, bool, error) {
	f.record("read " + mount + " " + name)
	if err := f.fails("read"); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := f.roles[name]
	if !ok {
		return nil, false, nil
	}
	return &role, true, nil
}

func (f *fakeOpenBao) WriteDatabaseRole(_ context.Context, mount, name string, role openbao.DatabaseRole) error {
	f.record("write " + mount + " " + name)
	if err := f.fails("write"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[name] = role
	f.writes = append(f.writes, role)
	return nil
}

func (f *fakeOpenBao) DeleteDatabaseRole(_ context.Context, mount, name string) error {
	f.record("delete " + mount + " " + name)
	if err := f.fails("delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.roles, name)
	return nil
}

func (f *fakeOpenBao) RevokeLeasePrefix(_ context.Context, prefix string) error {
	f.record("revoke " + prefix)
	return f.fails("revoke")
}

func (f *fakeOpenBao) Close(context.Context) error {
	f.record("close")
	return nil
}

// role returns the stored role name, read under the lock: the envtest manager
// writes it from its own goroutine.
func (f *fakeOpenBao) role(name string) (openbao.DatabaseRole, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role, ok := f.roles[name]
	return role, ok
}

// recorded returns the calls so far.
func (f *fakeOpenBao) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// count returns how many recorded calls start with prefix.
func (f *fakeOpenBao) count(prefix string) int {
	n := 0
	for _, call := range f.recorded() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// mdbHarness is the shared order harness with the fake OpenBao wired in.
type mdbHarness struct {
	*orderHarness
	bao *fakeOpenBao
	// token is what ServiceAccountToken returns; tokenErr fails it.
	tokenErr error
}

func newMDBHarness(t *testing.T, cluster string, mgmtFuncs, orderFuncs *interceptor.Funcs, objs ...client.Object) *mdbHarness {
	t.Helper()
	h := &mdbHarness{bao: newFakeOpenBao()}
	h.orderHarness = newOrderHarness(t, cluster, types.NamespacedName{Namespace: kuTestNamespace, Name: mdbTestName},
		mgmtFuncs, orderFuncs, objs, func(mgmt client.Client, resolver commonmulticluster.ClusterResolver) orderReconciler {
			return &MariaDBDatabaseReconciler{
				Client: mgmt, Scheme: mgmt.Scheme(), Resolver: resolver,
				OpenBaoDial: h.bao.dial,
				ServiceAccountToken: func() (string, error) {
					if h.tokenErr != nil {
						return "", h.tokenErr
					}
					return "operator-jwt", nil
				},
			}
		})
	return h
}

func (h *mdbHarness) reconciler() *MariaDBDatabaseReconciler {
	return h.r.(*MariaDBDatabaseReconciler)
}

// reconcileLogged runs one pass with a logger that records every line.
func (h *mdbHarness) reconcileLogged() (ctrl.Result, string, error) {
	var (
		mu   sync.Mutex
		logs []string
	)
	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, prefix+" "+args)
	}, funcr.Options{Verbosity: 1})
	result, err := h.reconcile(log.IntoContext(context.Background(), logger))
	return result, strings.Join(logs, "\n"), err
}

func mdbGet(t *testing.T, h *mdbHarness) *c5c3v1alpha1.MariaDBDatabase {
	t.Helper()
	return reloadOrder[c5c3v1alpha1.MariaDBDatabase](t, h.orderHarness)
}

func mdbCondition(order *c5c3v1alpha1.MariaDBDatabase, condType string) *metav1.Condition {
	return conditions.GetCondition(order.Status.Conditions, condType)
}

// expectMDBCondition asserts condType reads status with reason.
func expectMDBCondition(t *testing.T, order *c5c3v1alpha1.MariaDBDatabase, condType string, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)
	cond := mdbCondition(order, condType)
	g.Expect(cond).NotTo(BeNil(), condType)
	g.Expect(cond.Status).To(Equal(status), condType)
	g.Expect(cond.Reason).To(Equal(reason), condType+": "+cond.Message)
	return cond
}

// expectMDBBothConditions asserts both sub-conditions read False with reason.
func expectMDBBothConditions(t *testing.T, order *c5c3v1alpha1.MariaDBDatabase, reason string) {
	t.Helper()
	for _, condType := range mariaDBDatabaseSubConditionTypes {
		expectMDBCondition(t, order, condType, metav1.ConditionFalse, reason)
	}
}

// mdbLabelled counts the objects in "default" (the ControlPlane's and
// Keystone's namespace) that carry the order's name label.
func mdbLabelled(t *testing.T, c client.Client) int {
	t.Helper()
	key := mariaDBDatabaseLabelKeys.Name
	return labelledIn(t, c, &esov1.ExternalSecretList{}, key, mdbTestName) +
		labelledIn(t, c, &esgenv1alpha1.VaultDynamicSecretList{}, key, mdbTestName) +
		labelledIn(t, c, &mariadbv1alpha1.DatabaseList{}, key, mdbTestName)
}

// mdbProjectedIdentity reports whether the shared ServiceAccount exists in
// "default".
func mdbProjectedIdentity(t *testing.T, c client.Client) bool {
	t.Helper()
	sa := &corev1.ServiceAccount{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: orderDBCredentialServiceAccountName}, sa)
	return err == nil
}

// --- lifecycle and gates ---

func TestMariaDBDatabase_InstallsTheFinalizerOnceAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	order.Finalizers = nil
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, mdbControlPlane(""))

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).NotTo(BeZero(), "the finalizer pass requeues")
	g.Expect(mdbGet(t, h).Finalizers).To(ContainElement(mariaDBDatabaseFinalizerName))
	g.Expect(h.bao.recorded()).To(BeEmpty())
}

// TestMariaDBDatabase_NamespaceNotAssigned covers the freeze's entry: an order
// whose namespace has no assignment is refused on both conditions, gets no
// finalizer, and nothing is written to OpenBao or to the ControlPlane's and
// Keystone's namespace.
func TestMariaDBDatabase_NamespaceNotAssigned(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	order.Finalizers = nil
	cp := mdbControlPlane("")
	cp.Spec.NamespaceAssignments = nil
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, cp, mdbReadyMariaDB(), mdbClientCertSecret())

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(namespaceAssignmentRequeueAfter))
	got := mdbGet(t, h)
	expectMDBBothConditions(t, got, reasonOrderNamespaceNotAssigned)
	g.Expect(got.Finalizers).To(BeEmpty())
	g.Expect(h.bao.recorded()).To(BeEmpty(), "nothing is written to OpenBao")
	g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero())
	g.Expect(mdbProjectedIdentity(t, h.mgmt)).To(BeFalse(), "the shared identity is not projected")
}

// TestMariaDBDatabase_WithdrawnAssignmentFreezes pins the freeze of a delivered
// order: its Secret, its Database CR, its role and its ESO objects stay, and an
// edited password is not repaired.
func TestMariaDBDatabase_WithdrawnAssignmentFreezes(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	cp := mdbControlPlane("")
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil,
		append([]client.Object{order, cp}, mdbConverged(order, "")...)...)
	_, err := h.reconcile(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(conditions.AllTrue(mdbGet(t, h).Status.Conditions, "Ready")).To(BeTrue())

	// The owner edits the password, and the platform operator withdraws the
	// assignment.
	delivered := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: kuTestNamespace, Name: mariaDBDatabaseCredentialsSecretName(order)}
	g.Expect(h.order.Get(context.Background(), secretKey, delivered)).To(Succeed())
	delivered.Data["password"] = []byte("edited")
	g.Expect(h.order.Update(context.Background(), delivered)).To(Succeed())
	live := &c5c3v1alpha1.ControlPlane{}
	g.Expect(h.mgmt.Get(context.Background(), client.ObjectKeyFromObject(cp), live)).To(Succeed())
	live.Spec.NamespaceAssignments = nil
	g.Expect(h.mgmt.Update(context.Background(), live)).To(Succeed())
	callsBefore := len(h.bao.recorded())

	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	expectMDBBothConditions(t, mdbGet(t, h), reasonOrderNamespaceNotAssigned)
	g.Expect(h.order.Get(context.Background(), secretKey, delivered)).To(Succeed())
	g.Expect(string(delivered.Data["password"])).To(Equal("edited"), "a frozen order repairs nothing")
	g.Expect(mdbLabelled(t, h.mgmt)).To(Equal(3), "the Database CR and the ESO objects stay")
	g.Expect(h.bao.recorded()).To(HaveLen(callsBefore), "the role is neither read nor removed")
	_, kept := h.bao.role(mariaDBDatabaseRoleName("default", order, ""))
	g.Expect(kept).To(BeTrue(), "the role stays while the order is frozen")
}

// TestMariaDBDatabase_DynamicUnavailable refuses the four ControlPlane shapes
// whose shared database the OpenBao engine does not issue for, on both
// conditions, with nothing projected and nothing dialled.
func TestMariaDBDatabase_DynamicUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cp *c5c3v1alpha1.ControlPlane)
	}{
		{name: "External mode", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Spec.Services.Keystone.Mode = c5c3v1alpha1.KeystoneModeExternal
			cp.Spec.Infrastructure = nil
		}},
		{name: "a brownfield database", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Spec.Infrastructure.Database = commonv1.DatabaseSpec{Host: "db.example.com", Database: "keystone"}
		}},
		{name: "a dedicated Keystone database", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Spec.Services.Keystone.DedicatedBackingServices = &c5c3v1alpha1.KeystoneDedicatedBackingServicesSpec{
				Database: &commonv1.DatabaseSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "keystone-db"}, Database: "keystone",
				},
			}
		}},
		{name: "credentialsMode Static", mutate: func(cp *c5c3v1alpha1.ControlPlane) {
			cp.Spec.Infrastructure.Database.CredentialsMode = commonv1.CredentialsModeStatic
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := mdbControlPlane("")
			tc.mutate(cp)
			order := mariaDBDatabaseCR()
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, order, cp, mdbReadyMariaDB(), mdbClientCertSecret())

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(orderRefreshAfter), "a zero leg result comes back on the refresh")
			got := mdbGet(t, h)
			expectMDBBothConditions(t, got, reasonMariaDBDatabaseDynamicUnavailable)
			g.Expect(mdbCondition(got, conditionTypeMariaDBDatabaseDatabaseReady).Message).To(
				ContainSubstring("ControlPlane default/cp does not issue dynamic credentials for its shared database"))
			g.Expect(h.bao.recorded()).To(BeEmpty())
			g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero())
			g.Expect(mdbProjectedIdentity(t, h.mgmt)).To(BeFalse())
		})
	}
}

func TestMariaDBDatabase_WaitsForDBCredentials(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := mdbControlPlane("")
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type: conditionTypeDBCredentialsReady, Status: metav1.ConditionFalse, Reason: "WaitingForESO",
	})
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mariaDBDatabaseCR(), cp, mdbReadyMariaDB())

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(dbCredentialsRequeueAfter))
	got := mdbGet(t, h)
	expectMDBBothConditions(t, got, reasonMariaDBDatabaseWaitingForDBCredentials)
	g.Expect(mdbCondition(got, conditionTypeMariaDBDatabaseDeliveryReady).Message).To(
		ContainSubstring("deploy/openbao/bootstrap/setup-database-tenant.sh"))
	g.Expect(h.bao.recorded()).To(BeEmpty())
	g.Expect(mdbProjectedIdentity(t, h.mgmt)).To(BeFalse())
}

func TestMariaDBDatabase_UnresolvableClusterIsSkipped(t *testing.T) {
	g := NewGomegaWithT(t)

	h := newMDBHarness(t, kuTestCluster, nil, nil, mariaDBDatabaseCR(), mdbControlPlane(kuTestCluster))
	h.reconciler().Resolver = &childrenResolver{err: mcruntime.ErrClusterNotFound}

	result, logs, err := h.reconcileLogged()

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(logs).To(ContainSubstring(`"cluster"="edge-1"`))
	g.Expect(mdbGet(t, h).Status.Conditions).To(BeEmpty())
	g.Expect(h.bao.recorded()).To(BeEmpty())
}

// TestMariaDBDatabase_PassResult pins when a converged order comes back: on an
// event on the management cluster, on the refresh on a target cluster.
func TestMariaDBDatabase_PassResult(t *testing.T) {
	cases := []struct {
		name    string
		cluster string
		want    time.Duration
	}{
		{name: "management cluster", cluster: c5c3v1alpha1.ManagementCluster, want: 0},
		{name: "target cluster", cluster: kuTestCluster, want: orderRefreshAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mariaDBDatabaseCR()
			cp := mdbControlPlane(tc.cluster)
			cp.Spec.Infrastructure.PublishedDatabaseEndpoint = "db.example.test:3306"
			h := newMDBHarness(t, tc.cluster, nil, nil, append([]client.Object{order, cp}, mdbConverged(order, tc.cluster)...)...)

			result, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(conditions.AllTrue(mdbGet(t, h).Status.Conditions, "Ready")).To(BeTrue())
			g.Expect(result.RequeueAfter).To(Equal(tc.want))
		})
	}
}

// --- naming and defaults ---

func TestMariaDBDatabase_DefaultsDatabaseNameAndDeletionPolicy(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	g.Expect(mariaDBDatabaseName(order)).To(Equal("app_db"), "every dash of the name is an underscore")
	order.Spec.DatabaseName = "Custom_DB"
	g.Expect(mariaDBDatabaseName(order)).To(Equal("Custom_DB"), "an explicit name is taken verbatim")

	g.Expect(mariaDBDatabaseDeletionPolicy(order)).To(Equal("Retain"))
	order.Spec.DeletionPolicy = ""
	g.Expect(mariaDBDatabaseDeletionPolicy(order)).To(Equal("Retain"), "a stored order without the field reads as Retain")
	g.Expect(*mariaDBDatabaseChild(order, "", "default", "openstack-db", "app_db").Spec.CleanupPolicy).To(
		Equal(mariadbv1alpha1.CleanupPolicySkip))
	order.Spec.DeletionPolicy = c5c3v1alpha1.MariaDBDatabaseDeletionPolicyDelete
	g.Expect(*mariaDBDatabaseChild(order, "", "default", "openstack-db", "app_db").Spec.CleanupPolicy).To(
		Equal(mariadbv1alpha1.CleanupPolicyDelete))
}

func TestMariaDBDatabase_ChildNamesAndRoleName(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	prefix := mariaDBDatabaseRef(order, "").childPrefix()
	g.Expect(prefix).To(MatchRegexp(`^app-db-[0-9a-f]{8}-database-$`))
	g.Expect(mariaDBDatabaseChildName(order, "")).To(Equal(prefix + "database"))
	g.Expect(mariaDBDatabaseGeneratorName(order, "")).To(Equal(prefix + "credentials"))
	g.Expect(mariaDBDatabaseRoleName("openstack", order, "")).To(Equal("order.openstack." + strings.TrimSuffix(prefix, "-")))
	g.Expect(mariaDBDatabaseCredentialsSecretName(order)).To(Equal("app-db-credentials"))
	g.Expect(mariaDBDatabaseRef(order, kuTestCluster).childPrefix()).NotTo(Equal(prefix),
		"the same order on another cluster is another order")
}

// --- teardown ---

// mdbDeleting returns the fixture order marked for deletion.
func mdbDeleting() *c5c3v1alpha1.MariaDBDatabase {
	order := mariaDBDatabaseCR()
	order.DeletionTimestamp = ptr.To(metav1.Now())
	return order
}

// mdbDeliveredSecret is the Secret the order delivered beside itself.
func mdbDeliveredSecret(order *c5c3v1alpha1.MariaDBDatabase) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: mariaDBDatabaseCredentialsSecretName(order), Namespace: order.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: c5c3v1alpha1.GroupVersion.String(), Kind: "MariaDBDatabase",
				Name: order.Name, UID: order.UID, Controller: ptr.To(true),
			}},
		},
		Data: map[string][]byte{"password": []byte("pw-1")},
	}
}

// mdbTeardownObjects seeds a deleting order with everything it created.
func mdbTeardownObjects(order *c5c3v1alpha1.MariaDBDatabase, withCP bool) []client.Object {
	objs := append([]client.Object{
		order, mdbDeliveredSecret(order),
		dbCredentialVaultDynamicSecret(mdbTarget(order, ""), openBaoDefaultServer, openBaoDefaultKubernetesMount),
	},
		mdbConverged(order, "")...)
	objs[2].SetLabels(mariaDBDatabaseRef(order, "").childLabels())
	if withCP {
		objs = append(objs, mdbControlPlane(""))
	}
	return objs
}

// TestMariaDBDatabase_DeleteRevokesThenDeletesThenSweeps pins the teardown
// with the ControlPlane present: the leases are revoked and the role deleted
// first, then the ESO objects, the Database CR and the delivered Secret are
// deleted, and the finalizer is held until none of them is listed.
func TestMariaDBDatabase_DeleteRevokesThenDeletesThenSweeps(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mdbDeleting()
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mdbTeardownObjects(order, true)...)
	roleName := mariaDBDatabaseRoleName("default", order, "")
	h.bao.roles[roleName] = mariaDBDatabaseOpenBaoRole(mdbControlPlane(""), "app_db")

	result, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.bao.recorded()).To(Equal([]string{
		"dial",
		"revoke database/mariadb/creds/" + roleName,
		"delete database/mariadb " + roleName,
		"close",
	}))
	g.Expect(h.bao.configs[0].Role).To(Equal(openBaoOperatorAuthRole))
	g.Expect(h.bao.configs[0].JWT).To(Equal("operator-jwt"))
	g.Expect(h.bao.configs[0].ClientCert).To(Equal([]byte("client-cert")))
	_, kept := h.bao.role(roleName)
	g.Expect(kept).To(BeFalse(), "the role is deleted")
	g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "the ESO objects and the Database CR are deleted")
	err = h.order.Get(context.Background(), client.ObjectKeyFromObject(mdbDeliveredSecret(order)), &corev1.Secret{})
	g.Expect(err).To(HaveOccurred(), "the delivered Secret is deleted")
	g.Expect(result.RequeueAfter).To(Equal(korcRequeueAfter), "the finalizer holds while deletes were issued")
	g.Expect(mdbGet(t, h).Finalizers).To(ContainElement(mariaDBDatabaseFinalizerName))

	_, err = h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mdbGet(t, h)).To(BeNil(), "the finalizer is released once nothing remains")
	g.Expect(h.bao.count("revoke")).To(Equal(2), "the OpenBao step repeats without harm")
}

// TestMariaDBDatabase_DeleteRetriesAFailingOpenBaoCall holds the finalizer with
// the plane present while any part of the OpenBao step fails, and sweeps
// nothing before the role is gone.
func TestMariaDBDatabase_DeleteRetriesAFailingOpenBaoCall(t *testing.T) {
	sealed := &openbao.APIError{StatusCode: 500, Errors: []string{"sealed"}}
	cases := []struct {
		name      string
		setup     func(h *mdbHarness)
		mgmtFuncs *interceptor.Funcs
		want      string
		dialled   bool
	}{
		{
			name:    "the lease revocation fails",
			setup:   func(h *mdbHarness) { h.bao.failOn, h.bao.err = "revoke", sealed },
			want:    "sealed",
			dialled: true,
		},
		{
			name:    "the role deletion fails",
			setup:   func(h *mdbHarness) { h.bao.failOn, h.bao.err = "delete", sealed },
			want:    "sealed",
			dialled: true,
		},
		{
			name:  "the login fails",
			setup: func(h *mdbHarness) { h.bao.dialErr = errors.New("connection refused") },
			want:  "connection refused",
		},
		{
			name:  "the client certificate cannot be read",
			setup: func(*mdbHarness) {},
			mgmtFuncs: &interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == orderDBClientCertName {
						return errors.New("etcd timeout")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
			want: "reading the OpenBao client certificate default/order-db-openbao-client: etcd timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mdbDeleting()
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, tc.mgmtFuncs, nil, mdbTeardownObjects(order, true)...)
			tc.setup(h)

			_, err := h.reconcile(context.Background())

			g.Expect(err).To(MatchError(ContainSubstring("removing OpenBao database role")))
			g.Expect(err).To(MatchError(ContainSubstring(tc.want)))
			g.Expect(mdbGet(t, h).Finalizers).To(ContainElement(mariaDBDatabaseFinalizerName))
			g.Expect(mdbLabelled(t, h.mgmt)).To(Equal(3), "nothing is swept before the role is gone")
			delivered := client.ObjectKeyFromObject(mdbDeliveredSecret(order))
			g.Expect(h.order.Get(context.Background(), delivered, &corev1.Secret{})).To(Succeed(), "the delivered Secret stays")
			if tc.dialled {
				g.Expect(h.bao.recorded()).To(ContainElement("close"), "the session is closed after a failed call")
			}
		})
	}
}

// TestMariaDBDatabase_DeleteSkipsOpenBaoWithoutTheClientCertificate leaves the
// role to its TTLs when the operator cannot log in, and still sweeps.
func TestMariaDBDatabase_DeleteSkipsOpenBaoWithoutTheClientCertificate(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mdbDeleting()
	var objs []client.Object
	for _, obj := range mdbTeardownObjects(order, true) {
		if obj.GetName() != orderDBClientCertName {
			objs = append(objs, obj)
		}
	}
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

	_, logs, err := h.reconcileLogged()

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(logs).To(ContainSubstring(`the OpenBao client certificate is gone; leaving database role \"order.default.`))
	g.Expect(h.bao.recorded()).To(BeEmpty())
	g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "the deletes are issued")
}

// TestMariaDBDatabase_DeleteFailsOpenWithoutTheControlPlane still removes the
// role through the client certificate in the plane's namespace, issues every
// Kubernetes delete whether or not OpenBao answers, and releases the finalizer
// in one pass whatever the outcome.
func TestMariaDBDatabase_DeleteFailsOpenWithoutTheControlPlane(t *testing.T) {
	cases := []struct {
		name      string
		failOn    string
		dialErr   error
		wantCalls []string
	}{
		{name: "OpenBao answers", wantCalls: []string{"dial", "revoke", "delete", "close"}},
		{name: "the revocation fails", failOn: "revoke", wantCalls: []string{"dial", "revoke", "close"}},
		{name: "the login fails", dialErr: errors.New("connection refused"), wantCalls: []string{"dial"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			order := mdbDeleting()
			h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, mdbTeardownObjects(order, false)...)
			h.bao.failOn, h.bao.err, h.bao.dialErr = tc.failOn, errors.New("connection refused"), tc.dialErr

			_, err := h.reconcile(context.Background())

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mdbGet(t, h)).To(BeNil(), "the finalizer is released with the plane gone")
			calls := h.bao.recorded()
			g.Expect(calls).To(HaveLen(len(tc.wantCalls)))
			for i, want := range tc.wantCalls {
				g.Expect(calls[i]).To(HavePrefix(want))
			}
			if len(calls) > 1 {
				g.Expect(calls[1]).To(Equal("revoke database/mariadb/creds/" + mariaDBDatabaseRoleName("default", order, "")))
			}
			g.Expect(h.bao.configs[0].Server).To(Equal(openBaoDefaultServer), "the default store names the server")
			g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero(), "the ESO objects and the Database CR are deleted")
			err = h.order.Get(context.Background(), client.ObjectKeyFromObject(mdbDeliveredSecret(order)), &corev1.Secret{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the delivered Secret is deleted")
		})
	}
}

// TestMariaDBDatabase_DeleteAppliesTheDeletionPolicyInForce writes the order's
// deletion policy into the Database CR's cleanupPolicy before deleting it,
// whatever the CR carried: no provision pass needs to have run since the
// policy changed. The mariadb-operator's finalizer keeps the deleted CR
// readable.
func TestMariaDBDatabase_DeleteAppliesTheDeletionPolicyInForce(t *testing.T) {
	cases := []struct {
		policy      string
		stale, want mariadbv1alpha1.CleanupPolicy
	}{
		{
			policy: c5c3v1alpha1.MariaDBDatabaseDeletionPolicyDelete, stale: mariadbv1alpha1.CleanupPolicySkip,
			want: mariadbv1alpha1.CleanupPolicyDelete,
		},
		{
			policy: c5c3v1alpha1.MariaDBDatabaseDeletionPolicyRetain, stale: mariadbv1alpha1.CleanupPolicyDelete,
			want: mariadbv1alpha1.CleanupPolicySkip,
		},
	}
	for _, tc := range cases {
		for _, withCP := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, ControlPlane present %t", tc.policy, withCP), func(t *testing.T) {
				g := NewGomegaWithT(t)
				order := mdbDeleting()
				order.Spec.DeletionPolicy = tc.policy
				objs := mdbTeardownObjects(order, withCP)
				for _, obj := range objs {
					if db, ok := obj.(*mariadbv1alpha1.Database); ok {
						db.Spec.CleanupPolicy = ptr.To(tc.stale)
						db.Finalizers = []string{"database.k8s.mariadb.com/finalizer"}
					}
				}
				h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, objs...)

				_, err := h.reconcile(context.Background())

				g.Expect(err).NotTo(HaveOccurred())
				db := &mariadbv1alpha1.Database{}
				key := types.NamespacedName{Namespace: "default", Name: mariaDBDatabaseChildName(order, "")}
				g.Expect(h.mgmt.Get(context.Background(), key, db)).To(Succeed())
				g.Expect(db.DeletionTimestamp).NotTo(BeNil(), "the Database CR is deleted")
				g.Expect(db.Spec.CleanupPolicy).To(HaveValue(Equal(tc.want)))
			})
		}
	}
}

// TestMariaDBDatabase_DeleteOfAFrozenOrderTearsDown consults no assignment: a
// withdrawn one freezes an order, and deleting a frozen order still tears it
// down.
func TestMariaDBDatabase_DeleteOfAFrozenOrderTearsDown(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mdbDeleting()
	objs := mdbTeardownObjects(order, false)
	cp := mdbControlPlane("")
	cp.Spec.NamespaceAssignments = nil
	h := newMDBHarness(t, c5c3v1alpha1.ManagementCluster, nil, nil, append(objs, cp)...)

	_, err := h.reconcile(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h.bao.count("delete")).To(Equal(1))
	g.Expect(mdbLabelled(t, h.mgmt)).To(BeZero())
}

// --- watches ---

func TestMariaDBDatabaseChildToRequests(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	db := mdbReadyDatabase(order, kuTestCluster)
	g.Expect(orderChildToRequests(mariaDBDatabaseLabelKeys)(context.Background(), db)).To(Equal([]mcreconcile.Request{{
		Request:     reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: mdbTestName}},
		ClusterName: kuTestCluster,
	}}), "a Database CR at home maps to the order on the cluster its label names")
	g.Expect(orderChildToRequests(mariaDBDatabaseLabelKeys)(context.Background(), mdbClientCertSecret())).To(BeEmpty(),
		"the shared client certificate belongs to no order")
}

// TestDatabaseToMariaDBDatabaseRequests maps a Database CR back to its order
// only from Keystone's namespace on Keystone's cluster, so one an owner writes
// with another order's labels in an assigned namespace wakes nothing.
func TestDatabaseToMariaDBDatabaseRequests(t *testing.T) {
	order := mariaDBDatabaseCR()
	want := []mcreconcile.Request{{
		Request:     reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kuTestNamespace, Name: mdbTestName}},
		ClusterName: kuTestCluster,
	}}
	// home runs Keystone in the plane's namespace on the management cluster.
	home := mdbControlPlane(kuTestCluster)
	// placed runs Keystone in "identity" on kuTestCluster.
	placed := mdbControlPlane(kuTestCluster)
	placed.Name = "placed"
	placed.Spec.Services.Keystone.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "identity", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	placed.Spec.Services.Keystone.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: kuTestCluster}

	cases := []struct {
		name      string
		cluster   mcruntime.ClusterName
		namespace string
		want      []mcreconcile.Request
	}{
		{
			name: "Keystone's namespace at home", cluster: mcruntime.ClusterName(c5c3v1alpha1.ManagementCluster),
			namespace: "default", want: want,
		},
		{name: "a placed Keystone's namespace on its cluster", cluster: kuTestCluster, namespace: "identity", want: want},
		{name: "an assigned namespace", cluster: kuTestCluster, namespace: kuTestNamespace},
		{name: "Keystone's namespace name on another cluster", cluster: kuTestCluster, namespace: "default"},
		{
			name:    "a placed Keystone's namespace name at home",
			cluster: mcruntime.ClusterName(c5c3v1alpha1.ManagementCluster), namespace: "identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			db := mdbReadyDatabase(order, kuTestCluster)
			db.Namespace = tc.namespace
			mapper := databaseToMariaDBDatabaseRequests(newMariaDBDatabaseMapperClient(t, nil, home, placed), tc.cluster)
			g.Expect(mapper(context.Background(), db)).To(Equal(tc.want))
		})
	}

	t.Run("a failing List maps to nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		failing := newMariaDBDatabaseMapperClient(t, &interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("cache not synced")
			},
		}, home)
		mapper := databaseToMariaDBDatabaseRequests(failing, mcruntime.ClusterName(c5c3v1alpha1.ManagementCluster))
		g.Expect(mapper(context.Background(), mdbReadyDatabase(order, kuTestCluster))).To(BeNil())
	})

	t.Run("an unlabelled Database maps to nothing without reading the cache", func(t *testing.T) {
		g := NewGomegaWithT(t)
		listed := false
		c := newMariaDBDatabaseMapperClient(t, &interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listed = true
				return cl.List(ctx, list, opts...)
			},
		}, home)
		db := mdbReadyDatabase(order, kuTestCluster)
		db.Labels = nil
		mapper := databaseToMariaDBDatabaseRequests(c, mcruntime.ClusterName(c5c3v1alpha1.ManagementCluster))
		g.Expect(mapper(context.Background(), db)).To(BeNil())
		g.Expect(listed).To(BeFalse(), "a Database without the order labels is no order's, whatever its namespace")
	})
}

func TestMariaDBDatabaseControlPlaneRefExtractor(t *testing.T) {
	g := NewGomegaWithT(t)

	order := mariaDBDatabaseCR()
	g.Expect(mariaDBDatabaseControlPlaneRefExtractor(order)).To(Equal([]string{"default/cp"}))
	order.Spec.ControlPlaneRef.Namespace = ""
	g.Expect(mariaDBDatabaseControlPlaneRefExtractor(order)).To(Equal([]string{"tenant-a/cp"}),
		"an empty namespace defaults to the order's own")
	order.Spec.ControlPlaneRef.Name = ""
	g.Expect(mariaDBDatabaseControlPlaneRefExtractor(order)).To(BeNil())
	g.Expect(mariaDBDatabaseControlPlaneRefExtractor(ksControlPlane())).To(BeNil())
}

func newMariaDBDatabaseMapperClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(korcTestScheme(t)).WithObjects(objs...).
		WithIndex(&c5c3v1alpha1.MariaDBDatabase{}, MariaDBDatabaseControlPlaneRefIndexKey, mariaDBDatabaseControlPlaneRefExtractor)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func TestControlPlaneToMariaDBDatabasesMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	explicit := mariaDBDatabaseCR()
	defaulted := mariaDBDatabaseCR()
	defaulted.Namespace, defaulted.Name = "default", "same-namespace"
	defaulted.Spec.ControlPlaneRef.Namespace = ""
	elsewhere := mariaDBDatabaseCR()
	elsewhere.Name = "other-plane"
	elsewhere.Spec.ControlPlaneRef = c5c3v1alpha1.ControlPlaneRefSpec{Name: "cp", Namespace: "other"}

	mapper := controlPlaneToMariaDBDatabasesMapper(newMariaDBDatabaseMapperClient(t, nil, explicit, defaulted, elsewhere))

	g.Expect(mapper(context.Background(), ksControlPlane())).To(ConsistOf(
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(explicit)},
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(defaulted)},
	))
	unrelated := ksControlPlane()
	unrelated.Name = "another"
	g.Expect(mapper(context.Background(), unrelated)).To(BeEmpty())

	failing := newMariaDBDatabaseMapperClient(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	}, explicit)
	g.Expect(controlPlaneToMariaDBDatabasesMapper(failing)(context.Background(), ksControlPlane())).To(BeNil())
}
