// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the shared bus delivery (reconcileServiceMessaging,
// pruneServiceMessagingCA), which resolves the shared bus in the ControlPlane's
// own namespace and delivers it as a brownfield Secret into the namespace (and
// onto the cluster) the consumer runs in, on the Neutron target and on a second
// target.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

const (
	// neutronMsgBusCluster is the RabbitmqCluster the managed fixtures reference,
	// neutronMsgUserSecret the default-user Secret its status names.
	neutronMsgBusCluster = "openstack-rabbitmq"
	neutronMsgUserSecret = "openstack-rabbitmq-default-user" //nolint:gosec // G101 false positive: Secret name, not a credential.
	// neutronMsgURL is the URL the managed fixtures assemble from
	// neutronMsgDefaultUserSecret's four keys.
	neutronMsgURL = "rabbit://default_user_abc:s3cr3t@openstack-rabbitmq.openstack.svc:5672/"
)

func serviceMessagingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// neutronMessagingControlPlane builds a ControlPlane in the namespace "openstack"
// that declares a MANAGED bus and a co-located network service.
func neutronMessagingControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "openstack",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2025.2",
			Region:           "RegionOne",
			Infrastructure: &c5c3v1alpha1.InfrastructureSpec{
				Messaging: &commonv1.MessagingSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: neutronMsgBusCluster},
				},
			},
			Services: c5c3v1alpha1.ServicesSpec{
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn-central", Namespace: "ovn-system"},
					},
				},
			},
		},
	}
}

// placeNeutron moves the network service into a namespace of its own, and onto a
// target cluster when targetCluster is non-empty.
func placeNeutron(cp *c5c3v1alpha1.ControlPlane, namespace, targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp.Spec.Services.Neutron.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: namespace, Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	if targetCluster != "" {
		cp.Spec.Services.Neutron.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	}
	return cp
}

// neutronMsgRabbitmqCluster builds the unstructured RabbitmqCluster the managed
// flow reads. The kind is addressed unstructured, so it needs no scheme entry.
func neutronMsgRabbitmqCluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(messaging.RabbitmqClusterGVK)
	u.SetName(neutronMsgBusCluster)
	u.SetNamespace("openstack")
	if err := unstructured.SetNestedMap(u.Object,
		map[string]interface{}{"name": neutronMsgUserSecret}, "status", "defaultUser", "secretReference"); err != nil {
		panic(err)
	}
	return u
}

// neutronMsgDefaultUserSecret is the Secret the RabbitMQ Cluster Operator writes
// the default user's credentials and endpoint into.
func neutronMsgDefaultUserSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: neutronMsgUserSecret, Namespace: "openstack"},
		Data: map[string][]byte{
			"username": []byte("default_user_abc"),
			"password": []byte("s3cr3t"),
			"host":     []byte("openstack-rabbitmq.openstack.svc"),
			"port":     []byte("5672"),
		},
	}
}

// newServiceMessagingReconciler builds a reconciler on the management cluster
// alone; the tests that place the service hand in a resolver of their own.
func newServiceMessagingReconciler(t *testing.T, s *runtime.Scheme, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	return &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Scheme: s,
	}
}

// neutronMessagingCondition returns the NeutronReady condition, failing the test
// when it is absent: every halting arm has to leave one behind.
func neutronMessagingCondition(t *testing.T, cp *c5c3v1alpha1.ControlPlane) *metav1.Condition {
	t.Helper()
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)
	if cond == nil {
		t.Fatalf("reconcileServiceMessaging left no %s condition", conditionTypeNeutronReady)
	}
	return cond
}

// getMsgSecret reads a Secret through c, returning nil when it is absent.
func getMsgSecret(t *testing.T, c client.Client, namespace, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil
	}
	return secret
}

