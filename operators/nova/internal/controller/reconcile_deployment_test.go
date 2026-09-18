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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// workloadArtifacts stands in for what reconcileConfig hands the workload steps:
// a rendered config ConfigMap carrying the shared document, the four role
// overlays and the logging file the json fixture produces, sorted the way the
// config step sorts them.
func workloadArtifacts() configArtifacts {
	return configArtifacts{
		configMapName: "nova-config-abc123",
		dataKeys: []string{
			conductorConfDataKey, loggingINIDataKey, metadataConfDataKey,
			novaConfDataKey, novncproxyConfDataKey, schedulerConfDataKey,
		},
	}
}

// textLoggingArtifacts are the artefacts of a render that produced no
// logging.ini, which is what spec.logging.format=text renders.
func textLoggingArtifacts() configArtifacts {
	art := workloadArtifacts()
	art.dataKeys = []string{
		conductorConfDataKey, metadataConfDataKey, novaConfDataKey,
		novncproxyConfDataKey, schedulerConfDataKey,
	}
	return art
}

// workloadTestDigests are the five content digests the credential steps return.
func workloadTestDigests() workloadDigests {
	return workloadDigests{
		apiDSN:         "apidsn1",
		cellDSN:        "celldsn2",
		authToken:      "auth3",
		transport:      "bus4",
		metadataSecret: "meta5",
	}
}

// testEgressPort is the broker port the messaging step resolves, threaded into
// the readiness probe of the two bus processes.
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

// readyAPIDeployment returns the API Deployment the step builds, with the status
// of a completed rollout.
func readyAPIDeployment(nova *novav1alpha1.Nova) *appsv1.Deployment {
	return markDeploymentRolledOut(buildAPIDeployment(nova, workloadArtifacts(), workloadDigests{}))
}

// readyRoleDeployments returns the four non-API Deployments with the status of a
// completed rollout, which is what the API step's upgrade gate reads back off
// the cluster.
func readyRoleDeployments(nova *novav1alpha1.Nova) []client.Object {
	art := workloadArtifacts()
	return []client.Object{
		markDeploymentRolledOut(buildMetadataDeployment(nova, art, workloadDigests{})),
		markDeploymentRolledOut(buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort)),
		markDeploymentRolledOut(buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort)),
		markDeploymentRolledOut(buildConsoleProxyDeployment(nova, art, workloadDigests{})),
	}
}

// onTheOldImage puts a Deployment back on the release the upgrade fixture moves
// away from, which is what a role whose own step has not applied the new
// template yet looks like.
func onTheOldImage(deploy *appsv1.Deployment) {
	deploy.Spec.Template.Spec.Containers[0].Image = "ghcr.io/c5c3/nova:2025.2"
}

// failingApplyReconciler builds a reconciler whose apply of the named kind and
// object fails, so the wrapping of the error can be asserted.
func failingApplyReconciler(boom error, kind, name string, objs ...client.Object) *NovaReconciler {
	c := novaFakeClientBuilder(objs...).
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
	return &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(20)}
}

// tlsNova returns the fixture with every transport verified: both schemas carry
// client TLS material and the bus carries the broker's CA bundle, which is the
// shape that projects all three optional volumes.
func tlsNova() *novav1alpha1.Nova {
	nova := novaWithMessagingTLS()
	for _, db := range []*commonv1.DatabaseSpec{&nova.Spec.APIDatabase, &nova.Spec.Database} {
		db.TLS = &commonv1.DatabaseTLSSpec{
			Mode:                "verify-full",
			CABundleSecretRef:   commonv1.SecretRefSpec{Name: "db-ca"},
			ClientCertSecretRef: commonv1.SecretRefSpec{Name: "db-client"},
		}
	}
	return nova
}

// rollingUpdateNova returns the fixture mid-upgrade, parked in the RollingUpdate
// phase every workload step gates on.
func rollingUpdateNova() *novav1alpha1.Nova {
	return upgradingNova(commonv1.UpgradePhaseRollingUpdate)
}

