// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the credential-confinement invariant the minted
// admin application credential must be restricted, and the minted APP-CREDENTIAL
// Secret must never leak into service workloads — only the c5c3-operator and
// K-ORC may read it.
//
// DECISION (mapping to c5c3's CR-projection model):
// is classically phrased against Pod templates ("the app-credential
// Secret must not appear in any service Deployment/StatefulSet/DaemonSet env or
// volume"). c5c3, however, does NOT create Deployments/StatefulSets/DaemonSets
// directly: it PROJECTS child CRs (Keystone, MariaDB, Memcached, and the K-ORC
// ApplicationCredential/Service/Endpoint), and those downstream operators own the
// actual workloads. The invariant is therefore mapped onto c5c3's PROJECTION
// BOUNDARY — the only place c5c3 can leak the credential is into an object it
// itself writes:
//
//  1. The minted ApplicationCredential MUST be restricted
//     (Spec.Resource.Unrestricted == false).
//  2. The app-credential Secret name MUST NOT appear in the projected Keystone CR
//     spec. The Keystone CR legitimately references the admin PASSWORD Secret for
//     bootstrap; that is DISTINCT and allowed — only the minted app-credential
//     Secret name must be absent.
//  3. The app-credential Secret MUST be referenced ONLY by the PushSecret (which
//     mirrors it to OpenBao) and by the AC's Resource.SecretRef (where K-ORC
//     writes it). The credential thus flows operator -> OpenBao -> (ESO) -> K-ORC,
//     never directly into a service CR.
//  4. A generic Pod-template walker over the appsv1 kinds is run for
//     future-proofing: c5c3 creates ZERO such workloads today, so the walk finds
//     none; if a future change starts emitting a Deployment/StatefulSet/DaemonSet,
//     the walk catches an app-credential / "keystone-admin" leak into its env or
//     volumes.
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	esov1alpha1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha1"
	orcv1alpha1 "github.com/k-orc/openstack-resource-controller/v2/api/v1alpha1"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/c5c3/forge/internal/common/conditions"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
)

// invariantScheme registers every type the credential chain reads or writes,
// including keystone (so the projected Keystone CR can be inspected) and appsv1
// (so the future-proofing Pod-template walker can list workloads).
func invariantScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := korcTestScheme(t)
	if err := keystonev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding keystone scheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("adding apps/v1 scheme: %v", err)
	}
	return s
}

// invariantControlPlane builds a representative restricted-credential ControlPlane
// with managed infrastructure and InfrastructureReady pre-set so the Keystone
// projection gate passes.
func invariantControlPlane() *c5c3v1alpha1.ControlPlane {
	cp := korcControlPlane()
	cp.Spec.KORC.AdminCredential.ApplicationCredential.Restricted = ptr.To(true)
	cp.Spec.Infrastructure = &c5c3v1alpha1.InfrastructureSpec{
		Database: commonv1.DatabaseSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "openstack-db"},
			Database:   "keystone",
			SecretRef:  commonv1.SecretRefSpec{Name: "keystone-db"},
		},
		Cache: commonv1.CacheSpec{
			ClusterRef: &corev1.LocalObjectReference{Name: "openstack-memcached"},
			Backend:    "dogpile.cache.pymemcache",
			Replicas:   3,
		},
	}
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeInfrastructureReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cp.Generation,
		Reason:             "InfrastructureReady",
		Message:            "ready",
	})
	return cp
}

// invariantAdminPasswordSecret returns the admin-password Secret the managed
// invariantControlPlane's credential chain reads. invariantControlPlane is MANAGED
// (Database.ClusterRef != nil), so the effective admin-password ref is the operator-owned per-ControlPlane Secret adminPasswordSecretName(cp)
// — NOT the cp-level "keystone-admin" of the brownfield adminPasswordSecret() helper.
// readAdminPassword/computeAdminPasswordHash resolve this Secret to the cleartext.
func invariantAdminPasswordSecret(cp *c5c3v1alpha1.ControlPlane) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminPasswordSecretName(cp), Namespace: cp.Namespace},
		Data:       map[string][]byte{"password": []byte(testAdminPassword)},
	}
}

