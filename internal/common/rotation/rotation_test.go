// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package rotation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func rotationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1: %v", err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("rbacv1: %v", err)
	}
	return s
}

func rotationOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "keystone", Namespace: "openstack", UID: "u"}}
}

func TestCompletedAt(t *testing.T) {
	g := gomega.NewWithT(t)
	_, ok := CompletedAt(nil)
	g.Expect(ok).To(gomega.BeFalse())
	_, ok = CompletedAt(&corev1.Secret{})
	g.Expect(ok).To(gomega.BeFalse())
	_, ok = CompletedAt(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CompletedAnnotation: "not-a-time"}}})
	g.Expect(ok).To(gomega.BeFalse())
	ts := time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339)
	parsed, ok := CompletedAt(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CompletedAnnotation: ts}}})
	g.Expect(ok).To(gomega.BeTrue())
	g.Expect(parsed.Unix()).To(gomega.Equal(int64(1_700_000_000)))
}

func TestObserveAge_PrefersMainThenStaging(t *testing.T) {
	g := gomega.NewWithT(t)
	ts := time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339)
	main := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CompletedAnnotation: ts}}}
	staging := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CompletedAnnotation: time.Unix(1, 0).UTC().Format(time.RFC3339)}}}

	var got time.Time
	ObserveAge(main, staging, func(c time.Time) error { got = c; return nil })
	g.Expect(got.Unix()).To(gomega.Equal(int64(1_700_000_000)), "main wins over staging")

	// No main annotation → staging is used.
	got = time.Time{}
	ObserveAge(&corev1.Secret{}, staging, func(c time.Time) error { got = c; return nil })
	g.Expect(got.Unix()).To(gomega.Equal(int64(1)))

	// Neither → callback never runs.
	called := false
	ObserveAge(&corev1.Secret{}, &corev1.Secret{}, func(time.Time) error { called = true; return nil })
	g.Expect(called).To(gomega.BeFalse())
}

func TestEnsureStagingSecret_CreatesOwnedEmpty(t *testing.T) {
	g := gomega.NewWithT(t)
	s := rotationScheme(t)
	owner := rotationOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	secret, err := EnsureStagingSecret(context.Background(), c, s, owner, "keystone-fernet-keys-rotation",
		map[string]string{StagingSecretLabelKey: "fernet-keys"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(secret.Data).To(gomega.BeNil(), "staging data is owned by the CronJob PATCH, not this call")
	g.Expect(secret.Labels).To(gomega.HaveKeyWithValue(StagingSecretLabelKey, "fernet-keys"))
	g.Expect(secret.OwnerReferences).To(gomega.HaveLen(1))
}

func TestEnsureRBAC_LeastPrivilege(t *testing.T) {
	g := gomega.NewWithT(t)
	s := rotationScheme(t)
	owner := rotationOwner()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	g.Expect(EnsureRBAC(context.Background(), c, s, owner, "keystone-fernet-rotate", "keystone-fernet-keys", "keystone-fernet-keys-rotation")).To(gomega.Succeed())

	role := &rbacv1.Role{}
	g.Expect(c.Get(context.Background(), apitypes.NamespacedName{Namespace: "openstack", Name: "keystone-fernet-rotate"}, role)).To(gomega.Succeed())
	g.Expect(role.Rules).To(gomega.HaveLen(2))
	// Rule 1: read-only get on the source secret.
	g.Expect(role.Rules[0].Verbs).To(gomega.Equal([]string{"get"}))
	g.Expect(role.Rules[0].ResourceNames).To(gomega.Equal([]string{"keystone-fernet-keys"}))
	// Rule 2: get+patch on the staging secret only (no create/delete).
	g.Expect(role.Rules[1].Verbs).To(gomega.Equal([]string{"get", "patch"}))
	g.Expect(role.Rules[1].ResourceNames).To(gomega.Equal([]string{"keystone-fernet-keys-rotation"}))

	rb := &rbacv1.RoleBinding{}
	g.Expect(c.Get(context.Background(), apitypes.NamespacedName{Namespace: "openstack", Name: "keystone-fernet-rotate"}, rb)).To(gomega.Succeed())
	g.Expect(rb.RoleRef.Name).To(gomega.Equal("keystone-fernet-rotate"))
}

func commitSpec() CommitSpec {
	return CommitSpec{
		TargetNoun:              "main secret",
		Validate:                func(map[string][]byte) error { return nil },
		ClearStagingOnReject:    true,
		AnnotationInvalidReason: "AnnInvalid",
		RejectedReason:          "Rejected",
		AppliedReason:           "Rotated",
		AppliedMessage:          func(name string, _ map[string][]byte) string { return "applied from " + name },
	}
}

func stagingSecret(annotated bool, data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: "openstack", UID: "s-uid", ResourceVersion: "1"},
		Data:       data,
	}
	if annotated {
		s.Annotations = map[string]string{CompletedAnnotation: time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339)}
	}
	return s
}

