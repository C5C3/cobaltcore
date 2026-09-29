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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// watchNamespace is the namespace the mapper fixtures live in, apart from the
// shared test namespace so a lookup that ignored it would find nothing.
const watchNamespace = "ovn"

// watchCentral is an OVNCentral in namespace ns that publishes clientSecret as
// its client identity, on the given target cluster (nil for the management
// cluster).
func watchCentral(ns, name, clientSecret string, target *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNCentral {
	return &ovnv1alpha1.OVNCentral{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       ovnv1alpha1.OVNCentralSpec{TargetClusterRef: target},
		Status:     ovnv1alpha1.OVNCentralStatus{ClientSecretName: clientSecret},
	}
}

// watchChassis is an OVNChassis in namespace ns attached to central, on the
// given target cluster (nil for the management cluster).
func watchChassis(ns, name, central string, target *commonv1.TargetClusterRefSpec) *ovnv1alpha1.OVNChassis {
	return &ovnv1alpha1.OVNChassis{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ovnv1alpha1.OVNChassisSpec{
			CentralRef:       ovnv1alpha1.OVNCentralRef{Name: central},
			TargetClusterRef: target,
		},
	}
}

// interceptedChassisClient is indexedChassisClient with funcs intercepting its
// calls.
func interceptedChassisClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()

	return ovnChassisFakeClientBuilder(t, objs...).
		WithIndex(&ovnv1alpha1.OVNChassis{}, OVNChassisCentralRefIndexKey, ovnChassisCentralRefExtractor).
		WithInterceptorFuncs(funcs).
		Build()
}

func secretNamed(ns, name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func requestFor(ns, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func TestClientSecretToChassisMapper(t *testing.T) {
	ctx := context.Background()
	objs := []client.Object{
		watchCentral(watchNamespace, "ovn-a", "ovn-a-client", nil),
		watchCentral(watchNamespace, "ovn-b", "ovn-b-client", nil),
		// The same Secret name in another namespace is another Secret.
		watchCentral("elsewhere", "ovn-a", "ovn-a-client", nil),
		watchChassis(watchNamespace, "compute-1", "ovn-a", nil),
		watchChassis(watchNamespace, "compute-2", "ovn-a", &commonv1.TargetClusterRefSpec{Name: "edge-1"}),
		watchChassis(watchNamespace, "compute-3", "ovn-b", nil),
		watchChassis("elsewhere", "compute-4", "ovn-a", nil),
	}

	t.Run("a published Secret maps to every chassis of its central, once each", func(t *testing.T) {
		g := NewGomegaWithT(t)
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{}, objs...))

		requests := mapper(ctx, secretNamed(watchNamespace, "ovn-a-client"))

		g.Expect(requests).To(ConsistOf(
			requestFor(watchNamespace, "compute-1"),
			requestFor(watchNamespace, "compute-2"),
		))
	})

	t.Run("two centrals publishing the same Secret request each chassis once", func(t *testing.T) {
		g := NewGomegaWithT(t)
		shared := append([]client.Object{
			watchCentral(watchNamespace, "ovn-c", "ovn-a-client", nil),
			watchChassis(watchNamespace, "compute-5", "ovn-c", nil),
		}, objs...)
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{}, shared...))

		requests := mapper(ctx, secretNamed(watchNamespace, "ovn-a-client"))

		g.Expect(requests).To(ConsistOf(
			requestFor(watchNamespace, "compute-1"),
			requestFor(watchNamespace, "compute-2"),
			requestFor(watchNamespace, "compute-5"),
		))
	})

	t.Run("a Secret no central publishes maps to nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{}, objs...))

		g.Expect(mapper(ctx, secretNamed(watchNamespace, "unrelated"))).To(BeEmpty())
	})

	t.Run("an object that is not a Secret maps to nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{}, objs...))

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ovn-a-client", Namespace: watchNamespace}}
		g.Expect(mapper(ctx, cm)).To(BeEmpty())
	})

	t.Run("a failed central list maps to nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*ovnv1alpha1.OVNCentralList); ok {
					return errors.New("list refused")
				}
				return cl.List(ctx, list, opts...)
			},
		}, objs...))

		g.Expect(mapper(ctx, secretNamed(watchNamespace, "ovn-a-client"))).To(BeEmpty())
	})

	t.Run("a failed chassis list returns what was collected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		shared := append([]client.Object{
			watchCentral(watchNamespace, "ovn-c", "ovn-a-client", nil),
			watchChassis(watchNamespace, "compute-5", "ovn-c", nil),
		}, objs...)
		chassisLists := 0
		mapper := clientSecretToChassisMapper(interceptedChassisClient(t, interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*ovnv1alpha1.OVNChassisList); ok {
					chassisLists++
					if chassisLists == 2 {
						return errors.New("list refused")
					}
				}
				return cl.List(ctx, list, opts...)
			},
		}, shared...))

		requests := mapper(ctx, secretNamed(watchNamespace, "ovn-a-client"))

		// The centrals come back in name order, so the first list is ovn-a's
		// and the refused one is ovn-c's.
		g.Expect(requests).To(ConsistOf(
			requestFor(watchNamespace, "compute-1"),
			requestFor(watchNamespace, "compute-2"),
		))
	})
}

