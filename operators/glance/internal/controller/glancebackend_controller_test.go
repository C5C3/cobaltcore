// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	commonconditions "github.com/c5c3/forge/internal/common/conditions"
	commonmulticluster "github.com/c5c3/forge/internal/common/multicluster"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

const (
	testS3AccessKeyID     = "AKIAEXAMPLE"
	testS3SecretAccessKey = "s3-secret-key"
)

// testScheme registers the types the fake client resolves in this package's
// tests: core/apps (Secret, Deployment), the glance API, and the
// external-secrets v1 group the credential gate reads to attribute a missing
// Secret.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = glancev1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	// Gateway API and MariaDB types back the HTTPRoute and database
	// finalize/provision paths the controller and workload steps exercise.
	_ = gatewayv1.Install(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// glanceFakeClientBuilder returns a fake client builder with the glance scheme,
// the Glance/GlanceBackend field indexes the mappers resolve against, and the
// status subresources both controllers write.
func glanceFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&glancev1alpha1.Glance{}, &glancev1alpha1.GlanceBackend{}).
		WithIndex(&glancev1alpha1.Glance{}, GlanceSecretNameIndexKey, glanceSecretNameExtractor).
		WithIndex(&glancev1alpha1.GlanceBackend{}, GlanceBackendGlanceRefIndexKey, glanceBackendGlanceRefExtractor).
		WithIndex(&glancev1alpha1.GlanceBackend{}, GlanceBackendSecretNameIndexKey, glanceBackendSecretNameExtractor)
}

// newMapperFakeClient is the common-case shortcut for the watch-mapper tests:
// a pre-indexed fake client with the given objects seeded.
func newMapperFakeClient(objs ...client.Object) client.Client {
	return glanceFakeClientBuilder(objs...).Build()
}

// childrenFake builds a target cluster's client: core/apps only, and
// deliberately WITHOUT the glance API group — a CR read, a sibling list, or a
// status write misrouted to it then fails with "no kind is registered" instead
// of passing on an empty result that would read as "nothing attached".
func childrenFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("building the children scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// fakeTargetCluster exposes a fake client as a registered cluster. Embedding the
// interface leaves every other method nil, which panics if the resolver ever
// reaches for one.
type fakeTargetCluster struct {
	cluster.Cluster
	c client.Client
}

func (f fakeTargetCluster) GetClient() client.Client { return f.c }

// A fake client is read-your-writes, so it stands in for both the cached client
// and the uncached reader the write sites read live state through.
func (f fakeTargetCluster) GetAPIReader() client.Reader { return f.c }

// childrenResolver registers exactly one cluster under every name and records
// the names it was asked for, so a test can prove which ref a backend resolved —
// or that it resolved none at all.
type childrenResolver struct {
	children client.Client
	names    []mcruntime.ClusterName
}

func (r *childrenResolver) GetCluster(_ context.Context, name mcruntime.ClusterName) (cluster.Cluster, error) {
	r.names = append(r.names, name)
	return fakeTargetCluster{c: r.children}, nil
}

// targetedGlance returns the parent Glance placing its workload on the named
// target cluster.
func targetedGlance(target string) *glancev1alpha1.Glance {
	glance := testGlance()
	glance.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: target}
	return glance
}

// newBackendTestReconciler builds a GlanceBackendReconciler over a fake client
// pre-loaded with objs.
func newBackendTestReconciler(objs ...client.Object) *GlanceBackendReconciler {
	s := testScheme()
	return &GlanceBackendReconciler{
		Client:   glanceFakeClientBuilder(objs...).Build(),
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// testGlance returns a minimal Glance CR a backend attaches to, with the
// serviceUser and database Secret references the index extractor reads.
func testGlance() *glancev1alpha1.Glance {
	return &glancev1alpha1.Glance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-glance",
			Namespace:  "default",
			UID:        "glance-uid",
			Generation: 1,
		},
		Spec: glancev1alpha1.GlanceSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/glance", Tag: "2026.1"},
			Database:         commonv1.DatabaseSpec{Host: "db.example.com", Port: 3306, Database: "glance", SecretRef: commonv1.SecretRefSpec{Name: "glance-db"}},
			Cache:            commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			KeystoneEndpoint: "http://keystone.default.svc:5000",
			ServiceUser:      glancev1alpha1.ServiceUserSpec{SecretRef: commonv1.SecretRefSpec{Name: "glance-service-user"}},
		},
	}
}

