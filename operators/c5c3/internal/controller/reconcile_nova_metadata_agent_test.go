// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the delivery of the metadata shared secret to the
// NeutronMetadataAgents on target clusters: the targets, the copy, and the
// NovaReady reasons the leg reports.
package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// metadataCentralNamespace is the OVN central's namespace the fixtures name, so
// the agents live apart from the ControlPlane and the Nova.
const metadataCentralNamespace = "ovn"

// metadataAgentControlPlane is novaControlPlane with its OVN central in
// metadataCentralNamespace.
func metadataAgentControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := novaControlPlane()
	cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = metadataCentralNamespace
	return cp
}

// metadataAgent builds a NeutronMetadataAgent in namespace, placed on cluster
// ("" keeps it local), that signs with the named Secret.
func metadataAgent(name, namespace, cluster, sharedSecret string) *neutronv1alpha1.NeutronMetadataAgent {
	agent := &neutronv1alpha1.NeutronMetadataAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: neutronv1alpha1.NeutronMetadataAgentSpec{
			ChassisRef: neutronv1alpha1.OVNChassisRef{Name: "chassis"},
			NovaMetadata: &neutronv1alpha1.NovaMetadataSpec{
				Protocol:        "https",
				SharedSecretRef: &commonv1.SecretRefSpec{Name: sharedSecret, Key: "shared_secret"},
			},
		},
	}
	if cluster != "" {
		agent.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: cluster}
	}
	return agent
}

// metadataContract is the compute contract carrying the metadata shared secret
// under its contract key.
func metadataContract(cp *c5c3v1alpha1.ControlPlane, value string) *corev1.Secret {
	contract := publishedComputeConfig(cp)
	contract.Data[novav1alpha1.ComputeConfigMetadataSharedSecretKey] = []byte(value)
	return contract
}

// metadataAgentCopyKey is the key of the copy on a target cluster.
func metadataAgentCopyKey(cp *c5c3v1alpha1.ControlPlane) types.NamespacedName {
	return types.NamespacedName{Name: novaMetadataAgentSecretName(cp), Namespace: metadataCentralNamespace}
}

// TestNovaMetadataAgentTargets pins the enumeration: one target per cluster a
// qualifying agent runs on, sorted, each in the central's namespace.
func TestNovaMetadataAgentTargets(t *testing.T) {
	ctx := context.Background()

	t.Run("one target per cluster a qualifying agent runs on", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		copyName := novaMetadataAgentSecretName(cp)
		deleting := metadataAgent("deleting", metadataCentralNamespace, "compute-c", copyName)
		deleting.DeletionTimestamp = ptr.To(metav1.Now())
		deleting.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
		noBlock := metadataAgent("no-block", metadataCentralNamespace, "compute-d", copyName)
		noBlock.Spec.NovaMetadata = nil
		noRef := metadataAgent("no-ref", metadataCentralNamespace, "compute-e", copyName)
		noRef.Spec.NovaMetadata.SharedSecretRef = nil
		r := newNovaTestReconciler(t, cp,
			metadataAgent("agent-b", metadataCentralNamespace, "b", copyName),
			metadataAgent("agent-a1", metadataCentralNamespace, "a", copyName),
			metadataAgent("agent-a2", metadataCentralNamespace, "a", copyName),
			metadataAgent("local", metadataCentralNamespace, "", copyName),
			metadataAgent("other-secret", metadataCentralNamespace, "compute-f", "hand-made"),
			metadataAgent("elsewhere", "other-namespace", "compute-g", copyName),
			deleting, noBlock, noRef,
		)

		targets, err := r.novaMetadataAgentTargets(ctx, cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(targets).To(Equal([]computeConfigMirrorTarget{
			{ClusterRef: &commonv1.TargetClusterRefSpec{Name: "a"}, Namespace: metadataCentralNamespace},
			{ClusterRef: &commonv1.TargetClusterRefSpec{Name: "b"}, Namespace: metadataCentralNamespace},
		}))
	})

	t.Run("a plane without a neutron block lists nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		cp.Spec.Services.Neutron = nil
		lists := 0
		r := newHVOTestReconciler(t, interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*neutronv1alpha1.NeutronMetadataAgentList); ok {
					lists++
				}
				return cl.List(ctx, list, opts...)
			},
		}, cp, readyNovaRegistration(cp))

		targets, err := r.novaMetadataAgentTargets(ctx, cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(targets).To(BeEmpty())
		g.Expect(lists).To(BeZero())
	})

	for _, tc := range []struct {
		name    string
		listErr error
		wantErr string
	}{
		{
			name: "an unserved kind yields no target",
			listErr: &meta.NoKindMatchError{
				GroupKind: neutronv1alpha1.GroupVersion.WithKind("NeutronMetadataAgent").GroupKind(),
			},
		},
		{
			name:    "any other list error is returned",
			listErr: errors.New("etcd is down"),
			wantErr: `listing the NeutronMetadataAgents in namespace "ovn": etcd is down`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := metadataAgentControlPlane()
			r := newHVOTestReconciler(t, interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*neutronv1alpha1.NeutronMetadataAgentList); ok {
						return tc.listErr
					}
					return cl.List(ctx, list, opts...)
				},
			}, cp, readyNovaRegistration(cp))

			targets, err := r.novaMetadataAgentTargets(ctx, cp)

			g.Expect(targets).To(BeEmpty())
			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(MatchError(tc.wantErr))
		})
	}
}

