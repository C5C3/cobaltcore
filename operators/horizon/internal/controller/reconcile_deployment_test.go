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
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/naming"
	commonreconcile "github.com/c5c3/forge/internal/common/reconcile"
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

	// The dashboard pods carry the component the Service selects on.
	g.Expect(deploy.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
	g.Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
	// The Deployment selector is immutable, so the component key must stay out
	// of it: adding one would force a delete/recreate of every live Deployment.
	g.Expect(deploy.Spec.Selector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
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
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
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

// TestReconcileDeployment_SelectorsNarrowOnlyAfterRollout pins the two-phase
// narrowing of the dashboard Service selector. EnsureDeployment is a
// Server-Side Apply that returns as soon as the API server accepts the pod
// template, so narrowing to app.kubernetes.io/component=api in the same pass
// would drop every pod still running from the pre-upgrade template out of the
// EndpointSlices — the Service would have no backends at all until the first
// re-rolled pod passed its probes, and a rollout that wedges would never
// repool them. The PDB selector, by contrast, covers the dashboard pods in
// every phase — labelled or not, see buildPodDisruptionBudget.
func TestReconcileDeployment_SelectorsNarrowOnlyAfterRollout(t *testing.T) {
	cases := []struct {
		name string
		// updatedReplicas is what the deployment controller reports; a value
		// below the replica count means old-template pods are still counted
		// and still serving.
		updatedReplicas int32
		totalReplicas   int32
		narrowed        bool
	}{
		{name: "rollout in flight", updatedReplicas: 1, totalReplicas: 4, narrowed: false},
		{name: "fully rolled out", updatedReplicas: 3, totalReplicas: 3, narrowed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			h := testHorizon()
			r := newTestReconciler(testScheme(), h)
			ctx := context.Background()

			// First pass creates the Deployment; then stamp the rollout state.
			_, err := r.reconcileDeployment(ctx, h, "cm-name", "")
			g.Expect(err).NotTo(HaveOccurred())

			key := types.NamespacedName{Namespace: "default", Name: "test-horizon"}
			var deploy appsv1.Deployment
			g.Expect(r.Get(ctx, key, &deploy)).To(Succeed())
			deploy.Status.ReadyReplicas = 3
			deploy.Status.UpdatedReplicas = tc.updatedReplicas
			deploy.Status.Replicas = tc.totalReplicas
			deploy.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}}
			g.Expect(r.Status().Update(ctx, &deploy)).To(Succeed())

			_, err = r.reconcileDeployment(ctx, h, "cm-name", "")
			g.Expect(err).NotTo(HaveOccurred())

			var svc corev1.Service
			g.Expect(r.Get(ctx, key, &svc)).To(Succeed())
			var pdb policyv1.PodDisruptionBudget
			g.Expect(r.Get(ctx, key, &pdb)).To(Succeed())

			if tc.narrowed {
				g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
			} else {
				g.Expect(svc.Spec.Selector).NotTo(HaveKey(naming.LabelKeyComponent),
					"pods from the pre-upgrade template must stay in the EndpointSlices until the rollout completes")
			}
			// Either way the dashboard pods must satisfy the Service selector,
			// or it has no backends at all.
			podLabels := buildHorizonDeployment(h, "cm-name", "").Spec.Template.Labels
			g.Expect(labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(podLabels))).To(BeTrue())
			// The budget covers the dashboard pods in BOTH phases, the
			// pre-upgrade ones included. Anything else leaves the pods that are
			// actually serving outside every budget for the whole migration —
			// and forever if the rollout wedges — because the eviction API only
			// consults budgets matching the pod being evicted.
			pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pdbSelector.Matches(labels.Set(podLabels))).To(BeTrue(),
				"the budget must cover the dashboard pods")
			g.Expect(pdbSelector.Matches(labels.Set(commonLabels(h)))).To(BeTrue(),
				"the budget must cover dashboard pods from a template predating the component label")
		})
	}
}

// TestReconcileDeployment_NarrowedServiceSelectorNeverWidens pins the latch on
// the two-phase narrowing. deployment.TemplateConverged turns false on every
// later rollout, so deriving the selector from it on every pass would not
// migrate the Service once but oscillate it.
func TestReconcileDeployment_NarrowedServiceSelectorNeverWidens(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)
	key := types.NamespacedName{Namespace: "default", Name: "test-horizon"}

	// Pass 1 creates the Deployment; stamping full convergence and reconciling
	// again completes the migration and narrows the Service selector.
	_, err := r.reconcileDeployment(ctx, h, "cm-name", "")
	g.Expect(err).NotTo(HaveOccurred())
	var deploy appsv1.Deployment
	g.Expect(r.Get(ctx, key, &deploy)).To(Succeed())
	deploy.Status.ReadyReplicas = 3
	deploy.Status.UpdatedReplicas = 3
	deploy.Status.Replicas = 3
	deploy.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
	}}
	g.Expect(r.Status().Update(ctx, &deploy)).To(Succeed())

	_, err = r.reconcileDeployment(ctx, h, "cm-name", "")
	g.Expect(err).NotTo(HaveOccurred())
	var svc corev1.Service
	g.Expect(r.Get(ctx, key, &svc)).To(Succeed())
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))

	// A later rollout drops the Deployment back below full convergence. The
	// Deployment is watched without a generation predicate, so this status
	// transition drives a reconcile of its own.
	g.Expect(r.Get(ctx, key, &deploy)).To(Succeed())
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.Replicas = 4
	g.Expect(r.Status().Update(ctx, &deploy)).To(Succeed())

	_, err = r.reconcileDeployment(ctx, h, "cm-name", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, key, &svc)).To(Succeed())
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI),
		"an already narrowed Service selector must never widen again — every pod template this operator builds carries the component label")
}

func TestBuildHorizonService_Port8080(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()

	svc := buildHorizonService(h, true)

	g.Expect(svc.Name).To(Equal("test-horizon"))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
	// The Service routes to a numeric targetPort, which admits any pod of this
	// instance regardless of the ports it declares, and a pod without a
	// readiness probe counts as Ready as soon as it starts. The component key
	// is what keeps a workload other than the dashboard from becoming an
	// endpoint with nothing listening on 8080.
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{
		"app.kubernetes.io/name":      "horizon",
		"app.kubernetes.io/instance":  "test-horizon",
		"app.kubernetes.io/component": "api",
	}))

	// The dashboard pods must keep satisfying the selector, or the Service
	// would have no backends at all.
	podLabels := buildHorizonDeployment(h, "cm", "").Spec.Template.Labels
	g.Expect(labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(podLabels))).To(BeTrue(),
		"the dashboard pod template must satisfy the Service selector")
}
