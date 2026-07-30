// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/forge/internal/common/config"
	commonv1 "github.com/c5c3/forge/internal/common/types"
	placementv1alpha1 "github.com/c5c3/forge/operators/placement/api/v1alpha1"
)

// placementForConfig returns a Placement carrying what the defaulting webhook
// materializes — the service-user identity and the logging baseline — plus a
// region, so the config render exercises the optional keystone_authtoken keys.
func placementForConfig() *placementv1alpha1.Placement {
	placement := testPlacement()
	placement.Spec.ServiceUser.Username = "placement"
	placement.Spec.ServiceUser.ProjectName = "service"
	placement.Spec.ServiceUser.UserDomainName = "Default"
	placement.Spec.ServiceUser.ProjectDomainName = "Default"
	placement.Spec.Region = "RegionOne"
	placement.Spec.Logging = &placementv1alpha1.LoggingSpec{Format: "text", Level: "INFO", Debug: ptr.To(false)}
	return placement
}

// renderedConfigMap re-reads the ConfigMap the config step created.
func renderedConfigMap(t *testing.T, r *PlacementReconciler, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "default", Name: name}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("re-reading config ConfigMap %s: %v", name, err)
	}
	return &cm
}

// renderedConfig returns the placement.conf produced by reconcileConfig.
func renderedConfig(t *testing.T, r *PlacementReconciler, name string) string {
	t.Helper()
	return renderedConfigMap(t, r, name).Data["placement.conf"]
}

func TestReconcileConfig_RendersPlacementConf(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	r := newPlacementTestReconciler(placement)

	res, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).NotTo(BeEmpty())

	cm := renderedConfigMap(t, r, name)
	// Placement reads exactly one file from OS_PLACEMENT_CONFIG_DIR, so the
	// ConfigMap carries exactly one data key unless a policy or logging file is
	// requested.
	g.Expect(cm.Data).To(HaveLen(1))
	g.Expect(cm.Data).To(HaveKey("placement.conf"))
	// The content hash makes the name change with the content, and immutability
	// is what forces a new name (and therefore a pod roll) instead of an in-place
	// edit the kubelet would propagate mid-request.
	g.Expect(name).To(HavePrefix("test-placement-config-"))
	g.Expect(cm.Immutable).NotTo(BeNil())
	g.Expect(*cm.Immutable).To(BeTrue())

	conf := cm.Data["placement.conf"]
	g.Expect(conf).To(ContainSubstring("[DEFAULT]"))
	g.Expect(conf).To(ContainSubstring("use_stderr = true"))
	g.Expect(conf).To(ContainSubstring("debug = false"))
	// Placement reads auth_strategy from [api], not from [DEFAULT].
	g.Expect(conf).To(ContainSubstring("[api]"))
	g.Expect(conf).To(ContainSubstring("auth_strategy = keystone"))
	// The database options live in [placement_database], and the connection is a
	// placeholder the env override replaces at runtime.
	g.Expect(conf).To(ContainSubstring("[placement_database]"))
	g.Expect(conf).To(ContainSubstring("connection = " + dbConnectionPlaceholder))
	g.Expect(conf).To(ContainSubstring("connection_recycle_time = 600"))
	g.Expect(conf).To(ContainSubstring("max_retries = -1"))
	// [keystone_authtoken]: identity + region + memcached, www_authenticate_uri
	// falls back to auth_url when no public endpoint is set, and NO password.
	g.Expect(conf).To(ContainSubstring("[keystone_authtoken]"))
	g.Expect(conf).To(ContainSubstring("auth_url = http://keystone.default.svc:5000"))
	g.Expect(conf).To(ContainSubstring("www_authenticate_uri = http://keystone.default.svc:5000"))
	g.Expect(conf).To(ContainSubstring("username = placement"))
	g.Expect(conf).To(ContainSubstring("project_name = service"))
	g.Expect(conf).To(ContainSubstring("region_name = RegionOne"))
	g.Expect(conf).To(ContainSubstring("memcached_servers = mc:11211"))
	// The service password reaches the pod through
	// OS_KEYSTONE_AUTHTOKEN__PASSWORD only: a file value would leak it into a
	// namespace-readable ConfigMap. The auth_type = password line is the
	// middleware's plugin name, not a credential, so the assertion anchors on the
	// key.
	g.Expect(conf).NotTo(ContainSubstring("password ="))
	// No policy overrides configured, so no oslo_policy section.
	g.Expect(conf).NotTo(ContainSubstring("[oslo_policy]"))
}

