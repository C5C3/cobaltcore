//go:build integration

package config_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/config"
	testenvtest "github.com/c5c3/forge/internal/common/testutil/envtest"
)

var (
	k8sClient client.Client
	scheme    *k8sruntime.Scheme
	testNS    string
)

func TestMain(m *testing.M) {
	_, c, teardown, err := testenvtest.SetupEnvTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup envtest: %v\n", err)
		os.Exit(1)
	}
	k8sClient = c

	// Retrieve the scheme from the client for owner reference setup.
	scheme = k8sruntime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-config-",
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

func TestCreateImmutableConfigMap_CreatesWithHashSuffix(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner-create",
			Namespace: testNS,
			UID:       "fake-uid-create",
		},
	}
	g.Expect(k8sClient.Create(ctx, owner)).To(Succeed())

	data := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}

	cmName, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "my-config", testNS, data)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cmName).To(HavePrefix("my-config-"))
	g.Expect(len(cmName)).To(Equal(len("my-config-") + 8))

	// Verify the ConfigMap exists with correct data.
	cm := &corev1.ConfigMap{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cmName, Namespace: testNS}, cm)).To(Succeed())
	g.Expect(cm.Data).To(Equal(data))
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue())
}

func TestCreateImmutableConfigMap_Idempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner-idempotent",
			Namespace: testNS,
			UID:       "fake-uid-idempotent",
		},
	}
	g.Expect(k8sClient.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"idem": "potent"}

	cmName1, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "idem-config", testNS, data)
	g.Expect(err).NotTo(HaveOccurred())

	cmName2, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "idem-config", testNS, data)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cmName2).To(Equal(cmName1))

	// Verify only one ConfigMap with that name exists (list by exact name).
	cmList := &corev1.ConfigMapList{}
	g.Expect(k8sClient.List(ctx, cmList, client.InNamespace(testNS))).To(Succeed())
	count := 0
	for _, cm := range cmList.Items {
		if cm.Name == cmName1 {
			count++
		}
	}
	g.Expect(count).To(Equal(1))
}

func TestCreateImmutableConfigMap_DifferentDataDifferentHash(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner-diffhash",
			Namespace: testNS,
			UID:       "fake-uid-diffhash",
		},
	}
	g.Expect(k8sClient.Create(ctx, owner)).To(Succeed())

	data1 := map[string]string{"env": "dev"}
	data2 := map[string]string{"env": "prod"}

	cmName1, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "hash-config", testNS, data1)
	g.Expect(err).NotTo(HaveOccurred())

	cmName2, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "hash-config", testNS, data2)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cmName1).NotTo(Equal(cmName2))
}

func TestCreateImmutableConfigMap_OwnerReferenceSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-owner-ref",
			Namespace: testNS,
			UID:       "fake-uid-ownerref",
		},
	}
	g.Expect(k8sClient.Create(ctx, owner)).To(Succeed())

	data := map[string]string{"owner": "test"}

	cmName, err := config.CreateImmutableConfigMap(ctx, k8sClient, owner, scheme, "owned-config", testNS, data)
	g.Expect(err).NotTo(HaveOccurred())

	cm := &corev1.ConfigMap{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cmName, Namespace: testNS}, cm)).To(Succeed())
	g.Expect(cm.OwnerReferences).To(HaveLen(1))
	g.Expect(cm.OwnerReferences[0].Name).To(Equal(owner.Name))
	g.Expect(cm.OwnerReferences[0].UID).To(Equal(owner.UID))
}