// TestTopologySpreadSelectorsMatchTheDeployments feeds the pod selector of every
// Deployment the operator builds back into the validating webhook as a
// topologySpreadConstraints selector. The webhook demands exact equality with
// the block's selector labels, so one it rejects would leave the field unusable:
// the only accepted value would be a selector no Deployment carries.
func TestTopologySpreadSelectorsMatchTheDeployments(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	art := workloadArtifacts()

	obj := nova.DeepCopy()
	obj.Spec.API.Deployment.TopologySpreadConstraints = spreadOver(
		buildAPIDeployment(nova, art, workloadDigests{}).Spec.Selector.MatchLabels)
	obj.Spec.Metadata.Deployment.TopologySpreadConstraints = spreadOver(
		buildMetadataDeployment(nova, art, workloadDigests{}).Spec.Selector.MatchLabels)
	obj.Spec.Scheduler.Deployment.TopologySpreadConstraints = spreadOver(
		buildSchedulerDeployment(nova, art, workloadDigests{}, testEgressPort).Spec.Selector.MatchLabels)
	obj.Spec.Conductor.Deployment.TopologySpreadConstraints = spreadOver(
		buildConductorDeployment(nova, art, workloadDigests{}, testEgressPort).Spec.Selector.MatchLabels)
	obj.Spec.ConsoleProxy.Deployment.TopologySpreadConstraints = spreadOver(
		buildConsoleProxyDeployment(nova, art, workloadDigests{}).Spec.Selector.MatchLabels)

	_, err := (&novav1alpha1.NovaWebhook{}).ValidateCreate(context.Background(), obj)
	g.Expect(err).NotTo(HaveOccurred(),
		"the webhook must accept the selector of the Deployment each block configures")

	// The pair every Nova pod shares is not a selector any single Deployment
	// carries, so the webhook has to reject it: a constraint on it would measure
	// the scheduler pods against the API's.
	rejected := obj.DeepCopy()
	rejected.Spec.Scheduler.Deployment.TopologySpreadConstraints = spreadOver(
		naming.SelectorLabels(novaAppName, nova.Name))

	_, err = (&novav1alpha1.NovaWebhook{}).ValidateCreate(context.Background(), rejected)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.scheduler.deployment.topologySpreadConstraints"))
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
	nova := validNova()
	r := newNovaTestReconciler(nova)

	res, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadTestDigests())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
	g.Expect(r.Get(ctx, objectKey(nova.Name), &appsv1.Deployment{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey(nova.Name), &corev1.Service{})).To(Succeed())
	g.Expect(r.Get(ctx, objectKey(nova.Name), &policyv1.PodDisruptionBudget{})).To(Succeed())

	cond := novaCondition(nova, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
	g.Expect(nova.Status.Endpoint).To(BeEmpty(),
		"the endpoint is only advertised once the Deployment is available")
}

// TestReconcileDeployment_ReadyStampsTheEndpoint pins the URL clients read off
// the CR: the gateway hostname while the API is exposed through one, and the
// cluster-local Service address otherwise.
func TestReconcileDeployment_ReadyStampsTheEndpoint(t *testing.T) {
	ctx := context.Background()

	t.Run("behind a gateway", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		r := newNovaTestReconciler(nova, readyAPIDeployment(nova))

		res, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := novaCondition(nova, "DeploymentReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
		g.Expect(nova.Status.Endpoint).To(Equal("https://nova.example.com/"))
	})

	t.Run("without one", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		nova.Spec.Gateway = nil
		r := newNovaTestReconciler(nova, readyAPIDeployment(nova))

		_, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(nova.Status.Endpoint).To(Equal("http://nova.openstack.svc.cluster.local:8774"))
		g.Expect(nova.Status.Endpoint).To(Equal(internalNovaURL(nova)))
	})
}

