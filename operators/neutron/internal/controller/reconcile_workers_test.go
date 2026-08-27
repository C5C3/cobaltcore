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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/database"
	"github.com/c5c3/cobaltcore/internal/common/messaging"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// The names the two worker Deployments carry, derived the way the step derives
// them.
const (
	periodicWorkersName = testNeutronName + "-periodic-workers"
	ovnMaintenanceName  = testNeutronName + "-ovn-maintenance-worker"
)

// readyWorkerDeployment returns one worker Deployment with the status of a
// completed rollout.
func readyWorkerDeployment(neutron *neutronv1alpha1.Neutron, component string, command []string) *appsv1.Deployment {
	deploy := buildWorkerDeployment(neutron, component, command, deploymentConfigMapName, "", "", "", "")
	markDeploymentRolledOut(deploy)
	return deploy
}

// readyWorkerDeployments returns both worker Deployments in their ready state,
// which is the only state the WorkersReady condition turns True in.
func readyWorkerDeployments(neutron *neutronv1alpha1.Neutron) []client.Object {
	return []client.Object{
		readyWorkerDeployment(neutron, componentPeriodicWorkers, neutronCommand("neutron-periodic-workers")),
		readyWorkerDeployment(neutron, componentOVNMaintenanceWorker, neutronCommand("neutron-ovn-maintenance-worker")),
	}
}

// TestReconcileWorkers_ProjectsBothWorkloads pins what the two Deployments run
// and what they deliberately lack: no port to dial, no probe to gate on, and no
// autoscaling, because neither process serves requests.
func TestReconcileWorkers_ProjectsBothWorkloads(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	_, err := r.reconcileWorkers(ctx, r.Client, neutron, deploymentConfigMapName, "", "", "", "")
	g.Expect(err).NotTo(HaveOccurred())

	cases := []struct {
		name      string
		component string
		binary    string
	}{
		{periodicWorkersName, componentPeriodicWorkers, "neutron-periodic-workers"},
		{ovnMaintenanceName, componentOVNMaintenanceWorker, "neutron-ovn-maintenance-worker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			var deploy appsv1.Deployment
			g.Expect(r.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: tc.name}, &deploy)).To(Succeed())

			container := deploy.Spec.Template.Spec.Containers[0]
			g.Expect(container.Name).To(Equal(tc.component))
			g.Expect(container.Command).To(Equal([]string{
				tc.binary,
				"--config-file", "/etc/neutron/neutron.conf",
				"--config-file", "/etc/neutron/ml2_conf.ini",
			}))
			g.Expect(container.Ports).To(BeEmpty())
			g.Expect(container.StartupProbe).To(BeNil())
			g.Expect(container.LivenessProbe).To(BeNil())
			g.Expect(container.ReadinessProbe).To(BeNil())

			var envNames []string
			for _, env := range container.Env {
				envNames = append(envNames, env.Name)
			}
			g.Expect(envNames).To(Equal([]string{database.ConnectionEnvVarName, messaging.TransportURLEnvVarName}),
				"a worker authenticates to no Keystone, so it carries no service-user password")

			g.Expect(deploy.Spec.Selector.MatchLabels).To(Equal(map[string]string{
				"app.kubernetes.io/name":      "neutron",
				"app.kubernetes.io/instance":  testNeutronName,
				"app.kubernetes.io/component": tc.component,
			}), "each Deployment selects its own component so none adopts another's pods")
			g.Expect(deploy.Spec.Replicas).NotTo(BeNil(),
				"no HPA owns the worker replica count, so the operator writes it")

			// Neither worker is dialled, protected or scaled.
			key := client.ObjectKey{Namespace: testNamespace, Name: tc.name}
			g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &corev1.Service{}))).To(BeTrue())
			g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &policyv1.PodDisruptionBudget{}))).To(BeTrue())
			g.Expect(apierrors.IsNotFound(r.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{}))).To(BeTrue())
		})
	}

	// The RPC server is not projected: nothing in this deployment consumes RPC,
	// and the rendered config sets both RPC worker counts to zero to match.
	var deployments appsv1.DeploymentList
	g.Expect(r.List(ctx, &deployments, client.InNamespace(testNamespace))).To(Succeed())
	var names []string
	for _, deploy := range deployments.Items {
		names = append(names, deploy.Name)
	}
	g.Expect(names).To(ConsistOf(periodicWorkersName, ovnMaintenanceName))
	g.Expect(names).NotTo(ContainElement(testNeutronName + "-rpc-server"))
}

// TestReconcileWorkers_MountsTheSharedWorkloadVolumes pins that the workers read
// the same files the API pods do: an option only the API mounts is an option the
// maintenance worker silently ignores.
func TestReconcileWorkers_MountsTheSharedWorkloadVolumes(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	deploy := buildWorkerDeployment(neutron, componentPeriodicWorkers,
		neutronCommand("neutron-periodic-workers"), deploymentConfigMapName, "dsn", "auth", "amqp", "ovn")

	volumes, mounts := neutronWorkloadVolumes(neutron, deploymentConfigMapName)
	g.Expect(deploy.Spec.Template.Spec.Volumes).To(Equal(volumes))
	g.Expect(deploy.Spec.Template.Spec.Containers[0].VolumeMounts).To(Equal(mounts))
	g.Expect(deploy.Spec.Template.Annotations).To(Equal(neutronPodAnnotations("dsn", "auth", "amqp", "ovn")),
		"a rotated credential has to roll the workers too")
}

// TestReconcileWorkers_WaitsForBothDeployments covers the readiness gate: one
// unready worker is enough to hold the condition, because the two run different
// maintenance tasks and neither substitutes for the other.
func TestReconcileWorkers_WaitsForBothDeployments(t *testing.T) {
	ctx := context.Background()

	t.Run("one worker unready holds the condition", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		// Only the periodic workers have rolled out; the maintenance worker has not.
		ready := readyWorkerDeployment(neutron, componentPeriodicWorkers, neutronCommand("neutron-periodic-workers"))
		r := newNeutronTestReconciler(neutron, ready)

		res, err := r.reconcileWorkers(ctx, r.Client, neutron, deploymentConfigMapName, "", "", "", "")

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueDeploymentPolling))
		cond := neutronCondition(neutron, conditionTypeWorkersReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForWorkers))
	})

	t.Run("both ready resolves the condition", func(t *testing.T) {
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		r := newNeutronTestReconciler(append(readyWorkerDeployments(neutron), neutron)...)

		res, err := r.reconcileWorkers(ctx, r.Client, neutron, deploymentConfigMapName, "", "", "", "")

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.IsZero()).To(BeTrue())
		cond := neutronCondition(neutron, conditionTypeWorkersReady)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(conditionReasonWorkersReady))
	})
}

// TestReconcileWorkers_ApplyFailureNamesTheDeployment covers the error path: the
// two Deployments differ only in their name, so an error that did not carry it
// would leave a triage reading the operator source to tell which apply failed.
func TestReconcileWorkers_ApplyFailureNamesTheDeployment(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	boom := errors.New("admission webhook rejected the Deployment")
	r := failingApplyReconciler(boom, "Deployment", ovnMaintenanceName, neutron)

	_, err := r.reconcileWorkers(context.Background(), r.Client, neutron, deploymentConfigMapName, "", "", "", "")

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("ensuring " + ovnMaintenanceName + " Deployment:")))
	g.Expect(neutronCondition(neutron, conditionTypeWorkersReady)).To(BeNil(),
		"a failed apply leaves the condition to the pipeline's error attribution")
}
