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

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/keystoneauth"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// deploymentConfigMapName stands in for what reconcileConfig hands the workload
// steps.
const deploymentConfigMapName = "neutron-config-abc"

// neutronKey addresses an object named after the CR itself.
func neutronKey(neutron *neutronv1alpha1.Neutron) client.ObjectKey {
	return client.ObjectKey{Namespace: neutron.Namespace, Name: neutron.Name}
}

// readyNeutronDeployment returns the API Deployment the step builds, with the
// status of a completed rollout: every replica updated, ready, counted, and the
// Available condition set.
func readyNeutronDeployment(neutron *neutronv1alpha1.Neutron) *appsv1.Deployment {
	deploy := buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "")
	markDeploymentRolledOut(deploy)
	return deploy
}

// markDeploymentRolledOut stamps the status of a Deployment whose rollout has
// fully converged onto the current pod template.
func markDeploymentRolledOut(deploy *appsv1.Deployment) {
	replicas := *deploy.Spec.Replicas
	deploy.Generation = 1
	deploy.Status.ObservedGeneration = 1
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.UpdatedReplicas = replicas
	deploy.Status.Replicas = replicas
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}
}

// failingApplyClient builds a reconciler whose apply of the named workload kind
// and object fails, so the wrapping of the error can be asserted.
func failingApplyReconciler(boom error, kind, name string, objs ...client.Object) *NeutronReconciler {
	c := neutronFakeClientBuilder(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if co, ok := obj.(client.Object); ok &&
					co.GetObjectKind().GroupVersionKind().Kind == kind && co.GetName() == name {
					return boom
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).
		Build()
	return &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(20)}
}

// TestReconcileDeployment_CreatesWorkloadAndWaitsForRollout covers the first
// pass: all three objects are projected, and the condition reports the rollout
// the CR is waiting on rather than an endpoint nothing serves yet.
func TestReconcileDeployment_CreatesWorkloadAndWaitsForRollout(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	res, err := r.reconcileDeployment(context.Background(), r.Client, neutron,
		deploymentConfigMapName, "", "", "", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))

	ctx := context.Background()
	g.Expect(r.Get(ctx, neutronKey(neutron), &appsv1.Deployment{})).To(Succeed())
	g.Expect(r.Get(ctx, neutronKey(neutron), &corev1.Service{})).To(Succeed())
	g.Expect(r.Get(ctx, neutronKey(neutron), &policyv1.PodDisruptionBudget{})).To(Succeed())

	cond := neutronCondition(neutron, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
	g.Expect(neutron.Status.Endpoint).To(BeEmpty(),
		"the endpoint is only advertised once the Deployment is available")
}

// TestReconcileDeployment_ReadyStampsTheEndpoint pins the URL clients read off
// the CR: the cluster-local Service address on the API port.
func TestReconcileDeployment_ReadyStampsTheEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron, readyNeutronDeployment(neutron))

	res, err := r.reconcileDeployment(context.Background(), r.Client, neutron,
		deploymentConfigMapName, "", "", "", "")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond := neutronCondition(neutron, "DeploymentReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
	g.Expect(neutron.Status.Endpoint).To(Equal("http://neutron.openstack.svc.cluster.local:9696"))
	g.Expect(neutron.Status.Endpoint).To(Equal(internalNeutronURL(neutron)))
}

// TestReconcileDeployment_ApplyFailureWrapsTheError covers the error path: the
// step names the object it could not apply, so a pipeline error points at the
// Deployment rather than at the step in general.
func TestReconcileDeployment_ApplyFailureWrapsTheError(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	boom := errors.New("admission webhook rejected the Deployment")
	r := failingApplyReconciler(boom, "Deployment", neutron.Name, neutron)

	_, err := r.reconcileDeployment(context.Background(), r.Client, neutron,
		deploymentConfigMapName, "", "", "", "")

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring Deployment:")))
}

// TestBuildNeutronDeployment_UWSGICommand pins the launch mode: the image ships
// no entry script, so the WSGI application is imported from the module path, and
// the trailing --ini names the uwsgi.ini rendered into the same ConfigMap the
// container mounts.
func TestBuildNeutronDeployment_UWSGICommand(t *testing.T) {
	g := NewGomegaWithT(t)

	command := buildNeutronDeployment(validNeutron(), deploymentConfigMapName, "", "", "", "").
		Spec.Template.Spec.Containers[0].Command

	g.Expect(command[:2]).To(Equal([]string{"uwsgi", "--http"}))
	g.Expect(command[2]).To(Equal(":9696"))
	g.Expect(command).To(ContainElements("--module", "neutron.wsgi.api"))
	g.Expect(command).NotTo(ContainElement("--wsgi-file"))
	g.Expect(command[len(command)-2:]).To(Equal([]string{"--ini", "/etc/neutron/uwsgi.ini"}),
		"the uWSGI ini is the last argument, after the flags the shared builder owns")
}

