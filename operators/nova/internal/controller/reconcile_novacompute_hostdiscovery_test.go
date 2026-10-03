// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// testNovaConfigMap is the config ConfigMap the Nova API Deployment fixture
// mounts.
const testNovaConfigMap = "nova-config-abc"

// testDiscoveryJob is the key of the discovery Job of validNova.
var testDiscoveryJob = client.ObjectKey{Namespace: testNamespace, Name: testNovaName + "-discover-hosts"}

// discoveryPass is apiPass with what the NovaRef step resolves for the host
// discovery: the Nova and the client of its cluster.
func discoveryPass(api *computeapitest.Fake, children client.Client) *novaComputePass {
	pass := apiPass(api)
	pass.nova = validNova()
	pass.novaChildren = children
	return pass
}

// discoveryResult is what one ensureHostDiscovery call returned and recorded.
type discoveryResult struct {
	note   string
	err    error
	events []string
}

// runDiscovery runs ensureHostDiscovery for the fixture node against children,
// the Nova's cluster.
func runDiscovery(children client.Client) discoveryResult {
	r := newNovaComputeTestReconciler(computeapitest.New())
	cr := poolWith(novav1alpha1.NovaComputeNodePending)
	note, err := r.ensureHostDiscovery(context.Background(), cr,
		discoveryPass(computeapitest.New(), children), []string{testNodeName})
	return discoveryResult{note: note, err: err, events: collectEvents(r.Recorder.(*record.FakeRecorder))}
}

// claimedDiscoveryJob is the Nova's discovery Job claimed for the Nova on
// children's cluster, with condition True since age ago. An empty condition
// leaves it running.
func claimedDiscoveryJob(t *testing.T, children client.Client, condition batchv1.JobConditionType,
	age time.Duration, message string,
) *batchv1.Job {
	t.Helper()
	discovery := hostDiscoveryJob(validNova(), testNovaConfigMap, validNova().Spec.Image.Reference())
	NewGomegaWithT(t).Expect(commonmulticluster.Claim(children, testScheme(), validNova(), discovery)).To(Succeed())
	if condition != "" {
		discovery.Status.Conditions = []batchv1.JobCondition{{
			Type:               condition,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
			Message:            message,
		}}
	}
	return discovery
}

// discoveryJobExists reports whether the discovery Job is on c.
func discoveryJobExists(g Gomega, c client.Client) bool {
	err := c.Get(context.Background(), testDiscoveryJob, &batchv1.Job{})
	if apierrors.IsNotFound(err) {
		return false
	}
	g.ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return true
}

func TestHostDiscoveryJobName(t *testing.T) {
	t.Run("a name the webhook admits keeps the plain form", func(t *testing.T) {
		g := NewGomegaWithT(t)
		name := strings.Repeat("n", novav1alpha1.MaxNovaNameLength)
		g.Expect(hostDiscoveryJobName(name)).To(Equal(name + "-discover-hosts"))
		g.Expect(hostDiscoveryJobName(testNovaName)).To(Equal(testDiscoveryJob.Name))
	})

	t.Run("the longest name that fits keeps the plain form", func(t *testing.T) {
		g := NewGomegaWithT(t)
		name := strings.Repeat("n", 48)
		g.Expect(hostDiscoveryJobName(name)).To(Equal(name + "-discover-hosts"))
		g.Expect(hostDiscoveryJobName(name)).To(HaveLen(63))
	})

	// A Nova admitted before the name bound can be up to 52 characters long;
	// '<name>-novncproxy' still fits 63 then.
	t.Run("a longer name collapses onto a hash within 63 characters", func(t *testing.T) {
		g := NewGomegaWithT(t)
		for _, name := range []string{strings.Repeat("n", 49), strings.Repeat("n", 52), strings.Repeat("n", 63)} {
			got := hostDiscoveryJobName(name)
			g.Expect(len(got)).To(BeNumerically("<=", 63), got)
			g.Expect(got).To(HaveSuffix("-discover-hosts"))
			g.Expect(got).To(MatchRegexp(`^n+-[0-9a-f]{8}-discover-hosts$`))
			g.Expect(hostDiscoveryJobName(name)).To(Equal(got), "the name is stable across passes")
		}
		g.Expect(hostDiscoveryJobName(strings.Repeat("n", 49))).
			NotTo(Equal(hostDiscoveryJobName(strings.Repeat("n", 50))), "two names keep two Jobs")
	})

	t.Run("the truncation does not end on a separator", func(t *testing.T) {
		g := NewGomegaWithT(t)
		got := hostDiscoveryJobName(strings.Repeat("n", 38) + "-." + strings.Repeat("n", 12))
		g.Expect(got).To(MatchRegexp(`^n{38}-[0-9a-f]{8}-discover-hosts$`))
	})
}

