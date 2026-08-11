// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration tests for the Barbican and BarbicanSecretStore reconcilers running
// together against a live envtest API server.
package controller

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/forge/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	"github.com/c5c3/forge/internal/common/watch"
	barbicanv1alpha1 "github.com/c5c3/forge/operators/barbican/api/v1alpha1"
	"github.com/c5c3/forge/operators/barbican/internal/openbao"
	"github.com/c5c3/forge/operators/barbican/internal/testutil"
)

// Test timeout constants for CI tuning.
const (
	// eventuallyTimeout is the default polling timeout for Eventually assertions.
	eventuallyTimeout = 30 * time.Second
	// eventuallyLongTimeout covers the db-sync/Deployment path, which depends on
	// the RequeueDatabaseWait backoff to rediscover a simulated Job completion.
	eventuallyLongTimeout = 2 * RequeueDatabaseWait
	// pollInterval is the polling interval for Eventually assertions.
	pollInterval = 500 * time.Millisecond
)

// The object names the integration fixtures below share.
const (
	integrationBarbicanName = "barbican"
	integrationStoreName    = "openbao-primary"
	integrationInstanceName = "openbao"

	// #nosec G101 -- Secret object name, not a credential.
	integrationStoreCredentialsSecret = "openbao-approle"
	integrationStoreCASecret          = "openbao-ca"
)

// --- Shared helpers ---

// setupEnvTestWithController wraps testutil.SetupBarbicanEnvTestWithController
// with the v1alpha1 scheme and the webhook + controller registration callbacks.
// It wires BOTH webhooks (the manifests envtest installs carry both kinds,
// failurePolicy=Fail) and BOTH reconcilers, registering the parent before the
// satellite exactly as main.go does: BarbicanReconciler.SetupWithManager is the
// single registration site for the field indexes both controllers resolve
// against.
//
// newOpenBaoClient is the BarbicanSecretStoreReconciler's OpenBao seam: nil
// leaves the production constructor in place, so the store talks to whatever
// server its spec addresses. The token-mint and clock seams stay on their
// production implementations in every case: envtest serves the TokenRequest
// subresource, so a managed store mints against the live API server.
//
// The reconcilers are built manually rather than via SetupWithManager: that
// method probes the RESTMapper for Gateway API (set explicitly here) and would
// trip controller-runtime's process-global controller-name validation across the
// sequential managers each test spins up, so the controllers opt out via
// SkipNameValidation, exactly as keystone's, glance's and placement's
// integration setups do.
func setupEnvTestWithController(
	t testing.TB,
	newOpenBaoClient func(openbao.Config) (openbao.Client, error),
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupBarbicanEnvTestWithController(
		t,
		barbicanv1alpha1.AddToScheme,
		func(mgr ctrl.Manager) error {
			if err := (&barbicanv1alpha1.BarbicanWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr); err != nil {
				return err
			}
			return (&barbicanv1alpha1.BarbicanSecretStoreWebhook{Client: mgr.GetAPIReader()}).SetupWebhookWithManager(mgr)
		},
		func(mgr ctrl.Manager) error {
			// Register both field-index sets here, the single registration site,
			// mirroring BarbicanReconciler.SetupWithManager.
			if err := registerBarbicanIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}
			if err := registerBarbicanSecretStoreIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
				return err
			}

			r := &BarbicanReconciler{
				Client:   mgr.GetClient(),
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorderFor("barbican-controller"),
				// A healthy stub keeps the health-check probe from firing slow HTTP
				// GETs at the unreachable Service DNS; envtest has no kubelet.
				HTTPClient: &stubDoer{status: http.StatusOK},
				// envtest loads the fake HTTPRoute CRD, so the kind is available.
				// Mirror what SetupWithManager would set from the RESTMapper.
				gatewayAPIAvailable: true,
				apiReader:           mgr.GetAPIReader(),
			}
			if err := ctrl.NewControllerManagedBy(mgr).
				For(&barbicanv1alpha1.Barbican{}, builder.WithPredicates(watch.CRUpdatePredicate())).
				Owns(&appsv1.Deployment{}).
				Owns(&corev1.Service{}).
				Owns(&corev1.Secret{}).
				Owns(&policyv1.PodDisruptionBudget{}).
				Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
				Owns(&networkingv1.NetworkPolicy{}).
				Owns(&batchv1.Job{}).
				Owns(&batchv1.CronJob{}).
				Owns(&gatewayv1.HTTPRoute{}).
				Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(
					secretToBarbicanWithStoresMapper(mgr.GetClient()),
				)).
				Watches(&barbicanv1alpha1.BarbicanSecretStore{}, handler.EnqueueRequestsFromMapFunc(
					barbicanSecretStoreToBarbicanMapper(),
				)).
				Watches(&mariadbv1alpha1.MariaDB{}, handler.EnqueueRequestsFromMapFunc(
					mariaDBToBarbicanMapper(mgr.GetClient()),
				)).
				Watches(&esov1.ClusterSecretStore{}, handler.EnqueueRequestsFromMapFunc(
					esoStoreToBarbicanMapper(mgr.GetClient(), commonv1.SecretStoreKindCluster),
				)).
				Watches(&esov1.SecretStore{}, handler.EnqueueRequestsFromMapFunc(
					esoStoreToBarbicanMapper(mgr.GetClient(), commonv1.SecretStoreKindNamespaced),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(r); err != nil {
				return err
			}

			sr := &BarbicanSecretStoreReconciler{
				Client:           mgr.GetClient(),
				Scheme:           mgr.GetScheme(),
				Recorder:         mgr.GetEventRecorderFor("barbicansecretstore-controller"),
				NewOpenBaoClient: newOpenBaoClient,
			}
			return ctrl.NewControllerManagedBy(mgr).
				For(&barbicanv1alpha1.BarbicanSecretStore{}, builder.WithPredicates(watch.CRUpdatePredicate())).
				Watches(&barbicanv1alpha1.Barbican{}, handler.EnqueueRequestsFromMapFunc(
					barbicanToSecretStoresMapper(mgr.GetClient()),
				)).
				Watches(&openbaov1alpha1.OpenBaoCluster{}, handler.EnqueueRequestsFromMapFunc(
					openBaoClusterToSecretStoresMapper(mgr.GetClient()),
				)).
				WithOptions(controller.Options{SkipNameValidation: ptr.To(true)}).
				Complete(sr)
		},
	)
}