func TestChassisTargetClusters(t *testing.T) {
	ctx := context.Background()
	edge := &commonv1.TargetClusterRefSpec{Name: "edge-1"}
	home := &commonv1.TargetClusterRefSpec{Name: "home-1"}
	key := types.NamespacedName{Namespace: watchNamespace, Name: "compute-1"}

	for _, tc := range []struct {
		name          string
		chassis       *commonv1.TargetClusterRefSpec
		central       *commonv1.TargetClusterRefSpec
		centralExists bool
		want          []string
	}{
		{"a placed chassis and a placed central", edge, home, true, []string{"edge-1", "home-1"}},
		{"a local chassis and a placed central", nil, home, true, []string{"home-1"}},
		{"a placed chassis and a local central", edge, nil, true, []string{"edge-1"}},
		{"both on the management cluster", nil, nil, true, nil},
		{"a placed chassis whose central does not exist", edge, nil, false, []string{"edge-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			objs := []client.Object{watchChassis(watchNamespace, "compute-1", "ovn-a", tc.chassis)}
			if tc.centralExists {
				objs = append(objs, watchCentral(watchNamespace, "ovn-a", "ovn-a-client", tc.central))
			}
			targets := chassisTargetClusters(interceptedChassisClient(t, interceptor.Funcs{}, objs...))

			names, err := targets(ctx, key)

			g.Expect(err).NotTo(HaveOccurred())
			if tc.want == nil {
				g.Expect(names).To(BeEmpty(), "a pair on the management cluster keeps no remote event")
			} else {
				g.Expect(names).To(Equal(tc.want))
			}
		})
	}

	t.Run("a missing chassis returns NotFound unwrapped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		targets := chassisTargetClusters(interceptedChassisClient(t, interceptor.Funcs{}))

		_, err := targets(ctx, key)

		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"RemoteRequestsAmong keeps a NotFound silent only when it can recognize it")
	})

	t.Run("a failed central read is wrapped with the chassis", func(t *testing.T) {
		g := NewGomegaWithT(t)
		readErr := errors.New("read refused")
		targets := chassisTargetClusters(interceptedChassisClient(t, interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*ovnv1alpha1.OVNCentral); ok {
					return readErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}, watchChassis(watchNamespace, "compute-1", "ovn-a", edge)))

		_, err := targets(ctx, key)

		g.Expect(err).To(MatchError("reading the OVNCentral of OVNChassis ovn/compute-1: read refused"))
		g.Expect(errors.Is(err, readErr)).To(BeTrue())
	})
}
