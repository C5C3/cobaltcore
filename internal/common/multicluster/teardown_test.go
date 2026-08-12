// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"strconv"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The kinds the sweep is driven with. Deployments are among them so the sweep
// has to cross an API group and not just two kinds of the core one.
var (
	configMapGVK  = corev1.SchemeGroupVersion.WithKind("ConfigMap")
	secretGVK     = corev1.SchemeGroupVersion.WithKind("Secret")
	deploymentGVK = appsv1.SchemeGroupVersion.WithKind("Deployment")
	// A kind a target cluster without cert-manager does not serve.
	certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
)

// teardownScheme knows the owner kind and every child kind the sweeps below
// list.
func teardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	g := gomega.NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
	g.Expect(appsv1.AddToScheme(scheme)).To(gomega.Succeed())
	return scheme
}

// ownedBy stamps owner's ownership labels on obj, which is what Claim did to it
// when the projection wrote it to the target cluster.
func ownedBy(t *testing.T, scheme *runtime.Scheme, owner, obj client.Object) client.Object {
	t.Helper()

	labels, err := OwnerLabels(scheme, owner)
	gomega.NewWithT(t).Expect(err).NotTo(gomega.HaveOccurred())
	obj.SetLabels(labels)
	return obj
}

// gone reports whether obj is absent from c. Every assertion about what a sweep
// did, and about what it left alone, is made through it.
func gone(t *testing.T, c client.Client, obj client.Object) bool {
	t.Helper()

	err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object))
	if apierrors.IsNotFound(err) {
		return true
	}
	gomega.NewWithT(t).Expect(err).NotTo(gomega.HaveOccurred())
	return false
}

func teardownConfigMap(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func teardownSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

func teardownDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// A CR that never named a target cluster keeps the behavior it always had: its
// children are collected by their owner references. Callers do not branch on
// that, so the sweep has to recognize the unmarked client and delete nothing at
// all, including objects whose labels would otherwise select them.
func TestDeleteRemoteChildrenLocalClientDeletesNothing(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	wouldMatch := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wouldMatch).Build()

	err := DeleteRemoteChildren(context.Background(), local, local, scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gone(t, local, wouldMatch)).To(gomega.BeFalse(),
		"a local child is the garbage collector's business, not the sweep's")
}

func TestDeleteRemoteChildrenDeletesEveryLabelledChildOfEveryKind(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	configMap := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	secret := ownedBy(t, scheme, owner, teardownSecret("keystone-credentials", "openstack"))
	deployment := ownedBy(t, scheme, owner, teardownDeployment("keystone", "openstack"))
	target := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(configMap, secret, deployment).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK, secretGVK, deploymentGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gone(t, target, configMap)).To(gomega.BeTrue())
	g.Expect(gone(t, target, secret)).To(gomega.BeTrue())
	g.Expect(gone(t, target, deployment)).To(gomega.BeTrue())
}

// A child written before ownership moved off the owner reference carries that
// reference and no labels, and the create-once write sites — the immutable
// config objects, the fernet and credential keys, the Certificates, the Jobs —
// never rewrite it, so nothing ever stamps the labels on it. The sweep decides
// ownership object by object for exactly this reason: a selector would not
// return it, the pass would report success, and key material would keep running
// on the target with no CR left to reclaim it.
func TestDeleteRemoteChildrenSweepsAChildOwnedByAReferenceAlone(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	legacy := teardownSecret("keystone-fernet-keys", "openstack")
	legacy.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       owner.Name,
		UID:        owner.UID,
		Controller: ptr.To(true),
	}}
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{secretGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gone(t, target, legacy)).To(gomega.BeTrue(),
		"a child the operator still recognizes has to be one the sweep reaches")
}

// The sweep runs on a cluster it shares with everybody else's children, so what
// it leaves standing matters as much as what it removes. The different-kind case
// is the reason the kind label exists: a Keystone and a Barbican of the same
// name in the same namespace project into the same target namespace.
func TestDeleteRemoteChildrenSparesEverythingItDoesNotOwn(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	ours := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	unlabelled := teardownConfigMap("cluster-ca", "openstack")
	otherName := ownedBy(t, scheme,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "openstack"}},
		teardownConfigMap("other-config", "openstack"))
	otherKind := ownedBy(t, scheme,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "openstack"}},
		teardownConfigMap("barbican-config", "openstack"))
	otherNamespace := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "other-namespace"))
	target := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ours, unlabelled, otherName, otherKind, otherNamespace).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gone(t, target, ours)).To(gomega.BeTrue())
	g.Expect(gone(t, target, unlabelled)).To(gomega.BeFalse(), "an object nobody claimed is nobody's child")
	g.Expect(gone(t, target, otherName)).To(gomega.BeFalse(), "another CR's child")
	g.Expect(gone(t, target, otherKind)).To(gomega.BeFalse(),
		"a same-named CR of another kind owns this one")
	g.Expect(gone(t, target, otherNamespace)).To(gomega.BeFalse(), "outside the swept namespace")
}

