// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package apply

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcruntime "sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
}

func testConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
		},
		Data: map[string]string{"key": "value"},
	}
}

func TestEnsureObject_createsWithOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	cm := testConfigMap()
	g.Expect(EnsureObject(context.Background(), c, s, owner, cm, FieldManager)).To(Succeed())

	// The caller's object is overwritten with the server response, so it carries
	// a resourceVersion after a successful apply.
	g.Expect(cm.ResourceVersion).NotTo(BeEmpty())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
	g.Expect(fetched.OwnerReferences[0].Name).To(Equal("test-owner"))
	g.Expect(*fetched.OwnerReferences[0].Controller).To(BeTrue())
}

func TestEnsureObject_usesServerSideApplyOptions(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	var fieldManager string
	var forced bool
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				ao := &client.ApplyOptions{}
				ao.ApplyOptions(opts)
				fieldManager = ao.FieldManager
				forced = ao.Force != nil && *ao.Force
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())
	g.Expect(fieldManager).To(Equal(FieldManager))
	g.Expect(forced).To(BeTrue(), "apply must set ForceOwnership")
}

func TestEnsureObject_retriesOnConflict(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	applyCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				applyCalls++
				if applyCalls == 1 {
					// A benign field-manager conflict must be retried, not surfaced.
					return apierrors.NewConflict(
						schema.GroupResource{Resource: "configmaps"}, "applied-cm",
						apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "applied-cm", nil),
					)
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())
	g.Expect(applyCalls).To(Equal(2), "conflict on the first apply must be retried once and then succeed")
}

func TestEnsureObject_propagatesApplyError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "applied-cm", nil)
			},
		}).
		Build()

	err := EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue())
}

func TestEnsureObject_errorsWhenGVKUnknown(t *testing.T) {
	g := NewGomegaWithT(t)
	// Scheme knows the owner (ConfigMap) but not the applied type (CronJob), so
	// GVK resolution for the applied object fails before any apply is attempted.
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	owner := testOwner()

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(owner).Build()

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "cj", Namespace: "default"},
	}
	err := EnsureObject(context.Background(), c, s, owner, cronJob, FieldManager)
	g.Expect(err).To(HaveOccurred())
}

// TestEnsureUnownedObject_appliesWithoutOwnerReference pins the cross-namespace
// apply path: the object lands with no owner reference at all, so nothing
// garbage-collects it — the caller carries ownership via labels and an explicit
// teardown instead.
func TestEnsureUnownedObject_appliesWithoutOwnerReference(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	cm := testConfigMap()
	cm.Namespace = "elsewhere"
	g.Expect(EnsureUnownedObject(context.Background(), c, s, cm, FieldManager)).To(Succeed())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.OwnerReferences).To(BeEmpty(), "a cross-namespace child must carry no owner reference")
}

// TestEnsureObject_rejectsCrossNamespaceOwner is the reason EnsureUnownedObject
// exists: Kubernetes garbage collection does not cascade across namespaces, so
// SetControllerReference refuses a foreign-namespace child — the apply never even
// reaches the API server.
func TestEnsureObject_rejectsCrossNamespaceOwner(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner() // namespace "default"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	cm := testConfigMap()
	cm.Namespace = "elsewhere"
	err := EnsureObject(context.Background(), c, s, owner, cm, FieldManager)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("setting owner reference"))

	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{})).NotTo(Succeed(),
		"nothing may be applied once the owner reference is refused")
}

// testOwnerLabels are the ownership labels a child of testOwner carries on a
// target cluster.
func testOwnerLabels() map[string]string {
	return map[string]string{
		multicluster.OwnerKindLabel:      "ConfigMap",
		multicluster.OwnerNameLabel:      "test-owner",
		multicluster.OwnerNamespaceLabel: "default",
	}
}

// TestEnsureObject_remoteAppliesLabelledAndUnowned pins the target-cluster path:
// the child is stamped with the ownership labels and carries no owner reference,
// because a reference to an owner living on another cluster names a UID this one
// cannot resolve.
func TestEnsureObject_remoteAppliesLabelledAndUnowned(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build())

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "applied-cm", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.Labels).To(Equal(testOwnerLabels()))
	g.Expect(fetched.OwnerReferences).To(BeEmpty(), "a remote child must carry no owner reference")
}

// TestEnsureObject_remoteRefusesForeignObject covers the destructive case the
// pre-check exists for: a same-named object somebody else provisioned would have
// its spec overwritten by the apply and would be deleted at teardown by the
// labels stamped on it. Nothing is written.
func TestEnsureObject_remoteRefusesForeignObject(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
			UID:       "foreign-uid",
			Labels:    map[string]string{"app": "somebody-else"},
		},
		Data: map[string]string{"key": "theirs"},
	}
	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(s).WithObjects(owner, foreign).Build())

	err := EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)
	g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt pre-existing ConfigMap default/applied-cm")))

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "theirs"), "the refused object must not be rewritten")
	g.Expect(fetched.Labels).To(Equal(map[string]string{"app": "somebody-else"}))
	g.Expect(fetched.ResourceVersion).To(Equal(foreign.ResourceVersion))
}

