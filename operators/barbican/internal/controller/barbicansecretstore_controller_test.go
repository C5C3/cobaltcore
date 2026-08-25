// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	api "github.com/openbao/openbao/api/v2"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonconditions "github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	mctestutil "github.com/c5c3/cobaltcore/internal/common/testutil/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/metrics"
	"github.com/c5c3/cobaltcore/operators/barbican/internal/openbao"
)

const (
	testNamespace    = "openstack"
	testStoreName    = "primary"
	testInstanceName = "openbao-instance"
	testBarbicanName = "test-barbican"

	testRoleID   = "role-id-value"
	testSecretID = "secret-id-value"
	testCAPEM    = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"

	// testSecretIDTTL is the TTL the fake server reports for a minted secret ID:
	// one hour, so the proactive threshold sits at 40 minutes.
	testSecretIDTTL = int64(3600)
)

// testClock is the fixed wall clock injected into every fixture, so the re-mint
// threshold is exercised by moving the Secret's mint stamp rather than by
// waiting.
var testClock = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// --- fakes -----------------------------------------------------------------

// fakeOpenBaoClient is a scriptable openbao.Client that records every call it
// receives, in order. The call log is what the tests assert the protocol
// against: which endpoints a mode touches, and that every session ends in a
// revocation.
type fakeOpenBaoClient struct {
	mu    sync.Mutex
	calls []string

	loginKubernetesErr error
	// loginAppRoleErrs is consumed one entry per LoginAppRole call, so a probe
	// can fail while the login validating the re-minted credentials succeeds.
	loginAppRoleErrs []error
	mountExists      bool
	mountExistsErr   error
	roleID           string
	readRoleIDErr    error
	// secretIDs is consumed one entry per GenerateSecretID call; the last entry
	// is reused once the script runs out.
	secretIDs       []string
	secretIDTTL     int64
	generateErr     error
	capabilities    []string
	capabilitiesErr error
}

func (f *fakeOpenBaoClient) record(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
}

func (f *fakeOpenBaoClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// callCount returns how often method was called.
func (f *fakeOpenBaoClient) callCount(method string) int {
	count := 0
	for _, call := range f.callLog() {
		if call == method {
			count++
		}
	}
	return count
}

func (f *fakeOpenBaoClient) LoginKubernetes(_ context.Context, _, _ string) error {
	f.record("LoginKubernetes")
	return f.loginKubernetesErr
}

func (f *fakeOpenBaoClient) LoginAppRole(_ context.Context, _, _ string) error {
	f.record("LoginAppRole")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.loginAppRoleErrs) == 0 {
		return nil
	}
	err := f.loginAppRoleErrs[0]
	f.loginAppRoleErrs = f.loginAppRoleErrs[1:]
	return err
}

func (f *fakeOpenBaoClient) ReadRoleID(_ context.Context, _ string) (string, error) {
	f.record("ReadRoleID")
	return f.roleID, f.readRoleIDErr
}

func (f *fakeOpenBaoClient) GenerateSecretID(_ context.Context, _ string) (string, int64, error) {
	f.record("GenerateSecretID")
	if f.generateErr != nil {
		return "", 0, f.generateErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	secretID := testSecretID
	if len(f.secretIDs) > 0 {
		secretID = f.secretIDs[0]
		if len(f.secretIDs) > 1 {
			f.secretIDs = f.secretIDs[1:]
		}
	}
	return secretID, f.secretIDTTL, nil
}

func (f *fakeOpenBaoClient) CapabilitiesSelf(_ context.Context, _ string) ([]string, error) {
	f.record("CapabilitiesSelf")
	return f.capabilities, f.capabilitiesErr
}

func (f *fakeOpenBaoClient) MountExists(_ context.Context, _ string) (bool, error) {
	f.record("MountExists")
	return f.mountExists, f.mountExistsErr
}

func (f *fakeOpenBaoClient) RevokeSelfToken(_ context.Context) error {
	f.record("RevokeSelfToken")
	return nil
}

// newProvisionedFakeClient returns a fake answering the managed happy path: the
// mount is there, the AppRole hands out its role ID, and every login succeeds.
func newProvisionedFakeClient() *fakeOpenBaoClient {
	return &fakeOpenBaoClient{
		mountExists: true,
		roleID:      testRoleID,
		secretIDTTL: testSecretIDTTL,
	}
}

// fakeClientFactory stands in for openbao.New and records the Config every
// construction got, so the tests can assert what the controller derived from the
// instance name.
type fakeClientFactory struct {
	client  *fakeOpenBaoClient
	configs []openbao.Config
	err     error
}

func (f *fakeClientFactory) new(cfg openbao.Config) (openbao.Client, error) {
	f.configs = append(f.configs, cfg)
	if f.err != nil {
		return nil, f.err
	}
	return f.client, nil
}

// tokenRequest is one call the ServiceAccount-token seam received.
type tokenRequest struct {
	namespace string
	name      string
	audience  string
}

// fakeTokenMinter stands in for the TokenRequest subresource, which a fake
// client does not serve.
type fakeTokenMinter struct {
	requests []tokenRequest
	token    string
	err      error
}

func (f *fakeTokenMinter) mint(_ context.Context, namespace, name, audience string) (string, error) {
	f.requests = append(f.requests, tokenRequest{namespace: namespace, name: name, audience: audience})
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// responseError builds the typed OpenBao response error the classifiers branch
// on, so a test can script a 403 or a 404 the way the server would answer.
func responseError(status int, message string) error {
	return &api.ResponseError{StatusCode: status, Errors: []string{message}}
}

// --- fixtures --------------------------------------------------------------

// testScheme registers the types the fake client resolves: core/apps (Secret,
// ConfigMap, Deployment, ServiceAccount), the barbican API, the openbao.org API
// the managed mode reads the instance from, the external-secrets v1 group the
// credential gate reads to attribute a missing Secret, the Gateway API types the
// httproute step projects, and the MariaDB types the Barbican finalizer path
// tears down.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = barbicanv1alpha1.AddToScheme(s)
	_ = openbaov1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// storeFixture bundles a reconciler over a fake client with the seams the tests
// script and assert against.
type storeFixture struct {
	reconciler *BarbicanSecretStoreReconciler
	recorder   *record.FakeRecorder
	bao        *fakeClientFactory
	minter     *fakeTokenMinter
}

// newStoreFixture builds a reconciler over a fake client pre-loaded with objs,
// wired to the given OpenBao fake and to the fixed test clock.
func newStoreFixture(bao *fakeOpenBaoClient, objs ...client.Object) *storeFixture {
	// A store always attaches to a parent Barbican, and the reconciler resolves
	// the cluster its children belong on through that parent — without one it
	// holds on WaitingForParent rather than guessing. Seed the shared parent so
	// every fixture describes a store that can actually run; a test that brings
	// its own (a targeted parent, say) keeps it.
	if !containsBarbican(objs) {
		objs = append(objs, testBarbican())
	}
	return newStoreFixtureWithoutParent(bao, objs...)
}

// containsBarbican reports whether the caller already supplied a parent CR.
func containsBarbican(objs []client.Object) bool {
	for _, obj := range objs {
		if _, ok := obj.(*barbicanv1alpha1.Barbican); ok {
			return true
		}
	}
	return false
}

// newStoreFixtureWithoutParent builds the fixture from exactly the objects
// given. Only the dangling-spec.barbicanRef case needs it.
func newStoreFixtureWithoutParent(bao *fakeOpenBaoClient, objs ...client.Object) *storeFixture {
	scheme := testScheme()
	factory := &fakeClientFactory{client: bao}
	minter := &fakeTokenMinter{token: "sa-token"}
	recorder := record.NewFakeRecorder(20)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&barbicanv1alpha1.BarbicanSecretStore{}).
		WithIndex(&barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreBarbicanRefIndexKey, barbicanSecretStoreBarbicanRefExtractor).
		WithIndex(&barbicanv1alpha1.BarbicanSecretStore{}, BarbicanSecretStoreInstanceRefIndexKey, barbicanSecretStoreInstanceRefExtractor).
		Build()

	return &storeFixture{
		reconciler: &BarbicanSecretStoreReconciler{
			Client:                  fakeClient,
			Scheme:                  scheme,
			Recorder:                recorder,
			NewOpenBaoClient:        factory.new,
			MintServiceAccountToken: minter.mint,
			Now:                     func() time.Time { return testClock },
		},
		recorder: recorder,
		bao:      factory,
		minter:   minter,
	}
}

// reconcile drives one pass over the store fixture.
func (f *storeFixture) reconcile(t *testing.T, store *barbicanv1alpha1.BarbicanSecretStore) (reconcile.Result, error) {
	t.Helper()
	return f.reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(store),
	})
}

// reconcileRemote drives a store whose parent names a target cluster past its
// two teardown marks and returns the result of the pass that does the work. The
// annotation and the finalizer are written one per pass, each requeuing so the
// next pass reads its own write back.
func (f *storeFixture) reconcileRemote(t *testing.T, store *barbicanv1alpha1.BarbicanSecretStore) (reconcile.Result, error) {
	t.Helper()
	for range 2 {
		if result, err := f.reconcile(t, store); err != nil {
			return result, err
		}
	}
	return f.reconcile(t, store)
}