// TestConfigPaths_MatchTheImageContract pins the in-pod layout the deployment
// step and the db-sync Job build on. placement.wsgi:init_application loads
// exactly $OS_PLACEMENT_CONFIG_DIR/placement.conf, so the ConfigMap is mounted
// as a whole read-only volume at /etc/placement and the env var carries that
// path. The db-tls keypair must then land outside that directory: the runtime
// cannot create a mountpoint inside a read-only tmpfs, so a nested path would
// fail the pod at container start rather than at render time.
func TestConfigPaths_MatchTheImageContract(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(placementConfigDir).To(Equal("/etc/placement/"))
	g.Expect(policyFilePath).To(Equal("/etc/placement/policy.yaml"))
	g.Expect(loggingConfFilePath).To(Equal("/etc/placement/logging.conf"))

	g.Expect(dbTLSMountPath).To(Equal("/etc/placement-db-tls/"))
	g.Expect(dbTLSMountPath).NotTo(HavePrefix(placementConfigDir),
		"a volume nested inside the read-only config mount cannot be created")
}

// TestReconcileConfig_OptionalKeystoneKeysOmitted covers the CR that configures
// neither a cache nor a region: both keys must be absent so keystonemiddleware
// keeps its own defaults rather than reading an empty override.
func TestReconcileConfig_OptionalKeystoneKeysOmitted(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	placement.Spec.Cache = commonv1.CacheSpec{}
	placement.Spec.Region = ""
	r := newPlacementTestReconciler(placement)

	_, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	conf := renderedConfig(t, r, name)
	g.Expect(conf).To(ContainSubstring("[keystone_authtoken]"))
	g.Expect(conf).NotTo(ContainSubstring("memcached_servers"))
	g.Expect(conf).NotTo(ContainSubstring("region_name"))
}

func TestReconcileConfig_PolicyOverridesRenderOsloPolicy(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	placement.Spec.PolicyOverrides = &commonv1.PolicySpec{
		Rules: map[string]string{"placement:resource_providers:list": "role:reader"},
	}
	r := newPlacementTestReconciler(placement)

	_, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	cm := renderedConfigMap(t, r, name)
	g.Expect(cm.Data).To(HaveKey("policy.yaml"))
	g.Expect(cm.Data["policy.yaml"]).To(ContainSubstring("placement:resource_providers:list"))

	conf := cm.Data["placement.conf"]
	g.Expect(conf).To(ContainSubstring("[oslo_policy]"))
	g.Expect(conf).To(ContainSubstring("policy_file = " + policyFilePath))
}

// TestReconcileConfig_JSONLoggingShipsLoggingConf covers the second file the
// renderer can emit: format=json points oslo.log at a fileConfig the same
// ConfigMap carries, and the per-logger levels render into the oslo.log CSV.
func TestReconcileConfig_JSONLoggingShipsLoggingConf(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	placement.Spec.Logging = &placementv1alpha1.LoggingSpec{
		Format:          "json",
		Level:           "DEBUG",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"sqlalchemy.engine": "WARNING", "placement": "DEBUG"},
	}
	r := newPlacementTestReconciler(placement)

	_, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	cm := renderedConfigMap(t, r, name)
	g.Expect(cm.Data).To(HaveKey("logging.conf"))
	g.Expect(cm.Data["logging.conf"]).To(ContainSubstring("class = oslo_log.formatters.JSONFormatter"))
	g.Expect(cm.Data["logging.conf"]).To(ContainSubstring("level = DEBUG"))

	conf := cm.Data["placement.conf"]
	g.Expect(conf).To(ContainSubstring("log_config_append = " + loggingConfFilePath))
	g.Expect(conf).To(ContainSubstring("debug = true"))
	// Sorted keys: the value feeds the ConfigMap content hash, so map iteration
	// order must not reach it.
	g.Expect(conf).To(ContainSubstring("default_log_levels = placement=DEBUG,sqlalchemy.engine=WARNING"))
}

