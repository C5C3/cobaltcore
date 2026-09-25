// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// TestMergedExtraConfig_PerServiceWinsPerKey verifies the merge semantics: a key
// present in both blocks yields the per-service value, global-only keys in the
// same section survive, and global-only sections survive.
func TestMergedExtraConfig_PerServiceWinsPerKey(t *testing.T) {
	g := NewGomegaWithT(t)

	global := map[string]map[string]string{
		"DEFAULT": {
			"debug":         "false",
			"transport_url": "rabbit://global",
		},
		"oslo_messaging": {
			"driver": "messagingv2",
		},
	}
	service := map[string]map[string]string{
		"DEFAULT": {
			"debug": "true",
		},
	}

	merged := MergedExtraConfig(global, service)

	// Per-service value wins the shared key.
	g.Expect(merged["DEFAULT"]["debug"]).To(Equal("true"))
	// Global-only key in a shared section survives.
	g.Expect(merged["DEFAULT"]["transport_url"]).To(Equal("rabbit://global"))
	// Global-only section survives.
	g.Expect(merged["oslo_messaging"]["driver"]).To(Equal("messagingv2"))
}

// TestMergedExtraConfig_NilWhenBothNil verifies two nil inputs normalize to a
// nil result rather than an empty map, so an absent field is projected.
func TestMergedExtraConfig_NilWhenBothNil(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(MergedExtraConfig(nil, nil)).To(BeNil())
}

// TestMergedExtraConfig_NilWhenBothEmpty verifies two empty (non-nil) maps
// normalize to nil, guarding the length-zero normalization against an empty
// merge result config.MergeDefaults would otherwise return.
func TestMergedExtraConfig_NilWhenBothEmpty(t *testing.T) {
	g := NewGomegaWithT(t)

	merged := MergedExtraConfig(
		map[string]map[string]string{},
		map[string]map[string]string{},
	)
	g.Expect(merged).To(BeNil())
}

// TestMergedExtraConfig_ReturnsIndependentCopy verifies mutating the returned
// map never writes back through to the single input that was set, exercising
// both the global-only and the service-only paths.
func TestMergedExtraConfig_ReturnsIndependentCopy(t *testing.T) {
	g := NewGomegaWithT(t)

	t.Run("only global set", func(t *testing.T) {
		global := map[string]map[string]string{
			"DEFAULT": {"debug": "false"},
		}
		merged := MergedExtraConfig(global, nil)

		// Add a key to an existing section and a whole new section.
		merged["DEFAULT"]["extra"] = "1"
		merged["new_section"] = map[string]string{"k": "v"}

		g.Expect(global["DEFAULT"]).To(HaveLen(1),
			"mutating the merged map must not add a key to the global input")
		g.Expect(global).NotTo(HaveKey("new_section"),
			"mutating the merged map must not add a section to the global input")
	})

	t.Run("only service set", func(t *testing.T) {
		service := map[string]map[string]string{
			"DEFAULT": {"debug": "true"},
		}
		merged := MergedExtraConfig(nil, service)

		merged["DEFAULT"]["extra"] = "1"
		merged["new_section"] = map[string]string{"k": "v"}

		g.Expect(service["DEFAULT"]).To(HaveLen(1),
			"mutating the merged map must not add a key to the service input")
		g.Expect(service).NotTo(HaveKey("new_section"),
			"mutating the merged map must not add a section to the service input")
	})
}

// TestDerivedPublicEndpoint pins the shared derivation used by both the
// identity-backend projection and the admission-time trusted-dashboard check.
func TestDerivedPublicEndpoint(t *testing.T) {
	g := NewGomegaWithT(t)

	for _, tc := range []struct {
		name string
		hz   *ServiceHorizonSpec
		want string
	}{
		{
			name: "nil receiver",
			hz:   nil,
			want: "",
		},
		{
			name: "explicit publicEndpoint with trailing slash is trimmed",
			hz:   &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com:8443/"},
			want: "https://horizon.example.com:8443",
		},
		{
			name: "explicit publicEndpoint without slash is verbatim",
			hz:   &ServiceHorizonSpec{PublicEndpoint: "https://horizon.example.com"},
			want: "https://horizon.example.com",
		},
		{
			name: "gateway hostname derives the default-443 form",
			hz:   &ServiceHorizonSpec{Gateway: &commonv1.GatewaySpec{Hostname: "horizon.127-0-0-1.nip.io"}},
			want: "https://horizon.127-0-0-1.nip.io",
		},
		{
			name: "neither set yields empty",
			hz:   &ServiceHorizonSpec{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.Expect(tc.hz.DerivedPublicEndpoint()).To(Equal(tc.want))
		})
	}
}

