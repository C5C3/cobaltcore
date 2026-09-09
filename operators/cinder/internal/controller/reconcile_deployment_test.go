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
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// workloadCinder is the fixture the workload steps run against: the shared
// Cinder with the four Deployment blocks the defaulting webhook materializes
// (one replica and Recreate on the two single-writer services, the raised backup
// memory limit). Running the real defaulter rather than restating its output
// keeps these tests describing a CR admission can actually produce.
func workloadCinder() *cinderv1alpha1.Cinder {
	return defaulted(validCinder())
}

// defaulted runs the real defaulting webhook over a fixture, so the workload
// tests describe a CR admission can actually produce instead of restating the
// webhook's output beside it.
func defaulted(cinder *cinderv1alpha1.Cinder) *cinderv1alpha1.Cinder {
	if err := (&cinderv1alpha1.CinderWebhook{}).Default(context.Background(), cinder); err != nil {
		panic("defaulting the workload fixture: " + err.Error())
	}
	return cinder
}

// workloadArtifacts stands in for what reconcileConfig hands the workload steps:
// a rendered config ConfigMap carrying the two always-present files plus the
// policy override, so the tests see a key the config volume selects and one
// (scheduler.conf) it must leave out.
func workloadArtifacts() configArtifacts {
	return configArtifacts{
		configMapName: "cinder-config-abc123",
		dataKeys:      []string{cinderConfDataKey, policyYAMLDataKey, schedulerConfDataKey},
	}
}

// workloadTestDigests are the three content digests the credential steps return.
func workloadTestDigests() workloadDigests {
	return workloadDigests{dsn: "dsn123", authToken: "auth456", transport: "bus789"}
}

// testEgressPort is the broker port the messaging step resolves, threaded into
// the readiness probe of the three bus processes.
const testEgressPort int32 = 5672

// objectKey addresses a named object in the fixture namespace.
func objectKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: name}
}

// markDeploymentRolledOut stamps the status of a Deployment whose rollout has
// fully converged onto the current pod template.
func markDeploymentRolledOut(deploy *appsv1.Deployment) *appsv1.Deployment {
	replicas := *deploy.Spec.Replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.UpdatedReplicas = replicas
	deploy.Status.Replicas = replicas
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}
	return deploy
}

// readyCinderDeployment returns the API Deployment the step builds, with the
// status of a completed rollout.
func readyCinderDeployment(cinder *cinderv1alpha1.Cinder) *appsv1.Deployment {
	return markDeploymentRolledOut(buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{}))
}

