// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the hypervisor operator's account: the registration
// spec.services.nova.hypervisorOperator projects, the auth Secret and its
// mirrors, and the prune once the block is cleared.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// TestDesiredNovaHypervisorOperatorRegistration pins the account-only
// registration: a name derived from the Nova child's, the Nova namespace, no
// catalog entry, a project of its own the registration creates, and admin
// alone.
func TestDesiredNovaHypervisorOperatorRegistration(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "controlplane", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{Services: c5c3v1alpha1.ServicesSpec{
			Nova: &c5c3v1alpha1.ServiceNovaSpec{HypervisorOperator: &c5c3v1alpha1.ServiceNovaHypervisorOperatorSpec{}},
		}},
	}

	ks := desiredNovaHypervisorOperatorRegistration(cp)

	g.Expect(ks.Name).To(Equal("controlplane-nova-hypervisor-operator"))
	g.Expect(ks.Namespace).To(Equal(cp.NovaNamespace()))
	g.Expect(ks.Spec.ControlPlaneRef).To(Equal(c5c3v1alpha1.ControlPlaneRefSpec{Name: "controlplane", Namespace: "openstack"}))
	g.Expect(ks.Spec.Catalog).To(BeNil(), "an account nothing calls advertises no endpoint")
	g.Expect(ks.Spec.Account).NotTo(BeNil())
	g.Expect(ks.Spec.Account.UserName).To(Equal("hypervisor-operator"))
	g.Expect(ks.Spec.Account.Project).To(Equal(c5c3v1alpha1.ServiceAccountProjectSpec{
		Name: "service-hypervisor-operator", Create: true,
	}), "the account gets a project of its own, apart from service-nova")
	g.Expect(ks.Spec.Account.Roles).To(Equal([]string{"admin"}))
	g.Expect(ks.Spec.Account.DomainName).To(BeEmpty(),
		"an unset domain lets the registration resolve the ControlPlane's admin domain")
	g.Expect(ks.Spec.Account.Adopt).To(BeFalse(), "a colliding user must fail loud, never be taken over")

	placed := cp.DeepCopy()
	placed.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
	g.Expect(desiredNovaHypervisorOperatorRegistration(placed).Namespace).To(Equal("compute"),
		"the registration follows the Nova into a dedicated namespace")
	g.Expect(desiredNovaHypervisorOperatorRegistration(placed).Spec.ControlPlaneRef.Namespace).To(Equal("openstack"),
		"the registration names the ControlPlane's namespace explicitly, not its own")

	g.Expect(desiredNovaRegistration(cp).Spec.Account.Roles).To(Equal([]string{"service", "admin"}),
		"nova's own account keeps its roles")
}

// TestNovaHypervisorOperatorRegistrationName_FitsTheLabel pins the length
// budget: validateNovaChildName caps a ControlPlane with Nova at 36
// characters, and the registration name then stays within the 63 characters of
// the c5c3.io/keystoneservice-name label it becomes.
func TestNovaHypervisorOperatorRegistrationName_FitsTheLabel(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("c", 36), Namespace: "openstack"},
	}

	g.Expect(novaHypervisorOperatorRegistrationName(cp)).To(HaveLen(61))
}

// --- the leg: fixtures ---

// hvoControlPlane is novaControlPlane named "controlplane" in "openstack", with
// spec.services.nova.hypervisorOperator set.
func hvoControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := novaControlPlane()
	cp.Name = "controlplane"
	cp.Namespace = "openstack"
	cp.Spec.Services.Nova.HypervisorOperator = &c5c3v1alpha1.ServiceNovaHypervisorOperatorSpec{}
	return cp
}

// readyHVORegistration builds the hypervisor operator's registration,
// converged.
func readyHVORegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := desiredNovaHypervisorOperatorRegistration(cp)
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	return markRegistrationConverged(ks)
}

// hvoCredentials is the consumer Secret the registration delivers, carrying
// data verbatim, so a test can leave the password out or empty.
func hvoCredentials(cp *c5c3v1alpha1.ControlPlane, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: novaHypervisorOperatorCredentialsSecretName(cp), Namespace: cp.NovaNamespace(),
		},
		Data: data,
	}
}