// TestValidateExtraConfigOwnership_ForbidsBarbicanRejectedKeys pins the three
// Rejected barbican keys. None of them is read from the rendered file at runtime
// — each arrives through an env override — so honoring the override buys
// nothing and copies credential material into the config Secret every API pod
// mounts. The check runs from whichever block carries the key, since the merged
// value is what would be rendered.
func TestValidateExtraConfigOwnership_ForbidsBarbicanRejectedKeys(t *testing.T) {
	for _, tc := range []struct {
		section string
		key     string
	}{
		{"keystone_authtoken", "password"},
		{"vault_plugin", "approle_secret_id"},
		{"vault_plugin", "root_token_id"},
	} {
		t.Run(tc.section+" "+tc.key, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := barbicanControlPlane()
			cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
				tc.section: {tc.key: "s3cr3t"},
			}

			_, errs := validateExtraConfigOwnership(cp, nil)
			g.Expect(errs.ToAggregate()).To(HaveOccurred())
			g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
				"spec.services.barbican.extraConfig[" + tc.section + "][" + tc.key + "]"))
			g.Expect(errs.ToAggregate().Error()).To(ContainSubstring("must not be set in extraConfig"))
		})
	}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"vault_plugin": {"root_token_id": "s.rootrootroot"},
		}

		_, errs := validateExtraConfigOwnership(cp, nil)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"spec.globalExtraConfig[vault_plugin][root_token_id]"))
	})
}

// TestValidateExtraConfigCatalogs_ExemptsBarbicanStoreSectionsByPrefix pins the
// prefix expansion barbican needs and no other service does: the per-store
// [secretstore:<name>] sections are named after the BarbicanSecretStore CRs
// attached to a Barbican, so no release catalog lists them and no static
// exemption can spell them. Every other unknown section still fails, which is
// what keeps the exemption from swallowing real typos.
func TestValidateExtraConfigCatalogs_ExemptsBarbicanStoreSectionsByPrefix(t *testing.T) {
	t.Run("a per-store section is skipped whole", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"secretstore:foo": {"global_default": "True", "secret_store_plugin": "vault_plugin"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs).To(BeEmpty())
	})

	t.Run("a section that merely resembles one is not", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"secretstore_foo": {"global_default": "True"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"spec.services.barbican.extraConfig[secretstore_foo][global_default]"))
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"no such section in the barbican 2025.2 option catalog"))
	})

	t.Run("an unknown option in a known section is rejected", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"host_hrefs": "https://barbican.example.com"},
		}

		_, errs := validateExtraConfigCatalogs(cp)
		g.Expect(errs.ToAggregate()).To(HaveOccurred())
		g.Expect(errs.ToAggregate().Error()).To(ContainSubstring(
			"no such option in the barbican 2025.2 option catalog"))
	})
}

// TestControlPlaneExtraConfigCatalogInputsChanged_Barbican pins the update gate
// for the barbican leg: the catalog family re-runs when the barbican block is
// added, dropped, or edited, and stays gated off for an update that leaves it
// alone.
func TestControlPlaneExtraConfigCatalogInputsChanged_Barbican(t *testing.T) {
	withExtraConfig := func(cfg map[string]map[string]string) *ControlPlane {
		cp := barbicanControlPlane()
		cp.Spec.Services.Barbican.ExtraConfig = cfg
		return cp
	}

	for _, tc := range []struct {
		name     string
		oldCP    *ControlPlane
		newCP    *ControlPlane
		expected bool
	}{
		{
			name:     "the barbican block is newly declared",
			oldCP:    validControlPlane(),
			newCP:    barbicanControlPlane(),
			expected: true,
		},
		{
			name:     "the barbican block is dropped",
			oldCP:    barbicanControlPlane(),
			newCP:    validControlPlane(),
			expected: true,
		},
		{
			name:     "the barbican extraConfig changes",
			oldCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "false"}}),
			newCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "true"}}),
			expected: true,
		},
		{
			name:  "an unrelated barbican edit leaves the gate closed",
			oldCP: barbicanControlPlane(),
			newCP: func() *ControlPlane {
				cp := barbicanControlPlane()
				cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
					Barbican: &APIServiceSizingSpec{API: &APISizingSpec{DeploymentSizingSpec: deploymentReplicas(5)}},
				}}
				return cp
			}(),
			expected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(controlPlaneExtraConfigCatalogInputsChanged(tc.oldCP, tc.newCP)).To(Equal(tc.expected))
		})
	}
}