// metadataAgentFleet is a plane with one qualifying agent on compute-a and one
// fake client per compute cluster.
type metadataAgentFleet struct {
	cp       *c5c3v1alpha1.ControlPlane
	r        *ControlPlaneReconciler
	clusters map[string]client.Client
}

func newMetadataAgentFleet(t *testing.T, computeA interceptor.Funcs, seedA []client.Object,
	objs ...client.Object,
) *metadataAgentFleet {
	t.Helper()
	cp := metadataAgentControlPlane()
	objs = append([]client.Object{
		cp, metadataAgent("agent-a", metadataCentralNamespace, "compute-a", novaMetadataAgentSecretName(cp)),
	}, objs...)
	r := newNovaTestReconciler(t, objs...)
	clusters := map[string]client.Client{
		"compute-a": fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(seedA...).
			WithInterceptorFuncs(computeA).Build(),
		"compute-b": fake.NewClientBuilder().WithScheme(r.Scheme).Build(),
	}
	r.Resolver = perClusterResolver(clusters)
	return &metadataAgentFleet{cp: cp, r: r, clusters: clusters}
}

// TestReconcileNovaMetadataAgentSecrets_DeliversTheCopy pins the copy: one
// Secret on the agent's cluster, in the central's namespace, carrying the
// single key the agent reads, the ownership labels and the mirror label the
// agent's teardown recognizes. A cluster no agent names receives nothing.
func TestReconcileNovaMetadataAgentSecrets_DeliversTheCopy(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := metadataAgentControlPlane()
	f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, metadataContract(cp, "s3cr3t"))

	res, halt, err := f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(res.IsZero()).To(BeTrue())
	copied := &corev1.Secret{}
	g.Expect(f.clusters["compute-a"].Get(ctx, metadataAgentCopyKey(f.cp), copied)).To(Succeed())
	g.Expect(copied.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(copied.Data).To(Equal(map[string][]byte{"shared_secret": []byte("s3cr3t")}),
		"the copy carries the shared secret alone, never the contract's bus URL or service password")
	for k, v := range remoteChildLabels(f.cp) {
		g.Expect(copied.Labels).To(HaveKeyWithValue(k, v))
	}
	g.Expect(copied.Labels).To(HaveKeyWithValue(neutronv1alpha1.MetadataSharedSecretMirrorLabel, "true"))

	var elsewhere corev1.SecretList
	g.Expect(f.clusters["compute-b"].List(ctx, &elsewhere)).To(Succeed())
	g.Expect(elsewhere.Items).To(BeEmpty())
}

