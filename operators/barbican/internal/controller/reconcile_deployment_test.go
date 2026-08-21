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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
)

// deploymentConfigSecretName is the rendered config Secret the deployment step
// mounts, standing in for what reconcileConfig hands it.
const deploymentConfigSecretName = "test-barbican-config-abc"

// projectionWithCA is the shared validProjection plus the trust bundle a managed
// store derives from its OpenBaoCluster.
func projectionWithCA() secretStoreProjection {
	projection := validProjection()
	projection.caSecretName = testInstanceName + instanceCASecretSuffix
	return projection
}

// readyBarbicanDeployment returns the desired Deployment with a fully converged,
// available status, so a seeded fixture drives the ready arm of the step.
func readyBarbicanDeployment(barbican *barbicanv1alpha1.Barbican) *appsv1.Deployment {
	deploy := buildBarbicanDeployment(barbican, validProjection(), deploymentConfigSecretName, "", "")
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

// narrowedServiceReader is a client.Reader that answers every Get with a Service
// already carrying the narrowed API selector. It stands in for the manager's
// uncached APIReader, whose whole purpose is to decide the latch from live state
// the informer cache may not have caught up with yet.
type narrowedServiceReader struct {
	client.Reader
	barbican *barbicanv1alpha1.Barbican
}

func (n narrowedServiceReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil
	}
	svc.Name = key.Name
	svc.Namespace = key.Namespace
	svc.Spec.Selector = naming.APISelectorLabels(barbicanAppName, n.barbican.Name)
	return nil
}

// A Barbican whose secret-store projection is invalid gets no workload at all:
// barbican resolves its secret store at process start and exits when none is
// configured, so a Deployment built without one would crash-loop rather than
// serve degraded. The step reports the wait and hands the pipeline back a clean
// result — a store flipping to ready re-enqueues the CR through the store watch,
// so there is nothing to poll for.
func TestReconcileDeployment_InvalidProjectionCreatesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDeployment(context.Background(), r.Client, barbican,
		secretStoreProjection{}, deploymentConfigSecretName, "dsn", "auth")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue(), "a waiting projection must not requeue; the store watch wakes the CR")

	cond := barbicanCondition(barbican, "DeploymentReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForSecretStores))

	key := client.ObjectKey{Namespace: testNamespace, Name: testBarbicanName}
	g.Expect(r.Get(context.Background(), key, &appsv1.Deployment{})).NotTo(Succeed())
	g.Expect(r.Get(context.Background(), key, &corev1.Service{})).NotTo(Succeed())
	g.Expect(r.Get(context.Background(), key, &policyv1.PodDisruptionBudget{})).NotTo(Succeed())
	g.Expect(barbican.Status.Endpoint).To(BeEmpty())
}

func TestReconcileDeployment_CreatesWorkloadAndWaitsForRollout(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()
	r := newBarbicanTestReconciler(barbican)

	res, err := r.reconcileDeployment(context.Background(), r.Client, barbican,
		validProjection(), deploymentConfigSecretName, "dsn", "auth")

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).NotTo(BeZero(), "an unavailable Deployment is polled")

	key := client.ObjectKey{Namespace: testNamespace, Name: testBarbicanName}
	var deploy appsv1.Deployment
	g.Expect(r.Get(context.Background(), key, &deploy)).To(Succeed())
	g.Expect(r.Get(context.Background(), key, &corev1.Service{})).To(Succeed())
	g.Expect(r.Get(context.Background(), key, &policyv1.PodDisruptionBudget{})).To(Succeed())

	cond := barbicanCondition(barbican, "DeploymentReady")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForDeployment))
	g.Expect(barbican.Status.Endpoint).To(BeEmpty(),
		"the endpoint is only advertised once the Deployment is available")
}

// status.endpoint is the same URL the rendered [DEFAULT] host_href carries, so
// what barbican advertises in its API links and what the CR reports cannot
// drift. It is stamped only once the Deployment is available, and the
// healthcheck step gates its probe on it being set.
func TestReconcileDeployment_ReadyStampsTheAdvertisedEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		barbican func() *barbicanv1alpha1.Barbican
		want     string
	}{
		{
			name:     "cluster-local",
			barbican: testBarbican,
			want:     "http://test-barbican.openstack.svc.cluster.local:9311",
		},
		{
			name: "gateway",
			barbican: func() *barbicanv1alpha1.Barbican {
				b := testBarbican()
				b.Spec.Gateway = barbicanGatewaySpec()
				return b
			},
			want: "https://barbican.127-0-0-1.nip.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			barbican := tc.barbican()
			r := newBarbicanTestReconciler(barbican, readyBarbicanDeployment(barbican))

			res, err := r.reconcileDeployment(context.Background(), r.Client, barbican,
				validProjection(), deploymentConfigSecretName, "", "")

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			cond := barbicanCondition(barbican, "DeploymentReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(conditionReasonDeploymentReady))
			g.Expect(barbican.Status.Endpoint).To(Equal(tc.want))
			g.Expect(barbican.Status.Endpoint).To(Equal(barbicanPublicEndpoint(barbican)),
				"the advertised endpoint and the rendered host_href are one value")
		})
	}
}