// TestValidateCreate_RejectsUnknownNeutronExtraConfigOption pins the neutron
// catalog leg from both sides: a per-service override the catalog does not
// accept, and the cross-service reach of globalExtraConfig, which is validated
// against the catalog of every declared service.
func TestValidateCreate_RejectsUnknownNeutronExtraConfigOption(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in neutron extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.ExtraConfig = map[string]map[string]string{
			"ovn": {"ovn_l3_schedulerz": "leastloaded"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.extraConfig[ovn][ovn_l3_schedulerz]"))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the neutron 2025.2 option catalog"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"ml2": {"mechanism_driverz": "ovn"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[ml2][mechanism_driverz]"))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the neutron 2025.2 option catalog"))
	})
}

// TestValidateCreate_ForbidsNeutronRejectedOwnedKey pins a Rejected neutron
// owned key from whichever block carries it. [ovn] ovn_nb_connection is the one
// to guard: the merged block has the last word, so honoring it points the
// ML2/OVN mechanism driver at a Northbound database the ControlPlane does not
// own, and every network, subnet and port lands in a logical model belonging to
// another deployment.
func TestValidateCreate_ForbidsNeutronRejectedOwnedKey(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in neutron extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.ExtraConfig = map[string]map[string]string{
			"ovn": {"ovn_nb_connection": "tcp:10.0.0.1:6641"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.extraConfig[ovn][ovn_nb_connection]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"ovn_nb_connection is managed via spec.ovn.centralRef and must not be set in extraConfig"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := neutronControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"ovn": {"ovn_nb_connection": "tcp:10.0.0.1:6641"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[ovn][ovn_nb_connection]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})
}

// TestValidateUpdate_CarriedNeutronRejectedKeyStaysUpdatable covers a Rejected
// neutron key a stored ControlPlane was admitted with before the neutron operator
// owned it: [nova] password was the documented way to configure the notifier
// before spec.nova existed. Forbidding it on every update would forbid the
// finalizer removal too, so a carried-over value is kept with a warning while a
// new or changed one is still forbidden.
func TestValidateUpdate_CarriedNeutronRejectedKeyStaysUpdatable(t *testing.T) {
	w := &ControlPlaneWebhook{}
	stored := func() *ControlPlane {
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.ExtraConfig = map[string]map[string]string{
			"nova": {"password": "hunter2"},
		}
		return cp
	}

	t.Run("an unrelated edit carries it over", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := stored()
		newCP := oldCP.DeepCopy()
		newCP.Finalizers = nil

		warnings, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(ContainElement(And(
			ContainSubstring("[nova] password is managed via spec.nova.serviceUser.secretRef"),
			ContainSubstring("spec.services.neutron.extraConfig[nova][password]"),
		)))
	})

	t.Run("a changed value is forbidden", func(t *testing.T) {
		g := NewGomegaWithT(t)
		oldCP := stored()
		newCP := oldCP.DeepCopy()
		newCP.Spec.Services.Neutron.ExtraConfig["nova"]["password"] = "rotated"

		_, err := w.ValidateUpdate(context.Background(), oldCP, newCP)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(
			"password is managed via spec.nova.serviceUser.secretRef and must not be set in extraConfig"))
	})

	t.Run("a value the stored object never carried is forbidden", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := w.ValidateUpdate(context.Background(), neutronControlPlane(), stored())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.neutron.extraConfig[nova][password]"))
	})

	t.Run("create still forbids it", func(t *testing.T) {
		g := NewGomegaWithT(t)
		_, err := w.ValidateCreate(context.Background(), stored())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})
}

