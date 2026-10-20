// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Tests for the ControlPlane controller skeleton.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	barbicanv1alpha1 "github.com/c5c3/cobaltcore/operators/barbican/api/v1alpha1"
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
	cinderv1alpha1 "github.com/c5c3/cobaltcore/operators/cinder/api/v1alpha1"
	glancev1alpha1 "github.com/c5c3/cobaltcore/operators/glance/api/v1alpha1"
	horizonv1alpha1 "github.com/c5c3/cobaltcore/operators/horizon/api/v1alpha1"
	keystonev1alpha1 "github.com/c5c3/cobaltcore/operators/keystone/api/v1alpha1"
	neutronv1alpha1 "github.com/c5c3/cobaltcore/operators/neutron/api/v1alpha1"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
	placementv1alpha1 "github.com/c5c3/cobaltcore/operators/placement/api/v1alpha1"
)

// controllerTestScheme returns a runtime.Scheme with the c5c3 ControlPlane type
// registered alongside the core client-go types.
func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := c5c3v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding c5c3 scheme: %v", err)
	}
	return s
}

// trueCondition returns a metav1.Condition of the given type with status True.
func trueCondition(condType string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "ready",
		LastTransitionTime: metav1.Now(),
	}
}

func TestAggregateReady_AllTrue(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for _, ct := range subConditionTypes {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeTrue(),
		"aggregate Ready must be true when all sub-conditions are True")
}

func TestAggregateReady_MissingCondition(t *testing.T) {
	g := NewGomegaWithT(t)

	// Drop the last sub-condition so one is missing entirely.
	conds := make([]metav1.Condition, 0, len(subConditionTypes)-1)
	for _, ct := range subConditionTypes[:len(subConditionTypes)-1] {
		conds = append(conds, trueCondition(ct))
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when a sub-condition is missing")
}

func TestAggregateReady_OneFalse(t *testing.T) {
	g := NewGomegaWithT(t)

	conds := make([]metav1.Condition, 0, len(subConditionTypes))
	for i, ct := range subConditionTypes {
		c := trueCondition(ct)
		if i == 0 {
			c.Status = metav1.ConditionFalse
		}
		conds = append(conds, c)
	}

	g.Expect(conditions.AllTrue(conds, subConditionTypes...)).To(BeFalse(),
		"aggregate Ready must be false when any sub-condition is False")
}

// TestSetServicesStatus_WritesPhaseAndKeystoneReadiness verifies that
// setServicesStatus populates the previously-unwritten status.updatePhase (fixed
// at Idle) and the status.services entry named "keystone", deriving the service
// readiness from the KeystoneReady sub-condition and the release from the
// releases map (#476).
func TestSetServicesStatus_WritesPhaseAndKeystoneReadiness(t *testing.T) {
	g := NewGomegaWithT(t)

	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services:         c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
		},
	}

	// KeystoneReady absent => service reported not Ready.
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.UpdatePhase).To(Equal(c5c3v1alpha1.UpdatePhaseIdle))
	svc := findServiceStatus(cp.Status.Services, "keystone")
	g.Expect(svc).NotTo(BeNil(), "status.services must report the projected keystone service")
	g.Expect(svc.Name).To(Equal("keystone"))
	g.Expect(svc.Ready).To(BeFalse(), "keystone service must be not Ready while KeystoneReady is absent")
	g.Expect(svc.Release).To(Equal("2026.1"))

	// KeystoneReady True => service reported Ready.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeKeystoneReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	svc = findServiceStatus(cp.Status.Services, "keystone")
	g.Expect(svc).NotTo(BeNil(), "status.services must still report the projected keystone service")
	g.Expect(svc.Ready).To(BeTrue(),
		"keystone service must be Ready once KeystoneReady is True")
}

// TestSetServicesStatus_GlanceEntry extends the status projection to the third
// service: services.glance produces a "glance" entry whose readiness tracks the
// GlanceReady sub-condition, and an unmanaged Glance is omitted.
func TestSetServicesStatus_GlanceEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:  &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:   &c5c3v1alpha1.ServiceGlanceSpec{},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(3))
	// Stable order: keystone, horizon, glance.
	g.Expect(cp.Status.Services[2].Name).To(Equal("glance"))
	g.Expect(cp.Status.Services[2].Ready).To(BeFalse(),
		"glance is not Ready while GlanceReady is absent")
	g.Expect(cp.Status.Services[2].Release).To(Equal("2026.1"))

	// GlanceReady True flips only the glance entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeGlanceReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	glance := findServiceStatus(cp.Status.Services, "glance")
	g.Expect(glance).NotTo(BeNil())
	g.Expect(glance.Ready).To(BeTrue())

	// An unmanaged glance is omitted rather than reported.
	cp.Spec.Services.Glance = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(2))
	g.Expect(findServiceStatus(cp.Status.Services, "glance")).To(BeNil())
}