// TestReconcileNovaMetadataAgentSecrets_OneCopyPerCluster pins a plane with
// agents on two clusters: each cluster receives a copy through its own client.
func TestReconcileNovaMetadataAgentSecrets_OneCopyPerCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := metadataAgentControlPlane()
	f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, metadataContract(cp, "s3cr3t"),
		metadataAgent("agent-b", metadataCentralNamespace, "compute-b", novaMetadataAgentSecretName(cp)))

	_, halt, err := f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	for _, cluster := range []string{"compute-a", "compute-b"} {
		copied := &corev1.Secret{}
		g.Expect(f.clusters[cluster].Get(ctx, metadataAgentCopyKey(f.cp), copied)).To(Succeed(), cluster)
		g.Expect(copied.Data).To(Equal(map[string][]byte{"shared_secret": []byte("s3cr3t")}), cluster)
	}
}

// TestReconcileNovaMetadataAgentSecrets_PlacedNovaReadsTheContractThere pins a
// Nova on a target cluster: the contract is read where the nova operator
// published it, on the Nova's cluster, and never on the management cluster.
func TestReconcileNovaMetadataAgentSecrets_PlacedNovaReadsTheContractThere(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := placedNovaControlPlane("nova-cluster")
	cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = metadataCentralNamespace
	r := newNovaTestReconciler(t, cp,
		metadataAgent("agent-a", metadataCentralNamespace, "compute-a", novaMetadataAgentSecretName(cp)))
	novaCluster := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(metadataContract(cp, "s3cr3t")).Build()
	computeA := fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.Resolver = perClusterResolver{"nova-cluster": novaCluster, "compute-a": computeA}

	_, halt, err := r.reconcileNovaMetadataAgentSecrets(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	copied := &corev1.Secret{}
	g.Expect(computeA.Get(ctx, metadataAgentCopyKey(cp), copied)).To(Succeed())
	g.Expect(copied.Data).To(Equal(map[string][]byte{"shared_secret": []byte("s3cr3t")}))
}

// TestReconcileNovaMetadataAgentSecrets_NoTargetNoWrite pins a plane no placed
// agent asks for a copy: nothing is read, nothing is written, nothing halts.
// The contract is left unpublished, so a read would have halted on it.
func TestReconcileNovaMetadataAgentSecrets_NoTargetNoWrite(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := metadataAgentControlPlane()
	r := newNovaTestReconciler(t, cp,
		metadataAgent("local", metadataCentralNamespace, "", novaMetadataAgentSecretName(cp)))
	computeA := fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.Resolver = perClusterResolver{"compute-a": computeA}

	res, halt, err := r.reconcileNovaMetadataAgentSecrets(ctx, cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	g.Expect(res.IsZero()).To(BeTrue())
	var secrets corev1.SecretList
	g.Expect(computeA.List(ctx, &secrets)).To(Succeed())
	g.Expect(secrets.Items).To(BeEmpty())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, metadataAgentCopyKey(cp), &corev1.Secret{}))).To(BeTrue())
}

