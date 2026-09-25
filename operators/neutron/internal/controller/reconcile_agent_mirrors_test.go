// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
)

// The coordinates of the reap tests: the agents live in the OVN central's
// namespace and run on compute-a, where the ControlPlane delivered the copy.
const (
	reapNamespace  = "ovn"
	reapCluster    = "compute-a"
	reapMirrorName = "cp-nova-metadata-agent-secret"
)

// reapAgent builds an agent in reapNamespace on the named cluster that signs
// with the named Secret.
func reapAgent(name, cluster, sharedSecret string) *neutronv1alpha1.NeutronMetadataAgent {
	cr := agentFor(name, reapNamespace, testOVNChassisName)
	cr.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: cluster}
	cr.Spec.NovaMetadata = &neutronv1alpha1.NovaMetadataSpec{
		Protocol:        "https",
		SharedSecretRef: &commonv1.SecretRefSpec{Name: sharedSecret, Key: "shared_secret"},
	}
	return cr
}

// terminating marks cr as being deleted. The finalizer keeps the fake client
// from refusing an object created with a deletion timestamp.
func terminating(cr *neutronv1alpha1.NeutronMetadataAgent) *neutronv1alpha1.NeutronMetadataAgent {
	deletedAt := metav1.NewTime(time.Now())
	cr.DeletionTimestamp = &deletedAt
	cr.Finalizers = []string{commonmulticluster.RemoteChildrenFinalizer}
	return cr
}

// metadataSecret builds a Secret in reapNamespace, labelled as a ControlPlane
// copy when mirror is set.
func metadataSecret(name string, mirror bool) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: reapNamespace},
		Data:       map[string][]byte{"shared_secret": []byte("s3cr3t")},
	}
	if mirror {
		secret.Labels = map[string]string{neutronv1alpha1.MetadataSharedSecretMirrorLabel: "true"}
	}
	return secret
}

// reapEvents returns the MetadataSharedSecretMirrorReaped events recorded so far.
func reapEvents(r *NeutronMetadataAgentReconciler) []string {
	var reaped []string
	for _, e := range collectEvents(r.Recorder.(*record.FakeRecorder)) {
		if strings.Contains(e, eventReasonMetadataSharedSecretMirrorReaped) {
			reaped = append(reaped, e)
		}
	}
	return reaped
}

// A copy is reaped once no other live agent on the same cluster names it. What
// holds it is exactly that: an agent in the namespace, not the terminating one,
// not being deleted itself, and placed on the same cluster.
func TestReapMetadataSharedSecretMirrors_DeletesWhatNoOtherAgentHere(t *testing.T) {
	tests := []struct {
		name       string
		cr         *neutronv1alpha1.NeutronMetadataAgent
		siblings   []client.Object
		wantReaped bool
	}{
		{
			name:       "the only agent on the cluster reaps the copy",
			cr:         reapAgent("agent-a", reapCluster, reapMirrorName),
			wantReaped: true,
		},
		{
			name:     "a live sibling on the same cluster keeps it",
			cr:       reapAgent("agent-a", reapCluster, reapMirrorName),
			siblings: []client.Object{reapAgent("agent-b", reapCluster, reapMirrorName)},
		},
		{
			name:       "a live sibling on another cluster does not hold it",
			cr:         reapAgent("agent-a", reapCluster, reapMirrorName),
			siblings:   []client.Object{reapAgent("agent-b", "compute-b", reapMirrorName)},
			wantReaped: true,
		},
		{
			name:       "a sibling that is being deleted too does not hold it",
			cr:         reapAgent("agent-a", reapCluster, reapMirrorName),
			siblings:   []client.Object{terminating(reapAgent("agent-b", reapCluster, reapMirrorName))},
			wantReaped: true,
		},
		{
			name:       "a local sibling does not hold a copy on a target cluster",
			cr:         reapAgent("agent-a", reapCluster, reapMirrorName),
			siblings:   []client.Object{localSibling()},
			wantReaped: true,
		},
		// The terminating agent was re-pointed away from the copy earlier; the
		// copy nobody names any more goes with this teardown.
		{
			name:       "a copy no remaining agent names is reaped whatever the terminating agent names",
			cr:         reapAgent("agent-a", reapCluster, "hand-made-secret"),
			siblings:   []client.Object{reapAgent("agent-b", reapCluster, "hand-made-secret")},
			wantReaped: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := terminating(tc.cr)
			r := newAgentTestReconciler(append([]client.Object{cr}, tc.siblings...)...)
			target := neutronFakeClientBuilder(metadataSecret(reapMirrorName, true)).Build()

			g.Expect(r.reapMetadataSharedSecretMirrors(ctx, commonmulticluster.Remote(target), cr)).To(Succeed())

			err := target.Get(ctx, client.ObjectKey{Namespace: reapNamespace, Name: reapMirrorName}, &corev1.Secret{})
			if !tc.wantReaped {
				g.Expect(err).NotTo(HaveOccurred(), "the copy a live sibling names must survive")
				g.Expect(reapEvents(r)).To(BeEmpty())
				return
			}
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the copy must be reaped")
			events := reapEvents(r)
			g.Expect(events).To(HaveLen(1))
			g.Expect(events[0]).To(HavePrefix(corev1.EventTypeNormal))
			g.Expect(events[0]).To(ContainSubstring(
				"Deleted the metadata shared-secret mirror " + reapMirrorName +
					": no other NeutronMetadataAgent on this cluster names it"))
		})
	}
}