// TestSetServicesStatus_PlacementEntry extends the status projection to the
// fourth service: services.placement produces a "placement" entry whose readiness
// tracks the PlacementReady sub-condition, and an unmanaged Placement is omitted.
func TestSetServicesStatus_PlacementEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(4))
	// Stable order: keystone, horizon, glance, placement.
	g.Expect(cp.Status.Services[3].Name).To(Equal("placement"))
	g.Expect(cp.Status.Services[3].Ready).To(BeFalse(),
		"placement is not Ready while PlacementReady is absent")
	g.Expect(cp.Status.Services[3].Release).To(Equal("2026.1"))

	// PlacementReady True flips only the placement entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypePlacementReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	placement := findServiceStatus(cp.Status.Services, "placement")
	g.Expect(placement).NotTo(BeNil())
	g.Expect(placement.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "glance").Ready).To(BeFalse(),
		"PlacementReady must not flip a peer service entry")

	// An unmanaged placement is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Placement = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(3))
	g.Expect(findServiceStatus(cp.Status.Services, "placement")).To(BeNil())
	g.Expect(cp.Status.Services[2].Name).To(Equal("glance"))
}

// TestSetServicesStatus_BarbicanEntry extends the status projection to the fifth
// service: services.barbican produces a "barbican" entry whose readiness tracks the
// BarbicanReady sub-condition, and an unmanaged Barbican is omitted. The entry is
// appended LAST, so the four established positions keep their meaning.
func TestSetServicesStatus_BarbicanEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(5))
	// Stable order: keystone, horizon, glance, placement, barbican.
	g.Expect(cp.Status.Services[4].Name).To(Equal("barbican"))
	g.Expect(cp.Status.Services[4].Ready).To(BeFalse(),
		"barbican is not Ready while BarbicanReady is absent")
	g.Expect(cp.Status.Services[4].Release).To(Equal("2026.1"))

	// BarbicanReady True flips only the barbican entry.
	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeBarbicanReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	barbican := findServiceStatus(cp.Status.Services, "barbican")
	g.Expect(barbican).NotTo(BeNil())
	g.Expect(barbican.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "placement").Ready).To(BeFalse(),
		"BarbicanReady must not flip a peer service entry")

	// An unmanaged barbican is omitted rather than reported, and the remaining
	// entries keep their positions. The webhook's shared/dedicated transition
	// freeze reads status.services[].name, so a dropped service disappearing from
	// the list is what eventually reopens its CREATE path (serviceDeclaredBefore).
	cp.Spec.Services.Barbican = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(4))
	g.Expect(findServiceStatus(cp.Status.Services, "barbican")).To(BeNil())
	g.Expect(cp.Status.Services[3].Name).To(Equal("placement"))
}

// TestSetServicesStatus_NeutronEntry extends the status projection to the sixth
// service: services.neutron produces a "neutron" entry whose readiness tracks the
// NeutronReady sub-condition, and an unmanaged Neutron is omitted. The entry is
// appended LAST, so the five established positions keep their meaning.
func TestSetServicesStatus_NeutronEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(6))
	// Stable order: keystone, horizon, glance, placement, barbican, neutron.
	g.Expect(cp.Status.Services[5].Name).To(Equal("neutron"))
	g.Expect(cp.Status.Services[5].Ready).To(BeFalse(),
		"neutron is not Ready while NeutronReady is absent")
	g.Expect(cp.Status.Services[5].Release).To(Equal("2026.1"))

	// A False NeutronReady keeps the entry not-ready; only True flips it, and only
	// the neutron entry.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditionTypeNeutronReady,
		Status:  metav1.ConditionFalse,
		Reason:  "WaitingForOVN",
		Message: "OVNReady is not True; Neutron projection deferred",
	})
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(findServiceStatus(cp.Status.Services, "neutron").Ready).To(BeFalse())

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeNeutronReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	neutron := findServiceStatus(cp.Status.Services, "neutron")
	g.Expect(neutron).NotTo(BeNil())
	g.Expect(neutron.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "barbican").Ready).To(BeFalse(),
		"NeutronReady must not flip a peer service entry")

	// An unmanaged neutron is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Neutron = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(5))
	g.Expect(findServiceStatus(cp.Status.Services, "neutron")).To(BeNil())
	g.Expect(cp.Status.Services[4].Name).To(Equal("barbican"))
}

