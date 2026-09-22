// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the generated metadata shared secret
// (reconcile_nova_metadata_secret.go): the Password generator and the
// ExternalSecret that draws from it, the reference the projection resolves, and
// the reap a ControlPlane-supplied Secret leads to.
package controller

import (
	"context"
	"errors"
	"testing"

	esov1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esgenv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// getNovaMetadataGenerator / getNovaMetadataExternalSecret fetch the generated
// pair at its derived name/namespace.
func getNovaMetadataGenerator(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) (*esgenv1alpha1.Password, error) {
	t.Helper()
	gen := &esgenv1alpha1.Password{}
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: cp.NovaNamespace(), Name: novaMetadataSecretName(cp)}, gen)
	return gen, err
}

func getNovaMetadataExternalSecret(
	t *testing.T, c client.Client, cp *c5c3v1alpha1.ControlPlane,
) (*esov1.ExternalSecret, error) {
	t.Helper()
	es := &esov1.ExternalSecret{}
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: cp.NovaNamespace(), Name: novaMetadataSecretName(cp)}, es)
	return es, err
}

// TestNovaMetadataSecretBuilders pins the two objects the generation rests on:
// a 32-character symbol-free Password, and an ExternalSecret whose sole source
// is that generator and whose refresh is off, because a regenerated shared
// secret would reject every metadata request until each agent was reconfigured.
func TestNovaMetadataSecretBuilders(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "compute"}

	gen := novaMetadataPasswordGenerator(cp)
	g.Expect(gen.Name).To(Equal("cp-nova-metadata-secret"))
	g.Expect(gen.Namespace).To(Equal("compute"), "the generator lands beside the child that consumes it")
	g.Expect(gen.Spec.Length).To(Equal(32))
	g.Expect(gen.Spec.Symbols).To(Equal(ptr.To(0)),
		"the value is rendered into two INI files, so it carries no symbols to quote")
	g.Expect(gen.Spec.AllowRepeat).To(BeTrue())
	g.Expect(gen.Spec.Digits).To(BeNil())
	g.Expect(gen.Spec.SecretKeys).To(BeEmpty(),
		"the ESO 0.x generator the stack deploys knows no secretKeys; the ExternalSecret renames its key")
	g.Expect(gen.Spec.Encoding).To(BeNil())

	es := novaMetadataExternalSecret(cp)
	g.Expect(es.Name).To(Equal("cp-nova-metadata-secret"))
	g.Expect(es.Namespace).To(Equal("compute"))
	g.Expect(es.Spec.RefreshInterval).To(Equal(&metav1.Duration{Duration: 0}),
		"the value is generated exactly once; a refresh would mint a new one")
	g.Expect(es.Spec.Target.Name).To(Equal("cp-nova-metadata-secret"))
	g.Expect(es.Spec.Target.CreationPolicy).To(Equal(esov1.CreatePolicyOwner))
	g.Expect(es.Spec.SecretStoreRef.Name).To(BeEmpty(),
		"a generator-backed ExternalSecret must not reference a SecretStore")
	g.Expect(es.Spec.Data).To(BeEmpty())
	g.Expect(es.Spec.DataFrom).To(HaveLen(1))
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef).To(Equal(&esov1.GeneratorRef{
		APIVersion: "generators.external-secrets.io/v1alpha1",
		Kind:       "Password",
		Name:       "cp-nova-metadata-secret",
	}))
	// Both consumers default their reference's key to shared_secret, so the
	// generator's "password" key is renamed to it: an agent written by hand that
	// names only the Secret reaches the value.
	g.Expect(es.Spec.DataFrom[0].Rewrite).To(Equal([]esov1.ExternalSecretRewrite{{
		Regexp: &esov1.ExternalSecretRewriteRegexp{Source: "^password$", Target: "shared_secret"},
	}}))
}