// The AppRole secret ID reaches the pods exclusively through the env override.
// It must never enter the rendered config, which is mounted into every API pod
// and read back by the store controller.
func TestBuildBarbicanDeployment_SecretIDTravelsAsAnEnvOverride(t *testing.T) {
	g := NewGomegaWithT(t)
	projection := validProjection()

	deploy := buildBarbicanDeployment(testBarbican(), projection, deploymentConfigSecretName, "", "")

	var secretIDEnv *corev1.EnvVar
	env := deploy.Spec.Template.Spec.Containers[0].Env
	for i := range env {
		if env[i].Name == secretIDEnvVarName {
			secretIDEnv = &env[i]
		}
	}
	g.Expect(secretIDEnv).NotTo(BeNil())
	g.Expect(secretIDEnv.ValueFrom.SecretKeyRef.Name).To(Equal(projection.credentialsSecretName))
	g.Expect(secretIDEnv.ValueFrom.SecretKeyRef.Key).To(Equal(barbicanv1alpha1.OpenBaoSecretIDKey))
	g.Expect(secretIDEnv.Value).To(BeEmpty(), "the secret ID is referenced, never inlined")
}

// The CA volume is projected only when the store carries a bundle, and it has to
// land exactly where the rendered ssl_ca_crt_file points: a mount one directory
// off leaves the plugin opening a file that does not exist, and every request to
// the secret store fails.
func TestBuildBarbicanDeployment_SecretStoreCAVolume(t *testing.T) {
	g := NewGomegaWithT(t)

	without := buildBarbicanDeployment(testBarbican(), validProjection(), deploymentConfigSecretName, "", "")
	for _, v := range without.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(secretStoreCAVolumeName),
			"without a bundle the config omits ssl_ca_crt_file, so the mount would point at nothing")
	}

	projection := projectionWithCA()
	deploy := buildBarbicanDeployment(testBarbican(), projection, deploymentConfigSecretName, "", "")

	var caVolume *corev1.Volume
	for i, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == secretStoreCAVolumeName {
			caVolume = &deploy.Spec.Template.Spec.Volumes[i]
		}
	}
	g.Expect(caVolume).NotTo(BeNil())
	g.Expect(caVolume.Secret.SecretName).To(Equal(projection.caSecretName))
	g.Expect(caVolume.Secret.Items).To(Equal([]corev1.KeyToPath{
		{Key: barbicanv1alpha1.OpenBaoCAKey, Path: barbicanv1alpha1.OpenBaoCAKey},
	}))

	var caMount *corev1.VolumeMount
	mounts := deploy.Spec.Template.Spec.Containers[0].VolumeMounts
	for i := range mounts {
		if mounts[i].Name == secretStoreCAVolumeName {
			caMount = &mounts[i]
		}
	}
	g.Expect(caMount).NotTo(BeNil())
	g.Expect(caMount.MountPath+"/"+barbicanv1alpha1.OpenBaoCAKey).To(Equal(secretStoreCAFilePath),
		"the mount and the rendered ssl_ca_crt_file must resolve to the same file")
}

func TestBarbicanPodAnnotations(t *testing.T) {
	tests := []struct {
		name                                       string
		dsnDigest, authtokenDigest, secretIDDigest string
		want                                       map[string]string
	}{
		{
			name: "every digest empty leaves the template annotation-free",
			want: nil,
		},
		{
			name:      "only the DSN digest",
			dsnDigest: "dsn123",
			want:      map[string]string{dbConnectionHashAnnotation: "dsn123"},
		},
		{
			name:           "only the secret-id digest",
			secretIDDigest: "sid789",
			want:           map[string]string{secretStoreCredentialsHashAnnotation: "sid789"},
		},
		{
			name:            "all three stamped",
			dsnDigest:       "dsn123",
			authtokenDigest: "auth456",
			secretIDDigest:  "sid789",
			want: map[string]string{
				dbConnectionHashAnnotation:           "dsn123",
				authTokenHashAnnotation:              "auth456",
				secretStoreCredentialsHashAnnotation: "sid789",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(barbicanPodAnnotations(tc.dsnDigest, tc.authtokenDigest, tc.secretIDDigest)).To(Equal(tc.want))
		})
	}
}

// A re-minted AppRole secret ID only takes effect on a Pod restart: it is
// env-var-consumed, so a running pod keeps authenticating with the value it
// started with. The digest is what turns the re-mint into a rollout.
func TestBuildBarbicanDeployment_ReMintedSecretIDRollsThePods(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	before := validProjection()
	before.secretIDDigest = "digest-before"
	after := validProjection()
	after.secretIDDigest = "digest-after"

	beforeAnnotations := buildBarbicanDeployment(barbican, before, deploymentConfigSecretName, "", "").Spec.Template.Annotations
	afterAnnotations := buildBarbicanDeployment(barbican, after, deploymentConfigSecretName, "", "").Spec.Template.Annotations

	g.Expect(beforeAnnotations[secretStoreCredentialsHashAnnotation]).To(Equal("digest-before"))
	g.Expect(afterAnnotations[secretStoreCredentialsHashAnnotation]).To(Equal("digest-after"))
}