// TestSetServicesStatus_CinderEntry extends the status projection to the seventh
// service: services.cinder produces a "cinder" entry whose readiness tracks the
// CinderReady sub-condition, and an unmanaged Cinder is omitted. The entry is
// appended LAST, so the six established positions keep their meaning.
func TestSetServicesStatus_CinderEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS: &c5c3v1alpha1.NFSShareSpec{
							Server: "nfs-server.openstack.svc.cluster.local",
							Path:   "/volumes",
						},
					}},
				},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(7))
	// Stable order: keystone, horizon, glance, placement, barbican, neutron, cinder.
	g.Expect(cp.Status.Services[6].Name).To(Equal("cinder"))
	g.Expect(cp.Status.Services[6].Ready).To(BeFalse(),
		"cinder is not Ready while CinderReady is absent")
	g.Expect(cp.Status.Services[6].Release).To(Equal("2026.1"))

	// A False CinderReady keeps the entry not-ready; only True flips it, and only
	// the cinder entry.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditionTypeCinderReady,
		Status:  metav1.ConditionFalse,
		Reason:  "WaitingForKeystone",
		Message: "KeystoneReady is not True; Cinder projection deferred",
	})
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(findServiceStatus(cp.Status.Services, "cinder").Ready).To(BeFalse())

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeCinderReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	cinder := findServiceStatus(cp.Status.Services, "cinder")
	g.Expect(cinder).NotTo(BeNil())
	g.Expect(cinder.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "neutron").Ready).To(BeFalse(),
		"CinderReady must not flip a peer service entry")

	// An unmanaged cinder is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Cinder = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(6))
	g.Expect(findServiceStatus(cp.Status.Services, "cinder")).To(BeNil())
	g.Expect(cp.Status.Services[5].Name).To(Equal("neutron"))
}

// TestSetServicesStatus_NovaEntry extends the status projection to the eighth
// service: services.nova produces a "nova" entry whose readiness tracks the
// NovaReady sub-condition, and an unmanaged Nova is omitted. The entry is
// appended LAST, so the seven established positions keep their meaning.
func TestSetServicesStatus_NovaEntry(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron: &c5c3v1alpha1.ServiceNeutronSpec{
					OVN: c5c3v1alpha1.NeutronOVNSpec{
						CentralRef: c5c3v1alpha1.NeutronOVNCentralRef{Name: "ovn"},
					},
				},
				Cinder: &c5c3v1alpha1.ServiceCinderSpec{
					Backends: []c5c3v1alpha1.CinderBackendEntry{{
						Name: "nfs1",
						Type: "NFS",
						NFS: &c5c3v1alpha1.NFSShareSpec{
							Server: "nfs-server.openstack.svc.cluster.local",
							Path:   "/volumes",
						},
					}},
				},
				Nova: &c5c3v1alpha1.ServiceNovaSpec{},
			},
		},
	}

	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(8))
	// Stable order: keystone, horizon, glance, placement, barbican, neutron,
	// cinder, nova.
	g.Expect(cp.Status.Services[7].Name).To(Equal("nova"))
	g.Expect(cp.Status.Services[7].Ready).To(BeFalse(),
		"nova is not Ready while NovaReady is absent")
	g.Expect(cp.Status.Services[7].Release).To(Equal("2026.1"))

	// A False NovaReady keeps the entry not-ready; only True flips it, and only
	// the nova entry.
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditionTypeNovaReady,
		Status:  metav1.ConditionFalse,
		Reason:  "WaitingForPlacement",
		Message: "PlacementReady is not True; Nova projection deferred",
	})
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(findServiceStatus(cp.Status.Services, "nova").Ready).To(BeFalse())

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeNovaReady))
	setServicesStatus(cp, releasesAt("2026.1"))
	nova := findServiceStatus(cp.Status.Services, "nova")
	g.Expect(nova).NotTo(BeNil())
	g.Expect(nova.Ready).To(BeTrue())
	g.Expect(findServiceStatus(cp.Status.Services, "cinder").Ready).To(BeFalse(),
		"NovaReady must not flip a peer service entry")

	// An unmanaged nova is omitted rather than reported, and the remaining
	// entries keep their positions.
	cp.Spec.Services.Nova = nil
	setServicesStatus(cp, releasesAt("2026.1"))
	g.Expect(cp.Status.Services).To(HaveLen(7))
	g.Expect(findServiceStatus(cp.Status.Services, "nova")).To(BeNil())
	g.Expect(cp.Status.Services[6].Name).To(Equal("cinder"))
}

// TestAggregateReady_NovaBlocksIt pins the condition type the compute service
// contributes to the aggregate vocabulary: with every sub-condition True the
// plane reports Ready, and NovaReady turning False takes it back to
// False/NotAllReady.
func TestAggregateReady_NovaBlocksIt(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
	}
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&cp.Status.Conditions, trueCondition(ct))
	}

	blocking := trueCondition(conditionTypeNovaReady)
	blocking.Status = metav1.ConditionFalse
	blocking.Reason = "WaitingForPlacement"
	conditions.SetCondition(&cp.Status.Conditions, blocking)
	setReadyCondition(cp)

	ready := conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal("NotAllReady"))

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeNovaReady))
	setReadyCondition(cp)
	ready = conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

