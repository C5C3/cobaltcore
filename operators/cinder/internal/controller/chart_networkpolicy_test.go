// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/c5c3/cobaltcore/internal/common/naming"
	"github.com/c5c3/cobaltcore/internal/common/testutil"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
)

// TestChartNetworkPolicy_AdmitsAPIHealthCheck guards the egress rule the
// chart's opt-in NetworkPolicy hard-codes for the health check: it must admit
// the target port of the API Service on the pods labelled the way this operator
// labels its API pods, since an enforcing CNI judges the probe after the Service
// DNAT.
func TestChartNetworkPolicy_AdmitsAPIHealthCheck(t *testing.T) {
	g := NewGomegaWithT(t)
	cinder := &cinderv1alpha1.Cinder{ObjectMeta: metav1.ObjectMeta{Name: "cinder", Namespace: "openstack"}}
	apiPod := componentLabels(cinder, naming.ComponentAPI)
	svc := buildCinderService(cinder)
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	port := svc.Spec.Ports[0].TargetPort.IntVal

	g.Expect(testutil.ChartEgressAdmits(t, filepath.Join("..", "..", "helm", "cinder-operator"), apiPod, port)).To(BeTrue(),
		"the chart NetworkPolicy must admit TCP %d to the pods labelled %v", port, apiPod)
}