// createTestNamespace creates a uniquely named namespace per test.
func createTestNamespace(t testing.TB, ctx context.Context, c client.Client) string {
	t.Helper()
	g := NewGomegaWithT(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "barbican-it-"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed(), "create test namespace")
	return ns.Name
}

// createBarbicanPrerequisites materializes the secret store and the plain
// database and service-user Secrets the secrets gate reads. The gate reads the
// materialized Secret directly (its steady-state fast path), so no ExternalSecret
// fixture is required, mirroring the reconcile_secrets unit fixtures.
func createBarbicanPrerequisites(t testing.TB, ctx context.Context, c client.Client, ns string) {
	t.Helper()
	g := NewGomegaWithT(t)

	store := &esov1.ClusterSecretStore{ObjectMeta: metav1.ObjectMeta{Name: openBaoClusterStoreName}}
	g.Expect(c.Create(ctx, store)).To(Succeed(), "create ClusterSecretStore")
	store.Status = esov1.SecretStoreStatus{
		Conditions: []esov1.SecretStoreStatusCondition{
			{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
		},
	}
	g.Expect(c.Status().Update(ctx, store)).To(Succeed(), "mark the ClusterSecretStore Ready")

	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barbican-db", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("barbican"), "password": []byte("db-pw")},
	}
	g.Expect(c.Create(ctx, dbSecret)).To(Succeed(), "create DB Secret")

	svcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "barbican-service-user", Namespace: ns},
		Data:       map[string][]byte{"password": []byte("svc-pw")},
	}
	g.Expect(c.Create(ctx, svcSecret)).To(Succeed(), "create service-user Secret")
}

