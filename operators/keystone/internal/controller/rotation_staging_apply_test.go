// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for KeystoneReconciler.applyRotationOutput.
//
// These tests exercise the controller's "apply staging to production" path:
// GET staging Secret, check RotationCompletedAnnotation, validate payload,
// PATCH main Secret, DELETE staging Secret, emit event. Uses fake client +
// FakeRecorder per the package testing conventions.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// applyTestKeystone returns a minimal Keystone CR suitable for applyRotationOutput
// tests.
func applyTestKeystone() *keystonev1alpha1.Keystone {
	return &keystonev1alpha1.Keystone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-keystone",
			Namespace: "default",
			UID:       "ks-uid",
		},
	}
}

// makeValidFernetKeys generates n unique valid Fernet keys (44-byte base64url)
// suitable for use as staging Secret Data.
func makeValidFernetKeys(t *testing.T, n int) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		k, err := generateFernetKey()
		if err != nil {
			t.Fatalf("generateFernetKey: %v", err)
		}
		out[strconv.Itoa(i)] = []byte(k)
	}
	return out
}

// newApplyTestReconciler builds a KeystoneReconciler wired to a fake client
// preloaded with the given objects and a FakeRecorder large enough for tests
// that may emit several events.
func newApplyTestReconciler(objs ...client.Object) *KeystoneReconciler {
	s := testScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &KeystoneReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}
}

// runApplyRotation fetches the main and (optionally) staging Secrets by name
// from the client — mirroring the production caller, which threads the objects
// it already read/ensured this pass — and invokes applyRotationOutput. A
// staging Secret absent from the client is passed as nil so the no-op path is
// exercised.
func runApplyRotation(t *testing.T, r *KeystoneReconciler, ks *keystonev1alpha1.Keystone, stagingName, mainName, reason string, minKeys, maxKeys int) (bool, error) {
	t.Helper()
	ctx := context.Background()
	var main corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: mainName, Namespace: ks.Namespace}, &main); err != nil {
		t.Fatalf("getting main secret %s: %v", mainName, err)
	}
	var staging *corev1.Secret
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: stagingName, Namespace: ks.Namespace}, &s); err == nil {
		staging = &s
	}
	return r.applyRotationOutput(ctx, ks, staging, &main, reason, minKeys, maxKeys)
}

func TestApplyRotationOutput_NoStagingSecret(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	// Production secret exists, staging does not.
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Production untouched.
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveKeyWithValue("0", []byte("existing")))
	expectNoEvent(g, r)
}

func TestApplyRotationOutput_NoAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			// No RotationCompletedAnnotation.
		},
		Data: makeValidFernetKeys(t, 3),
	}
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Production untouched.
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveKeyWithValue("0", []byte("existing")))

	// Staging retained.
	var gotStaging corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &gotStaging)).To(Succeed())

	expectNoEvent(g, r)
}

func TestApplyRotationOutput_InvalidAnnotation(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: "not-a-date",
			},
		},
		Data: makeValidFernetKeys(t, 3),
	}
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Production untouched.
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveKeyWithValue("0", []byte("existing")))

	// Staging retained for human inspection.
	var gotStaging corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &gotStaging)).To(Succeed())

	expectEvent(g, r, "Warning RotationAnnotationInvalid")
}

func TestApplyRotationOutput_ValidationFailsWrongLength(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	// Wrong length: 32-byte raw keys instead of 44-byte base64url. Use distinct
	// values so we fail on length rather than on duplicates.
	bad := map[string][]byte{
		"0": []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0"), // 32 bytes
		"1": []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1"), // 32 bytes
		"2": []byte("ccccccccccccccccccccccccccccccc2"), // 32 bytes
	}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Data: bad,
	}
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Production untouched.
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveKeyWithValue("0", []byte("existing")))

	// Staging Secret retained but cleared: Data emptied and the completion
	// annotation removed so the next CronJob strategic-merge PATCH starts from
	// an empty base rather than accumulating over the rejected payload (#475).
	var gotStaging corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &gotStaging)).To(Succeed())
	g.Expect(gotStaging.Data).To(BeEmpty(), "rejected staging Data must be cleared (issue #475)")
	g.Expect(gotStaging.Annotations).NotTo(HaveKey(RotationCompletedAnnotation),
		"rejected staging completion annotation must be removed (issue #475)")

	expectEvent(g, r, "Warning RotationRejected")
}