// TestReconcileServiceMessaging_ManagedBusWritesTheSecretWithAnOwnerReference
// covers the co-located default: the URL assembled from the RabbitmqCluster's
// default user lands beside the child under the ControlPlane's own name, owned by
// a controller reference so it is reaped with the plane.
func TestReconcileServiceMessaging_ManagedBusWritesTheSecretWithAnOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse(), "a delivered bus must let the projection run")
	g.Expect(res).To(Equal(ctrl.Result{}))

	secret := getMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil(), "the messaging Secret must exist beside the child")
	g.Expect(secret.Name).To(Equal("cp-neutron-messaging"))
	g.Expect(secret.Name).NotTo(Equal(messaging.TransportURLSecretName(neutronName(cp))),
		"the neutron operator claims its own derived name in this namespace")
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(neutronMsgURL))
	g.Expect(secret.OwnerReferences).To(HaveLen(1))
	g.Expect(secret.OwnerReferences[0].Name).To(Equal(cp.Name))
	g.Expect(secret.OwnerReferences[0].Controller).NotTo(BeNil())
	g.Expect(*secret.OwnerReferences[0].Controller).To(BeTrue())

	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)).To(BeNil(),
		"the delivery reports only failures; readiness belongs to the projection")
}

// TestReconcileServiceMessaging_BrownfieldBusCopiesTheURLVerbatim covers the bus
// an administrator runs outside the plane: the vhost, port and credentials the
// referenced Secret carries have to survive the hand-off untouched.
func TestReconcileServiceMessaging_BrownfieldBusCopiesTheURLVerbatim(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "external-bus", Key: "transport_url"},
	}
	const external = "rabbit://neutron:pw@broker.example.com:5673/openstack"
	bus := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bus", Namespace: "openstack"},
		Data:       map[string][]byte{"transport_url": []byte(external)},
	}
	r := newServiceMessagingReconciler(t, s, cp, bus)

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil())
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(external))
}

// TestReconcileServiceMessaging_WaitHaltsOnWaitingForMessagingCredentials covers
// the bus whose default-user Secret the RabbitMQ Cluster Operator has not written
// yet. Nothing may be delivered from half a credential, so the pass halts on the
// shared reason and the Neutron namespace stays empty.
func TestReconcileServiceMessaging_WaitHaltsOnWaitingForMessagingCredentials(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster())

	res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred(), "an unwritten default-user Secret is a wait, not a reconcile failure")
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(messaging.ReasonWaitingForMessagingCredentials))
	g.Expect(cond.Message).To(ContainSubstring("default-user Secret openstack/" + neutronMsgUserSecret))

	g.Expect(getMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))).To(BeNil(),
		"no Secret may be delivered while the credential is incomplete")
}

// TestReconcileServiceMessaging_NilBusHalts covers the caller-contract guard:
// this pass is only entered for a ControlPlane that declares a bus, so a missing
// block is a programming error rather than a state to wait out. The caller checks
// the same thing today, which is why the guard is unreachable through
// reconcileNeutron — it is called directly here so the arm keeps its verdict if
// that pre-check is ever relaxed or reordered.
func TestReconcileServiceMessaging_NilBusHalts(t *testing.T) {
	for name, drop := range map[string]func(cp *c5c3v1alpha1.ControlPlane){
		"nil infrastructure": func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure = nil },
		"nil messaging":      func(cp *c5c3v1alpha1.ControlPlane) { cp.Spec.Infrastructure.Messaging = nil },
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			s := serviceMessagingScheme(t)
			cp := neutronMessagingControlPlane()
			drop(cp)
			r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

			res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

			g.Expect(err).To(MatchError(ContainSubstring("spec.infrastructure.messaging is nil")))
			g.Expect(halt).To(BeTrue(), "nothing may be projected from a bus that was never declared")
			g.Expect(res).To(Equal(ctrl.Result{}), "an unreachable state does not converge on a requeue")

			cond := neutronMessagingCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))

			g.Expect(getMsgSecret(t, r.Client, "openstack", neutronMessagingSecretName(cp))).To(BeNil(),
				"no Secret may be written from a bus that was never declared")
		})
	}
}