// TestReconcileDeployment_RollingUpdateHoldsUntilEveryRoleDrained covers the
// upgrade gate. The contract phase runs data migrations the old image has no
// code for, and nova spreads that code over five processes, so the flip to
// Contracting waits for every role rather than for the API alone.
func TestReconcileDeployment_RollingUpdateHoldsUntilEveryRoleDrained(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*appsv1.Deployment)
	}{
		{
			name: "a conductor replica still on the old template",
			mutate: func(deploy *appsv1.Deployment) {
				deploy.Status.UpdatedReplicas--
			},
		},
		{name: "a conductor still running the old image", mutate: onTheOldImage},
	}
	for _, tc := range cases {
		t.Run(tc.name+" holds the phase", func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := rollingUpdateNova()
			roles := readyRoleDeployments(nova)
			conductor, ok := roles[2].(*appsv1.Deployment)
			g.Expect(ok).To(BeTrue())
			tc.mutate(conductor)
			r := newNovaTestReconciler(append(roles, nova, readyAPIDeployment(nova))...)

			res, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
			g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
			cond := novaCondition(nova, "DeploymentReady")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
			g.Expect(cond.Message).To(ContainSubstring("every role"))
			g.Expect(nova.Status.Endpoint).To(BeEmpty())
		})
	}

	t.Run("a surge pod on the API holds the phase", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		surging := readyAPIDeployment(nova)
		surging.Status.Replicas++
		r := newNovaTestReconciler(append(readyRoleDeployments(nova), nova, surging)...)

		res, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
	})

	t.Run("every role drained advances to contracting", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		r := newNovaTestReconciler(append(readyRoleDeployments(nova), nova, readyAPIDeployment(nova))...)

		res, err := r.reconcileDeployment(ctx, r.Client, nova, workloadArtifacts(), workloadDigests{})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
		g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseContracting))
		g.Expect(novaCondition(nova, "DeploymentReady").Status).To(Equal(metav1.ConditionTrue))
		g.Expect(nova.Status.Endpoint).To(BeEmpty(),
			"the endpoint is not stamped on the phase-flip pass")
		recorder, ok := r.Recorder.(*record.FakeRecorder)
		g.Expect(ok).To(BeTrue())
		g.Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("DeploymentRolloutComplete")))
	})
}

// TestReconcileDeployment_RollingUpdateIgnoresTheDisabledProxy covers the gate's
// role list: a Nova without a console proxy has no novncproxy Deployment, and
// waiting for one that is never projected would park the upgrade in the
// RollingUpdate phase forever.
func TestReconcileDeployment_RollingUpdateIgnoresTheDisabledProxy(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := rollingUpdateNova()
	nova.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
	// The first three roles only: the proxy is switched off, so its step deleted
	// the Deployment the gate would otherwise look for.
	roles := readyRoleDeployments(nova)[:3]
	r := newNovaTestReconciler(append(roles, nova, readyAPIDeployment(nova))...)

	res, err := r.reconcileDeployment(context.Background(), r.Client, nova,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
	g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseContracting))
}

// TestReconcileDeployment_ApplyFailureWrapsTheError covers the error path: the
// step names the object it could not apply, so a pipeline error points at the
// Deployment, the Service or the budget rather than at the step in general. The
// three share one name, so only the kind in the message tells them apart.
func TestReconcileDeployment_ApplyFailureWrapsTheError(t *testing.T) {
	cases := []struct {
		kind string
		wrap string
	}{
		{kind: "Deployment", wrap: "ensuring Deployment:"},
		{kind: "Service", wrap: "ensuring Service:"},
		{kind: "PodDisruptionBudget", wrap: "ensuring PodDisruptionBudget:"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := validNova()
			boom := errors.New("admission webhook rejected the " + tc.kind)
			r := failingApplyReconciler(boom, tc.kind, nova.Name, nova)

			_, err := r.reconcileDeployment(context.Background(), r.Client, nova,
				workloadArtifacts(), workloadDigests{})

			g.Expect(err).To(MatchError(boom))
			g.Expect(err).To(MatchError(ContainSubstring(tc.wrap)))
			g.Expect(novaCondition(nova, "DeploymentReady")).To(BeNil(),
				"a failed apply reports no rollout the step never started")
		})
	}
}