// TestReconcileNovaMetadataAgentSecrets_WaitsForTheContract pins the two waits
// on the contract: not published yet, and published without the shared secret.
func TestReconcileNovaMetadataAgentSecrets_WaitsForTheContract(t *testing.T) {
	cp := metadataAgentControlPlane()
	withoutKey := publishedComputeConfig(cp)
	emptyKey := metadataContract(cp, "")
	for _, tc := range []struct {
		name        string
		contract    *corev1.Secret
		wantMessage []string
	}{
		{
			name: "the contract is not published yet",
			wantMessage: []string{
				"the compute config Secret default/cp-nova-compute-config has not been published yet",
				`the metadata agents in namespace "ovn" sign with the shared secret it carries`,
			},
		},
		{
			name:     "the contract carries no shared secret",
			contract: withoutKey,
			wantMessage: []string{
				"the compute config Secret default/cp-nova-compute-config carries no metadata_proxy_shared_secret yet",
			},
		},
		{
			name:     "the contract carries an empty shared secret",
			contract: emptyKey,
			wantMessage: []string{
				"the compute config Secret default/cp-nova-compute-config carries no metadata_proxy_shared_secret yet",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			var objs []client.Object
			if tc.contract != nil {
				objs = append(objs, tc.contract.DeepCopy())
			}
			f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, objs...)

			res, halt, err := f.r.reconcileNovaMetadataAgentSecrets(context.Background(), f.cp)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(halt).To(BeTrue())
			g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			cond := novaCondition(t, f.cp)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonWaitingForComputeConfig))
			for _, want := range tc.wantMessage {
				g.Expect(cond.Message).To(ContainSubstring(want))
			}
			g.Expect(apierrors.IsNotFound(f.clusters["compute-a"].Get(context.Background(),
				metadataAgentCopyKey(f.cp), &corev1.Secret{}))).To(BeTrue(), "no copy is written without a value")
		})
	}
}

// TestReconcileNovaMetadataAgentSecrets_ContractReadError pins a read of the
// contract that fails for another reason than NotFound: a failed reconcile.
func TestReconcileNovaMetadataAgentSecrets_ContractReadError(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := metadataAgentControlPlane()
	injected := errors.New("etcd is down")
	r := newHVOTestReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if key.Name == novaComputeConfigSecretName(cp) {
				return injected
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, cp, readyNovaRegistration(cp), metadataContract(cp, "s3cr3t"),
		metadataAgent("agent-a", metadataCentralNamespace, "compute-a", novaMetadataAgentSecretName(cp)))
	r.Resolver = perClusterResolver{"compute-a": fake.NewClientBuilder().WithScheme(r.Scheme).Build()}

	_, halt, err := r.reconcileNovaMetadataAgentSecrets(context.Background(), cp)

	g.Expect(halt).To(BeTrue())
	g.Expect(err).To(MatchError("reading the compute config Secret default/cp-nova-compute-config: etcd is down"))
	g.Expect(errors.Is(err, injected)).To(BeTrue())
	g.Expect(novaCondition(t, cp).Reason).To(Equal(reasonNovaMetadataAgentSecretError))
}

// TestReconcileNovaMetadataAgentSecrets_AgentListError pins a failed list of the
// agents: a failed reconcile, never the no-target pass that lets NovaReady turn
// True.
func TestReconcileNovaMetadataAgentSecrets_AgentListError(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := metadataAgentControlPlane()
	injected := errors.New("etcd is down")
	r := newHVOTestReconciler(t, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*neutronv1alpha1.NeutronMetadataAgentList); ok {
				return injected
			}
			return cl.List(ctx, list, opts...)
		},
	}, cp, readyNovaRegistration(cp), metadataContract(cp, "s3cr3t"))

	_, halt, err := r.reconcileNovaMetadataAgentSecrets(context.Background(), cp)

	g.Expect(halt).To(BeTrue())
	g.Expect(err).To(MatchError(`listing the NeutronMetadataAgents in namespace "ovn": etcd is down`))
	g.Expect(errors.Is(err, injected)).To(BeTrue())
	g.Expect(novaCondition(t, cp).Reason).To(Equal(reasonNovaMetadataAgentSecretError))
}