// TestReconcileServiceMessaging_ResolveErrorSurfaces covers a bus block that named
// neither a cluster nor a Secret, which only a bypassed admission produces. It is
// not a state that converges on its own, so it surfaces as an error rather than a
// wait.
func TestReconcileServiceMessaging_ResolveErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
	r := newServiceMessagingReconciler(t, s, cp)

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolving the shared bus transport URL:"))
	g.Expect(halt).To(BeTrue())

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))
}

// TestReconcileServiceMessaging_UnresolvableClusterHaltsOnTargetClusterUnavailable
// covers the network service placed on a cluster that is not registered (yet).
// The Secret belongs on that cluster, so nothing is written anywhere and the pass
// parks on the reason every placed sub-reconciler shares.
func TestReconcileServiceMessaging_UnresolvableClusterHaltsOnTargetClusterUnavailable(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "remote-a")
	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())
	r.Resolver = &childrenResolver{children: remote, err: mcruntime.ErrClusterNotFound}

	res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out, not a reconcile failure")
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))

	for name, c := range map[string]client.Client{"management": r.Client, "target": remote} {
		g.Expect(getMsgSecret(t, c, "networking", neutronMessagingSecretName(cp))).To(BeNil(),
			"no Secret may be delivered on the %s cluster", name)
	}
}

// TestReconcileServiceMessaging_DedicatedNamespaceCarriesTheOwnershipLabels covers
// the service in a namespace of its own on the local cluster: Kubernetes forbids a
// cross-namespace controller reference, so the ownership labels are what mark the
// Secret as this plane's child.
func TestReconcileServiceMessaging_DedicatedNamespaceCarriesTheOwnershipLabels(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil())
	g.Expect(secret.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, cp.Name))
	g.Expect(secret.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, cp.Namespace))
	g.Expect(secret.OwnerReferences).To(BeEmpty(),
		"a controller reference across namespaces is rejected by the API server")
}

// TestReconcileServiceMessaging_TargetClusterCarriesTheOwnerTriple covers the
// placed service: the Secret is written on the cluster the Neutron pods run on,
// with the owner triple the shared teardown selects on, and nothing is left at
// home for a cluster that will never read it.
func TestReconcileServiceMessaging_TargetClusterCarriesTheOwnerTriple(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "remote-a")
	remote := fake.NewClientBuilder().WithScheme(s).Build()
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())
	r.Resolver = &childrenResolver{children: remote}

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	secret := getMsgSecret(t, remote, "networking", neutronMessagingSecretName(cp))
	g.Expect(secret).NotTo(BeNil(), "the bus must be delivered on the network service's own cluster")
	g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(neutronMsgURL))
	g.Expect(secret.Labels).To(Equal(remoteChildLabels(cp)))
	g.Expect(secret.OwnerReferences).To(BeEmpty(),
		"an owner reference on the target cluster names a UID that cluster cannot resolve")

	g.Expect(getMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))).To(BeNil(),
		"nothing may be left behind at home")
}

// TestReconcileServiceMessaging_RefusesAForeignSameNamedSecret covers the name
// taken by somebody else in a namespace the ControlPlane does not own. Adopting it
// would overwrite its data and get it deleted at teardown, so the write is refused
// and the refusal reaches the condition.
func TestReconcileServiceMessaging_RefusesAForeignSameNamedSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	// The UID is what refuseForeignAdoption reads as "this already exists".
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingSecretName(cp),
			Namespace: "networking",
			UID:       types.UID("foreign-secret-uid"),
		},
		Data: map[string][]byte{"transport_url": []byte("rabbit://someone:else@broker:5672/")},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), foreign)

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))
	g.Expect(halt).To(BeTrue())

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NeutronMessagingError"))
	g.Expect(cond.Message).To(ContainSubstring("refusing to adopt"))

	live := getMsgSecret(t, r.Client, "networking", neutronMessagingSecretName(cp))
	g.Expect(live).NotTo(BeNil())
	g.Expect(string(live.Data["transport_url"])).To(Equal("rabbit://someone:else@broker:5672/"),
		"the foreign Secret's data must survive untouched")
}