// TestReconcileDeployment_RolloutGateReadFailureHoldsThePhase covers the read the
// upgrade gate makes of the other roles. A failed Get is not an absent
// Deployment: it must surface as an error naming the role, and it must not flip
// the phase to Contracting, which would run the contract migrations against
// processes nobody saw restart.
func TestReconcileDeployment_RolloutGateReadFailureHoldsThePhase(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := rollingUpdateNova()
	boom := errors.New("etcd unavailable")
	conductorKey := objectKey(conductorName(nova))
	c := novaFakeClientBuilder(append(readyRoleDeployments(nova), nova, readyAPIDeployment(nova))...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, ok := obj.(*appsv1.Deployment); ok && key == conductorKey {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &NovaReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(20)}

	_, err := r.reconcileDeployment(context.Background(), r.Client, nova,
		workloadArtifacts(), workloadDigests{})

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring(
		"fetching Deployment openstack/nova-conductor for the rollout gate:")))
	g.Expect(nova.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
}

// TestNovaWorkloadVolumes_PerRoleItems covers the file layout each role sees.
// One ConfigMap carries the documents of all five, and oslo.config reads every
// file in a --config-dir, so the projection is what keeps a scheduler from
// taking over the console proxy's listen address.
func TestNovaWorkloadVolumes_PerRoleItems(t *testing.T) {
	cases := []struct {
		role       workloadRole
		configKeys []string
		overlayKey string
		overlayDir string
	}{
		{role: roleAPI, configKeys: []string{loggingINIDataKey, novaConfDataKey}},
		{
			role:       roleMetadata,
			configKeys: []string{loggingINIDataKey, metadataConfDataKey, novaConfDataKey},
		},
		{
			role:       roleScheduler,
			configKeys: []string{loggingINIDataKey, novaConfDataKey},
			overlayKey: schedulerConfDataKey,
			overlayDir: "/etc/nova/scheduler.conf.d",
		},
		{
			role:       roleConductor,
			configKeys: []string{loggingINIDataKey, novaConfDataKey},
			overlayKey: conductorConfDataKey,
			overlayDir: "/etc/nova/conductor.conf.d",
		},
		{
			role:       roleConsoleProxy,
			configKeys: []string{loggingINIDataKey, novaConfDataKey},
			overlayKey: novncproxyConfDataKey,
			overlayDir: "/etc/nova/novncproxy.conf.d",
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			g := NewGomegaWithT(t)
			volumes, mounts := novaWorkloadVolumes(validNova(), workloadArtifacts(), tc.role)

			config := volumes[0]
			g.Expect(config.Name).To(Equal(configVolumeName))
			g.Expect(config.ConfigMap.Name).To(Equal("nova-config-abc123"))
			g.Expect(keyPaths(config.ConfigMap.Items)).To(Equal(tc.configKeys))
			g.Expect(mounts[0]).To(Equal(corev1.VolumeMount{
				Name: configVolumeName, MountPath: novaConfigDir, ReadOnly: true,
			}))
			if tc.role != roleMetadata {
				g.Expect(keyPaths(config.ConfigMap.Items)).NotTo(ContainElement(metadataConfDataKey),
					"the shared secret's front end is the only reader of the metadata overlay")
			}

			names := make([]string, 0, len(volumes))
			for _, volume := range volumes {
				names = append(names, volume.Name)
			}
			if tc.overlayKey == "" {
				g.Expect(names).To(Equal([]string{configVolumeName, stateVolumeName, tmpVolumeName}))
				return
			}

			overlay := volumes[1]
			g.Expect(overlay.Name).To(Equal(string(tc.role) + "-overlay"))
			g.Expect(overlay.ConfigMap.Name).To(Equal("nova-config-abc123"))
			g.Expect(overlay.ConfigMap.Items).To(Equal([]corev1.KeyToPath{
				{Key: tc.overlayKey, Path: tc.overlayKey},
			}), "the overlay directory holds one role's options and nothing else")
			g.Expect(mounts[1]).To(Equal(corev1.VolumeMount{
				Name: overlay.Name, MountPath: tc.overlayDir, ReadOnly: true,
			}))
		})
	}

	t.Run("a render without logging.ini projects none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		volumes, _ := novaWorkloadVolumes(validNova(), textLoggingArtifacts(), roleAPI)

		g.Expect(keyPaths(volumes[0].ConfigMap.Items)).To(Equal([]string{novaConfDataKey}))
	})

	t.Run("the scratch directories are shared by every role", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, mounts := novaWorkloadVolumes(validNova(), workloadArtifacts(), roleAPI)

		g.Expect(mounts[1]).To(Equal(corev1.VolumeMount{
			Name: stateVolumeName, MountPath: novaStatePath,
		}))
		g.Expect(mounts[2]).To(Equal(corev1.VolumeMount{
			Name: tmpVolumeName, MountPath: tmpMountPath,
		}))
	})
}