// hvoPassword is the credentials data carrying pw.
func hvoPassword(pw string) map[string][]byte {
	return map[string][]byte{"password": []byte(pw)}
}

// hvoAuthKey is the key of the auth Secret in namespace.
func hvoAuthKey(cp *c5c3v1alpha1.ControlPlane, namespace string) types.NamespacedName {
	return types.NamespacedName{Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: namespace}
}

// newHVOTestReconciler is newNovaTestReconciler with an interceptor on the
// local client. The hypervisor-operator tests seed the nova registration
// explicitly, because withReadyNovaRegistration backs off once any
// KeystoneService is seeded.
func newHVOTestReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := novaTestScheme(t)
	seeded := withNovaTenantStore(withReadyNovaRegistration(withNovaBusSecret(withReadyNovaDBCreds(objs))))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(seeded...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}, &novav1alpha1.Nova{},
			&c5c3v1alpha1.KeystoneService{}).
		WithInterceptorFuncs(funcs).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// perClusterResolver hands out one client per registered cluster name and
// fails every other name, the shape of a fleet of compute clusters.
// childrenResolver hands one client out under every name, which cannot tell
// the mirrors on two clusters apart.
type perClusterResolver map[string]client.Client

func (p perClusterResolver) GetCluster(_ context.Context, name mcruntime.ClusterName) (cluster.Cluster, error) {
	c, ok := p[string(name)]
	if !ok {
		return nil, fmt.Errorf("cluster %q not found", name)
	}
	return fakeTargetCluster{c: c}, nil
}

// convergeHVO drives two Nova passes: the first projects the Nova child, which
// is then marked Ready, and the second runs everything behind that verdict,
// the hypervisor-operator leg among it. It returns the second pass's result.
func convergeHVO(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, error) {
	t.Helper()
	if _, err := r.reconcileNova(context.Background(), cp); err != nil {
		t.Fatalf("first Nova pass: %v", err)
	}
	convergeNovaChild(t, r, cp, getProjectedNova(t, r.Client, cp).Generation)
	return r.reconcileNova(context.Background(), cp)
}

// --- the leg ---

// TestReconcileNovaHypervisorOperator_UnsetBlockChangesNothing pins the
// default: without the block no account is registered, no auth Secret is
// written, and NovaReady converges exactly as before.
func TestReconcileNovaHypervisorOperator_UnsetBlockChangesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	cp.Spec.Services.Nova.HypervisorOperator = nil
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	res, err := convergeHVO(t, r, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NovaReady"))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(desiredNovaHypervisorOperatorRegistration(cp)),
		&c5c3v1alpha1.KeystoneService{}))).To(BeTrue(), "no account is registered without the block")
	g.Expect(apierrors.IsNotFound(r.Get(ctx, hvoAuthKey(cp, cp.NovaNamespace()), &corev1.Secret{}))).To(BeTrue(),
		"no auth Secret is written without the block")
}

// TestReconcileNovaHypervisorOperator_WaitsForTheAccount pins the relay: while
// the account is not provisioned, NovaReady carries the registration's own
// reason, the Nova child is applied regardless, and no auth Secret exists.
func TestReconcileNovaHypervisorOperator_WaitsForTheAccount(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	r := newNovaTestReconciler(t, cp)
	ctx := context.Background()

	res, err := convergeHVO(t, r, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(korcRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForServiceRegistration))
	g.Expect(cond.Message).To(ContainSubstring(novaHypervisorOperatorRegistrationName(cp)))
	getProjectedNova(t, r.Client, cp)
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(desiredNovaHypervisorOperatorRegistration(cp)),
		&c5c3v1alpha1.KeystoneService{})).To(Succeed(), "the registration is applied")
	g.Expect(apierrors.IsNotFound(r.Get(ctx, hvoAuthKey(cp, cp.NovaNamespace()), &corev1.Secret{}))).To(BeTrue(),
		"no auth Secret before the account exists")
}

