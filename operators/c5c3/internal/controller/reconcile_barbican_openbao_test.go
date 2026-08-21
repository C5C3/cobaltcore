// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the dedicated Barbican OpenBao instance ensemble.
package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	openbaov1alpha1 "github.com/dc-tec/openbao-operator/api/v1alpha1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// barbicanOpenBaoTestNamespace is the Barbican service namespace the fixtures
// place the service in. It is deliberately NOT the ControlPlane's own namespace,
// so every projection takes the cross-namespace branch: ownership labels instead
// of owner references, and a foreign same-named object refused rather than
// adopted.
const barbicanOpenBaoTestNamespace = "barbican"

// barbicanOpenBaoTestScheme registers client-go, c5c3, and the openbao-operator
// types. The two cert-manager Certificates need no registration: they are
// unstructured and carry their own GVK.
func barbicanOpenBaoTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	if err := openbaov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding openbao scheme: %v", err)
	}
	return s
}

// barbicanOpenBaoControlPlane builds a ControlPlane whose Barbican service runs in
// its own namespace and declares a dedicated secret store.
func barbicanOpenBaoControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cp",
			Namespace:  "default",
			Generation: 1,
			UID:        types.UID("cp-uid"),
		},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Region:           "RegionOne",
			Services: c5c3v1alpha1.ServicesSpec{
				Barbican: &c5c3v1alpha1.ServiceBarbicanSpec{
					Namespace: &c5c3v1alpha1.ServiceNamespaceSpec{Name: barbicanOpenBaoTestNamespace},
					SecretStore: c5c3v1alpha1.ServiceBarbicanSecretStoreSpec{
						Dedicated: &c5c3v1alpha1.BarbicanDedicatedSecretStoreSpec{},
					},
				},
			},
		},
	}
}

// defaultKubernetesEndpointSlice builds the EndpointSlice kube-apiserver publishes
// for itself under a well-known name. Seeding it is what makes a fake client
// resemble the cluster the reconciler actually runs against: every conformant
// cluster carries one, and resolveAPIServerEndpointIPs reads it on every dedicated
// secret-store pass.
//
// With no addresses given it carries one, which is the single-control-plane shape
// of a kind cluster.
func defaultKubernetesEndpointSlice(addresses ...string) *discoveryv1.EndpointSlice {
	if len(addresses) == 0 {
		addresses = []string{"172.18.0.2"}
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiServerEndpointSliceName,
			Namespace: apiServerEndpointSliceNamespace,
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: addresses}},
	}
}

// seedAPIServerEndpointSlice appends the API server's EndpointSlice unless the
// caller already supplied one, so a test that pins particular addresses keeps
// control of them without the fake client rejecting a duplicate key.
func seedAPIServerEndpointSlice(objs []client.Object) []client.Object {
	for _, o := range objs {
		if _, ok := o.(*discoveryv1.EndpointSlice); ok {
			return objs
		}
	}
	return append(objs, defaultKubernetesEndpointSlice())
}

// barbicanOpenBaoReconciler builds a reconciler over a fake client seeded with cp,
// objs, and the API server's EndpointSlice.
func barbicanOpenBaoReconciler(t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	return barbicanOpenBaoReconcilerWithExactSeeds(t, cp, seedAPIServerEndpointSlice(objs)...)
}

// barbicanOpenBaoReconcilerWithExactSeeds seeds exactly what it is given, so a test
// can model the cluster resolveAPIServerEndpointIPs must refuse to project against:
// one with no API-server EndpointSlice at all.
func barbicanOpenBaoReconcilerWithExactSeeds(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object,
) *ControlPlaneReconciler {
	t.Helper()
	s := barbicanOpenBaoTestScheme(t)
	seeded := append([]client.Object{cp}, objs...)
	return &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &openbaov1alpha1.OpenBaoCluster{}).Build(),
		Scheme: s,
	}
}

// getBarbicanOpenBaoCluster reads the projected instance.
func getBarbicanOpenBaoCluster(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) *openbaov1alpha1.OpenBaoCluster {
	t.Helper()
	instance := &openbaov1alpha1.OpenBaoCluster{}
	key := types.NamespacedName{Namespace: cp.BarbicanNamespace(), Name: barbicanOpenBaoName(cp)}
	if err := r.Get(context.Background(), key, instance); err != nil {
		t.Fatalf("reading projected OpenBaoCluster %s: %v", key, err)
	}
	return instance
}

// getBarbicanOpenBaoCertificate reads one of the two projected Certificates.
func getBarbicanOpenBaoCertificate(t *testing.T, r *ControlPlaneReconciler, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cert); err != nil {
		t.Fatalf("reading projected Certificate %s/%s: %v", namespace, name, err)
	}
	return cert
}