// keyPaths returns the keys of a ConfigMap projection, in projection order.
func keyPaths(items []corev1.KeyToPath) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}

// TestNovaWorkloadVolumes_TLSProjections covers the three optional mounts: each
// exists only while its spec block does, and the broker's CA bundle is renamed
// to the file name the rendered ssl_ca_file option points at.
func TestNovaWorkloadVolumes_TLSProjections(t *testing.T) {
	g := NewGomegaWithT(t)
	plain, plainMounts := novaWorkloadVolumes(validNova(), workloadArtifacts(), roleAPI)
	for _, volume := range plain {
		g.Expect(volume.Name).NotTo(BeElementOf(apiDBTLSVolumeName, cellDBTLSVolumeName, rabbitmqCAVolumeName),
			"no projection exists while its spec block does not")
	}
	g.Expect(plainMounts).To(HaveLen(3))

	volumes, mounts := novaWorkloadVolumes(tlsNova(), workloadArtifacts(), roleAPI)
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	g.Expect(names).To(Equal([]string{
		configVolumeName, stateVolumeName, tmpVolumeName,
		apiDBTLSVolumeName, cellDBTLSVolumeName, rabbitmqCAVolumeName,
	}), "a process reads both schemas, so both keypairs are projected")

	ca := volumes[len(volumes)-1]
	g.Expect(ca.Secret.SecretName).To(Equal(testMessagingCASecret))
	g.Expect(ca.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}))
	g.Expect(mounts[len(mounts)-1]).To(Equal(corev1.VolumeMount{
		Name: rabbitmqCAVolumeName, MountPath: rabbitmqCAMountPath, ReadOnly: true,
	}))
	g.Expect(rabbitmqCAFilePath).To(Equal(rabbitmqCAMountPath + "/ca.crt"))

	// The fixture stores the bundle under ca.crt already, so only a different key
	// shows the projection renaming it: a bundle mounted under its own key would
	// leave ssl_ca_file pointing at a file that does not exist.
	t.Run("the CA bundle is renamed to the configured file", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := novaWithMessagingTLS()
		nova.Spec.Messaging.TLS.CABundleSecretRef.Key = "bundle.pem"

		volumes, _ := novaWorkloadVolumes(nova, workloadArtifacts(), roleAPI)
		ca := volumes[len(volumes)-1]
		g.Expect(ca.Name).To(Equal(rabbitmqCAVolumeName))
		g.Expect(ca.Secret.SecretName).To(Equal(testMessagingCASecret))
		g.Expect(ca.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "bundle.pem", Path: "ca.crt"}}))
	})
}

// TestNovaPodAnnotations covers the rollout triggers: a digest the upstream step
// could not compute leaves its annotation off rather than stamping an empty
// value, which would roll every pod the moment the credential appears.
func TestNovaPodAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(novaPodAnnotations(workloadDigests{})).To(BeNil(),
		"a template with no digest at all carries no annotations")
	g.Expect(novaPodAnnotations(workloadDigests{cellDSN: "celldsn2"})).To(Equal(map[string]string{
		dbConnectionHashAnnotation: "celldsn2",
	}))
	g.Expect(novaPodAnnotations(workloadTestDigests())).To(Equal(map[string]string{
		apiDBConnectionHashAnnotation: "apidsn1",
		dbConnectionHashAnnotation:    "celldsn2",
		authTokenHashAnnotation:       "auth3",
		transportURLHashAnnotation:    "bus4",
	}), "the metadata secret is not read by the API")

	rotated := workloadTestDigests()
	rotated.apiDSN = "apidsn9"
	g.Expect(novaPodAnnotations(rotated)).NotTo(Equal(novaPodAnnotations(workloadTestDigests())),
		"a rotated credential must change the template")
	g.Expect(novaPodAnnotations(workloadTestDigests())).To(Equal(novaPodAnnotations(workloadTestDigests())),
		"an unchanged digest set must render the same template twice")
}

