// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// The shared fixture coordinates. The Nova, the MariaDB holding its two schemas
// and the RabbitmqCluster it publishes on live in one namespace, which is the
// ordinary single-cluster control plane.
const (
	testNamespace           = "openstack"
	testNovaName            = "nova"
	testMariaDBName         = "mariadb"
	testMemcachedName       = "memcached"
	testRabbitmqClusterName = "rabbitmq"
	testRabbitmqUserSecret  = "rabbitmq-default-user"
)

// The Secrets validNova references, one per credential gate.
const (
	testAPIDBSecret       = "nova-api-db"
	testCellDBSecret      = "nova-db"
	testServiceUserSecret = "nova-service-user"
	testSharedSecret      = "nova-metadata-secret"
	testMessagingCASecret = "nova-messaging-ca"
)

// openBaoClusterStoreName is the default effective ClusterSecretStore a Nova
// selects when spec.secretStoreRef is omitted; the secrets sub-reconciler gates
// on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// testScheme registers the types the fake client resolves in this package's
// tests: core/apps/batch/policy/autoscaling/networking via the client-go scheme,
// the Nova API, the external-secrets v1 group the credential gate reads to
// attribute a missing Secret, the Gateway API the route steps project, and the
// MariaDB group the database step provisions and the deletion path finalizes.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = novav1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// novaFakeClientBuilder returns a fake client builder with the package scheme,
// the status subresource both reconcilers write, the Secret-name and novaRef
// field indexes the watch mappers and the NovaCompute steps resolve against,
// and the spec.nodeName pod index the API server provides as a field selector.
func novaFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&novav1alpha1.Nova{}, &novav1alpha1.NovaCompute{}).
		WithIndex(&novav1alpha1.Nova{}, NovaSecretNameIndexKey, novaSecretNameExtractor).
		WithIndex(&novav1alpha1.NovaCompute{}, NovaComputeNovaRefIndexKey, novaComputeNovaRefExtractor).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		})
}

// newNovaTestReconciler builds a NovaReconciler over a fake client pre-loaded
// with objs. The Resolver stays nil, which is the always-local mode every
// single-cluster test runs in: the children land on the same fake client the CRs
// live on.
func newNovaTestReconciler(objs ...client.Object) *NovaReconciler {
	return &NovaReconciler{
		Client:   novaFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

// validNova returns the shared Nova fixture, run through the real defaulting
// webhook. A CR the operator reads has been through admission, so a fixture that
// left the defaults off would exercise a spec no reconcile ever sees, and
// running the defaulter rather than restating its output keeps the workload
// blocks (one replica on the three non-API Deployments, the raised RPC
// termination window, the two uWSGI blocks) in step with it.
func validNova() *novav1alpha1.Nova {
	return defaulted(&novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testNovaName,
			Namespace:  testNamespace,
			UID:        "nova-uid",
			Generation: 1,
		},
		Spec: novav1alpha1.NovaSpec{
			OpenStackRelease: "2025.2",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/nova", Tag: "2025.2"},
			APIDatabase: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMariaDBName},
				Database:   "nova_api",
				SecretRef:  commonv1.SecretRefSpec{Name: testAPIDBSecret},
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMariaDBName},
				Database:   "nova",
				SecretRef:  commonv1.SecretRefSpec{Name: testCellDBSecret},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMemcachedName},
				Backend:    commonv1.DefaultCacheBackend,
				Replicas:   3,
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
				Replicas:   3,
			},
			KeystoneEndpoint:       "http://keystone.openstack.svc.cluster.local:5000",
			KeystonePublicEndpoint: "https://keystone.example.com",
			ServiceUser: novav1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: testServiceUserSecret},
			},
			Region: "RegionOne",
			Metadata: novav1alpha1.NovaMetadataSpec{
				SharedSecretRef: commonv1.SecretRefSpec{Name: testSharedSecret},
			},
			Endpoints: novav1alpha1.NovaEndpointsSpec{
				Cinder:   novav1alpha1.NovaOptionalEndpointSpec{Enabled: true},
				Barbican: novav1alpha1.NovaOptionalEndpointSpec{Enabled: true},
			},
			ConsoleProxy: novav1alpha1.NovaConsoleProxySpec{
				Gateway: &novav1alpha1.GatewaySpec{
					Hostname:  "console.example.com",
					ParentRef: novav1alpha1.GatewayParentRefSpec{Name: "gw", Namespace: "gateway-system"},
				},
			},
			Gateway: &novav1alpha1.GatewaySpec{
				Hostname:  "nova.example.com",
				ParentRef: novav1alpha1.GatewayParentRefSpec{Name: "gw", Namespace: "gateway-system"},
			},
			Logging: &novav1alpha1.LoggingSpec{Format: "json", Level: "INFO", Debug: ptr.To(false)},
		},
	})
}