// TestValidateCreate_AcceptsEmptyNeutronExtraConfig pins that a declared but
// empty block is admitted and raises nothing: the merge normalizes it to nil, so
// there is no config to scan and no catalog to fail open on.
func TestValidateCreate_AcceptsEmptyNeutronExtraConfig(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, cfg := range map[string]map[string]map[string]string{
		"an empty map": {},
		"nil":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := neutronControlPlane()
			cp.Spec.Services.Neutron.ExtraConfig = cfg

			warnings, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(warnings).To(BeEmpty())
		})
	}
}

// TestControlPlaneExtraConfigCatalogInputsChanged_Neutron pins the update gate
// for the neutron leg: the catalog family re-runs when the neutron block is
// added, dropped, or edited, and stays gated off for an update that leaves it
// alone.
func TestControlPlaneExtraConfigCatalogInputsChanged_Neutron(t *testing.T) {
	withoutNeutron := func() *ControlPlane {
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron = nil
		return cp
	}
	withExtraConfig := func(cfg map[string]map[string]string) *ControlPlane {
		cp := neutronControlPlane()
		cp.Spec.Services.Neutron.ExtraConfig = cfg
		return cp
	}

	for _, tc := range []struct {
		name     string
		oldCP    *ControlPlane
		newCP    *ControlPlane
		expected bool
	}{
		{
			name:     "the neutron block is newly declared",
			oldCP:    withoutNeutron(),
			newCP:    neutronControlPlane(),
			expected: true,
		},
		{
			name:     "the neutron block is dropped",
			oldCP:    neutronControlPlane(),
			newCP:    withoutNeutron(),
			expected: true,
		},
		{
			name:     "the neutron extraConfig changes",
			oldCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "false"}}),
			newCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "true"}}),
			expected: true,
		},
		{
			name:  "an unrelated neutron edit leaves the gate closed",
			oldCP: neutronControlPlane(),
			newCP: func() *ControlPlane {
				cp := neutronControlPlane()
				cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
					Neutron: &NeutronSizingSpec{API: &APISizingSpec{DeploymentSizingSpec: deploymentReplicas(5)}},
				}}
				return cp
			}(),
			expected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(controlPlaneExtraConfigCatalogInputsChanged(tc.oldCP, tc.newCP)).To(Equal(tc.expected))
		})
	}
}

// TestValidateCreate_RejectsUnknownCinderExtraConfigOption pins the cinder
// catalog leg from both sides: a per-service override the catalog does not
// accept, and the cross-service reach of globalExtraConfig, which is validated
// against the catalog of every declared service.
func TestValidateCreate_RejectsUnknownCinderExtraConfigOption(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in cinder extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := cinderControlPlane()
		cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"volume_name_templatez": "volume-%s"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.cinder.extraConfig[DEFAULT][volume_name_templatez]"))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the cinder 2025.2 option catalog"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := cinderControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT": {"volume_name_templatez": "volume-%s"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[DEFAULT][volume_name_templatez]"))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the cinder 2025.2 option catalog"))
	})
}

// TestValidateCreate_ForbidsCinderRejectedOwnedKey pins a Rejected cinder owned
// key from whichever block carries it. [DEFAULT] transport_url is the one to
// guard: the broker URL reaches the pods through OS_DEFAULT__TRANSPORT_URL, so a
// file value changes nothing at runtime and only copies the broker credentials
// into the config Secret every cinder pod mounts.
func TestValidateCreate_ForbidsCinderRejectedOwnedKey(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in cinder extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := cinderControlPlane()
		cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"transport_url": "rabbit://user:pw@broker:5672/"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.cinder.extraConfig[DEFAULT][transport_url]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"transport_url is managed via spec.messaging and must not be set in extraConfig"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := cinderControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT": {"transport_url": "rabbit://user:pw@broker:5672/"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[DEFAULT][transport_url]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})
}

// cinderAndBarbicanControlPlane returns the cinder baseline with the minimal
// barbican block beside it, the shape in which the ControlPlane projects the
// Cinder child's keyManager from the Barbican child.
func cinderAndBarbicanControlPlane() *ControlPlane {
	cp := cinderControlPlane()
	cp.Spec.Services.Barbican = &ServiceBarbicanSpec{
		SecretStore: ServiceBarbicanSecretStoreSpec{Dedicated: &BarbicanDedicatedSecretStoreSpec{}},
	}
	return cp
}