// TestEnsureBarbicanOpenBao_UnsealKeyGeneratedOnceAndOwnedByInstance pins the two
// properties the static seal depends on. The key is generated exactly once: a
// static-seal key that changes seals the instance permanently, so a second pass
// must read the existing value rather than mint a fresh one. And once the instance
// exists the Secret carries its controller ownerReference — the ownership proof
// the openbao-operator adopts a pre-existing unseal Secret against, and the reason
// the two are garbage-collected together.
func TestEnsureBarbicanOpenBao_UnsealKeyGeneratedOnceAndOwnedByInstance(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	key := types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace,
		Name:      barbicanOpenBaoName(cp) + barbicanOpenBaoUnsealSecretSuffix,
	}
	first := &corev1.Secret{}
	g.Expect(r.Get(context.Background(), key, first)).To(Succeed())
	raw, derr := base64.StdEncoding.DecodeString(string(first.Data[barbicanOpenBaoUnsealSecretKey]))
	g.Expect(derr).NotTo(HaveOccurred(), "the seal key must be base64 the operator can decode")
	g.Expect(raw).To(HaveLen(32))

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	owner := metav1.GetControllerOf(first)
	g.Expect(owner).NotTo(BeNil(), "the unseal Secret must be owned by the instance it seals")
	g.Expect(owner.Kind).To(Equal("OpenBaoCluster"))
	g.Expect(owner.Name).To(Equal(instance.Name))
	g.Expect(isControlPlaneChild(first, cp)).To(BeTrue(),
		"the Secret must also carry the ownership labels the teardown finds it by")

	_, err = r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	second := &corev1.Secret{}
	g.Expect(r.Get(context.Background(), key, second)).To(Succeed())
	g.Expect(second.Data[barbicanOpenBaoUnsealSecretKey]).To(Equal(first.Data[barbicanOpenBaoUnsealSecretKey]),
		"regenerating the seal key would seal the instance permanently")
}

// TestEnsureBarbicanOpenBao_UnsealKeyRandomnessFailure covers the path where the
// randomness source fails. Nothing recovers from an unseal key that was never
// generated, so the ensure aborts with the failure named rather than writing an
// empty-keyed Secret the instance would later adopt.
func TestEnsureBarbicanOpenBao_UnsealKeyRandomnessFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	original := barbicanUnsealKeyRandRead
	t.Cleanup(func() { barbicanUnsealKeyRandRead = original })
	barbicanUnsealKeyRandRead = func([]byte) (int, error) { return 0, errors.New("entropy pool drained") }

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("generating barbican unseal key"))
	g.Expect(err.Error()).To(ContainSubstring("entropy pool drained"))

	instance := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Namespace: barbicanOpenBaoTestNamespace, Name: barbicanOpenBaoName(cp)},
		instance)).NotTo(Succeed(), "no instance may be created without a seal key")
}

// TestEnsureBarbicanOpenBao_ProjectsInstanceSpec pins the posture of the projected
// instance: the pinned OpenBao version, the single-replica Development profile,
// External TLS against the two cert-manager Secrets, the static seal, and
// DeletePVCs — without which a re-created ControlPlane of the same name inherits
// raft storage initialised under a seal key that no longer exists.
func TestEnsureBarbicanOpenBao_ProjectsInstanceSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Name).To(Equal("cp-barbican-bao"))
	g.Expect(instance.Spec.Version).To(Equal("2.6.1"))
	g.Expect(instance.Spec.Profile).To(Equal(openbaov1alpha1.ProfileDevelopment))
	g.Expect(instance.Spec.Replicas).To(Equal(int32(1)))
	g.Expect(instance.Spec.Storage.Size).To(Equal("1Gi"))
	g.Expect(instance.Spec.TLS.Enabled).To(BeTrue())
	g.Expect(instance.Spec.TLS.Mode).To(Equal(openbaov1alpha1.TLSModeExternal))
	g.Expect(instance.Spec.Unseal).NotTo(BeNil())
	g.Expect(instance.Spec.Unseal.Type).To(Equal("static"))
	g.Expect(instance.Spec.DeletionPolicy).To(Equal(openbaov1alpha1.DeletionPolicyDeletePVCs))
	g.Expect(isControlPlaneChild(instance, cp)).To(BeTrue())
}

// TestEnsureBarbicanOpenBao_PinsIngressPeers asserts the NetworkPolicy allowlist
// names exactly two sources and neither of them with a wildcard namespace
// selector. A wildcard peer would admit any pod carrying the label from any
// namespace, and the operator's default-deny policy is the only thing standing
// between the instance's API port and the rest of the cluster.
func TestEnsureBarbicanOpenBao_PinsIngressPeers(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.Network).NotTo(BeNil())
	peers := instance.Spec.Network.TrustedIngressPeers
	g.Expect(peers).To(HaveLen(2))

	for i, peer := range peers {
		g.Expect(peer.NamespaceSelector).NotTo(BeNil(), "peer %d must pin a namespace", i)
		g.Expect(peer.NamespaceSelector.MatchLabels).NotTo(BeEmpty(),
			"peer %d must not select every namespace", i)
		g.Expect(peer.PodSelector).NotTo(BeNil(), "peer %d must pin a pod label", i)
	}
	g.Expect(peers[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", defaultBarbicanOperatorNamespace))
	g.Expect(peers[0].PodSelector.MatchLabels).To(
		HaveKeyWithValue("app.kubernetes.io/name", "barbican-operator"))
	g.Expect(peers[1].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", barbicanOpenBaoTestNamespace))
	g.Expect(peers[1].PodSelector.MatchLabels).To(
		HaveKeyWithValue("app.kubernetes.io/name", "barbican"))
}

// TestEnsureBarbicanOpenBao_ProjectsAPIServerEndpointIPs asserts the API server's
// endpoint addresses reach the instance's egress allowlist, deduplicated and
// sorted. Without them the operator-rendered NetworkPolicy allows only the
// in-cluster service VIP on port 443, which a CNI enforcing egress against the
// post-DNAT destination never matches, and the instance loses the API server.
//
// The sort is load-bearing rather than cosmetic: the projection is compared
// against the live spec on every pass, and the API server guarantees no endpoint
// order, so an unsorted list would read as drift and rewrite the instance forever.
func TestEnsureBarbicanOpenBao_ProjectsAPIServerEndpointIPs(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	// Deliberately unsorted, and with one address repeated across two endpoints.
	r := barbicanOpenBaoReconciler(t, cp, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiServerEndpointSliceName,
			Namespace: apiServerEndpointSliceNamespace,
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"172.18.0.4", "172.18.0.2"}},
			{Addresses: []string{"172.18.0.3", "172.18.0.2"}},
		},
	})

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.Network).NotTo(BeNil())
	g.Expect(instance.Spec.Network.APIServerEndpointIPs).To(
		Equal([]string{"172.18.0.2", "172.18.0.3", "172.18.0.4"}))
	// The egress allowance is additive to the ingress allowlist, never a
	// replacement for it.
	g.Expect(instance.Spec.Network.TrustedIngressPeers).To(HaveLen(2))
}

