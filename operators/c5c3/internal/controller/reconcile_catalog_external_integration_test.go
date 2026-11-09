// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Envtest coverage of the External-mode catalog import retirement against a real
// API server. The fake client honors no UID delete precondition, so the stale
// cache read below can only be shown here.
package controller

import (
	"context"
	"testing"

	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v3/api/v1alpha1"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	commonenvtest "github.com/c5c3/cobaltcore/internal/common/testutil/envtest"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	"github.com/c5c3/cobaltcore/operators/c5c3/internal/testutil"
)

// TestRetirePreRegionEndpointImport_StaleReadSparesReimportedCR is the pass that
// follows a re-import before the cache has seen it: an earlier pass deleted the
// region-less public import and its apply re-created the name under the
// regioned filter, but the cache still returns the deleted CR. The pass must
// leave the re-created CR alone, or K-ORC loses the import it may already have
// resolved and CatalogReady drops back to WaitingForCatalog.
func TestRetirePreRegionEndpointImport_StaleReadSparesReimportedCR(t *testing.T) {
	testutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := korcTestScheme(t)

	_, cfg := commonenvtest.StartEnvTestWithConfig(t, s, commonenvtest.CommonFakeCRDDirs())
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: s})
	g.Expect(err).NotTo(HaveOccurred())

	cp := externalCatalogControlPlane()
	public := c5c3v1alpha1.ExternalEndpointTypePublic

	preRegion := preRegionEndpointImport(t, s, cp, public, nil, "")
	preRegion.UID = ""
	g.Expect(c.Create(ctx, preRegion)).To(Succeed())
	cached := preRegion.DeepCopy()
	g.Expect(c.Delete(ctx, preRegion)).To(Succeed())

	reimported := preRegionEndpointImport(t, s, cp, public, nil, "")
	reimported.UID = ""
	reimported.Spec.Import.Filter.RegionRef = ptr.To(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))
	g.Expect(c.Create(ctx, reimported)).To(Succeed())
	key := client.ObjectKeyFromObject(reimported)

	staleCache := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, wc client.WithWatch, k client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if ep, ok := obj.(*orcv1alpha1.Endpoint); ok && k == key {
				cached.DeepCopyInto(ep)
				return nil
			}
			return wc.Get(ctx, k, obj, opts...)
		},
	})
	r := &ControlPlaneReconciler{Client: staleCache, Scheme: s}

	g.Expect(r.retirePreRegionEndpointImport(ctx, cp, key)).To(Succeed())

	live := &orcv1alpha1.Endpoint{}
	g.Expect(c.Get(ctx, key, live)).To(Succeed(), "the re-imported CR must survive a stale read of the deleted one")
	g.Expect(live.UID).To(Equal(reimported.UID))
	g.Expect(live.Spec.Import.Filter.RegionRef).To(HaveValue(Equal(orcv1alpha1.KubernetesNameRef(keystoneRegionName(cp)))))
}
