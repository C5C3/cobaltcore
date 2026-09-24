// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// The Secret the platform hands the ControlPlane the external bus listener in,
// and the URL it carries.
const (
	novaRemoteTransportSecretName = "nova-remote-transport"
	novaRemoteTransportURL        = "rabbit://u:p@203.0.113.5:5671/"
)

// remoteComputeNovaControlPlane is novaControlPlane with services.nova.remoteCompute
// set and Keystone published through a gateway, the publication the remote
// contract's Keystone URL is derived from.
func remoteComputeNovaControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := novaControlPlane()
	cp.Spec.Services.Keystone.Gateway = &commonv1.GatewaySpec{
		Hostname:  "keystone.example.com",
		ParentRef: commonv1.GatewayParentRefSpec{Name: "openstack-gw"},
	}
	cp.Spec.Services.Nova.RemoteCompute = &c5c3v1alpha1.ServiceNovaRemoteComputeSpec{
		TransportURLSecretRef: commonv1.SecretRefSpec{Name: novaRemoteTransportSecretName},
	}
	return cp
}

// handedRemoteTransportSecret is the Secret the platform hands over, in the
// ControlPlane's own namespace, carrying value under the default key.
func handedRemoteTransportSecret(cp *c5c3v1alpha1.ControlPlane, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaRemoteTransportSecretName, Namespace: cp.Namespace},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(value)},
	}
}

// deliveredRemoteMessagingKey is where reconcileNovaRemoteMessaging writes the
// handed URL: beside the Nova child.
func deliveredRemoteMessagingKey(cp *c5c3v1alpha1.ControlPlane) types.NamespacedName {
	return types.NamespacedName{Name: novaRemoteMessagingSecretName(cp), Namespace: cp.NovaNamespace()}
}

// TestNovaRemoteComputeSpec pins what the child receives: nothing without the
// block, and otherwise the public Keystone URL the ControlPlane registers, beside
// the Secret it delivers the handed URL in.
func TestNovaRemoteComputeSpec(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(novaRemoteComputeSpec(novaControlPlane())).To(BeNil())

	cp := remoteComputeNovaControlPlane()
	g.Expect(novaRemoteComputeSpec(cp)).To(Equal(&novav1alpha1.NovaRemoteComputeSpec{
		KeystoneEndpoint: "https://keystone.example.com/v3",
		TransportURLSecretRef: commonv1.SecretRefSpec{
			Name: "cp-nova-remote-messaging", Key: commonv1.DefaultTransportURLSecretKey,
		},
	}))

	cp.Spec.Services.Keystone.PublicEndpoint = "https://identity.example.com:8443/v3"
	g.Expect(novaRemoteComputeSpec(cp).KeystoneEndpoint).To(Equal("https://identity.example.com:8443/v3"),
		"an explicit publicEndpoint wins over the gateway, as it does in the catalog")
}

func TestReconcileNovaRemoteMessaging_NoBlockWritesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaTestReconciler(t, cp)

	res, halt, err := r.reconcileNovaRemoteMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(context.Background(), deliveredRemoteMessagingKey(cp),
		&corev1.Secret{}))).To(BeTrue())
}

// TestReconcileNovaRemoteMessaging_DeliversTheHandedURL covers the delivery into
// a dedicated Nova namespace: the value lands under transport_url carrying the
// ownership labels, a ref without a key is read at transport_url, and a rotated
// value reaches the delivered Secret on the next pass.
func TestReconcileNovaRemoteMessaging_DeliversTheHandedURL(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := remoteComputeNovaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	g.Expect(cp.Spec.Services.Nova.RemoteCompute.TransportURLSecretRef.Key).To(BeEmpty())
	handed := handedRemoteTransportSecret(cp, novaRemoteTransportURL)
	r := newNovaTestReconciler(t, cp, handed)

	_, halt, err := r.reconcileNovaRemoteMessaging(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	delivered := &corev1.Secret{}
	g.Expect(r.Get(ctx, deliveredRemoteMessagingKey(cp), delivered)).To(Succeed())
	g.Expect(delivered.Namespace).To(Equal("compute"))
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey,
		[]byte(novaRemoteTransportURL)))
	g.Expect(delivered.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	g.Expect(delivered.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))

	const rotated = "rabbit://u:rotated@203.0.113.5:5671/"
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(handed), handed)).To(Succeed())
	handed.Data[commonv1.DefaultTransportURLSecretKey] = []byte(rotated)
	g.Expect(r.Update(ctx, handed)).To(Succeed())

	_, halt, err = r.reconcileNovaRemoteMessaging(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(r.Get(ctx, deliveredRemoteMessagingKey(cp), delivered)).To(Succeed())
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey, []byte(rotated)))
}