// TestValidateCreate_ForbidsCinderKeyManagerOverrideBesideBarbican pins the one
// cinder rule the registry alone does not carry. The three key-manager keys are
// Reported there, because a Cinder child accepts a Barbican it did not
// provision; beside a declared services.barbican the ControlPlane owns them, so
// an override points castellan at a key manager the plane never provisioned and
// every encrypted volume is written with, or read against, the wrong keys.
func TestValidateCreate_ForbidsCinderKeyManagerOverrideBesideBarbican(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		section string
		key     string
		value   string
	}{
		{"key_manager", "backend", "barbican.key_manager.API"},
		{"barbican", "barbican_endpoint", "https://barbican.example.com"},
		{"barbican", "auth_endpoint", "https://keystone.example.com/v3"},
	} {
		t.Run(tc.section+" "+tc.key, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderAndBarbicanControlPlane()
			cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
				tc.section: {tc.key: tc.value},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(
				"spec.services.cinder.extraConfig[" + tc.section + "][" + tc.key + "]"))
			g.Expect(err.Error()).To(ContainSubstring(
				"is projected by the ControlPlane from services.barbican"))
		})
	}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := cinderAndBarbicanControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"key_manager": {"backend": "barbican.key_manager.API"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[key_manager][backend]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"is projected by the ControlPlane from services.barbican"))
	})
}

// TestValidateCreate_ReportsCinderKeyManagerOverrideWithoutBarbican pins the
// other half of the same rule: with no services.barbican the ControlPlane
// projects no key manager, so pointing cinder at an externally-run Barbican is
// legitimate and the override is honored-but-reported, the way the cinder
// registry classifies it.
func TestValidateCreate_ReportsCinderKeyManagerOverrideWithoutBarbican(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := cinderControlPlane()
	cp.Spec.Services.Cinder.ExtraConfig = map[string]map[string]string{
		"barbican": {"barbican_endpoint": "https://barbican.example.com"},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(ContainSubstring("barbican_endpoint")))
}

// TestValidateCreate_AcceptsEmptyCinderExtraConfig pins that a declared but
// empty block is admitted and raises nothing: the merge normalizes it to nil, so
// there is no config to scan and no catalog to fail open on.
func TestValidateCreate_AcceptsEmptyCinderExtraConfig(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, cfg := range map[string]map[string]map[string]string{
		"an empty map": {},
		"nil":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := cinderControlPlane()
			cp.Spec.Services.Cinder.ExtraConfig = cfg

			warnings, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(warnings).To(BeEmpty())
		})
	}
}

// TestControlPlaneExtraConfigCatalogInputsChanged_Cinder pins the update gate
// for the cinder leg: the catalog family re-runs when the cinder block is added,
// dropped, or edited, and stays gated off for an update that leaves it alone.
func TestControlPlaneExtraConfigCatalogInputsChanged_Cinder(t *testing.T) {
	withoutCinder := func() *ControlPlane {
		cp := cinderControlPlane()
		cp.Spec.Services.Cinder = nil
		return cp
	}
	withExtraConfig := func(cfg map[string]map[string]string) *ControlPlane {
		cp := cinderControlPlane()
		cp.Spec.Services.Cinder.ExtraConfig = cfg
		return cp
	}

	for _, tc := range []struct {
		name     string
		oldCP    *ControlPlane
		newCP    *ControlPlane
		expected bool
	}{
		{
			name:     "the cinder block is newly declared",
			oldCP:    withoutCinder(),
			newCP:    cinderControlPlane(),
			expected: true,
		},
		{
			name:     "the cinder block is dropped",
			oldCP:    cinderControlPlane(),
			newCP:    withoutCinder(),
			expected: true,
		},
		{
			name:     "the cinder extraConfig changes",
			oldCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "false"}}),
			newCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "true"}}),
			expected: true,
		},
		{
			name:  "an unrelated cinder edit leaves the gate closed",
			oldCP: cinderControlPlane(),
			newCP: func() *ControlPlane {
				cp := cinderControlPlane()
				cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
					Cinder: &CinderSizingSpec{API: &APISizingSpec{DeploymentSizingSpec: deploymentReplicas(5)}},
				}}
				return cp
			}(),
			expected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(controlPlaneExtraConfigCatalogInputsChanged(tc.oldCP, tc.newCP)).To(Equal(tc.expected))
		})
	}
}

