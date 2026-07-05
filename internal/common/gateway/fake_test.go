// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// newGatewayScheme returns a scheme with the Gateway API types registered.
func newGatewayScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := gatewayv1.Install(s); err != nil {
		return nil, err
	}
	return s, nil
}

// newFakeClient builds a fake client over the given scheme.
func newFakeClient(scheme *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}
