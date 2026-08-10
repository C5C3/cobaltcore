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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
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

// barbicanOpenBaoReconciler builds a reconciler over a fake client seeded with cp
// and objs.
func barbicanOpenBaoReconciler(t *testing.T, cp *c5c3v1alpha1.ControlPlane, objs ...client.Object) *ControlPlaneReconciler {
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err = r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

// TestEnsureBarbicanOpenBao_ProjectsSelfInitRequests pins the eight self-init
// requests and their order. self-init is one-shot — the requests run once against
// freshly initialised storage — so a mount or auth method must be enabled before
// anything writes under it, and a reordering cannot be corrected by a later
// reconcile.
func TestEnsureBarbicanOpenBao_ProjectsSelfInitRequests(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := barbicanOpenBaoControlPlane()
	r := barbicanOpenBaoReconciler(t, cp)

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	available, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	available, err := r.ensureBarbicanOpenBao(context.Background(), cp)
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

	available, err = r.ensureBarbicanOpenBao(context.Background(), cp)
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
	_, err := r.ensureBarbicanOpenBao(ctx, first)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.ensureBarbicanOpenBao(ctx, second)
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

	_, err := r.ensureBarbicanOpenBao(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())

	instance := getBarbicanOpenBaoCluster(t, r, cp)
	g.Expect(instance.Spec.Storage.Size).To(Equal("10Gi"),
		"spec.storage is carried over from the live object, never re-projected")
	g.Expect(instance.Spec.Version).To(Equal(defaultOpenBaoVersion),
		"every other field is still re-projected")
}