// TestEnsureBarbicanOpenBao_TracksAPIServerEndpointDrift asserts a live instance is
// corrected when the API server's endpoint addresses change, which is what happens
// when a kind cluster is re-created under a control plane of the same name.
func TestEnsureBarbicanOpenBao_TracksAPIServerEndpointDrift(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp, defaultKubernetesEndpointSlice("172.18.0.2"))

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getBarbicanOpenBaoCluster(t, r, cp).Spec.Network.APIServerEndpointIPs).
		To(Equal([]string{"172.18.0.2"}))

	slice := &discoveryv1.EndpointSlice{}
	key := types.NamespacedName{
		Name:      apiServerEndpointSliceName,
		Namespace: apiServerEndpointSliceNamespace,
	}
	g.Expect(r.Get(context.Background(), key, slice)).To(Succeed())
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.7"}}}
	g.Expect(r.Update(context.Background(), slice)).To(Succeed())

	_, err = r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(getBarbicanOpenBaoCluster(t, r, cp).Spec.Network.APIServerEndpointIPs).
		To(Equal([]string{"10.0.0.7"}))
}

// TestEnsureBarbicanOpenBao_RefusesWithoutAPIServerEndpointSlice asserts the
// projection fails closed when the EndpointSlice is absent, writing no instance at
// all.
//
// Creating one anyway is the expensive mistake. An instance whose NetworkPolicy
// denies API-server egress cannot finish self-init, and the partial raft state it
// leaves behind wedges every later initialisation attempt: recovering it means
// deleting the instance together with its PVC, so an instance never created is
// strictly cheaper than one created wrong.
func TestEnsureBarbicanOpenBao_RefusesWithoutAPIServerEndpointSlice(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconcilerWithExactSeeds(t, cp)

	available, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("getting EndpointSlice default/kubernetes"))
	g.Expect(available).To(BeFalse())

	instance := &openbaov1alpha1.OpenBaoCluster{}
	instanceKey := types.NamespacedName{
		Namespace: cp.BarbicanNamespace(), Name: barbicanOpenBaoName(cp),
	}
	g.Expect(apierrors.IsNotFound(r.Get(context.Background(), instanceKey, instance))).To(BeTrue(),
		"no OpenBaoCluster may be written when the API-server addresses cannot be resolved")
}

// TestEnsureBarbicanOpenBao_RefusesEmptyAPIServerEndpointSlice asserts the same
// fail-closed outcome for an EndpointSlice that exists but carries no address. A
// present-but-empty slice would otherwise project an empty allowlist, which the
// openbao-operator renders as no egress rule at all — the very posture this
// resolves.
func TestEnsureBarbicanOpenBao_RefusesEmptyAPIServerEndpointSlice(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiServerEndpointSliceName,
			Namespace: apiServerEndpointSliceNamespace,
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{},
	})

	available, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(
		"EndpointSlice default/kubernetes carries no API server address"))
	g.Expect(available).To(BeFalse())

	instance := &openbaov1alpha1.OpenBaoCluster{}
	instanceKey := types.NamespacedName{
		Namespace: cp.BarbicanNamespace(), Name: barbicanOpenBaoName(cp),
	}
	g.Expect(apierrors.IsNotFound(r.Get(context.Background(), instanceKey, instance))).To(BeTrue(),
		"no OpenBaoCluster may be written when the API-server addresses cannot be resolved")
}

// TestEnsureBarbicanOpenBao_ProjectsSelfInitRequests pins the eight self-init
// requests and their order. self-init is one-shot — the requests run once against
// freshly initialised storage — so a mount or auth method must be enabled before
// anything writes under it, and a reordering cannot be corrected by a later
// reconcile.
func TestEnsureBarbicanOpenBao_ProjectsSelfInitRequests(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.SelfInit).NotTo(BeNil())
	g.Expect(instance.Spec.SelfInit.Enabled).To(BeTrue())

	names := make([]string, 0, len(instance.Spec.SelfInit.Requests))
	for _, req := range instance.Spec.SelfInit.Requests {
		names = append(names, req.Name)
	}
	g.Expect(names).To(Equal([]string{
		"barbican_kv",
		"barbican_secretstore_policy",
		"approle_auth",
		"barbican_approle_role",
		"kubernetes_auth",
		"kubernetes_auth_config",
		"provisioner_policy",
		"provisioner_k8s_role",
	}))

	role := instance.Spec.SelfInit.Requests[7]
	g.Expect(role.Data).NotTo(BeNil())
	data := map[string]string{}
	g.Expect(json.Unmarshal(role.Data.Raw, &data)).To(Succeed())
	g.Expect(data["bound_service_account_names"]).To(Equal("cp-barbican-bao-provisioner"))
	g.Expect(data["bound_service_account_namespaces"]).To(Equal(barbicanOpenBaoTestNamespace))
	g.Expect(data["audience"]).To(Equal("cp-barbican-bao"),
		"without the instance audience the role accepts any default-audience token of that account")
	g.Expect(data["token_policies"]).To(Equal("provisioner"))
}