// TestReconcileNovaHypervisorOperator_WaitsForThePassword pins the bounded
// wait on the consumer Secret: absent, without a password key, or with an
// empty one, NovaReady waits with a nil error.
func TestReconcileNovaHypervisorOperator_WaitsForThePassword(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds map[string][]byte
		seed  bool
	}{
		{name: "the Secret is missing"},
		{name: "the password key is absent", creds: map[string][]byte{"username": []byte("x")}, seed: true},
		{name: "the password is empty", creds: hvoPassword(""), seed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := hvoControlPlane()
			objs := []client.Object{cp, readyNovaRegistration(cp), readyHVORegistration(cp)}
			if tc.seed {
				objs = append(objs, hvoCredentials(cp, tc.creds))
			}
			r := newNovaTestReconciler(t, objs...)

			res, err := convergeHVO(t, r, cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := novaCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonWaitingForHypervisorOperatorCredentials))
			g.Expect(cond.Message).To(Equal(fmt.Sprintf(
				"the credentials Secret openstack/%s of the hypervisor operator account has not been materialized yet",
				novaHypervisorOperatorCredentialsSecretName(cp))))
			g.Expect(apierrors.IsNotFound(r.Get(context.Background(), hvoAuthKey(cp, cp.NovaNamespace()),
				&corev1.Secret{}))).To(BeTrue())
		})
	}
}

// TestReconcileNovaHypervisorOperator_CredentialsReadFailure pins a
// non-NotFound read of the consumer Secret: a failed reconcile, not a wait.
func TestReconcileNovaHypervisorOperator_CredentialsReadFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	injected := errors.New("injected read failure")
	credsName := novaHypervisorOperatorCredentialsSecretName(cp)
	r := newHVOTestReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if key.Name == credsName {
				return injected
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, cp, readyNovaRegistration(cp), readyHVORegistration(cp))

	_, err := convergeHVO(t, r, cp)

	g.Expect(err).To(MatchError(injected))
	g.Expect(err.Error()).To(HavePrefix("reading the hypervisor operator credentials Secret"))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonHypervisorOperatorError))
}

// TestReconcileNovaHypervisorOperator_WritesTheAuthSecret pins the local
// delivery: seven keys, each a value of the hypervisor operator's chart, the
// ControlPlane's ownership labels, no mirror label, and NovaReady True.
func TestReconcileNovaHypervisorOperator_WritesTheAuthSecret(t *testing.T) {
	for _, tc := range []struct {
		name           string
		publicEndpoint string
		wantAuthURL    string
	}{
		{name: "an unpublished Keystone", wantAuthURL: "http://controlplane-keystone.openstack.svc:5000/v3"},
		{
			name:           "a published Keystone",
			publicEndpoint: "https://keystone.example.com/v3",
			wantAuthURL:    "https://keystone.example.com/v3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := hvoControlPlane()
			cp.Spec.Services.Keystone.PublicEndpoint = tc.publicEndpoint
			r := newNovaTestReconciler(t, cp, readyNovaRegistration(cp), readyHVORegistration(cp),
				hvoCredentials(cp, hvoPassword("s3cret")))

			res, err := convergeHVO(t, r, cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(novaCondition(t, cp).Status).To(Equal(metav1.ConditionTrue))

			auth := &corev1.Secret{}
			g.Expect(r.Get(context.Background(), hvoAuthKey(cp, "openstack"), auth)).To(Succeed())
			g.Expect(auth.Name).To(Equal("controlplane-nova-hypervisor-operator-auth"))
			g.Expect(auth.Type).To(Equal(corev1.SecretTypeOpaque))
			g.Expect(auth.Data).To(Equal(map[string][]byte{
				"auth_url":            []byte(tc.wantAuthURL),
				"username":            []byte("hypervisor-operator"),
				"user_domain_name":    []byte("Default"),
				"project_name":        []byte("service-hypervisor-operator"),
				"project_domain_name": []byte("Default"),
				"region_name":         []byte("RegionOne"),
				"password":            []byte("s3cret"),
			}))
			g.Expect(auth.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "controlplane"))
			g.Expect(auth.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
			g.Expect(auth.Labels).NotTo(HaveKey(novav1alpha1.ComputeConfigMirrorLabel),
				"the reap of the last pool on a cluster must never delete the source")
		})
	}
}

