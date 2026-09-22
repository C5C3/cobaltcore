// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// The projected Nova CR is named "{controlplane.Name}-nova", the same
// deterministic, collision-free naming convention as the Keystone, Horizon,
// Glance, Placement, Barbican, Neutron, and Cinder children (see
// keystoneNameSuffix), and lives in cp.NovaNamespace(): the ControlPlane's own
// namespace by default, or the one services.nova.namespace assigns.

// novaNameSuffix is appended to the ControlPlane name to derive the name of the
// projected Nova CR (and, through it, its credential and registration objects).
const novaNameSuffix = "-nova"

// novaAPIPort is the port the nova operator's API Service listens on.
const novaAPIPort int32 = 8774

// novaName returns the name of the Nova CR the reconciler projects for the given
// ControlPlane (see novaNameSuffix).
func novaName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + novaNameSuffix
}

// novaEndpointURL renders the in-cluster URL of the projected Nova API Service
// by naming convention, the cross-service endpoint contract the catalog
// registers against. It carries no path: the catalog rows append the "/v2.1" the
// compute API is served under (see novaCatalogURL).
func novaEndpointURL(cp *c5c3v1alpha1.ControlPlane) string {
	return managedServiceURL(novaName(cp), cp.NovaNamespace(), novaAPIPort, "")
}