// TestAggregateReady_CinderBlocksIt pins the condition type the block-storage
// service contributes to the aggregate vocabulary: with every sub-condition True
// the plane reports Ready, and CinderReady turning False takes it back to
// False/NotAllReady.
func TestAggregateReady_CinderBlocksIt(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
	}
	for _, ct := range subConditionTypes {
		conditions.SetCondition(&cp.Status.Conditions, trueCondition(ct))
	}

	blocking := trueCondition(conditionTypeCinderReady)
	blocking.Status = metav1.ConditionFalse
	blocking.Reason = "WaitingForKeystone"
	conditions.SetCondition(&cp.Status.Conditions, blocking)
	setReadyCondition(cp)

	ready := conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Reason).To(Equal("NotAllReady"))

	conditions.SetCondition(&cp.Status.Conditions, trueCondition(conditionTypeCinderReady))
	setReadyCondition(cp)
	ready = conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

// TestAggregateReady_OVNAndNeutronBlockIt pins the two condition types the network
// service contributes to the aggregate vocabulary: with every sub-condition True
// the plane reports Ready, and either of the two turning False takes it back to
// False/NotAllReady.
func TestAggregateReady_OVNAndNeutronBlockIt(t *testing.T) {
	for _, blocked := range []string{conditionTypeOVNReady, conditionTypeNeutronReady} {
		t.Run(blocked, func(t *testing.T) {
			g := NewGomegaWithT(t)
			cp := &c5c3v1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
			}
			for _, ct := range subConditionTypes {
				conditions.SetCondition(&cp.Status.Conditions, trueCondition(ct))
			}
			setReadyCondition(cp)
			ready := conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			blocking := trueCondition(blocked)
			blocking.Status = metav1.ConditionFalse
			blocking.Reason = "NotReady"
			conditions.SetCondition(&cp.Status.Conditions, blocking)
			setReadyCondition(cp)

			ready = conditions.GetCondition(cp.Status.Conditions, conditionTypeReady)
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(Equal("NotAllReady"))
		})
	}
}

// releasesAt reports every service at release, as observedServiceReleases does
// for a plane whose children all installed it.
func releasesAt(release string) map[string]string {
	return map[string]string{
		keystoneServiceKey:  release,
		horizonServiceKey:   release,
		glanceServiceKey:    release,
		placementServiceKey: release,
		barbicanServiceKey:  release,
		neutronServiceKey:   release,
		cinderServiceKey:    release,
		novaServiceKey:      release,
	}
}

// distinctReleases gives every service its own release, so a lookup under the
// wrong key or a read of spec.openStackRelease fails the assertion.
func distinctReleases() map[string]string {
	return map[string]string{
		keystoneServiceKey:  "r-keystone",
		horizonServiceKey:   "r-horizon",
		glanceServiceKey:    "r-glance",
		placementServiceKey: "r-placement",
		barbicanServiceKey:  "r-barbican",
		neutronServiceKey:   "r-neutron",
		cinderServiceKey:    "r-cinder",
		novaServiceKey:      "r-nova",
	}
}

// findServiceStatus returns a pointer to the ServiceStatus entry with the given
// name, or nil when the listType=map status.services list has no such entry.
func findServiceStatus(services []c5c3v1alpha1.ServiceStatus, name string) *c5c3v1alpha1.ServiceStatus {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

// TestUpdateStatus_SkipsWriteWhenUnchanged locks in the no-op status-write
// skip the ControlPlane controller gained by adopting the shared status
// writer: a converged steady-state pass must not issue Status().Update (no
// write → no watch event → no resourceVersion churn), while a changed status
// still writes. The reconciler's Status().Update is wired to always fail, so
// a skipped write is observable as a nil error return.
func TestUpdateStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default", Generation: 3},
	}
	statusErr := fmt.Errorf("status update must not be called on an unchanged status")
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusErr
			},
		}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	// Bring cp.Status into the exact state updateStatus would compute (Ready
	// aggregated + services projected + ObservedGeneration stamped), then
	// snapshot it — a converged steady-state pass.
	setReadyCondition(cp)
	setServicesStatus(cp, nil)
	cp.Status.ObservedGeneration = cp.Generation
	snapshot := cp.Status.DeepCopy()

	_, err := r.updateStatus(context.Background(), cp, snapshot, ctrl.Result{}, nil)
	g.Expect(err).NotTo(HaveOccurred(),
		"an unchanged status must skip the write; the failing Status().Update proves it was not called")

	// An empty snapshot differs from the computed status, so the (failing)
	// write must be attempted.
	_, err = r.updateStatus(context.Background(), cp, &c5c3v1alpha1.ControlPlaneStatus{}, ctrl.Result{}, nil)
	g.Expect(err).To(HaveOccurred(), "a changed status must attempt the write")
	g.Expect(err.Error()).To(ContainSubstring("updating status:"))
}