// integrationBarbicanCR returns a valid brownfield Barbican CR (spec.database.host
// set, no clusterRef) so no MariaDB cluster CR is needed to progress the pipeline.
func integrationBarbicanCR(name, ns string) *barbicanv1alpha1.Barbican {
	return &barbicanv1alpha1.Barbican{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: barbicanv1alpha1.BarbicanSpec{
			OpenStackRelease: "2026.1",
			Deployment:       barbicanv1alpha1.DeploymentSpec{Replicas: 1},
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/barbican", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				Host:      "db.example.com",
				Port:      3306,
				Database:  "barbican",
				SecretRef: commonv1.SecretRefSpec{Name: "barbican-db"},
			},
			Cache: commonv1.CacheSpec{
				Backend: commonv1.DefaultCacheBackend,
				Servers: []string{"mc:11211"},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000/v3",
			ServiceUser: barbicanv1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: "barbican-service-user", Key: "password"},
			},
		},
	}
}

// integrationBrownfieldStore returns a default OpenBao store addressing a server
// provisioned outside this operator at serverURL, reading its trust bundle and
// AppRole credentials from the Secrets the test creates.
func integrationBrownfieldStore(name, ns, barbicanRef, serverURL string) *barbicanv1alpha1.BarbicanSecretStore {
	return &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: barbicanRef},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
			IsDefault:   true,
			OpenBao: &barbicanv1alpha1.OpenBaoStoreSpec{
				KVMountpoint: barbicanv1alpha1.DefaultKVMountpoint,
				Server: &barbicanv1alpha1.OpenBaoServerSpec{
					URL:                  serverURL,
					CABundleSecretRef:    &barbicanv1alpha1.SecretNameRefSpec{Name: integrationStoreCASecret},
					CredentialsSecretRef: barbicanv1alpha1.SecretNameRefSpec{Name: integrationStoreCredentialsSecret},
				},
			},
		},
	}
}

// integrationManagedStore returns a default OpenBao store provisioning against
// the OpenBaoCluster instanceRef names.
func integrationManagedStore(name, ns, barbicanRef, instanceRef string) *barbicanv1alpha1.BarbicanSecretStore {
	return &barbicanv1alpha1.BarbicanSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: barbicanv1alpha1.BarbicanSecretStoreSpec{
			BarbicanRef: barbicanv1alpha1.BarbicanRefSpec{Name: barbicanRef},
			Type:        barbicanv1alpha1.BarbicanSecretStoreTypeOpenBao,
			IsDefault:   true,
			OpenBao: &barbicanv1alpha1.OpenBaoStoreSpec{
				KVMountpoint: barbicanv1alpha1.DefaultKVMountpoint,
				InstanceRef:  &barbicanv1alpha1.InstanceRefSpec{Name: instanceRef},
			},
		},
	}
}