// availableAC pre-seeds an Available ApplicationCredential whose stamped password
// hash already matches, so reconcileKORC flips KORCReady=True on its single pass
// (a missing/mismatched hash would instead trigger a delete+recreate re-mint).
func availableAC(cp *c5c3v1alpha1.ControlPlane) *orcv1alpha1.ApplicationCredential {
	return &orcv1alpha1.ApplicationCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:        adminAppCredentialName(cp),
			Namespace:   childNamespace(cp),
			Annotations: map[string]string{adminPasswordHashAnnotation: testPasswordHash()},
		},
		Status: orcv1alpha1.ApplicationCredentialStatus{
			ID: ptr.To("ac-id-invariant"),
			Conditions: []metav1.Condition{{
				Type:               orcv1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             orcv1alpha1.ConditionReasonSuccess,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// driveCredentialChain runs the credential-minting sub-reconcilers in dependency
// order (KORC -> AdminCredential -> Catalog -> Keystone) against the one fake
// client so the asserts below see the full set of objects the operator created.
func driveCredentialChain(t *testing.T, r *ControlPlaneReconciler, cp *c5c3v1alpha1.ControlPlane) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.reconcileKORC(ctx, cp); err != nil {
		t.Fatalf("reconcileKORC: %v", err)
	}
	if _, err := r.reconcileAdminCredential(ctx, cp); err != nil {
		t.Fatalf("reconcileAdminCredential: %v", err)
	}
	if _, err := r.reconcileCatalog(ctx, cp); err != nil {
		t.Fatalf("reconcileCatalog: %v", err)
	}
	if _, err := r.reconcileKeystone(ctx, cp); err != nil {
		t.Fatalf("reconcileKeystone: %v", err)
	}
}

func TestCredentialInvariant_MintedACIsRestricted(t *testing.T) {
	g := NewGomegaWithT(t)

	s := invariantScheme(t)
	cp := invariantControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, invariantAdminPasswordSecret(cp), availableAC(cp), readyCloudsYamlES(cp)).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	driveCredentialChain(t, r, cp)

	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.Resource).NotTo(BeNil())
	g.Expect(ac.Spec.Resource.Unrestricted).NotTo(BeNil())
	g.Expect(*ac.Spec.Resource.Unrestricted).To(BeFalse(),
		"the minted admin application credential MUST be restricted (Unrestricted==false)")
}

func TestCredentialInvariant_AppCredentialSecretAbsentFromKeystoneSpec(t *testing.T) {
	g := NewGomegaWithT(t)

	s := invariantScheme(t)
	cp := invariantControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, invariantAdminPasswordSecret(cp), availableAC(cp), readyCloudsYamlES(cp)).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	driveCredentialChain(t, r, cp)

	k := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{
		Name: keystoneName(cp), Namespace: childNamespace(cp),
	}, k)).To(Succeed())

	appCredSecret := adminAppCredentialSecretName(cp)
	g.Expect(renderObjectStrings(k.Spec)).NotTo(ContainSubstring(appCredSecret),
		"the minted app-credential Secret name MUST NOT appear in the Keystone CR spec")

	// The Keystone CR may legitimately reference the admin PASSWORD Secret for
	// bootstrap — that is DISTINCT and allowed. Assert the allowed reference is
	// present (and distinct) so the test cannot pass by Keystone referencing
	// neither secret.
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Name).To(Equal(adminPasswordSecretName(cp)),
		"the admin PASSWORD secret reference is allowed and expected for bootstrap "+
			"(operator-projected per-CP Secret in managed mode)")
	g.Expect(k.Spec.Bootstrap.AdminPasswordSecretRef.Name).NotTo(Equal(appCredSecret),
		"the bootstrap password secret MUST be distinct from the minted app-credential secret")
}

func TestCredentialInvariant_AppCredentialSecretReferencedOnlyByPushSecretAndAC(t *testing.T) {
	g := NewGomegaWithT(t)

	s := invariantScheme(t)
	cp := invariantControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, invariantAdminPasswordSecret(cp), availableAC(cp), readyCloudsYamlES(cp)).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	driveCredentialChain(t, r, cp)

	ctx := context.Background()
	appCredSecret := adminAppCredentialSecretName(cp)

	// (a) The AC references the app-credential Secret via Resource.SecretRef — the
	// ALLOWED K-ORC write target.
	ac := getAC(t, c, cp)
	g.Expect(string(ac.Spec.Resource.SecretRef)).To(Equal(appCredSecret),
		"the AC's Resource.SecretRef is the allowed K-ORC write target for the credential")

	// (b) The PushSecret references the app-credential Secret as its push source —
	// the ALLOWED OpenBao mirror path.
	ps := &esov1alpha1.PushSecret{}
	g.Expect(c.Get(ctx, client.ObjectKey{
		Name: adminAppCredentialPushSecretName(cp), Namespace: childNamespace(cp),
	}, ps)).To(Succeed())
	g.Expect(ps.Spec.Selector.Secret).NotTo(BeNil())
	g.Expect(ps.Spec.Selector.Secret.Name).To(Equal(appCredSecret),
		"the PushSecret selects the app-credential Secret as its OpenBao push source")

	// (c) NO OTHER object the operator created may reference the app-credential
	// Secret name. Walk every object kind the operator can write and assert the
	// only references are the two ALLOWED ones above.
	leaks := findAppCredentialLeaks(ctx, t, c, appCredSecret, ps.Name, ac.Name)
	g.Expect(leaks).To(BeEmpty(),
		"the app-credential Secret must flow operator->OpenBao->(ESO)->K-ORC only; "+
			"no service CR may reference it. Unexpected referrers: %v", leaks)
}