// TestEffectiveNovaMetadataSharedSecretRef pins which reference the projection
// hands the child: the generated pair by default, and the ControlPlane's own
// Secret when it names one. Clearing that reference reverts to the generated
// pair rather than pinning the last value.
func TestEffectiveNovaMetadataSharedSecretRef(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()

	g.Expect(effectiveNovaMetadataSharedSecretRef(cp)).To(Equal(commonv1.SecretRefSpec{
		Name: "cp-nova-metadata-secret", Key: "shared_secret",
	}))

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{
		Name: "seeded-metadata", Key: "shared_secret",
	}
	g.Expect(effectiveNovaMetadataSharedSecretRef(cp)).To(Equal(commonv1.SecretRefSpec{
		Name: "seeded-metadata", Key: "shared_secret",
	}))

	cp.Spec.Services.Nova.MetadataSharedSecretRef = nil
	g.Expect(effectiveNovaMetadataSharedSecretRef(cp).Name).To(Equal("cp-nova-metadata-secret"),
		"clearing the reference reverts to the generated pair")
}

// newNovaMetadataReconciler wires a reconciler over the seeded objects, with the
// ESO generator types registered so the fake client can serve a Password.
func newNovaMetadataReconciler(t *testing.T, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := novaTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// TestReconcileNovaMetadataSecret_GeneratesThePair covers the default: the
// ControlPlane names no Secret, so it writes the generator and the ExternalSecret
// into the Nova namespace and does not wait for the value to materialise, which
// is the child's own gate.
func TestReconcileNovaMetadataSecret_GeneratesThePair(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaMetadataReconciler(t, cp)

	res, halt, err := r.reconcileNovaMetadataSecret(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse(), "the leg must not wait for the materialised Secret")
	g.Expect(res.IsZero()).To(BeTrue())

	gen, err := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gen.Spec.Length).To(Equal(32))
	g.Expect(gen.Spec.Symbols).To(Equal(ptr.To(0)))
	g.Expect(metav1.IsControlledBy(gen, cp)).To(BeTrue(),
		"a co-located generator carries the ControlPlane controller owner reference")

	es, err := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(es.Spec.RefreshInterval.Duration).To(BeZero())
	g.Expect(es.Spec.DataFrom[0].SourceRef.GeneratorRef.Kind).To(Equal("Password"))

	// The Secret behind the pair is ESO's to materialise, so nothing writes it here.
	g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)).To(BeNil(),
		"a leg that wrote what it had to write parks no condition")
}

// TestReconcileNovaMetadataSecret_LandsInTheNovaNamespace covers the placed
// service: both objects follow the child into the namespace it was assigned,
// carrying the ownership labels rather than an owner reference, and nothing is
// left in the ControlPlane's own namespace.
func TestReconcileNovaMetadataSecret_LandsInTheNovaNamespace(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	r := newNovaMetadataReconciler(t, cp)
	ctx := context.Background()

	_, halt, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	gen, err := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gen.Namespace).To(Equal("compute"))
	g.Expect(gen.OwnerReferences).To(BeEmpty(), "a cross-namespace object cannot carry an owner reference")
	g.Expect(gen.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))

	es, err := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(es.Labels).To(HaveKeyWithValue(controlPlaneNamespaceLabel, "default"))

	g.Expect(r.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: novaMetadataSecretName(cp),
	}, &esgenv1alpha1.Password{})).NotTo(Succeed(),
		"nothing may be left in the ControlPlane's own namespace")
}

// TestReconcileNovaMetadataSecret_OverrideLeavesThePairForTheReap covers the
// ControlPlane that starts generating the value and then names a Secret of its
// own: this leg writes nothing more and deletes nothing either. The live metadata
// Deployment keeps sourcing its env from the generated Secret until the child
// has converged on the new reference, so the pair is reaped behind that
// convergence and not here, ahead of the projection.
func TestReconcileNovaMetadataSecret_OverrideLeavesThePairForTheReap(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaMetadataReconciler(t, cp)
	ctx := context.Background()

	_, _, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	_, halt, err := r.reconcileNovaMetadataSecret(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(genErr).NotTo(HaveOccurred(), "the generator must outlive the child's switch to the override")
	_, esErr := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(esErr).NotTo(HaveOccurred(), "the ExternalSecret must outlive it too")
}

// TestReconcileNovaMetadataSecret_OverrideWritesNothing covers the ControlPlane
// that named a Secret from the start: nothing is generated.
func TestReconcileNovaMetadataSecret_OverrideWritesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{
		Name: "seeded-metadata", Key: "shared_secret",
	}
	r := newNovaMetadataReconciler(t, cp)

	_, halt, err := r.reconcileNovaMetadataSecret(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(genErr)).To(BeTrue(), "an overridden ControlPlane generates nothing")
	_, esErr := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(esErr)).To(BeTrue())
}