// Every operator sweeps the same kind list, and a target cluster is free to
// serve only part of it. A missing CRD is not a failure to clean up: there can
// be no children of a kind the cluster does not know, and failing the pass would
// wedge the CR on a finalizer nothing could ever release.
func TestDeleteRemoteChildrenSkipsAKindTheTargetDoesNotServe(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	configMap := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "CertificateList" {
					return &meta.NoKindMatchError{
						GroupKind:        schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"},
						SearchedVersions: []string{"v1"},
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{certificateGVK, configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(gone(t, target, configMap)).To(gomega.BeTrue(),
		"the unserved kind must be skipped, not abort the sweep")
}

// The list is a moment older than the deletes it drives, so a child that went
// away in between is already in the state the sweep wanted.
func TestDeleteRemoteChildrenToleratesAChildDeletedUnderIt(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	configMap := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return apierrors.NewNotFound(corev1.Resource("configmaps"), "keystone-config")
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
}

// The sweep lists without a server-side selector, so the API server answers with
// every object of the kind in a namespace it shares with everything else
// deployed there. It therefore has to page: a single unbounded response would be
// decoded into unstructured maps in one go and can OOMKill the operator
// mid-sweep, which then restarts into the same sweep and never releases the CR.
func TestDeleteRemoteChildrenPagesThroughAKind(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	// One page per child, so the sweep can only reach the last one by following
	// the continue token of the page before it.
	names := []string{"keystone-config", "keystone-policy", "keystone-defaults"}
	var children []client.Object
	for _, name := range names {
		children = append(children, ownedBy(t, scheme, owner, teardownConfigMap(name, "openstack")))
	}

	var limits []int64
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(children...).
		WithInterceptorFuncs(interceptor.Funcs{
			// The fake client serves the whole list and ignores Limit/Continue, so
			// the paging server is emulated here: one name per page, and a continue
			// token naming the next until the last page carries none.
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				options := &client.ListOptions{}
				options.ApplyOptions(opts)
				limits = append(limits, options.Limit)

				index := 0
				if options.Continue != "" {
					var err error
					if index, err = strconv.Atoi(options.Continue); err != nil {
						return err
					}
				}
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				items, err := meta.ExtractList(list)
				if err != nil {
					return err
				}
				var page []runtime.Object
				for _, item := range items {
					if item.(client.Object).GetName() == names[index] {
						page = append(page, item)
					}
				}
				if err := meta.SetList(list, page); err != nil {
					return err
				}
				if index+1 < len(names) {
					list.SetContinue(strconv.Itoa(index + 1))
				}
				return nil
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	for _, child := range children {
		g.Expect(gone(t, target, child)).To(gomega.BeTrue(),
			"a child on a later page is as much this owner's as one on the first")
	}
	g.Expect(limits).To(gomega.Equal([]int64{teardownPageSize, teardownPageSize, teardownPageSize}),
		"every request is bounded, and the sweep followed the continue token to the last page")
}

// Paging alone does not bound the sweep: what a page costs is its size times
// what one item costs, and the swept kinds include Secret and ConfigMap, whose
// payload the API server caps at about a megabyte each. A page of whole objects
// would carry tens of megabytes the sweep never reads into an operator limited
// to a fraction of that. It reads what it uses — labels, owner references, a
// name — and nothing else.
func TestDeleteRemoteChildrenListsMetadataOnly(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	payload := teardownSecret("keystone-credentials", "openstack")
	payload.Data = map[string][]byte{"fernet": []byte("key-material")}
	secret := ownedBy(t, scheme, owner, payload)
	var listed []client.ObjectList
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				listed = append(listed, list)
				return c.List(ctx, list, opts...)
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{secretGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(listed).To(gomega.HaveLen(1))
	g.Expect(listed[0]).To(gomega.BeAssignableToTypeOf(&metav1.PartialObjectMetadataList{}),
		"the sweep must not pull object payloads it never reads into memory")
	g.Expect(gone(t, target, secret)).To(gomega.BeTrue(),
		"a metadata item still carries everything the delete is routed by")
}

// A paged list is served from the etcd revision its first page was read at, and
// a sweep deleting its way through a large kind can outlive that revision's
// compaction window: the continue token is then refused for good. Failing the
// pass would only restart the same scan from the first page, so the sweep does
// that itself rather than handing a CR back a finalizer nothing releases.
func TestDeleteRemoteChildrenRescansAKindWhoseContinueTokenExpired(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	first := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	second := ownedBy(t, scheme, owner, teardownConfigMap("keystone-policy", "openstack"))

	lists := 0
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				lists++
				if lists == 2 {
					// The second request is the one carrying the continue token.
					return apierrors.NewResourceExpired("continue parameter is too old to display a consistent list result")
				}
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				items, err := meta.ExtractList(list)
				if err != nil {
					return err
				}
				if lists == 1 {
					// A first page of one, so the rest is only reachable through the
					// token the sweep never gets to redeem.
					if err := meta.SetList(list, items[:1]); err != nil {
						return err
					}
					list.SetContinue("next")
				}
				return nil
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(lists).To(gomega.Equal(3), "the expired token is answered by scanning the kind again from the start")
	g.Expect(gone(t, target, first)).To(gomega.BeTrue())
	g.Expect(gone(t, target, second)).To(gomega.BeTrue(),
		"a child left on the page behind an expired token is still this owner's")
}

// The rescan is taken once per kind. A cluster that expires every scan cannot be
// swept, and retrying it here would pin a reconcile worker on a kind that never
// finishes; failing the pass requeues the CR under the controller's own backoff
// instead, with the finalizer still on it.
func TestDeleteRemoteChildrenFailsAKindThatKeepsExpiring(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	configMap := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))

	lists := 0
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				lists++
				return apierrors.NewResourceExpired("continue parameter is too old to display a consistent list result")
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("listing remote ConfigMap children for teardown")))
	g.Expect(lists).To(gomega.Equal(2), "one rescan, not a retry loop")
	g.Expect(gone(t, target, configMap)).To(gomega.BeFalse())
}

// A list the target cluster refuses says nothing about whether children exist,
// so the pass has to fail: the caller keeps its finalizer and sweeps again
// rather than releasing a CR whose children it never saw.
func TestDeleteRemoteChildrenPropagatesAListError(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	secret := ownedBy(t, scheme, owner, teardownSecret("keystone-credentials", "openstack"))
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if list.GetObjectKind().GroupVersionKind().Kind == "SecretList" {
					return apierrors.NewForbidden(corev1.Resource("secrets"), "", nil)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{secretGVK})

	// The kind is in the message because the sweep spans kinds and the operator
	// reading the CR's condition has to know which one it lost.
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("listing remote Secret children for teardown")))
	g.Expect(gone(t, target, secret)).To(gomega.BeFalse())
}