// TestValidateCreate_RejectsUnknownNovaExtraConfigOption pins the nova catalog
// leg from both sides, and for both releases this build embeds a catalog for: a
// per-service override the catalog does not accept, and the cross-service reach
// of globalExtraConfig, which is validated against the catalog of every declared
// service.
func TestValidateCreate_RejectsUnknownNovaExtraConfigOption(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for _, release := range []string{"2025.2", "2026.1"} {
		t.Run("in nova extraConfig on "+release, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.OpenStackRelease = release
			cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
				"DEFAULT": {"cpu_allocation_ratioz": "4.0"},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services.nova.extraConfig[DEFAULT][cpu_allocation_ratioz]"))
			g.Expect(err.Error()).To(ContainSubstring("no such option in the nova " + release + " option catalog"))
		})
	}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT": {"cpu_allocation_ratioz": "4.0"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[DEFAULT][cpu_allocation_ratioz]"))
		g.Expect(err.Error()).To(ContainSubstring("no such option in the nova 2025.2 option catalog"))
	})

	t.Run("a known option is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"cpu_allocation_ratio": "4.0"},
		}

		warnings, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})
}

// TestValidateCreate_RejectsMalformedNovaExtraConfigBlock pins the shape check on
// the nova block. The rendered INI writes every section name, key and value
// verbatim, so a newline in one of them injects config lines the ownership and
// catalog gates never saw, both of which inspect map structure alone.
func TestValidateCreate_RejectsMalformedNovaExtraConfigBlock(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, tc := range map[string]struct {
		config map[string]map[string]string
		detail string
	}{
		"an empty section name": {
			config: map[string]map[string]string{"": {"cpu_allocation_ratio": "4.0"}},
			detail: "extraConfig section name must not be empty",
		},
		"an empty key": {
			config: map[string]map[string]string{"DEFAULT": {"": "4.0"}},
			detail: "extraConfig key must not be empty",
		},
		"a smuggled section in a value": {
			config: map[string]map[string]string{
				"DEFAULT": {"cpu_allocation_ratio": "4.0\n[vnc]\nnovncproxy_port = 1"},
			},
			detail: "must not contain a newline or carriage return",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Nova.ExtraConfig = tc.config

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services.nova.extraConfig"))
			g.Expect(err.Error()).To(ContainSubstring(tc.detail))
		})
	}
}

// TestValidateCreate_ForbidsNovaRejectedOwnedKey pins a Rejected nova owned key
// from whichever block carries it. [DEFAULT] transport_url is the one to guard:
// the broker URL reaches the pods through OS_DEFAULT__TRANSPORT_URL, so a file
// value changes nothing at runtime and only copies the broker credentials into
// the config Secret every nova pod mounts.
func TestValidateCreate_ForbidsNovaRejectedOwnedKey(t *testing.T) {
	w := &ControlPlaneWebhook{}

	t.Run("in nova extraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
			"DEFAULT": {"transport_url": "rabbit://user:pw@broker:5672/"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.nova.extraConfig[DEFAULT][transport_url]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"transport_url is managed via spec.messaging and must not be set in extraConfig"))
	})

	t.Run("the console listen port", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
			"vnc": {"novncproxy_port": "6081"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.services.nova.extraConfig[vnc][novncproxy_port]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"DEFAULT": {"transport_url": "rabbit://user:pw@broker:5672/"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[DEFAULT][transport_url]"))
		g.Expect(err.Error()).To(ContainSubstring("must not be set in extraConfig"))
	})
}

// novaAndCinderControlPlane returns the nova baseline with the minimal
// block-storage block beside it, the shape in which the ControlPlane switches
// the Nova child's [cinder] section on from the sibling.
func novaAndCinderControlPlane() *ControlPlane {
	cp := novaControlPlane()
	cp.Spec.Services.Cinder = cinderControlPlane().Spec.Services.Cinder
	return cp
}

