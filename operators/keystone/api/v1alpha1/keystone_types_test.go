// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "keystone.openstack.c5c3.io" {
		t.Errorf("expected group %q, got %q", "keystone.openstack.c5c3.io", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("expected version %q, got %q", "v1alpha1", GroupVersion.Version)
	}
}

func TestSchemeBuilderRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	// Verify Keystone is registered
	gvk := schema.GroupVersionKind{
		Group:   "keystone.openstack.c5c3.io",
		Version: "v1alpha1",
		Kind:    "Keystone",
	}
	obj, err := scheme.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
	}
	if _, ok := obj.(*Keystone); !ok {
		t.Errorf("expected *Keystone, got %T", obj)
	}

	// Verify KeystoneList is registered
	gvk.Kind = "KeystoneList"
	obj, err = scheme.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v) failed: %v", gvk, err)
	}
	if _, ok := obj.(*KeystoneList); !ok {
		t.Errorf("expected *KeystoneList, got %T", obj)
	}
}

func TestKeystoneImplementsRuntimeObject(t *testing.T) {
	var _ runtime.Object = &Keystone{}
	var _ runtime.Object = &KeystoneList{}
}

func TestKeystoneSpecFields(t *testing.T) {
	spec := KeystoneSpec{}

	// Verify zero values for struct fields — these will be defaulted by kubebuilder markers at CRD level
	if spec.Replicas != 0 {
		t.Errorf("expected zero value for Replicas, got %d", spec.Replicas)
	}
	if spec.Federation != nil {
		t.Errorf("expected nil Federation, got %v", spec.Federation)
	}
	if spec.PolicyOverrides != nil {
		t.Errorf("expected nil PolicyOverrides, got %v", spec.PolicyOverrides)
	}
	if spec.Middleware != nil {
		t.Errorf("expected nil Middleware, got %v", spec.Middleware)
	}
	if spec.Plugins != nil {
		t.Errorf("expected nil Plugins, got %v", spec.Plugins)
	}
	if spec.ExtraConfig != nil {
		t.Errorf("expected nil ExtraConfig, got %v", spec.ExtraConfig)
	}
	if spec.UWSGI != nil {
		t.Errorf("expected nil UWSGI, got %v", spec.UWSGI)
	}
	if spec.TerminationGracePeriodSeconds != nil {
		t.Errorf("expected nil TerminationGracePeriodSeconds, got %v", spec.TerminationGracePeriodSeconds)
	}
	if spec.PreStopSleepSeconds != nil {
		t.Errorf("expected nil PreStopSleepSeconds, got %v", spec.PreStopSleepSeconds)
	}
	if spec.Strategy != nil {
		t.Errorf("expected nil Strategy, got %v", spec.Strategy)
	}
}

// TestKeystoneSpecTerminationGracePeriodSecondsField verifies that the optional
// pointer fields added for feature CC-0084 round-trip through the Go struct
// — REQ-001 and REQ-002 require these fields on KeystoneSpec.
func TestKeystoneSpecTerminationGracePeriodSecondsField(t *testing.T) {
	grace := int64(120)
	preStop := int64(15)
	spec := KeystoneSpec{
		TerminationGracePeriodSeconds: &grace,
		PreStopSleepSeconds:           &preStop,
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 120 {
		t.Errorf("expected TerminationGracePeriodSeconds=120, got %v", spec.TerminationGracePeriodSeconds)
	}
	if spec.PreStopSleepSeconds == nil || *spec.PreStopSleepSeconds != 15 {
		t.Errorf("expected PreStopSleepSeconds=15, got %v", spec.PreStopSleepSeconds)
	}

	// Zero preStop is a valid value (REQ-002) — the pointer must be distinguishable from unset.
	zero := int64(0)
	spec2 := KeystoneSpec{PreStopSleepSeconds: &zero}
	if spec2.PreStopSleepSeconds == nil || *spec2.PreStopSleepSeconds != 0 {
		t.Errorf("expected PreStopSleepSeconds=0 (set), got %v", spec2.PreStopSleepSeconds)
	}

	// Deep copy must preserve pointer values without aliasing the source.
	out := spec.DeepCopy()
	if out.TerminationGracePeriodSeconds == spec.TerminationGracePeriodSeconds {
		t.Errorf("DeepCopy must allocate a new TerminationGracePeriodSeconds pointer")
	}
	if out.PreStopSleepSeconds == spec.PreStopSleepSeconds {
		t.Errorf("DeepCopy must allocate a new PreStopSleepSeconds pointer")
	}
	if *out.TerminationGracePeriodSeconds != 120 || *out.PreStopSleepSeconds != 15 {
		t.Errorf("DeepCopy lost values: grace=%v preStop=%v",
			out.TerminationGracePeriodSeconds, out.PreStopSleepSeconds)
	}
}

// TestKeystoneSpecStrategyField verifies that spec.strategy accepts a full
// appsv1.DeploymentStrategy and round-trips through deep copy — REQ-006 (CC-0084).
func TestKeystoneSpecStrategyField(t *testing.T) {
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromInt(1)
	strategy := &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
	spec := KeystoneSpec{Strategy: strategy}
	if spec.Strategy == nil {
		t.Fatalf("expected Strategy to be settable, got nil")
	}
	if spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("expected Type=RollingUpdate, got %q", spec.Strategy.Type)
	}
	if spec.Strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate block, got nil")
	}
	if spec.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 {
		t.Errorf("expected MaxUnavailable=0, got %v", spec.Strategy.RollingUpdate.MaxUnavailable)
	}
	if spec.Strategy.RollingUpdate.MaxSurge.IntVal != 1 {
		t.Errorf("expected MaxSurge=1, got %v", spec.Strategy.RollingUpdate.MaxSurge)
	}

	// Recreate strategy must also be settable (REQ-006 scenario "Recreate propagated").
	recreate := KeystoneSpec{Strategy: &appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}}
	if recreate.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("expected Type=Recreate, got %q", recreate.Strategy.Type)
	}

	// DeepCopy must clone the pointer chain.
	out := spec.DeepCopy()
	if out.Strategy == spec.Strategy {
		t.Errorf("DeepCopy must allocate a new Strategy pointer")
	}
	if out.Strategy.RollingUpdate == spec.Strategy.RollingUpdate {
		t.Errorf("DeepCopy must allocate a new RollingUpdate block")
	}
	if out.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 ||
		out.Strategy.RollingUpdate.MaxSurge.IntVal != 1 {
		t.Errorf("DeepCopy lost strategy values: %+v", out.Strategy.RollingUpdate)
	}
}