// serviceReleasesTestScheme extends controllerTestScheme with the eight service
// child kinds observedServiceReleases reads.
func serviceReleasesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := controllerTestScheme(t)
	adders := []struct {
		name string
		add  func(*runtime.Scheme) error
	}{
		{"keystone", keystonev1alpha1.AddToScheme},
		{"horizon", horizonv1alpha1.AddToScheme},
		{"glance", glancev1alpha1.AddToScheme},
		{"placement", placementv1alpha1.AddToScheme},
		{"barbican", barbicanv1alpha1.AddToScheme},
		{"neutron", neutronv1alpha1.AddToScheme},
		{"cinder", cinderv1alpha1.AddToScheme},
		{"nova", novav1alpha1.AddToScheme},
	}
	for _, a := range adders {
		if err := a.add(s); err != nil {
			t.Fatalf("adding %s scheme: %v", a.name, err)
		}
	}
	return s
}

// serviceReleasesReconciler returns a reconciler whose fake client holds objs
// and routes its calls through funcs.
func serviceReleasesReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *ControlPlaneReconciler {
	t.Helper()
	s := serviceReleasesTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return &ControlPlaneReconciler{Client: c, Scheme: s}
}

// allServicesControlPlane returns a ControlPlane at release that declares all
// eight services in its own namespace.
func allServicesControlPlane(release string) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: release,
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone:  &c5c3v1alpha1.ServiceKeystoneSpec{},
				Horizon:   &c5c3v1alpha1.ServiceHorizonSpec{},
				Glance:    &c5c3v1alpha1.ServiceGlanceSpec{},
				Placement: &c5c3v1alpha1.ServicePlacementSpec{},
				Barbican:  &c5c3v1alpha1.ServiceBarbicanSpec{},
				Neutron:   &c5c3v1alpha1.ServiceNeutronSpec{},
				Cinder:    &c5c3v1alpha1.ServiceCinderSpec{},
				Nova:      &c5c3v1alpha1.ServiceNovaSpec{},
			},
		},
	}
}

// databaseChildren returns the seven database children cp projects, each
// reporting the installedRelease that installed names under its service key.
func databaseChildren(cp *c5c3v1alpha1.ControlPlane, installed map[string]string) []client.Object {
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: cp.Namespace} }
	ks := &keystonev1alpha1.Keystone{ObjectMeta: om(keystoneName(cp))}
	ks.Status.InstalledRelease = installed[keystoneServiceKey]
	gl := &glancev1alpha1.Glance{ObjectMeta: om(glanceName(cp))}
	gl.Status.InstalledRelease = installed[glanceServiceKey]
	pl := &placementv1alpha1.Placement{ObjectMeta: om(placementName(cp))}
	pl.Status.InstalledRelease = installed[placementServiceKey]
	bn := &barbicanv1alpha1.Barbican{ObjectMeta: om(barbicanName(cp))}
	bn.Status.InstalledRelease = installed[barbicanServiceKey]
	nn := &neutronv1alpha1.Neutron{ObjectMeta: om(neutronName(cp))}
	nn.Status.InstalledRelease = installed[neutronServiceKey]
	cn := &cinderv1alpha1.Cinder{ObjectMeta: om(cinderName(cp))}
	cn.Status.InstalledRelease = installed[cinderServiceKey]
	nv := &novav1alpha1.Nova{ObjectMeta: om(novaName(cp))}
	nv.Status.InstalledRelease = installed[novaServiceKey]
	return []client.Object{ks, gl, pl, bn, nn, cn, nv}
}

// TestObservedServiceReleases_ReportsEachChildsInstalledRelease pins the source
// of every database service's release: the child's status.installedRelease, not
// spec.openStackRelease. Every child reports its own release, none of them the
// spec's 2026.2, so a child read into another service's key fails as well.
func TestObservedServiceReleases_ReportsEachChildsInstalledRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := allServicesControlPlane("2026.2")
	installed := distinctReleases()
	r := serviceReleasesReconciler(t, interceptor.Funcs{}, databaseChildren(cp, installed)...)

	releases := r.observedServiceReleases(context.Background(), cp)

	for _, key := range []string{keystoneServiceKey, glanceServiceKey, placementServiceKey, barbicanServiceKey, neutronServiceKey, cinderServiceKey, novaServiceKey} {
		g.Expect(releases).To(HaveKeyWithValue(key, installed[key]), "service %s", key)
	}
	g.Expect(releases).To(HaveKeyWithValue(horizonServiceKey, ""),
		"a declared Horizon without a child reports no release")
}