// TestBuildNeutronDeployment_EnvAndProbes pins the five variables the API
// container runs with and the probe target. The two OS_NEUTRON_* variables are
// how a uWSGI-imported application finds its configuration: there is no argv to
// carry --config-file, so the file list travels in the environment and has to
// name the same two files the workers pass on their command line.
func TestBuildNeutronDeployment_EnvAndProbes(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	container := buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "").
		Spec.Template.Spec.Containers[0]

	var names []string
	for _, env := range container.Env {
		names = append(names, env.Name)
	}
	g.Expect(names).To(Equal([]string{
		"OS_NEUTRON_CONFIG_DIR",
		"OS_NEUTRON_CONFIG_FILES",
		database.ConnectionEnvVarName,
		messaging.TransportURLEnvVarName,
		keystoneauth.PasswordEnvVarName,
	}))
	g.Expect(container.Env[0].Value).To(Equal("/etc/neutron"))
	g.Expect(container.Env[1].Value).To(Equal("neutron.conf;ml2_conf.ini"))
	g.Expect(container.Env[4].ValueFrom.SecretKeyRef.Name).To(Equal("neutron-service-user"))
	g.Expect(container.Env[4].ValueFrom.SecretKeyRef.Key).To(Equal("password"))

	g.Expect(container.Ports).To(HaveLen(1))
	g.Expect(container.Ports[0].ContainerPort).To(Equal(int32(9696)))
	for name, probe := range map[string]*corev1.Probe{
		"startup":   container.StartupProbe,
		"liveness":  container.LivenessProbe,
		"readiness": container.ReadinessProbe,
	} {
		g.Expect(probe).NotTo(BeNil(), name+" probe")
		g.Expect(probe.HTTPGet.Path).To(Equal("/"), name+" probe path")
		g.Expect(probe.HTTPGet.Port.IntValue()).To(Equal(9696), name+" probe port")
	}
}

// TestNeutronPodAnnotations covers the rollout trigger: every digest is stamped
// only when it carries content, because the upstream requeue and error paths
// return an empty one and an annotation flipping to empty would roll the pods
// for nothing.
func TestNeutronPodAnnotations(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(neutronPodAnnotations("", "", "", "")).To(BeNil(),
		"a pass that resolved no digest at all leaves the template annotation-free")

	g.Expect(neutronPodAnnotations("dsn", "", "", "")).To(Equal(map[string]string{
		"neutron.c5c3.io/db-connection-hash": "dsn",
	}), "a partially resolved pass stamps only what it resolved")

	g.Expect(neutronPodAnnotations("dsn", "auth", "amqp", "ovn")).To(Equal(map[string]string{
		"neutron.c5c3.io/db-connection-hash": "dsn",
		"neutron.c5c3.io/authtoken-hash":     "auth",
		"neutron.c5c3.io/transport-url-hash": "amqp",
		"neutron.c5c3.io/ovn-client-hash":    "ovn",
	}))
}

// TestNeutronWorkloadVolumes covers the mount layout every workload shares: the
// three unconditional volumes, and the two that exist only while their spec
// block does. A rendered config naming a file no volume carries fails the
// process on first use, so the two have to move together.
func TestNeutronWorkloadVolumes(t *testing.T) {
	volumeNames := func(volumes []corev1.Volume) []string {
		var out []string
		for _, vol := range volumes {
			out = append(out, vol.Name)
		}
		return out
	}

	t.Run("without TLS", func(t *testing.T) {
		g := NewGomegaWithT(t)
		volumes, mounts := neutronWorkloadVolumes(validNeutron(), deploymentConfigMapName)

		g.Expect(volumeNames(volumes)).To(Equal([]string{"config", "ovn-tls", "state"}))
		g.Expect(mounts).To(Equal([]corev1.VolumeMount{
			{Name: "config", MountPath: "/etc/neutron", ReadOnly: true},
			{Name: "ovn-tls", MountPath: "/etc/ovn/tls", ReadOnly: true},
			{Name: "state", MountPath: "/var/lib/neutron"},
		}))
		g.Expect(volumes[0].ConfigMap.Name).To(Equal(deploymentConfigMapName))
		g.Expect(volumes[1].Secret.SecretName).To(Equal("neutron-ovn-client"))
		g.Expect(volumes[2].EmptyDir).NotTo(BeNil(),
			"state_path must be writable under a read-only root filesystem")
	})

	t.Run("with the broker CA", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		neutron.Spec.Messaging.TLS = &commonv1.MessagingTLSSpec{
			CABundleSecretRef: commonv1.SecretRefSpec{Name: "rabbitmq-ca", Key: "bundle.pem"},
		}
		volumes, mounts := neutronWorkloadVolumes(neutron, deploymentConfigMapName)

		g.Expect(volumeNames(volumes)).To(ContainElement("rabbitmq-ca"))
		g.Expect(mounts).To(ContainElement(corev1.VolumeMount{
			Name: "rabbitmq-ca", MountPath: "/etc/rabbitmq-ca", ReadOnly: true,
		}))
		ca := volumes[len(volumes)-1]
		g.Expect(ca.Secret.SecretName).To(Equal("rabbitmq-ca"))
		g.Expect(ca.Secret.Items).To(Equal([]corev1.KeyToPath{{Key: "bundle.pem", Path: "ca.crt"}}),
			"whatever key the CR names is projected as the file ssl_ca_file points at")
	})

	t.Run("with database TLS", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		neutron.Spec.Database.TLS = &commonv1.DatabaseTLSSpec{
			Mode:                "verify-full",
			CABundleSecretRef:   commonv1.SecretRefSpec{Name: "neutron-db-ca"},
			ClientCertSecretRef: commonv1.SecretRefSpec{Name: "neutron-db-client"},
		}
		volumes, mounts := neutronWorkloadVolumes(neutron, deploymentConfigMapName)

		tlsVol, tlsMount := neutronDBTLSVolumeAndMount(neutron)
		g.Expect(volumes).To(ContainElement(tlsVol))
		g.Expect(mounts).To(ContainElement(tlsMount))
	})
}

