// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package multicluster

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// A real owner is a service CR, which this package cannot import without
// depending on the operators it is imported by. A ConfigMap owning a Secret
// stands in: the ownership rules read the scheme and the object meta, neither of
// which knows the difference.
func testOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      "example",
		Namespace: "openstack",
		UID:       "owner-uid",
	}}
}

func testChild() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "example-config",
		Namespace: "openstack",
	}}
}

// ownershipScheme knows the owner and child kinds. Its counterpart in the error
// tests is a bare runtime.NewScheme(), which knows neither.
func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	gomega.NewWithT(t).Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
	return scheme
}

func TestOwnerLabelsNamesTheOwnerByKindNameAndNamespace(t *testing.T) {
	g := gomega.NewWithT(t)

	labels, err := OwnerLabels(ownershipScheme(t), testOwner())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(labels).To(gomega.Equal(map[string]string{
		OwnerKindLabel:      "ConfigMap",
		OwnerNameLabel:      "example",
		OwnerNamespaceLabel: "openstack",
	}))
}

// An owner whose kind the scheme does not know cannot be labelled at all, and
// the three entry points that need the kind all have to say so rather than stamp
// an empty label that another CR's children would then match.
func TestOwnerLabelsUnregisteredOwnerKindFails(t *testing.T) {
	g := gomega.NewWithT(t)

	labels, err := OwnerLabels(runtime.NewScheme(), testOwner())

	g.Expect(labels).To(gomega.BeNil())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving GVK for owner openstack/example")))
}

func TestControlsUnregisteredOwnerKindFails(t *testing.T) {
	g := gomega.NewWithT(t)

	controlled, err := Controls(runtime.NewScheme(), testOwner(), testChild())

	g.Expect(controlled).To(gomega.BeFalse())
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving GVK for owner openstack/example")))
}

func TestClaimRemoteUnregisteredOwnerKindFails(t *testing.T) {
	g := gomega.NewWithT(t)

	child := testChild()
	err := Claim(Remote(fake.NewClientBuilder().Build()), runtime.NewScheme(), testOwner(), child)

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("resolving GVK for owner openstack/example")))
	g.Expect(child.GetLabels()).To(gomega.BeEmpty(), "a claim that failed must not have stamped a partial label set")
}

// The local path has to stay what it was before the seam existed, so it is
// asserted against controllerutil.SetControllerReference itself rather than
// against a hand-written expectation of what it produces.
func TestClaimLocalSetsTheControllerReference(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := ownershipScheme(t)
	owner := testOwner()
	claimed, expected := testChild(), testChild()

	g.Expect(Claim(fake.NewClientBuilder().Build(), scheme, owner, claimed)).To(gomega.Succeed())
	g.Expect(controllerutil.SetControllerReference(owner, expected, scheme)).To(gomega.Succeed())

	g.Expect(claimed.OwnerReferences).To(gomega.Equal(expected.OwnerReferences))
	g.Expect(claimed.OwnerReferences).To(gomega.HaveLen(1))
	ref := claimed.OwnerReferences[0]
	g.Expect(ref.Kind).To(gomega.Equal("ConfigMap"))
	g.Expect(ref.Name).To(gomega.Equal("example"))
	g.Expect(ref.UID).To(gomega.Equal(owner.UID))
	g.Expect(ref.Controller).To(gomega.Equal(ptr.To(true)))
	g.Expect(ref.BlockOwnerDeletion).To(gomega.Equal(ptr.To(true)))
	// The labels belong to the remote path only: locally the reference is the
	// whole claim, and stamping them too would make a local child answer to the
	// remote teardown's selector.
	g.Expect(claimed.GetLabels()).To(gomega.BeEmpty())
}