// An owner whose kind the scheme does not know cannot be labelled, so its
// children cannot be selected either. Deleting nothing at all is the only safe
// answer: a partial label set would select another CR's children.
func TestDeleteRemoteChildrenUnregisteredOwnerKindFailsWithoutDeleting(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	configMap := ownedBy(t, scheme, owner, teardownConfigMap("keystone-config", "openstack"))
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), runtime.NewScheme(), owner,
		[]schema.GroupVersionKind{configMapGVK})

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving GVK for owner openstack/example")))
	g.Expect(gone(t, target, configMap)).To(gomega.BeFalse())
}

// A child of a child (a Deployment's ReplicaSet, a MariaDB's StatefulSet) has an
// owner reference on the target cluster and is collected there. Foreground
// propagation would make the sweep wait for that cascade before the delete
// returns, and background is what the local cascade does.
func TestDeleteRemoteChildrenDeletesWithBackgroundPropagation(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := teardownScheme(t)
	owner := testOwner()
	deployment := ownedBy(t, scheme, owner, teardownDeployment("keystone", "openstack"))
	var policies []*metav1.DeletionPropagation
	target := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption,
			) error {
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				policies = append(policies, options.PropagationPolicy)
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()

	err := DeleteRemoteChildren(context.Background(), target, Remote(target), scheme, owner,
		[]schema.GroupVersionKind{deploymentGVK})

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(policies).To(gomega.HaveLen(1))
	g.Expect(policies[0]).To(gomega.HaveValue(gomega.Equal(metav1.DeletePropagationBackground)))
}