// store re-reads the reconciled store.
func (f *storeFixture) store(t *testing.T) *barbicanv1alpha1.BarbicanSecretStore {
	t.Helper()
	var store barbicanv1alpha1.BarbicanSecretStore
	if err := f.reconciler.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testStoreName}, &store); err != nil {
		t.Fatalf("re-reading BarbicanSecretStore: %v", err)
	}
	return &store
}

// credentialsSecret re-reads the minted AppRole credentials Secret.
func (f *storeFixture) credentialsSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}
	if err := f.reconciler.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("re-reading credentials Secret %s: %v", key, err)
	}
	return &secret
}

// warnings drains the recorded events.
func (f *storeFixture) warnings() []string {
	var events []string
	for {
		select {
		case e := <-f.recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// testManagedStore returns a managed store attached to testBarbicanName and
// provisioning against testInstanceName.
func testManagedStore() *barbicanv1alpha1.BarbicanSecretStore {
	store := testStore()
	store.Spec.OpenBao = &barbicanv1alpha1.OpenBaoStoreSpec{
		InstanceRef:  &barbicanv1alpha1.InstanceRefSpec{Name: testInstanceName},
		KVMountpoint: "barbican",
	}
	return store
}

// testBrownfieldStore returns a store pointing at a server provisioned outside
// this operator, reading its credentials from the named Secret.
func testBrownfieldStore() *barbicanv1alpha1.BarbicanSecretStore {
	store := testStore()
	store.Spec.OpenBao = &barbicanv1alpha1.OpenBaoStoreSpec{
		Server: &barbicanv1alpha1.OpenBaoServerSpec{
			URL:                  "https://bao.example.com:8200",
			CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: "brownfield-approle"},
		},
		KVMountpoint: "barbican",
	}
	return store
}

// testStore returns the mode-independent part of a store CR.
func testStore() *barbicanv1alpha1.BarbicanSecretStore {
	return &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testStoreName,
			Namespace:  testNamespace,
			UID:        types.UID("store-uid"),
			Generation: 1,
		},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: testBarbicanName},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
		},
	}
}

// testInstance returns the OpenBaoCluster a managed store provisions against.
func testInstance(available bool) *openbaov1alpha1.OpenBaoCluster {
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	return &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testInstanceName, Namespace: testNamespace},
		Status: openbaov1alpha1.OpenBaoClusterStatus{
			Conditions: []metav1.Condition{{
				Type:               string(openbaov1alpha1.ConditionAvailable),
				Status:             status,
				Reason:             "Reconciled",
				LastTransitionTime: metav1.NewTime(testClock),
			}},
		},
	}
}

// testCASecret returns the instance's fixed-name trust-bundle Secret.
func testCASecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testInstanceName + instanceCASecretSuffix, Namespace: testNamespace},
		Data:       map[string][]byte{barbicanv1alpha1.OpenBaoCAKey: []byte(testCAPEM)},
	}
}

// testMintedSecret returns a credentials Secret as the operator would have left
// it behind: both contract keys plus the mint stamp.
func testMintedSecret(mintedAt time.Time, ttlSeconds string, secretID string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testStoreName + approleSecretNameSuffix,
			Namespace: testNamespace,
			Annotations: map[string]string{
				secretIDMintedAtAnnotation:   mintedAt.Format(time.RFC3339),
				secretIDTTLSecondsAnnotation: ttlSeconds,
			},
		},
		Data: map[string][]byte{
			barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
			barbicanv1alpha1.OpenBaoSecretIDKey: []byte(secretID),
		},
	}
}

// testBrownfieldSecret returns the AppRole credentials Secret a brownfield store
// references, carrying the given data.
func testBrownfieldSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "brownfield-approle", Namespace: testNamespace},
		Data:       data,
	}
}

// testBarbican returns the parent Barbican CR a store attaches to. It is the
// shared fixture of this package: the store tests use it as the projection
// target, the parent-reconciler tests reconcile it, so it carries the Secret
// references and the release/image/cache fields the Barbican pipeline reads.
func testBarbican() *barbicanv1alpha1.Barbican {
	return &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testBarbicanName,
			Namespace:  testNamespace,
			UID:        types.UID("barbican-uid"),
			Generation: 1,
		},
		Spec: barbicanv1alpha1.BarbicanSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/barbican", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				Host: "db.example.com", Port: 3306, Database: "barbican",
				SecretRef: commonv1.SecretRefSpec{Name: "barbican-db"},
			},
			Cache:            commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			KeystoneEndpoint: "http://keystone.openstack.svc:5000",
			ServiceUser:      barbicanv1alpha1.ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "barbican-service-user"}},
		},
	}
}

// testProjectedDeployment returns the Barbican Deployment mounting the rendered
// config from the named Secret.
func testProjectedDeployment(configSecretName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: testBarbicanName, Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name:         configVolumeName,
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: configSecretName}},
					}},
				},
			},
		},
	}
}

// testConfigSecret returns the config Secret the Deployment mounts, carrying the
// rendered barbican.conf.
func testConfigSecret(name, rendered string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{barbicanConfDataKey: []byte(rendered)},
	}
}

// projectedConfigObjects returns the parent Barbican, its Deployment and the
// config object carrying this store's rendered section — the fixture set that
// makes ConfigProjected True.
func projectedConfigObjects() []client.Object {
	rendered := "[secretstore]\nstores_lookup_suffix = primary\n\n[secretstore:primary]\nsecret_store_plugin = store_crypto\n"
	return []client.Object{
		testBarbican(),
		testProjectedDeployment("barbican-config-abc123"),
		testConfigSecret("barbican-config-abc123", rendered),
	}
}

// condition returns one of the store's conditions, or nil.
func condition(store *barbicanv1alpha1.BarbicanSecretStore, conditionType string) *metav1.Condition {
	return commonconditions.GetCondition(store.Status.Conditions, conditionType)
}

// remintCount reads the re-mint counter for one store and trigger off the
// controller-runtime registry the operator records on. Tests compare a delta,
// because the registry is process-wide.
func remintCount(t *testing.T, store, trigger string) float64 {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(metrics.Register()).To(Succeed())

	families, err := ctrlmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	for _, fam := range families {
		if fam.GetName() != "barbican_operator_secretstore_remints_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			if matchesLabels(m, store, trigger) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// matchesLabels reports whether m carries the store/namespace/trigger label set.
func matchesLabels(m *dto.Metric, store, trigger string) bool {
	labels := map[string]string{}
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	return labels["store"] == store && labels["namespace"] == testNamespace && labels["trigger"] == trigger
}

// --- managed mode ----------------------------------------------------------

func TestSecretStoreReconcile_ManagedMintsCredentials(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	f := newStoreFixture(bao, append(projectedConfigObjects(), store, testInstance(true), testCASecret())...)

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(proactiveThreshold(time.Duration(testSecretIDTTL) * time.Second)))

	// The token is minted for the instance's provisioner ServiceAccount, in the
	// store's namespace, bound to the instance's own audience.
	g.Expect(f.minter.requests).To(HaveLen(1))
	g.Expect(f.minter.requests[0]).To(Equal(tokenRequest{
		namespace: testNamespace,
		name:      testInstanceName + instanceProvisionerSASuffix,
		audience:  testInstanceName,
	}))

	// The client addresses the instance by its in-cluster SAN and verifies
	// against the instance's own CA bundle.
	g.Expect(f.bao.configs).NotTo(BeEmpty())
	g.Expect(f.bao.configs[0].URL).To(Equal("https://openbao-instance.openstack.svc:8200"))
	g.Expect(f.bao.configs[0].CACertPEM).To(Equal([]byte(testCAPEM)))
	g.Expect(f.bao.configs[0].Timeout).To(Equal(openBaoRequestTimeout))

	// Both sessions — the provisioner one and the probe validating the fresh
	// credentials — are handed back.
	g.Expect(bao.callLog()).To(Equal([]string{
		"LoginKubernetes", "MountExists", "ReadRoleID", "GenerateSecretID",
		"LoginAppRole", "RevokeSelfToken", "RevokeSelfToken",
	}))

	secret := f.credentialsSecret(t)
	g.Expect(secret.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoRoleIDKey, []byte(testRoleID)))
	g.Expect(secret.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte(testSecretID)))
	g.Expect(secret.Annotations).To(HaveKeyWithValue(secretIDMintedAtAnnotation, testClock.Format(time.RFC3339)))
	g.Expect(secret.Annotations).To(HaveKeyWithValue(secretIDTTLSecondsAnnotation, "3600"))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal(testStoreName))
	g.Expect(secret.OwnerReferences[0].Controller).To(Equal(ptrTrue()))

	updated := f.store(t)
	g.Expect(condition(updated, conditionTypeProvisioningReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition(updated, conditionTypeProvisioningReady).Reason).To(Equal(conditionReasonProvisioned))
	g.Expect(condition(updated, conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition(updated, conditionTypeCredentialsReady).Reason).To(Equal(conditionReasonCredentialsAvailable))
	g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionTrue))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
}

