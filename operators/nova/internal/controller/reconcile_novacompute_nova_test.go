// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	"github.com/c5c3/cobaltcore/operators/nova/internal/computeapi/computeapitest"
)

// TestReconcileNovaComputeNova_Waits covers the four waits on the Nova, in the
// order the step checks them. None of them reaches the Nova API: the step
// resolves what the API calls need and makes none itself.
func TestReconcileNovaComputeNova_Waits(t *testing.T) {
	noRelease := readyNovaForCompute()
	noRelease.Status.InstalledRelease = ""
	noContract := readyNovaForCompute()
	noContract.Status.ComputeConfigSecretRef = nil
	keyless := novaServiceUserSecret("")
	keyless.Data = map[string][]byte{"other": []byte("x")}

	for _, tc := range []struct {
		name   string
		objs   []client.Object
		reason string
	}{
		{name: "Nova not found", reason: conditionReasonNovaNotFound},
		{
			name:   "no installed release",
			objs:   []client.Object{noRelease, novaServiceUserSecret("pw")},
			reason: conditionReasonWaitingForInstalledRelease,
		},
		{
			name:   "no published contract",
			objs:   []client.Object{noContract, novaServiceUserSecret("pw")},
			reason: conditionReasonComputeConfigNotPublished,
		},
		{
			name:   "service-user Secret absent",
			objs:   []client.Object{readyNovaForCompute()},
			reason: conditionReasonWaitingForServiceUserSecret,
		},
		{
			name:   "service-user Secret without its key",
			objs:   []client.Object{readyNovaForCompute(), keyless},
			reason: conditionReasonWaitingForServiceUserSecret,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			api := computeapitest.New()
			r := newNovaComputeTestReconciler(api, tc.objs...)
			cr := validNovaCompute()
			pass := &novaComputePass{}

			result, err := r.reconcileNovaComputeNova(context.Background(), cr, pass)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			cond := novaComputeCondition(cr, conditionTypeNovaReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(tc.reason))
			g.Expect(*pass).To(BeZero())
			g.Expect(api.Calls()).To(BeEmpty())
		})
	}
}

// TestReconcileNovaComputeNova_Resolves pins what a resolved Nova hands the
// later steps: the default image on the installed release, the contract name,
// the internal compute URL, the service user's credentials, and the Nova with
// the client of its cluster for the host discovery Job.
func TestReconcileNovaComputeNova_Resolves(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := readyNovaForCompute()
	// The control plane moves to 2026.2 while it still runs 2026.1: the pool
	// stays on what is installed.
	nova.Spec.OpenStackRelease = "2026.2"
	api := computeapitest.New()
	r := newNovaComputeTestReconciler(api, nova, novaServiceUserSecret("pw"))
	cr := validNovaCompute()
	pass := &novaComputePass{}

	result, err := r.reconcileNovaComputeNova(context.Background(), cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(pass.image.Reference()).To(Equal("ghcr.io/c5c3/nova-compute:2026.1"))
	// spec.image is nil: the default image names no pullPolicy, so its tag
	// resolves to Always on every container of the pool's pod.
	pod := buildNovaComputeDaemonSet(cr, pass.image, pass.secretName, pinNovaComputeConfigMap, pinNovaComputeHash, nil).Spec.Template.Spec
	g.Expect(novaComputePullPolicies(pod)).To(Equal(map[string]corev1.PullPolicy{
		"create-instances-dir": corev1.PullAlways,
		"wait-for-chassis":     corev1.PullAlways,
		novaComputeComponent:   corev1.PullAlways,
	}))
	g.Expect(pass.secretName).To(Equal(testContract))
	g.Expect(pass.computeURL).To(Equal("http://nova.openstack.svc.cluster.local:8774"))
	g.Expect(pass.keystoneURL).To(Equal(nova.Spec.KeystoneEndpoint))
	g.Expect(pass.creds.Password).To(Equal("pw"))
	g.Expect(pass.creds.Username).To(Equal(nova.Spec.ServiceUser.Username))
	g.Expect(pass.doer).To(BeIdenticalTo(api))
	g.Expect(client.ObjectKeyFromObject(pass.nova)).To(Equal(client.ObjectKeyFromObject(nova)))
	g.Expect(pass.novaChildren).To(BeIdenticalTo(r.Client), "a local Nova's children live on the local cluster")

	cond := novaComputeCondition(cr, conditionTypeNovaReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonNovaResolved))
	g.Expect(api.Calls()).To(BeEmpty())
}

func TestReconcileNovaComputeNova_SpecImageWins(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNovaComputeTestReconciler(computeapitest.New(), readyNovaForCompute(), novaServiceUserSecret("pw"))
	cr := validNovaCompute()
	cr.Spec.Image = &commonv1.ImageSpec{Repository: "registry.example.com/nova-compute", Tag: "custom"}
	pass := &novaComputePass{}

	_, err := r.reconcileNovaComputeNova(context.Background(), cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pass.image.Reference()).To(Equal("registry.example.com/nova-compute:custom"))
}

// TestReconcileNovaComputeNova_SpecImageDigestPullPolicy pins that a digest
// spec.image reaches every container of the pool's pod as IfNotPresent.
func TestReconcileNovaComputeNova_SpecImageDigestPullPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNovaComputeTestReconciler(computeapitest.New(), readyNovaForCompute(), novaServiceUserSecret("pw"))
	cr := validNovaCompute()
	cr.Spec.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/nova-compute",
		Digest:     "sha256:" + strings.Repeat("b", 64),
	}
	pass := &novaComputePass{}

	_, err := r.reconcileNovaComputeNova(context.Background(), cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	pod := buildNovaComputeDaemonSet(cr, pass.image, pass.secretName, pinNovaComputeConfigMap, pinNovaComputeHash, nil).Spec.Template.Spec
	g.Expect(novaComputePullPolicies(pod)).To(Equal(map[string]corev1.PullPolicy{
		"create-instances-dir": corev1.PullIfNotPresent,
		"wait-for-chassis":     corev1.PullIfNotPresent,
		novaComputeComponent:   corev1.PullIfNotPresent,
	}))
}

// TestReconcileNovaComputeNova_UnregisteredNovaCluster pins the wait on a Nova
// placed on a cluster the operator has not engaged: the doer and the Secret
// read both resolve through it.
func TestReconcileNovaComputeNova_UnregisteredNovaCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	nova := readyNovaForCompute()
	nova.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "control-a"}
	r := newNovaComputeTestReconciler(nil, nova)
	r.HTTPClient = nil
	r.Resolver = unresolvableResolver{}
	cr := validNovaCompute()

	result, err := r.reconcileNovaComputeNova(context.Background(), cr, &novaComputePass{})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	cond := novaComputeCondition(cr, conditionTypeNovaReady)
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
}