// TestReconcileNovaRemoteMessaging_WaitsForTheHandedSecret covers a handed
// Secret that is not there yet, and one whose key is still empty: both are
// waits, and nothing is written.
func TestReconcileNovaRemoteMessaging_WaitsForTheHandedSecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs func(cp *c5c3v1alpha1.ControlPlane) []client.Object
		want string
	}{
		{
			name: "missing Secret",
			objs: func(cp *c5c3v1alpha1.ControlPlane) []client.Object { return nil },
			want: "not found",
		},
		{
			name: "empty key",
			objs: func(cp *c5c3v1alpha1.ControlPlane) []client.Object {
				return []client.Object{handedRemoteTransportSecret(cp, "")}
			},
			want: `missing key "transport_url"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := remoteComputeNovaControlPlane()
			r := newNovaTestReconciler(t, append([]client.Object{cp}, tc.objs(cp)...)...)

			res, halt, err := r.reconcileNovaRemoteMessaging(context.Background(), cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(halt).To(BeTrue())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := novaCondition(t, cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonWaitingForRemoteMessaging))
			g.Expect(cond.Message).To(HavePrefix("services.nova.remoteCompute.transportURLSecretRef:"))
			g.Expect(cond.Message).To(ContainSubstring(tc.want))
			g.Expect(apierrors.IsNotFound(r.Get(context.Background(), deliveredRemoteMessagingKey(cp),
				&corev1.Secret{}))).To(BeTrue())
		})
	}
}

// TestReconcileNovaRemoteMessaging_RejectsANonRabbitURL covers a handed URL for
// a driver nova is not configured for. The error names the scheme and never the
// URL, which carries the broker password.
func TestReconcileNovaRemoteMessaging_RejectsANonRabbitURL(t *testing.T) {
	g := NewGomegaWithT(t)
	const amqp = "amqp://u:p@203.0.113.5:5671/"
	cp := remoteComputeNovaControlPlane()
	r := newNovaTestReconciler(t, cp, handedRemoteTransportSecret(cp, amqp))

	_, halt, err := r.reconcileNovaRemoteMessaging(context.Background(), cp)

	g.Expect(halt).To(BeTrue())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(HavePrefix("resolving the nova remote transport URL:"))
	g.Expect(err.Error()).To(ContainSubstring("scheme must be rabbit"))
	g.Expect(err.Error()).NotTo(ContainSubstring(amqp))
	cond := novaCondition(t, cp)
	g.Expect(cond.Reason).To(Equal(reasonNovaRemoteMessagingError))
}

// TestReconcileNovaRemoteMessaging_WriteFailure covers a refused write of the
// delivered Secret.
func TestReconcileNovaRemoteMessaging_WriteFailure(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := remoteComputeNovaControlPlane()
	s := novaTestScheme(t)
	boom := errors.New("the API server is unavailable")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, handedRemoteTransportSecret(cp, novaRemoteTransportURL)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetName() == novaRemoteMessagingSecretName(cp) {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, halt, err := r.reconcileNovaRemoteMessaging(context.Background(), cp)

	g.Expect(halt).To(BeTrue())
	g.Expect(err).To(MatchError(boom))
	g.Expect(err.Error()).To(HavePrefix("writing the nova remote messaging Secret default/cp-nova-remote-messaging:"))
	cond := novaCondition(t, cp)
	g.Expect(cond.Reason).To(Equal(reasonNovaRemoteMessagingError))
	g.Expect(cond.Message).To(ContainSubstring("the API server is unavailable"))
}

// TestReconcileNovaRemoteMessaging_UnresolvableCluster covers a Nova placed on a
// cluster that does not resolve: the resolver's text is relayed and the pass
// waits for the registration.
func TestReconcileNovaRemoteMessaging_UnresolvableCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := placedNovaControlPlane("compute-target")
	cp.Spec.Services.Nova.RemoteCompute = remoteComputeNovaControlPlane().Spec.Services.Nova.RemoteCompute
	r := newNovaTestReconciler(t, cp, handedRemoteTransportSecret(cp, novaRemoteTransportURL))
	r.Resolver = &childrenResolver{err: errors.New("cluster not found")}

	res, halt, err := r.reconcileNovaRemoteMessaging(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeTrue())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := novaCondition(t, cp)
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))
}

// TestReconcileNovaRemoteMessaging_PlacedNovaWritesOnTheTarget covers a Nova on a
// target cluster: the handed Secret is read at home, and the delivery is written
// through the target's client, where the nova operator reads it.
func TestReconcileNovaRemoteMessaging_PlacedNovaWritesOnTheTarget(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedNovaControlPlane("compute-target")
	cp.Spec.Services.Nova.RemoteCompute = remoteComputeNovaControlPlane().Spec.Services.Nova.RemoteCompute
	r := newNovaTestReconciler(t, cp, handedRemoteTransportSecret(cp, novaRemoteTransportURL))
	target := fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	resolver := &childrenResolver{children: target}
	r.Resolver = resolver

	_, halt, err := r.reconcileNovaRemoteMessaging(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(resolver.names).To(ContainElement(BeEquivalentTo("compute-target")))
	delivered := &corev1.Secret{}
	g.Expect(target.Get(ctx, deliveredRemoteMessagingKey(cp), delivered)).To(Succeed())
	g.Expect(delivered.Data).To(HaveKeyWithValue(commonv1.DefaultTransportURLSecretKey,
		[]byte(novaRemoteTransportURL)))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{}))).To(BeTrue(),
		"nothing may be written on the management cluster")
}

// reapableNova is the projected child as reapNovaRemoteMessaging reads it: at
// generation, having reconciled observedGeneration.
func reapableNova(cp *c5c3v1alpha1.ControlPlane, generation, observedGeneration int64) *novav1alpha1.Nova {
	nv := &novav1alpha1.Nova{
		ObjectMeta: metav1.ObjectMeta{Name: novaName(cp), Namespace: cp.NovaNamespace(), Generation: generation},
	}
	nv.Status.ObservedGeneration = observedGeneration
	return nv
}

// deliveredRemoteMessagingSecret is a delivered Secret carrying the ownership
// labels the reap recognizes this ControlPlane's children by.
func deliveredRemoteMessagingSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: novaRemoteMessagingSecretName(cp), Namespace: cp.NovaNamespace()},
		Data:       map[string][]byte{commonv1.DefaultTransportURLSecretKey: []byte(novaRemoteTransportURL)},
	}
	stampControlPlaneChildLabels(secret, cp)
	return secret
}

// TestReapNovaRemoteMessaging covers the reap: it waits while the block is set
// and while the child has not reconciled the spec without spec.remoteCompute,
// deletes the delivered Secret once it has, and leaves a same-named Secret it
// did not write alone.
func TestReapNovaRemoteMessaging(t *testing.T) {
	ctx := context.Background()

	t.Run("kept while the block is set", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := remoteComputeNovaControlPlane()
		r := newNovaTestReconciler(t, cp, deliveredRemoteMessagingSecret(cp))

		_, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())
		g.Expect(r.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{})).To(Succeed())
	})

	t.Run("kept until the child has converged", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		r := newNovaTestReconciler(t, cp, deliveredRemoteMessagingSecret(cp))

		_, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 1))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())
		g.Expect(r.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{})).To(Succeed(),
			"the nova operator still reads the Secret until it has reconciled the drop")

		_, halt, err = r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())
		g.Expect(apierrors.IsNotFound(r.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{}))).To(BeTrue())
	})

	t.Run("a Secret without the ownership labels survives", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		foreign := deliveredRemoteMessagingSecret(cp)
		foreign.Labels = nil
		r := newNovaTestReconciler(t, cp, foreign)

		_, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())
		g.Expect(r.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{})).To(Succeed())
	})

	// The delivery of a placed Nova sits on the target cluster, so the reap has
	// to delete it there: left behind, it keeps the broker password on a
	// cluster nothing reads it on any more.
	t.Run("a placed Nova's Secret is deleted on the target", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedNovaControlPlane("compute-target")
		r := newNovaTestReconciler(t, cp)
		target := fake.NewClientBuilder().WithScheme(r.Scheme).
			WithObjects(deliveredRemoteMessagingSecret(cp)).Build()
		resolver := &childrenResolver{children: target}
		r.Resolver = resolver

		_, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeFalse())
		g.Expect(resolver.names).To(ContainElement(BeEquivalentTo("compute-target")))
		g.Expect(apierrors.IsNotFound(target.Get(ctx, deliveredRemoteMessagingKey(cp), &corev1.Secret{}))).
			To(BeTrue())
	})

	t.Run("an unresolvable cluster waits for the registration", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedNovaControlPlane("compute-target")
		r := newNovaTestReconciler(t, cp)
		r.Resolver = &childrenResolver{err: errors.New("cluster not found")}

		res, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeTrue())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
		cond := novaCondition(t, cp)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring("cluster not found"))
	})

	t.Run("a failing delete is reported", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		s := novaTestScheme(t)
		boom := errors.New("the API server is unavailable")
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(cp, deliveredRemoteMessagingSecret(cp)).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object,
					opts ...client.DeleteOption,
				) error {
					if obj.GetName() == novaRemoteMessagingSecretName(cp) {
						return boom
					}
					return cl.Delete(ctx, obj, opts...)
				},
			}).
			Build()
		r := &ControlPlaneReconciler{Client: c, Scheme: s}

		_, halt, err := r.reapNovaRemoteMessaging(ctx, cp, reapableNova(cp, 2, 2))

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(MatchError(boom))
		g.Expect(err.Error()).To(HavePrefix(
			"deleting the stale nova remote messaging Secret default/cp-nova-remote-messaging:"))
		g.Expect(novaCondition(t, cp).Reason).To(Equal(reasonNovaRemoteMessagingError))
	})
}
