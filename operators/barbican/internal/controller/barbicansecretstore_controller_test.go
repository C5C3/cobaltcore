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
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	"github.com/c5c3/forge/operators/barbican/internal/metrics"
	"github.com/c5c3/forge/operators/barbican/internal/openbao"
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
// ConfigMap, Deployment, ServiceAccount), the barbican API, and the
// openbao.org API the managed mode reads the instance from.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = barbicanv1alpha1.AddToScheme(s)
	_ = openbaov1alpha1.AddToScheme(s)
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

// testBarbican returns the parent Barbican CR a store attaches to.
func testBarbican() *barbicanv1alpha1.Barbican {
	return &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{Name: testBarbicanName, Namespace: testNamespace, Generation: 1},
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