// TestBuildNeutronService_And_PDB_SelectTheAPIComponent pins the selector
// deviation: one Neutron owns three Deployments, so a selector without the
// component key would route API traffic to a worker pod that serves no HTTP and
// would let the API budget count worker pods as healthy.
func TestBuildNeutronService_And_PDB_SelectTheAPIComponent(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	want := map[string]string{
		"app.kubernetes.io/name":      "neutron",
		"app.kubernetes.io/instance":  "neutron",
		"app.kubernetes.io/component": "api",
	}

	svc := buildNeutronService(neutron)
	g.Expect(svc.Spec.Selector).To(Equal(want))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9696)))
	g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(9696))

	pdb := buildPodDisruptionBudget(neutron)
	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(want))
	g.Expect(pdb.Spec.Selector.MatchExpressions).To(Equal(naming.ExcludeJobPods()),
		"Job pods carry no readiness probe, so counting them would raise disruptionsAllowed")

	deploy := buildNeutronDeployment(neutron, deploymentConfigMapName, "", "", "", "")
	g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(want),
		"the three Deployments of one CR must not select each other's pods")
}

// TestReconcileDeployment_RollingUpdateHoldsUntilTheImageIsDrained covers the
// upgrade gate. The surge-tolerant readiness turns true while old-image pods
// still serve, and the contract phase drops what those pods still read, so the
// flip to Contracting has to wait for the rollout to converge.
func TestReconcileDeployment_RollingUpdateHoldsUntilTheImageIsDrained(t *testing.T) {
	ctx := context.Background()
	upgrading := func() *neutronv1alpha1.Neutron {
		neutron := validNeutron()
		neutron.Status.InstalledRelease = "2025.2"
		neutron.Status.TargetRelease = "2026.1"
		neutron.Status.UpgradePhase = commonv1.UpgradePhaseRollingUpdate
		return neutron
	}

	t.Run("a surge pod still running holds the phase", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := upgrading()
		// Ready by the surge-tolerant measure, not converged: one old-image pod is
		// still counted and not yet updated.
		surging := readyNeutronDeployment(neutron)
		surging.Status.Replicas++
		r := newNeutronTestReconciler(neutron, surging)

		res, err := r.reconcileDeployment(ctx, r.Client, neutron, deploymentConfigMapName, "", "", "", "")

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		g.Expect(neutron.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseRollingUpdate))
		cond := neutronCondition(neutron, "DeploymentReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
		g.Expect(neutron.Status.Endpoint).To(BeEmpty())
	})

	t.Run("a drained rollout advances to contracting", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := upgrading()
		r := newNeutronTestReconciler(neutron, readyNeutronDeployment(neutron))

		res, err := r.reconcileDeployment(ctx, r.Client, neutron, deploymentConfigMapName, "", "", "", "")

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueNextPass))
		g.Expect(neutron.Status.UpgradePhase).To(Equal(commonv1.UpgradePhaseContracting))
		cond := neutronCondition(neutron, "DeploymentReady")
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(neutron.Status.Endpoint).To(BeEmpty(),
			"the endpoint is not stamped on the phase-flip pass")
		recorder, ok := r.Recorder.(*record.FakeRecorder)
		g.Expect(ok).To(BeTrue())
		g.Expect(collectEvents(recorder)).To(ContainElement(ContainSubstring("DeploymentRolloutComplete")))
	})
}