// TestReconcileNovaHypervisorOperator_NoPoolNoMirror pins the empty target
// set: an hvo on the Nova's own cluster reads the source, and no compute
// cluster is written to.
func TestReconcileNovaHypervisorOperator_NoPoolNoMirror(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	r := newNovaTestReconciler(t, cp, readyNovaRegistration(cp), readyHVORegistration(cp),
		hvoCredentials(cp, hvoPassword("s3cret")))
	computeA := fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.Resolver = perClusterResolver{"compute-a": computeA}
	ctx := context.Background()

	_, err := convergeHVO(t, r, cp)
	g.Expect(err).NotTo(HaveOccurred())

	var local corev1.SecretList
	g.Expect(r.Client.List(ctx, &local)).To(Succeed())
	var copies []string
	for _, secret := range local.Items {
		if secret.Name == novaHypervisorOperatorAuthSecretName(cp) {
			copies = append(copies, secret.Namespace)
		}
	}
	g.Expect(copies).To(ConsistOf("openstack"), "the source is the only auth Secret")
	var remote corev1.SecretList
	g.Expect(computeA.List(ctx, &remote)).To(Succeed())
	g.Expect(remote.Items).To(BeEmpty(), "a cluster no pool runs on receives nothing")
}

// hvoFleet is a converged plane with the hypervisor operator's account, two
// pools on compute-a and one on compute-b, and one fake client per cluster.
type hvoFleet struct {
	cp       *c5c3v1alpha1.ControlPlane
	r        *ControlPlaneReconciler
	clusters map[string]client.Client
}

func newHVOFleet(t *testing.T, computeA interceptor.Funcs, seeds map[string][]client.Object,
	objs ...client.Object,
) *hvoFleet {
	t.Helper()
	cp := hvoControlPlane()
	objs = append([]client.Object{
		cp, readyNovaRegistration(cp), readyHVORegistration(cp),
		hvoCredentials(cp, hvoPassword("s3cret")), publishedComputeConfig(cp),
		mirrorPool(cp, "pool-a1", novaName(cp), "compute-a"),
		mirrorPool(cp, "pool-a2", novaName(cp), "compute-a"),
		mirrorPool(cp, "pool-b", novaName(cp), "compute-b"),
	}, objs...)
	r := newNovaTestReconciler(t, objs...)
	clusters := map[string]client.Client{
		"compute-a": fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(seeds["compute-a"]...).
			WithInterceptorFuncs(computeA).Build(),
		"compute-b": fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(seeds["compute-b"]...).Build(),
	}
	r.Resolver = perClusterResolver(clusters)
	return &hvoFleet{cp: cp, r: r, clusters: clusters}
}

// authOn reads the auth Secret on one cluster, in the Nova namespace.
func (f *hvoFleet) authOn(t *testing.T, name string) (*corev1.Secret, error) {
	t.Helper()
	secret := &corev1.Secret{}
	err := f.clusters[name].Get(context.Background(), hvoAuthKey(f.cp, f.cp.NovaNamespace()), secret)
	return secret, err
}

// TestReconcileNovaHypervisorOperator_MirrorsOntoEveryComputeCluster pins the
// delivery: one mirror per cluster a pool runs on, in the Nova namespace, with
// the source's data, the ownership labels and the mirror label the nova
// operator's reap recognizes.
func TestReconcileNovaHypervisorOperator_MirrorsOntoEveryComputeCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	f := newHVOFleet(t, interceptor.Funcs{}, nil)

	res, err := convergeHVO(t, f.r, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	source := &corev1.Secret{}
	g.Expect(f.r.Get(context.Background(), hvoAuthKey(f.cp, "openstack"), source)).To(Succeed())
	for _, name := range []string{"compute-a", "compute-b"} {
		var all corev1.SecretList
		g.Expect(f.clusters[name].List(context.Background(), &all)).To(Succeed())
		var auth []string
		for _, secret := range all.Items {
			if secret.Name == novaHypervisorOperatorAuthSecretName(f.cp) {
				auth = append(auth, secret.Namespace)
			}
		}
		g.Expect(auth).To(ConsistOf("openstack"), "one mirror on %s, however many pools run there", name)

		mirror, err := f.authOn(t, name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mirror.Data).To(Equal(source.Data))
		for k, v := range remoteChildLabels(f.cp) {
			g.Expect(mirror.Labels).To(HaveKeyWithValue(k, v))
		}
		g.Expect(mirror.Labels).To(HaveKeyWithValue(novav1alpha1.ComputeConfigMirrorLabel, "true"))
	}
}

