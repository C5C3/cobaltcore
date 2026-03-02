//go:build integration

package policy_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/policy"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var (
	k8sClient client.Client
	testNS    string
)

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-policy-",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test namespace: %v\n", err)
		teardown()
		os.Exit(1)
	}
	testNS = ns.Name

	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestLoadPolicyFromConfigMap_HappyPath(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-happy",
			Namespace: testNS,
		},
		Data: map[string]string{
			"policy.yaml": "compute:create: role:member\ncompute:delete: role:admin\n",
		},
	}
	g.Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	ref := &corev1.LocalObjectReference{Name: "policy-happy"}
	rules, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ref, testNS)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rules).To(HaveKeyWithValue("compute:create", "role:member"))
	g.Expect(rules).To(HaveKeyWithValue("compute:delete", "role:admin"))
	g.Expect(rules).To(HaveLen(2))
}

func TestLoadPolicyFromConfigMap_ConfigMapNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	ref := &corev1.LocalObjectReference{Name: "does-not-exist"}
	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ref, testNS)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fetching ConfigMap"))
}

func TestLoadPolicyFromConfigMap_MissingPolicyYAMLKey(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-nokey",
			Namespace: testNS,
		},
		Data: map[string]string{
			"other.yaml": "some: content\n",
		},
	}
	g.Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	ref := &corev1.LocalObjectReference{Name: "policy-nokey"}
	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ref, testNS)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("missing required key"))
	g.Expect(err.Error()).To(ContainSubstring("policy.yaml"))
}

func TestLoadPolicyFromConfigMap_InvalidYAML(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "policy-badyaml",
			Namespace: testNS,
		},
		Data: map[string]string{
			"policy.yaml": "{{invalid yaml content",
		},
	}
	g.Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	ref := &corev1.LocalObjectReference{Name: "policy-badyaml"}
	_, err := policy.LoadPolicyFromConfigMap(ctx, k8sClient, ref, testNS)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing policy YAML"))
}