// ptrTrue returns a pointer to true, for the owner-reference assertion.
func ptrTrue() *bool {
	t := true
	return &t
}

// The instance-side waiting states all report a distinct reason on
// ProvisioningReady, emit a Warning, and retry on the store cadence without
// failing the reconcile.
func TestSecretStoreReconcile_ManagedInstanceWaitingStates(t *testing.T) {
	tests := []struct {
		name    string
		objects func() []client.Object
		reason  string
	}{
		{
			name:    "instance absent",
			objects: func() []client.Object { return nil },
			reason:  conditionReasonInstanceNotFound,
		},
		{
			name:    "instance not available",
			objects: func() []client.Object { return []client.Object{testInstance(false), testCASecret()} },
			reason:  conditionReasonWaitingForInstance,
		},
		{
			name:    "trust bundle absent",
			objects: func() []client.Object { return []client.Object{testInstance(true)} },
			reason:  conditionReasonWaitingForInstanceTLS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testManagedStore()
			bao := newProvisionedFakeClient()
			f := newStoreFixture(bao, append(tt.objects(), store)...)

			result, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

			updated := f.store(t)
			provisioning := condition(updated, conditionTypeProvisioningReady)
			g.Expect(provisioning).NotTo(BeNil())
			g.Expect(provisioning.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(provisioning.Reason).To(Equal(tt.reason))
			g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionFalse))

			// Nothing is asked of a server the operator has not resolved yet.
			g.Expect(f.bao.configs).To(BeEmpty())
			g.Expect(bao.callLog()).To(BeEmpty())
			g.Expect(f.warnings()).To(ContainElement(ContainSubstring(tt.reason)))
		})
	}
}

// The TokenRequest grant is namespace-scoped by design — create on
// serviceaccounts/token cannot be narrowed to a single account — and
// rbac.secretStoreNamespaces defaults to empty, so a 403 from the subresource is
// the DEFAULT state of any namespace nobody opted in. It must be readable off
// the store: returned bare it would leave ProvisioningReady absent and put the
// only evidence of the missing Role in the operator log.
func TestSecretStoreReconcile_ManagedTokenRequestForbidden(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	f := newStoreFixture(bao, store, testInstance(true), testCASecret())
	f.reconciler.MintServiceAccountToken = func(_ context.Context, namespace, name, _ string) (string, error) {
		return "", apierrors.NewForbidden(
			schema.GroupResource{Resource: "serviceaccounts"}, name,
			fmt.Errorf("cannot create resource %q in API group \"\" in the namespace %q", "serviceaccounts/token", namespace))
	}

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred(), "a missing grant is a state to report, not a reconcile failure")
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	provisioning := condition(f.store(t), conditionTypeProvisioningReady)
	g.Expect(provisioning).NotTo(BeNil())
	g.Expect(provisioning.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioning.Reason).To(Equal(conditionReasonProvisioningDenied))
	g.Expect(provisioning.Message).To(ContainSubstring("rbac.secretStoreNamespaces"))
	g.Expect(provisioning.Message).To(ContainSubstring(testNamespace))

	// Nothing is asked of the server without a token to log in with.
	g.Expect(bao.callLog()).To(BeEmpty())
	g.Expect(f.warnings()).To(ContainElement(ContainSubstring(conditionReasonProvisioningDenied)))
}

func TestSecretStoreReconcile_ManagedLoginConnectionFailure(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	bao.loginKubernetesErr = errors.New("dial tcp 10.96.0.7:8200: connect: connection refused")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret())

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	provisioning := condition(f.store(t), conditionTypeProvisioningReady)
	g.Expect(provisioning.Reason).To(Equal(conditionReasonOpenBaoUnreachable))
	g.Expect(provisioning.Message).To(ContainSubstring("logging in to OpenBao:"))
	g.Expect(provisioning.Message).To(ContainSubstring("connection refused"))

	// The session that never came to be is still handed back, which is a no-op.
	g.Expect(bao.callLog()).To(Equal([]string{"LoginKubernetes", "RevokeSelfToken"}))
	g.Expect(f.warnings()).To(ContainElement(ContainSubstring(conditionReasonOpenBaoUnreachable)))
}

func TestSecretStoreReconcile_ManagedLoginPermissionDenied(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	bao.loginKubernetesErr = responseError(http.StatusForbidden, "permission denied")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret())

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	provisioning := condition(f.store(t), conditionTypeProvisioningReady)
	g.Expect(provisioning.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioning.Reason).To(Equal(conditionReasonProvisioningDenied))
	g.Expect(f.warnings()).To(ContainElement(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonProvisioningDenied),
	)))
}

// A mount or an AppRole the self-init contract should have created but did not
// is reported by naming the requests that create them.
func TestSecretStoreReconcile_ManagedInstanceNotProvisioned(t *testing.T) {
	tests := []struct {
		name string
		bao  func() *fakeOpenBaoClient
	}{
		{
			name: "kv mount absent",
			bao: func() *fakeOpenBaoClient {
				c := newProvisionedFakeClient()
				c.mountExists = false
				return c
			},
		},
		{
			name: "approle absent",
			bao: func() *fakeOpenBaoClient {
				c := newProvisionedFakeClient()
				c.readRoleIDErr = responseError(http.StatusNotFound, "no handler for this path")
				return c
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testManagedStore()
			bao := tt.bao()
			f := newStoreFixture(bao, store, testInstance(true), testCASecret())

			result, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

			provisioning := condition(f.store(t), conditionTypeProvisioningReady)
			g.Expect(provisioning.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(provisioning.Reason).To(Equal(conditionReasonInstanceNotProvisioned))
			g.Expect(provisioning.Message).To(ContainSubstring("barbican_kv"))
			g.Expect(provisioning.Message).To(ContainSubstring("approle_auth"))
			g.Expect(provisioning.Message).To(ContainSubstring("barbican_approle_role"))

			// Nothing was minted against a half-provisioned instance.
			g.Expect(bao.callCount("GenerateSecretID")).To(Equal(0))
			g.Expect(bao.callLog()).To(ContainElement("RevokeSelfToken"))
		})
	}
}

func TestSecretStoreReconcile_ManagedProactiveRemint(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	bao.secretIDs = []string{"refreshed-secret-id"}
	// Minted 45 minutes ago with a one-hour TTL: past the 40-minute threshold.
	existing := testMintedSecret(testClock.Add(-45*time.Minute), "3600", "stale-secret-id")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret(), existing)

	before := remintCount(t, testStoreName, remintTriggerProactive)
	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	// The projection is not there in this fixture, so the shorter retry wins.
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	g.Expect(bao.callCount("GenerateSecretID")).To(Equal(1))
	// The credential is refreshed before it is rejected, so no probe precedes
	// the re-mint.
	g.Expect(bao.callLog()).To(Equal([]string{
		"LoginKubernetes", "MountExists", "ReadRoleID", "GenerateSecretID",
		"LoginAppRole", "RevokeSelfToken", "RevokeSelfToken",
	}))

	secret := f.credentialsSecret(t)
	g.Expect(secret.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("refreshed-secret-id")))
	g.Expect(secret.Annotations).To(HaveKeyWithValue(secretIDMintedAtAnnotation, testClock.Format(time.RFC3339)))
	g.Expect(remintCount(t, testStoreName, remintTriggerProactive)).To(Equal(before + 1))

	g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// A TTL of 0 means the secret ID does not expire, so the timer is disabled
// however old the stamp is: the pass probes the credential and leaves it alone.
// The store still comes back on the revalidation cadence, because the probe is
// the only thing that would notice a revocation.
func TestSecretStoreReconcile_ManagedZeroTTLNeverRemintsProactively(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	existing := testMintedSecret(testClock.Add(-10000*time.Hour), "0", "eternal-secret-id")
	f := newStoreFixture(bao, append(projectedConfigObjects(),
		store, testInstance(true), testCASecret(), existing)...)

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRevalidate))

	g.Expect(bao.callCount("GenerateSecretID")).To(Equal(0))
	g.Expect(bao.callLog()).To(Equal([]string{
		"LoginKubernetes", "MountExists", "ReadRoleID",
		"LoginAppRole", "RevokeSelfToken", "RevokeSelfToken",
	}))

	secret := f.credentialsSecret(t)
	g.Expect(secret.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("eternal-secret-id")))
	g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// A credential the server rejects is replaced on the spot. Both rejections