// TestReconcileNovaHypervisorOperator_MirrorWriteFailure pins a failed write
// on one compute cluster: a failed reconcile naming the namespace and the
// cluster.
func TestReconcileNovaHypervisorOperator_MirrorWriteFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	injected := errors.New("injected write failure")
	f := newHVOFleet(t, interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
			opts ...client.ApplyOption,
		) error {
			if ac, ok := obj.(client.Object); ok && ac.GetName() == "controlplane-nova-hypervisor-operator-auth" {
				return injected
			}
			return cl.Apply(ctx, obj, opts...)
		},
	}, nil)

	_, err := convergeHVO(t, f.r, f.cp)

	g.Expect(err).To(MatchError(injected))
	g.Expect(err.Error()).To(ContainSubstring(`into namespace "openstack" on cluster "compute-a"`))
	cond := novaCondition(t, f.cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonHypervisorOperatorError))
}

// TestReconcileNovaHypervisorOperator_UnresolvableTarget pins a compute
// cluster that does not resolve: a wait on TargetClusterUnavailable, not a
// failed reconcile.
func TestReconcileNovaHypervisorOperator_UnresolvableTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	r := newNovaTestReconciler(t, cp, readyNovaRegistration(cp), readyHVORegistration(cp),
		hvoCredentials(cp, hvoPassword("s3cret")))
	r.Resolver = perClusterResolver{}

	res, halt, err := r.reconcileNovaHypervisorOperator(context.Background(), cp, []computeConfigMirrorTarget{
		{ClusterRef: &commonv1.TargetClusterRefSpec{Name: "compute-x"}, Namespace: cp.NovaNamespace()},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring(`cluster "compute-x" not found`))
}

// TestNovaHypervisorOperator_OutsideTheControlPlaneNamespace pins both legs
// for a Nova outside the ControlPlane's namespace. No owner reference crosses
// into it, so the auth Secret carries the ownership labels and the prune
// recognizes it by them. A placed Nova keeps everything on its own cluster:
// the credentials are materialized and read there, the auth Secret is written
// there alone, and the prune deletes both there.
func TestNovaHypervisorOperator_OutsideTheControlPlaneNamespace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		placed bool
	}{
		{name: "a dedicated namespace on the management cluster"},
		{name: "a Nova placed on a target cluster", placed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cp := hvoControlPlane()
			cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
			local := []client.Object{cp, readyHVORegistration(cp)}
			var remote []client.Object
			creds := hvoCredentials(cp, hvoPassword("s3cret"))
			if tc.placed {
				cp.Spec.Services.Nova.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nova-cluster"}
				remote = append(remote, readyTenantSecretStore(esoTenantStoreName, "compute", "", ""), creds)
			} else {
				local = append(local, creds)
			}
			r := newNovaTestReconciler(t, local...)
			novaSide := r.Client
			if tc.placed {
				novaSide = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(remote...).Build()
				r.Resolver = perClusterResolver{"nova-cluster": novaSide}
			}
			authKey := hvoAuthKey(cp, "compute")
			esKey := types.NamespacedName{Name: novaHypervisorOperatorCredentialsSecretName(cp), Namespace: "compute"}

			_, halt, err := r.reconcileNovaHypervisorOperator(ctx, cp, nil)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(halt).To(BeFalse())
			auth := &corev1.Secret{}
			g.Expect(novaSide.Get(ctx, authKey, auth)).To(Succeed())
			g.Expect(string(auth.Data["password"])).To(Equal("s3cret"))
			g.Expect(auth.OwnerReferences).To(BeEmpty(), "no owner reference crosses namespaces")
			g.Expect(auth.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "controlplane"))
			g.Expect(auth.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "openstack"))
			if tc.placed {
				for k, v := range remoteChildLabels(cp) {
					g.Expect(auth.Labels).To(HaveKeyWithValue(k, v))
				}
				g.Expect(apierrors.IsNotFound(r.Get(ctx, authKey, &corev1.Secret{}))).To(BeTrue(),
					"nothing is written on the management cluster, where no hypervisor operator reads it")
				g.Expect(novaSide.Get(ctx, esKey, &esov1.ExternalSecret{})).To(Succeed(),
					"the credentials are materialized on the Nova's cluster")
			}

			cp.Spec.Services.Nova.HypervisorOperator = nil
			_, halt, err = r.pruneNovaHypervisorOperator(ctx, cp, nil)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(halt).To(BeFalse())
			g.Expect(apierrors.IsNotFound(novaSide.Get(ctx, authKey, &corev1.Secret{}))).To(BeTrue(),
				"the auth Secret is recognized by its labels and deleted")
			g.Expect(apierrors.IsNotFound(novaSide.Get(ctx, esKey, &esov1.ExternalSecret{}))).To(BeTrue(),
				"no ExternalSecret is left to serve the deleted user's password to a block set again")
		})
	}
}

