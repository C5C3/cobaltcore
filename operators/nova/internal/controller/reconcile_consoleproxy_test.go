// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// readyConsoleProxyDeployment returns the console proxy Deployment the step
// builds, with the status of a completed rollout.
func readyConsoleProxyDeployment(nova *novav1alpha1.Nova) *appsv1.Deployment {
	return markDeploymentRolledOut(
		buildConsoleProxyDeployment(nova, workloadArtifacts(), workloadDigests{}))
}

// disabledProxyNova returns the fixture with the console switched off, which is
// the shape a user who wants no console submits.
func disabledProxyNova() *novav1alpha1.Nova {
	nova := validNova()
	nova.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
	return nova
}

// TestBuildConsoleProxyDeployment covers the proxy's own shape: the noVNC client
// page it is measured on, its overlay directory, and the one connection it
// opens.
func TestBuildConsoleProxyDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	deploy := buildConsoleProxyDeployment(nova, workloadArtifacts(), workloadTestDigests())
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(deploy.Name).To(Equal("nova-novncproxy"))
	g.Expect(deploy.Name).To(Equal(consoleProxyName(nova)))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", componentConsoleProxy))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(
		componentSelectorLabels(nova, componentConsoleProxy)))
	g.Expect(deploy.Spec.Template.Annotations).To(
		HaveKeyWithValue(installedReleaseAnnotation, "2025.2"))

	g.Expect(container.Command).To(Equal([]string{
		"nova-novncproxy",
		"--config-dir", "/etc/nova/nova.conf.d",
		"--config-dir", "/etc/nova/novncproxy.conf.d",
	}))
	g.Expect(mountPaths(container.VolumeMounts)).To(Equal([]string{
		novaConfigDir, "/etc/nova/novncproxy.conf.d", novaStatePath, tmpMountPath,
	}))

	g.Expect(container.Ports).To(Equal([]corev1.ContainerPort{
		{Name: componentConsoleProxy, ContainerPort: novaConsolePort},
	}))
	g.Expect(container.LivenessProbe).To(BeNil(),
		"a restart would cut every console session in flight")
	g.Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/vnc_lite.html"))
	g.Expect(container.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(6080))
	g.Expect(container.ReadinessProbe.FailureThreshold).To(Equal(int32(3)))

	g.Expect(envNames(container.Env)).To(ContainElement("OS_DATABASE__CONNECTION"))
	g.Expect(envNames(container.Env)).NotTo(ContainElement("OS_API_DATABASE__CONNECTION"),
		"the proxy reads the console tokens out of the cell schema alone")
}

// TestBuildConsoleProxyService covers the address the console URL names: the
// proxy port, and a selector that keeps the other four workloads out of its
// endpoints.
func TestBuildConsoleProxyService(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	svc := buildConsoleProxyService(nova)

	g.Expect(svc.Name).To(Equal("nova-novncproxy"))
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{
		"app.kubernetes.io/name":      "nova",
		"app.kubernetes.io/instance":  "nova",
		"app.kubernetes.io/component": componentConsoleProxy,
	}))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(novaConsolePort)))
	g.Expect(consoleBaseURL(nova)).To(ContainSubstring(consoleProxyVNCPath),
		"the page the API hands a browser is the page the probe measures")
}

// TestReconcileConsoleProxy_NilDeploymentBlockFallsBackToOneReplica covers the
// CR that bypassed the defaulting webhook: the block is a pointer the webhook
// only materializes, and a nil one must not panic the builder.
func TestReconcileConsoleProxy_NilDeploymentBlockFallsBackToOneReplica(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	nova.Spec.ConsoleProxy.Deployment = nil

	deploy := buildConsoleProxyDeployment(nova, workloadArtifacts(), workloadDigests{})

	g.Expect(deploy.Spec.Replicas).NotTo(BeNil())
	g.Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
}

// TestReconcileConsoleProxy_DisabledDeletesAndReportsDisabled covers the opt-out
// arm: a proxy switched off after it ran leaves no pod and no address behind,
// and the condition stays True because a Nova without a console serves instances
// as before.
func TestReconcileConsoleProxy_DisabledDeletesAndReportsDisabled(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	enabled := validNova()
	nova := disabledProxyNova()
	r := newNovaTestReconciler(nova,
		readyConsoleProxyDeployment(enabled), buildConsoleProxyService(enabled))

	res, err := r.reconcileConsoleProxy(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(r.Get(ctx, objectKey("nova-novncproxy"), &appsv1.Deployment{})).NotTo(Succeed())
	g.Expect(r.Get(ctx, objectKey("nova-novncproxy"), &corev1.Service{})).NotTo(Succeed())
	cond := novaCondition(nova, "ConsoleProxyReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonConsoleProxyDisabled))
}

// TestReconcileConsoleProxy_DisabledIsIdempotent covers the steady state of the
// opt-out arm: nothing to delete is not an error.
func TestReconcileConsoleProxy_DisabledIsIdempotent(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := disabledProxyNova()
	r := newNovaTestReconciler(nova)

	res, err := r.reconcileConsoleProxy(context.Background(), r.Client, nova,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(novaCondition(nova, "ConsoleProxyReady").Reason).To(
		Equal(conditionReasonConsoleProxyDisabled))
}

// TestReconcileConsoleProxy_ReturnsZeroOutsideAnUpgrade covers the result
// contract: a proxy that is still starting reports its state without holding the
// pass back.
func TestReconcileConsoleProxy_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()

	t.Run("a proxy still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova)

		res, err := r.reconcileConsoleProxy(ctx, r.Client, nova, workloadArtifacts(),
			workloadTestDigests())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("nova-novncproxy"), &appsv1.Deployment{})).To(Succeed())
		g.Expect(r.Get(ctx, objectKey("nova-novncproxy"), &corev1.Service{})).To(Succeed())
		cond := novaCondition(nova, "ConsoleProxyReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForConsoleProxy))
	})

	t.Run("an available proxy", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova, readyConsoleProxyDeployment(nova))

		res, err := r.reconcileConsoleProxy(ctx, r.Client, nova, workloadArtifacts(),
			workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := novaCondition(nova, "ConsoleProxyReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonConsoleProxyReady))
	})
}