// count: OpenBao answers an expired or unknown secret ID with 400 and a policy
// gap with 403, and neither may read as an unreachable server.
func TestSecretStoreReconcile_ManagedReactiveRemint(t *testing.T) {
	tests := []struct {
		name     string
		probeErr error
	}{
		{
			name:     "secret id expired",
			probeErr: responseError(http.StatusBadRequest, "invalid role or secret ID"),
		},
		{
			name:     "secret id revoked by policy",
			probeErr: responseError(http.StatusForbidden, "permission denied"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testManagedStore()
			bao := newProvisionedFakeClient()
			bao.secretIDs = []string{"replacement-secret-id"}
			// The probe is rejected; the login validating the replacement succeeds.
			bao.loginAppRoleErrs = []error{tt.probeErr}
			// Older than reactiveRemintCooldown so the reactive path is open, but
			// well inside the proactive threshold (2/3 of 3600s) so the timer has
			// nothing to say and the re-mint is attributable to the rejected probe.
			existing := testMintedSecret(testClock.Add(-2*reactiveRemintCooldown), "3600", "revoked-secret-id")
			f := newStoreFixture(bao, store, testInstance(true), testCASecret(), existing)

			before := remintCount(t, testStoreName, remintTriggerReactive)
			_, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())

			// Exactly one re-mint: the rejected probe, then the replacement and
			// its validating login.
			g.Expect(bao.callCount("GenerateSecretID")).To(Equal(1))
			g.Expect(bao.callLog()).To(Equal([]string{
				"LoginKubernetes", "MountExists", "ReadRoleID",
				"LoginAppRole", "RevokeSelfToken",
				"GenerateSecretID", "LoginAppRole", "RevokeSelfToken",
				"RevokeSelfToken",
			}))

			secret := f.credentialsSecret(t)
			g.Expect(secret.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("replacement-secret-id")))
			g.Expect(remintCount(t, testStoreName, remintTriggerReactive)).To(Equal(before + 1))
			g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
		})
	}
}

// A rejection the server keeps repeating must not turn into an unbounded mint
// loop. Every failure state of this controller retries at RequeueSecretStoreRetry
// without reaching the workqueue as an error, so credentials minted inside the
// cooldown are reported as rejected instead of replaced — otherwise a 400 the
// mint cannot cure (a CIDR-bound AppRole, bind_secret_id disabled, a proxy in
// front of the server) would mint one new secret-ID accessor per retry, forever.
func TestSecretStoreReconcile_ManagedReactiveRemintIsRateLimited(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	// Every login is rejected, which is what a re-mint would run into again.
	bao.loginAppRoleErrs = []error{
		responseError(http.StatusBadRequest, "invalid role or secret ID"),
		responseError(http.StatusBadRequest, "invalid role or secret ID"),
	}
	// Minted just now: inside the cooldown, outside the proactive threshold.
	fresh := testMintedSecret(testClock, "3600", "rejected-secret-id")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret(), fresh)

	before := remintCount(t, testStoreName, remintTriggerReactive)
	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(bao.callCount("GenerateSecretID")).To(Equal(0), "no secret ID is minted inside the cooldown")
	g.Expect(remintCount(t, testStoreName, remintTriggerReactive)).To(Equal(before))
	g.Expect(f.credentialsSecret(t).Data).To(
		HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("rejected-secret-id")))

	cond := condition(f.store(t), conditionTypeCredentialsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonInvalidCredentials))
}

// A Secret without a readable mint stamp carries no re-mint decision, so it is
// refilled like an initial mint — which is not counted as a re-mint.
func TestSecretStoreReconcile_ManagedUnstampedSecretRemintsUncounted(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	unstamped := testMintedSecret(testClock, "not-a-number", "stale-secret-id")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret(), unstamped)

	beforeProactive := remintCount(t, testStoreName, remintTriggerProactive)
	beforeReactive := remintCount(t, testStoreName, remintTriggerReactive)
	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(bao.callCount("GenerateSecretID")).To(Equal(1))
	g.Expect(f.credentialsSecret(t).Annotations).To(HaveKeyWithValue(secretIDTTLSecondsAnnotation, "3600"))
	g.Expect(remintCount(t, testStoreName, remintTriggerProactive)).To(Equal(beforeProactive))
	g.Expect(remintCount(t, testStoreName, remintTriggerReactive)).To(Equal(beforeReactive))
}

// A failure after the login still hands the provisioner session back: the
// revocation is deferred, not appended to the success path.
func TestSecretStoreReconcile_ManagedRevokesSessionWhenMintFails(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	bao.generateErr = errors.New("i/o timeout")
	f := newStoreFixture(bao, store, testInstance(true), testCASecret())

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	g.Expect(bao.callLog()).To(Equal([]string{
		"LoginKubernetes", "MountExists", "ReadRoleID", "GenerateSecretID", "RevokeSelfToken",
	}))

	updated := f.store(t)
	credentials := condition(updated, conditionTypeCredentialsReady)
	g.Expect(credentials.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(credentials.Reason).To(Equal(conditionReasonOpenBaoUnreachable))
	// The instance itself was fine, so its condition stays True.
	g.Expect(condition(updated, conditionTypeProvisioningReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionFalse))
}

// A deleting store sheds its metric series and writes no status.
func TestSecretStoreReconcile_DeletingStoreDropsMetrics(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	store.Finalizers = []string{"barbican.openstack.c5c3.io/test"}
	store.DeletionTimestamp = &metav1.Time{Time: testClock}
	bao := newProvisionedFakeClient()
	f := newStoreFixture(bao, store, testInstance(true), testCASecret())

	metrics.RecordSecretStoreRemint(testStoreName, testNamespace, remintTriggerProactive)
	g.Expect(remintCount(t, testStoreName, remintTriggerProactive)).To(BeNumerically(">", 0))

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	g.Expect(remintCount(t, testStoreName, remintTriggerProactive)).To(Equal(float64(0)))
	g.Expect(bao.callLog()).To(BeEmpty())
	g.Expect(f.store(t).Status.Conditions).To(BeEmpty())
}

// --- brownfield mode -------------------------------------------------------

// Every brownfield credential gap is reported without touching the server: an
// operator that cannot authenticate must not produce connection attempts.
func TestSecretStoreReconcile_BrownfieldWaitsForCredentials(t *testing.T) {
	tests := []struct {
		name    string
		objects func() []client.Object
	}{
		{
			name:    "credentials Secret absent",
			objects: func() []client.Object { return nil },
		},
		{
			name: "secret-id key absent",
			objects: func() []client.Object {
				return []client.Object{testBrownfieldSecret(map[string][]byte{
					barbicanv1alpha1.OpenBaoRoleIDKey: []byte(testRoleID),
				})}
			},
		},
		{
			name: "role-id empty",
			objects: func() []client.Object {
				return []client.Object{testBrownfieldSecret(map[string][]byte{
					barbicanv1alpha1.OpenBaoRoleIDKey:   {},
					barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
				})}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testBrownfieldStore()
			bao := newProvisionedFakeClient()
			f := newStoreFixture(bao, append(tt.objects(), store)...)

			result, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

			updated := f.store(t)
			credentials := condition(updated, conditionTypeCredentialsReady)
			g.Expect(credentials.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(credentials.Reason).To(Equal(conditionReasonWaitingForCredentials))
			g.Expect(condition(updated, conditionTypeProvisioningReady)).To(BeNil())

			g.Expect(f.bao.configs).To(BeEmpty())
			g.Expect(bao.callLog()).To(BeEmpty())
			g.Expect(f.warnings()).To(ContainElement(ContainSubstring(conditionReasonWaitingForCredentials)))
		})
	}
}

// A referenced CA bundle that is not there yet is the same waiting state: the
// operator would have to skip verification to proceed, which it never does.
func TestSecretStoreReconcile_BrownfieldWaitsForCABundle(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	store.Spec.OpenBao.Server.CABundleSecretRef = &barbicanv1alpha1.SecretNameRefSpec{Name: "brownfield-ca"}
	bao := newProvisionedFakeClient()
	creds := testBrownfieldSecret(map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	})
	f := newStoreFixture(bao, store, creds)

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	credentials := condition(f.store(t), conditionTypeCredentialsReady)
	g.Expect(credentials.Reason).To(Equal(conditionReasonWaitingForCredentials))
	g.Expect(credentials.Message).To(ContainSubstring("brownfield-ca"))
	g.Expect(bao.callLog()).To(BeEmpty())
}

// A login the server answers is a statement about the credentials — 400 for an
// expired or unknown secret ID, 403 for a policy gap — while a login that never
// arrives says nothing about them.
func TestSecretStoreReconcile_BrownfieldLoginFailures(t *testing.T) {
	tests := []struct {
		name     string
		loginErr error
		reason   string
	}{
		{
			name:     "secret id expired",
			loginErr: responseError(http.StatusBadRequest, "invalid role or secret ID"),
			reason:   conditionReasonInvalidCredentials,
		},
		{
			name:     "permission denied",
			loginErr: responseError(http.StatusForbidden, "permission denied"),
			reason:   conditionReasonInvalidCredentials,
		},
		{
			name:     "server out of reach",
			loginErr: errors.New("dial tcp 203.0.113.9:8200: i/o timeout"),
			reason:   conditionReasonOpenBaoUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testBrownfieldStore()
			bao := newProvisionedFakeClient()
			bao.loginAppRoleErrs = []error{tt.loginErr}
			creds := testBrownfieldSecret(map[string][]byte{
				barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
				barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
			})
			f := newStoreFixture(bao, store, creds)

			result, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

			updated := f.store(t)
			credentials := condition(updated, conditionTypeCredentialsReady)
			g.Expect(credentials.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(credentials.Reason).To(Equal(tt.reason))
			g.Expect(condition(updated, conditionTypeProvisioningReady)).To(BeNil())

			// The rejected session is still handed back, and nothing was written.
			g.Expect(bao.callLog()).To(Equal([]string{"LoginAppRole", "RevokeSelfToken"}))
			g.Expect(f.warnings()).To(ContainElement(And(
				ContainSubstring(corev1.EventTypeWarning),
				ContainSubstring(tt.reason),
			)))
		})
	}
}

func TestSecretStoreReconcile_BrownfieldInsufficientCapabilities(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	bao := newProvisionedFakeClient()
	bao.capabilities = []string{"create", "read", "update", "list"}
	creds := testBrownfieldSecret(map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	})
	f := newStoreFixture(bao, store, creds)

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))

	updated := f.store(t)
	credentials := condition(updated, conditionTypeCredentialsReady)
	g.Expect(credentials.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(credentials.Reason).To(Equal(conditionReasonInsufficientCapabilities))
	g.Expect(credentials.Message).To(ContainSubstring("delete"))
	g.Expect(credentials.Message).To(ContainSubstring("barbican/data/probe"))
	g.Expect(condition(updated, conditionTypeProvisioningReady)).To(BeNil())
	g.Expect(f.warnings()).To(ContainElement(And(
		ContainSubstring(corev1.EventTypeWarning),
		ContainSubstring(conditionReasonInsufficientCapabilities),
	)))
}