// failingApplyReconciler builds a reconciler whose apply of the named kind and
// object fails, so the wrapping of the error can be asserted.
func failingApplyReconciler(boom error, kind, name string, objs ...client.Object) *CinderReconciler {
	c := cinderFakeClientBuilder(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if co, ok := obj.(client.Object); ok &&
					co.GetObjectKind().GroupVersionKind().Kind == kind && co.GetName() == name {
					return boom
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()
	return &CinderReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(20)}
}

// rollingUpdateCinder returns the workload fixture mid-upgrade, parked in the
// RollingUpdate phase every workload step gates on.
func rollingUpdateCinder() *cinderv1alpha1.Cinder {
	return defaulted(upgradingCinder(commonv1.UpgradePhaseRollingUpdate))
}

// TestTopologySpreadSelectorsMatchTheDeployments feeds the pod selector of every
// Deployment the operator builds back into the validating webhook as a
// topologySpreadConstraints selector. The webhook demands exact equality with
// the block's selector labels, so one it rejects would leave the field unusable:
// the only accepted value would be a selector no Deployment carries.
//
// The volume block is the exception, and the assertion inverts there. One
// Deployment is projected per attached backend, each pinned to a single replica
// and narrowed by its own "volume-<backend>" component, so a selector that
// matches all of them matches the API, scheduler and backup pods too. The block
// takes no constraint at all rather than one measuring a volume pod against pods
// it does not control.
func TestTopologySpreadSelectorsMatchTheDeployments(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	art := workloadArtifacts()

	api := buildCinderDeployment(cinder, art, workloadDigests{})
	scheduler := buildSchedulerDeployment(cinder, art, workloadDigests{}, testEgressPort)
	volume := buildVolumeDeployment(cinder, testBackendProjection("nfs"), art, workloadDigests{}, testEgressPort)
	backup := buildBackupDeployment(cinder, testBackupProjection(), nil, art, workloadDigests{}, testEgressPort)

	obj := cinder.DeepCopy()
	obj.Spec.API.Deployment.TopologySpreadConstraints = spreadOver(api.Spec.Selector.MatchLabels)
	obj.Spec.Scheduler.Deployment.TopologySpreadConstraints = spreadOver(scheduler.Spec.Selector.MatchLabels)
	obj.Spec.Backup.Deployment.TopologySpreadConstraints = spreadOver(backup.Spec.Selector.MatchLabels)

	_, err := (&cinderv1alpha1.CinderWebhook{}).ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(HaveOccurred(),
		"the webhook must accept the selector of the Deployment each block configures")

	// Neither one projected Deployment's own selector nor the wider name+instance
	// pair every volume pod shares buys a constraint on the volume block.
	for name, labels := range map[string]map[string]string{
		"one projected Deployment's own selector": volume.Spec.Selector.MatchLabels,
		"the pair every volume pod shares":        selectorLabels(cinder),
	} {
		rejected := obj.DeepCopy()
		rejected.Spec.Volume.Deployment.TopologySpreadConstraints = spreadOver(labels)

		_, err := (&cinderv1alpha1.CinderWebhook{}).ValidateCreate(context.Background(), rejected)
		g.Expect(err).To(HaveOccurred(),
			"the volume block must take no topology-spread constraint, not even "+name)
		g.Expect(err.Error()).To(ContainSubstring("spec.volume.deployment.topologySpreadConstraints"))
	}

	// The empty list stays legal: it selects nothing and only switches the
	// injected per-Deployment defaults off.
	optedOut := obj.DeepCopy()
	optedOut.Spec.Volume.Deployment.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{}

	_, err = (&cinderv1alpha1.CinderWebhook{}).ValidateCreate(context.Background(), optedOut)
	g.Expect(err).NotTo(HaveOccurred(),
		"an empty list must stay accepted, or the injected defaults cannot be switched off")
}

// spreadOver returns one topology-spread constraint selecting the given labels.
func spreadOver(labels map[string]string) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
	}}
}