// TestReconcileServiceMessaging_TLSWritesTheCAMirror covers a bus the consumer
// verifies: the bundle lives beside the bus, the Neutron pods may be somewhere
// else entirely, so it is mirrored into their namespace under the key the
// projected messaging block names.
func TestReconcileServiceMessaging_TLSWritesTheCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	bundle := []byte("-----BEGIN CERTIFICATE-----\nbus\n-----END CERTIFICATE-----\n")
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: "openstack"},
		Data:       map[string][]byte{"ca.crt": bundle},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), busCA)

	_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	mirror := getMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))
	g.Expect(mirror).NotTo(BeNil())
	g.Expect(mirror.Name).To(Equal("cp-neutron-messaging-ca"))
	g.Expect(mirror.Data).To(HaveLen(1))
	g.Expect(mirror.Data).To(HaveKeyWithValue(serviceMessagingCAKey, bundle))
}

// TestReconcileServiceMessaging_TLSHaltsWhenTheCABundleSecretIsMissing covers the
// referenced bundle that has not been created yet. Delivering the URL without the
// trust anchor would leave the pods failing TLS on every connection, so the pass
// halts and names both halves of the reference.
func TestReconcileServiceMessaging_TLSHaltsWhenTheCABundleSecretIsMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca", Key: "ca.crt"},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

	res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForMessagingCABundle"))
	g.Expect(cond.Message).To(ContainSubstring("openstack/bus-ca"))
	g.Expect(cond.Message).To(ContainSubstring(`"ca.crt"`))

	g.Expect(getMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))).To(BeNil(),
		"no mirror may be written from an absent bundle")
}

// TestReconcileServiceMessaging_TLSHaltsWhenTheCABundleKeyIsMissing covers the
// Secret that exists but does not carry the key, the ordinary transient of a
// two-step create-then-populate flow. The ref leaves the key empty, so the pass
// also has to fall back on the default the webhooks materialize.
func TestReconcileServiceMessaging_TLSHaltsWhenTheCABundleKeyIsMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := neutronMessagingControlPlane()
	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca"},
	}
	busCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bus-ca", Namespace: "openstack"},
		Data:       map[string][]byte{"tls.crt": []byte("not the bundle")},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), busCA)

	res, halt, err := r.reconcileServiceMessaging(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))

	cond := neutronMessagingCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForMessagingCABundle"))
	g.Expect(cond.Message).To(ContainSubstring("openstack/bus-ca"))
	g.Expect(cond.Message).To(ContainSubstring(`"` + c5c3v1alpha1.DefaultCABundleSecretKey + `"`))

	g.Expect(getMsgSecret(t, r.Client, "openstack", neutronMessagingCASecretName(cp))).To(BeNil())
}

// TestPruneServiceMessagingCA_DeletesAStaleCAMirror covers dropping the tls block
// from a bus that had one: the mirror the earlier pass wrote is trust nobody reads
// any more, so it comes down instead of lingering in the Neutron namespace.
//
// The prune is its own entry point, not part of the messaging leg, because the
// live child still names the mirror until the projection several gates later
// rewrites it. reconcileNeutron calls this only on the far side of that apply.
func TestPruneServiceMessagingCA_DeletesAStaleCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingCASecretName(cp),
			Namespace: "networking",
			Labels: map[string]string{
				controlPlaneNameLabel:      cp.Name,
				controlPlaneNamespaceLabel: cp.Namespace,
			},
		},
		Data: map[string][]byte{serviceMessagingCAKey: []byte("stale bundle")},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), stale)

	_, halt, err := r.pruneServiceMessagingCA(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(getMsgSecret(t, r.Client, "networking", neutronMessagingCASecretName(cp))).To(BeNil(),
		"a mirror this ControlPlane wrote must not outlive the tls block")
}