func TestSecretStoreReconcile_BrownfieldValidatesWithoutWriting(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	bao := newProvisionedFakeClient()
	bao.capabilities = requiredStoreCapabilities
	creds := testBrownfieldSecret(map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	})
	f := newStoreFixture(bao, append(projectedConfigObjects(), store, creds)...)

	result, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRevalidate))

	// Read-only by contract: a login, a capability read, a revocation.
	g.Expect(bao.callLog()).To(Equal([]string{"LoginAppRole", "CapabilitiesSelf", "RevokeSelfToken"}))
	g.Expect(f.bao.configs).To(HaveLen(1))
	g.Expect(f.bao.configs[0].URL).To(Equal("https://bao.example.com:8200"))
	g.Expect(f.bao.configs[0].Namespace).To(BeEmpty(), "an unset spec.openBao.namespace is the root namespace")

	updated := f.store(t)
	g.Expect(condition(updated, conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition(updated, conditionTypeCredentialsReady).Reason).To(Equal(conditionReasonCredentialsAvailable))
	g.Expect(condition(updated, conditionTypeProvisioningReady)).To(BeNil())
	// Two sub-conditions carry the aggregate in this mode, and both are True.
	g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionTrue))

	// No credentials Secret is minted for a brownfield store.
	var minted corev1.Secret
	err = f.reconciler.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}, &minted)
	g.Expect(err).To(HaveOccurred())
}

// spec.openBao.namespace is rendered into the pods' barbican.conf, so the
// operator's own login and capability read have to run against the same
// namespace: the AppRole lives there, and a root-namespace login is rejected —
// which would surface as InvalidCredentials against credentials that are
// correct, pointing the diagnosis at the wrong thing.
func TestSecretStoreReconcile_BrownfieldScopesTheClientToTheStoreNamespace(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	store.Spec.OpenBao.Namespace = "tenant-a"
	bao := newProvisionedFakeClient()
	bao.capabilities = requiredStoreCapabilities
	creds := testBrownfieldSecret(map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	})
	f := newStoreFixture(bao, append(projectedConfigObjects(), store, creds)...)

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(f.bao.configs).To(HaveLen(1))
	g.Expect(f.bao.configs[0].Namespace).To(Equal("tenant-a"))
}

// --- config projection -----------------------------------------------------

func TestSecretStoreReconcile_ConfigProjection(t *testing.T) {
	credsData := map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	}

	tests := []struct {
		name      string
		objects   func() []client.Object
		projected bool
	}{
		{
			name:      "section rendered on its own line",
			objects:   projectedConfigObjects,
			projected: true,
		},
		{
			name: "store name only a substring of another section",
			objects: func() []client.Object {
				return []client.Object{
					testBarbican(),
					testProjectedDeployment("barbican-config-abc123"),
					testConfigSecret("barbican-config-abc123",
						"[secretstore:primary-two]\nsecret_store_plugin = store_crypto\n"),
				}
			},
			projected: false,
		},
		{
			name: "parent Barbican absent",
			objects: func() []client.Object {
				return nil
			},
			projected: false,
		},
		{
			name: "Deployment without a config volume",
			objects: func() []client.Object {
				return []client.Object{
					testBarbican(),
					&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: testBarbicanName, Namespace: testNamespace}},
				}
			},
			projected: false,
		},
		{
			// The rendered config carries the vault plugin's role ID, so the
			// projection is a Secret. A config volume backed by anything else is
			// not this operator's projection.
			name: "config volume backed by a ConfigMap",
			objects: func() []client.Object {
				deploy := testProjectedDeployment("")
				deploy.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "barbican-config-abc123"},
					},
				}
				return []client.Object{
					testBarbican(),
					deploy,
					&corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "barbican-config-abc123", Namespace: testNamespace},
						Data:       map[string]string{barbicanConfDataKey: "[secretstore:primary]\n"},
					},
				}
			},
			projected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)

			store := testBrownfieldStore()
			bao := newProvisionedFakeClient()
			bao.capabilities = requiredStoreCapabilities
			objs := append(tt.objects(), store, testBrownfieldSecret(credsData))
			f := newStoreFixture(bao, objs...)

			result, err := f.reconcile(t, store)
			g.Expect(err).NotTo(HaveOccurred())

			updated := f.store(t)
			projected := condition(updated, conditionTypeConfigProjected)
			g.Expect(projected).NotTo(BeNil())
			if tt.projected {
				g.Expect(projected.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(projected.Reason).To(Equal(conditionReasonConfigProjected))
				g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionTrue))
				g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRevalidate))
				return
			}
			g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
			// The credentials are fine, so only the projection holds Ready back.
			g.Expect(condition(updated, conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition(updated, "Ready").Status).To(Equal(metav1.ConditionFalse))
			// The waiting projection shortens the revalidation cadence.
			g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))
		})
	}
}

// --- parent target cluster -------------------------------------------------

// fakeTargetCluster exposes a fake client as a registered cluster. Embedding the
// interface leaves every other method nil, which panics if the resolver ever
// reaches for one.
type fakeTargetCluster struct {
	cluster.Cluster
	c      client.Client
	reader client.Reader
	config *rest.Config
}

func (f fakeTargetCluster) GetClient() client.Client { return f.c }

// GetConfig answers with the cluster's REST config, which a remote store reaches
// for to build the tunnel its OpenBao dials go through. It is never connected to
// in a unit test — the OpenBao client is faked at the seam above it — so a
// placeholder host is enough to build a dialer from.
func (f fakeTargetCluster) GetConfig() *rest.Config { return f.config }

// GetAPIReader answers with the cluster's uncached reader. A test that sets none
// gets the client itself, which for a fake is read-your-writes anyway.
func (f fakeTargetCluster) GetAPIReader() client.Reader {
	if f.reader != nil {
		return f.reader
	}
	return f.c
}

// childrenResolver registers exactly one cluster under every name and records
// the names it was asked for, so a test can prove which ref the store resolved —
// or that it resolved none at all. reader is the cluster's uncached view, set
// only by the test that has the cache trail it. byName overrides children per
// name, for the tests that need two clusters to hold different objects.
type childrenResolver struct {
	children client.Client
	reader   client.Reader
	byName   map[string]client.Client
	names    []mcruntime.ClusterName
}

func (r *childrenResolver) GetCluster(_ context.Context, name mcruntime.ClusterName) (cluster.Cluster, error) {
	r.names = append(r.names, name)
	if c, named := r.byName[string(name)]; named {
		return fakeTargetCluster{c: c, config: targetClusterConfig()}, nil
	}
	return fakeTargetCluster{c: r.children, reader: r.reader, config: targetClusterConfig()}, nil
}

// targetClusterConfig is the REST config every registered target cluster is
// handed out with. Nothing dials it: it only has to be complete enough for the
// port-forward dialer a remote store builds its OpenBao route from.
func targetClusterConfig() *rest.Config {
	return &rest.Config{Host: "https://api.target.example:6443"}
}

// targetedBarbican returns the parent Barbican placing its workload on the named
// target cluster.
func targetedBarbican(target string) *barbicanv1alpha1.Barbican {
	barbican := testBarbican()
	barbican.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: target}
	return barbican
}

// childrenFake builds the target cluster's client: core/apps plus the openbao
// API a managed store reads its instance from, and deliberately WITHOUT the
// barbican group — a store read or a status write misrouted to it then fails
// with "no kind is registered" instead of passing on an empty result.
func childrenFake(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the children scheme: %v", err)
	}
	if err := openbaov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building the children scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestSecretStoreReconcile_MintsCredentialsOnParentTargetCluster pins the split