// TestNovaHypervisorOperator_UnresolvableNovaCluster pins a placed Nova whose
// cluster does not resolve: both legs wait on TargetClusterUnavailable with
// the resolver's text instead of failing the reconcile.
func TestNovaHypervisorOperator_UnresolvableNovaCluster(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prune       bool
		wantRequeue time.Duration
	}{
		{name: "the reconcile leg", wantRequeue: korcRequeueAfter},
		{name: "the prune", prune: true, wantRequeue: infraRequeueAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := hvoControlPlane()
			cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
			cp.Spec.Services.Nova.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "nova-cluster"}
			r := newNovaTestReconciler(t, cp, readyHVORegistration(cp))
			r.Resolver = perClusterResolver{}
			leg := r.reconcileNovaHypervisorOperator
			if tc.prune {
				cp.Spec.Services.Nova.HypervisorOperator = nil
				leg = r.pruneNovaHypervisorOperator
			}

			res, halt, err := leg(context.Background(), cp, nil)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(halt).To(BeTrue())
			g.Expect(res.RequeueAfter).To(Equal(tc.wantRequeue))
			cond := novaCondition(t, cp)
			g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
			g.Expect(cond.Message).To(ContainSubstring(`cluster "nova-cluster" not found`))
		})
	}
}

// TestReconcileNovaHypervisorOperator_RefusesAForeignSecret pins the adoption
// guard on both writes. The auth Secret carries a cloud-admin password, so a
// same-named Secret somebody else wrote beside the Nova or on a compute
// cluster must be neither overwritten nor claimed for the teardown to delete.
func TestReconcileNovaHypervisorOperator_RefusesAForeignSecret(t *testing.T) {
	// The UID is what an API server stamps on every object; the fake client
	// does not, and a target cluster's claim refuses only an object that has one.
	foreignIn := func(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: cp.NovaNamespace(),
				UID: types.UID("foreign-secret-uid"),
			},
			Data: map[string][]byte{"theirs": []byte("keep me")},
		}
	}
	untouched := func(g Gomega, c client.Reader, foreign *corev1.Secret) {
		live := &corev1.Secret{}
		g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), live)).To(Succeed())
		g.Expect(live.Data).To(Equal(foreign.Data), "the foreign Secret's data must be untouched")
		g.Expect(live.Labels).NotTo(HaveKey(controlPlaneNameLabel), "nor may it be marked for the teardown")
	}

	t.Run("beside the Nova, in a dedicated namespace", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := hvoControlPlane()
		cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}
		foreign := foreignIn(cp)
		r := newNovaTestReconciler(t, cp, readyHVORegistration(cp), hvoCredentials(cp, hvoPassword("s3cret")), foreign)

		_, halt, err := r.reconcileNovaHypervisorOperator(context.Background(), cp, nil)

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt")))
		g.Expect(novaCondition(t, cp).Reason).To(Equal(reasonHypervisorOperatorError))
		untouched(g, r.Client, foreign)
	})

	t.Run("on a compute cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		foreign := foreignIn(hvoControlPlane())
		f := newHVOFleet(t, interceptor.Funcs{}, map[string][]client.Object{"compute-a": {foreign}})

		_, err := convergeHVO(t, f.r, f.cp)

		g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt")))
		g.Expect(err.Error()).To(ContainSubstring(`on cluster "compute-a"`))
		g.Expect(novaCondition(t, f.cp).Reason).To(Equal(reasonHypervisorOperatorError))
		untouched(g, f.clusters["compute-a"], foreign)
	})
}