// newFakeOpenBaoServer starts a TLS OpenBao stand-in answering the three
// endpoints a brownfield store's validation touches: the AppRole login, the
// capability read on the KV data path, and the session revocation every
// controller session ends with. The response shapes are the ones the real API
// returns (see internal/openbao/client_test.go); any other path answers 404,
// which is what OpenBao returns for an unmounted path, so an unexpected call
// fails the test instead of hanging it.
func newFakeOpenBaoServer(t testing.TB) *httptest.Server {
	t.Helper()

	routes := map[string]string{
		"/v1/auth/approle/login":     `{"auth":{"client_token":"approle.token"}}`,
		"/v1/sys/capabilities-self":  `{"data":{"capabilities":["create","read","update","delete","list"]}}`,
		"/v1/auth/token/revoke-self": `{}`,
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			body = `{"errors":["no handler for this path"]}`
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// openBaoServerCAPEM returns the PEM encoding of the test server's self-signed
// certificate, the trust bundle a client has to carry to complete the handshake.
func openBaoServerCAPEM(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

// waitForBarbicanCondition polls the Barbican CR until the named condition
// reaches the expected status. Returns the condition.
func waitForBarbicanCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var barbican barbicanv1alpha1.Barbican
		if err := c.Get(ctx, key, &barbican); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(barbican.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		fmt.Sprintf("Barbican condition %s should reach %s", condType, expected))
	return cond
}

// waitForStoreCondition polls the BarbicanSecretStore CR until the named
// condition reaches the expected status. Returns the condition.
func waitForStoreCondition(t testing.TB, ctx context.Context, c client.Client, key types.NamespacedName, condType string, expected metav1.ConditionStatus, timeout time.Duration) *metav1.Condition {
	t.Helper()
	g := NewGomegaWithT(t)

	var cond *metav1.Condition
	g.Eventually(func() metav1.ConditionStatus {
		var store barbicanv1alpha1.BarbicanSecretStore
		if err := c.Get(ctx, key, &store); err != nil {
			return ""
		}
		cond = meta.FindStatusCondition(store.Status.Conditions, condType)
		if cond == nil {
			return ""
		}
		return cond.Status
	}, timeout, pollInterval).Should(Equal(expected),
		fmt.Sprintf("BarbicanSecretStore condition %s should reach %s", condType, expected))
	return cond
}

// mountedConfigSecretName returns the name of the Secret the Deployment's config
// volume mounts, which is the rendered barbican.conf carrier.
func mountedConfigSecretName(deploy *appsv1.Deployment) string {
	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		if v.Name == configVolumeName && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// --- Tests ---

// TestIntegrationBarbican_BrownfieldStoreDrivesParentToReady walks the full
// parent chain with both controllers running and a brownfield store validated
// against a live (in-process) OpenBao server: the store's AppRole credentials are
// accepted, the parent projects the store into a rendered barbican.conf, the
// db-sync Job and the API workload materialize, and the aggregate Ready of both
// CRs turns True.
//
// envtest runs no Job or Deployment controller, so the db-sync completion and the
// Deployment rollout are simulated the way the sibling operators' integration
// tests simulate them; nothing else in the chain is stubbed.
func TestIntegrationBarbican_BrownfieldStoreDrivesParentToReady(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	bao := newFakeOpenBaoServer(t)

	c, ctx, _ := setupEnvTestWithController(t, nil)
	ns := createTestNamespace(t, ctx, c)
	createBarbicanPrerequisites(t, ctx, c, ns)

	// The brownfield store's own inputs: the trust bundle authenticating the test
	// server, and the AppRole credentials barbican authenticates with.
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationStoreCASecret, Namespace: ns},
		Data:       map[string][]byte{barbicanv1alpha1.OpenBaoCAKey: openBaoServerCAPEM(bao)},
	})).To(Succeed(), "create the OpenBao trust-bundle Secret")
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationStoreCredentialsSecret, Namespace: ns},
		Data: map[string][]byte{
			barbicanv1alpha1.OpenBaoRoleIDKey:   []byte("role-id-value"),
			barbicanv1alpha1.OpenBaoSecretIDKey: []byte("secret-id-value"),
		},
	})).To(Succeed(), "create the AppRole credentials Secret")

	barbican := integrationBarbicanCR(integrationBarbicanName, ns)
	g.Expect(c.Create(ctx, barbican)).To(Succeed(), "create Barbican CR")
	barbicanKey := types.NamespacedName{Name: integrationBarbicanName, Namespace: ns}

	store := integrationBrownfieldStore(integrationStoreName, ns, integrationBarbicanName, bao.URL)
	g.Expect(c.Create(ctx, store)).To(Succeed(), "create BarbicanSecretStore CR")
	storeKey := types.NamespacedName{Name: integrationStoreName, Namespace: ns}

	// The store controller logs in with the referenced credentials and reads back
	// the capabilities the AppRole holds on the KV data path.
	credentials := waitForStoreCondition(t, ctx, c, storeKey, conditionTypeCredentialsReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(credentials.Reason).To(Equal(conditionReasonCredentialsAvailable))
	// Nothing provisions a brownfield server, so the managed-only condition must
	// stay absent.
	var validated barbicanv1alpha1.BarbicanSecretStore
	g.Expect(c.Get(ctx, storeKey, &validated)).To(Succeed())
	g.Expect(meta.FindStatusCondition(validated.Status.Conditions, conditionTypeProvisioningReady)).To(BeNil(),
		"a brownfield store has no instance this operator provisions")

	// The parent clears its credential gate and aggregates the credential-ready
	// default into the rendered config.
	waitForBarbicanCondition(t, ctx, c, barbicanKey, "SecretsReady", metav1.ConditionTrue, eventuallyTimeout)
	stores := waitForBarbicanCondition(t, ctx, c, barbicanKey, conditionTypeSecretStoresReady, metav1.ConditionTrue, eventuallyTimeout)
	g.Expect(stores.Reason).To(Equal(conditionReasonAllStoresProjected))

	// The database step db-syncs against the rendered config; simulate the Job so
	// the API workload (which mounts the projection) can be created. envtest runs
	// no kubelet, so nothing completes the Job otherwise.
	dbSyncKey := client.ObjectKey{Namespace: ns, Name: integrationBarbicanName + "-db-sync"}
	g.Eventually(func() error {
		return c.Get(ctx, dbSyncKey, &batchv1.Job{})
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "db-sync Job should appear")
	g.Expect(simulators.SimulateJobComplete(ctx, c, dbSyncKey)).To(Succeed(), "simulate db-sync Job completion")

	// The Deployment and its Service materialize.
	deployKey := client.ObjectKey{Namespace: ns, Name: integrationBarbicanName}
	var deploy appsv1.Deployment
	g.Eventually(func() error {
		return c.Get(ctx, deployKey, &deploy)
	}, eventuallyLongTimeout, pollInterval).Should(Succeed(), "Barbican Deployment should appear")
	g.Expect(c.Get(ctx, deployKey, &corev1.Service{})).To(Succeed(), "Barbican API Service should exist")

	// The mounted config carries this store's rendered section.
	configSecretName := mountedConfigSecretName(&deploy)
	g.Expect(configSecretName).To(HavePrefix(integrationBarbicanName+"-config-"),
		"the config volume must mount the content-hashed config Secret")
	var configSecret corev1.Secret
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: configSecretName}, &configSecret)).
		To(Succeed(), "rendered config Secret should exist")
	g.Expect(string(configSecret.Data[barbicanConfDataKey])).To(ContainSubstring(storeSectionHeader(integrationStoreName)),
		"barbican.conf should carry the store's secret-store section")

	// Simulate the rollout so the parent's deployment step reports the workload
	// available; the aggregate then resolves.
	g.Expect(simulators.SimulateDeploymentReady(ctx, c, deployKey, ptr.Deref(deploy.Spec.Replicas, 1))).
		To(Succeed(), "simulate Deployment readiness")

	waitForBarbicanCondition(t, ctx, c, barbicanKey, "DatabaseReady", metav1.ConditionTrue, eventuallyLongTimeout)
	waitForBarbicanCondition(t, ctx, c, barbicanKey, "DeploymentReady", metav1.ConditionTrue, eventuallyLongTimeout)
	waitForBarbicanCondition(t, ctx, c, barbicanKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)

	var ready barbicanv1alpha1.Barbican
	g.Expect(c.Get(ctx, barbicanKey, &ready)).To(Succeed())
	g.Expect(ready.Status.InstalledRelease).To(Equal("2026.1"),
		"installedRelease should be promoted after the db-sync")
	g.Expect(ready.Status.Endpoint).NotTo(BeEmpty(), "the endpoint should be advertised once the workload is available")

	// The post-deployment parallel group runs only once the workload reports
	// available (the deployment step requeues until then), so the clean-up
	// CronJob is asserted here rather than alongside the Deployment.
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: dbCleanCronJobName(integrationBarbicanName)}, &batchv1.CronJob{})).
		To(Succeed(), "db-clean CronJob should exist")

	// With the section projected into the running Deployment, the store observes
	// the projection and its own aggregate resolves.
	waitForStoreCondition(t, ctx, c, storeKey, conditionTypeConfigProjected, metav1.ConditionTrue, eventuallyLongTimeout)
	waitForStoreCondition(t, ctx, c, storeKey, "Ready", metav1.ConditionTrue, eventuallyLongTimeout)
}