func TestHostDiscoveryJob(t *testing.T) {
	t.Run("one run of the plain discovery with the API's config", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		discovery := hostDiscoveryJob(nova, testNovaConfigMap, "ghcr.io/c5c3/nova:running")

		g.Expect(client.ObjectKeyFromObject(discovery)).To(Equal(testDiscoveryJob))
		labels := componentLabels(nova, componentHostDiscovery)
		g.Expect(discovery.Labels).To(Equal(labels))
		g.Expect(discovery.Spec.Template.Labels).To(Equal(labels))
		g.Expect(discovery.Spec.BackoffLimit).To(Equal(ptr.To[int32](0)))
		g.Expect(discovery.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To[int64](300)))
		g.Expect(discovery.Spec.TTLSecondsAfterFinished).To(Equal(ptr.To[int32](300)))

		pod := discovery.Spec.Template.Spec
		g.Expect(pod.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		g.Expect(pod.Containers).To(HaveLen(1))
		container := pod.Containers[0]
		g.Expect(container.Name).To(Equal("discover-hosts"))
		g.Expect(container.Image).To(Equal("ghcr.io/c5c3/nova:running"))
		g.Expect(container.Command).To(Equal([]string{
			"nova-manage", "--config-dir", "/etc/nova/nova.conf.d", "cell_v2", "discover_hosts", "--verbose",
		}))
		g.Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name: configVolumeName, MountPath: "/etc/nova/nova.conf.d", ReadOnly: true,
		}))
		g.Expect(pod.Volumes).To(ContainElement(corev1.Volume{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: testNovaConfigMap},
			}},
		}))
	})

	// The discovery pod is the archive pod with another command: the same
	// mounts, environment, security contexts, resources and placement, so the
	// two cannot drift apart. The archive's three bounds are its own.
	archivePod := func(nova *novav1alpha1.Nova) corev1.PodSpec {
		pod := *dbArchiveCronJob(nova, configArtifacts{configMapName: testNovaConfigMap}).
			Spec.JobTemplate.Spec.Template.Spec.DeepCopy()
		pod.RestartPolicy = corev1.RestartPolicyNever
		container := &pod.Containers[0]
		container.Name = componentHostDiscovery
		container.Command = []string{
			"nova-manage", "--config-dir", novaConfigDir, "cell_v2", "discover_hosts", "--verbose",
		}
		container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool {
			return env.Name == dbArchiveMaxRowsEnvVarName || env.Name == dbArchiveSleepEnvVarName ||
				env.Name == dbArchiveRetentionDaysEnvVarName
		})
		return pod
	}

	t.Run("the pod is the archive pod", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		g.Expect(hostDiscoveryJob(nova, testNovaConfigMap, nova.Spec.Image.Reference()).Spec.Template.Spec).
			To(Equal(archivePod(nova)))
	})

	t.Run("the pod is the archive pod with database TLS", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		for _, db := range []*commonv1.DatabaseSpec{&nova.Spec.APIDatabase, &nova.Spec.Database} {
			db.TLS = &commonv1.DatabaseTLSSpec{
				Mode:                "verify-full",
				CABundleSecretRef:   commonv1.SecretRefSpec{Name: "nova-db-ca"},
				ClientCertSecretRef: commonv1.SecretRefSpec{Name: "nova-db-client"},
			}
		}
		pod := hostDiscoveryJob(nova, testNovaConfigMap, nova.Spec.Image.Reference()).Spec.Template.Spec
		g.Expect(pod.Volumes).To(HaveLen(4), "config, tmp and one TLS projection per schema")
		g.Expect(pod).To(Equal(archivePod(nova)))
	})
}