// One pass decides every labelled copy on its own: a copy a live sibling names
// does not stop the reap of one nobody names, and each deletion records its own
// event. The held copy lists first, so a pass that stopped at it would reap
// nothing.
func TestReapMetadataSharedSecretMirrors_DecidesEachCopy(t *testing.T) {
	const staleMirrorName = "other-nova-metadata-agent-secret"
	tests := []struct {
		name       string
		siblings   []client.Object
		wantKept   []string
		wantReaped []string
	}{
		{
			name:       "a held copy survives beside an unused one",
			siblings:   []client.Object{reapAgent("agent-b", reapCluster, reapMirrorName)},
			wantKept:   []string{reapMirrorName},
			wantReaped: []string{staleMirrorName},
		},
		{
			name:       "two unused copies both go",
			wantReaped: []string{reapMirrorName, staleMirrorName},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := terminating(reapAgent("agent-a", reapCluster, reapMirrorName))
			r := newAgentTestReconciler(append([]client.Object{cr}, tc.siblings...)...)
			target := neutronFakeClientBuilder(
				metadataSecret(reapMirrorName, true), metadataSecret(staleMirrorName, true)).Build()

			g.Expect(r.reapMetadataSharedSecretMirrors(ctx, commonmulticluster.Remote(target), cr)).To(Succeed())

			for _, name := range tc.wantKept {
				g.Expect(target.Get(ctx, client.ObjectKey{Namespace: reapNamespace, Name: name}, &corev1.Secret{})).
					To(Succeed(), "the copy a live sibling names must survive")
			}
			for _, name := range tc.wantReaped {
				err := target.Get(ctx, client.ObjectKey{Namespace: reapNamespace, Name: name}, &corev1.Secret{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s must be reaped", name)
			}
			events := reapEvents(r)
			g.Expect(events).To(HaveLen(len(tc.wantReaped)))
			for _, name := range tc.wantReaped {
				g.Expect(events).To(ContainElement(ContainSubstring("Deleted the metadata shared-secret mirror " + name + ":")))
			}
		})
	}
}

// localSibling is an agent in the same namespace that keeps its children on the
// management cluster and names the copy.
func localSibling() *neutronv1alpha1.NeutronMetadataAgent {
	sibling := reapAgent("agent-local", reapCluster, reapMirrorName)
	sibling.Spec.TargetClusterRef = nil
	return sibling
}

// A Secret without the label is somebody else's, even under the copy's name:
// the reap never deletes it, and without a labelled Secret it deletes nothing.
func TestReapMetadataSharedSecretMirrors_LeavesUnlabelledSecretsAlone(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := terminating(reapAgent("agent-a", reapCluster, reapMirrorName))
	r := newAgentTestReconciler(cr)
	deletes := 0
	target := neutronFakeClientBuilder(metadataSecret(reapMirrorName, false)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()

	g.Expect(r.reapMetadataSharedSecretMirrors(ctx, commonmulticluster.Remote(target), cr)).To(Succeed())

	g.Expect(target.Get(ctx, client.ObjectKey{Namespace: reapNamespace, Name: reapMirrorName}, &corev1.Secret{})).
		To(Succeed())
	g.Expect(deletes).To(BeZero())
	g.Expect(reapEvents(r)).To(BeEmpty())
}

// Each failure of the reap is returned wrapped, so the deletion pass fails and
// keeps the finalizer for the next one. A copy already gone is no failure.
func TestReapMetadataSharedSecretMirrors_Errors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		mgmt    interceptor.Funcs
		target  interceptor.Funcs
		wantErr string
	}{
		{
			name: "a Secret list error",
			target: interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return boom
				},
			},
			wantErr: `listing metadata shared-secret mirrors in namespace "ovn": boom`,
		},
		{
			name: "an agent list error",
			mgmt: interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return boom
				},
			},
			wantErr: `listing NeutronMetadataAgents in namespace "ovn": boom`,
		},
		{
			name: "a Delete error",
			target: interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return boom
				},
			},
			wantErr: "deleting metadata shared-secret mirror ovn/" + reapMirrorName + ": boom",
		},
		{
			name: "a copy deleted in between is no error",
			target: interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return apierrors.NewNotFound(corev1.Resource("secrets"), reapMirrorName)
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			ctx := context.Background()
			cr := terminating(reapAgent("agent-a", reapCluster, reapMirrorName))
			r := &NeutronMetadataAgentReconciler{
				Client:   neutronFakeClientBuilder(cr).WithInterceptorFuncs(tc.mgmt).Build(),
				Scheme:   testScheme(),
				Recorder: record.NewFakeRecorder(10),
			}
			target := neutronFakeClientBuilder(metadataSecret(reapMirrorName, true)).
				WithInterceptorFuncs(tc.target).Build()

			err := r.reapMetadataSharedSecretMirrors(ctx, commonmulticluster.Remote(target), cr)

			if tc.wantErr == "" {
				g.Expect(err).NotTo(HaveOccurred())
			} else {
				g.Expect(err).To(MatchError(tc.wantErr))
				g.Expect(errors.Is(err, boom)).To(BeTrue(), "the cause stays in the chain")
			}
			g.Expect(reapEvents(r)).To(BeEmpty(), "no copy was deleted by this pass")
		})
	}
}
