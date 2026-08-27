// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// testOVNClientSecretName is the Secret the OVNCentral fixture publishes as its
// client identity, the source the mirror is copied from.
const testOVNClientSecretName = "ovn-client"

// The addresses an OVNCentral publishes on node ports, the pair a Neutron on
// another cluster reaches it at.
const (
	testExternalNorthboundAddress = "ssl:192.0.2.10:30641"
	testExternalSouthboundAddress = "ssl:192.0.2.10:30642"
)

// ovnCentralFixture returns the OVNCentral the shared Neutron fixture names,
// publishing both internal addresses and its client Secret.
func ovnCentralFixture() *ovnv1alpha1.OVNCentral {
	return readyOVNCentral(testOVNCentralName, testNamespace,
		testNorthboundAddress, testSouthboundAddress, testOVNClientSecretName)
}

// publishOnNodePorts turns the node-port publication on for both databases and
// stamps the addresses it yields, the pair the endpoint step selects across a
// cluster boundary.
func publishOnNodePorts(central *ovnv1alpha1.OVNCentral, nbAddress, sbAddress string) *ovnv1alpha1.OVNCentral {
	central.Spec.Northbound.ExternallyReachable = true
	central.Spec.Southbound.ExternallyReachable = true
	central.Status.Northbound.DbAddress = nbAddress
	central.Status.Southbound.DbAddress = sbAddress
	return central
}

// placeOn returns a target-cluster ref naming the given cluster.
func placeOn(name string) *commonv1.TargetClusterRefSpec {
	return &commonv1.TargetClusterRefSpec{Name: name}
}

func TestReconcileOVNEndpoints_CentralNotFound(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron)

	resolved, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(resolved.central).To(BeNil())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNCentralNotFound))
	g.Expect(cond.Message).To(ContainSubstring("openstack/ovn"))
}

// TestReconcileOVNEndpoints_ReadErrorPropagatesWrapped covers the difference
// between "not there yet" and "the API server said no": a NotFound polls, any
// other read failure is an error the caller retries with backoff.
func TestReconcileOVNEndpoints_ReadErrorPropagatesWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	boom := errors.New("etcd unavailable")
	c := neutronFakeClientBuilder(neutron, ovnCentralFixture()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isCentral := obj.(*ovnv1alpha1.OVNCentral); isCentral {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	_, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading OVNCentral openstack/ovn:")))
	g.Expect(res.IsZero()).To(BeTrue())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNCentralReadError))
}

// TestReconcileOVNEndpoints_AddressSelection pins which published pair applies.
// Inside one cluster the databases are reached at their Service addresses;
// across a cluster boundary only the node ports are routable, so the two CRs'
// target-cluster refs decide the selection.
func TestReconcileOVNEndpoints_AddressSelection(t *testing.T) {
	tests := []struct {
		name          string
		neutronTarget *commonv1.TargetClusterRefSpec
		centralTarget *commonv1.TargetClusterRefSpec
		wantNB        string
		wantSB        string
	}{
		{
			name:   "both refs nil selects the internal addresses",
			wantNB: testNorthboundAddress,
			wantSB: testSouthboundAddress,
		},
		{
			name:          "the same named cluster selects the internal addresses",
			neutronTarget: placeOn("edge-1"),
			centralTarget: placeOn("edge-1"),
			wantNB:        testNorthboundAddress,
			wantSB:        testSouthboundAddress,
		},
		{
			name:          "different clusters select the node-port addresses",
			neutronTarget: placeOn("edge-1"),
			centralTarget: placeOn("edge-2"),
			wantNB:        testExternalNorthboundAddress,
			wantSB:        testExternalSouthboundAddress,
		},
		{
			name:          "a placed Neutron and a management-cluster central select the node-port addresses",
			neutronTarget: placeOn("edge-1"),
			wantNB:        testExternalNorthboundAddress,
			wantSB:        testExternalSouthboundAddress,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			neutron.Spec.TargetClusterRef = tc.neutronTarget
			central := publishOnNodePorts(ovnCentralFixture(),
				testExternalNorthboundAddress, testExternalSouthboundAddress)
			central.Spec.TargetClusterRef = tc.centralTarget
			r := newNeutronTestReconciler(neutron, central)

			resolved, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.IsZero()).To(BeTrue())
			g.Expect(resolved.nbAddress).To(Equal(tc.wantNB))
			g.Expect(resolved.sbAddress).To(Equal(tc.wantSB))
			g.Expect(resolved.central).NotTo(BeNil())

			cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(conditionReasonOVNEndpointsResolved))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantNB))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantSB))
		})
	}
}