// TestPruneServiceMessagingCA_LeavesAForeignCAMirror covers the same name held by
// somebody else. The cleanup deletes children, not neighbours, so an unowned
// Secret survives the pass untouched.
func TestPruneServiceMessagingCA_LeavesAForeignCAMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	s := serviceMessagingScheme(t)
	cp := placeNeutron(neutronMessagingControlPlane(), "networking", "")
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      neutronMessagingCASecretName(cp),
			Namespace: "networking",
			UID:       types.UID("foreign-ca-uid"),
		},
		Data: map[string][]byte{serviceMessagingCAKey: []byte("somebody else's bundle")},
	}
	r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret(), foreign)

	_, halt, err := r.pruneServiceMessagingCA(context.Background(), cp, neutronMessagingTarget(cp))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	live := getMsgSecret(t, r.Client, "networking", neutronMessagingCASecretName(cp))
	g.Expect(live).NotTo(BeNil(), "a Secret this ControlPlane never wrote must not be deleted")
	g.Expect(string(live.Data[serviceMessagingCAKey])).To(Equal("somebody else's bundle"))
}

// TestReconcileServiceMessaging_ASecondTargetLandsUnderItsOwnName covers the
// consumer that is not Neutron: the target alone decides the Secret name, the
// failure reason and the condition every arm reports on, so a second service
// reuses the delivery without touching the network service's verdict.
func TestReconcileServiceMessaging_ASecondTargetLandsUnderItsOwnName(t *testing.T) {
	cinder := serviceMessagingTarget{
		Service:       "Cinder",
		ChildName:     "cp-cinder",
		Namespace:     "openstack",
		ConditionType: "CinderReady",
	}

	t.Run("the delivery lands under the target's own name", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := serviceMessagingScheme(t)
		cp := neutronMessagingControlPlane()
		r := newServiceMessagingReconciler(t, s, cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret())

		_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, cinder)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())

		secret := getMsgSecret(t, r.Client, "openstack", "cp-cinder-messaging")
		g.Expect(secret).NotTo(BeNil(), "the bus must be delivered beside the second target's child")
		g.Expect(string(secret.Data[commonv1.DefaultTransportURLSecretKey])).To(Equal(neutronMsgURL))
		g.Expect(secret.OwnerReferences).To(HaveLen(1))
		g.Expect(secret.OwnerReferences[0].Name).To(Equal(cp.Name))
		g.Expect(secret.OwnerReferences[0].Controller).NotTo(BeNil())
		g.Expect(*secret.OwnerReferences[0].Controller).To(BeTrue())

		g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNeutronReady)).To(BeNil(),
			"a delivery for another consumer must leave NeutronReady alone")
	})

	t.Run("a failure reports on the target's own condition", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := serviceMessagingScheme(t)
		cp := neutronMessagingControlPlane()
		// A bus block naming neither a cluster nor a Secret, the same unconvergeable
		// state TestReconcileServiceMessaging_ResolveErrorSurfaces drives.
		cp.Spec.Infrastructure.Messaging = &commonv1.MessagingSpec{}
		r := newServiceMessagingReconciler(t, s, cp)

		_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, cinder)

		g.Expect(err).To(HaveOccurred())
		g.Expect(halt).To(BeTrue())

		cond := conditions.GetCondition(cp.Status.Conditions, "CinderReady")
		g.Expect(cond).NotTo(BeNil(), "every halting arm has to leave a condition behind")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("CinderMessagingError"))
	})

	t.Run("the wrapped write error names the lowercase service", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := serviceMessagingScheme(t)
		cp := neutronMessagingControlPlane()
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, neutronMsgRabbitmqCluster(), neutronMsgDefaultUserSecret()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(
					ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption,
				) error {
					if secret, ok := obj.(*corev1.Secret); ok && secret.Name == "cp-cinder-messaging" {
						return apierrors.NewInternalError(errors.New("boom"))
					}
					return cl.Create(ctx, obj, opts...)
				},
			}).Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, halt, err := r.reconcileServiceMessaging(context.Background(), cp, cinder)

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(HavePrefix("writing the cinder messaging Secret openstack/cp-cinder-messaging: "))

		cond := conditions.GetCondition(cp.Status.Conditions, "CinderReady")
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("CinderMessagingError"))
	})
}