// novaMinimal returns the smallest Nova a user can submit: both optional client
// integrations off, no gateways, no region, no logging block and no public
// Keystone endpoint. It is deliberately NOT run through the defaulting webhook,
// so its two Secret keys stay empty and the effective-key fallbacks are
// exercised on the shape a CR that bypassed admission has.
func novaMinimal() *novav1alpha1.Nova {
	return &novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testNovaName,
			Namespace:  testNamespace,
			UID:        "nova-uid",
			Generation: 1,
		},
		Spec: novav1alpha1.NovaSpec{
			OpenStackRelease: "2025.2",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/nova", Tag: "2025.2"},
			APIDatabase: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMariaDBName},
				Database:   "nova_api",
				SecretRef:  commonv1.SecretRefSpec{Name: testAPIDBSecret},
			},
			Database: commonv1.DatabaseSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMariaDBName},
				Database:   "nova",
				SecretRef:  commonv1.SecretRefSpec{Name: testCellDBSecret},
			},
			Cache: commonv1.CacheSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testMemcachedName},
				Backend:    commonv1.DefaultCacheBackend,
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
			},
			KeystoneEndpoint: "http://keystone.openstack.svc.cluster.local:5000",
			ServiceUser: novav1alpha1.ServiceUserSpec{
				SecretRef: commonv1.SecretRefSpec{Name: testServiceUserSecret},
			},
			Metadata: novav1alpha1.NovaMetadataSpec{
				SharedSecretRef: commonv1.SecretRefSpec{Name: testSharedSecret},
			},
		},
	}
}

// novaWithMessagingTLS returns the shared fixture pointed at a bus it verifies:
// spec.messaging.tls names the Secret carrying the broker's CA bundle, which is
// what adds the fifth credential gate.
func novaWithMessagingTLS() *novav1alpha1.Nova {
	nova := validNova()
	nova.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: testMessagingCASecret, Key: "ca.crt"},
	}
	return nova
}

// defaulted runs the real defaulting webhook over a fixture, so the tests
// describe a CR admission can actually produce instead of restating the
// webhook's output beside it.
func defaulted(nova *novav1alpha1.Nova) *novav1alpha1.Nova {
	if err := (&novav1alpha1.NovaWebhook{}).Default(context.Background(), nova); err != nil {
		panic("defaulting the Nova fixture: " + err.Error())
	}
	return nova
}

// readyClusterSecretStore returns a ClusterSecretStore with Ready=True so the
// secrets sub-reconciler proceeds past the store gate.
func readyClusterSecretStore(name string) *esov1.ClusterSecretStore {
	return &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// notReadyClusterSecretStore returns a ClusterSecretStore whose Ready condition
// is explicitly False so the secrets sub-reconciler flips SecretsReady=False
// with reason SecretStoreNotReady.
func notReadyClusterSecretStore(name string) *esov1.ClusterSecretStore {
	return &esov1.ClusterSecretStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: esov1.SecretStoreStatus{
			Conditions: []esov1.SecretStoreStatusCondition{
				{Type: esov1.SecretStoreReady, Status: corev1.ConditionFalse},
			},
		},
	}
}

// novaAPIDBSecret returns the nova_api database credentials Secret referenced by
// validNova, carrying the username+password gate keys.
func novaAPIDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testAPIDBSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("nova-api"), "password": []byte("api-db-pw")},
	}
}

// novaCellDBSecret returns the cell database credentials Secret referenced by
// validNova, carrying the username+password gate keys.
func novaCellDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCellDBSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("nova"), "password": []byte("cell-db-pw")},
	}
}

// novaServiceUserSecret returns the service-user credentials Secret referenced
// by validNova, carrying the default "password" key with the given value.
func novaServiceUserSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceUserSecret, Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte(password)},
	}
}

// novaSharedSecret returns the metadata shared-secret Secret referenced by
// validNova, carrying the default "shared_secret" key with the given value.
func novaSharedSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSharedSecret, Namespace: testNamespace},
		Data: map[string][]byte{
			novav1alpha1.DefaultSharedSecretKey: []byte(value),
		},
	}
}

// novaMessagingCASecret returns the broker CA bundle Secret a TLS bus
// references, carrying the "ca.crt" key the gate expects.
func novaMessagingCASecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testMessagingCASecret, Namespace: testNamespace},
		Data:       map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----")},
	}
}