func TestClaimRemoteStampsTheOwnershipLabelsInsteadOfAReference(t *testing.T) {
	g := gomega.NewWithT(t)

	child := testChild()
	child.Labels = map[string]string{"app.kubernetes.io/name": "keystone"}

	g.Expect(Claim(Remote(fake.NewClientBuilder().Build()), ownershipScheme(t), testOwner(), child)).To(gomega.Succeed())

	g.Expect(child.Labels).To(gomega.Equal(map[string]string{
		"app.kubernetes.io/name": "keystone",
		OwnerKindLabel:           "ConfigMap",
		OwnerNameLabel:           "example",
		OwnerNamespaceLabel:      "openstack",
	}))
	g.Expect(child.OwnerReferences).To(gomega.BeEmpty(),
		"a reference to a management-cluster owner names a UID the target cluster cannot resolve")
}

// A child that already carries a controller reference to its owner is ours, and
// the claim both keeps it and heals it: the reference goes, the labels arrive.
// Unrelated references are somebody else's business and stay.
func TestClaimRemoteDropsTheDanglingControllerReference(t *testing.T) {
	g := gomega.NewWithT(t)

	owner := testOwner()
	unrelated := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "example",
		UID:        "someone-else",
	}
	child := testChild()
	child.UID = "child-uid"
	child.OwnerReferences = []metav1.OwnerReference{unrelated, {
		APIVersion:         "v1",
		Kind:               "ConfigMap",
		Name:               owner.Name,
		UID:                owner.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}

	g.Expect(Claim(Remote(fake.NewClientBuilder().Build()), ownershipScheme(t), owner, child)).To(gomega.Succeed())

	g.Expect(child.OwnerReferences).To(gomega.Equal([]metav1.OwnerReference{unrelated}))
	g.Expect(child.Labels).To(gomega.HaveKeyWithValue(OwnerNameLabel, "example"))
	g.Expect(child.Labels).To(gomega.HaveKeyWithValue(OwnerKindLabel, "ConfigMap"))
	g.Expect(child.Labels).To(gomega.HaveKeyWithValue(OwnerNamespaceLabel, "openstack"))
}

// The destructive case: a live object of the same name that nobody claimed. The
// apply would overwrite its spec and the labels would get it deleted at
// teardown, so the claim fails instead and leaves the object alone.
func TestClaimRemoteRefusesToAdoptAPreExistingObject(t *testing.T) {
	g := gomega.NewWithT(t)

	child := testChild()
	child.UID = "provisioned-by-somebody-else"

	err := Claim(Remote(fake.NewClientBuilder().Build()), ownershipScheme(t), testOwner(), child)

	g.Expect(err).To(gomega.MatchError("refusing to adopt pre-existing Secret openstack/example-config: it was not " +
		"created by this ConfigMap, so adopting it would overwrite its spec and delete it at teardown"))
	g.Expect(child.Labels).To(gomega.BeEmpty(), "a refused object must not be marked as ours")
}

// A name longer than a label value may be is refused at the claim, the one gate
// every remote write passes. Left to the API server it would come back as an
// invalid-label-value rejection from inside each of a dozen write sites, on a
// CR that had already installed the finalizer holding it open for a teardown.
func TestClaimRemoteRefusesAnOwnerNameTooLongForALabel(t *testing.T) {
	g := gomega.NewWithT(t)

	owner := testOwner()
	owner.Name = strings.Repeat("k", MaxOwnerNameLength+1)
	child := testChild()

	err := Claim(Remote(fake.NewClientBuilder().Build()), ownershipScheme(t), owner, child)

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("the owner's name is 64 characters")))
	g.Expect(child.GetLabels()).To(gomega.BeEmpty(), "a refused claim must not have stamped a partial label set")
}

// The cap is the remote path's alone. A local child is claimed by a reference
// naming the owner's UID, which no length rule touches, so the same name is
// claimed exactly as it always was.
func TestClaimLocalAcceptsAnOwnerNameTooLongForALabel(t *testing.T) {
	g := gomega.NewWithT(t)

	owner := testOwner()
	owner.Name = strings.Repeat("k", MaxOwnerNameLength+1)
	child := testChild()

	g.Expect(Claim(fake.NewClientBuilder().Build(), ownershipScheme(t), owner, child)).To(gomega.Succeed())
	g.Expect(child.OwnerReferences).To(gomega.HaveLen(1))
}