// TestReconcileDeployment_CreatesWorkloadAndWaitsForRollout covers the first
// pass: all three objects are projected, and the condition reports the rollout
// the CR is waiting on rather than an endpoint nothing serves yet.
func TestReconcileDeployment_CreatesWorkloadAndWaitsForRollout(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cinder := workloadCinder()
	r := newCinderTestReconciler(cinder)

	res, err := r.reconcileDeployment(ctx, r.Client, cinder, workloadArtifacts(), workloadTestDigests())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	g.Expect(r.Get(ctx, objectKey(cinder.Name), &appsv1.Deployment{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey(cinder.Name), &corev1.Service{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey(cinder.Name), &policyv1.PodDisruptionBudget{})).To(Succeed())

	cond := cinderCondition(cinder, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
	g.Expect(cinder.Status.Endpoint).To(BeEmpty(),
		"the endpoint is only advertised once the Deployment is available")
}

// TestReconcileDeployment_ReadyStampsTheEndpoint pins the URL clients read off
// the CR: the cluster-local Service address on the API port.
func TestReconcileDeployment_ReadyStampsTheEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	r := newCinderTestReconciler(cinder, readyCinderDeployment(cinder))

	res, err := r.reconcileDeployment(context.Background(), r.Client, cinder,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := cinderCondition(cinder, "DeploymentReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(cinder.Status.Endpoint).To(Equal("http://cinder.openstack.svc.cluster.local:8776"))
	g.Expect(cinder.Status.Endpoint).To(Equal(internalCinderURL(cinder)))
}

// TestReconcileDeployment_ApplyFailureWrapsTheError covers the error path: the
// step names the object it could not apply, so a pipeline error points at the
// Deployment rather than at the step in general.
func TestReconcileDeployment_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	boom := errors.New("admission webhook rejected the Deployment")
	r := failingApplyReconciler(boom, "Deployment", cinder.Name, cinder)

	_, err := r.reconcileDeployment(context.Background(), r.Client, cinder,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring Deployment:")))
}

// TestReconcileDeployment_RollingUpdateHoldsUntilTheImageIsDrained covers the
// upgrade gate. The surge-tolerant readiness turns true while old-image pods
// still serve, and the contract phase runs migrations those pods have no code
// for, so the flip to Contracting has to wait for the rollout to converge.
func TestReconcileDeployment_RollingUpdateHoldsUntilTheImageIsDrained(t *testing.T) {
	ctx := context.Background()

	t.Run("a surge pod still running holds the phase", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		// Ready by the surge-tolerant measure, not converged: one old-image pod is
		// still counted and not yet updated.
		surging := readyCinderDeployment(cinder)
		surging.Status.Replicas++
		r := newCinderTestReconciler(cinder, surging)

		res, err := r.reconcileDeployment(ctx, r.Client, cinder, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		g.Expect(cinder.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
		cond := cinderCondition(cinder, "DeploymentReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
		g.Expect(cinder.Status.Endpoint).To(BeEmpty())
	})

	t.Run("a drained rollout advances to contracting", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := rollingUpdateCinder()
		r := newCinderTestReconciler(cinder, readyCinderDeployment(cinder))

		res, err := r.reconcileDeployment(ctx, r.Client, cinder, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
		g.Expect(cinder.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseContracting))
		cond := cinderCondition(cinder, "DeploymentReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cinder.Status.Endpoint).To(BeEmpty(),
			"the endpoint is not stamped on the phase-flip pass")
		recorder, ok := r.Recorder.(*record.FakeRecorder)
		g.Expect(ok).To(BeTrue())
		g.Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("DeploymentRolloutComplete")))
	})
}

// TestCinderWorkloadVolumes covers the file layout every process shares: the
// config mount names the rendered keys one by one and leaves the scheduler
// overlay out, because oslo.config reads every file in a --config-dir and a pod
// that saw it would register under the scheduler's host identity.
func TestCinderWorkloadVolumes(t *testing.T) {
	g := NewGomegaWithT(t)
	volumes, mounts := cinderWorkloadVolumes(workloadCinder(), workloadArtifacts())

	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	g.Expect(names).To(Equal([]string{configVolumeName, stateVolumeName, tmpVolumeName}))

	g.Expect(volumes[0].ConfigMap.Name).To(Equal("cinder-config-abc123"))
	g.Expect(volumes[0].ConfigMap.Items).To(Equal([]corev1.KeyToPath{
		{Key: cinderConfDataKey, Path: cinderConfDataKey},
		{Key: policyYAMLDataKey, Path: policyYAMLDataKey},
	}), "scheduler.conf belongs to the scheduler alone")

	g.Expect(mounts[0].MountPath).To(Equal(cinderConfigDir))
	g.Expect(mounts[0].ReadOnly).To(BeTrue())
	g.Expect(mounts[1].MountPath).To(Equal(cinderStatePath))
	g.Expect(mounts[2].MountPath).To(Equal(tmpMountPath))
}

// TestCinderWorkloadVolumes_TLSProjections covers the two optional mounts: each
// exists only while its spec block does, and the CA bundle is renamed to the
// file name the rendered ssl_ca_file option points at.
func TestCinderWorkloadVolumes_TLSProjections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*cinderv1alpha1.Cinder)
		volume  string
		mounted string
	}{
		{
			name: "database TLS",
			mutate: func(cinder *cinderv1alpha1.Cinder) {
				cinder.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
					Mode:                "verify-ca",
					CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
					ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
				}
			},
			volume:  dbTLSVolumeName,
			mounted: dbTLSMountPath,
		},
		{
			name: "messaging TLS",
			mutate: func(cinder *cinderv1alpha1.Cinder) {
				cinder.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
					CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "bundle.pem"},
				}
			},
			volume:  rabbitmqCAVolumeName,
			mounted: rabbitmqCAMountPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cinder := workloadCinder()

			plain, _ := cinderWorkloadVolumes(cinder, workloadArtifacts())
			for _, volume := range plain {
				g.Expect(volume.Name).NotTo(Equal(tc.volume),
					"the projection must not exist while its spec block does not")
			}

			tc.mutate(cinder)
			volumes, mounts := cinderWorkloadVolumes(cinder, workloadArtifacts())
			g.Expect(volumes[len(volumes)-1].Name).To(Equal(tc.volume))
			g.Expect(mounts[len(mounts)-1]).To(Equal(corev1.VolumeMount{
				Name: tc.volume, MountPath: tc.mounted, ReadOnly: true,
			}))
		})
	}

	t.Run("the CA bundle is renamed to the configured file", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cinder := workloadCinder()
		cinder.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "bundle.pem"},
		}

		volumes, _ := cinderWorkloadVolumes(cinder, workloadArtifacts())
		ca := volumes[len(volumes)-1]
		g.Expect(ca.Secret.SecretName).To(Equal("rabbitmq-ca"))
		g.Expect(ca.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "bundle.pem", Path: "ca.crt"}}))
		g.Expect(rabbitmqCAFilePath).To(Equal(rabbitmqCAMountPath + "/ca.crt"))
	})
}