// TestNovaMetadataSecret_OverrideNamingTheGeneratedSecretKeepsIt covers a
// reference that names the generated Secret itself, for instance to adopt its
// value under a key of the ControlPlane's choosing. The child still reads the
// generated Secret, so the pair keeps being ensured and is never reaped: the
// ExternalSecret's owner reference would otherwise take the very Secret the
// child now names down with it.
func TestNovaMetadataSecret_OverrideNamingTheGeneratedSecretKeepsIt(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{
		Name: novaMetadataSecretName(cp), Key: "shared_secret",
	}
	r := newNovaMetadataReconciler(t, cp)
	ctx := context.Background()

	_, halt, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, halt, err = r.reapGeneratedNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(genErr).NotTo(HaveOccurred(), "the generator behind the named Secret must stay")
	_, esErr := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(esErr).NotTo(HaveOccurred(), "the ExternalSecret owning the named Secret must stay")
}

// TestReapGeneratedNovaMetadataSecret_DeletesTheOwnedPair covers the reap once
// the ControlPlane names a Secret of its own: a live generator behind an
// ExternalSecret nothing references leaves a second shared value in the
// namespace.
func TestReapGeneratedNovaMetadataSecret_DeletesTheOwnedPair(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	r := newNovaMetadataReconciler(t, cp)
	ctx := context.Background()

	_, _, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	// While the child still reads the generated pair, the reap is a no-op.
	_, halt, err := r.reapGeneratedNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(genErr).NotTo(HaveOccurred())

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	_, halt, err = r.reapGeneratedNovaMetadataSecret(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, genErr = getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(genErr)).To(BeTrue(), "the generator must come down with the override")
	_, esErr := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(esErr)).To(BeTrue(), "the ExternalSecret must come down with it")

	// A pair that is already gone converges instead of failing.
	_, halt, err = r.reapGeneratedNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
}

// TestReapGeneratedNovaMetadataSecret_LeavesAForeignPairAlone is the ownership
// guard on the reap: in a service namespace this ControlPlane does not own, a
// same-named Password and ExternalSecret belonging to somebody else survive.
func TestReapGeneratedNovaMetadataSecret_LeavesAForeignPairAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleExternal,
	}
	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	name := novaMetadataSecretName(cp)
	foreignGen := &esgenv1alpha1.Password{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "compute"},
	}
	foreignES := &esov1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "compute"},
	}
	r := newNovaMetadataReconciler(t, cp, foreignGen, foreignES)

	_, halt, err := r.reapGeneratedNovaMetadataSecret(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(genErr).NotTo(HaveOccurred(), "a Password we do not own must never be deleted")
	_, esErr := getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(esErr).NotTo(HaveOccurred(), "an ExternalSecret we do not own must never be deleted")
}

// TestReapGeneratedNovaMetadataSecret_ReadsThePasswordUncached pins where the
// generator is read from. Nothing watches the kind, so a read through the cached
// client starts an informer over every Password on the cluster, and on a target
// whose credentials cannot list the kind that informer never syncs and the read
// blocks the reconcile worker for good. The cached client here fails every
// Password read, so the reap only converges through the uncached reader.
func TestReapGeneratedNovaMetadataSecret_ReadsThePasswordUncached(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	gen := novaMetadataPasswordGenerator(cp)
	gen.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cp,
		c5c3v1alpha1.GroupVersion.WithKind("ControlPlane"))}
	s := novaTestScheme(t)
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, gen).Build()
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, gen).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, isPassword := obj.(*esgenv1alpha1.Password); isPassword {
					return errors.New("no informer for Password has synced")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: cached, APIReader: live, Scheme: s}

	_, halt, err := r.reapGeneratedNovaMetadataSecret(context.Background(), cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	var remaining esgenv1alpha1.PasswordList
	g.Expect(cached.List(context.Background(), &remaining)).To(Succeed())
	g.Expect(remaining.Items).To(BeEmpty(), "the owned generator must be deleted after an uncached read")
}