// TestObservedServiceReleases_MissingChildReportsEmpty pins that a declared
// service whose child does not exist (not created yet, or deleted) reports no
// release, even when its entry carried one before.
func TestObservedServiceReleases_MissingChildReportsEmpty(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services:         c5c3v1alpha1.ServicesSpec{Placement: &c5c3v1alpha1.ServicePlacementSpec{}},
		},
		Status: c5c3v1alpha1.ControlPlaneStatus{
			Services: []c5c3v1alpha1.ServiceStatus{{Name: placementServiceKey, Release: "2026.1"}},
		},
	}
	r := serviceReleasesReconciler(t, interceptor.Funcs{})

	releases := r.observedServiceReleases(context.Background(), cp)

	g.Expect(releases).To(Equal(map[string]string{placementServiceKey: ""}))
}

// TestObservedServiceReleases_ReadErrorKeepsPreviousRelease pins the failure
// policy: a child read that fails with anything but NotFound keeps the release the
// entry reported before, and the other services are still read.
func TestObservedServiceReleases_ReadErrorKeepsPreviousRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := allServicesControlPlane("2026.2")
	cp.Status.Services = []c5c3v1alpha1.ServiceStatus{{Name: novaServiceKey, Release: "2026.1"}}
	r := serviceReleasesReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*novav1alpha1.Nova); ok {
				return apierrors.NewServiceUnavailable("cache not synced")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, databaseChildren(cp, releasesAt("2026.2"))...)

	releases := r.observedServiceReleases(context.Background(), cp)

	g.Expect(releases).To(HaveKeyWithValue(novaServiceKey, "2026.1"),
		"a failed read keeps the release the entry reported before")
	for _, key := range []string{keystoneServiceKey, glanceServiceKey, placementServiceKey, barbicanServiceKey, neutronServiceKey, cinderServiceKey} {
		g.Expect(releases).To(HaveKeyWithValue(key, "2026.2"), "service %s is still read", key)
	}
}

// TestObservedServiceReleases_HorizonReportsTagOfConvergedGeneration pins
// Horizon's rule: it has no installed release, so its entry reports the image
// tag of the generation the child last reported Ready for.
func TestObservedServiceReleases_HorizonReportsTagOfConvergedGeneration(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name               string
		image              commonv1.ImageSpec
		generation         int64
		observedGeneration int64
		ready              bool
		previous           string
		want               string
	}{
		{
			name:               "Ready at its current generation reports the tag",
			image:              commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2026.2"},
			generation:         3,
			observedGeneration: 3,
			ready:              true,
			previous:           "2026.1",
			want:               "2026.2",
		},
		{
			name:               "Ready at an older generation keeps the previous release",
			image:              commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2026.2"},
			generation:         4,
			observedGeneration: 3,
			ready:              true,
			previous:           "2026.1",
			want:               "2026.1",
		},
		{
			name:               "not Ready at its current generation keeps the previous release",
			image:              commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2026.2"},
			generation:         4,
			observedGeneration: 4,
			ready:              false,
			previous:           "2026.1",
			want:               "2026.1",
		},
		{
			name:               "a first pass before convergence reports no release",
			image:              commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Tag: "2026.2"},
			generation:         1,
			observedGeneration: 0,
			ready:              false,
			previous:           "",
			want:               "",
		},
		{
			name:               "a digest-pinned image reports no release",
			image:              commonv1.ImageSpec{Repository: "ghcr.io/c5c3/horizon", Digest: digest},
			generation:         3,
			observedGeneration: 3,
			ready:              true,
			previous:           "2026.1",
			want:               "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGomegaWithT(t)
			cp := &c5c3v1alpha1.ControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
				Spec: c5c3v1alpha1.ControlPlaneSpec{
					OpenStackRelease: "2026.2",
					Services:         c5c3v1alpha1.ServicesSpec{Horizon: &c5c3v1alpha1.ServiceHorizonSpec{}},
				},
			}
			// A first pass has no horizon entry yet.
			if tc.previous != "" {
				cp.Status.Services = []c5c3v1alpha1.ServiceStatus{{Name: horizonServiceKey, Release: tc.previous}}
			}
			hz := &horizonv1alpha1.Horizon{
				ObjectMeta: metav1.ObjectMeta{Name: horizonName(cp), Namespace: cp.Namespace, Generation: tc.generation},
				Spec:       horizonv1alpha1.HorizonSpec{Image: tc.image},
			}
			hz.Status.ObservedGeneration = tc.observedGeneration
			ready := trueCondition("Ready")
			if !tc.ready {
				ready.Status = metav1.ConditionFalse
			}
			hz.Status.Conditions = []metav1.Condition{ready}
			r := serviceReleasesReconciler(t, interceptor.Funcs{}, hz)

			releases := r.observedServiceReleases(context.Background(), cp)

			g.Expect(releases).To(HaveKeyWithValue(horizonServiceKey, tc.want))
		})
	}
}