// a store inherits from its parent: the CR and its status stay on the management
// cluster, while the OpenBao instance it authenticates against and the AppRole
// credentials Secret it mints live on the cluster the parent's
// spec.targetClusterRef names — the same one the API pods read that Secret from.
func TestSecretStoreReconcile_MintsCredentialsOnParentTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	bao := newProvisionedFakeClient()
	// Management carries the store and its parent; the instance and its trust
	// bundle exist ONLY on the target cluster, so a misrouted read reports
	// InstanceNotFound instead of provisioning.
	f := newStoreFixture(bao, store, targetedBarbican("edge-1"))
	children := childrenFake(t, testInstance(true), testCASecret())
	resolver := &childrenResolver{children: children}
	f.reconciler.Resolver = resolver

	_, err := f.reconcileRemote(t, store)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolver.names).NotTo(BeEmpty())
	g.Expect(resolver.names).To(HaveEach(mcruntime.ClusterName("edge-1")),
		"the store resolves the target its parent names, having none of its own")

	credsKey := client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}
	var onChildren corev1.Secret
	g.Expect(children.Get(ctx, credsKey, &onChildren)).To(Succeed())
	g.Expect(onChildren.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte(testSecretID)))
	g.Expect(f.reconciler.Get(ctx, credsKey, &corev1.Secret{})).NotTo(Succeed(),
		"the credentials must not be minted beside the CR, where nothing consumes them")

	// The instance side resolved from the target cluster too: its CA bundle is
	// what the OpenBao client was configured with.
	g.Expect(f.bao.configs).NotTo(BeEmpty())
	g.Expect(f.bao.configs[0].CACertPEM).To(Equal([]byte(testCAPEM)))

	// The status landed on the management cluster, the only one that carries the
	// store CR at all.
	updated := f.store(t)
	g.Expect(condition(updated, conditionTypeProvisioningReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition(updated, conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// TestSecretStoreReconcile_RemoteManagedStoreTunnelsTheOpenBaoDial pins how a
// placed store reaches the OpenBao it provisions against. The instance runs on
// the target cluster, so its Service DNS name does not resolve from the
// operator — and the URL must not be rewritten to compensate: it is the SAN the
// instance's server certificate carries, and the operator verifies that
// certificate against the instance's own trust bundle. Only the dial moves.
//
// The seeded credential is inside its TTL, so the pass also runs the AppRole
// login probe, whose client copies the managed config: both clients have to come
// out tunnelled, or the probe would report an unreachable server on every pass
// of a perfectly healthy placed store.
func TestSecretStoreReconcile_RemoteManagedStoreTunnelsTheOpenBaoDial(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: "edge-1"}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}

	live := claimedByStore(t, store, testMintedSecret(testClock, "3600", testSecretID))
	live.UID = types.UID("live-credentials-uid")

	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	f.reconciler.Resolver = &childrenResolver{children: childrenFake(t, testInstance(true), testCASecret(), live)}

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(f.bao.configs).To(HaveLen(2),
		"the provisioner and the credential probe each build a client")
	for i, cfg := range f.bao.configs {
		g.Expect(cfg.DialContext).NotTo(BeNil(), "client %d must dial through the target cluster", i)
		g.Expect(cfg.URL).To(Equal(fmt.Sprintf("https://%s.%s.svc:8200", testInstanceName, testNamespace)),
			"client %d keeps the Service URL, which is the certificate's SAN", i)
		g.Expect(cfg.CACertPEM).To(Equal([]byte(testCAPEM)),
			"client %d still verifies against the instance's own trust bundle", i)
	}
	g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// TestSecretStoreReconcile_RemoteBrownfieldStoreTunnelsTheOpenBaoDial covers the
// validating half. A brownfield server may or may not be a Service of the target
// cluster — this fixture's is not — so the dial is handed over either way and
// the dialer decides: a cluster-local name is tunnelled, anything else falls
// through to an ordinary dial.
func TestSecretStoreReconcile_RemoteBrownfieldStoreTunnelsTheOpenBaoDial(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testBrownfieldStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: "edge-1"}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}

	bao := newProvisionedFakeClient()
	bao.capabilities = requiredStoreCapabilities
	f := newStoreFixture(bao, store, targetedBarbican("edge-1"))
	credentials := testBrownfieldSecret(map[string][]byte{
		barbicanv1alpha1.OpenBaoRoleIDKey:   []byte(testRoleID),
		barbicanv1alpha1.OpenBaoSecretIDKey: []byte(testSecretID),
	})
	f.reconciler.Resolver = &childrenResolver{children: childrenFake(t, credentials)}

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(f.bao.configs).To(HaveLen(1))
	g.Expect(f.bao.configs[0].DialContext).NotTo(BeNil())
	g.Expect(f.bao.configs[0].URL).To(Equal("https://bao.example.com:8200"),
		"the CR's server URL is used as written; the dialer, not the URL, decides the route")
	g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// TestSecretStoreReconcile_LocalStoreDialsDirectly is the other half of the
// contract: a store whose parent keeps its workload local resolves the instance
// over ordinary Service DNS, so nothing is redirected and the client is built
// exactly as it was before there were target clusters.
func TestSecretStoreReconcile_LocalStoreDialsDirectly(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	f := newStoreFixture(newProvisionedFakeClient(), store, testInstance(true), testCASecret())
	f.reconciler.Resolver = &childrenResolver{children: childrenFake(t)}

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(f.bao.configs).NotTo(BeEmpty())
	for i, cfg := range f.bao.configs {
		g.Expect(cfg.DialContext).To(BeNil(), "client %d must dial the Service directly", i)
	}
}

// TestSecretStoreReconcile_UnbuildableTunnelGatesTheStore covers a target
// cluster that resolves but carries no REST config, which is what a cluster
// mid-registration looks like. The tunnel cannot be built, and the store has to
// report that on the condition it gates on rather than dial a name that does not
// resolve here and time out on it.
func TestSecretStoreReconcile_UnbuildableTunnelGatesTheStore(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: "edge-1"}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}

	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	f.reconciler.Resolver = mctestutil.ResolverFor(mctestutil.TargetCluster{
		Client: childrenFake(t, testInstance(true), testCASecret()),
	})

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred(), "an unusable target cluster is a wait, not a reconcile failure")
	g.Expect(result.RequeueAfter).To(Equal(RequeueSecretStoreRetry))
	g.Expect(f.bao.configs).To(BeEmpty(), "no client is built against a route that cannot be established")

	cond := condition(f.store(t), conditionTypeProvisioningReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOpenBaoUnreachable))
	g.Expect(cond.Message).To(ContainSubstring("the target cluster has no REST config"))
}

// TestSecretStoreReconcile_TargetClusterUnavailableGatesCredentials pins the
// failure surface of a parent naming a cluster nothing registered: the store
// reports it on its first gate condition and waits, rather than minting a
// credential into the cluster the workload does not run on.
func TestSecretStoreReconcile_TargetClusterUnavailableGatesCredentials(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	// The instance and its trust bundle ARE seeded here, so only the gate can
	// explain an absent credentials Secret.
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("nowhere"),
		testInstance(true), testCASecret())
	f.reconciler.Resolver = unresolvableResolver{}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := f.store(t)
	cond := condition(updated, conditionTypeCredentialsReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	var creds corev1.Secret
	err = f.reconciler.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}, &creds)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing may be written before the target resolves")
	g.Expect(f.bao.configs).To(BeEmpty(), "the server is not contacted either")
}

// TestSecretStoreReconcile_MissingParentHoldsCredentials covers the dangling
// spec.barbicanRef, which is also the ordinary case of a GitOps apply landing
// the store before its parent: there is no parent to read a target off, so
// nothing says which cluster this store's credentials belong on. The store has
// to wait for one. Falling back to the management cluster would MINT there — a
// live AppRole secret ID against the wrong OpenBao, left behind unreferenced as
// soon as the parent appears and the ref resolves elsewhere.
func TestSecretStoreReconcile_MissingParentHoldsCredentials(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), store, testInstance(true), testCASecret())
	resolver := &childrenResolver{children: childrenFake(t)}
	f.reconciler.Resolver = resolver

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred(), "an absent parent is a wait, not a reconcile failure")
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(resolver.names).To(BeEmpty(), "a store whose parent is gone costs no cluster lookup")

	updated := f.store(t)
	cond := condition(updated, conditionTypeCredentialsReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the wait")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForParent))
	g.Expect(cond.Message).To(ContainSubstring(testBarbicanName))

	var creds corev1.Secret
	err = f.reconciler.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}, &creds)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"no credential may be minted while the target cluster is unknown")
	g.Expect(f.bao.configs).To(BeEmpty(), "the server is not contacted either")
}

// --- remote teardown -------------------------------------------------------

// terminatingRemoteStore returns a managed store that minted its credentials
// onto childrenCluster and is now being deleted. It carries the remote-children
// finalizer plus a foreign one, so the CR survives the release and a test can
// read back which finalizer the pass dropped. An empty childrenCluster leaves
// the annotation off, which is the crash window between the two teardown marks.
func terminatingRemoteStore(childrenCluster string, deletedAt time.Time) *barbicanv1alpha1.BarbicanSecretStore {
	store := testManagedStore()
	if childrenCluster != "" {
		store.Annotations = map[string]string{childrenClusterAnnotation: childrenCluster}
	}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer, "foreign.example.com/keep-alive"}
	timestamp := metav1.NewTime(deletedAt)
	store.DeletionTimestamp = &timestamp
	return store
}