func TestEnsureHostDiscovery(t *testing.T) {
	startedNote := "started discovery Job " + testDiscoveryJob.String()

	t.Run("the first pass creates the Job for the Nova", func(t *testing.T) {
		g := NewGomegaWithT(t)
		nova := validNova()
		c := novaFakeClientBuilder(novaAPIDeployment(nova, testNovaConfigMap)).Build()

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal(startedNote))
		var created batchv1.Job
		g.Expect(c.Get(context.Background(), testDiscoveryJob, &created)).To(Succeed())
		want := hostDiscoveryJob(nova, testNovaConfigMap, nova.Spec.Image.Reference())
		g.Expect(created.Labels).To(Equal(want.Labels))
		g.Expect(created.Spec).To(Equal(want.Spec))
		g.Expect(metav1.IsControlledBy(&created, nova)).To(BeTrue(), "the Nova is the Job's controller")
		g.Expect(res.events).To(Equal([]string{
			"Normal HostDiscoveryStarted Started Job " + testDiscoveryJob.String() + " to map node-1 into the cell",
		}))
	})

	// During an upgrade spec.image already names the next release while the API
	// Deployment, and the schemas, are still on the running one.
	t.Run("the Job runs the image the API Deployment runs", func(t *testing.T) {
		g := NewGomegaWithT(t)
		deploy := novaAPIDeployment(validNova(), testNovaConfigMap)
		deploy.Spec.Template.Spec.Containers[0].Image = "ghcr.io/c5c3/nova:2025.1"
		c := novaFakeClientBuilder(deploy).Build()

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		var created batchv1.Job
		g.Expect(c.Get(context.Background(), testDiscoveryJob, &created)).To(Succeed())
		g.Expect(created.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/c5c3/nova:2025.1"))
		g.Expect(validNova().Spec.Image.Reference()).NotTo(Equal("ghcr.io/c5c3/nova:2025.1"))
	})

	t.Run("on a target cluster the Job carries the Nova's ownership labels", func(t *testing.T) {
		g := NewGomegaWithT(t)
		target := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).Build()

		res := runDiscovery(commonmulticluster.Remote(target))

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal(startedNote))
		var created batchv1.Job
		g.Expect(target.Get(context.Background(), testDiscoveryJob, &created)).To(Succeed())
		g.Expect(created.OwnerReferences).To(BeEmpty())
		g.Expect(created.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerKindLabel, "Nova"))
		g.Expect(created.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerNameLabel, testNovaName))
		g.Expect(created.Labels).To(HaveKeyWithValue(commonmulticluster.OwnerNamespaceLabel, testNamespace))
		g.Expect(res.events).To(HaveLen(1))
	})

	t.Run("a running Job is left alone", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).Build()
		g.Expect(c.Create(context.Background(), claimedDiscoveryJob(t, c, "", 0, ""))).To(Succeed())

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal("discovery Job " + testDiscoveryJob.String() + " is running"))
		g.Expect(discoveryJobExists(g, c)).To(BeTrue())
		g.Expect(res.events).To(BeEmpty())
	})

	for _, tc := range []struct {
		condition batchv1.JobConditionType
		outcome   string
	}{
		{condition: batchv1.JobComplete, outcome: "finished without mapping them"},
		{condition: batchv1.JobFailed, outcome: "failed"},
	} {
		t.Run("a Job "+string(tc.condition)+" for less than 30s is left alone", func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).Build()
			g.Expect(c.Create(context.Background(),
				claimedDiscoveryJob(t, c, tc.condition, 10*time.Second, "boom"))).To(Succeed())

			res := runDiscovery(c)

			g.Expect(res.err).NotTo(HaveOccurred())
			g.Expect(res.note).To(Equal("discovery Job " + testDiscoveryJob.String() + " " + tc.outcome +
				"; it is replaced after 30s"))
			g.Expect(discoveryJobExists(g, c)).To(BeTrue())
			g.Expect(res.events).To(BeEmpty())
		})
	}

	t.Run("a Job complete for longer is deleted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).Build()
		g.Expect(c.Create(context.Background(),
			claimedDiscoveryJob(t, c, batchv1.JobComplete, time.Minute, ""))).To(Succeed())

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal("replacing the finished discovery Job " + testDiscoveryJob.String()))
		g.Expect(discoveryJobExists(g, c)).To(BeFalse())
		g.Expect(res.events).To(BeEmpty())
	})

	t.Run("a Job failed for longer is deleted with an event", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).Build()
		g.Expect(c.Create(context.Background(), claimedDiscoveryJob(t, c, batchv1.JobFailed, time.Minute,
			"Job has reached the specified backoff limit"))).To(Succeed())

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal("replacing the finished discovery Job " + testDiscoveryJob.String()))
		g.Expect(discoveryJobExists(g, c)).To(BeFalse())
		g.Expect(res.events).To(Equal([]string{
			"Warning HostDiscoveryFailed Job " + testDiscoveryJob.String() +
				" failed: Job has reached the specified backoff limit",
		}))
	})

	for name, deploy := range map[string][]client.Object{
		"no Job runs while the Nova API Deployment is missing":       nil,
		"no Job runs while the Nova API Deployment mounts no config": {novaAPIDeployment(validNova(), "")},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := novaFakeClientBuilder(deploy...).Build()

			res := runDiscovery(c)

			g.Expect(res.err).NotTo(HaveOccurred())
			g.Expect(res.note).To(Equal(
				"no discovery Job can run: the Nova API Deployment openstack/nova mounts no config yet"))
			g.Expect(discoveryJobExists(g, c)).To(BeFalse())
			g.Expect(res.events).To(BeEmpty())
		})
	}

	t.Run("AlreadyExists on the create counts as started", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"},
						testDiscoveryJob.Name)
				},
			}).Build()

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal(startedNote))
		g.Expect(res.events).To(BeEmpty(), "another pass started the Job")
	})

	t.Run("NotFound on the delete counts as replaced", func(t *testing.T) {
		g := NewGomegaWithT(t)
		c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"},
						testDiscoveryJob.Name)
				},
			}).Build()
		g.Expect(c.Create(context.Background(),
			claimedDiscoveryJob(t, c, batchv1.JobComplete, time.Minute, ""))).To(Succeed())

		res := runDiscovery(c)

		g.Expect(res.err).NotTo(HaveOccurred())
		g.Expect(res.note).To(Equal("replacing the finished discovery Job " + testDiscoveryJob.String()))
	})

	boom := errors.New("the server is unavailable")
	for _, tc := range []struct {
		name    string
		funcs   func(error) interceptor.Funcs
		job     batchv1.JobConditionType
		wantErr string
	}{
		{
			name: "a failed Job read is an error",
			funcs: func(err error) interceptor.Funcs {
				return interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, isJob := obj.(*batchv1.Job); isJob {
						return err
					}
					return cl.Get(ctx, key, obj, opts...)
				}}
			},
			wantErr: "getting discovery Job " + testDiscoveryJob.String(),
		},
		{
			name: "a failed Deployment read is an error",
			funcs: func(err error) interceptor.Funcs {
				return interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
						return err
					}
					return cl.Get(ctx, key, obj, opts...)
				}}
			},
			wantErr: "reading the config of Nova openstack/nova",
		},
		{
			name: "a failed create is an error",
			funcs: func(err error) interceptor.Funcs {
				return interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
					...client.CreateOption,
				) error {
					return err
				}}
			},
			wantErr: "creating discovery Job " + testDiscoveryJob.String(),
		},
		{
			name: "a failed delete is an error",
			funcs: func(err error) interceptor.Funcs {
				return interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object,
					...client.DeleteOption,
				) error {
					return err
				}}
			},
			job:     batchv1.JobComplete,
			wantErr: "deleting discovery Job " + testDiscoveryJob.String(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			c := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap)).
				WithInterceptorFuncs(tc.funcs(boom)).Build()
			if tc.job != "" {
				g.Expect(c.Create(context.Background(),
					claimedDiscoveryJob(t, c, tc.job, time.Minute, ""))).To(Succeed())
			}

			res := runDiscovery(c)

			g.Expect(res.err).To(MatchError(boom))
			g.Expect(res.err).To(MatchError(ContainSubstring(tc.wantErr)))
			g.Expect(res.events).To(BeEmpty())
		})
	}

	t.Run("a foreign Job on a target cluster is refused and kept", func(t *testing.T) {
		g := NewGomegaWithT(t)
		foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: testDiscoveryJob.Name, Namespace: testNamespace, UID: "foreign-job-uid",
		}}
		foreign.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}
		target := novaFakeClientBuilder(novaAPIDeployment(validNova(), testNovaConfigMap), foreign).Build()

		res := runDiscovery(commonmulticluster.Remote(target))

		g.Expect(res.err).To(MatchError(ContainSubstring("refusing to adopt pre-existing Job " + testDiscoveryJob.String())))
		g.Expect(res.note).To(BeEmpty())
		g.Expect(discoveryJobExists(g, target)).To(BeTrue(), "a Job the Nova does not own is not deleted")
		g.Expect(res.events).To(BeEmpty())
	})
}
