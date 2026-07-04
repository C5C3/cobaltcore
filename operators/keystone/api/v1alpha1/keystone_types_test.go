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
// *int64 fields are settable and round-trip through a DeepCopy
// unchanged at the type level; webhook range enforcement is covered
// separately in keystone_webhook_test.go.
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

	clone := spec.DeepCopy()
	if clone.TerminationGracePeriodSeconds == spec.TerminationGracePeriodSeconds {
		t.Errorf("DeepCopy did not allocate a new *int64 for TerminationGracePeriodSeconds")
	}
	if clone.PreStopSleepSeconds == spec.PreStopSleepSeconds {
		t.Errorf("DeepCopy did not allocate a new *int64 for PreStopSleepSeconds")
	}
	if *clone.TerminationGracePeriodSeconds != 120 || *clone.PreStopSleepSeconds != 15 {
		t.Errorf("DeepCopy altered values: grace=%d preStop=%d",
			*clone.TerminationGracePeriodSeconds, *clone.PreStopSleepSeconds)
	}
}

// TestKeystoneSpecStrategyField verifies the optional *appsv1.DeploymentStrategy
// field round-trips through DeepCopy with independent memory for the
// pointer and nested RollingUpdate block at the type level.
func TestKeystoneSpecStrategyField(t *testing.T) {
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromInt(1)
	spec := KeystoneSpec{
		Strategy: &appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			},
		},
	}

	if spec.Strategy == nil {
		t.Fatal("expected non-nil Strategy")
	}
	if spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("expected RollingUpdate type, got %q", spec.Strategy.Type)
	}

	clone := spec.DeepCopy()
	if clone.Strategy == spec.Strategy {
		t.Errorf("DeepCopy did not allocate a new *DeploymentStrategy")
	}
	if clone.Strategy.RollingUpdate == spec.Strategy.RollingUpdate {
		t.Errorf("DeepCopy did not allocate a new *RollingUpdateDeployment")
	}
	if clone.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 ||
		clone.Strategy.RollingUpdate.MaxSurge.IntVal != 1 {
		t.Errorf("DeepCopy altered RollingUpdate values: %+v", clone.Strategy.RollingUpdate)
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
	if uwsgi.HTTPKeepAlive != nil {
		t.Errorf("expected nil HTTPKeepAlive, got %v", *uwsgi.HTTPKeepAlive)
	}
	if uwsgi.Harakiri != nil {
		t.Errorf("expected nil Harakiri, got %v", uwsgi.Harakiri)
	}
	if uwsgi.HTTPKeepAliveTimeout != nil {
		t.Errorf("expected nil HTTPKeepAliveTimeout, got %v", uwsgi.HTTPKeepAliveTimeout)
	}
}

// TestUWSGISpecOptionalTimeoutFields verifies that Harakiri and
// HTTPKeepAliveTimeout are settable optional *int32 pointers with DeepCopy
// allocating independent storage, at the type level.
func TestUWSGISpecOptionalTimeoutFields(t *testing.T) {
	harakiri := int32(20)
	keepAlive := int32(5)
	uwsgi := UWSGISpec{
		Harakiri:             &harakiri,
		HTTPKeepAliveTimeout: &keepAlive,
	}

	if uwsgi.Harakiri == nil || *uwsgi.Harakiri != 20 {
		t.Errorf("expected Harakiri=20, got %v", uwsgi.Harakiri)
	}
	if uwsgi.HTTPKeepAliveTimeout == nil || *uwsgi.HTTPKeepAliveTimeout != 5 {
		t.Errorf("expected HTTPKeepAliveTimeout=5, got %v", uwsgi.HTTPKeepAliveTimeout)
	}

	clone := uwsgi.DeepCopy()
	if clone.Harakiri == uwsgi.Harakiri {
		t.Errorf("DeepCopy did not allocate a new *int32 for Harakiri")
	}
	if clone.HTTPKeepAliveTimeout == uwsgi.HTTPKeepAliveTimeout {
		t.Errorf("DeepCopy did not allocate a new *int32 for HTTPKeepAliveTimeout")
	}
	if *clone.Harakiri != 20 || *clone.HTTPKeepAliveTimeout != 5 {
		t.Errorf("DeepCopy altered values: harakiri=%d keepAlive=%d",
			*clone.Harakiri, *clone.HTTPKeepAliveTimeout)
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

// TestKeystoneSpecPasswordRotationField verifies the optional
// spec.passwordRotation *PasswordRotationSpec field round-trips through DeepCopy
// with independent memory for the pointer, so mutating the clone does not alias
// the original; webhook defaulting/validation is covered separately in
// keystone_webhook_test.go.
func TestKeystoneSpecPasswordRotationField(t *testing.T) {
	spec := KeystoneSpec{
		PasswordRotation: &PasswordRotationSpec{
			Enabled:        true,
			Schedule:       "0 0 1 * *",
			Suspend:        false,
			PasswordLength: 32,
		},
	}

	clone := spec.DeepCopy()
	if clone.PasswordRotation == spec.PasswordRotation {
		t.Errorf("DeepCopy did not allocate a new *PasswordRotationSpec")
	}
	if *clone.PasswordRotation != *spec.PasswordRotation {
		t.Errorf("DeepCopy altered PasswordRotation values: got %+v want %+v",
			clone.PasswordRotation, spec.PasswordRotation)
	}
	// Mutating the clone must not affect the original (independent memory).
	clone.PasswordRotation.Schedule = "*/5 * * * *"
	if spec.PasswordRotation.Schedule != "0 0 1 * *" {
		t.Errorf("DeepCopy aliased PasswordRotation: mutating clone changed original schedule to %q",
			spec.PasswordRotation.Schedule)
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