// TestEnsureBarbicanOpenBao_ProjectsCertificates pins the two Certificates the
// instance's External TLS mode consumes. The server SAN list must carry the
// operator's internal TLS server name, or External-mode validation rejects the
// certificate and the instance never serves.
func TestEnsureBarbicanOpenBao_ProjectsCertificates(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	server := getBarbicanOpenBaoCertificate(t, r, barbicanOpenBaoTestNamespace, "cp-barbican-bao-tls-server")
	issuer, _, ierr := unstructured.NestedStringMap(server.Object, "spec", "issuerRef")
	g.Expect(ierr).NotTo(HaveOccurred())
	g.Expect(issuer).To(HaveKeyWithValue("name", openBaoCAIssuerName))
	g.Expect(issuer).To(HaveKeyWithValue("kind", "ClusterIssuer"))
	secretName, _, serr := unstructured.NestedString(server.Object, "spec", "secretName")
	g.Expect(serr).NotTo(HaveOccurred())
	g.Expect(secretName).To(Equal("cp-barbican-bao-tls-server"))
	sans, _, derr := unstructured.NestedStringSlice(server.Object, "spec", "dnsNames")
	g.Expect(derr).NotTo(HaveOccurred())
	g.Expect(sans).To(Equal([]string{
		"openbao-cluster-cp-barbican-bao.local",
		"cp-barbican-bao.barbican.svc",
		"*.cp-barbican-bao.barbican.svc",
		"cp-barbican-bao-public.barbican.svc",
	}))
	usages, _, uerr := unstructured.NestedStringSlice(server.Object, "spec", "usages")
	g.Expect(uerr).NotTo(HaveOccurred())
	g.Expect(usages).To(Equal([]string{"digital signature", "key encipherment", "server auth"}),
		"an unpinned EKU makes the server keypair a client identity for the OpenBao mTLS gate")
	g.Expect(isControlPlaneChild(server, cp)).To(BeTrue())

	ca := getBarbicanOpenBaoCertificate(t, r, barbicanOpenBaoTestNamespace, "cp-barbican-bao-tls-ca")
	caSecret, _, cerr := unstructured.NestedString(ca.Object, "spec", "secretName")
	g.Expect(cerr).NotTo(HaveOccurred())
	g.Expect(caSecret).To(Equal("cp-barbican-bao-tls-ca"),
		"the operator reads the trust bundle from this fixed-name Secret")
	caIssuer, _, cierr := unstructured.NestedStringMap(ca.Object, "spec", "issuerRef")
	g.Expect(cierr).NotTo(HaveOccurred())
	g.Expect(caIssuer).To(HaveKeyWithValue("name", openBaoCAIssuerName),
		"the server certificate must chain directly to the CA this Secret carries")
}

// TestEnsureBarbicanOpenBao_ProjectsAccessObjects pins the RBAC ensemble: the
// provisioner account the instance's Kubernetes-auth role binds to, the
// auth-delegator binding without which every login fails 403, and the TokenRequest
// grant without which the barbican-operator cannot mint the token it logs in with.
func TestEnsureBarbicanOpenBao_ProjectsAccessObjects(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	ctx := context.Background()

	sa := &corev1.ServiceAccount{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-provisioner",
	}, sa)).To(Succeed())
	g.Expect(sa.AutomountServiceAccountToken).NotTo(BeNil())
	g.Expect(*sa.AutomountServiceAccountToken).To(BeFalse(),
		"every consumer mints a token for the instance audience explicitly")
	g.Expect(isControlPlaneChild(sa, cp)).To(BeTrue())

	crb := &rbacv1.ClusterRoleBinding{}
	// Namespace-qualified (through the name hash): the object is cluster-scoped
	// while the instance name it is derived from is only namespace-unique.
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name: barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", barbicanOpenBaoTestNamespace),
	}, crb)).To(Succeed())
	g.Expect(crb.RoleRef.Kind).To(Equal("ClusterRole"))
	g.Expect(crb.RoleRef.Name).To(Equal("system:auth-delegator"))
	g.Expect(crb.Subjects).To(HaveLen(1))
	g.Expect(crb.Subjects[0].Kind).To(Equal("ServiceAccount"))
	g.Expect(crb.Subjects[0].Name).To(Equal("cp-barbican-bao-serviceaccount"),
		"the subject is the account the openbao-operator gives the instance pods")
	g.Expect(crb.Subjects[0].Namespace).To(Equal(barbicanOpenBaoTestNamespace))
	g.Expect(isControlPlaneChild(crb, cp)).To(BeTrue(),
		"no namespace deletion collects a cluster-scoped object, so the labels are its only handle")

	role := &rbacv1.Role{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-provisioner-token",
	}, role)).To(Succeed())
	g.Expect(role.Rules).To(HaveLen(1))
	g.Expect(role.Rules[0].APIGroups).To(Equal([]string{""}))
	g.Expect(role.Rules[0].Resources).To(Equal([]string{"serviceaccounts/token"}))
	g.Expect(role.Rules[0].ResourceNames).To(Equal([]string{"cp-barbican-bao-provisioner"}),
		"an unnamed grant would let the operator mint a token for any account in the namespace")
	g.Expect(role.Rules[0].Verbs).To(Equal([]string{"create"}))

	binding := &rbacv1.RoleBinding{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-provisioner-token",
	}, binding)).To(Succeed())
	g.Expect(binding.RoleRef.Kind).To(Equal("Role"))
	g.Expect(binding.RoleRef.Name).To(Equal("cp-barbican-bao-provisioner-token"))
	g.Expect(binding.Subjects).To(HaveLen(1))
	g.Expect(binding.Subjects[0].Name).To(Equal(defaultBarbicanOperatorServiceAccount))
	g.Expect(binding.Subjects[0].Namespace).To(Equal(defaultBarbicanOperatorNamespace))
}