// TestNovaRPCPodAnnotations covers the extra stamps the non-API processes carry:
// the installed release for the four that cache their peers' RPC versions, and
// the metadata secret for the one front end that reads it.
func TestNovaRPCPodAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	g.Expect(novaRPCPodAnnotations(nova, workloadDigests{})).To(Equal(map[string]string{
		installedReleaseAnnotation: "2025.2",
	}), "a fresh install stamps spec.openStackRelease rather than an empty value")

	nova.Status.InstalledRelease = "2024.2"
	g.Expect(novaRPCPodAnnotations(nova, workloadDigests{cellDSN: "celldsn2"})).To(Equal(map[string]string{
		dbConnectionHashAnnotation: "celldsn2",
		installedReleaseAnnotation: "2024.2",
	}), "once the schema is installed, the marker is what the pods run against")

	g.Expect(novaRPCPodAnnotations(nova, workloadTestDigests())).NotTo(
		HaveKey(metadataSecretHashAnnotation),
		"only the metadata API reads the shared secret")
	g.Expect(novaMetadataPodAnnotations(nova, workloadTestDigests())).To(
		HaveKeyWithValue(metadataSecretHashAnnotation, "meta5"))
	g.Expect(novaMetadataPodAnnotations(nova, workloadDigests{})).NotTo(
		HaveKey(metadataSecretHashAnnotation),
		"a secret digest the credential step could not compute stamps nothing")
}

// TestPodTemplateAnnotationsPerRole covers which template carries the release
// stamp: the four processes that cache their peers' RPC versions do, and the API
// does not, because it rolled during the RollingUpdate phase already.
func TestPodTemplateAnnotationsPerRole(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	art := workloadArtifacts()
	digests := workloadTestDigests()

	api := buildAPIDeployment(nova, art, digests)
	g.Expect(api.Spec.Template.Annotations).NotTo(HaveKey(installedReleaseAnnotation))

	for name, deploy := range map[string]*appsv1.Deployment{
		"metadata":   buildMetadataDeployment(nova, art, digests),
		"scheduler":  buildSchedulerDeployment(nova, art, digests, testEgressPort),
		"conductor":  buildConductorDeployment(nova, art, digests, testEgressPort),
		"novncproxy": buildConsoleProxyDeployment(nova, art, digests),
	} {
		g.Expect(deploy.Spec.Template.Annotations).To(
			HaveKeyWithValue(installedReleaseAnnotation, "2025.2"), name)
	}
}

// TestUWSGIFrontEndsCarryAStartupProbe covers the cold start of the two HTTP
// front ends. Importing nova took 77 seconds on a CI node, and the liveness probe
// alone restarts the container 55 seconds after it started, so without a startup
// probe holding it back the API never finishes loading.
func TestUWSGIFrontEndsCarryAStartupProbe(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	art := workloadArtifacts()
	digests := workloadTestDigests()

	for name, tc := range map[string]struct {
		deploy *appsv1.Deployment
		port   int
	}{
		"api":      {buildAPIDeployment(nova, art, digests), 8774},
		"metadata": {buildMetadataDeployment(nova, art, digests), 8775},
	} {
		container := tc.deploy.Spec.Template.Spec.Containers[0]
		probe := container.StartupProbe
		g.Expect(probe).NotTo(BeNil(), name+" startup probe")
		g.Expect(probe.HTTPGet.Path).To(Equal("/"), name+" startup probe path")
		g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(tc.port), name+" startup probe port")
		g.Expect(probe.FailureThreshold*probe.PeriodSeconds).To(
			BeNumerically(">=", 300), name+" startup budget in seconds")
		g.Expect(probe.TimeoutSeconds).To(BeNumerically(">", 1),
			name+": a loading WSGI app holds a GET past the kubelet's 1s default")
	}
}