// A child the operator has just built has no UID yet, so it cannot be a
// pre-existing foreign object and is claimed without a read.
func TestClaimRemoteClaimsAnObjectThatDoesNotExistYet(t *testing.T) {
	g := gomega.NewWithT(t)

	child := testChild()

	g.Expect(Claim(Remote(fake.NewClientBuilder().Build()), ownershipScheme(t), testOwner(), child)).To(gomega.Succeed())
	g.Expect(child.Labels).To(gomega.HaveKeyWithValue(OwnerNameLabel, "example"))
}

func TestControlsRecognizesTheControllerReference(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := ownershipScheme(t)
	owner := testOwner()
	child := testChild()
	g.Expect(controllerutil.SetControllerReference(owner, child, scheme)).To(gomega.Succeed())

	controlled, err := Controls(scheme, owner, child)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(controlled).To(gomega.BeTrue())
}

func TestControlsRecognizesTheOwnershipLabels(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := ownershipScheme(t)
	owner := testOwner()
	child := testChild()
	labels, err := OwnerLabels(scheme, owner)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	child.Labels = labels

	controlled, err := Controls(scheme, owner, child)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(controlled).To(gomega.BeTrue())
}

func TestControlsRejectsAnObjectWithNeitherMark(t *testing.T) {
	g := gomega.NewWithT(t)

	controlled, err := Controls(ownershipScheme(t), testOwner(), testChild())

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(controlled).To(gomega.BeFalse())
}

// Name and namespace are not enough: two service CRs of different kinds share
// them routinely, and each would otherwise select and delete the other's
// children on the target cluster.
func TestControlsRejectsADifferentOwnerKindOfTheSameName(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := ownershipScheme(t)
	child := testChild()
	labels, err := OwnerLabels(scheme, testOwner())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	child.Labels = labels

	// Same name, same namespace, different kind.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "example",
		Namespace: "openstack",
		UID:       "other-owner-uid",
	}}
	controlled, err := Controls(scheme, other, child)

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(controlled).To(gomega.BeFalse())
}

func TestRemoteMarksTheClientAndPassesEveryCallThrough(t *testing.T) {
	g := gomega.NewWithT(t)

	seeded := testChild()
	inner := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(seeded).Build()

	g.Expect(IsRemote(inner)).To(gomega.BeFalse())
	g.Expect(IsRemote(Remote(inner))).To(gomega.BeTrue())

	// The mark is a wrapper, not a substitute: reads and writes still land on the
	// cluster the resolver handed out.
	read := &corev1.Secret{}
	g.Expect(Remote(inner).Get(context.Background(), client.ObjectKeyFromObject(seeded), read)).To(gomega.Succeed())
	g.Expect(read.Name).To(gomega.Equal(seeded.Name))
}

// LiveReader is what keeps an ownership decision off the cache: a client the
// resolver marked answers with its cluster's uncached reader, and everything
// else with itself.
func TestLiveReaderPrefersTheClustersUncachedReader(t *testing.T) {
	g := gomega.NewWithT(t)

	scheme := ownershipScheme(t)
	child := testChild()
	// The cache has not caught up with the cluster it describes: only the
	// uncached reader sees the object that is on it.
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	uncached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child).Build()
	key := client.ObjectKeyFromObject(child)

	g.Expect(LiveReader(remoteClient{Client: cached, reader: uncached}).
		Get(context.Background(), key, &corev1.Secret{})).To(gomega.Succeed())

	missed := LiveReader(Remote(cached)).Get(context.Background(), key, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(missed)).To(gomega.BeTrue(),
		"a mark carrying no reader falls back to the client it wraps")
	missed = LiveReader(cached).Get(context.Background(), key, &corev1.Secret{})
	g.Expect(apierrors.IsNotFound(missed)).To(gomega.BeTrue(), "an unmarked client is its own reader")
}