// TestEnsureBarbicanOpenBao_HonoursOperatorIdentityOverride covers a
// barbican-operator deployed away from its chart defaults: both the TokenRequest
// subject and the operator ingress peer must follow the configured identity, or
// the operator is granted nothing and reaches nothing.
func TestEnsureBarbicanOpenBao_HonoursOperatorIdentityOverride(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)
	r.BarbicanOperatorNamespace = "openstack-operators"
	r.BarbicanOperatorServiceAccount = "barbican-controller-manager"

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	binding := &rbacv1.RoleBinding{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-provisioner-token",
	}, binding)).To(Succeed())
	g.Expect(binding.Subjects[0].Name).To(Equal("barbican-controller-manager"))
	g.Expect(binding.Subjects[0].Namespace).To(Equal("openstack-operators"))

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.Network.TrustedIngressPeers[0].NamespaceSelector.MatchLabels).To(
		HaveKeyWithValue("kubernetes.io/metadata.name", "openstack-operators"))
}

// TestEnsureBarbicanOpenBao_CreatesTenantForUnadmittedNamespace covers the plain
// case: nothing has admitted the Barbican service namespace to the
// openbao-operator yet, so the ControlPlane admits it.
func TestEnsureBarbicanOpenBao_CreatesTenantForUnadmittedNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	tenant := &openbaov1alpha1.OpenBaoTenant{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-tenant",
	}, tenant)).To(Succeed())
	g.Expect(tenant.Spec.TargetNamespace).To(Equal(barbicanOpenBaoTestNamespace))
	g.Expect(isControlPlaneChild(tenant, cp)).To(BeTrue())
}

// TestEnsureBarbicanOpenBao_SkipsTenantWhenNamespaceAlreadyAdmitted covers the
// kind stack, where the proving instance's tenant already targets the namespace.
// A second tenant over the same namespace holds a second finalizer on it, so
// teardown of this ControlPlane would then wait behind an object it does not own.
// The pre-existing tenant is left exactly as it is, under a name of its own.
func TestEnsureBarbicanOpenBao_SkipsTenantWhenNamespaceAlreadyAdmitted(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	existing := &openbaov1alpha1.OpenBaoTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proving-tenant",
			Namespace: "shared-services",
			UID:       types.UID("proving-tenant-uid"),
		},
		Spec: openbaov1alpha1.OpenBaoTenantSpec{TargetNamespace: barbicanOpenBaoTestNamespace},
	}
	r := barbicanOpenBaoReconciler(t, cp, existing)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	ours := &openbaov1alpha1.OpenBaoTenant{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Namespace: barbicanOpenBaoTestNamespace, Name: "cp-barbican-bao-tenant",
	}, ours)).NotTo(Succeed(),
		"a namespace already admitted must not be admitted a second time")

	after := &openbaov1alpha1.OpenBaoTenant{}
	g.Expect(r.Get(context.Background(), types.NamespacedName{
		Namespace: "shared-services", Name: "proving-tenant",
	}, after)).To(Succeed())
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse(), "the pre-existing tenant must not be adopted")
}

// TestEnsureBarbicanOpenBao_RefusesForeignInstance covers a same-named
// OpenBaoCluster somebody else provisioned in the Barbican service namespace.
// Re-projecting it would overwrite its spec, and the ownership labels the
// projection stamps would make the teardown delete it, so the ensure fails loud
// and leaves it untouched.
func TestEnsureBarbicanOpenBao_RefusesForeignInstance(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	foreign := &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanOpenBaoName(cp),
			Namespace: barbicanOpenBaoTestNamespace,
			UID:       types.UID("foreign-instance-uid"),
			Labels:    map[string]string{"owner": "someone-else"},
		},
		Spec: openbaov1alpha1.OpenBaoClusterSpec{
			Profile:  openbaov1alpha1.ProfileHardened,
			Version:  "2.5.0",
			Replicas: 3,
		},
	}
	r := barbicanOpenBaoReconciler(t, cp, foreign)

	available, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("refusing to adopt"))
	g.Expect(available).To(BeFalse())

	after := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(after.Spec.Version).To(Equal("2.5.0"), "the foreign instance's spec must not be reshaped")
	g.Expect(after.Spec.Profile).To(Equal(openbaov1alpha1.ProfileHardened))
	g.Expect(isControlPlaneChild(after, cp)).To(BeFalse())
}