// TestReconcileNovaMetadataAgentSecrets_UnresolvableClusters pins the two
// clusters that may not resolve, the Nova's and the agent's: a wait on
// TargetClusterUnavailable with the resolver's text, not a failed reconcile.
func TestReconcileNovaMetadataAgentSecrets_UnresolvableClusters(t *testing.T) {
	t.Run("the Nova's cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := placedNovaControlPlane("nova-cluster")
		cp.Spec.Services.Neutron.OVN.CentralRef.Namespace = metadataCentralNamespace
		r := newNovaTestReconciler(t, cp,
			metadataAgent("agent-a", metadataCentralNamespace, "compute-a", novaMetadataAgentSecretName(cp)))
		r.Resolver = perClusterResolver{"compute-a": fake.NewClientBuilder().WithScheme(r.Scheme).Build()}

		res, halt, err := r.reconcileNovaMetadataAgentSecrets(context.Background(), cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeTrue())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
		cond := novaCondition(t, cp)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring(`cluster "nova-cluster" not found`))
	})

	t.Run("the agent's cluster", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		r := newNovaTestReconciler(t, cp, metadataContract(cp, "s3cr3t"),
			metadataAgent("agent-x", metadataCentralNamespace, "compute-x", novaMetadataAgentSecretName(cp)))
		r.Resolver = perClusterResolver{}

		res, halt, err := r.reconcileNovaMetadataAgentSecrets(context.Background(), cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeTrue())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
		cond := novaCondition(t, cp)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(ContainSubstring(`cluster "compute-x" not found`))
	})
}

// TestReconcileNovaMetadataAgentSecrets_WriteFailures pins a failed write and a
// same-named Secret the plane did not write on the agent's cluster: a failed
// reconcile naming the namespace and the cluster, and a foreign Secret left
// untouched.
func TestReconcileNovaMetadataAgentSecrets_WriteFailures(t *testing.T) {
	t.Run("the apply fails", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		injected := errors.New("injected write failure")
		f := newMetadataAgentFleet(t, interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration,
				opts ...client.ApplyOption,
			) error {
				if ac, ok := obj.(client.Object); ok && ac.GetName() == novaMetadataAgentSecretName(cp) {
					return injected
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}, nil, metadataContract(cp, "s3cr3t"))

		_, halt, err := f.r.reconcileNovaMetadataAgentSecrets(context.Background(), f.cp)

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(MatchError(injected))
		g.Expect(err.Error()).To(ContainSubstring(
			`delivering the metadata shared secret into namespace "ovn" on cluster "compute-a"`))
		g.Expect(novaCondition(t, f.cp).Reason).To(Equal(reasonNovaMetadataAgentSecretError))
	})

	t.Run("a foreign same-named Secret is refused", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		// The UID is what an API server stamps on every object; the fake client
		// does not, and a target cluster's claim refuses only an object that has one.
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: novaMetadataAgentSecretName(cp), Namespace: metadataCentralNamespace,
				UID: types.UID("foreign-secret-uid"),
			},
			Data: map[string][]byte{"shared_secret": []byte("theirs")},
		}
		f := newMetadataAgentFleet(t, interceptor.Funcs{}, []client.Object{foreign}, metadataContract(cp, "s3cr3t"))

		_, halt, err := f.r.reconcileNovaMetadataAgentSecrets(context.Background(), f.cp)

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(MatchError(ContainSubstring("refusing to adopt")))
		g.Expect(err.Error()).To(ContainSubstring(`into namespace "ovn" on cluster "compute-a"`))
		g.Expect(novaCondition(t, f.cp).Reason).To(Equal(reasonNovaMetadataAgentSecretError))
		live := &corev1.Secret{}
		g.Expect(f.clusters["compute-a"].Get(context.Background(), client.ObjectKeyFromObject(foreign), live)).
			To(Succeed())
		g.Expect(live.Data).To(Equal(foreign.Data))
		g.Expect(live.Labels).NotTo(HaveKey(neutronv1alpha1.MetadataSharedSecretMirrorLabel))
	})
}

