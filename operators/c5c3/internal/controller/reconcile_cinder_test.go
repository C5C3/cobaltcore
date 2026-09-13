// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the Cinder naming helpers in reconcile_cinder.go and the catalog URL
// they feed (cinderCatalogURL).
package controller

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// cinderControlPlane builds a ControlPlane running the block-storage service on
// one NFS backend, co-located in the ControlPlane's own namespace.
func cinderControlPlane() *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			Services: c5c3v1alpha1.ServicesSpec{
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS:  &c5c3v1alpha1.NFSShareSpec{Server: "nfs.example.com", Path: "/exports/cinder"},
					}},
				},
			},
		},
	}
}

// readyCinderRegistration builds the KeystoneService child the Cinder projection
// gates on, converged: account provisioned (with the ids K-ORC resolved), catalog
// registered, aggregate Ready. A child in a dedicated namespace carries the
// ownership labels, so the projection re-applies it instead of refusing to adopt a
// same-named foreign CR.
func readyCinderRegistration(cp *c5c3v1alpha1.ControlPlane) *c5c3v1alpha1.KeystoneService {
	ks := desiredCinderRegistration(cp)
	if ks.Namespace != cp.Namespace {
		stampControlPlaneChildLabels(ks, cp)
	}
	ks.Status.Account = &c5c3v1alpha1.KeystoneServiceAccountStatus{
		ProjectID: "project-" + ks.Name,
		UserID:    "user-" + ks.Name,
	}
	for _, cond := range []metav1.Condition{{
		Type:    conditionTypeKeystoneServiceAccountReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceAccountProvisioned,
		Message: "account provisioned",
	}, {
		Type:    conditionTypeKeystoneServiceCatalogReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonKeystoneServiceCatalogRegistered,
		Message: "catalog registered",
	}, {
		Type:    conditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: "All sub-conditions are ready",
	}} {
		conditions.SetCondition(&ks.Status.Conditions, cond)
	}
	return ks
}

// TestCinderEndpointURL pins the in-cluster address the catalog's internal row is
// built from: the projected API Service by naming convention, on the cinder
// operator's port, in the namespace the service is assigned to.
func TestCinderEndpointURL(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := cinderControlPlane()
	g.Expect(cinderEndpointURL(cp)).To(Equal("http://cp-cinder.openstack.svc:8776"))

	cp.Spec.Services.Cinder.Namespace = &c5c3v1alpha1.ServiceNamespaceSpec{Name: "block"}
	g.Expect(cinderEndpointURL(cp)).To(Equal("http://cp-cinder.block.svc:8776"),
		"a placed service is reached in the namespace it was assigned")
}

// TestCinderCatalogURL walks the origin precedence of the block-storage catalog
// row and pins the "/v3" suffix on each outcome: an explicit publicEndpoint wins,
// then the gateway hostname, then the in-cluster URL. The suffix is appended
// exactly once whichever origin wins, so no client ever resolves a doubled "/v3".
func TestCinderCatalogURL(t *testing.T) {
	for name, tc := range map[string]struct {
		publicEndpoint string
		gateway        *commonv1.GatewaySpec
		want           string
	}{
		"an explicit publicEndpoint wins, port and all": {
			publicEndpoint: "https://cinder.example.com:8443",
			gateway:        &commonv1.GatewaySpec{Hostname: "cinder.example.com"},
			want:           "https://cinder.example.com:8443/v3",
		},
		"a gateway alone is advertised on the default https port": {
			gateway: &commonv1.GatewaySpec{Hostname: "cinder.example.com"},
			want:    "https://cinder.example.com/v3",
		},
		"without external exposure the in-cluster URL is registered": {
			want: "http://cp-cinder.openstack.svc:8776/v3",
		},
		// The webhook tolerates a single trailing slash on the publicEndpoint, so the
		// origin has to be normalized before the version prefix is joined: an
		// unnormalized join registers "//v3", which every client resolves to a path
		// cinder's routes do not map.
		"a trailing slash on the publicEndpoint is joined once": {
			publicEndpoint: "https://cinder.example.com/",
			want:           "https://cinder.example.com/v3",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			cp.Spec.Services.Cinder.PublicEndpoint = tc.publicEndpoint
			cp.Spec.Services.Cinder.Gateway = tc.gateway

			got := cinderCatalogURL(cp)

			g.Expect(got).To(Equal(tc.want))
			g.Expect(strings.Count(got, "/v3")).To(Equal(1), "the version prefix is appended exactly once")
		})
	}
}