// TestReconcileOVNEndpoints_CrossClusterWithoutNodePortsWaits covers the
// misconfiguration a reader has to be able to act on: the central is reached
// from another cluster, and a database that is not published on node ports has
// no address there. The message names the two fields that publish them.
func TestReconcileOVNEndpoints_CrossClusterWithoutNodePortsWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.TargetClusterRef = placeOn("edge-1")
	central := ovnCentralFixture() // internal addresses only
	central.Spec.TargetClusterRef = placeOn("edge-2")
	r := newNeutronTestReconciler(neutron, central)

	resolved, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(resolved.central).To(BeNil())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNEndpointsPending))
	g.Expect(cond.Message).To(ContainSubstring("spec.northbound.externallyReachable"))
	g.Expect(cond.Message).To(ContainSubstring("spec.southbound.externallyReachable"))
	g.Expect(cond.Message).To(ContainSubstring("target cluster edge-2"))
	g.Expect(cond.Message).To(ContainSubstring("target cluster edge-1"))
}

// TestReconcileOVNEndpoints_PendingArms covers the two halves the driver cannot
// work without, each missing on its own: an unpublished address and an
// unpublished client Secret. Inside one cluster the message stays about the
// central, with no externallyReachable advice that would not apply.
func TestReconcileOVNEndpoints_PendingArms(t *testing.T) {
	tests := []struct {
		name        string
		central     *ovnv1alpha1.OVNCentral
		wantMessage string
	}{
		{
			name: "no Northbound address yet",
			central: readyOVNCentral(testOVNCentralName, testNamespace,
				"", testSouthboundAddress, testOVNClientSecretName),
			wantMessage: "Northbound and Southbound addresses",
		},
		{
			name: "no Southbound address yet",
			central: readyOVNCentral(testOVNCentralName, testNamespace,
				testNorthboundAddress, "", testOVNClientSecretName),
			wantMessage: "Northbound and Southbound addresses",
		},
		{
			name: "no client Secret yet",
			central: readyOVNCentral(testOVNCentralName, testNamespace,
				testNorthboundAddress, testSouthboundAddress, ""),
			wantMessage: "client Secret",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			r := newNeutronTestReconciler(neutron, tc.central)

			_, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))

			cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonOVNEndpointsPending))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantMessage))
			g.Expect(cond.Message).NotTo(ContainSubstring("externallyReachable"),
				"the node-port advice does not apply inside one cluster")
		})
	}
}