// TestReconcileNovaMetadataAgentSecrets_AFailingClusterHoldsBackNoOther pins a
// cluster that fails ahead of another in the sorted targets: compute-b still
// receives its copy, and NovaReady reports the failure. compute-0, which no
// resolver serves, sorts first; a failed write outranks it.
func TestReconcileNovaMetadataAgentSecrets_AFailingClusterHoldsBackNoOther(t *testing.T) {
	injected := errors.New("injected write failure")
	failingWrites := interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return injected
		},
	}
	for _, tc := range []struct {
		name        string
		computeA    interceptor.Funcs
		unresolved  bool
		wantErr     bool
		wantReason  string
		wantMessage string
	}{
		{
			name:        "an unresolvable cluster",
			unresolved:  true,
			wantReason:  commonmulticluster.TargetClusterUnavailable,
			wantMessage: `cluster "compute-0" not found`,
		},
		{
			name:        "a failed write",
			computeA:    failingWrites,
			wantErr:     true,
			wantReason:  reasonNovaMetadataAgentSecretError,
			wantMessage: injected.Error(),
		},
		{
			name:        "a failed write outranks an unresolvable cluster",
			computeA:    failingWrites,
			unresolved:  true,
			wantErr:     true,
			wantReason:  reasonNovaMetadataAgentSecretError,
			wantMessage: injected.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cp := metadataAgentControlPlane()
			copyName := novaMetadataAgentSecretName(cp)
			objs := []client.Object{
				metadataContract(cp, "s3cr3t"),
				metadataAgent("agent-b", metadataCentralNamespace, "compute-b", copyName),
			}
			if tc.unresolved {
				objs = append(objs, metadataAgent("agent-0", metadataCentralNamespace, "compute-0", copyName))
			}
			f := newMetadataAgentFleet(t, tc.computeA, nil, objs...)

			res, halt, err := f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)

			g.Expect(halt).To(BeTrue())
			if tc.wantErr {
				g.Expect(err).To(MatchError(injected))
				g.Expect(err.Error()).To(ContainSubstring(`on cluster "compute-a"`))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
			}
			cond := novaCondition(t, f.cp)
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantMessage))
			copied := &corev1.Secret{}
			g.Expect(f.clusters["compute-b"].Get(ctx, metadataAgentCopyKey(f.cp), copied)).To(Succeed(),
				"a cluster sorted after the failing one still receives its copy")
			g.Expect(copied.Data).To(Equal(map[string][]byte{"shared_secret": []byte("s3cr3t")}))
		})
	}
}

// TestReconcileNovaMetadataAgentSecrets_ReportsEveryFailingCluster pins a pass on
// which several clusters fail. Every copy has the same namespace and name, so
// each failure names its cluster, and no second write failure or second
// unresolvable cluster is dropped from NovaReady or the returned error.
func TestReconcileNovaMetadataAgentSecrets_ReportsEveryFailingCluster(t *testing.T) {
	failingWrites := func(injected error) interceptor.Funcs {
		return interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return injected
			},
		}
	}

	t.Run("two clusters refuse the write and one does not resolve", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := metadataAgentControlPlane()
		copyName := novaMetadataAgentSecretName(cp)
		refusedA := errors.New("refused on a")
		refusedB := errors.New("refused on b")
		f := newMetadataAgentFleet(t, failingWrites(refusedA), nil, metadataContract(cp, "s3cr3t"),
			metadataAgent("agent-b", metadataCentralNamespace, "compute-b", copyName),
			metadataAgent("agent-0", metadataCentralNamespace, "compute-0", copyName))
		f.clusters["compute-b"] = fake.NewClientBuilder().WithScheme(f.r.Scheme).
			WithInterceptorFuncs(failingWrites(refusedB)).Build()

		_, halt, err := f.r.reconcileNovaMetadataAgentSecrets(context.Background(), f.cp)

		g.Expect(halt).To(BeTrue())
		g.Expect(err).To(MatchError(refusedA))
		g.Expect(err).To(MatchError(refusedB))
		g.Expect(err.Error()).To(And(
			ContainSubstring(`on cluster "compute-a": applying Secret ovn/cp-nova-metadata-agent-secret: refused on a`),
			ContainSubstring(`on cluster "compute-b": applying Secret ovn/cp-nova-metadata-agent-secret: refused on b`)))
		cond := novaCondition(t, f.cp)
		g.Expect(cond.Reason).To(Equal(reasonNovaMetadataAgentSecretError))
		g.Expect(cond.Message).To(And(
			ContainSubstring(`on cluster "compute-0": cluster "compute-0" not found`),
			ContainSubstring(`on cluster "compute-a": applying Secret ovn/cp-nova-metadata-agent-secret: refused on a`),
			ContainSubstring(`on cluster "compute-b": applying Secret ovn/cp-nova-metadata-agent-secret: refused on b`)))
	})

	t.Run("two clusters do not resolve", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctx := context.Background()
		cp := metadataAgentControlPlane()
		copyName := novaMetadataAgentSecretName(cp)
		f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, metadataContract(cp, "s3cr3t"),
			metadataAgent("agent-0", metadataCentralNamespace, "compute-0", copyName),
			metadataAgent("agent-x", metadataCentralNamespace, "compute-x", copyName))

		res, halt, err := f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(halt).To(BeTrue())
		g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
		cond := novaCondition(t, f.cp)
		g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
		g.Expect(cond.Message).To(And(
			ContainSubstring(`on cluster "compute-0": cluster "compute-0" not found`),
			ContainSubstring(`on cluster "compute-x": cluster "compute-x" not found`)))
		g.Expect(f.clusters["compute-a"].Get(ctx, metadataAgentCopyKey(f.cp), &corev1.Secret{})).To(Succeed(),
			"the cluster that resolved still receives its copy")
	})
}