// TestReconcileConfig_NilAndEmptyExtraConfigRenderIdentically pins the
// nil-versus-empty contract: an empty spec.extraConfig map takes the merge path
// while nil skips it, and both must produce the same bytes — otherwise adding
// and removing the last override would rotate the ConfigMap and roll the pods
// for a spec that renders the same file.
func TestReconcileConfig_NilAndEmptyExtraConfigRenderIdentically(t *testing.T) {
	g := NewGomegaWithT(t)

	nilCfg := placementForConfig()
	nilCfg.Spec.ExtraConfig = nil
	rNil := newPlacementTestReconciler(nilCfg)
	_, nilName, err := rNil.reconcileConfig(context.Background(), nilCfg)
	g.Expect(err).NotTo(HaveOccurred())

	emptyCfg := placementForConfig()
	emptyCfg.Spec.ExtraConfig = map[string]map[string]string{}
	rEmpty := newPlacementTestReconciler(emptyCfg)
	_, emptyName, err := rEmpty.reconcileConfig(context.Background(), emptyCfg)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(emptyName).To(Equal(nilName), "an empty extraConfig must not rotate the ConfigMap")
	g.Expect(renderedConfig(t, rEmpty, emptyName)).To(Equal(renderedConfig(t, rNil, nilName)))
}

// TestReconcileConfig_OwnedKeyOverrideReported verifies the report-only
// contract: overriding an operator-owned key in spec.extraConfig sets
// ExtraConfigHealthy=False, emits a Warning event, AND still honours the
// override in the rendered placement.conf (the guard reports, it does not
// reject). The [placement_database] connection override is the illustrative
// case: it is honoured in the file and still inert at runtime, because the env
// override wins over the file value.
func TestReconcileConfig_OwnedKeyOverrideReported(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	placement.Spec.ExtraConfig = map[string]map[string]string{
		"placement_database": {"connection": "mysql+pymysql://user:pw@elsewhere/placement"},
	}
	r := newPlacementTestReconciler(placement)

	_, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(placement.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonOwnedKeysOverridden))
	g.Expect(cond.Message).To(ContainSubstring("[placement_database] connection"))

	// A Warning event carries the reason and the overridden-key text.
	events := collectEvents(r.Recorder.(*record.FakeRecorder))
	g.Expect(events).To(ContainElement(And(
		ContainSubstring("Warning"),
		ContainSubstring(config.EventReasonExtraConfigOwnedKeyOverride),
		ContainSubstring("[placement_database] connection"),
	)))

	// Report-only: the override is honoured in the rendered placement.conf.
	conf := renderedConfig(t, r, name)
	g.Expect(conf).To(ContainSubstring("connection = mysql+pymysql://user:pw@elsewhere/placement"))
	g.Expect(conf).NotTo(ContainSubstring("connection = " + dbConnectionPlaceholder))
}