// TestReapGeneratedNovaMetadataSecret_PasswordReadErrors pins which Password
// read errors the reap absorbs. Forbidden is a target cluster whose access
// release predates the passwords grant: credentials that cannot read the kind
// never wrote a generator, so the reap converges on the ExternalSecret alone
// instead of holding NovaReady False on every pass. Any other read error still
// parks NovaReady and surfaces.
func TestReapGeneratedNovaMetadataSecret_PasswordReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readErr error
		wantErr string
	}{
		{
			name: "forbidden converges",
			readErr: apierrors.NewForbidden(
				schema.GroupResource{Group: esgenv1alpha1.Group, Resource: "passwords"},
				"cp-nova-metadata-secret", errors.New("no passwords grant")),
		},
		{
			name:    "any other error surfaces",
			readErr: apierrors.NewInternalError(errors.New("etcd is unavailable")),
			wantErr: "etcd is unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
			es := novaMetadataExternalSecret(cp)
			es.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(cp,
				c5c3v1alpha1.GroupVersion.WithKind("ControlPlane"))}
			s := novaTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp, es).Build()
			reader := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
						opts ...client.GetOption,
					) error {
						if _, isPassword := obj.(*esgenv1alpha1.Password); isPassword {
							return tc.readErr
						}
						return cl.Get(ctx, key, obj, opts...)
					},
				}).Build()
			r := &ControlPlaneReconciler{Client: c, APIReader: reader, Scheme: s}

			_, halt, err := r.reapGeneratedNovaMetadataSecret(context.Background(), cp)

			_, esErr := getNovaMetadataExternalSecret(t, c, cp)
			g.Expect(apierrors.IsNotFound(esErr)).To(BeTrue(), "the owned ExternalSecret is deleted ahead of the read")
			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(halt).To(BeFalse())
				g.Expect(conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)).To(BeNil())
				return
			}
			g.Expect(err).To(MatchError(ContainSubstring(tc.wantErr)))
			g.Expect(halt).To(BeTrue())
			cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("NovaMetadataSecretError"))
		})
	}
}

