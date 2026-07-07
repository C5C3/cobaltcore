// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/forge/internal/common/conditions"
	"github.com/c5c3/forge/internal/common/config"
	horizonv1alpha1 "github.com/c5c3/forge/operators/horizon/api/v1alpha1"
)

func TestReconcileConfig_RendersLocalSettings(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(name).To(HavePrefix("test-horizon-config-"))

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]
	g.Expect(rendered).NotTo(BeEmpty())

	// SECRET_KEY comes from the environment and never enters the ConfigMap.
	g.Expect(rendered).To(ContainSubstring(`SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`))
	g.Expect(rendered).NotTo(ContainSubstring("super-secret"))

	// Design decision D1: signed-cookie sessions + Memcached-backed cache.
	g.Expect(rendered).To(ContainSubstring(`SESSION_ENGINE = "django.contrib.sessions.backends.signed_cookies"`))
	g.Expect(rendered).To(ContainSubstring(`"BACKEND": "django.core.cache.backends.memcached.PyMemcacheCache"`))
	g.Expect(rendered).To(ContainSubstring(`"LOCATION": ["memcached:11211"]`))

	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_KEYSTONE_URL = "http://keystone.default.svc.cluster.local:5000/v3"`))
	// The server-side clients must use the internal catalog interface — the
	// public entries may only resolve outside the cluster. The legacy "...URL"
	// spelling is required by openstack_dashboard's ENDPOINT_TYPE_TO_INTERFACE
	// map; a bare "internal" fails every catalog lookup.
	g.Expect(rendered).To(ContainSubstring(`OPENSTACK_ENDPOINT_TYPE = "internalURL"`))
	g.Expect(rendered).To(ContainSubstring("COMPRESS_OFFLINE = True"))
	g.Expect(rendered).To(ContainSubstring(`STATIC_ROOT = "/var/lib/openstack/horizon-static"`))
	g.Expect(rendered).To(ContainSubstring("DEBUG = False"))

	// ALLOWED_HOSTS is a closed allow-list — the fixed probe Host header and
	// every standard Kubernetes DNS form of the dashboard Service — never
	// the "*" wildcard that would disable Django's Host-header validation.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["localhost", "test-horizon", "test-horizon.default", "test-horizon.default.svc", "test-horizon.default.svc.cluster.local"]`))
	g.Expect(rendered).NotTo(ContainSubstring(`ALLOWED_HOSTS = ["*"]`))

	cond := conditions.GetCondition(h.Status.Conditions, conditionTypeConfigReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigRendered))
}

func TestReconcileConfig_ExtraConfigOverridesDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"ALLOWED_HOSTS":   {Raw: []byte(`["horizon.example.com"]`)},
		"SESSION_TIMEOUT": {Raw: []byte(`1800`)},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// User value wins over the operator default.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["horizon.example.com"]`))
	g.Expect(rendered).NotTo(ContainSubstring(`ALLOWED_HOSTS = ["*"]`))
	// New settings are appended.
	g.Expect(rendered).To(ContainSubstring("SESSION_TIMEOUT = 1800"))
}

func TestReconcileConfig_ExtraConfigSecretKeyNeverEntersConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	// A webhook bypass (direct etcd write, disabled admission, an old stored
	// object) could smuggle a SECRET_KEY into extraConfig. The renderer must
	// fail closed and drop it so the session-signing key never enters the
	// namespace-readable ConfigMap and cannot override the env-sourced lookup.
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"SECRET_KEY":      {Raw: []byte(`"attacker-known"`)},
		"SESSION_TIMEOUT": {Raw: []byte(`1800`)},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// The attacker-controlled key material must not appear anywhere, and no
	// plaintext SECRET_KEY assignment may shadow the env-sourced lookup.
	g.Expect(rendered).NotTo(ContainSubstring("attacker-known"))
	g.Expect(rendered).NotTo(ContainSubstring(`SECRET_KEY = "`))
	// The env-sourced SECRET_KEY line remains the only SECRET_KEY assignment.
	g.Expect(rendered).To(ContainSubstring(`SECRET_KEY = os.environ["HORIZON_SECRET_KEY"]`))
	// Non-SECRET_KEY extraConfig entries still pass through unaffected.
	g.Expect(rendered).To(ContainSubstring("SESSION_TIMEOUT = 1800"))
}