// TestReconcileOVNEndpoints_CrossNamespaceRefResolves covers the ordinary
// production layout: the OVN control plane lives in the privileged networking
// namespace while the Neutron API lives with the rest of the control plane.
func TestReconcileOVNEndpoints_CrossNamespaceRefResolves(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.OVN.CentralRef.Namespace = "openstack-networking"
	central := readyOVNCentral(testOVNCentralName, "openstack-networking",
		testNorthboundAddress, testSouthboundAddress, testOVNClientSecretName)
	r := newNeutronTestReconciler(neutron, central)

	resolved, res, err := r.reconcileOVNEndpoints(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(resolved.nbAddress).To(Equal(testNorthboundAddress))
	g.Expect(resolved.central.Namespace).To(Equal("openstack-networking"))
	g.Expect(neutronCondition(neutron, conditionTypeOVNEndpointsReady).Message).
		To(ContainSubstring("openstack-networking/ovn"))
}

// TestReconcileOVNEndpoints_EmptyRefNamespaceFallsBackToTheCR covers the CR that
// bypassed the defaulting webhook: an empty ref namespace resolves in the
// Neutron's own namespace rather than in the empty one.
func TestReconcileOVNEndpoints_EmptyRefNamespaceFallsBackToTheCR(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.OVN.CentralRef.Namespace = ""
	r := newNeutronTestReconciler(neutron, ovnCentralFixture())

	resolved, _, err := r.reconcileOVNEndpoints(context.Background(), neutron)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(resolved.sbAddress).To(Equal(testSouthboundAddress))
}

// resolveEndpoints runs the endpoint step and hands its result to the
// client-Secret step, so the second step is always exercised on the state the
// first one produced.
func resolveEndpoints(t *testing.T, r *NeutronReconciler, neutron *neutronv1alpha1.Neutron) resolvedOVNEndpoints {
	t.Helper()

	resolved, _, err := r.reconcileOVNEndpoints(context.Background(), neutron)
	if err != nil {
		t.Fatalf("resolving the OVN endpoints: %v", err)
	}
	if resolved.central == nil {
		t.Fatal("the OVN endpoints did not resolve")
	}
	return resolved
}

func TestReconcileOVNClientSecret_MirrorsTheClientIdentity(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	neutron := validNeutron()
	r := newNeutronTestReconciler(neutron, ovnCentralFixture(),
		ovnClientSecret(testOVNClientSecretName, testNamespace))
	resolved := resolveEndpoints(t, r, neutron)

	digest, res, err := r.reconcileOVNClientSecret(ctx, r.Client, neutron, resolved)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).NotTo(BeEmpty())

	var mirror corev1.Secret
	key := client.ObjectKey{Namespace: testNamespace, Name: "neutron-ovn-client"}
	g.Expect(key.Name).To(Equal(ovnClientSecretName(neutron)))
	g.Expect(r.Get(ctx, key, &mirror)).To(Succeed())
	g.Expect(mirror.Data).To(HaveLen(3))
	g.Expect(string(mirror.Data["tls.crt"])).To(Equal("client-cert"))
	g.Expect(string(mirror.Data["tls.key"])).To(Equal("client-key"))
	g.Expect(string(mirror.Data["ca.crt"])).To(Equal("ca-bundle"))

	// A local owner is claimed by controller owner reference, so the garbage
	// collection cascade reaps the mirror with the CR.
	g.Expect(mirror.OwnerReferences).To(HaveLen(1))
	g.Expect(mirror.OwnerReferences[0].Kind).To(Equal("Neutron"))
	g.Expect(mirror.OwnerReferences[0].Name).To(Equal(testNeutronName))
	g.Expect(mirror.OwnerReferences[0].Controller).NotTo(BeNil())
	g.Expect(*mirror.OwnerReferences[0].Controller).To(BeTrue())

	// The endpoint step's True condition survives a successful mirror.
	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNEndpointsResolved))

	// A second pass is a no-op and returns the identical digest.
	digest2, _, err := r.reconcileOVNClientSecret(ctx, r.Client, neutron, resolved)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(digest2).To(Equal(digest))
}