// TestReconcileNovaMetadataAgentSecrets_RotationRewritesTheCopy pins a changed
// contract value: the next pass rewrites the copy.
func TestReconcileNovaMetadataAgentSecrets_RotationRewritesTheCopy(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := metadataAgentControlPlane()
	f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, metadataContract(cp, "first"))
	_, halt, err := f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())

	contract := &corev1.Secret{}
	g.Expect(f.r.Get(ctx, client.ObjectKeyFromObject(metadataContract(cp, "")), contract)).To(Succeed())
	contract.Data[novav1alpha1.ComputeConfigMetadataSharedSecretKey] = []byte("second")
	g.Expect(f.r.Update(ctx, contract)).To(Succeed())

	_, halt, err = f.r.reconcileNovaMetadataAgentSecrets(ctx, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(halt).To(BeFalse())
	copied := &corev1.Secret{}
	g.Expect(f.clusters["compute-a"].Get(ctx, metadataAgentCopyKey(f.cp), copied)).To(Succeed())
	g.Expect(copied.Data).To(Equal(map[string][]byte{"shared_secret": []byte("second")}))
}

// TestReconcileNova_MetadataAgentCopyGatesNovaReady pins the leg's place in
// reconcileNova: with the Nova child Ready and an agent asking for a copy,
// NovaReady waits on the contract's shared secret and turns True only once the
// copy exists on the agent's cluster.
func TestReconcileNova_MetadataAgentCopyGatesNovaReady(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cp := metadataAgentControlPlane()
	f := newMetadataAgentFleet(t, interceptor.Funcs{}, nil, publishedComputeConfig(cp))

	res, err := convergeHVO(t, f.r, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(infraRequeueAfter))
	cond := novaCondition(t, f.cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonWaitingForComputeConfig))
	g.Expect(cond.Message).To(ContainSubstring("carries no metadata_proxy_shared_secret yet"))
	g.Expect(apierrors.IsNotFound(f.clusters["compute-a"].Get(ctx, metadataAgentCopyKey(f.cp),
		&corev1.Secret{}))).To(BeTrue())

	contract := &corev1.Secret{}
	g.Expect(f.r.Get(ctx, client.ObjectKeyFromObject(publishedComputeConfig(cp)), contract)).To(Succeed())
	contract.Data[novav1alpha1.ComputeConfigMetadataSharedSecretKey] = []byte("s3cr3t")
	g.Expect(f.r.Update(ctx, contract)).To(Succeed())

	res, err = f.r.reconcileNova(ctx, f.cp)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	cond = novaCondition(t, f.cp)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("NovaReady"))
	g.Expect(f.clusters["compute-a"].Get(ctx, metadataAgentCopyKey(f.cp), &corev1.Secret{})).To(Succeed())
}