func TestApplyRotationOutput_ValidationFailsDuplicates(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	// Two identical valid keys => duplicate detection.
	k, err := generateFernetKey()
	g.Expect(err).NotTo(HaveOccurred())
	dup := map[string][]byte{
		"0": []byte(k),
		"1": []byte(k),
	}
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Data: dup,
	}
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse())

	// Rejected staging payload is cleared (Data emptied, annotation removed)
	// so a duplicate-key payload cannot persist as a strategic-merge base for
	// the next CronJob run (issue #475).
	var gotStaging corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &gotStaging)).To(Succeed())
	g.Expect(gotStaging.Data).To(BeEmpty())
	g.Expect(gotStaging.Annotations).NotTo(HaveKey(RotationCompletedAnnotation))

	expectEvent(g, r, "Warning RotationRejected")
}

// TestApplyRotationOutput_AlreadyCommitted_NoOpUpdate covers B3: when the
// target already holds the exact staging payload and completion timestamp (a
// prior pass committed it but its staging delete was outstanding), the commit
// must not re-Update the target or re-emit the success event, but must still
// delete the staging Secret and report applied=false.
func TestApplyRotationOutput_AlreadyCommitted_NoOpUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	completedAt := time.Now().UTC().Format(time.RFC3339)
	payload := makeValidFernetKeys(t, 3)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fernetStagingSecretName(ks),
			Namespace:   "default",
			Annotations: map[string]string{RotationCompletedAnnotation: completedAt},
		},
		Data: payload,
	}
	// Production already carries the identical payload and completion timestamp.
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-keystone-fernet-keys",
			Namespace:   "default",
			Annotations: map[string]string{RotationCompletedAnnotation: completedAt},
		},
		Data: payload,
	}
	r := newApplyTestReconciler(staging, prod)

	// Capture the production ResourceVersion before the commit.
	var before corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &before)).To(Succeed())

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeFalse(), "an already-committed payload must report applied=false")

	// Production ResourceVersion unchanged: no redundant Update was issued.
	var after corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &after)).To(Succeed())
	g.Expect(after.ResourceVersion).To(Equal(before.ResourceVersion),
		"target must not be re-written when the payload is already committed")

	// Staging Secret still deleted so the CronJob's next run starts clean.
	var leftover corev1.Secret
	getErr := r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &leftover)
	g.Expect(apierrors.IsNotFound(getErr)).To(BeTrue(), "staging Secret must still be deleted")

	// No success event: the rotation was announced by the earlier committing pass.
	expectNoEvent(g, r)
}

func TestApplyRotationOutput_HappyPath(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	stagingData := makeValidFernetKeys(t, 3)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Data: stagingData,
	}
	// Production starts with a single key that must be replaced by the
	// full-object Update.
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeTrue())

	// Production now equals staging data.
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveLen(len(stagingData)))
	for k, v := range stagingData {
		g.Expect(gotProd.Data).To(HaveKeyWithValue(k, v))
	}

	// Staging deleted.
	var gotStaging corev1.Secret
	err = r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &gotStaging)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "staging Secret should be deleted after apply")

	expectEvent(g, r, "Normal FernetKeysRotated")
}

// TestApplyRotationOutput_ConcurrentStagingPatchTolerated is the regression
// guard for the staging-delete race (issue #475, Problem 1a): the step-5 Delete
// must carry client.Preconditions{UID, ResourceVersion} from the step-1 read so
// that a rotation CronJob PATCHing fresh output between the read and the Delete
// is rejected with 409 Conflict instead of being silently deleted uncommitted.
// applyRotationOutput must tolerate that Conflict (this run's payload is already
// on the production Secret) and leave the staging Secret intact so its newer
// payload commits on the next reconcile.
func TestApplyRotationOutput_ConcurrentStagingPatchTolerated(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()
	s := testScheme()

	stagingData := makeValidFernetKeys(t, 3)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			UID:       "staging-uid",
			Annotations: map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Data: stagingData,
	}
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}

	var sawPreconditions *metav1.Preconditions
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(staging, prod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, wc client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if sec, ok := obj.(*corev1.Secret); ok && sec.Name == fernetStagingSecretName(ks) {
					do := &client.DeleteOptions{}
					for _, o := range opts {
						o.ApplyToDelete(do)
					}
					sawPreconditions = do.Preconditions
					// Simulate a CronJob PATCH landing between the operator's
					// read and this Delete: the precondition no longer matches,
					// so the API server rejects the Delete with 409 Conflict.
					return apierrors.NewConflict(corev1.Resource("secrets"), sec.Name,
						fmt.Errorf("simulated concurrent rotation PATCH"))
				}
				return wc.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &KeystoneReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeTrue(), "a Conflict on the staging Delete must be tolerated as a completed apply")

	// The Delete carried the UID + ResourceVersion observed at the step-1 read.
	g.Expect(sawPreconditions).NotTo(BeNil(), "staging Delete must carry client.Preconditions")
	g.Expect(sawPreconditions.UID).NotTo(BeNil())
	g.Expect(*sawPreconditions.UID).To(Equal(types.UID("staging-uid")))
	g.Expect(sawPreconditions.ResourceVersion).NotTo(BeNil())
	g.Expect(*sawPreconditions.ResourceVersion).NotTo(BeEmpty())

	// Production carries this run's payload (step 4 completed before the Delete).
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveLen(len(stagingData)))

	// Staging Secret still present — its newer payload was NOT deleted, so it
	// commits on the next reconcile rather than being lost (issue #475).
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: fernetStagingSecretName(ks), Namespace: "default"}, &corev1.Secret{})).To(Succeed())

	expectEvent(g, r, "Normal FernetKeysRotated")
}