// TestReconcileOVNClientSecret_RepairsDrift covers both shapes of a mirror that
// no longer matches its source: a value somebody edited and a key somebody
// added. Either would leave the pods presenting an identity the databases do not
// accept, so the mirror is replaced wholesale.
func TestReconcileOVNClientSecret_RepairsDrift(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "changed value",
			data: map[string][]byte{
				"tls.crt": []byte("stale-cert"),
				"tls.key": []byte("client-key"),
				"ca.crt":  []byte("ca-bundle"),
			},
		},
		{
			name: "extra key",
			data: map[string][]byte{
				"tls.crt": []byte("client-cert"),
				"tls.key": []byte("client-key"),
				"ca.crt":  []byte("ca-bundle"),
				"extra":   []byte("x"),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			neutron := validNeutron()
			drifted := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "neutron-ovn-client", Namespace: testNamespace},
				Data:       tc.data,
			}
			r := newNeutronTestReconciler(neutron, ovnCentralFixture(),
				ovnClientSecret(testOVNClientSecretName, testNamespace), drifted)
			resolved := resolveEndpoints(t, r, neutron)

			_, _, err := r.reconcileOVNClientSecret(ctx, r.Client, neutron, resolved)
			g.Expect(err).NotTo(HaveOccurred())

			var mirror corev1.Secret
			key := client.ObjectKey{Namespace: testNamespace, Name: "neutron-ovn-client"}
			g.Expect(r.Get(ctx, key, &mirror)).To(Succeed())
			g.Expect(mirror.Data).To(HaveLen(3))
			g.Expect(string(mirror.Data["tls.crt"])).To(Equal("client-cert"))
			g.Expect(mirror.Data).NotTo(HaveKey("extra"))
		})
	}
}

// TestReconcileOVNClientSecret_DigestFollowsEveryKey pins what the deployment
// step rolls the pods on: a reissued certificate, a rotated key and a new CA
// each have to move the digest.
func TestReconcileOVNClientSecret_DigestFollowsEveryKey(t *testing.T) {
	digestFor := func(t *testing.T, mutate func(*corev1.Secret)) string {
		t.Helper()
		g := NewGomegaWithT(t)
		neutron := validNeutron()
		source := ovnClientSecret(testOVNClientSecretName, testNamespace)
		mutate(source)
		r := newNeutronTestReconciler(neutron, ovnCentralFixture(), source)
		resolved := resolveEndpoints(t, r, neutron)

		digest, _, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)
		g.Expect(err).NotTo(HaveOccurred())
		return digest
	}

	g := NewGomegaWithT(t)
	base := digestFor(t, func(*corev1.Secret) {})
	for _, key := range ovnClientSecretKeys {
		rotated := digestFor(t, func(s *corev1.Secret) { s.Data[key] = []byte("rotated-" + key) })
		g.Expect(rotated).NotTo(Equal(base), "a changed %s must move the digest", key)
	}
}

func TestReconcileOVNClientSecret_SourceNotFoundWaits(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	// The central publishes the Secret name before cert-manager has issued it.
	r := newNeutronTestReconciler(neutron, ovnCentralFixture())
	resolved := resolveEndpoints(t, r, neutron)

	digest, res, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNClientSecretPending))
	g.Expect(cond.Message).To(ContainSubstring("openstack/ovn-client"))
}

// TestReconcileOVNClientSecret_IncompleteSourceWaits covers the half-written
// source: cert-manager creates the Secret before every key is populated, and the
// message names the first key that is still missing.
func TestReconcileOVNClientSecret_IncompleteSourceWaits(t *testing.T) {
	tests := []struct {
		name    string
		drop    []string
		wantKey string
	}{
		{name: "no certificate", drop: []string{"tls.crt"}, wantKey: "tls.crt"},
		{name: "no private key", drop: []string{"tls.key"}, wantKey: "tls.key"},
		{name: "no CA bundle", drop: []string{"ca.crt"}, wantKey: "ca.crt"},
		{name: "keypair missing reports the certificate first", drop: []string{"tls.crt", "tls.key"}, wantKey: "tls.crt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			neutron := validNeutron()
			source := ovnClientSecret(testOVNClientSecretName, testNamespace)
			for _, key := range tc.drop {
				delete(source.Data, key)
			}
			r := newNeutronTestReconciler(neutron, ovnCentralFixture(), source)
			resolved := resolveEndpoints(t, r, neutron)

			digest, res, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
			g.Expect(digest).To(BeEmpty())

			cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(conditionReasonOVNClientSecretIncomplete))
			g.Expect(cond.Message).To(ContainSubstring(tc.wantKey))

			var mirror corev1.Secret
			key := client.ObjectKey{Namespace: testNamespace, Name: "neutron-ovn-client"}
			g.Expect(r.Get(context.Background(), key, &mirror)).NotTo(Succeed(),
				"no partial identity is mirrored")
		})
	}
}