// TestCinderPodAnnotations covers the rollout triggers: a digest the upstream
// step could not compute leaves its annotation off rather than stamping an empty
// value, which would roll every pod the moment the credential appears.
func TestCinderPodAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(cinderPodAnnotations(workloadDigests{})).To(BeNil(),
		"a template with no digest at all carries no annotations")
	g.Expect(cinderPodAnnotations(workloadDigests{dsn: "dsn123"})).To(Equal(map[string]string{
		dbConnectionHashAnnotation: "dsn123",
	}))
	g.Expect(cinderPodAnnotations(workloadTestDigests())).To(Equal(map[string]string{
		dbConnectionHashAnnotation: "dsn123",
		authTokenHashAnnotation:    "auth456",
		transportURLHashAnnotation: "bus789",
	}))
}

// TestCinderRPCPodAnnotations covers the extra stamp the three bus processes
// carry: they cache the RPC versions their peers announced at startup, so the
// installed release has to roll them once the contract phase has run.
func TestCinderRPCPodAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()

	g.Expect(cinderRPCPodAnnotations(cinder, workloadDigests{})).To(Equal(map[string]string{
		installedReleaseAnnotation: "2026.1",
	}), "a fresh install stamps spec.openStackRelease rather than an empty value")

	cinder.Status.InstalledRelease = "2025.2"
	g.Expect(cinderRPCPodAnnotations(cinder, workloadDigests{dsn: "dsn123"})).To(Equal(map[string]string{
		dbConnectionHashAnnotation: "dsn123",
		installedReleaseAnnotation: "2025.2",
	}), "once the schema is installed, the marker is what the pods run against")

	g.Expect(cinderPodAnnotations(workloadDigests{})).To(BeNil(),
		"the API template stays free of the release stamp")
}

// TestBuildCinderService_And_PDB covers the selectors: one Cinder owns four
// kinds of Deployment, so the API Service and its budget must reach the API pods
// and nothing else.
func TestBuildCinderService_And_PDB(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := workloadCinder()
	want := map[string]string{
		"app.kubernetes.io/name":      "cinder",
		"app.kubernetes.io/instance":  "cinder",
		"app.kubernetes.io/component": "api",
	}

	svc := buildCinderService(cinder)
	g.Expect(svc.Spec.Selector).To(Equal(want))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(cinderAPIPort))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(int(cinderAPIPort)))

	pdb := buildPodDisruptionBudget(cinder)
	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(want))
	g.Expect(pdb.Spec.Selector.MatchExpressions).To(Equal(naming.ExcludeJobPods()),
		"Job pods carry no readiness probe, so counting them would raise disruptionsAllowed")

	deploy := buildCinderDeployment(cinder, workloadArtifacts(), workloadDigests{})
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(want),
		"the four Deployments of one CR must not adopt each other's pods")
}

// TestCinderDeploymentRolledOut covers the strict convergence gate the upgrade
// flip reads: a surge pod, a lagging status and a replica that is counted but
// not ready each keep it false.
func TestCinderDeploymentRolledOut(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*appsv1.Deployment)
		want   bool
	}{
		{name: "a converged rollout", mutate: func(*appsv1.Deployment) {}, want: true},
		{
			name:   "a status predating the template",
			mutate: func(deploy *appsv1.Deployment) { deploy.Status.ObservedGeneration = 0 },
		},
		{
			name:   "a surge pod still counted",
			mutate: func(deploy *appsv1.Deployment) { deploy.Status.Replicas++ },
		},
		{
			name:   "a replica not ready yet",
			mutate: func(deploy *appsv1.Deployment) { deploy.Status.ReadyReplicas-- },
		},
		{
			name:   "a replica still on the old template",
			mutate: func(deploy *appsv1.Deployment) { deploy.Status.UpdatedReplicas-- },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			deploy := readyCinderDeployment(workloadCinder())
			tc.mutate(deploy)

			g.Expect(cinderDeploymentRolledOut(deploy)).To(Equal(tc.want))
		})
	}

	t.Run("an HPA-owned count converges at one replica", func(t *testing.T) {
		g := NewGomegaWithT(t)
		deploy := readyCinderDeployment(workloadCinder())
		deploy.Spec.Replicas = nil

		g.Expect(desiredReplicas(deploy)).To(Equal(int32(1)))
	})
}