// TestBuildAPIService_And_PDB covers the selectors: one Nova owns five kinds of
// Deployment, so the API Service and its budget must reach the API pods and
// nothing else.
func TestBuildAPIService_And_PDB(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()
	want := map[string]string{
		"app.kubernetes.io/name":      "nova",
		"app.kubernetes.io/instance":  "nova",
		"app.kubernetes.io/component": "api",
	}
	g.Expect(want).To(Equal(naming.APISelectorLabels("nova", nova.Name)))

	svc := buildAPIService(nova)
	g.Expect(svc.Spec.Selector).To(Equal(want))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(novaAPIPort))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(int(novaAPIPort)))

	pdb := buildPodDisruptionBudget(nova)
	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(want))
	g.Expect(pdb.Spec.Selector.MatchExpressions).To(Equal(naming.ExcludeJobPods()),
		"Job pods carry no readiness probe, so counting them would raise disruptionsAllowed")

	deploy := buildAPIDeployment(nova, workloadArtifacts(), workloadDigests{})
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(want),
		"the five Deployments of one CR must not adopt each other's pods")
}

// TestNovaDeploymentRolledOut covers the strict convergence gate the upgrade
// flip reads: a surge pod, a lagging status and a replica that is counted but
// not ready each keep it false.
func TestNovaDeploymentRolledOut(t *testing.T) {
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
			deploy := readyAPIDeployment(validNova())
			tc.mutate(deploy)

			g.Expect(novaDeploymentRolledOut(deploy)).To(Equal(tc.want))
		})
	}

	t.Run("an HPA-owned count converges at one replica", func(t *testing.T) {
		g := NewGomegaWithT(t)
		deploy := readyAPIDeployment(validNova())
		deploy.Spec.Replicas = nil

		g.Expect(desiredReplicas(deploy)).To(Equal(int32(1)))
	})
}

// TestNovaDeploymentsRolledOut covers the gate over the four non-API roles: an
// absent Deployment, one still on the old image and one whose rollout has not
// converged each keep the contract phase waiting.
func TestNovaDeploymentsRolledOut(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func([]client.Object) []client.Object
		want   bool
	}{
		{
			name:   "every role converged",
			mutate: func(objs []client.Object) []client.Object { return objs },
			want:   true,
		},
		{
			name:   "the metadata Deployment not projected yet",
			mutate: func(objs []client.Object) []client.Object { return objs[1:] },
		},
		{
			name: "a scheduler still on the old image",
			mutate: func(objs []client.Object) []client.Object {
				onTheOldImage(objs[1].(*appsv1.Deployment))
				return objs
			},
		},
		{
			name: "a conductor rollout that has not converged",
			mutate: func(objs []client.Object) []client.Object {
				objs[2].(*appsv1.Deployment).Status.ReadyReplicas = 0
				return objs
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			nova := rollingUpdateNova()
			r := newNovaTestReconciler(append(tc.mutate(readyRoleDeployments(nova)), nova)...)

			drained, err := novaDeploymentsRolledOut(ctx, r.Client, nova)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(drained).To(Equal(tc.want))
		})
	}

	t.Run("a disabled proxy is not waited for", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := rollingUpdateNova()
		nova.Spec.ConsoleProxy = novav1alpha1.NovaConsoleProxySpec{Enabled: ptr.To(false)}
		r := newNovaTestReconciler(append(readyRoleDeployments(nova)[:3], nova)...)

		drained, err := novaDeploymentsRolledOut(ctx, r.Client, nova)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(drained).To(BeTrue())
	})
}

// TestNovaStatusEndpoint pins the two shapes of the advertised endpoint against
// the cluster-local URL the health check dials.
func TestNovaStatusEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := validNova()

	g.Expect(novaStatusEndpoint(nova)).To(Equal("https://nova.example.com/"))

	nova.Spec.Gateway = nil
	g.Expect(novaStatusEndpoint(nova)).To(Equal("http://nova.openstack.svc.cluster.local:8774"))
}