// TestEnsureBarbicanOpenBao_ReportsAvailability asserts the readiness gate the
// caller drives its condition from: an instance that has not reported Available is
// not available, and one that reports Available=True is.
func TestEnsureBarbicanOpenBao_ReportsAvailability(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	available, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(available).To(BeFalse(), "a freshly created instance reports no conditions yet")

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	instance.Status.Conditions = []metav1.Condition{{
		Type:               string(openbaov1alpha1.ConditionAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: instance.Generation,
		Reason:             "ClusterRunning",
		Message:            "serving",
		LastTransitionTime: metav1.Now(),
	}}
	g.Expect(r.Status().Update(context.Background(), instance)).To(Succeed())

	available, err = r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(available).To(BeTrue())
}

// TestOpenBaoClusterAvailable covers the two states the readiness helper must not
// confuse: a Degraded-but-present condition set is not availability, and a nil
// instance is not either.
func TestOpenBaoClusterAvailable(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(openBaoClusterAvailable(nil)).To(BeFalse())

	instance := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(openBaoClusterAvailable(instance)).To(BeFalse())

	instance.Status.Conditions = []metav1.Condition{{
		Type:               string(openbaov1alpha1.ConditionDegraded),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Sealed",
	}}
	g.Expect(openBaoClusterAvailable(instance)).To(BeFalse())

	instance.Status.Conditions = append(instance.Status.Conditions, metav1.Condition{
		Type:               string(openbaov1alpha1.ConditionAvailable),
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "Initializing",
	})
	g.Expect(openBaoClusterAvailable(instance)).To(BeFalse())
}

// TestEnsureBarbicanOpenBao_AuthDelegatorNameIsNamespaceQualified pins the one
// name in the ensemble that has to carry the ControlPlane's NAMESPACE.
//
// Admission allows one ControlPlane per namespace, not one ControlPlane name per
// cluster, so two ControlPlanes called "cp" in different namespaces derive the
// same instance name. Every other object in the ensemble is namespaced and
// therefore still distinct; the auth-delegator binding is cluster-scoped. Under
// an unqualified name the second ControlPlane would meet a binding it does not
// own, ensureUnownedOrOwned would refuse it on every pass, and BarbicanReady
// would park on BarbicanOpenBaoError with no way out short of recreating the
// control plane.
func TestEnsureBarbicanOpenBao_AuthDelegatorNameIsNamespaceQualified(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	first := barbicanOpenBaoControlPlane()
	first.Namespace = "tenant-a"
	first.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "barbican-a"}

	second := barbicanOpenBaoControlPlane()
	second.Namespace = "tenant-b"
	second.UID = types.UID("cp-uid-b")
	second.Spec.Services.Barbican.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "barbican-b"}

	g.Expect(barbicanOpenBaoName(first)).To(Equal(barbicanOpenBaoName(second)),
		"the fixture only proves anything while both derive the SAME instance name")

	r := barbicanOpenBaoReconciler(t, first, second)
	_, err := r.ensureBarbicanOpenBao(ctx, r.Client, first)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.ensureBarbicanOpenBao(ctx, r.Client, second)
	g.Expect(err).NotTo(HaveOccurred(),
		"the second ControlPlane must not collide with the first one's cluster-scoped binding")

	firstName := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(first), first.BarbicanNamespace())
	secondName := barbicanOpenBaoAuthDelegatorName(barbicanOpenBaoName(second), second.BarbicanNamespace())
	g.Expect(firstName).NotTo(Equal(secondName))

	for _, tc := range []struct {
		cp   *c5c3v1alpha1.ControlPlane
		name string
	}{
		{first, firstName},
		{second, secondName},
	} {
		crb := &rbacv1.ClusterRoleBinding{}
		g.Expect(r.Get(ctx, types.NamespacedName{Name: tc.name}, crb)).To(Succeed())
		g.Expect(isControlPlaneChild(crb, tc.cp)).To(BeTrue())
		g.Expect(crb.Subjects[0].Namespace).To(Equal(tc.cp.BarbicanNamespace()))
	}
}

// TestBarbicanOpenBaoAuthDelegatorName_DisambiguatesTheNamespaceBoundary is the
// regression guard for the collision the namespace qualification above is meant
// to rule out but a "namespace-name" concatenation would still allow: "-" is
// legal inside both operands, so the boundary between them is not recoverable and
// two different (namespace, name) pairs can spell the same binding name. Both
// pairs below are reachable — the barbican namespace is operator-chosen — and the
// second ControlPlane to arrive would meet a cluster-scoped binding it does not
// own, with no recovery short of recreating the control plane.
func TestBarbicanOpenBaoAuthDelegatorName_DisambiguatesTheNamespaceBoundary(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(barbicanOpenBaoAuthDelegatorName("prod-barbican-bao", "cloud-eu")).NotTo(
		Equal(barbicanOpenBaoAuthDelegatorName("eu-prod-barbican-bao", "cloud")))

	// The same instance in the same namespace still derives one stable name: the
	// teardown sweep re-derives it from the ControlPlane to find the binding.
	g.Expect(barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", "openstack")).To(
		Equal(barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", "openstack")))
	g.Expect(barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", "openstack")).To(
		HavePrefix("cp-barbican-bao-"), "the instance it belongs to stays readable in the name")
	g.Expect(barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", "openstack")).To(
		HaveSuffix(barbicanOpenBaoAuthDelegatorSuffix))

	// The disambiguator carries 64 bits, not the 32 an accidental collision would
	// need. Namespace names are free-form DNS-1123 labels, so where tenants create
	// their own, a colliding one is an offline search rather than a coincidence —
	// and whoever registers the cluster-scoped binding first leaves the other
	// ControlPlane parked on BarbicanOpenBaoError for good.
	g.Expect(barbicanOpenBaoAuthDelegatorName("cp-barbican-bao", "openstack")).To(
		MatchRegexp(`^cp-barbican-bao-[0-9a-f]{16}`+barbicanOpenBaoAuthDelegatorSuffix+`$`),
		"truncating the hash further puts the binding name within reach of a brute-force search")
}

// --- per-service target clusters: the ensemble follows the service ---

// placedBarbicanOpenBaoControlPlane places the Barbican service, and with it the
// whole dedicated ensemble, on a target cluster.
func placedBarbicanOpenBaoControlPlane(targetCluster string) *c5c3v1alpha1.ControlPlane {
	cp := barbicanOpenBaoControlPlane()
	cp.Spec.Services.Barbican.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: targetCluster}
	return cp
}

// splitBarbicanOpenBaoReconciler builds a reconciler over two fake clusters: the
// management one holding cp plus home, and a target cluster — registered under
// every name — holding target.
//
// Neither is seeded with an API-server EndpointSlice, so each test says which
// cluster publishes one. That is the whole point of a placed ensemble: the
// NetworkPolicy is enforced over pods on the target, so only the target's
// addresses are the right ones, and a test that seeded both could not tell which
// cluster was read.
func splitBarbicanOpenBaoReconciler(
	t *testing.T, cp *c5c3v1alpha1.ControlPlane, home, target []client.Object,
) (*ControlPlaneReconciler, client.Client) {
	t.Helper()
	s := barbicanOpenBaoTestScheme(t)
	remote := fake.NewClientBuilder().WithScheme(s).WithObjects(target...).
		WithStatusSubresource(&openbaov1alpha1.OpenBaoCluster{}).Build()
	return &ControlPlaneReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{cp}, home...)...).
			WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: remote},
	}, remote
}