// novaAndBarbicanControlPlane returns the nova baseline with the minimal
// key-manager block beside it, the shape in which the ControlPlane switches the
// Nova child's [key_manager] and [barbican] sections on from the sibling.
func novaAndBarbicanControlPlane() *ControlPlane {
	cp := novaControlPlane()
	cp.Spec.Services.Barbican = &ServiceBarbicanSpec{
		SecretStore: ServiceBarbicanSecretStoreSpec{Dedicated: &BarbicanDedicatedSecretStoreSpec{}},
	}
	return cp
}

// TestValidateCreate_ForbidsNovaCinderSectionOverrideBesideCinder pins the
// takeover the registry alone does not carry. The [cinder] keys are Reported
// there, because a Nova child accepts a volume service it did not provision;
// beside a declared services.cinder the ControlPlane projects the section, so an
// override addresses a volume service the plane does own and every attachment
// goes to the wrong API.
func TestValidateCreate_ForbidsNovaCinderSectionOverrideBesideCinder(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		key   string
		value string
	}{
		{"catalog_info", "volumev3:cinderv3:publicURL"},
		{"endpoint_template", "https://cinder.example.com/v3/%(project_id)s"},
		{"os_region_name", "RegionTwo"},
	} {
		t.Run("cinder "+tc.key, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaAndCinderControlPlane()
			cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
				"cinder": {tc.key: tc.value},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("spec.services.nova.extraConfig[cinder][" + tc.key + "]"))
			g.Expect(err.Error()).To(ContainSubstring(
				"is projected by the ControlPlane from services.cinder"))
			g.Expect(err.Error()).To(ContainSubstring("remove the override or unset services.cinder"))
		})
	}

	t.Run("in globalExtraConfig", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cp := novaAndCinderControlPlane()
		cp.Spec.GlobalExtraConfig = map[string]map[string]string{
			"cinder": {"catalog_info": "volumev3:cinderv3:publicURL"},
		}

		_, err := w.ValidateCreate(context.Background(), cp)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.globalExtraConfig[cinder][catalog_info]"))
		g.Expect(err.Error()).To(ContainSubstring(
			"is projected by the ControlPlane from services.cinder"))
	})
}

// TestValidateCreate_ReportsNovaCinderSectionOverrideWithoutCinder pins the
// other half of the same rule: with no services.cinder the ControlPlane projects
// no volume service, so pointing nova at an externally-run one is legitimate and
// the override is honored-but-reported, the way the nova registry classifies it.
func TestValidateCreate_ReportsNovaCinderSectionOverrideWithoutCinder(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := novaControlPlane()
	cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
		"cinder": {"catalog_info": "volumev3:cinderv3:publicURL"},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(ContainSubstring("catalog_info")))
}

// TestValidateCreate_ForbidsNovaKeyManagerOverrideBesideBarbican pins the second
// takeover: the [key_manager] and [barbican] keys follow services.barbican
// exactly as the [cinder] ones follow services.cinder, and an override beside it
// points castellan at a key manager the plane never provisioned, so every
// encrypted volume nova attaches is unlocked against the wrong keys.
func TestValidateCreate_ForbidsNovaKeyManagerOverrideBesideBarbican(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for _, tc := range []struct {
		section string
		key     string
		value   string
	}{
		{"key_manager", "backend", "barbican.key_manager.API"},
		{"barbican", "barbican_endpoint", "https://barbican.example.com"},
		{"barbican", "auth_endpoint", "https://keystone.example.com/v3"},
	} {
		t.Run(tc.section+" "+tc.key, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaAndBarbicanControlPlane()
			cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
				tc.section: {tc.key: tc.value},
			}

			_, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(
				"spec.services.nova.extraConfig[" + tc.section + "][" + tc.key + "]"))
			g.Expect(err.Error()).To(ContainSubstring(
				"is projected by the ControlPlane from services.barbican"))
		})
	}
}

// TestValidateCreate_ReportsNovaKeyManagerOverrideWithoutBarbican pins the other
// half: with no services.barbican the ControlPlane projects no key manager, so
// the override is honored-but-reported.
func TestValidateCreate_ReportsNovaKeyManagerOverrideWithoutBarbican(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := novaControlPlane()
	cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
		"barbican": {"barbican_endpoint": "https://barbican.example.com"},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(ContainSubstring("barbican_endpoint")))
}

