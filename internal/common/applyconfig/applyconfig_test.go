package applyconfig

import (
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDefaultFieldManager verifies the constant value matches the expected
// operator field manager name. (CC-0005)
func TestDefaultFieldManager(t *testing.T) {
	g := NewGomegaWithT(t)
	g.Expect(DefaultFieldManager).To(Equal("cobaltcore-operator"))
}

// TestToApplyConfiguration_Deployment verifies that a typed Deployment with
// its GVK set is correctly converted to an ApplyConfiguration. (CC-0005)
func TestToApplyConfiguration_Deployment(t *testing.T) {
	g := NewGomegaWithT(t)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
		},
	}
	deploy.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))

	ac, err := ToApplyConfiguration(deploy)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ac).ToNot(BeNil())
}

// TestToApplyConfiguration_Service verifies conversion for a Service. (CC-0005)
func TestToApplyConfiguration_Service(t *testing.T) {
	g := NewGomegaWithT(t)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc",
			Namespace: "default",
		},
	}
	svc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))

	ac, err := ToApplyConfiguration(svc)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ac).ToNot(BeNil())
}

// TestToApplyConfiguration_CronJob verifies conversion for a CronJob. (CC-0005)
func TestToApplyConfiguration_CronJob(t *testing.T) {
	g := NewGomegaWithT(t)

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cronjob",
			Namespace: "default",
		},
	}
	cj.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("CronJob"))

	ac, err := ToApplyConfiguration(cj)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ac).ToNot(BeNil())
}

// TestToApplyConfiguration_NilObjectFails verifies that passing nil returns
// an error. (CC-0005)
func TestToApplyConfiguration_NilObjectFails(t *testing.T) {
	g := NewGomegaWithT(t)

	_, err := ToApplyConfiguration((*appsv1.Deployment)(nil))
	g.Expect(err).To(HaveOccurred())
}
