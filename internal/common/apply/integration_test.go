// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package apply

import (
	"testing"

	. "github.com/onsi/gomega"

	envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func integrationDeployment(ns string) *appsv1.Deployment {
	labels := map[string]string{"app": "ssa-demo"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ssa-demo", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "busybox:latest"}},
				},
			},
		},
	}
}

// TestIntegration_EnsureObject_convergesWithoutRewrite verifies that re-applying
// an unchanged desired object is a no-op: the API server detects no field-level
// change under the operator's field manager and leaves the resourceVersion (and
// thus generation) untouched. This is the convergence guarantee the Update-based
// helpers lacked.
func TestIntegration_EnsureObject_convergesWithoutRewrite(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-apply-converge"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	g.Expect(EnsureObject(ctx, c, scheme, owner, integrationDeployment(ns.Name), FieldManager)).To(Succeed())

	first := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "ssa-demo", Namespace: ns.Name}, first)).To(Succeed())
	g.Expect(first.OwnerReferences).To(HaveLen(1))

	// Apply the identical desired object again.
	g.Expect(EnsureObject(ctx, c, scheme, owner, integrationDeployment(ns.Name), FieldManager)).To(Succeed())

	second := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "ssa-demo", Namespace: ns.Name}, second)).To(Succeed())
	g.Expect(second.ResourceVersion).To(Equal(first.ResourceVersion),
		"re-applying an unchanged object must not rewrite it")
	g.Expect(second.Generation).To(Equal(first.Generation))
}

// TestIntegration_EnsureObject_takesOverFromUpdate verifies that EnsureObject can
// adopt an object first written with a plain Create/Update (whole-object
// ownership). ForceOwnership lets the apply field manager take over the fields it
// sets without surfacing a conflict — the migration path from the old Update
// helpers to SSA.
func TestIntegration_EnsureObject_takesOverFromUpdate(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	g := NewGomegaWithT(t)

	c, ctx, _ := envtestutil.SetupEnvTest(t)
	scheme := envtestutil.SharedScheme()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-apply-takeover"}}
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns.Name}}
	g.Expect(c.Create(ctx, owner)).To(Succeed())

	// Pre-create with a plain client Create (default field manager), mimicking an
	// object originally written by the Update-based helper.
	pre := integrationDeployment(ns.Name)
	g.Expect(c.Create(ctx, pre)).To(Succeed())

	// Now adopt it via SSA: should succeed without a conflict error.
	desired := integrationDeployment(ns.Name)
	desired.Spec.Template.Spec.Containers[0].Image = "busybox:1.36"
	g.Expect(EnsureObject(ctx, c, scheme, owner, desired, FieldManager)).To(Succeed())

	fetched := &appsv1.Deployment{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "ssa-demo", Namespace: ns.Name}, fetched)).To(Succeed())
	g.Expect(fetched.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:1.36"))
	g.Expect(fetched.OwnerReferences).To(HaveLen(1))
}