func TestCommitStaged_AppliesAndDeletes(t *testing.T) {
	g := gomega.NewWithT(t)
	s := rotationScheme(t)
	staging := stagingSecret(true, map[string][]byte{"key1": []byte("v")})
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "openstack"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(staging, target).Build()
	rec := record.NewFakeRecorder(4)

	applied, err := CommitStaged(context.Background(), c, rec, rotationOwner(), staging, target, commitSpec())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(applied).To(gomega.BeTrue())
	// Target holds the payload and the completion annotation.
	g.Expect(target.Data).To(gomega.HaveKeyWithValue("key1", []byte("v")))
	g.Expect(target.Annotations).To(gomega.HaveKey(CompletedAnnotation))
	// Staging secret deleted.
	err = c.Get(context.Background(), apitypes.NamespacedName{Namespace: "openstack", Name: "staging"}, &corev1.Secret{})
	g.Expect(err).To(gomega.HaveOccurred())
	var ev string
	g.Eventually(rec.Events).Should(gomega.Receive(&ev))
	g.Expect(ev).To(gomega.ContainSubstring("Normal Rotated"))
}

func TestCommitStaged_NoAnnotationNoOp(t *testing.T) {
	g := gomega.NewWithT(t)
	s := rotationScheme(t)
	staging := stagingSecret(false, nil)
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "openstack"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(staging, target).Build()

	applied, err := CommitStaged(context.Background(), c, record.NewFakeRecorder(1), rotationOwner(), staging, target, commitSpec())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(applied).To(gomega.BeFalse())
	g.Expect(target.Data).To(gomega.BeNil())
}

func TestCommitStaged_RejectClearsStaging(t *testing.T) {
	g := gomega.NewWithT(t)
	s := rotationScheme(t)
	staging := stagingSecret(true, map[string][]byte{"bad": []byte("x")})
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "main", Namespace: "openstack"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(staging, target).Build()
	rec := record.NewFakeRecorder(4)
	spec := commitSpec()
	spec.Validate = func(map[string][]byte) error { return fmt.Errorf("bad payload") }

	applied, err := CommitStaged(context.Background(), c, rec, rotationOwner(), staging, target, spec)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(applied).To(gomega.BeFalse())
	// Staging Data cleared so the next PATCH starts from an empty base.
	g.Expect(staging.Data).To(gomega.BeNil())
	g.Expect(staging.Annotations).NotTo(gomega.HaveKey(CompletedAnnotation))
	var ev string
	g.Eventually(rec.Events).Should(gomega.Receive(&ev))
	g.Expect(ev).To(gomega.ContainSubstring("Warning Rejected"))
}

func TestBuildCronJob(t *testing.T) {
	g := gomega.NewWithT(t)
	cj := BuildCronJob(CronJobParams{
		Name: "keystone-fernet-rotate", Namespace: "openstack",
		Labels: map[string]string{"app": "keystone"}, Schedule: "0 0 * * 0",
		PodLabels: map[string]string{"app": "keystone"},
		PodSpec:   corev1.PodSpec{ServiceAccountName: "keystone-fernet-rotate"},
	})
	g.Expect(cj.Name).To(gomega.Equal("keystone-fernet-rotate"))
	g.Expect(cj.Spec.Schedule).To(gomega.Equal("0 0 * * 0"))
	g.Expect(cj.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName).To(gomega.Equal("keystone-fernet-rotate"))
	g.Expect(cj.Spec.JobTemplate.Spec.Template.Labels).To(gomega.HaveKeyWithValue("app", "keystone"))
}

var _ client.Object = (*corev1.ConfigMap)(nil)
