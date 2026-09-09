// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/secrets"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// The shared fixture coordinates. The Cinder, the backends attached to it and
// the RabbitmqCluster it publishes on live in one namespace, which is the
// ordinary single-cluster control plane.
const (
	testNamespace           = "openstack"
	testCinderName          = "cinder"
	testRabbitmqClusterName = "openstack-rabbitmq"
	testRabbitmqUserSecret  = "openstack-rabbitmq-default-user"
)

// openBaoClusterStoreName is the default effective ClusterSecretStore a Cinder
// selects when spec.secretStoreRef is omitted; the secrets sub-reconciler gates
// on its Ready condition.
const openBaoClusterStoreName = secrets.OpenBaoClusterStoreName

// testScheme registers the types the fake client resolves in this package's
// tests: core (Secret), the Cinder API, and the external-secrets v1 group the
// credential gate reads to attribute a missing Secret.
func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = cinderv1alpha1.AddToScheme(s)
	_ = esov1.SchemeBuilder.AddToScheme(s)
	return s
}

// cinderFakeClientBuilder returns a fake client builder with the package scheme
// and the status subresource the Cinder reconciler writes.
func cinderFakeClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&cinderv1alpha1.Cinder{})
}

// newCinderTestReconciler builds a CinderReconciler over a fake client
// pre-loaded with objs. The Resolver stays nil, which is the always-local mode
// every single-cluster test runs in: the children land on the same fake client
// the CRs live on.
func newCinderTestReconciler(objs ...client.Object) *CinderReconciler {
	return &CinderReconciler{
		Client:   cinderFakeClientBuilder(objs...).Build(),
		Scheme:   testScheme(),
		Recorder: record.NewFakeRecorder(50),
	}
}

// validCinder returns the shared Cinder fixture, carrying the values the
// defaulting webhook materializes. A CR the operator reads has been through
// admission, so a fixture that left the defaults off would exercise a spec no
// reconcile ever sees.
func validCinder() *cinderv1alpha1.Cinder {
	return &cinderv1alpha1.Cinder{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testCinderName,
			Namespace:  testNamespace,
			UID:        "cinder-uid",
			Generation: 1,
		},
		Spec: cinderv1alpha1.CinderSpec{
			OpenStackRelease: "2026.1",
			Image:            commonv1.ImageSpec{Repository: "ghcr.io/c5c3/cinder", Tag: "2026.1"},
			Database: commonv1.DatabaseSpec{
				Host:      "mariadb.example.com",
				Port:      3306,
				Database:  "cinder",
				SecretRef: commonv1.SecretRefSpec{Name: "cinder-db"},
			},
			Cache:            commonv1.CacheSpec{Backend: "dogpile.cache.pymemcache", Servers: []string{"mc:11211"}},
			KeystoneEndpoint: "http://keystone.openstack.svc:5000",
			ServiceUser: &cinderv1alpha1.ServiceUserSpec{
				Username:          "cinder",
				ProjectName:       "service",
				UserDomainName:    "Default",
				ProjectDomainName: "Default",
				SecretRef:         commonv1.SecretRefSpec{Name: "cinder-service-user", Key: "password"},
			},
			Messaging: commonv1.MessagingSpec{
				ClusterRef: &corev1.LocalObjectReference{Name: testRabbitmqClusterName},
				Replicas:   3,
			},
			Logging: &cinderv1alpha1.LoggingSpec{Format: "text", Level: "INFO", Debug: ptr.To(false)},
		},
	}
}

// keystoneFreeCinder returns the Cinder fixture without the Keystone
// integration: the CEL pairing rule keeps spec.keystoneEndpoint and
// spec.serviceUser together, so dropping one drops the other. It is the shape
// the operator's own storage suites deploy on a cluster that runs no identity
// service.
func keystoneFreeCinder() *cinderv1alpha1.Cinder {
	cinder := validCinder()
	cinder.Spec.KeystoneEndpoint = ""
	cinder.Spec.ServiceUser = nil
	return cinder
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

// cinderDBSecret returns the database credentials Secret referenced by
// validCinder, carrying the username+password gate keys.
func cinderDBSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cinder-db", Namespace: testNamespace},
		Data:       map[string][]byte{"username": []byte("cinder"), "password": []byte("db-pw")},
	}
}

// cinderServiceUserSecret returns the service-user credentials Secret referenced
// by validCinder, carrying the default "password" key with the given value.
func cinderServiceUserSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cinder-service-user", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte(password)},
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
		"host":     []byte("openstack-rabbitmq.openstack.svc"),
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

// cinderCondition returns one of the Cinder CR's conditions, or nil.
func cinderCondition(cinder *cinderv1alpha1.Cinder, conditionType string) *metav1.Condition {
	return conditions.GetCondition(cinder.Status.Conditions, conditionType)
}