func TestCredentialInvariant_NoWorkloadReferencesAppCredentialSecret(t *testing.T) {
	g := NewGomegaWithT(t)

	s := invariantScheme(t)
	cp := invariantControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, invariantAdminPasswordSecret(cp), availableAC(cp), readyCloudsYamlES(cp)).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	driveCredentialChain(t, r, cp)

	ctx := context.Background()
	appCredSecret := adminAppCredentialSecretName(cp)
	const keystoneAdminSecret = "keystone-admin"

	// Future-proofing: c5c3 creates ZERO appsv1 workloads today. Assert that, and
	// run the generic Pod-template walker so that if a future change DOES start
	// emitting a Deployment/StatefulSet/DaemonSet, any env/volume reference to the
	// app-credential or "keystone-admin" secret is caught as a leak.
	workloadCount, leaks := walkPodTemplatesForSecretLeaks(ctx, t, c, appCredSecret, keystoneAdminSecret)

	g.Expect(workloadCount).To(BeZero(),
		"c5c3 must not create any Deployment/StatefulSet/DaemonSet directly; "+
			"it projects child CRs whose downstream operators own the workloads")
	g.Expect(leaks).To(BeEmpty(),
		"no operator-created workload Pod template may reference the app-credential "+
			"or keystone-admin Secret in an env var or volume. Leaks: %v", leaks)
}

// TestCredentialInvariant_PasswordCloudConfinedToAC asserts the operator-owned
// password-cloud — which carries the CLEARTEXT admin password in its clouds.yaml —
// is confined to the admin ApplicationCredential's CloudCredentialsRef: it must not
// appear in the projected Keystone CR spec, and (like the app-credential secret)
// must not leak into any operator-created workload Pod template.
func TestCredentialInvariant_PasswordCloudConfinedToAC(t *testing.T) {
	g := NewGomegaWithT(t)

	s := invariantScheme(t)
	cp := invariantControlPlane()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, invariantAdminPasswordSecret(cp), availableAC(cp), readyCloudsYamlES(cp)).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	driveCredentialChain(t, r, cp)

	ctx := context.Background()
	pwCloud := adminPasswordCloudSecretName(cp)

	// (a) The password-cloud name must NOT appear in the projected Keystone CR.
	k := &keystonev1alpha1.Keystone{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: keystoneName(cp), Namespace: childNamespace(cp)}, k)).To(Succeed())
	g.Expect(renderObjectStrings(k.Spec)).NotTo(ContainSubstring(pwCloud),
		"the password-cloud secret (cleartext admin password) must not appear in the Keystone CR spec")

	// (b) The admin AC references it via CloudCredentialsRef — the one allowed use.
	ac := getAC(t, c, cp)
	g.Expect(ac.Spec.CloudCredentialsRef.SecretName).To(Equal(pwCloud),
		"the admin AC is the only object that may reference the password-cloud")

	// (c) No operator-created workload references it (c5c3 creates none).
	workloadCount, leaks := walkPodTemplatesForSecretLeaks(ctx, t, c, pwCloud)
	g.Expect(workloadCount).To(BeZero())
	g.Expect(leaks).To(BeEmpty(),
		"no operator-created workload Pod template may reference the password-cloud. Leaks: %v", leaks)
}

// --- shared walkers / helpers ---

// renderObjectStrings renders an object's spec to a string so a substring search
// can detect a Secret-name reference anywhere in the (possibly nested) spec
// without enumerating every field. fmt's %+v recurses into nested structs,
// pointers, slices, and maps, so a leaked name surfaces regardless of where it is
// placed.
func renderObjectStrings(spec any) string {
	return fmt.Sprintf("%+v", spec)
}