// rabbitmqCluster builds the unstructured RabbitmqCluster the managed messaging
// flow reads. The kind is addressed unstructured, so no scheme registration is
// involved, which is exactly how the operator reads it.
func rabbitmqCluster(name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// rabbitmqDefaultUserSecret returns the default-user Secret the RabbitMQ Cluster
// Operator writes, carrying the four halves of the transport URL. A key named in
// omit is left out so a test can exercise the incomplete-Secret path.
func rabbitmqDefaultUserSecret(port string, omit ...string) *corev1.Secret {
	data := map[string][]byte{
		"username": []byte("default_user_abc"),
		"password": []byte("s3cr3t"),
		"host":     []byte("rabbitmq.openstack.svc"),
		"port":     []byte(port),
	}
	for _, key := range omit {
		delete(data, key)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRabbitmqUserSecret, Namespace: testNamespace},
		Data:       data,
	}
}

// novaCondition returns one of the Nova CR's conditions, or nil.
func novaCondition(nova *novav1alpha1.Nova, conditionType string) *metav1.Condition {
	return conditions.GetCondition(nova.Status.Conditions, conditionType)
}

// collectEvents drains a FakeRecorder and returns the events it buffered, so a
// test can assert on the whole set rather than on the first one off the channel.
func collectEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// --- NovaCompute fixtures ---

// The NovaCompute fixture coordinates: one pool selecting one node in one zone.
const (
	testPoolName   = "pool-a"
	testPoolLabel  = "openstack.c5c3.io/nova-compute-pool"
	testNodeName   = "node-1"
	testZone       = "az1"
	testContract   = testNovaName + "-" + componentComputeConfig
	testPoolMarker = testNamespace + "/" + testNovaName
)

// testPoolCreated is the creation time of the pool fixture; rivals are made
// older or younger than it.
var testPoolCreated = metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))

// validNovaCompute returns a NovaCompute joining validNova with a one-label
// selector, in the shape an admitted CR has.
func validNovaCompute() *novav1alpha1.NovaCompute {
	return &novav1alpha1.NovaCompute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testPoolName,
			Namespace:         testNamespace,
			UID:               "pool-a-uid",
			Generation:        1,
			CreationTimestamp: testPoolCreated,
		},
		Spec: novav1alpha1.NovaComputeSpec{
			NovaRef:        novav1alpha1.NovaRef{Name: testNovaName},
			NodeSelector:   map[string]string{testPoolLabel: "a"},
			Libvirt:        novav1alpha1.NovaComputeLibvirtSpec{VirtType: "kvm"},
			UpdateStrategy: novav1alpha1.NovaComputeUpdateStrategy{Type: "RollingUpdate"},
		},
	}
}

// readyNovaForCompute returns validNova as a pool finds it once the control
// plane is up: a release installed and the compute contract published.
func readyNovaForCompute() *novav1alpha1.Nova {
	nova := validNova()
	nova.Status.InstalledRelease = "2025.2"
	nova.Status.ComputeConfigSecretRef = &corev1.LocalObjectReference{Name: testContract}
	return nova
}

// computeContractSecret returns the compute contract readyNovaForCompute
// publishes, carrying the given service password.
func computeContractSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testContract, Namespace: testNamespace},
		Data: map[string][]byte{
			computeConfigFragmentKey: []byte("[DEFAULT]\nuse_stderr = true\n"),
			transportURLKey:          []byte("rabbit://nova:secret@rabbitmq.openstack.svc:5672/"),
			passwordKey:              []byte(password),
			cellNameKey:              []byte(computeCellName),
		},
	}
}

// poolNode returns a Node carrying the given labels.
func poolNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// selectedNode returns a node the pool fixture selects, in testZone.
func selectedNode(name string) *corev1.Node {
	return poolNode(name, map[string]string{testPoolLabel: "a", zoneLabel: testZone})
}

// newNovaComputeTestReconciler returns a reconciler on a fake client holding
// objs, talking to api for Keystone and Nova.
func newNovaComputeTestReconciler(api *computeapitest.Fake, objs ...client.Object) *NovaComputeReconciler {
	c := novaFakeClientBuilder(objs...).Build()
	return &NovaComputeReconciler{
		Client:     c,
		Scheme:     c.Scheme(),
		Recorder:   record.NewFakeRecorder(100),
		HTTPClient: api,
	}
}

// novaComputeCondition returns one of the NovaCompute's conditions, or nil.
func novaComputeCondition(cr *novav1alpha1.NovaCompute, conditionType string) *metav1.Condition {
	return conditions.GetCondition(cr.Status.Conditions, conditionType)
}