// Every uWSGI worker imports the whole app under the container's CPU limit, so
// the cold start stretches past the liveness budget once
// spec.apiServer.uwsgi.processes rises above the default (observed 66-91s at
// processes=4 under the default 500m limit, against a ~55s liveness budget:
// the container is killed before the app ever answers, forever). The startup
// probe must therefore exist and carry a budget that outlasts the worst
// observed cold start, and only then does the liveness probe take over.
func TestBuildBarbicanDeployment_StartupProbeOutlastsSlowColdStarts(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	deploy := buildBarbicanDeployment(barbican, validProjection(), deploymentConfigSecretName, "", "")
	container := deploy.Spec.Template.Spec.Containers[0]

	g.Expect(container.StartupProbe).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet).NotTo(BeNil())
	g.Expect(container.StartupProbe.HTTPGet.Path).To(Equal(barbicanHealthcheckPath))
	g.Expect(container.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(int(barbicanAPIPort)))
	g.Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(30)))
	g.Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(10)))
	g.Expect(container.StartupProbe.TimeoutSeconds).To(Equal(int32(8)))
}

// Once the Service selects by component, it must never widen again: re-widening
// would re-admit db-clean pods as endpoints for the duration of every later
// rollout. The latch is decided from the uncached reader, so a cache that still
// predates the narrowing write cannot undo it.
func TestReconcileDeployment_SelectorLatchReadsTheUncachedReader(t *testing.T) {
	g := NewGomegaWithT(t)
	key := client.ObjectKey{Namespace: testNamespace, Name: testBarbicanName}

	t.Run("unconverged template with no narrowed Service keeps the wide selector", func(t *testing.T) {
		barbican := testBarbican()
		r := newBarbicanTestReconciler(barbican)

		_, err := r.reconcileDeployment(context.Background(), r.Client, barbican,
			validProjection(), deploymentConfigSecretName, "", "")
		g.Expect(err).NotTo(HaveOccurred())

		var svc corev1.Service
		g.Expect(r.Get(context.Background(), key, &svc)).To(Succeed())
		g.Expect(svc.Spec.Selector).To(Equal(selectorLabels(barbican)),
			"pods from before the component label are still the only ones serving")
	})

	t.Run("an already narrowed Service stays narrowed", func(t *testing.T) {
		barbican := testBarbican()
		r := newBarbicanTestReconciler(barbican)
		r.apiReader = narrowedServiceReader{barbican: barbican}

		_, err := r.reconcileDeployment(context.Background(), r.Client, barbican,
			validProjection(), deploymentConfigSecretName, "", "")
		g.Expect(err).NotTo(HaveOccurred())

		var svc corev1.Service
		g.Expect(r.Get(context.Background(), key, &svc)).To(Succeed())
		g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(naming.LabelKeyComponent, naming.ComponentAPI))
	})
}

// The PDB selects by the absence of the Job name label rather than by the API
// component: a pod no budget selects has no budget at all, and keying on the
// component would open exactly that gap for every pod built by an operator
// predating the label.
func TestBuildPodDisruptionBudget_ExcludesJobPods(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	pdb := buildPodDisruptionBudget(barbican)

	g.Expect(pdb.Spec.Selector.MatchLabels).To(Equal(selectorLabels(barbican)))
	g.Expect(pdb.Spec.Selector.MatchExpressions).To(Equal(naming.ExcludeJobPods()))
	g.Expect(pdb.Spec.Selector.MatchLabels).NotTo(HaveKey(naming.LabelKeyComponent))
}

// Only the API pods may satisfy the API Service selector: the Service targets a
// numeric port and Job pods carry no readiness probe, so a clean-up pod matching
// it would become a ready endpoint with nothing listening on 9311.
func TestDBCleanPodsNotSelectedByAPIService(t *testing.T) {
	g := NewGomegaWithT(t)
	barbican := testBarbican()

	selector := labels.SelectorFromSet(buildBarbicanService(barbican, true).Spec.Selector)

	apiPodLabels := buildBarbicanDeployment(barbican, validProjection(), deploymentConfigSecretName, "", "").Spec.Template.Labels
	g.Expect(selector.Matches(labels.Set(apiPodLabels))).To(BeTrue(),
		"the API pod template must satisfy the API Service selector")

	cleanPodLabels := dbCleanCronJob(barbican, deploymentConfigSecretName).Spec.JobTemplate.Spec.Template.Labels
	g.Expect(cleanPodLabels).To(HaveKeyWithValue(naming.LabelKeyComponent, dbCleanComponent))
	g.Expect(selector.Matches(labels.Set(cleanPodLabels))).To(BeFalse(),
		"db-clean pods must never become endpoints of the API Service")
}