func TestReconcileConfig_AllowedHostsIncludesGatewayHostname(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Gateway = gatewaySpec()
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]

	// The Gateway forwards requests with spec.gateway.hostname as the Host
	// header, so it joins the closed ALLOWED_HOSTS allow-list alongside the
	// probe Host and the Service DNS names.
	g.Expect(rendered).To(ContainSubstring(`ALLOWED_HOSTS = ["localhost", "test-horizon", "test-horizon.default", "test-horizon.default.svc", "test-horizon.default.svc.cluster.local", "horizon.127-0-0-1.nip.io"]`))
}

func TestReconcileConfig_InvalidExtraConfigJSONFails(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
		"BROKEN": {Raw: []byte(`{not json`)},
	}
	r := newTestReconciler(testScheme(), h)

	_, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("rendering local_settings.py"))
}

func TestReconcileConfig_PerLoggerLevelsReachLoggingDict(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	h.Spec.Logging = &horizonv1alpha1.LoggingSpec{
		Level:           "WARNING",
		PerLoggerLevels: map[string]string{"django.request": "ERROR"},
	}
	r := newTestReconciler(testScheme(), h)

	name, err := r.reconcileConfig(context.Background(), h)
	g.Expect(err).NotTo(HaveOccurred())

	var cm corev1.ConfigMap
	g.Expect(r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &cm)).To(Succeed())
	rendered := cm.Data["local_settings.py"]
	g.Expect(rendered).To(ContainSubstring(`"level": "WARNING"`))
	g.Expect(rendered).To(ContainSubstring(`"django.request": {"level": "ERROR"}`))
}

func TestPruneStaleConfigMaps_RetainsNewestAndCurrent(t *testing.T) {
	g := NewGomegaWithT(t)
	h := testHorizon()
	r := newTestReconciler(testScheme(), h)
	ctx := context.Background()

	// Produce 6 distinct ConfigMap generations by varying extraConfig.
	var names []string
	for i := 0; i < 6; i++ {
		h.Spec.ExtraConfig = map[string]apiextensionsv1.JSON{
			"SESSION_TIMEOUT": {Raw: []byte(fmt.Sprintf(`%d`, 1000+i))},
		}
		name, err := r.reconcileConfig(ctx, h)
		g.Expect(err).NotTo(HaveOccurred())
		names = append(names, name)
	}

	current := names[len(names)-1]
	g.Expect(r.pruneStaleConfigMaps(ctx, h, current)).To(Succeed())

	var cms corev1.ConfigMapList
	g.Expect(r.List(ctx, &cms, client.InNamespace("default"),
		client.MatchingLabels{config.ConfigBaseLabelKey: "test-horizon-config"})).To(Succeed())

	remaining := make([]string, 0, len(cms.Items))
	for _, cm := range cms.Items {
		remaining = append(remaining, cm.Name)
	}
	// Retain = 3 historical + the current one.
	g.Expect(remaining).To(HaveLen(defaultConfigMapRetainCount + 1))
	g.Expect(remaining).To(ContainElement(current), "the active ConfigMap must survive pruning")

	// Identity of the pruned pair: with the fake client every ConfigMap
	// shares one CreationTimestamp second, so the prune helper's documented
	// tie-break (name descending) decides retention. Compute the expected
	// survivors the same way and assert the exact pruned set.
	historical := append([]string(nil), names[:len(names)-1]...)
	sort.Sort(sort.Reverse(sort.StringSlice(historical)))
	expectedRetained := historical[:defaultConfigMapRetainCount]
	expectedPruned := historical[defaultConfigMapRetainCount:]
	g.Expect(remaining).To(ContainElements(expectedRetained[0], expectedRetained[1], expectedRetained[2]))
	for _, pruned := range expectedPruned {
		g.Expect(remaining).NotTo(ContainElement(pruned), "ConfigMap %s must be pruned", pruned)
	}
}
