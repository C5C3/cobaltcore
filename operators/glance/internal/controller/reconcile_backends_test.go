// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	glancev1alpha1 "github.com/c5c3/forge/operators/glance/api/v1alpha1"
)

// collectEvents drains the FakeRecorder channel non-blocking.
func collectEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// renderedBackendsConf returns the backends.conf document from the projection
// Secret named secretName.
func renderedBackendsConf(t *testing.T, r *GlanceReconciler, secretName string) string {
	t.Helper()
	var s corev1.Secret
	key := client.ObjectKey{Namespace: "default", Name: secretName}
	if err := r.Get(context.Background(), key, &s); err != nil {
		t.Fatalf("re-reading backends Secret %s: %v", secretName, err)
	}
	return string(s.Data[backendsConfDataKey])
}

func TestReconcileBackends_ZeroAttachedIsNoDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance)

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "waiting states never requeue")
	g.Expect(proj.valid).To(BeFalse())
	g.Expect(proj.hosts).To(BeEmpty())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultBackend))
}

func TestReconcileBackends_SingleReadyDefaultProjects(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	backend := credentialReadyBackend("store", "test-glance", true)
	r := newGlanceTestReconciler(glance, backend, testS3CredentialsSecret("store"))

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(proj.valid).To(BeTrue())
	g.Expect(proj.enabledBackends).To(Equal("store:s3"))
	g.Expect(proj.defaultBackend).To(Equal("store"))
	g.Expect(proj.hosts).To(ConsistOf("https://s3.example.com"))
	g.Expect(proj.secretName).NotTo(BeEmpty())

	conf := renderedBackendsConf(t, r, proj.secretName)
	g.Expect(backendSectionPresent([]byte(conf), "store")).To(BeTrue(),
		"the store section header must be a whole line [store]")
	g.Expect(conf).To(ContainSubstring("s3_store_host = https://s3.example.com"))
	g.Expect(conf).To(ContainSubstring("s3_store_bucket = images"))
	g.Expect(conf).To(ContainSubstring("s3_store_access_key = " + testS3AccessKeyID))
	g.Expect(conf).To(ContainSubstring("s3_store_secret_key = " + testS3SecretAccessKey))
	// bucketURLFormat defaults to path.
	g.Expect(conf).To(ContainSubstring("s3_store_bucket_url_format = path"))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonAllBackendsProjected))
}

func TestReconcileBackends_TwoReadyDefaultsIsNoDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	r := newGlanceTestReconciler(glance,
		credentialReadyBackend("store", "test-glance", true), testS3CredentialsSecret("store"),
		credentialReadyBackend("store2", "test-glance", true), testS3CredentialsSecret("store2"))

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(proj.valid).To(BeFalse())
	// Both hosts still surface for the networkpolicy step.
	g.Expect(proj.hosts).To(HaveLen(2))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultBackend))
	g.Expect(cond.Message).To(And(ContainSubstring("store"), ContainSubstring("store2")))
}

func TestReconcileBackends_DefaultNotCredentialReadyIsNoDefault(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// The only default is attached but has no CredentialsReady=True condition, so
	// it is not a candidate: zero credential-ready defaults.
	notReady := testGlanceBackend("store", "test-glance")
	notReady.Spec.IsDefault = true
	r := newGlanceTestReconciler(glance, notReady, testS3CredentialsSecret("store"))

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(proj.valid).To(BeFalse())

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonNoDefaultBackend))
}

func TestReconcileBackends_ReadyDefaultWithPendingSiblingWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// The default is credential-ready; a sibling is attached but not yet
	// credential-ready. The projection is valid over the ready subset.
	pending := testGlanceBackend("store2", "test-glance")
	pending.Spec.S3.Host = "https://s3-2.example.com"
	r := newGlanceTestReconciler(glance,
		credentialReadyBackend("store", "test-glance", true), testS3CredentialsSecret("store"),
		pending)

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "waiting states never requeue")
	g.Expect(proj.valid).To(BeTrue())
	g.Expect(proj.enabledBackends).To(Equal("store:s3"), "only the ready subset is enabled")
	g.Expect(proj.defaultBackend).To(Equal("store"))
	// hosts include BOTH attached backends, ready or not.
	g.Expect(proj.hosts).To(ConsistOf("https://s3.example.com", "https://s3-2.example.com"))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
}