// testGlanceBackend returns a minimal S3 GlanceBackend attached to glanceRef,
// referencing the credentials Secret testS3CredentialsSecret provides.
func testGlanceBackend(name, glanceRef string) *glancev1alpha1.GlanceBackend {
	return &glancev1alpha1.GlanceBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID("backend-uid-" + name),
			Generation: 1,
		},
		Spec: glancev1alpha1.GlanceBackendSpec{
			GlanceRef: glancev1alpha1.GlanceRefSpec{Name: glanceRef},
			Type:      glancev1alpha1.GlanceBackendTypeS3,
			S3: &glancev1alpha1.S3BackendSpec{
				Host:                 "https://s3.example.com",
				Bucket:               "images",
				CredentialsSecretRef: glancev1alpha1.SecretNameRefSpec{Name: name + "-s3-creds"},
			},
		},
	}
}

// testS3CredentialsSecret returns the S3 credentials Secret referenced by a
// backend built via testGlanceBackend, carrying both contract data keys.
func testS3CredentialsSecret(backendName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backendName + "-s3-creds", Namespace: "default"},
		Data: map[string][]byte{
			glancev1alpha1.S3AccessKeyIDKey:     []byte(testS3AccessKeyID),
			glancev1alpha1.S3SecretAccessKeyKey: []byte(testS3SecretAccessKey),
		},
	}
}

// testProjectedDeployment returns the Glance Deployment carrying the backends
// volume pointing at the given projection Secret name.
func testProjectedDeployment(glance *glancev1alpha1.Glance, backendsSecretName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: subResourceName(glance), Namespace: glance.Namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name:         backendsVolumeName,
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: backendsSecretName}},
					}},
				},
			},
		},
	}
}

// testBackendsSecret returns the projection Secret whose backends.conf carries
// the given rendered INI document.
func testBackendsSecret(name, backendsConf string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string][]byte{backendsConfDataKey: []byte(backendsConf)},
	}
}

// backendRequest builds the reconcile request for a backend fixture.
func backendRequest(b *glancev1alpha1.GlanceBackend) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)}
}

// getBackend re-reads the backend from the given client.
func getBackend(t *testing.T, c client.Client, name string) *glancev1alpha1.GlanceBackend {
	t.Helper()
	var b glancev1alpha1.GlanceBackend
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &b); err != nil {
		t.Fatalf("re-reading backend %s: %v", name, err)
	}
	return &b
}

func TestBackendReconcile_CredentialsSecretMissingWaits(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	// Parent Glance present, credentials Secret absent.
	r := newBackendTestReconciler(backend, testGlance())

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCredentials))

	// The aggregate mirrors the failure, and ObservedGeneration is stamped.
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
}

func TestBackendReconcile_CredentialsSecretMissingOneKeyWaits(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	// Secret present but missing the secret-access-key contract key.
	partial := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "store-s3-creds", Namespace: "default"},
		Data:       map[string][]byte{glancev1alpha1.S3AccessKeyIDKey: []byte(testS3AccessKeyID)},
	}
	r := newBackendTestReconciler(backend, testGlance(), partial)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForCredentials))
}

func TestBackendReconcile_CredentialsReadyWhenBothKeysPresent(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	// Credentials complete, but no Deployment yet: CredentialsReady flips True
	// while ConfigProjected keeps waiting.
	r := newBackendTestReconciler(backend, testGlance(), testS3CredentialsSecret("store"))

	_, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())

	updated := getBackend(t, r.Client, "store")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonCredentialsAvailable))
}

// A parent that does not exist stops the pass at the first gate: without one
// there is nothing to say which cluster this backend's credentials and its
// parent Deployment live on, so neither is read and the projection observation
// never runs.
func TestBackendReconcile_NoParentGlanceHoldsBeforeProjection(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	// The credentials Secret is seeded, so only the missing parent can explain
	// the hold.
	r := newBackendTestReconciler(backend, testS3CredentialsSecret("store"))

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	creds := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForParent))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)).To(BeNil(),
		"the pass returned before the projection observation could run")
}

// A backend that already converged carries ConfigProjected=True observed
// against the parent's Deployment. Deleting the parent must demote that claim
// (the deletion-cleanup e2e pins the surviving backend on
// ConfigProjected=False/WaitingForProjection) instead of leaving it standing
// forever behind the WaitingForParent hold.
func TestBackendReconcile_ParentDeletionDemotesStaleConfigProjected(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	backend.Status.Conditions = []metav1.Condition{
		{Type: conditionTypeCredentialsReady, Status: metav1.ConditionTrue, Reason: conditionReasonCredentialsAvailable, ObservedGeneration: 1, LastTransitionTime: metav1.Now()},
		{Type: conditionTypeConfigProjected, Status: metav1.ConditionTrue, Reason: conditionReasonConfigProjected, ObservedGeneration: 1, LastTransitionTime: metav1.Now()},
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReady", ObservedGeneration: 1, LastTransitionTime: metav1.Now()},
	}
	// No parent Glance in the client: it was deleted after the convergence.
	r := newBackendTestReconciler(backend, testS3CredentialsSecret("store"))

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
	creds := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForParent))
	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal("NotAllReady"))
}