// TestObservedServiceReleases_ExternalKeystoneReadsNoChild pins that a Keystone
// in External mode, which has no child, reports no release and is never read,
// even when a Keystone of the projected name exists.
func TestObservedServiceReleases_ExternalKeystoneReadsNoChild(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.1",
			Services: c5c3v1alpha1.ServicesSpec{
				Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{Mode: c5c3v1alpha1.KeystoneModeExternal},
			},
		},
	}
	stray := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{Name: keystoneName(cp), Namespace: cp.Namespace}}
	stray.Status.InstalledRelease = "2026.1"
	keystoneReads := 0
	r := serviceReleasesReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*keystonev1alpha1.Keystone); ok {
				keystoneReads++
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, stray)

	releases := r.observedServiceReleases(context.Background(), cp)

	g.Expect(keystoneReads).To(BeZero(), "an External Keystone has no child to read")
	g.Expect(releases[keystoneServiceKey]).To(BeEmpty())
}

// TestObservedServiceReleases_NoServiceDeclared pins that a ControlPlane without
// services reads nothing, and that status.services stays nil for it.
func TestObservedServiceReleases_NoServiceDeclared(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec:       c5c3v1alpha1.ControlPlaneSpec{OpenStackRelease: "2026.1"},
	}
	reads := 0
	r := serviceReleasesReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads++
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	releases := r.observedServiceReleases(context.Background(), cp)

	g.Expect(reads).To(BeZero())
	g.Expect(releases).To(BeEmpty())
	setServicesStatus(cp, releases)
	g.Expect(cp.Status.Services).To(BeNil())
}

// TestSetServicesStatus_ReleaseFollowsTheChildNotTheSpec pins that an entry's
// release comes from the releases map under its own service key, so it lags a
// spec that already names the next release.
func TestSetServicesStatus_ReleaseFollowsTheChildNotTheSpec(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := allServicesControlPlane("2026.2")
	releases := distinctReleases()

	setServicesStatus(cp, releases)

	g.Expect(cp.Status.Services).To(HaveLen(8))
	for _, s := range cp.Status.Services {
		g.Expect(s.Release).To(Equal(releases[s.Name]), "service %s", s.Name)
	}
}

// TestObservedServiceReleases_CoversEveryReportedService keeps the two
// per-service ladders in step: every entry setServicesStatus writes has a
// release observedServiceReleases looked up for it. Every ServicesSpec field is
// declared, so a service added to ServicesSpec and setServicesStatus but not to
// observedServiceReleases fails here.
func TestObservedServiceReleases_CoversEveryReportedService(t *testing.T) {
	g := NewGomegaWithT(t)
	cp := &c5c3v1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"}}
	services := reflect.ValueOf(&cp.Spec.Services).Elem()
	for i := range services.NumField() {
		f := services.Field(i)
		f.Set(reflect.New(f.Type().Elem()))
	}
	r := serviceReleasesReconciler(t, interceptor.Funcs{})

	releases := r.observedServiceReleases(context.Background(), cp)
	setServicesStatus(cp, releases)

	g.Expect(cp.Status.Services).To(HaveLen(services.NumField()))
	for _, s := range cp.Status.Services {
		g.Expect(releases).To(HaveKey(s.Name), "service %s has no release lookup", s.Name)
	}
}

// TestUpdateStatus_WritesTheChildsInstalledRelease pins the write path end to
// end: updateStatus reads the Keystone child and persists the release it reports
// installed, not the spec's.
func TestUpdateStatus_WritesTheChildsInstalledRelease(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	s := serviceReleasesTestScheme(t)
	stored := &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "openstack"},
		Spec: c5c3v1alpha1.ControlPlaneSpec{
			OpenStackRelease: "2026.2",
			Services:         c5c3v1alpha1.ServicesSpec{Keystone: &c5c3v1alpha1.ServiceKeystoneSpec{}},
		},
	}
	ks := &keystonev1alpha1.Keystone{ObjectMeta: metav1.ObjectMeta{Name: keystoneName(stored), Namespace: stored.Namespace}}
	ks.Status.InstalledRelease = "2026.1"
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(stored, ks).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		Build()
	r := &ControlPlaneReconciler{Client: c, Scheme: s}
	key := client.ObjectKeyFromObject(stored)
	cp := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, key, cp)).To(Succeed())

	_, err := r.updateStatus(ctx, cp, &c5c3v1alpha1.ControlPlaneStatus{}, ctrl.Result{}, nil)
	g.Expect(err).NotTo(HaveOccurred())

	written := &c5c3v1alpha1.ControlPlane{}
	g.Expect(c.Get(ctx, key, written)).To(Succeed())
	g.Expect(written.Status.Services).To(HaveLen(1))
	g.Expect(written.Status.Services[0].Release).To(Equal("2026.1"),
		"the written entry carries the child's installedRelease, not spec.openStackRelease 2026.2")
}