// claimedByStore stamps the ownership labels the remote claim writes, so a
// Secret on the target cluster is recognizable as this store's child — the mark
// that stands in for the owner reference no cross-cluster owner can carry.
func claimedByStore(t *testing.T, store *barbicanv1alpha1.BarbicanSecretStore, secret *corev1.Secret) *corev1.Secret {
	t.Helper()
	labels, err := commonmulticluster.OwnerLabels(testScheme(), store)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	secret.Labels = labels
	return secret
}

// credentialsKey names the AppRole credentials Secret of the fixture store.
var credentialsKey = client.ObjectKey{Namespace: testNamespace, Name: testStoreName + approleSecretNameSuffix}

// TestSecretStoreReconcile_RemoteStoreStampsTheAnnotationBeforeTheFinalizer
// pins the ordering the teardown depends on. The finalizer commits this
// controller to deleting a Secret on another cluster; the annotation is the only
// record of which cluster that is once the parent Barbican is gone. Writing them
// in this order means a crash between the two writes leaves an annotation
// without a finalizer, which costs nothing, and never a finalizer whose handler
// cannot name its cluster.
func TestSecretStoreReconcile_RemoteStoreStampsTheAnnotationBeforeTheFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	children := childrenFake(t, testInstance(true), testCASecret())
	f.reconciler.Resolver = &childrenResolver{children: children}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}),
		"the mark is read back from etcd on the next pass, not trusted in memory")
	marked := f.store(t)
	g.Expect(marked.Annotations).To(HaveKeyWithValue(childrenClusterAnnotation, "edge-1"))
	g.Expect(marked.Finalizers).To(BeEmpty(), "the annotation is written first, on a pass of its own")
	g.Expect(children.Get(ctx, credentialsKey, &corev1.Secret{})).NotTo(Succeed(),
		"nothing is minted before both marks are on the store")

	result, err = f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	marked = f.store(t)
	g.Expect(marked.Finalizers).To(ConsistOf(commonmulticluster.RemoteChildrenFinalizer))
	g.Expect(marked.Annotations).To(HaveKeyWithValue(childrenClusterAnnotation, "edge-1"),
		"a finalizer never exists without the annotation naming the cluster it cleans up on")
	g.Expect(children.Get(ctx, credentialsKey, &corev1.Secret{})).NotTo(Succeed())

	_, err = f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(children.Get(ctx, credentialsKey, &corev1.Secret{})).To(Succeed(),
		"the credential lands only once the teardown can find it again")
}

// TestSecretStoreReconcile_RemoteStoreCorrectsAPlantedAnnotation covers the
// annotation as what it is: an ordinary field of a user-created CR. Anyone who
// may update the store can put a value under that key before the operator's
// first pass, and a stamp guarded on presence alone would keep it. The teardown
// would then resolve a cluster the credentials were never minted onto — or, for
// the empty value, none at all — abandon the live AppRole secret ID on the real
// target, and release the finalizer reporting a clean deletion.
func TestSecretStoreReconcile_RemoteStoreCorrectsAPlantedAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: ""}
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	f.reconciler.Resolver = &childrenResolver{children: childrenFake(t, testInstance(true), testCASecret())}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(Equal(ctrl.Result{RequeueAfter: commonreconcile.RequeueNextPass}))
	g.Expect(f.store(t).Annotations).To(HaveKeyWithValue(childrenClusterAnnotation, "edge-1"),
		"the resolved cluster is authoritative, not whatever the annotation already said")
}

// TestSecretStoreReconcile_RemoteCredentialsAreClaimedByLabel pins how the
// Secret is owned on a cluster the store CR does not live on. An owner reference
// there names a UID the target's garbage collector cannot resolve, so the claim
// is three labels instead, and they are what the teardown selects on.
func TestSecretStoreReconcile_RemoteCredentialsAreClaimedByLabel(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	children := childrenFake(t, testInstance(true), testCASecret())
	f.reconciler.Resolver = &childrenResolver{children: children}

	_, err := f.reconcileRemote(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	var creds corev1.Secret
	g.Expect(children.Get(ctx, credentialsKey, &creds)).To(Succeed())
	g.Expect(creds.OwnerReferences).To(BeEmpty())
	g.Expect(creds.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerKindLabel, "BarbicanSecretStore"))
	g.Expect(creds.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerNameLabel, testStoreName))
	g.Expect(creds.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerNamespaceLabel, testNamespace))
}

// TestSecretStoreReconcile_RemintDoesNotRefuseItsOwnRemoteSecret covers the
// second pass over a Secret that is already there. The claim refuses to adopt a
// live object it does not own, and a Secret read back from the API server is
// live by definition (it has a UID), so a re-mint that could not recognize its
// own child would fail every rotation from the first one on.
func TestSecretStoreReconcile_RemintDoesNotRefuseItsOwnRemoteSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: "edge-1"}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}

	// Minted an hour ago against a one-hour TTL, so the pass is past the
	// proactive threshold and re-mints. The UID is the one the API server would
	// have assigned; the fake client assigns none on Create.
	live := claimedByStore(t, store, testMintedSecret(testClock.Add(-time.Hour), "3600", testSecretID))
	live.UID = types.UID("live-credentials-uid")

	bao := newProvisionedFakeClient()
	bao.secretIDs = []string{"fresh-secret-id"}
	f := newStoreFixture(bao, store, targetedBarbican("edge-1"))
	children := childrenFake(t, testInstance(true), testCASecret(), live)
	f.reconciler.Resolver = &childrenResolver{children: children}

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	var creds corev1.Secret
	g.Expect(children.Get(ctx, credentialsKey, &creds)).To(Succeed())
	g.Expect(creds.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("fresh-secret-id")))
	g.Expect(creds.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerNameLabel, testStoreName),
		"the claim survives the update that re-mints")
	g.Expect(condition(f.store(t), conditionTypeCredentialsReady).Status).To(Equal(metav1.ConditionTrue))
}

// TestSecretStoreReconcile_RemoteMintRefusesAForeignSecret is the other side of
// that recognition. A live Secret of the same name that nobody claimed belongs
// to whoever put it there: overwriting its data and stamping the ownership
// labels on it would also get it deleted at this store's teardown.
func TestSecretStoreReconcile_RemoteMintRefusesAForeignSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	store.Annotations = map[string]string{childrenClusterAnnotation: "edge-1"}
	store.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}

	foreign := managedApproleSecret(testStoreName, "someone-elses-role", "someone-elses-secret")
	foreign.UID = types.UID("foreign-credentials-uid")

	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	children := childrenFake(t, testInstance(true), testCASecret(), foreign)
	f.reconciler.Resolver = &childrenResolver{children: children}

	_, err := f.reconcile(t, store)

	g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt pre-existing")))
	var untouched corev1.Secret
	g.Expect(children.Get(ctx, credentialsKey, &untouched)).To(Succeed())
	g.Expect(untouched.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("someone-elses-secret")))
	g.Expect(untouched.Labels).To(BeEmpty())
}

// TestSecretStoreDelete_DeletesRemoteCredentialsWithoutTheParent is what the
// finalizer is for. The Secret sits on a cluster no garbage collection cascade
// reaches from here, and the parent Barbican that named that cluster is already
// gone — the ordinary order when a whole namespace is deleted. The annotation is
// what still answers where to clean up.
func TestSecretStoreDelete_DeletesRemoteCredentialsWithoutTheParent(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("edge-1", testClock)
	creds := claimedByStore(t, store, managedApproleSecret(testStoreName, testRoleID, testSecretID))
	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), store)
	children := childrenFake(t, creds)
	f.reconciler.Resolver = &childrenResolver{children: children}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	err = children.Get(ctx, credentialsKey, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"nothing on the target cluster deletes the Secret, so the teardown has to")
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_ForeignSecretOfTheSameNameSurvives keeps the teardown as
// narrow as the claim: the store name reserves nothing in a namespace it shares,
// and a Secret carrying none of this store's ownership labels is not its to
// delete. Leaving it standing must still release the finalizer, or the CR would
// hang on an object it may never touch.
func TestSecretStoreDelete_ForeignSecretOfTheSameNameSurvives(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("edge-1", testClock)
	foreign := managedApproleSecret(testStoreName, "someone-elses-role", "someone-elses-secret")
	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), store)
	children := childrenFake(t, foreign)
	f.reconciler.Resolver = &childrenResolver{children: children}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	var survivor corev1.Secret
	g.Expect(children.Get(ctx, credentialsKey, &survivor)).To(Succeed())
	g.Expect(survivor.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte("someone-elses-secret")))
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"),
		"a Secret this store may not delete must not pin it either")
}