// TestReconcileConfig_ExtraConfigHealthyTrueOnUnownedKey verifies that a CR
// whose spec.extraConfig overrides no operator-owned key sets
// ExtraConfigHealthy=True and emits no event.
func TestReconcileConfig_ExtraConfigHealthyTrueOnUnownedKey(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	// max_pool_size is a real placement.conf key the operator does not own, so
	// overriding it must not trip the guard.
	placement.Spec.ExtraConfig = map[string]map[string]string{
		"placement_database": {"max_pool_size": "10"},
	}
	r := newPlacementTestReconciler(placement)

	_, name, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(placement.Status.Conditions, config.ConditionTypeExtraConfigHealthy)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(config.ConditionReasonNoOwnedKeysOverridden))
	g.Expect(collectEvents(r.Recorder.(*record.FakeRecorder))).To(BeEmpty())
	g.Expect(renderedConfig(t, r, name)).To(ContainSubstring("max_pool_size = 10"))
}

// TestReconcileConfig_ApplyFailureMarksSecretsReady covers the write path: a
// ConfigMap Create the API server refuses must surface as SecretsReady=False
// with reason ConfigError, so the aggregate Ready cannot stay stale-True at the
// new generation, and the client error must stay unwrappable.
func TestReconcileConfig_ApplyFailureMarksSecretsReady(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()

	boom := errors.New("admission webhook rejected the ConfigMap")
	c := placementFakeClientBuilder(placement).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &PlacementReconciler{Client: c, Scheme: testScheme(), Recorder: record.NewFakeRecorder(10)}

	res, name, err := r.reconcileConfig(context.Background(), placement)

	g.Expect(err).To(MatchError(boom))
	g.Expect(err).To(MatchError(ContainSubstring("creating config ConfigMap:")))
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(name).To(BeEmpty())

	cond := meta.FindStatusCondition(placement.Status.Conditions, "SecretsReady")
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigError))
	g.Expect(cond.Message).To(ContainSubstring(boom.Error()))
}

// TestReconcileConfig_SpecChangeRotatesConfigMap verifies the content-hashed
// name reaches the running pods: a changed spec value must produce a new
// ConfigMap name for the deployment step to roll.
func TestReconcileConfig_SpecChangeRotatesConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	placement := placementForConfig()
	r := newPlacementTestReconciler(placement)

	_, before, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	placement.Spec.Region = "RegionTwo"
	_, after, err := r.reconcileConfig(context.Background(), placement)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(after).NotTo(Equal(before), "a spec change must rotate the content-hashed ConfigMap")
	g.Expect(renderedConfig(t, r, after)).To(ContainSubstring("region_name = RegionTwo"))
}

// TestOperatorDefaults_RegistryDriftGuard is the completeness check tying
// operatorDefaults to placementv1alpha1.OwnedConfigKeys: every key the operator
// renders must be registered (forward) and every registered key must be
// rendered (reverse), so the registry and the renderer cannot drift apart.
func TestOperatorDefaults_RegistryDriftGuard(t *testing.T) {
	placement := placementForConfig()
	// placementForConfig already sets Region (emits region_name) and
	// Cache.Servers (emits memcached_servers). Add json logging with per-logger
	// levels so log_config_append and default_log_levels render too.
	placement.Spec.Logging = &placementv1alpha1.LoggingSpec{
		Format:          "json",
		Debug:           ptr.To(true),
		PerLoggerLevels: map[string]string{"placement": "DEBUG"},
	}

	defaults := operatorDefaults(placement)
	defaults = config.InjectOsloPolicyConfig(defaults, policyFilePath)

	registered := make(map[[2]string]struct{}, len(placementv1alpha1.OwnedConfigKeys))
	for _, o := range placementv1alpha1.OwnedConfigKeys {
		registered[[2]string{o.Section, o.Key}] = struct{}{}
	}

	// Forward check: every rendered (section, key) is registered — no exceptions.
	for section, kvs := range defaults {
		for key := range kvs {
			if _, ok := registered[[2]string{section, key}]; ok {
				continue
			}
			t.Errorf("operatorDefaults renders unregistered key [%s] %s: add it to "+
				"placementv1alpha1.OwnedConfigKeys", section, key)
		}
	}

	// Reverse check: every registered key is rendered by operatorDefaults or on
	// the extras list. keystone_authtoken/password is never rendered —
	// keystoneauth.Section omits it — but is registered because a user putting it
	// in spec.extraConfig leaks the credential. DEFAULT/auth_strategy is never
	// rendered either — the operator writes the [api] spelling — but is registered
	// because oslo.config still honors the deprecated alias, so an extraConfig
	// entry under it would disable token validation.
	reverseExtras := map[[2]string]struct{}{
		{"keystone_authtoken", "password"}: {},
		{"DEFAULT", "auth_strategy"}:       {},
	}
	for _, o := range placementv1alpha1.OwnedConfigKeys {
		if _, ok := defaults[o.Section][o.Key]; ok {
			continue
		}
		if _, ok := reverseExtras[[2]string{o.Section, o.Key}]; ok {
			continue
		}
		t.Errorf("registry key [%s] %s is not rendered by operatorDefaults: remove it "+
			"from placementv1alpha1.OwnedConfigKeys or extend the drift-guard extras list", o.Section, o.Key)
	}
}