// TestServiceMessagingSpec covers the spec.messaging the projected child
// receives: a brownfield secretRef naming the delivery, and a TLS block only
// while the shared bus declares one, so dropping tls reverts the child instead of
// pinning the last mirror.
func TestServiceMessagingSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronMessagingControlPlane()
	target := neutronMessagingTarget(cp)

	g.Expect(serviceMessagingSpec(cp, target)).To(Equal(commonv1.MessagingSpec{
		SecretRef: &commonv1.SecretRefSpec{Name: "cp-neutron-messaging", Key: "transport_url"},
	}), "a plaintext bus leaves the child naming no trust anchor")

	cp.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca"},
	}

	spec := serviceMessagingSpec(cp, target)
	g.Expect(spec.TLS).NotTo(BeNil())
	g.Expect(spec.TLS.CABundleSecretRef).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-neutron-messaging-ca", Key: "ca.crt",
	}), "the child names the mirror beside it, not the bundle beside the bus")
}

// TestMessagingCAMirrorReleasable covers the reap gate. The mirror is a volume
// source the live workload still mounts, so it may only come down once nothing
// names it: the bus dropped its tls block, the applied child carries no TLS block
// either, and the child has reconciled the generation that apply produced.
func TestMessagingCAMirrorReleasable(t *testing.T) {
	noInfrastructure := neutronMessagingControlPlane()
	noInfrastructure.Spec.Infrastructure = nil

	busWithTLS := neutronMessagingControlPlane()
	busWithTLS.Spec.Infrastructure.Messaging.TLS = &commonv1.MessagingTLSSpec{
		CABundleSecretRef: commonv1.SecretRefSpec{Name: "bus-ca"},
	}

	childNamesTheMirror := commonv1.MessagingSpec{
		TLS: &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{
				Name: "cp-neutron-messaging-ca", Key: serviceMessagingCAKey,
			},
		},
	}

	for name, tc := range map[string]struct {
		cp                 *c5c3v1alpha1.ControlPlane
		child              commonv1.MessagingSpec
		observedGeneration int64
		generation         int64
		want               bool
	}{
		"no infrastructure block": {
			cp: noInfrastructure, observedGeneration: 2, generation: 2,
		},
		"the bus still declares tls": {
			cp: busWithTLS, observedGeneration: 2, generation: 2,
		},
		"the child still names the mirror": {
			cp: neutronMessagingControlPlane(), child: childNamesTheMirror, observedGeneration: 2, generation: 2,
		},
		"the child lags the apply": {
			cp: neutronMessagingControlPlane(), observedGeneration: 1, generation: 2,
		},
		"nothing names the mirror any more": {
			cp: neutronMessagingControlPlane(), observedGeneration: 2, generation: 2, want: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(messagingCAMirrorReleasable(tc.cp, tc.child, tc.observedGeneration, tc.generation)).
				To(Equal(tc.want))
		})
	}
}

// TestServiceMessagingSecrets covers the stubs both teardown paths delete: the
// transport-URL Secret and the CA mirror, in the namespace the consumer runs in,
// so a placed service leaves no broker credential behind either.
func TestServiceMessagingSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := neutronMessagingControlPlane()

	stubs := serviceMessagingSecrets(neutronMessagingTarget(cp))
	g.Expect(stubs).To(HaveLen(2))
	for i, want := range []string{"cp-neutron-messaging", "cp-neutron-messaging-ca"} {
		secret, ok := stubs[i].(*corev1.Secret)
		g.Expect(ok).To(BeTrue(), "the teardown deletes Secrets, not %T", stubs[i])
		g.Expect(secret.Name).To(Equal(want))
		g.Expect(secret.Namespace).To(Equal(cp.NeutronNamespace()))
	}

	placed := serviceMessagingSecrets(neutronMessagingTarget(placeNeutron(cp, "network", "")))
	for _, stub := range placed {
		g.Expect(stub.GetNamespace()).To(Equal("network"),
			"a placed service is swept in the namespace it runs in")
	}
}
