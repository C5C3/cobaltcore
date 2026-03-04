// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Feature: CC-0001

func TestManagerConfig_validate_nilScheme(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           nil,
		LeaderElectionID: "test.c5c3.io",
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for nil Scheme, got nil")
	}
	if err.Error() != "bootstrap: Scheme must not be nil" {
		t.Fatalf("unexpected error message: %s", err.Error())
	}
}

func TestManagerConfig_validate_emptyLeaderElectionID(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "",
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for empty LeaderElectionID, got nil")
	}
	if err.Error() != "bootstrap: LeaderElectionID must not be empty" {
		t.Fatalf("unexpected error message: %s", err.Error())
	}
}

func TestManagerConfig_validate_valid(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config, got: %v", err)
	}
}

func TestManagerConfig_validate_validWithSetupFunc(t *testing.T) {
	cfg := ManagerConfig{
		Scheme:           runtime.NewScheme(),
		LeaderElectionID: "test.c5c3.io",
		SetupFunc: func(_ ctrl.Manager) error {
			return nil
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("expected no error for valid config with SetupFunc, got: %v", err)
	}
}
