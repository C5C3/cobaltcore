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

	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// readyMetadataDeployment returns the metadata Deployment the step builds, with
// the status of a completed rollout.
func readyMetadataDeployment(nova *novav1alpha1.Nova) *appsv1.Deployment {
	return markDeploymentRolledOut(buildMetadataDeployment(nova, workloadArtifacts(), workloadDigests{}))
}

// TestBuildMetadataDeployment covers what separates the metadata API from the
// compute API: its own WSGI module on its own port, the overlay that makes it
// trust a proxied instance identity, and the shared secret it verifies the
// signature of such a request with.
func TestBuildMetadataDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	deploy := buildMetadataDeployment(nova, workloadArtifacts(), workloadTestDigests())
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(deploy.Name).To(Equal("nova-metadata"))
	g.Expect(deploy.Name).To(Equal(metadataName(nova)))
	g.Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", componentMetadata))
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(
		componentSelectorLabels(nova, componentMetadata)))
	g.Expect(deploy.Spec.Template.Annotations).To(
		HaveKeyWithValue(metadataSecretHashAnnotation, "meta5"))
	g.Expect(deploy.Spec.Template.Annotations).To(
		HaveKeyWithValue(installedReleaseAnnotation, "2025.2"))

	g.Expect(container.Name).To(Equal("nova-metadata"))
	g.Expect(container.Command).To(ContainElements("--http", ":8775",
		"--module", "nova.wsgi.metadata:application"))
	g.Expect(container.Command).NotTo(ContainElement("--pyargv"),
		"nova's WSGI entry points read their documents from the environment")
	g.Expect(container.Ports).To(Equal([]corev1.ContainerPort{
		{Name: "nova-metadata", ContainerPort: novaMetadataPort},
	}))
	g.Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/"))
	g.Expect(container.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(8775))
	g.Expect(container.LivenessProbe.HTTPGet.Port.IntValue()).To(Equal(8775))

	g.Expect(envNames(container.Env)).To(ContainElements(
		metadataSharedSecretEnvVarName, novaConfigDirEnvVarName, novaConfigFilesEnvVarName))
	g.Expect(container.Env).To(ContainElement(corev1.EnvVar{
		Name:  novaConfigFilesEnvVarName,
		Value: apiPasteConfigPath + ";" + novaConfDataKey + ";" + metadataConfDataKey,
	}), "the front end loads the paste pipeline and both documents of its role")

	mountPaths := make([]string, 0, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		mountPaths = append(mountPaths, mount.MountPath)
	}
	g.Expect(mountPaths).To(Equal([]string{novaConfigDir, novaStatePath, tmpMountPath}),
		"the metadata API reads one config directory, its overlay included")
}

// TestBuildMetadataService covers the address the Neutron metadata agent of
// every compute node dials: the metadata port, and a selector that keeps the
// other four workloads out of its endpoints.
func TestBuildMetadataService(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	svc := buildMetadataService(nova)

	g.Expect(svc.Name).To(Equal("nova-metadata"))
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{
		"app.kubernetes.io/name":      "nova",
		"app.kubernetes.io/instance":  "nova",
		"app.kubernetes.io/component": componentMetadata,
	}))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(novaMetadataPort))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(int(novaMetadataPort)))
}

// TestReconcileMetadata_ReturnsZeroOutsideAnUpgrade covers the result contract:
// the API step runs after this one, so a metadata API that is still starting
// must report its state without holding the pass back.
func TestReconcileMetadata_ReturnsZeroOutsideAnUpgrade(t *testing.T) {
	ctx := context.Background()

	t.Run("a metadata API still rolling out", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova)

		res, err := r.reconcileMetadata(ctx, r.Client, nova, workloadArtifacts(), workloadTestDigests())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(r.Get(ctx, objectKey("nova-metadata"), &appsv1.Deployment{})).To(Succeed())
		g.Expect(r.Get(ctx, objectKey("nova-metadata"), &corev1.Service{})).To(Succeed())
		cond := novaCondition(nova, "MetadataReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForMetadata))
	})

	t.Run("an available metadata API", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova, readyMetadataDeployment(nova))

		res, err := r.reconcileMetadata(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := novaCondition(nova, "MetadataReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonMetadataReady))
	})
}

// TestReconcileMetadata_RollingUpdateHoldsUntilConverged covers the upgrade
// gate: the contract phase runs data migrations the old front end has no code
// for, so the pass polls until every replica runs the new image.
func TestReconcileMetadata_RollingUpdateHoldsUntilConverged(t *testing.T) {
	ctx := context.Background()

	t.Run("a surge pod still running holds the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		surging := readyMetadataDeployment(nova)
		surging.Status.Replicas++
		r := newNovaTestReconciler(nova, surging)

		res, err := r.reconcileMetadata(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := novaCondition(nova, "MetadataReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForMetadata))
	})

	t.Run("a converged rollout releases the pass", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		r := newNovaTestReconciler(nova, readyMetadataDeployment(nova))

		res, err := r.reconcileMetadata(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		g.Expect(novaCondition(nova, "MetadataReady").Status).To(Equal(metav1.ConditionTrue))
	})
}

// TestReconcileMetadata_ApplyFailureWrapsTheError covers the error path: the
// message names the metadata API, so a pipeline error is not confused with the
// compute API's Deployment.
func TestReconcileMetadata_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("quota exceeded")
	r := failingApplyReconciler(boom, "Deployment", metadataName(nova), nova)

	_, err := r.reconcileMetadata(context.Background(), r.Client, nova,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring metadata Deployment:")))
}

// TestReconcileMetadata_ServiceFailureWrapsTheError covers the second object the
// step owns: a Service the API server rejects must name itself in the error.
func TestReconcileMetadata_ServiceFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	boom := errors.New("field manager conflict")
	r := failingApplyReconciler(boom, "Service", metadataName(nova), nova)

	_, err := r.reconcileMetadata(context.Background(), r.Client, nova,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring metadata Service:")))
}

// TestBuildMetadataDeployment_RendersResourceDefaults verifies that the
// metadata API memory follows spec.metadata.uwsgi, beside a 100m CPU request
// and no CPU limit: 512Mi at the default counts, 800Mi at four processes, and
// still 512Mi when only spec.api.uwsgi moves.
func TestBuildMetadataDeployment_RendersResourceDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(nova *novav1alpha1.Nova)
		want   string
	}{
		{name: "default uWSGI counts", mutate: func(*novav1alpha1.Nova) {}, want: "512Mi"},
		{
			name:   "four metadata processes",
			mutate: func(nova *novav1alpha1.Nova) { nova.Spec.Metadata.UWSGI.Processes = 4 },
			want:   "800Mi",
		},
		{
			name:   "four API processes",
			mutate: func(nova *novav1alpha1.Nova) { nova.Spec.API.UWSGI.Processes = 4 },
			want:   "512Mi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			tc.mutate(nova)

			deploy := buildMetadataDeployment(nova, workloadArtifacts(), workloadDigests{})

			g.Expect(deploy.Spec.Template.Spec.Containers[0].Resources).To(Equal(testutil.RenderedResourceDefaults(tc.want)))
		})
	}
}