// TestValidateCreate_ReportsNovaOwnedKeyOutsideSiblingSections pins the default
// branch of the sibling-section rule: an ordinary operator-owned key such as
// [DEFAULT] debug belongs to no sibling block, so it is honored-but-reported, the
// way the nova registry classifies it. A default that answered "declared" would
// turn every such override into a hard error on each update, the finalizer
// removal included.
func TestValidateCreate_ReportsNovaOwnedKeyOutsideSiblingSections(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	// Both sibling blocks are declared, so the key is reported for its section
	// alone and not because no sibling happens to be present.
	cp := novaAndCinderControlPlane()
	cp.Spec.Services.Barbican = novaAndBarbicanControlPlane().Spec.Services.Barbican
	cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"debug": "true"},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(ContainElement(And(
		ContainSubstring("[DEFAULT] debug overrides an operator-owned key"),
		ContainSubstring("spec.services.nova.extraConfig[DEFAULT][debug]"),
	)))
}

// TestValidateCreate_AcceptsEmptyNovaExtraConfig pins that a declared but empty
// block is admitted and raises nothing: the merge normalizes it to nil, so there
// is no config to scan and no catalog to fail open on.
func TestValidateCreate_AcceptsEmptyNovaExtraConfig(t *testing.T) {
	w := &ControlPlaneWebhook{}

	for name, cfg := range map[string]map[string]map[string]string{
		"an empty map": {},
		"nil":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := novaControlPlane()
			cp.Spec.Services.Nova.ExtraConfig = cfg

			warnings, err := w.ValidateCreate(context.Background(), cp)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(warnings).To(BeEmpty())
		})
	}
}

// TestValidateCreate_NovaUnknownReleaseWarnsOnce pins the fail-open for the nova
// leg: a syntactically valid release this build ships no nova catalog for yields
// exactly one warning and no error, so a plane pinned ahead of the operator
// still admits.
func TestValidateCreate_NovaUnknownReleaseWarnsOnce(t *testing.T) {
	g := NewGomegaWithT(t)
	w := &ControlPlaneWebhook{}
	cp := novaControlPlane()
	cp.Spec.OpenStackRelease = "2027.1"
	cp.Spec.Services.Nova.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"cpu_allocation_ratio": "4.0"},
	}

	warnings, err := w.ValidateCreate(context.Background(), cp)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(HaveLen(1))
	g.Expect(warnings[0]).To(ContainSubstring("nova"))
	g.Expect(warnings[0]).To(ContainSubstring(`no catalog for release "2027.1"`))
}

// TestControlPlaneExtraConfigCatalogInputsChanged_Nova pins the update gate for
// the nova leg: the catalog family re-runs when the nova block is added,
// dropped, or edited, and stays gated off for an update that leaves it alone.
func TestControlPlaneExtraConfigCatalogInputsChanged_Nova(t *testing.T) {
	withoutNova := func() *ControlPlane {
		cp := novaControlPlane()
		cp.Spec.Services.Nova = nil
		return cp
	}
	withExtraConfig := func(cfg map[string]map[string]string) *ControlPlane {
		cp := novaControlPlane()
		cp.Spec.Services.Nova.ExtraConfig = cfg
		return cp
	}

	for _, tc := range []struct {
		name     string
		oldCP    *ControlPlane
		newCP    *ControlPlane
		expected bool
	}{
		{
			name:     "the nova block is newly declared",
			oldCP:    withoutNova(),
			newCP:    novaControlPlane(),
			expected: true,
		},
		{
			name:     "the nova block is dropped",
			oldCP:    novaControlPlane(),
			newCP:    withoutNova(),
			expected: true,
		},
		{
			name:     "the nova extraConfig changes",
			oldCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "false"}}),
			newCP:    withExtraConfig(map[string]map[string]string{"DEFAULT": {"debug": "true"}}),
			expected: true,
		},
		{
			name:  "an unrelated nova edit leaves the gate closed",
			oldCP: novaControlPlane(),
			newCP: func() *ControlPlane {
				cp := novaControlPlane()
				cp.Spec.Sizing = &ControlPlaneSizingSpec{SizingSpec: SizingSpec{
					Nova: &NovaSizingSpec{API: &APISizingSpec{DeploymentSizingSpec: deploymentReplicas(5)}},
				}}
				return cp
			}(),
			expected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(controlPlaneExtraConfigCatalogInputsChanged(tc.oldCP, tc.newCP)).To(Equal(tc.expected))
		})
	}
}