// TestReconcileNovaHypervisorOperator_RotationReachesEveryCopy pins the
// rotation: a new password in the consumer Secret reaches the source and every
// mirror on the next pass.
func TestReconcileNovaHypervisorOperator_RotationReachesEveryCopy(t *testing.T) {
	g := NewGomegaWithT(t)
	f := newHVOFleet(t, interceptor.Funcs{}, nil)
	ctx := context.Background()
	_, err := convergeHVO(t, f.r, f.cp)
	g.Expect(err).NotTo(HaveOccurred())

	creds := hvoCredentials(f.cp, nil)
	g.Expect(f.r.Get(ctx, client.ObjectKeyFromObject(creds), creds)).To(Succeed())
	creds.Data = hvoPassword("rotated")
	g.Expect(f.r.Update(ctx, creds)).To(Succeed())

	_, err = f.r.reconcileNova(ctx, f.cp)
	g.Expect(err).NotTo(HaveOccurred())

	source := &corev1.Secret{}
	g.Expect(f.r.Get(ctx, hvoAuthKey(f.cp, "openstack"), source)).To(Succeed())
	g.Expect(string(source.Data["password"])).To(Equal("rotated"))
	for _, name := range []string{"compute-a", "compute-b"} {
		mirror, err := f.authOn(t, name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(mirror.Data["password"])).To(Equal("rotated"), "the mirror on %s", name)
	}
}