func TestBackendReconcile_ConfigProjectedNoDeploymentWaits(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	r := newBackendTestReconciler(backend, testGlance(), testS3CredentialsSecret("store"))

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

func TestBackendReconcile_ConfigProjectedDeploymentWithoutBackendsVolumeWaits(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := testGlance()
	backend := testGlanceBackend("store", "test-glance")
	// Deployment exists but has no backends volume yet.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: subResourceName(glance), Namespace: glance.Namespace},
	}
	r := newBackendTestReconciler(backend, glance, testS3CredentialsSecret("store"), deploy)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(projected.Reason).To(Equal(conditionReasonWaitingForProjection))
}

func TestBackendReconcile_ConfigProjectedFlipsAndAggregatesReady(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := testGlance()
	backend := testGlanceBackend("store", "test-glance")
	backendsSecretName := "test-glance-backends-abcd1234"
	deploy := testProjectedDeployment(glance, backendsSecretName)
	secret := testBackendsSecret(backendsSecretName, "[default]\n\n[store]\ns3_store_host = https://s3.example.com\n")
	r := newBackendTestReconciler(backend, glance, testS3CredentialsSecret("store"), deploy, secret)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(projected.Reason).To(Equal(conditionReasonConfigProjected))

	ready := commonconditions.GetCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("AllReady"))
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(1)))
}

// A section header that is only a prefix of the rendered backend's section
// ([store2] vs store) must not satisfy the lookup: ConfigProjected stays False.
func TestBackendReconcile_ConfigProjectedSubstringCollisionStaysFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := testGlance()
	backend := testGlanceBackend("store", "test-glance")
	backendsSecretName := "test-glance-backends-abcd1234"
	deploy := testProjectedDeployment(glance, backendsSecretName)
	// Only a collision section is present, not [store].
	secret := testBackendsSecret(backendsSecretName, "[store2]\ns3_store_host = https://s3.example.com\n")
	r := newBackendTestReconciler(backend, glance, testS3CredentialsSecret("store"), deploy, secret)

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionFalse))
}

// A currently-True ConfigProjected must ride out a transient Get failure during
// observation: the outage surfaces through the reconcile error path (workqueue
// backoff), not as a condition demotion.
func TestBackendReconcile_TransientObservationErrorPreservesTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := testGlance()
	backend := testGlanceBackend("store", "test-glance")
	backend.Status.Conditions = []metav1.Condition{
		{Type: conditionTypeCredentialsReady, Status: metav1.ConditionTrue, Reason: conditionReasonCredentialsAvailable, LastTransitionTime: metav1.Now()},
		{Type: conditionTypeConfigProjected, Status: metav1.ConditionTrue, Reason: conditionReasonConfigProjected, LastTransitionTime: metav1.Now()},
	}
	c := glanceFakeClientBuilder(backend, glance, testS3CredentialsSecret("store")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*glancev1alpha1.Glance); ok {
					return fmt.Errorf("simulated transient API error")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &GlanceBackendReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).To(HaveOccurred(), "the outage must surface through the error path")

	updated := getBackend(t, r.Client, "store")
	projected := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)
	g.Expect(projected.Status).To(Equal(metav1.ConditionTrue), "a cache blip says nothing about the projection")
	g.Expect(projected.Reason).To(Equal(conditionReasonConfigProjected))
}

// TestBackendReconcile_ObservesOnParentTargetCluster pins the split a backend
// inherits from its parent: the CR and its status stay on the management
// cluster, while the S3 credentials Secret it gates on, the parent Deployment it
// observes, and that Deployment's projection Secret live on the cluster the
// parent's spec.targetClusterRef names — the same one the API pods run on.
func TestBackendReconcile_ObservesOnParentTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := targetedGlance("edge-1")
	backend := testGlanceBackend("store", "test-glance")
	backendsSecretName := "test-glance-backends-abcd1234"
	// Management carries the backend and its parent; all three objects the gates
	// read exist ONLY on the target cluster, so a misrouted read reports
	// WaitingForCredentials or WaitingForProjection instead of Ready.
	r := newBackendTestReconciler(backend, glance)
	resolver := &childrenResolver{children: childrenFake(t,
		testS3CredentialsSecret("store"),
		testProjectedDeployment(glance, backendsSecretName),
		testBackendsSecret(backendsSecretName, "[default]\n\n[store]\ns3_store_host = https://s3.example.com\n"),
	)}
	r.Resolver = resolver

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(resolver.names).To(Equal([]mcruntime.ClusterName{"edge-1"}),
		"the backend resolves the target its parent names, having none of its own")

	// The status landed on the management cluster, the only one carrying the CR.
	updated := getBackend(t, r.Client, "store")
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady).Status).
		To(Equal(metav1.ConditionTrue))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected).Status).
		To(Equal(metav1.ConditionTrue))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, "Ready").Status).
		To(Equal(metav1.ConditionTrue))
}

