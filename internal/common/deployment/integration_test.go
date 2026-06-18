// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package deployment

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func intPtr(v int32) *int32 { return &v }

func TestIntegration_EnsureDeployment(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-deploy-ensure"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "deploy-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-deploy",
			Namespace: ns.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: intPtr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "integ-test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "integ-test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "busybox:latest"},
					},
				},
			},
		},
	}

	// Create.
	ready, err := EnsureDeployment(ctx, c, scheme, owner, deploy)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "newly created deployment should not be ready")

	created := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(deploy), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(Equal("deploy-owner"))

	// Update replicas.
	updated := deploy.DeepCopy()
	updated.Spec.Replicas = intPtr(3)
	ready, err = EnsureDeployment(ctx, c, scheme, owner, updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(deploy), fetched)).To(Succeed())
	g.Expect(*fetched.Spec.Replicas).To(Equal(int32(3)))
}

func TestIntegration_EnsureDeployment_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-deploy-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "deploy-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-deploy",
			Namespace: ns.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: intPtr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "idem"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "idem"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "busybox:latest"},
					},
				},
			},
		},
	}

	_, err := EnsureDeployment(ctx, c, scheme, owner, deploy)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = EnsureDeployment(ctx, c, scheme, owner, deploy)
	g.Expect(err).NotTo(HaveOccurred())

	list := &appsv1.DeploymentList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}

func TestIntegration_EnsureService(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-svc-ensure"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-owner", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integration-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "integ-test"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	// Create.
	g.Expect(EnsureService(ctx, c, scheme, owner, svc)).To(Succeed())

	created := &corev1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(svc), created)).To(Succeed())
	g.Expect(created.OwnerReferences).To(HaveLen(1))
	g.Expect(created.Spec.Ports[0].Port).To(Equal(int32(80)))

	// Update should preserve ClusterIP.
	assignedIP := created.Spec.ClusterIP
	g.Expect(assignedIP).NotTo(BeEmpty())

	updated := svc.DeepCopy()
	updated.Spec.Ports = []corev1.ServicePort{
		{Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	g.Expect(EnsureService(ctx, c, scheme, owner, updated)).To(Succeed())

	fetched := &corev1.Service{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(svc), fetched)).To(Succeed())
	g.Expect(fetched.Spec.ClusterIP).To(Equal(assignedIP))
	g.Expect(fetched.Spec.Ports[0].Port).To(Equal(int32(8080)))
}

func TestIntegration_EnsureService_idempotent(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-svc-idem"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-owner-idem", Namespace: ns.Name},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idem-svc",
			Namespace: ns.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "idem"},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	g.Expect(EnsureService(ctx, c, scheme, owner, svc)).To(Succeed())
	g.Expect(EnsureService(ctx, c, scheme, owner, svc)).To(Succeed())

	list := &corev1.ServiceList{}
	g.Expect(c.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))
}