// barbicanOpenBaoChildrenClient resolves the client a placed ensemble is written
// with, the same way the sub-reconciler does before it calls into the ensemble.
func barbicanOpenBaoChildrenClient(
	t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane,
) client.Client {
	t.Helper()
	c, err := r.childrenClientFor(context.Background(), cp, cp.BarbicanNamespace())
	if err != nil {
		t.Fatalf("resolving the children client of namespace %q: %v", cp.BarbicanNamespace(), err)
	}
	return c
}

// TestEnsureBarbicanOpenBao_PlacedEnsembleLandsOnTheTarget verifies every object
// of a placed ensemble is written to the service's own cluster, claimed by the
// labels alone: the openbao-operator that reconciles the instance, the
// cert-manager that issues its certificates, and the TokenReview its logins are
// validated with all run there, and none of them can act on an object left at
// home. The cluster-scoped ClusterRoleBinding is created on the target for the
// same reason — the account it names is a target-cluster account.
//
// The unseal Secret is the deliberate exception. It keeps the OpenBaoCluster's
// controller reference, which stays legal because the two live in the same
// namespace on the same cluster, and it is the ownership proof the
// openbao-operator adopts a pre-existing unseal Secret against.
func TestEnsureBarbicanOpenBao_PlacedEnsembleLandsOnTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedBarbicanOpenBaoControlPlane("remote-a")
	r, remote := splitBarbicanOpenBaoReconciler(t, cp, nil,
		[]client.Object{defaultKubernetesEndpointSlice()})

	_, err := r.ensureBarbicanOpenBao(ctx, barbicanOpenBaoChildrenClient(t, r, cp), cp)
	g.Expect(err).NotTo(HaveOccurred())

	ns := barbicanOpenBaoTestNamespace
	instanceName := barbicanOpenBaoName(cp)
	serverCert, caCert := &unstructured.Unstructured{}, &unstructured.Unstructured{}
	serverCert.SetGroupVersionKind(certificateGVK)
	caCert.SetGroupVersionKind(certificateGVK)

	ensemble := []struct {
		obj client.Object
		key types.NamespacedName
	}{
		{&openbaov1alpha1.OpenBaoTenant{}, types.NamespacedName{Namespace: ns, Name: instanceName + "-tenant"}},
		{serverCert, types.NamespacedName{Namespace: ns, Name: instanceName + "-tls-server"}},
		{caCert, types.NamespacedName{Namespace: ns, Name: instanceName + "-tls-ca"}},
		{&corev1.ServiceAccount{}, types.NamespacedName{Namespace: ns, Name: instanceName + "-provisioner"}},
		{&rbacv1.Role{}, types.NamespacedName{Namespace: ns, Name: instanceName + "-provisioner-token"}},
		{&rbacv1.RoleBinding{}, types.NamespacedName{Namespace: ns, Name: instanceName + "-provisioner-token"}},
		{&rbacv1.ClusterRoleBinding{}, types.NamespacedName{Name: barbicanOpenBaoAuthDelegatorName(instanceName, ns)}},
		{&openbaov1alpha1.OpenBaoCluster{}, types.NamespacedName{Namespace: ns, Name: instanceName}},
	}
	for _, member := range ensemble {
		g.Expect(remote.Get(ctx, member.key, member.obj)).To(Succeed(),
			"%T %s must be written to the target cluster", member.obj, member.key)
		g.Expect(member.obj.GetLabels()).To(Equal(remoteChildLabels(cp)),
			"%T must carry the full remote claim", member.obj)
		g.Expect(member.obj.GetOwnerReferences()).To(BeEmpty(),
			"%T must carry no owner reference on the target cluster", member.obj)
	}

	// The unseal Secret: same claim, but the instance's controller reference on top
	// of it.
	secret := &corev1.Secret{}
	g.Expect(remote.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: instanceName + barbicanOpenBaoUnsealSecretSuffix,
	}, secret)).To(Succeed())
	g.Expect(secret.Labels).To(Equal(remoteChildLabels(cp)))
	owner := metav1.GetControllerOf(secret)
	g.Expect(owner).NotTo(BeNil(), "the seal key must still be owned by the instance it seals")
	g.Expect(owner.Kind).To(Equal("OpenBaoCluster"))
	g.Expect(owner.Name).To(Equal(instanceName))

	// Nothing of the ensemble at home, the cluster-scoped binding included.
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: instanceName},
		&openbaov1alpha1.OpenBaoCluster{})).NotTo(Succeed(),
		"a placed instance must not be provisioned on the management cluster as well")
	g.Expect(r.Client.Get(ctx, types.NamespacedName{Name: barbicanOpenBaoAuthDelegatorName(instanceName, ns)},
		&rbacv1.ClusterRoleBinding{})).NotTo(Succeed(),
		"the TokenReview grant belongs to the cluster the instance pods run on")
}