// TestIntegrationBarbicanSecretStore_ManagedCredentialsSecretIsOwned proves the
// managed-mode ownership contract through the live API server: the operator
// mints the AppRole credentials into "<store>-approle" and stamps the store CR as
// its controller owner.
//
// The owner reference IS the teardown mechanism for that Secret (the store
// controller deliberately carries no finalizer), so this assertion covers a
// behaviour envtest itself cannot demonstrate: envtest runs no garbage
// collector, so deleting the store here would leave the Secret behind no matter
// how the reference is stamped.
//
// Only the OpenBao seam is faked. The provisioner token is minted through the
// production TokenRequest path against the live API server, so the
// "<instance>-provisioner" ServiceAccount below is load-bearing: without it the
// store would park on WaitingForInstance. The parent Barbican CR is equally
// load-bearing: the store resolves the cluster its children belong on through
// the parent's spec.targetClusterRef, and without the parent it parks on
// CredentialsReady=False/WaitingForParent before provisioning starts.
func TestIntegrationBarbicanSecretStore_ManagedCredentialsSecretIsOwned(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	bao := newProvisionedFakeClient()
	factory := &fakeClientFactory{client: bao}

	c, ctx, _ := setupEnvTestWithController(t, factory.new)
	ns := createTestNamespace(t, ctx, c)

	// The OpenBaoCluster the store provisions against, turned Available.
	instance := &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{Name: integrationInstanceName, Namespace: ns},
	}
	g.Expect(c.Create(ctx, instance)).To(Succeed(), "create OpenBaoCluster")
	instance.Status.Conditions = []metav1.Condition{{
		Type:               string(openbaov1alpha1.ConditionAvailable),
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		LastTransitionTime: metav1.Now(),
	}}
	g.Expect(c.Status().Update(ctx, instance)).To(Succeed(), "mark the OpenBaoCluster Available")

	// The two objects the managed-mode naming convention derives from the
	// instance name: its trust bundle and the ServiceAccount the operator mints a
	// bound token for.
	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: integrationInstanceName + instanceCASecretSuffix, Namespace: ns},
		Data:       map[string][]byte{barbicanv1alpha1.OpenBaoCAKey: []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n")},
	})).To(Succeed(), "create the instance trust-bundle Secret")
	g.Expect(c.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: integrationInstanceName + instanceProvisionerSASuffix, Namespace: ns},
	})).To(Succeed(), "create the provisioner ServiceAccount")

	// The parent the store resolves its children cluster through. Its own
	// reconciliation parks on the missing input Secrets, which this test never
	// needs; only its existence and its nil targetClusterRef matter here.
	g.Expect(c.Create(ctx, integrationBarbicanCR(integrationBarbicanName, ns))).To(Succeed(), "create the parent Barbican CR")

	store := integrationManagedStore(integrationStoreName, ns, integrationBarbicanName, integrationInstanceName)
	g.Expect(c.Create(ctx, store)).To(Succeed(), "create the managed BarbicanSecretStore")
	storeKey := types.NamespacedName{Name: integrationStoreName, Namespace: ns}

	waitForStoreCondition(t, ctx, c, storeKey, conditionTypeProvisioningReady, metav1.ConditionTrue, eventuallyTimeout)
	waitForStoreCondition(t, ctx, c, storeKey, conditionTypeCredentialsReady, metav1.ConditionTrue, eventuallyTimeout)

	var creds corev1.Secret
	credsKey := client.ObjectKey{Namespace: ns, Name: integrationStoreName + approleSecretNameSuffix}
	g.Eventually(func() error {
		return c.Get(ctx, credsKey, &creds)
	}, eventuallyTimeout, pollInterval).Should(Succeed(), "the minted credentials Secret should exist")
	g.Expect(creds.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoRoleIDKey, []byte(testRoleID)))
	g.Expect(creds.Data).To(HaveKeyWithValue(barbicanv1alpha1.OpenBaoSecretIDKey, []byte(testSecretID)))

	var persisted barbicanv1alpha1.BarbicanSecretStore
	g.Expect(c.Get(ctx, storeKey, &persisted)).To(Succeed())

	owner := metav1.GetControllerOf(&creds)
	g.Expect(owner).NotTo(BeNil(), "the credentials Secret must carry a controller owner reference")
	g.Expect(owner.Kind).To(Equal("BarbicanSecretStore"))
	g.Expect(owner.Name).To(Equal(integrationStoreName))
	g.Expect(owner.UID).To(Equal(persisted.UID))
	g.Expect(ptr.Deref(owner.BlockOwnerDeletion, false)).To(BeTrue(),
		"the reference must block deletion so the Secret is collected with its store")
}