// findAppCredentialLeaks lists every object kind the operator can write and
// returns the human-readable identities of any object — other than the allowed
// PushSecret and AC — that references the app-credential Secret name. The owned
// app-credential Secret object ITSELF (whose .Name equals appCredSecret) is not a
// "reference" and is excluded.
func findAppCredentialLeaks(
	ctx context.Context, t *testing.T, c client.Client, appCredSecret, allowedPSName, allowedACName string,
) []string {
	t.Helper()
	var leaks []string

	var ks keystonev1alpha1.KeystoneList
	if err := c.List(ctx, &ks); err != nil {
		t.Fatalf("listing Keystone: %v", err)
	}
	for i := range ks.Items {
		if strings.Contains(renderObjectStrings(ks.Items[i].Spec), appCredSecret) {
			leaks = append(leaks, "Keystone/"+ks.Items[i].Name)
		}
	}

	var svcs orcv1alpha1.ServiceList
	if err := c.List(ctx, &svcs); err != nil {
		t.Fatalf("listing Service: %v", err)
	}
	for i := range svcs.Items {
		if strings.Contains(renderObjectStrings(svcs.Items[i].Spec), appCredSecret) {
			leaks = append(leaks, "Service/"+svcs.Items[i].Name)
		}
	}

	var eps orcv1alpha1.EndpointList
	if err := c.List(ctx, &eps); err != nil {
		t.Fatalf("listing Endpoint: %v", err)
	}
	for i := range eps.Items {
		if strings.Contains(renderObjectStrings(eps.Items[i].Spec), appCredSecret) {
			leaks = append(leaks, "Endpoint/"+eps.Items[i].Name)
		}
	}

	// ApplicationCredential CRs: only the allowed AC may reference it (via SecretRef).
	var acs orcv1alpha1.ApplicationCredentialList
	if err := c.List(ctx, &acs); err != nil {
		t.Fatalf("listing ApplicationCredential: %v", err)
	}
	for i := range acs.Items {
		if acs.Items[i].Name == allowedACName {
			continue
		}
		if strings.Contains(renderObjectStrings(acs.Items[i].Spec), appCredSecret) {
			leaks = append(leaks, "ApplicationCredential/"+acs.Items[i].Name)
		}
	}

	// PushSecrets: only the allowed PushSecret may reference it.
	var pss esov1alpha1.PushSecretList
	if err := c.List(ctx, &pss); err != nil {
		t.Fatalf("listing PushSecret: %v", err)
	}
	for i := range pss.Items {
		if pss.Items[i].Name == allowedPSName {
			continue
		}
		if strings.Contains(renderObjectStrings(pss.Items[i].Spec), appCredSecret) {
			leaks = append(leaks, "PushSecret/"+pss.Items[i].Name)
		}
	}

	return leaks
}

// walkPodTemplatesForSecretLeaks is the generic, future-proofing Pod-template
// walker over the appsv1 workload kinds. It returns (a) the total count of such
// workloads the operator created and (b) the identities of any whose Pod template
// references one of the forbidden Secret names in an env var (envFrom or
// valueFrom.secretKeyRef) or a volume (secret/projected source).
func walkPodTemplatesForSecretLeaks(
	ctx context.Context, t *testing.T, c client.Client, forbidden ...string,
) (int, []string) {
	t.Helper()

	count := 0
	var leaks []string
	check := func(kind, name string, tmpl corev1.PodTemplateSpec) {
		count++
		if podTemplateReferencesSecret(tmpl, forbidden...) {
			leaks = append(leaks, kind+"/"+name)
		}
	}

	var deploys appsv1.DeploymentList
	if err := c.List(ctx, &deploys); err != nil {
		t.Fatalf("listing Deployment: %v", err)
	}
	for i := range deploys.Items {
		check("Deployment", deploys.Items[i].Name, deploys.Items[i].Spec.Template)
	}

	var stss appsv1.StatefulSetList
	if err := c.List(ctx, &stss); err != nil {
		t.Fatalf("listing StatefulSet: %v", err)
	}
	for i := range stss.Items {
		check("StatefulSet", stss.Items[i].Name, stss.Items[i].Spec.Template)
	}

	var dss appsv1.DaemonSetList
	if err := c.List(ctx, &dss); err != nil {
		t.Fatalf("listing DaemonSet: %v", err)
	}
	for i := range dss.Items {
		check("DaemonSet", dss.Items[i].Name, dss.Items[i].Spec.Template)
	}

	return count, leaks
}

// podTemplateReferencesSecret reports whether a Pod template references any of the
// given Secret names through a container env (envFrom / valueFrom.secretKeyRef) or
// a pod volume (secret / projected-secret source).
func podTemplateReferencesSecret(tmpl corev1.PodTemplateSpec, names ...string) bool {
	match := func(s string) bool {
		for _, n := range names {
			if s == n {
				return true
			}
		}
		return false
	}

	containers := append([]corev1.Container{}, tmpl.Spec.InitContainers...)
	containers = append(containers, tmpl.Spec.Containers...)
	for _, ctr := range containers {
		for _, ef := range ctr.EnvFrom {
			if ef.SecretRef != nil && match(ef.SecretRef.Name) {
				return true
			}
		}
		for _, e := range ctr.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && match(e.ValueFrom.SecretKeyRef.Name) {
				return true
			}
		}
	}

	for _, v := range tmpl.Spec.Volumes {
		if v.Secret != nil && match(v.Secret.SecretName) {
			return true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.Secret != nil && match(src.Secret.Name) {
					return true
				}
			}
		}
	}
	return false
}