// staleCacheCluster stands in for a registered target cluster whose informer
// cache trails the API server it describes: GetClient answers out of the stale
// cache, GetAPIReader out of the cluster's actual state. Embedding the interface
// leaves every other method nil, which panics if anything reaches for one.
type staleCacheCluster struct {
	cluster.Cluster
	cached client.Client
	live   client.Reader
}

func (c staleCacheCluster) GetClient() client.Client { return c.cached }

func (c staleCacheCluster) GetAPIReader() client.Reader { return c.live }

// staleCacheResolver registers that cluster under every name.
type staleCacheResolver struct{ cl cluster.Cluster }

func (r staleCacheResolver) GetCluster(context.Context, mcruntime.ClusterName) (cluster.Cluster, error) {
	return r.cl, nil
}

// TestEnsureObject_remotePrecheckReadsLiveState pins where the refusal gets its
// answer from. A cache is as capable of trailing a target cluster as a local
// one, and a foreign object it has not caught up on would be read as absent,
// adopted, overwritten by the apply and deleted at this owner's teardown. The
// same read through the cache would also pull every kind this function touches
// into an informer on the target — including the kinds nothing there reads.
func TestEnsureObject_remotePrecheckReadsLiveState(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
			UID:       "foreign-uid",
			Labels:    map[string]string{"app": "somebody-else"},
		},
		Data: map[string]string{"key": "theirs"},
	}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(owner, foreign).Build()

	c, err := multicluster.ResolveChildrenClient(context.Background(),
		staleCacheResolver{cl: staleCacheCluster{cached: cached, live: live}}, cached,
		&commonv1.TargetClusterRefSpec{Name: "edge-1"})
	g.Expect(err).NotTo(HaveOccurred())

	err = EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)

	g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt pre-existing ConfigMap default/applied-cm")))
	g.Expect(cached.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.ConfigMap{})).
		NotTo(Succeed(), "nothing may have been written while the cache still showed the object as absent")
}

// TestEnsureObject_remoteReappliesOwnChild is the other side of the refusal: an
// object already carrying this owner's labels is its child and is applied again,
// which is the ordinary steady-state pass.
func TestEnsureObject_remoteReappliesOwnChild(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	mine := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
			UID:       "child-uid",
			Labels:    testOwnerLabels(),
		},
		Data: map[string]string{"key": "stale"},
	}
	c := multicluster.Remote(fake.NewClientBuilder().WithScheme(s).WithObjects(owner, mine).Build())

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(mine), fetched)).To(Succeed())
	g.Expect(fetched.Data).To(HaveKeyWithValue("key", "value"))
	g.Expect(fetched.Labels).To(Equal(testOwnerLabels()))
	g.Expect(fetched.OwnerReferences).To(BeEmpty())
}

// TestEnsureObject_remotePropagatesPrecheckError pins that a read the target
// cluster answers with anything other than "absent" stops the pass: without an
// answer there is no way to tell our own child from somebody else's object, and
// applying anyway is the destructive guess.
func TestEnsureObject_remotePropagatesPrecheckError(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	applies := 0
	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			},
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applies++
				return nil
			},
		}).
		Build())

	err := EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)
	g.Expect(err).To(MatchError(ContainSubstring("checking for a pre-existing ConfigMap default/applied-cm")))
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the read error must survive the wrapping")
	g.Expect(applies).To(BeZero(), "no apply may be attempted while the live state is unknown")
}

// TestEnsureObject_remoteAppliesWhenKindNotInstalled covers the second tolerated
// read failure: the target cluster does not serve the child's kind at all, which
// says as much about a pre-existing object as a NotFound does — there is none.
// The apply proceeds and reports the missing kind itself.
func TestEnsureObject_remoteAppliesWhenKindNotInstalled(t *testing.T) {
	g := NewGomegaWithT(t)
	s := newScheme()
	owner := testOwner()

	// Only the pre-check's read is answered with the missing kind; the
	// verification below reads the fake's own state again.
	precheck := true
	c := multicluster.Remote(fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if precheck {
					precheck = false
					return &meta.NoKindMatchError{
						GroupKind:        schema.GroupKind{Kind: "ConfigMap"},
						SearchedVersions: []string{"v1"},
					}
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build())

	g.Expect(EnsureObject(context.Background(), c, s, owner, testConfigMap(), FieldManager)).To(Succeed())

	fetched := &corev1.ConfigMap{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Name: "applied-cm", Namespace: "default"}, fetched)).To(Succeed())
	g.Expect(fetched.Labels).To(Equal(testOwnerLabels()))
	g.Expect(fetched.OwnerReferences).To(BeEmpty())
}