// TestBackendReconcile_TargetClusterUnavailableGatesCredentials pins the failure
// surface of a parent naming a cluster nothing registered: the backend reports
// it on its first gate condition and waits, rather than attributing the gap to
// a credential the target cluster may well hold.
func TestBackendReconcile_TargetClusterUnavailableGatesCredentials(t *testing.T) {
	g := NewGomegaWithT(t)

	glance := targetedGlance("nowhere")
	backend := testGlanceBackend("store", "test-glance")
	backendsSecretName := "test-glance-backends-abcd1234"
	// Credentials and a fully projected Deployment ARE seeded beside the CR, so
	// only the gate can explain the backend not going Ready.
	r := newBackendTestReconciler(backend, glance, testS3CredentialsSecret("store"),
		testProjectedDeployment(glance, backendsSecretName),
		testBackendsSecret(backendsSecretName, "[store]\ns3_store_host = https://s3.example.com\n"))
	r.Resolver = unresolvableResolver{}

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred(),
		"an unregistered target cluster is a wait, not a reconcile failure")
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

	updated := getBackend(t, r.Client, "store")
	cond := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(cond).NotTo(BeNil(), "the first gate condition must carry the failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))
	g.Expect(commonconditions.GetCondition(updated.Status.Conditions, conditionTypeConfigProjected)).To(BeNil(),
		"the pass returned before the projection observation could run")
}

// TestBackendReconcile_MissingParentCostsNoClusterLookup covers the dangling
// spec.glanceRef from the resolver's side: with no parent there is no ref to
// resolve, so no cluster is looked up and the backend waits instead of gating
// on whatever Secret of that name happens to sit beside the CR — which for a
// parent that lives on a target cluster would be the wrong one entirely.
func TestBackendReconcile_MissingParentCostsNoClusterLookup(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	// No parent Glance: the credentials Secret beside the CR is all there is.
	r := newBackendTestReconciler(backend, testS3CredentialsSecret("store"))
	resolver := &childrenResolver{children: childrenFake(t)}
	r.Resolver = resolver

	result, err := r.Reconcile(context.Background(), backendRequest(backend))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(resolver.names).To(BeEmpty(), "a backend whose parent is gone costs no cluster lookup")

	updated := getBackend(t, r.Client, "store")
	creds := commonconditions.GetCondition(updated.Status.Conditions, conditionTypeCredentialsReady)
	g.Expect(creds.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(creds.Reason).To(Equal(conditionReasonWaitingForParent))
	g.Expect(creds.Message).To(ContainSubstring("test-glance"))
}

func TestBackendReconcile_DeletionTimestampIsNoOp(t *testing.T) {
	g := NewGomegaWithT(t)

	backend := testGlanceBackend("store", "test-glance")
	now := metav1.Now()
	backend.DeletionTimestamp = &now
	// A placeholder finalizer keeps the fake client from garbage-collecting the
	// seeded terminating object before the reconcile observes it.
	backend.Finalizers = []string{"test/keep"}
	r := newBackendTestReconciler(backend, testGlance(), testS3CredentialsSecret("store"))

	result, err := r.Reconcile(context.Background(), backendRequest(backend))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())

	updated := getBackend(t, r.Client, "store")
	g.Expect(updated.Status.Conditions).To(BeEmpty(), "a deleting backend writes no status")
}

func TestBackendSectionPresent(t *testing.T) {
	cases := []struct {
		name    string
		conf    string
		section string
		want    bool
	}{
		{name: "exact match on its own line", conf: "[store]\nk = v\n", section: "store", want: true},
		{name: "match at start of data", conf: "[store]", section: "store", want: true},
		{name: "match at end without trailing newline", conf: "[a]\nk = v\n[store]", section: "store", want: true},
		{name: "match with CRLF line ending", conf: "[store]\r\nk = v\r\n", section: "store", want: true},
		{name: "prefix collision is not a match", conf: "[store2]\nk = v\n", section: "store", want: false},
		{name: "suffix collision is not a match", conf: "[xstore]\nk = v\n", section: "store", want: false},
		{name: "header inside a value line is not a match", conf: "note = see [store]\n", section: "store", want: false},
		{name: "absent section", conf: "[other]\nk = v\n", section: "store", want: false},
		{name: "empty data", conf: "", section: "store", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(backendSectionPresent([]byte(tc.conf), tc.section)).To(Equal(tc.want))
		})
	}
}