// TestReapGeneratedNovaMetadataSecret_DeleteErrorSurfaces pins the failure leg of
// the reap: a delete that fails parks NovaReady on NovaMetadataSecretError and
// returns the error, rather than reporting a converged override over a generator
// that is still live.
func TestReapGeneratedNovaMetadataSecret_DeleteErrorSurfaces(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	s := novaTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, isES := obj.(*esov1.ExternalSecret); isES {
					return apierrors.NewInternalError(errors.New("etcd is unavailable"))
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	ctx := context.Background()
	_, _, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	res, halt, err := r.reapGeneratedNovaMetadataSecret(ctx, cp)

	g.Expect(err).To(MatchError(ContainSubstring("etcd is unavailable")))
	g.Expect(halt).To(BeTrue())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NovaMetadataSecretError"))
	_, genErr := getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(genErr).NotTo(HaveOccurred(), "the generator must stay while its consumer could not be deleted")
}

// TestNovaMetadataSecret_PlacedLandsOnTheTargetCluster covers a Nova on another
// cluster: the pair is written where the child and the ESO that runs the
// generator live, on the target, and nothing is written on the management
// cluster, where the child would never find it. The reap deletes it there too.
func TestNovaMetadataSecret_PlacedLandsOnTheTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Nova.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	s := novaTestScheme(t)
	target := fake.NewClientBuilder().WithScheme(s).Build()
	r := &ControlPlaneReconciler{
		Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
		Scheme:   s,
		Resolver: &childrenResolver{children: target},
	}
	ctx := context.Background()

	_, halt, err := r.reconcileNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	gen, err := getNovaMetadataGenerator(t, target, cp)
	g.Expect(err).NotTo(HaveOccurred(), "the generator lands on the cluster the child runs on")
	g.Expect(gen.Labels).To(HaveKeyWithValue(controlPlaneNameLabel, "cp"))
	_, err = getNovaMetadataExternalSecret(t, target, cp)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = getNovaMetadataGenerator(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing may be written on the management cluster")
	_, err = getNovaMetadataExternalSecret(t, r.Client, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	cp.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}
	_, halt, err = r.reapGeneratedNovaMetadataSecret(ctx, cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	_, err = getNovaMetadataGenerator(t, target, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the reap deletes on the target cluster")
	_, err = getNovaMetadataExternalSecret(t, target, cp)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// TestNovaMetadataSecret_UnresolvableClusterHolds covers a Nova whose target
// cluster does not resolve: both the generation and the reap hold the pass on
// TargetClusterUnavailable, a wait rather than a failed reconcile, and write
// nothing anywhere.
func TestNovaMetadataSecret_UnresolvableClusterHolds(t *testing.T) {
	cp := novaControlPlane()
	cp.Spec.Services.Nova.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{
		Name: "compute", Lifecycle: c5c3v1alpha1.ServiceNamespaceLifecycleManaged,
	}
	cp.Spec.Services.Nova.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge"}
	overridden := cp.DeepCopy()
	overridden.Spec.Services.Nova.MetadataSharedSecretRef = &commonv1.SecretRefSpec{Name: "seeded-metadata"}

	for name, tc := range map[string]struct {
		cp  *c5c3v1alpha1.ControlPlane
		leg func(*ControlPlaneReconciler, *c5c3v1alpha1.ControlPlane) (ctrl.Result, bool, error)
	}{
		"the generation": {cp, func(r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, bool, error) {
			return r.reconcileNovaMetadataSecret(context.Background(), cp)
		}},
		"the reap": {overridden, func(r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) (ctrl.Result, bool, error) {
			return r.reapGeneratedNovaMetadataSecret(context.Background(), cp)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := tc.cp.DeepCopy()
			s := novaTestScheme(t)
			target := fake.NewClientBuilder().WithScheme(s).Build()
			r := &ControlPlaneReconciler{
				Client:   fake.NewClientBuilder().WithScheme(s).WithObjects(cp).Build(),
				Scheme:   s,
				Resolver: &childrenResolver{children: target, err: mcruntime.ErrClusterNotFound},
			}

			res, halt, err := tc.leg(r, cp)

			g.Expect(err).NotTo(HaveOccurred(), "an unregistered cluster is a state to wait out")
			g.Expect(halt).To(BeTrue())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
			for clusterName, c := range map[string]client.Client{"management": r.Client, "target": target} {
				var gens esgenv1alpha1.PasswordList
				g.Expect(c.List(context.Background(), &gens)).To(Succeed())
				g.Expect(gens.Items).To(BeEmpty(), "no generator may be written on the %s cluster", clusterName)
			}
		})
	}
}

// TestReconcileNovaMetadataSecret_ApplyErrorSurfaces pins the failure leg: a
// write that fails parks NovaReady on NovaMetadataSecretError, halts the pass,
// and returns the error to the pipeline, so the projection never runs against a
// reference that resolves to nothing.
func TestReconcileNovaMetadataSecret_ApplyErrorSurfaces(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind string
	}{
		{name: "the generator cannot be written", kind: "Password"},
		{name: "the ExternalSecret cannot be written", kind: "ExternalSecret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			s := novaTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
				WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
						opts ...client.ApplyOption,
					) error {
						if ac, ok := obj.(client.Object); ok &&
							ac.GetObjectKind().GroupVersionKind().Kind == tt.kind {
							return apierrors.NewInternalError(errors.New("etcd is unavailable"))
						}
						return cl.Apply(ctx, obj, opts...)
					},
				}).Build()
			r := &ControlPlaneReconciler{Client: c, Scheme: s}

			res, halt, err := r.reconcileNovaMetadataSecret(context.Background(), cp)

			g.Expect(err).To(HaveOccurred())
			g.Expect(halt).To(BeTrue())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := conditions.GetCondition(cp.Status.Conditions, conditionTypeNovaReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("NovaMetadataSecretError"))
			g.Expect(cond.Message).To(ContainSubstring("etcd is unavailable"))
		})
	}
}