// TestReconcileOVNClientSecret_UnresolvableCentralCluster covers the central
// that projects onto a cluster this operator cannot reach: its client Secret is
// unreadable, so the endpoints it published are not usable either.
func TestReconcileOVNClientSecret_UnresolvableCentralCluster(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()
	neutron.Spec.TargetClusterRef = placeOn("edge-1")
	central := publishOnNodePorts(ovnCentralFixture(),
		testExternalNorthboundAddress, testExternalSouthboundAddress)
	central.Spec.TargetClusterRef = placeOn("edge-2")
	r := newNeutronTestReconciler(neutron, central)
	resolved := resolveEndpoints(t, r, neutron)
	r.Resolver = unresolvableResolver{}

	digest, res, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	g.Expect(digest).To(BeEmpty())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(commonmulticluster.TargetClusterUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("cluster not found"))
}

// TestReconcileOVNClientSecret_SourceReadErrorPropagatesWrapped covers the read
// failure that is not a NotFound: it is an error the caller retries with
// backoff, not a wait that looks like a healthy first install.
func TestReconcileOVNClientSecret_SourceReadErrorPropagatesWrapped(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	boom := errors.New("etcd unavailable")
	sourceKey := client.ObjectKey{Namespace: testNamespace, Name: testOVNClientSecretName}
	c := neutronFakeClientBuilder(neutron, ovnCentralFixture(),
		ovnClientSecret(testOVNClientSecretName, testNamespace)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret && key == sourceKey {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}
	resolved := resolveEndpoints(t, r, neutron)

	digest, res, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring("reading OVN client Secret openstack/ovn-client:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(digest).To(BeEmpty())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNClientSecretReadError))
}

// TestReconcileOVNClientSecret_MirrorWriteFailurePropagates covers the write
// half: a Create the API server refuses must surface as a wrapped error and
// overwrite the endpoint step's True condition, so the aggregate Ready cannot
// stay stale-True while the pods have no identity to mount.
func TestReconcileOVNClientSecret_MirrorWriteFailurePropagates(t *testing.T) {
	g := NewGomegaWithT(t)
	neutron := validNeutron()

	boom := errors.New("admission webhook rejected the Secret")
	c := neutronFakeClientBuilder(neutron, ovnCentralFixture(),
		ovnClientSecret(testOVNClientSecretName, testNamespace)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &NeutronReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}
	resolved := resolveEndpoints(t, r, neutron)

	digest, _, err := r.reconcileOVNClientSecret(context.Background(), r.Client, neutron, resolved)

	g.Expect(err).To(MatchError(boom), "the client error must stay unwrappable")
	g.Expect(err).To(MatchError(ContainSubstring(
		"creating mirrored OVN client Secret openstack/neutron-ovn-client:")))
	g.Expect(digest).To(BeEmpty())

	cond := neutronCondition(neutron, conditionTypeOVNEndpointsReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonOVNClientSecretMirrorFailed))
}

// TestSameTargetCluster pins the placement comparison the address selection
// rests on: two nil refs both mean the management cluster.
func TestSameTargetCluster(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(sameTargetCluster(nil, nil)).To(BeTrue())
	g.Expect(sameTargetCluster(placeOn("edge-1"), placeOn("edge-1"))).To(BeTrue())
	g.Expect(sameTargetCluster(placeOn("edge-1"), placeOn("edge-2"))).To(BeFalse())
	g.Expect(sameTargetCluster(placeOn("edge-1"), nil)).To(BeFalse())
	g.Expect(sameTargetCluster(nil, placeOn("edge-1"))).To(BeFalse())

	g.Expect(describeTargetCluster(nil)).To(Equal("the management cluster"))
	g.Expect(describeTargetCluster(placeOn("edge-1"))).To(Equal("target cluster edge-1"))
}