// TestReconcileConsoleProxy_RollingUpdateHoldsUntilConverged covers the upgrade
// gate: the API step waits for this Deployment as well, so the step has to keep
// polling until its rollout converges.
func TestReconcileConsoleProxy_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()

	t.Run("a surge pod still running holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		surging := readyConsoleProxyDeployment(nova)
		surging.Status.Replicas++
		r := newNovaTestReconciler(nova, surging)

		res, err := r.reconcileConsoleProxy(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := novaCondition(nova, "ConsoleProxyReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForConsoleProxy))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		r := newNovaTestReconciler(nova, readyConsoleProxyDeployment(nova))

		res, err := r.reconcileConsoleProxy(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(novaCondition(nova, "ConsoleProxyReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileConsoleProxy_ApplyFailureWrapsTheError covers the error path: the
// message names the console proxy and the object it could not apply, so a
// pipeline error is not confused with another role's Deployment or with the API
// Service.
func TestReconcileConsoleProxy_ApplyFailureWrapsTheError(t *testing.T) {
	cases := []struct {
		kind string
		wrap string
	}{
		{kind: "Deployment", wrap: "ensuring console proxy Deployment:"},
		{kind: "Service", wrap: "ensuring console proxy Service:"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			boom := errors.New("quota exceeded")
			r := failingApplyReconciler(boom, tc.kind, consoleProxyName(nova), nova)

			_, err := r.reconcileConsoleProxy(context.Background(), r.Client, nova,
				workloadArtifacts(), workloadDigests{})

			g.Expect(err).To(MatchError(boom))
			g.Expect(err).To(MatchError(ContainSubstring(tc.wrap)))
			g.Expect(novaCondition(nova, "ConsoleProxyReady")).To(BeNil())
		})
	}
}

// failingDeleteReconciler builds a reconciler whose delete of the named kind and
// object fails, so the wrapping of the error on the opt-out arm can be asserted.
func failingDeleteReconciler(boom error, kind, name string, objs ...client.Object) *NovaReconciler {
	c := novaFakeClientBuilder(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				opts ...client.DeleteOption,
			) error {
				gvk, err := apiutil.GVKForObject(obj, cl.Scheme())
				if err == nil && gvk.Kind == kind && obj.GetName() == name {
					return boom
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	return &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(20)}
}

// TestReconcileConsoleProxy_DeleteFailureWrapsTheError covers the opt-out arm
// when the API server refuses the delete: the error has to name the object that
// survived, and the condition must not report the proxy as disabled while its
// pods or its address are still there.
func TestReconcileConsoleProxy_DeleteFailureWrapsTheError(t *testing.T) {
	cases := []struct {
		kind string
		wrap string
	}{
		{kind: "Deployment", wrap: "deleting console proxy Deployment nova-novncproxy:"},
		{kind: "Service", wrap: "deleting console proxy Service nova-novncproxy:"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			g := NewGomegaWithT(t)
			enabled := validNova()
			nova := disabledProxyNova()
			boom := errors.New("admission webhook denied the delete")
			r := failingDeleteReconciler(boom, tc.kind, consoleProxyName(nova), nova,
				readyConsoleProxyDeployment(enabled), buildConsoleProxyService(enabled))

			_, err := r.reconcileConsoleProxy(context.Background(), r.Client, nova,
				workloadArtifacts(), workloadDigests{})

			g.Expect(err).To(MatchError(boom))
			g.Expect(err).To(MatchError(ContainSubstring(tc.wrap)))
			g.Expect(novaCondition(nova, "ConsoleProxyReady")).To(BeNil(),
				"a proxy that is still projected is not disabled yet")
		})
	}
}

// TestBuildConsoleProxyDeployment_RendersResourceDefaults verifies that the
// console proxy, one single-threaded process, renders 368Mi as memory request
// and limit beside a 100m CPU request and no CPU limit, both for the webhook's
// block and for the consoleProxyDeploymentSpec fallback a CR that bypassed
// admission renders from.
func TestBuildConsoleProxyDeployment_RendersResourceDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*novav1alpha1.Nova)
	}{
		{name: "defaulted block", mutate: func(*novav1alpha1.Nova) {}},
		{name: "nil block fallback", mutate: func(nova *novav1alpha1.Nova) { nova.Spec.ConsoleProxy.Deployment = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.mutate(nova)

			deploy := buildConsoleProxyDeployment(nova, workloadArtifacts(), workloadDigests{})

			g.Expect(deploy.Spec.Template.Spec.Containers[0].Resources).To(Equal(testutil.RenderedResourceDefaults("368Mi")))
		})
	}
}