func TestUWSGISpecFields(t *testing.T) {
	uwsgi := UWSGISpec{}
	if uwsgi.Processes != 0 {
		t.Errorf("expected zero value for Processes, got %d", uwsgi.Processes)
	}
	if uwsgi.Threads != 0 {
		t.Errorf("expected zero value for Threads, got %d", uwsgi.Threads)
	}
	if uwsgi.HTTPKeepAlive {
		t.Errorf("expected false for HTTPKeepAlive, got %v", uwsgi.HTTPKeepAlive)
	}
	// REQ-003: harakiri is an optional pointer; zero value must be nil (unset).
	if uwsgi.Harakiri != nil {
		t.Errorf("expected nil Harakiri, got %v", uwsgi.Harakiri)
	}
	// REQ-004: httpKeepAliveTimeout is an optional pointer; zero value must be nil (unset).
	if uwsgi.HTTPKeepAliveTimeout != nil {
		t.Errorf("expected nil HTTPKeepAliveTimeout, got %v", uwsgi.HTTPKeepAliveTimeout)
	}

	// Setting the pointer fields must preserve the provided value (CC-0084 REQ-003, REQ-004).
	harakiri := int32(20)
	keepAliveTimeout := int32(5)
	configured := UWSGISpec{
		Harakiri:             &harakiri,
		HTTPKeepAliveTimeout: &keepAliveTimeout,
	}
	if configured.Harakiri == nil || *configured.Harakiri != 20 {
		t.Errorf("expected Harakiri=20, got %v", configured.Harakiri)
	}
	if configured.HTTPKeepAliveTimeout == nil || *configured.HTTPKeepAliveTimeout != 5 {
		t.Errorf("expected HTTPKeepAliveTimeout=5, got %v", configured.HTTPKeepAliveTimeout)
	}

	// DeepCopy must clone each pointer so callers cannot mutate the source.
	cloned := configured.DeepCopy()
	if cloned.Harakiri == configured.Harakiri {
		t.Errorf("DeepCopy must allocate a new Harakiri pointer")
	}
	if cloned.HTTPKeepAliveTimeout == configured.HTTPKeepAliveTimeout {
		t.Errorf("DeepCopy must allocate a new HTTPKeepAliveTimeout pointer")
	}
	if *cloned.Harakiri != 20 || *cloned.HTTPKeepAliveTimeout != 5 {
		t.Errorf("DeepCopy lost values: harakiri=%v timeout=%v",
			cloned.Harakiri, cloned.HTTPKeepAliveTimeout)
	}
}

func TestFernetSpecFields(t *testing.T) {
	fernet := FernetSpec{}
	if fernet.MaxActiveKeys != 0 {
		t.Errorf("expected zero value for MaxActiveKeys, got %d", fernet.MaxActiveKeys)
	}
	if fernet.RotationSchedule != "" {
		t.Errorf("expected empty RotationSchedule, got %q", fernet.RotationSchedule)
	}
}

func TestBootstrapSpecFields(t *testing.T) {
	bootstrap := BootstrapSpec{}
	if bootstrap.AdminUser != "" {
		t.Errorf("expected empty AdminUser, got %q", bootstrap.AdminUser)
	}
	if bootstrap.Region != "" {
		t.Errorf("expected empty Region, got %q", bootstrap.Region)
	}
}

func TestKeystoneStatusFields(t *testing.T) {
	status := KeystoneStatus{}
	if status.Conditions != nil {
		t.Errorf("expected nil Conditions, got %v", status.Conditions)
	}
	if status.Endpoint != "" {
		t.Errorf("expected empty Endpoint, got %q", status.Endpoint)
	}
	if status.InstalledRelease != "" {
		t.Errorf("expected empty InstalledRelease, got %q", status.InstalledRelease)
	}
	if status.TargetRelease != "" {
		t.Errorf("expected empty TargetRelease, got %q", status.TargetRelease)
	}
	if status.UpgradePhase != "" {
		t.Errorf("expected empty UpgradePhase, got %q", status.UpgradePhase)
	}
}

func TestUpgradePhaseConstants(t *testing.T) {
	tests := []struct {
		phase UpgradePhase
		want  string
	}{
		{UpgradePhaseExpanding, "Expanding"},
		{UpgradePhaseMigrating, "Migrating"},
		{UpgradePhaseRollingUpdate, "RollingUpdate"},
		{UpgradePhaseContracting, "Contracting"},
	}
	for _, tt := range tests {
		if string(tt.phase) != tt.want {
			t.Errorf("expected %q, got %q", tt.want, tt.phase)
		}
	}
}