// TestEnsureBarbicanOpenBao_ResolvesTheTargetAPIServerEndpoints verifies the
// egress allowlist is built from the TARGET cluster's API-server addresses. The
// NetworkPolicy the openbao-operator renders is enforced by the CNI on that
// cluster, over pods that reach their own API server, so the management cluster's
// addresses would allow the instance nothing it needs — and the instance would
// wedge exactly as it does with no allowance at all.
func TestEnsureBarbicanOpenBao_ResolvesTheTargetAPIServerEndpoints(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedBarbicanOpenBaoControlPlane("remote-a")
	r, remote := splitBarbicanOpenBaoReconciler(t, cp,
		[]client.Object{defaultKubernetesEndpointSlice("10.0.0.1")},
		[]client.Object{defaultKubernetesEndpointSlice("172.18.0.5", "172.18.0.4")})

	_, err := r.ensureBarbicanOpenBao(ctx, barbicanOpenBaoChildrenClient(t, r, cp), cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := &openbaov1alpha1.OpenBaoCluster{}
	g.Expect(remote.Get(ctx, types.NamespacedName{
		Namespace: cp.BarbicanNamespace(), Name: barbicanOpenBaoName(cp),
	}, instance)).To(Succeed())
	g.Expect(instance.Spec.Network).NotTo(BeNil())
	g.Expect(instance.Spec.Network.APIServerEndpointIPs).To(Equal([]string{"172.18.0.4", "172.18.0.5"}),
		"the addresses must be the target's, deduplicated and sorted")
}

// TestEnsureBarbicanOpenBao_RefusesUnusableTargetEndpointSlice keeps the
// fail-closed posture on the target cluster: an absent slice and a present-but-
// empty one each abort with the error naming the object, and no instance is
// written on either cluster. The management cluster publishes a perfectly good
// slice in both cases, which is what makes the refusal a statement about the
// target rather than about the read failing everywhere.
func TestEnsureBarbicanOpenBao_RefusesUnusableTargetEndpointSlice(t *testing.T) {
	emptySlice := defaultKubernetesEndpointSlice()
	emptySlice.Endpoints = nil

	tests := []struct {
		name   string
		target []client.Object
		errmsg string
	}{
		{
			name:   "no slice on the target",
			errmsg: "getting EndpointSlice default/kubernetes",
		},
		{
			name:   "empty slice on the target",
			target: []client.Object{emptySlice},
			errmsg: "EndpointSlice default/kubernetes carries no API server address",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cp := placedBarbicanOpenBaoControlPlane("remote-a")
			r, remote := splitBarbicanOpenBaoReconciler(t, cp,
				[]client.Object{defaultKubernetesEndpointSlice("10.0.0.1")}, tc.target)

			available, err := r.ensureBarbicanOpenBao(ctx, barbicanOpenBaoChildrenClient(t, r, cp), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errmsg))
			g.Expect(available).To(BeFalse())

			for name, c := range map[string]client.Client{"management": r.Client, "target": remote} {
				var instances openbaov1alpha1.OpenBaoClusterList
				g.Expect(c.List(ctx, &instances)).To(Succeed())
				g.Expect(instances.Items).To(BeEmpty(),
					"no OpenBaoCluster may be written on the %s cluster", name)
			}
		})
	}
}

// TestEnsureBarbicanOpenBao_CarriesOverLiveStorage pins the one field the
// projection must NOT re-project onto a live instance: the openbao-operator
// rejects a change to spec.storage on an existing CR, so an instance whose volume
// was grown out of band must keep its size rather than be pushed back to the
// projection's create-time 1Gi on the next pass.
func TestEnsureBarbicanOpenBao_CarriesOverLiveStorage(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()

	live := &openbaov1alpha1.OpenBaoCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      barbicanOpenBaoName(cp),
			Namespace: cp.BarbicanNamespace(),
			Labels:    controlPlaneChildLabels(cp),
		},
		Spec: openbaov1alpha1.OpenBaoClusterSpec{Storage: openbaov1alpha1.StorageConfig{Size: "10Gi"}},
	}
	r := barbicanOpenBaoReconciler(t, cp, live)

	_, err := r.ensureBarbicanOpenBao(context.Background(), r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.Storage.Size).To(Equal("10Gi"),
		"spec.storage is carried over from the live object, never re-projected")
	g.Expect(instance.Spec.Version).To(Equal(defaultOpenBaoVersion),
		"every other field is still re-projected")
}
