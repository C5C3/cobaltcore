// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
}