// TestEffectiveLogging pins the render-time resolution of spec.logging: a CR
// that bypassed the defaulting webhook still renders the production baseline,
// and the resolver hands back a copy so nothing is written into the CR.
func TestEffectiveLogging(t *testing.T) {
	g := NewGomegaWithT(t)

	fromNil := effectiveLogging(nil)
	g.Expect(fromNil.Format).To(Equal("text"))
	g.Expect(fromNil.Level).To(Equal("INFO"))
	g.Expect(fromNil.Debug).NotTo(BeNil())
	g.Expect(*fromNil.Debug).To(BeFalse())

	// A half-filled block keeps what it sets and defaults the rest.
	partial := &placementv1alpha1.LoggingSpec{Level: "WARNING"}
	resolved := effectiveLogging(partial)
	g.Expect(resolved.Format).To(Equal("text"))
	g.Expect(resolved.Level).To(Equal("WARNING"))
	g.Expect(partial.Format).To(BeEmpty(), "the input spec must be left untouched")
	g.Expect(partial.Debug).To(BeNil(), "the input spec must be left untouched")
}

// TestRenderSortedPairs pins the deterministic wire format of the oslo.log
// default_log_levels CSV: an unsorted render would rotate the ConfigMap and roll
// the Deployment on a reconcile that changed nothing.
func TestRenderSortedPairs(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(config.RenderSortedPairs(nil, "=")).To(BeEmpty())
	g.Expect(config.RenderSortedPairs(map[string]string{}, "=")).To(BeEmpty())

	levels := map[string]string{
		"sqlalchemy.engine": "WARNING",
		"placement":         "DEBUG",
		"oslo_policy":       "INFO",
	}
	want := "oslo_policy=INFO,placement=DEBUG,sqlalchemy.engine=WARNING"
	for range 32 {
		g.Expect(config.RenderSortedPairs(levels, "=")).To(Equal(want),
			"Go map order must not reach the rendered value")
	}
}

// TestRenderLoggingConf pins the fileConfig shape Python's
// logging.config.fileConfig accepts and the level indirection: the root logger
// carries spec.logging.level while the handler stays at NOTSET, so a level
// change is not silently shadowed by the handler.
func TestRenderLoggingConf(t *testing.T) {
	g := NewGomegaWithT(t)

	conf := config.RenderLoggingConf("WARNING")
	for _, section := range []string{
		"[loggers]", "[handlers]", "[formatters]",
		"[logger_root]", "[handler_stderr]", "[formatter_json]",
	} {
		g.Expect(conf).To(ContainSubstring(section))
	}
	g.Expect(conf).To(ContainSubstring("\nlevel = WARNING\nhandlers = stderr\n"))
	g.Expect(strings.Count(conf, "level = NOTSET")).To(Equal(1))
}