// TestApplyRotationOutput_StampsCompletionAnnotationOnProduction is the
// regression guard for the M1 review finding the
// keystone_operator_key_rotation_age_seconds gauge needs a durable timestamp
// so it can refresh on every reconcile after the staging Secret is deleted.
// applyRotationOutput must therefore copy the staging Secret's
// RotationCompletedAnnotation onto the production Secret (verbatim, so the
// gauge measures wall-clock age rather than apply time) before the staging
// Secret is removed.
func TestApplyRotationOutput_StampsCompletionAnnotationOnProduction(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	completedAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second).Format(time.RFC3339)
	stagingData := makeValidFernetKeys(t, 3)
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: completedAt,
			},
		},
		Data: stagingData,
	}
	// Production has no rotation annotation yet — first successful apply must
	// stamp it.
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"0": []byte("existing")},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeTrue())

	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Annotations).To(HaveKeyWithValue(RotationCompletedAnnotation, completedAt),
		"production Secret must carry the staging completion annotation verbatim "+
			"so the rotation-age gauge stays accurate after staging is deleted")
}

// TestApplyRotationOutput_ReplacesDisjointIndices asserts that a successful
// apply fully replaces production Secret.Data with the staging payload, even
// when production holds key indices that are NOT present in staging (e.g.
// a historical "7" vs staging {"0","1","2"}). This is the regression guard
// for the strategic-merge-vs-replace bug: under strategic-merge on
// `map[string][]byte` the stale "7" would be preserved by key-merge, causing
// decommissioned keys to accumulate and violate the bounded-keys
// contract.
func TestApplyRotationOutput_ReplacesDisjointIndices(t *testing.T) {
	g := NewGomegaWithT(t)
	ks := applyTestKeystone()

	stagingData := makeValidFernetKeys(t, 3) // indices "0","1","2"
	staging := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fernetStagingSecretName(ks),
			Namespace: "default",
			Annotations: map[string]string{
				RotationCompletedAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Data: stagingData,
	}
	// Production holds a key at an index the staging payload does NOT mention.
	// A merge-by-key PATCH would leave "7" behind; a full Update must remove it.
	staleKey := []byte("stale-key-at-disjoint-index")
	prod := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-keystone-fernet-keys", Namespace: "default"},
		Data:       map[string][]byte{"7": staleKey},
	}
	r := newApplyTestReconciler(staging, prod)

	applied, err := runApplyRotation(t, r, ks,
		fernetStagingSecretName(ks),
		"test-keystone-fernet-keys",
		"FernetKeysRotated",
		1, 10,
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applied).To(BeTrue())

	// Production.Data must equal staging exactly — length and contents — with
	// no trace of the disjoint stale index "7".
	var gotProd corev1.Secret
	g.Expect(r.Get(context.Background(),
		types.NamespacedName{Name: "test-keystone-fernet-keys", Namespace: "default"}, &gotProd)).To(Succeed())
	g.Expect(gotProd.Data).To(HaveLen(len(stagingData)),
		"production Data must contain exactly the staging keys")
	g.Expect(gotProd.Data).NotTo(HaveKey("7"),
		"stale disjoint index must be removed by the full-replacement Update")
	for k, v := range stagingData {
		g.Expect(gotProd.Data).To(HaveKeyWithValue(k, v))
	}
	expectEvent(g, r, "Normal FernetKeysRotated")
}
