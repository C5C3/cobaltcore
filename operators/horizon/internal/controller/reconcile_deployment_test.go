// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/c5c3/forge/internal/common/conditions"
)

// findContainer returns the container with the given name, avoiding brittle
// index-based access.
func findContainer(t *testing.T, containers []corev1.Container, name string) *corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	t.Fatalf("container %q not found", name)
	return nil
}

// expectRestrictedSecurityContext asserts every field the Pod Security
// Standards Restricted profile requires on the container security context.
func expectRestrictedSecurityContext(g *WithT, sc *corev1.SecurityContext) {
	g.Expect(sc).NotTo(BeNil())
	g.Expect(sc.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
	g.Expect(sc.ReadOnlyRootFilesystem).To(HaveValue(BeTrue()))
	g.Expect(sc.RunAsNonRoot).To(HaveValue(BeTrue()))
	g.Expect(sc.Capabilities).NotTo(BeNil())
	g.Expect(sc.Capabilities.Drop).To(Equal([]corev1.Capability{"ALL"}))
	g.Expect(sc.SeccompProfile).NotTo(BeNil())
	g.Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
}

func TestBuildHorizonDeployment_Shape(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()

	deploy := buildHorizonDeployment(h, "test-horizon-config-abc12345", "digest123")

	g.Expect(deploy.Name).To(Equal("test-horizon"))
	g.Expect(deploy.Spec.Replicas).To(HaveValue(Equal(int32(3))))

	container := findContainer(t, deploy.Spec.Template.Spec.Containers, "horizon")
	g.Expect(container.Image).To(Equal("ghcr.io/c5c3/horizon:2025.2"))
	expectRestrictedSecurityContext(g, container.SecurityContext)

	// uWSGI loads the dashboard module directly and serves the pre-built
	// static assets; assert flag/value pairs, not the full ordered slice.
	cmd := container.Command
	g.Expect(cmd).To(ContainElement("uwsgi"))
	g.Expect(cmd).To(ContainElements("--module", "openstack_dashboard.wsgi"))
	g.Expect(cmd).To(ContainElements("--http", ":8080"))
	g.Expect(cmd).To(ContainElement("--static-map"))
	g.Expect(cmd).To(ContainElement("/static=/var/lib/openstack/horizon-static"))

	// SECRET_KEY env var sourced from the referenced Secret and key.
	var secretEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "HORIZON_SECRET_KEY" {
			secretEnv = &container.Env[i]
		}
	}
	g.Expect(secretEnv).NotTo(BeNil())
	g.Expect(secretEnv.ValueFrom.SecretKeyRef.Name).To(Equal("horizon-secret-key"))
	g.Expect(secretEnv.ValueFrom.SecretKeyRef.Key).To(Equal("secret-key"))

	// The rendered settings ConfigMap mounts where the image symlink points.
	mounts := map[string]string{}
	for _, m := range container.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	g.Expect(mounts).To(HaveKeyWithValue("config", "/etc/openstack-dashboard/"))

	volumes := map[string]string{}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			volumes[v.Name] = v.ConfigMap.Name
		}
	}
	g.Expect(volumes).To(HaveKeyWithValue("config", "test-horizon-config-abc12345"))

	// Probes render the login page (no live Keystone required).
	g.Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/auth/login/"))
	g.Expect(container.StartupProbe.HTTPGet.Path).To(Equal("/auth/login/"))
	g.Expect(container.LivenessProbe.TCPSocket).NotTo(BeNil())

	// The HTTP probes pin the Host header to a fixed value so the requests
	// satisfy Django's ALLOWED_HOSTS allow-list without the operator having to
	// allow-list the dynamic pod IP (see allowedHosts).
	wantHostHeader := []corev1.HTTPHeader{{Name: "Host", Value: "localhost"}}
	g.Expect(container.ReadinessProbe.HTTPGet.HTTPHeaders).To(Equal(wantHostHeader))
	g.Expect(container.StartupProbe.HTTPGet.HTTPHeaders).To(Equal(wantHostHeader))

	// Rotated SECRET_KEY rolls the pods via the hash annotation.
	g.Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue(secretKeyHashAnnotation, "digest123"))
}

func TestBuildHorizonDeployment_NoHashAnnotationWhenDigestEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()

	deploy := buildHorizonDeployment(h, "cm", "")

	g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey(secretKeyHashAnnotation))
}

func TestBuildHorizonDeployment_AutoscalingLeavesReplicasUnmanaged(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Autoscaling = autoscalingSpecWithCPU(2, 5)

	deploy := buildHorizonDeployment(h, "cm", "")

	g.Expect(deploy.Spec.Replicas).To(BeNil(),
		"replicas must stay unmanaged when the HPA owns the count")
}

func TestReconcileDeployment_NotReadySetsConditionAndRequeues(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)

	res, err := r.reconcileDeployment(context.Background(), h, "cm-name", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(RequeueDeploymentPolling))
	cond := conditions.GetCondition(h.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("WaitingForDeployment"))
	g.Expect(h.Status.Endpoint).To(BeEmpty())
}

func TestReconcileDeployment_ReadySetsEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)
	ctx := context.Background()

	// First pass creates the Deployment; then simulate availability.
	_, err := r.reconcileDeployment(ctx, h, "cm-name", "")
	g.Expect(err).NotTo(HaveOccurred())

	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: "default", Name: "test-horizon"}
	g.Expect(r.Get(ctx, key, &deploy)).To(Succeed())
	deploy.Status.ReadyReplicas = 3
	deploy.Status.Replicas = 3
	deploy.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
	}}
	g.Expect(r.Status().Update(ctx, &deploy)).To(Succeed())

	res, err := r.reconcileDeployment(ctx, h, "cm-name", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(h.Status.Endpoint).To(Equal("http://test-horizon.default.svc.cluster.local:8080/"))
	cond := conditions.GetCondition(h.Status.Conditions, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

func TestBuildHorizonService_Port8080(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()

	svc := buildHorizonService(h)

	g.Expect(svc.Name).To(Equal("test-horizon"))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
	g.Expect(svc.Spec.Selector).To(Equal(selectorLabels(h)))
}