// TestPruneNovaHypervisorOperator_RemovesWhatItWrote pins the removal: once
// the block is cleared the registration, the source and each labelled mirror
// go, a target Secret without the mirror label stays, and a second pass over
// nothing is no error.
func TestPruneNovaHypervisorOperator_RemovesWhatItWrote(t *testing.T) {
	g := NewGomegaWithT(t)
	f := newHVOFleet(t, interceptor.Funcs{}, nil)
	ctx := context.Background()
	_, err := convergeHVO(t, f.r, f.cp)
	g.Expect(err).NotTo(HaveOccurred())

	// The mirror on compute-b loses its mirror label: it now reads as a Secret
	// the ControlPlane wrote there for another reason, not as a mirror.
	unmarked, err := f.authOn(t, "compute-b")
	g.Expect(err).NotTo(HaveOccurred())
	delete(unmarked.Labels, novav1alpha1.ComputeConfigMirrorLabel)
	g.Expect(f.clusters["compute-b"].Update(ctx, unmarked)).To(Succeed())

	f.cp.Spec.Services.Nova.HypervisorOperator = nil
	res, err := f.r.reconcileNova(ctx, f.cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(novaCondition(t, f.cp).Status).To(Equal(metav1.ConditionTrue))

	g.Expect(apierrors.IsNotFound(f.r.Get(ctx, client.ObjectKeyFromObject(desiredNovaHypervisorOperatorRegistration(f.cp)),
		&c5c3v1alpha1.KeystoneService{}))).To(BeTrue(), "the registration is deleted, and Keystone with it")
	g.Expect(apierrors.IsNotFound(f.r.Get(ctx, hvoAuthKey(f.cp, "openstack"), &corev1.Secret{}))).To(BeTrue(),
		"the source is deleted")
	_, err = f.authOn(t, "compute-a")
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the labelled mirror is deleted")
	_, err = f.authOn(t, "compute-b")
	g.Expect(err).NotTo(HaveOccurred(), "a Secret without the mirror label is not a mirror")

	_, err = f.r.reconcileNova(ctx, f.cp)
	g.Expect(err).NotTo(HaveOccurred(), "a prune over objects that are already gone is no error")
}

// TestPruneNovaHypervisorOperator_KeepsForeignSecrets pins the ownership gate:
// a same-named source or mirror this ControlPlane did not write survives the
// prune.
func TestPruneNovaHypervisorOperator_KeepsForeignSecrets(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	foreignSource := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaHypervisorOperatorAuthSecretName(cp), Namespace: "openstack"},
		Data:       map[string][]byte{"theirs": []byte("keep me")},
	}
	foreignMirror := foreignSource.DeepCopy()
	foreignMirror.Labels = map[string]string{novav1alpha1.ComputeConfigMirrorLabel: "true"}
	f := newHVOFleet(t, interceptor.Funcs{}, map[string][]client.Object{"compute-a": {foreignMirror}}, foreignSource)
	f.cp.Spec.Services.Nova.HypervisorOperator = nil
	ctx := context.Background()

	_, err := convergeHVO(t, f.r, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f.r.Get(ctx, client.ObjectKeyFromObject(foreignSource), &corev1.Secret{})).To(Succeed(),
		"a Secret without the ControlPlane's ownership is never deleted")
	_, err = f.authOn(t, "compute-a")
	g.Expect(err).NotTo(HaveOccurred(), "a mirror label alone does not make a Secret the ControlPlane's")
	g.Expect(f.r.Get(ctx, client.ObjectKeyFromObject(desiredNovaHypervisorOperatorRegistration(f.cp)),
		&c5c3v1alpha1.KeystoneService{})).To(Succeed(),
		"a registration this ControlPlane never applied is not pruned, nor the Keystone user behind it")
}

// TestPruneNovaHypervisorOperator_ReadsComputeClustersUncached pins where the
// prune reads a mirror from. It runs on every Nova pass while the block is
// unset, the default, so a read through a compute cluster's cached client
// would start an informer over every Secret there. The cached client here
// fails every Secret read, so the prune only converges through the uncached
// reader.
func TestPruneNovaHypervisorOperator_ReadsComputeClustersUncached(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := hvoControlPlane()
	cp.Spec.Services.Nova.HypervisorOperator = nil
	r := newNovaTestReconciler(t, cp)
	mirror := desiredNovaHypervisorOperatorAuthSecret(cp, []byte("s3cret"))
	mirror.Labels[novav1alpha1.ComputeConfigMirrorLabel] = "true"
	live := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(mirror).Build()
	cached := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				return errors.New("no informer for Secret has synced")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	r.Resolver = &childrenResolver{children: cached, reader: live}

	_, halt, err := r.pruneNovaHypervisorOperator(ctx, cp, []computeConfigMirrorTarget{
		{ClusterRef: &commonv1.TargetClusterRefSpec{Name: "compute-a"}, Namespace: cp.NovaNamespace()},
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(apierrors.IsNotFound(live.Get(ctx, client.ObjectKeyFromObject(mirror), &corev1.Secret{}))).To(BeTrue(),
		"the mirror must be deleted after an uncached read")
}

// TestPruneNovaHypervisorOperator_DeleteFailure pins a failed delete in the
// prune: a failed reconcile that wraps the cause.
func TestPruneNovaHypervisorOperator_DeleteFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := hvoControlPlane()
	injected := errors.New("injected delete failure")
	r := newHVOTestReconciler(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == novaHypervisorOperatorAuthSecretName(cp) {
				return injected
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}, cp, readyNovaRegistration(cp), desiredNovaHypervisorOperatorAuthSecret(cp, []byte("s3cret")))
	cp.Spec.Services.Nova.HypervisorOperator = nil

	_, err := convergeHVO(t, r, cp)

	g.Expect(err).To(MatchError(injected))
	cond := novaCondition(t, cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonHypervisorOperatorError))
}