// TestSecretStoreDelete_ReadsTheCredentialsThroughTheUncachedReader pins where
// the teardown looks. A NotFound is what licenses the finalizer release, so a
// Secret the target's informer cache has not caught up on — an operator
// restarted, a cluster engaged moments ago — would be reported as already gone
// and left behind: a live OpenBao role ID and secret ID materialized on a
// cluster with no CR left to delete them.
func TestSecretStoreDelete_ReadsTheCredentialsThroughTheUncachedReader(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("edge-1", testClock)
	creds := claimedByStore(t, store, managedApproleSecret(testStoreName, testRoleID, testSecretID))
	children := childrenFake(t, creds)
	// One cluster, two views of it: the cache has not caught up with the Secret
	// the API server already serves.
	stale := interceptor.NewClient(children, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key == credentialsKey {
				return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), store)
	f.reconciler.Resolver = &childrenResolver{children: stale, reader: children}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	err = children.Get(ctx, credentialsKey, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the credential must be deleted from live state, not declared gone by a cache")
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_FallsBackToTheParentTargetRef covers the crash window
// between the two teardown marks: a store that carries the finalizer without the
// annotation. The parent Barbican still names the cluster, so the cleanup runs
// off it rather than giving up.
func TestSecretStoreDelete_FallsBackToTheParentTargetRef(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("", testClock)
	creds := claimedByStore(t, store, managedApproleSecret(testStoreName, testRoleID, testSecretID))
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	children := childrenFake(t, creds)
	resolver := &childrenResolver{children: children}
	f.reconciler.Resolver = resolver

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(resolver.names).To(ConsistOf(mcruntime.ClusterName("edge-1")))
	err = children.Get(ctx, credentialsKey, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_PlantedAnnotationCannotDivertTheSweep is the
// deletion-path half of the guard the normal path carries. Nothing corrects the
// annotation once the store has a deletionTimestamp — the API server takes
// annotation writes on a terminating object, and the normal path no longer runs
// — so a planted name must not be able to send the teardown somewhere else and
// have the finalizer released on a live AppRole secret ID abandoned on the real
// target. The parent still names that target, so the Secret goes; the planted
// cluster is visited too, and finds nothing to take.
func TestSecretStoreDelete_PlantedAnnotationCannotDivertTheSweep(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("planted-edge", testClock)
	creds := claimedByStore(t, store, managedApproleSecret(testStoreName, testRoleID, testSecretID))
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-1"))
	children := childrenFake(t, creds)
	planted := childrenFake(t)
	resolver := &childrenResolver{byName: map[string]client.Client{"edge-1": children, "planted-edge": planted}}
	f.reconciler.Resolver = resolver

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(resolver.names).To(HaveExactElements(
		mcruntime.ClusterName("edge-1"), mcruntime.ClusterName("planted-edge")),
		"the cluster the parent names is visited, and first")
	err = children.Get(ctx, credentialsKey, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the finalizer may only be released once the real Secret is gone")
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_ReachesTheClusterOnlyTheAnnotationStillNames covers the
// parent that answers with the wrong cluster. spec.targetClusterRef is immutable
// under a CEL transition rule, and a transition rule is evaluated on UPDATE
// only: deleting the parent Barbican and re-creating it under the same name
// against another target moves it without one ever firing. The annotation still
// names where this store minted, so the Secret there is deleted rather than left
// holding a live secret ID with no CR to reclaim it.
func TestSecretStoreDelete_ReachesTheClusterOnlyTheAnnotationStillNames(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := terminatingRemoteStore("edge-1", testClock)
	creds := claimedByStore(t, store, managedApproleSecret(testStoreName, testRoleID, testSecretID))
	// The parent was re-created against edge-2 since the mint; nothing on it
	// records that this store's Secret is still on edge-1.
	f := newStoreFixture(newProvisionedFakeClient(), store, targetedBarbican("edge-2"))
	minted := childrenFake(t, creds)
	current := childrenFake(t)
	resolver := &childrenResolver{byName: map[string]client.Client{"edge-1": minted, "edge-2": current}}
	f.reconciler.Resolver = resolver

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(resolver.names).To(ContainElement(mcruntime.ClusterName("edge-1")),
		"the cluster the annotation names is visited even while the parent answers")
	err = minted.Get(ctx, credentialsKey, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"a Secret the parent no longer points at is still this store's to delete")
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_WithoutAnyClusterNameWaitsThenAbandons covers the one
// state neither mark answers: no annotation and no parent either. Waiting cannot
// produce the name, but a parent deleted moments ago may still be readable on
// the next pass, so the store sits out the abandon window before it gives the
// Secret up rather than stranding itself in Terminating.
func TestSecretStoreDelete_WithoutAnyClusterNameWaitsThenAbandons(t *testing.T) {
	g := NewGomegaWithT(t)

	waiting := terminatingRemoteStore("", testClock)
	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), waiting)

	result, err := f.reconcile(t, waiting)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(f.store(t).Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	g.Expect(f.warnings()).To(BeEmpty(), "nothing is abandoned while the name may still appear")

	// The same store, deleted longer ago than the window: the Secret is given up,
	// and it is announced rather than dropped silently.
	abandoned := terminatingRemoteStore("", testClock.Add(-2*commonmulticluster.AbandonAfter))
	f = newStoreFixtureWithoutParent(newProvisionedFakeClient(), abandoned)

	result, err = f.reconcile(t, abandoned)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(f.warnings()).To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_AbandonsAnUnresolvableTargetCluster covers the cluster
// the store can name but nothing can reach: deregistering a target is a
// documented operation, and the store that minted onto it carries the finalizer
// by then. Its Secret is unreachable either way, so the CR is released rather
// than left Terminating — but only after the window, because engagement is
// asynchronous and a cluster that has not been engaged yet looks the same.
func TestSecretStoreDelete_AbandonsAnUnresolvableTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	abandonAfter := commonmulticluster.AbandonAfter
	t.Cleanup(func() { commonmulticluster.AbandonAfter = abandonAfter })
	commonmulticluster.AbandonAfter = time.Millisecond

	// Deleted at wall-clock now: the two windows the resolver weighs run against
	// the real clock, not this controller's injected one.
	store := terminatingRemoteStore("deregistered-edge", time.Now())
	f := newStoreFixtureWithoutParent(newProvisionedFakeClient(), store)
	f.reconciler.Resolver = unresolvableResolver{}

	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling),
		"the first pass this process fails to resolve on starts the window, it does not end it")
	g.Expect(f.store(t).Finalizers).To(ContainElement(commonmulticluster.RemoteChildrenFinalizer))
	time.Sleep(10 * commonmulticluster.AbandonAfter)

	result, err = f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(f.warnings()).To(ContainElement(ContainSubstring("RemoteChildrenAbandoned")))
	g.Expect(f.store(t).Finalizers).To(ConsistOf("foreign.example.com/keep-alive"))
}

// TestSecretStoreDelete_LocalStoreCarriesNoTeardownMarks pins the unchanged half.
// A store whose parent keeps its workload on the management cluster owns its
// Secret by owner reference, so the cascade collects it: no annotation, no
// finalizer, and deleting the CR takes it straight out of etcd.
func TestSecretStoreDelete_LocalStoreCarriesNoTeardownMarks(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	store := testManagedStore()
	f := newStoreFixture(newProvisionedFakeClient(),
		append(projectedConfigObjects(), store, testInstance(true), testCASecret())...)

	_, err := f.reconcile(t, store)
	g.Expect(err).NotTo(HaveOccurred())

	local := f.store(t)
	g.Expect(local.Annotations).NotTo(HaveKey(childrenClusterAnnotation))
	g.Expect(local.Finalizers).To(BeEmpty())
	creds := f.credentialsSecret(t)
	g.Expect(creds.OwnerReferences).To(HaveLen(1), "the cascade needs the reference the labels stand in for remotely")
	g.Expect(creds.Labels).NotTo(HaveKey(commonmulticluster.OwnerKindLabel))

	g.Expect(f.reconciler.Delete(ctx, local)).To(Succeed())
	result, err := f.reconcile(t, store)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	err = f.reconciler.Get(ctx, client.ObjectKeyFromObject(store), &barbicanv1alpha1.BarbicanSecretStore{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no finalizer holds a local store back")
}

// --- watch mappers ---------------------------------------------------------

func TestBarbicanToSecretStoresMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	other := testBrownfieldStore()
	other.Name = "other"
	other.Spec.BarbicanRef.Name = "another-barbican"
	f := newStoreFixture(newProvisionedFakeClient(), store, other, testBarbican())

	requests := barbicanToSecretStoresMapper(f.reconciler.Client)(context.Background(), testBarbican())
	g.Expect(requests).To(ConsistOf(reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testStoreName},
	}))
}

func TestOpenBaoClusterToSecretStoresMapper(t *testing.T) {
	g := NewGomegaWithT(t)

	store := testManagedStore()
	// A brownfield store names no instance, so it is indexed under none.
	other := testBrownfieldStore()
	other.Name = "other"
	f := newStoreFixture(newProvisionedFakeClient(), store, other, testInstance(true))

	requests := openBaoClusterToSecretStoresMapper(f.reconciler.Client)(context.Background(), testInstance(true))
	g.Expect(requests).To(ConsistOf(reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testStoreName},
	}))
}
