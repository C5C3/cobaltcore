// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

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
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// The shared fixture coordinates. The Neutron, the OVNCentral it drives and the
// RabbitmqCluster it publishes on live in one namespace, which is the ordinary
// single-cluster control plane; the tests that need a second namespace or a
// second cluster set them explicitly.
const (
	testNamespace           = "openstack"
	testNeutronName         = "neutron"
	testOVNCentralName      = "ovn"
	testRabbitmqClusterName = "openstack-rabbitmq"
	testRabbitmqUserSecret  = "openstack-rabbitmq-default-user"
)

// The addresses the OVNCentral fixture publishes for its two databases.
const (
	testNorthboundAddress = "ssl:10.96.0.11:6641"
	testSouthboundAddress = "ssl:10.96.0.21:6642"
)

// openBaoClusterStoreName is the default effective ClusterSecretStore a Neutron
// selects when spec.secretStoreRef is omitted; the secrets sub-reconciler gates
// on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// testScheme registers the types the fake client resolves in this package's
// tests: core/apps (Secret, ConfigMap, Deployment), the Neutron API, the OVN API
// the two refs resolve to, the external-secrets v1 group the credential gate
// reads to attribute a missing Secret, and the Gateway API and MariaDB types the
// later steps project. It mirrors the envtest scheme in internal/testutil.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = neutronv1alpha1.AddToScheme(s)
	_ = ovnv1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	_ = gatewayv1.Install(s)
	_ = mariadbv1alpha1.AddToScheme(s)
	return s
}

// neutronFakeClientBuilder returns a fake client builder with the package scheme
// and the status subresources the reconciler reads and writes.
func neutronFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&neutronv1alpha1.Neutron{}, &ovnv1alpha1.OVNCentral{})
}

// newNeutronTestReconciler builds a NeutronReconciler over a fake client
// pre-loaded with objs. The Resolver stays nil, which is the always-local mode
// every single-cluster test runs in: the children land on the same fake client
// the CRs live on.
func newNeutronTestReconciler(objs ...client.Object) *NeutronReconciler {
	return &NeutronReconciler{
		Client:   neutronFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

// unresolvableResolver is a ClusterResolver that never knows any cluster. It
// returns the upstream sentinel so the test asserts the message an operator
// actually reads on the CR, not a locally invented string.
type unresolvableResolver struct{}

func (unresolvableResolver) GetCluster(_ context.Context, _ mcruntime.ClusterName) (cluster.Cluster, error) {
	return nil, mcruntime.ErrClusterNotFound
}

// validNeutron returns the shared Neutron fixture, carrying the values the
// defaulting webhook materializes. A CR the operator reads has been through
// admission, so a fixture that left the defaults off would exercise a spec no
// reconcile ever sees.
func validNeutron() *neutronv1alpha1.Neutron {
	return &neutronv1alpha1.Neutron{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testNeutronName,
			Namespace:  testNamespace,
			UID:        "neutron-uid",
			Generation: 1,
		},
		Spec: neutronv1alpha1.NeutronSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/neutron", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				Host:      "mariadb.example.com",
				Port:      3306,
				Database:  "neutron",
				SecretRef: commonv1.SecretRefSpec{Name: "neutron-db"},
			},
			Cache:            commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			KeystoneEndpoint: "http://keystone.openstack.svc:5000",
			ServiceUser: neutronv1alpha1.ServiceUserSpec{
				Username:          "neutron",
				ProjectName:       "service",
				UserDomainName:    "Default",
				ProjectDomainName: "Default",
				SecretRef:         commonv1.SecretRefSpec{Name: "neutron-service-user", Key: "password"},
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
				Replicas:   3,
			},
			OVN: neutronv1alpha1.OVNSpec{
				CentralRef: neutronv1alpha1.OVNCentralRef{Name: testOVNCentralName, Namespace: testNamespace},
			},
			Logging: &neutronv1alpha1.LoggingSpec{Format: "text", Level: "INFO", Debug: ptr.To(false)},
		},
	}
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

// neutronDBSecret returns the database credentials Secret referenced by
// validNeutron, carrying the username+password gate keys.
func neutronDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-db", Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("neutron"), "password": []byte("db-pw")},
	}
}

// neutronServiceUserSecret returns the service-user credentials Secret
// referenced by validNeutron, carrying the default "password" key with the given
// value.
func neutronServiceUserSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "neutron-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte(password)},
	}
}

// readyOVNCentral returns an OVNCentral publishing the two internal database
// addresses and the name of its client Secret, the status shape the endpoint
// step resolves from.
func readyOVNCentral(name, namespace, nbAddress, sbAddress, clientSecretName string) *ovnv1alpha1.OVNCentral {
	central := &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "ovn-central-uid"},
	}
	central.Status.Northbound.InternalDbAddress = nbAddress
	central.Status.Southbound.InternalDbAddress = sbAddress
	central.Status.ClientSecretName = clientSecretName
	return central
}

// ovnClientSecret returns the client identity an OVNCentral publishes: the
// keypair the ML2/OVN mechanism driver presents and the CA bundle it verifies
// the databases with.
func ovnClientSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"tls.crt": []byte("client-cert"),
			"tls.key": []byte("client-key"),
			"ca.crt":  []byte("ca-bundle"),
		},
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
// Operator writes, carrying the four halves of the transport URL.
func rabbitmqDefaultUserSecret(port string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRabbitmqUserSecret, Namespace: testNamespace},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"),
			"password": []byte("s3cr3t"),
			"host":     []byte("openstack-rabbitmq.openstack.svc"),
			"port":     []byte(port),
		},
	}
}

// neutronCondition returns one of the Neutron CR's conditions, or nil.
func neutronCondition(neutron *neutronv1alpha1.Neutron, conditionType string) *metav1.Condition {
	return conditions.GetCondition(neutron.Status.Conditions, conditionType)
}

// collectEvents drains the FakeRecorder channel non-blocking.
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