func TestReconcileBackends_ControlCharCredentialSkipsBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// "bad" is credential-ready but its access key carries a control character;
	// it must be skipped (with a Warning event) while the default still renders.
	badCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-s3-creds", Namespace: "default"},
		Data: map[string][]byte{
			glancev1alpha1.S3AccessKeyIDKey:     []byte("AKIA\nEVIL"),
			glancev1alpha1.S3SecretAccessKeyKey: []byte("secret"),
		},
	}
	r := newGlanceTestReconciler(glance,
		credentialReadyBackend("store", "test-glance", true), testS3CredentialsSecret("store"),
		credentialReadyBackend("bad", "test-glance", false), badCreds)

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(proj.valid).To(BeTrue())
	// Only the healthy default is enabled; the poisoned backend is skipped.
	g.Expect(proj.enabledBackends).To(Equal("store:s3"))

	conf := renderedBackendsConf(t, r, proj.secretName)
	g.Expect(backendSectionPresent([]byte(conf), "store")).To(BeTrue())
	g.Expect(backendSectionPresent([]byte(conf), "bad")).To(BeFalse(),
		"the skipped backend's section must not be rendered")

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(ContainSubstring("GlanceBackendSkipped")))

	cond := conditions.GetCondition(glance.Status.Conditions, conditionTypeBackendsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForBackends))
}

func TestReconcileBackends_MissingCredentialsSecretSkipsBackend(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// "store" is the ready default with creds; "gone" is credential-ready but its
	// credentials Secret has vanished — a per-backend fault, not a step failure.
	r := newGlanceTestReconciler(glance,
		credentialReadyBackend("store", "test-glance", true), testS3CredentialsSecret("store"),
		credentialReadyBackend("gone", "test-glance", false))

	res, proj, err := r.reconcileBackends(context.Background(), glance)

	g.Expect(err).NotTo(HaveOccurred(), "a missing per-backend Secret never fails the step")
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(proj.valid).To(BeTrue())
	g.Expect(proj.enabledBackends).To(Equal("store:s3"))

	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(ContainSubstring("GlanceBackendSkipped")))
}

func TestReconcileBackends_ExtraOptionsRenderedOperatorKeysWin(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	backend := credentialReadyBackend("store", "test-glance", true)
	// A CRD-bypass CR sets both a genuinely-extra option and one that collides
	// with an operator key; the operator key must win the collision.
	backend.Spec.ExtraOptions = map[string]string{
		"custom_opt":    "custom_value",
		"s3_store_host": "https://override.example.com",
	}
	r := newGlanceTestReconciler(glance, backend, testS3CredentialsSecret("store"))

	_, proj, err := r.reconcileBackends(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(proj.valid).To(BeTrue())

	conf := renderedBackendsConf(t, r, proj.secretName)
	g.Expect(conf).To(ContainSubstring("custom_opt = custom_value"))
	g.Expect(conf).To(ContainSubstring("s3_store_host = https://s3.example.com"),
		"the operator key wins over a colliding extraOption")
	g.Expect(conf).NotTo(ContainSubstring("override.example.com"))
}

func TestReconcileBackends_DeterministicSectionOrder(t *testing.T) {
	g := NewGomegaWithT(t)
	glance := testGlance()
	// Two credential-ready backends whose sorted order (aaa before bbb) must be
	// reflected in both enabled_backends and the rendered section order.
	r := newGlanceTestReconciler(glance,
		credentialReadyBackend("bbb", "test-glance", false), testS3CredentialsSecret("bbb"),
		credentialReadyBackend("aaa", "test-glance", true), testS3CredentialsSecret("aaa"))

	_, proj, err := r.reconcileBackends(context.Background(), glance)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(proj.valid).To(BeTrue())
	g.Expect(proj.enabledBackends).To(Equal("aaa:s3,bbb:s3"))

	conf := renderedBackendsConf(t, r, proj.secretName)
	g.Expect(strings.Index(conf, "[aaa]")).To(BeNumerically("<", strings.Index(conf, "[bbb]")))
}