func TestReconcile_NotFound_EarlyReturn(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "absent"},
	})

	g.Expect(err).NotTo(HaveOccurred(),
		"Reconcile on a missing ControlPlane must not error")
	g.Expect(res).To(Equal(ctrl.Result{}),
		"Reconcile on a missing ControlPlane must return a zero result")
}

// --- Duplicate-ControlPlane guard tests (defense-in-depth) ---

// duplicateGuardControlPlane returns a minimal ControlPlane with the given
// identity and creation time for the duplicate-guard tests. The guard runs
// before any spec interpretation, so an empty spec is sufficient.
func duplicateGuardControlPlane(name, namespace, uid string, created time.Time) *c5c3v1alpha1.ControlPlane {
	return &c5c3v1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
		},
	}
}

func TestReconcile_DuplicateControlPlane_ParksYounger(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	now := time.Now()
	older := duplicateGuardControlPlane("alpha", "default", "uid-alpha", now.Add(-time.Hour))
	younger := duplicateGuardControlPlane("bravo", "default", "uid-bravo", now)
	// The scheme deliberately registers no child types (MariaDB, Keystone, ...):
	// if the guard ever failed to park the duplicate, the sub-reconciler chain
	// would error on the unregistered kinds and fail this test.
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(older, younger).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "bravo"},
	})

	g.Expect(err).NotTo(HaveOccurred(), "parking a duplicate must not error")
	g.Expect(res.RequeueAfter).To(Equal(duplicateControlPlaneRequeueAfter),
		"a parked duplicate must requeue so it can take over once the incumbent is gone")

	var parked c5c3v1alpha1.ControlPlane
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "bravo"}, &parked)).To(Succeed())
	ready := conditions.GetCondition(parked.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil(), "the parked duplicate must carry a Ready condition")
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse),
		"a parked duplicate must report Ready=False")
	g.Expect(ready.Reason).To(Equal("DuplicateControlPlane"),
		"a parked duplicate must report reason DuplicateControlPlane")
	g.Expect(ready.Message).To(ContainSubstring(`"alpha"`),
		"the Ready message must name the incumbent")
	g.Expect(parked.Status.ObservedGeneration).To(Equal(parked.Generation),
		"the parked status must stamp ObservedGeneration")
}

func TestReconcile_DuplicateControlPlane_TieBreakByName(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	created := metav1.Now().Time
	// Identical creation timestamps: the lexically smallest name wins.
	first := duplicateGuardControlPlane("alpha", "default", "uid-alpha", created)
	second := duplicateGuardControlPlane("bravo", "default", "uid-bravo", created)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(first, second).
		WithStatusSubresource(&c5c3v1alpha1.ControlPlane{}).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "bravo"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var parked c5c3v1alpha1.ControlPlane
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "bravo"}, &parked)).To(Succeed())
	ready := conditions.GetCondition(parked.Status.Conditions, conditionTypeReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("DuplicateControlPlane"),
		"on a creation-time tie the lexically larger name must be parked")
	g.Expect(ready.Message).To(ContainSubstring(`"alpha"`),
		"the tie-break incumbent must be the lexically smallest name")
}

func TestDuplicateControlPlaneIncumbent_OldestProceeds(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	now := time.Now()
	older := duplicateGuardControlPlane("alpha", "default", "uid-alpha", now.Add(-time.Hour))
	younger := duplicateGuardControlPlane("bravo", "default", "uid-bravo", now)
	// A ControlPlane in another namespace must not influence the guard.
	unrelated := duplicateGuardControlPlane("older-elsewhere", "other", "uid-other", now.Add(-2*time.Hour))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(older, younger, unrelated).
		Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	incumbent, err := r.duplicateControlPlaneIncumbent(context.Background(), older)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(BeEmpty(),
		"the oldest ControlPlane in the namespace must proceed")

	incumbent, err = r.duplicateControlPlaneIncumbent(context.Background(), younger)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(Equal("alpha"),
		"a younger ControlPlane must be parked behind the namespace's oldest")
}

func TestDuplicateControlPlaneIncumbent_SingleControlPlane(t *testing.T) {
	g := NewGomegaWithT(t)

	s := controllerTestScheme(t)
	only := duplicateGuardControlPlane("solo", "default", "uid-solo", time.Now())
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(only).Build()

	r := &ControlPlaneReconciler{Client: c, Scheme: s}

	incumbent, err := r.duplicateControlPlaneIncumbent(context.Background(), only)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(incumbent).To(BeEmpty(),
		"a namespace's only ControlPlane must proceed")
}
