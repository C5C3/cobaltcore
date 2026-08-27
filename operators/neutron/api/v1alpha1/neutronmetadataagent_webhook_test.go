// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validNeutronMetadataAgent returns a minimal NeutronMetadataAgent CR that
// passes every validation rule: the image it runs and the chassis it attaches
// to. Tests mutate single fields to exercise individual rules.
func validNeutronMetadataAgent() *NeutronMetadataAgent {
	return &NeutronMetadataAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "openstack"},
		Spec: NeutronMetadataAgentSpec{
			OpenStackRelease: "2025.2",
			Image: commonv1.ImageSpec{
				Repository: "ghcr.io/c5c3/neutron",
				Tag:        "2025.2",
			},
			ChassisRef: OVNChassisRef{Name: "chassis"},
		},
	}
}

// --- Defaulting webhook ---

// The two Nova metadata leaves are filled only when the block that carries them
// is present: a nil spec.novaMetadata stays nil, because the agent then renders
// neither key and the oslo defaults apply.
func TestNeutronMetadataAgentDefault_NovaMetadata(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	absent := validNeutronMetadataAgent()
	g.Expect(w.Default(context.Background(), absent)).To(gomega.Succeed())
	g.Expect(absent.Spec.NovaMetadata).To(gomega.BeNil())

	present := validNeutronMetadataAgent()
	present.Spec.NovaMetadata = &NovaMetadataSpec{
		Host:            "nova-metadata.openstack.svc.cluster.local",
		SharedSecretRef: &commonv1.SecretRefSpec{Name: "metadata-proxy-secret"},
	}
	g.Expect(w.Default(context.Background(), present)).To(gomega.Succeed())
	g.Expect(present.Spec.NovaMetadata.Port).To(gomega.Equal(DefaultNovaMetadataPort))
	g.Expect(present.Spec.NovaMetadata.SharedSecretRef.Key).To(gomega.Equal("shared_secret"))

	explicit := validNeutronMetadataAgent()
	explicit.Spec.NovaMetadata = &NovaMetadataSpec{
		Port:            18775,
		SharedSecretRef: &commonv1.SecretRefSpec{Name: "metadata-proxy-secret", Key: "proxy-secret"},
	}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.NovaMetadata.Port).To(gomega.Equal(int32(18775)))
	g.Expect(explicit.Spec.NovaMetadata.SharedSecretRef.Key).To(gomega.Equal("proxy-secret"))
}

// spec.logging is materialized so downstream reconciler code dereferences it
// unconditionally.
func TestNeutronMetadataAgentDefault_MaterializesLogging(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	obj := validNeutronMetadataAgent()
	g.Expect(w.Default(context.Background(), obj)).To(gomega.Succeed())
	g.Expect(obj.Spec.Logging).NotTo(gomega.BeNil())
	g.Expect(obj.Spec.Logging.Format).To(gomega.Equal("text"))
	g.Expect(obj.Spec.Logging.Level).To(gomega.Equal("INFO"))
	g.Expect(obj.Spec.Logging.Debug).To(gomega.HaveValue(gomega.BeFalse()))

	explicit := validNeutronMetadataAgent()
	explicit.Spec.Logging = &LoggingSpec{Format: "json", Level: "DEBUG"}
	g.Expect(w.Default(context.Background(), explicit)).To(gomega.Succeed())
	g.Expect(explicit.Spec.Logging.Format).To(gomega.Equal("json"))
	g.Expect(explicit.Spec.Logging.Level).To(gomega.Equal("DEBUG"))
}

// --- Validating webhook ---

func TestNeutronMetadataAgentValidateCreate_AcceptedShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NeutronMetadataAgent)
	}{
		{
			name:   "minimal",
			mutate: func(*NeutronMetadataAgent) {},
		},
		{
			name: "every optional block set",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Messaging = &commonv1.MessagingSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
				}
				o.Spec.NovaMetadata = &NovaMetadataSpec{
					Host:            "nova-metadata.openstack.svc.cluster.local",
					Port:            8775,
					SharedSecretRef: &commonv1.SecretRefSpec{Name: "metadata-proxy-secret", Key: "shared_secret"},
				}
				o.Spec.Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				}
				o.Spec.Logging = &LoggingSpec{Format: "json", Level: "WARNING"}
				o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: "edge-1"}
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"metadata_workers": "2"},
				}
			},
		},
		{
			name: "brownfield messaging",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Messaging = &commonv1.MessagingSpec{
					SecretRef: &commonv1.SecretRefSpec{Name: "neutron-transport-url", Key: "transport_url"},
				}
			},
		},
		{
			name: "digest-pinned image",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Image.Tag = ""
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronMetadataAgentWebhook{}

			obj := validNeutronMetadataAgent()
			tc.mutate(obj)
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).NotTo(gomega.HaveOccurred())
		})
	}
}

func TestNeutronMetadataAgentValidateCreate_RejectionTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(o *NeutronMetadataAgent)
		wantSub string
	}{
		{
			name:    "empty chassisRef name rejected",
			mutate:  func(o *NeutronMetadataAgent) { o.Spec.ChassisRef.Name = "" },
			wantSub: "chassisRef.name must be set",
		},
		{
			name: "chassisRef name with a newline rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.ChassisRef.Name = "chassis\novn_sb_connection = tcp:attacker.example:6642"
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "image tag and digest both set rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Image.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			wantSub: "exactly one of image.tag or image.digest",
		},
		{
			name:    "image without a repository rejected",
			mutate:  func(o *NeutronMetadataAgent) { o.Spec.Image.Repository = "" },
			wantSub: "spec.image.repository",
		},
		{
			name: "both messaging modes set rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Messaging = &commonv1.MessagingSpec{
					ClusterRef: &corev1.LocalObjectReference{Name: "rabbitmq"},
					SecretRef:  &commonv1.SecretRefSpec{Name: "neutron-transport-url"},
				}
			},
			wantSub: "exactly one of clusterRef or secretRef must be set",
		},
		{
			name:    "neither messaging mode set rejected",
			mutate:  func(o *NeutronMetadataAgent) { o.Spec.Messaging = &commonv1.MessagingSpec{} },
			wantSub: "exactly one of clusterRef or secretRef must be set",
		},
		{
			name:    "zero novaMetadata port rejected",
			mutate:  func(o *NeutronMetadataAgent) { o.Spec.NovaMetadata = &NovaMetadataSpec{Port: 0} },
			wantSub: "port must be between 1 and 65535",
		},
		{
			name:    "out-of-range novaMetadata port rejected",
			mutate:  func(o *NeutronMetadataAgent) { o.Spec.NovaMetadata = &NovaMetadataSpec{Port: 70000} },
			wantSub: "port must be between 1 and 65535",
		},
		{
			name: "sharedSecretRef without a name rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.NovaMetadata = &NovaMetadataSpec{
					Port:            8775,
					SharedSecretRef: &commonv1.SecretRefSpec{Key: "shared_secret"},
				}
			},
			wantSub: "sharedSecretRef.name must be set",
		},
		{
			name: "novaMetadata host with a newline rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.NovaMetadata = &NovaMetadataSpec{
					Port: 8775,
					Host: "nova.example\nmetadata_proxy_shared_secret = hunter2",
				}
			},
			wantSub: "must not contain a newline or carriage return",
		},
		{
			name: "unknown logging level rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.Logging = &LoggingSpec{Level: "TRACE"}
			},
			wantSub: "spec.logging.level",
		},
		{
			name: "empty targetClusterRef name rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: ""}
			},
			wantSub: "target cluster name must be set",
		},
		// The local OVS database is what ties the agent to the ports on its node.
		// Rendering an override would point it at another node's database, before
		// the ExtraConfigHealthy condition could surface it.
		{
			name: "rejected owned key in extraConfig",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"ovs": {"ovsdb_connection": "tcp:attacker.example:6640"},
				}
			},
			wantSub: "ovsdb_connection is managed via",
		},
		{
			name: "unknown extraConfig option rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"ovs": {"not_an_option": "x"},
				}
			},
			wantSub: "no such option in the neutron 2025.2 option catalog",
		},
		{
			name: "extraConfig value with a newline rejected",
			mutate: func(o *NeutronMetadataAgent) {
				o.Spec.ExtraConfig = map[string]map[string]string{
					"DEFAULT": {"metadata_workers": "2\nmetadata_proxy_shared_secret = hunter2"},
				}
			},
			wantSub: "extraConfig key and value must not contain a newline or carriage return",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronMetadataAgentWebhook{}

			obj := validNeutronMetadataAgent()
			tc.mutate(obj)
			_, err := w.ValidateCreate(context.Background(), obj)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring(tc.wantSub))
		})
	}
}

// metadata.name becomes the app.kubernetes.io/instance label value on every
// child, and the owner-name label value on the children of a placed CR.
// Kubernetes caps a label value at 63 characters, and nothing else in the CRD
// bounds the name.
func TestNeutronMetadataAgentValidateCreate_NameLengthBoundedByLabelValue(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	atLimit := validNeutronMetadataAgent()
	atLimit.Name = strings.Repeat("a", MaxNeutronMetadataAgentNameLength)
	_, err := w.ValidateCreate(context.Background(), atLimit)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"a name that still fits a label value must be accepted")

	tooLong := validNeutronMetadataAgent()
	tooLong.Name = strings.Repeat("a", MaxNeutronMetadataAgentNameLength+1)
	_, err = w.ValidateCreate(context.Background(), tooLong)
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.name")))
	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("app.kubernetes.io/instance")))
}

// The bound is create-only, for the reason the sibling kinds document: the name
// is immutable, so on update it could only fire against a grandfathered CR, and
// the validating webhook also sees the finalizer-removal update.
func TestNeutronMetadataAgentValidateUpdate_OverlongNameStaysUpdatable(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	grandfathered := validNeutronMetadataAgent()
	grandfathered.Name = strings.Repeat("a", MaxNeutronMetadataAgentNameLength+1)
	grandfathered.Finalizers = []string{"neutron.openstack.c5c3.io/finalizer"}

	deleting := grandfathered.DeepCopy()
	deleting.Finalizers = nil

	_, err := w.ValidateUpdate(context.Background(), grandfathered, deleting)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"an over-long grandfathered CR must stay updatable, or its deletion never completes")
}

// --- Update-only rules ---

// An agent re-pointed at another chassis lands on another set of nodes, whose
// local OVS databases carry none of the ports it was answering for.
func TestNeutronMetadataAgentValidateUpdate_ChassisRefChangeRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	old := validNeutronMetadataAgent()
	newObj := validNeutronMetadataAgent()
	newObj.Spec.ChassisRef.Name = "other-chassis"

	_, err := w.ValidateUpdate(context.Background(), old, newObj)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("chassisRef is immutable"))
}

func TestNeutronMetadataAgentValidateUpdate_TargetClusterRefTransitions(t *testing.T) {
	withRef := func(name string) *NeutronMetadataAgent {
		o := validNeutronMetadataAgent()
		if name != "" {
			o.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{Name: name}
		}
		return o
	}
	tests := []struct {
		name       string
		old, newer *NeutronMetadataAgent
		wantErr    bool
	}{
		{name: "added rejected", old: withRef(""), newer: withRef("edge-1"), wantErr: true},
		{name: "removed rejected", old: withRef("edge-1"), newer: withRef(""), wantErr: true},
		{name: "renamed rejected", old: withRef("edge-1"), newer: withRef("edge-2"), wantErr: true},
		{name: "unchanged accepted", old: withRef("edge-1"), newer: withRef("edge-1"), wantErr: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			w := &NeutronMetadataAgentWebhook{}

			_, err := w.ValidateUpdate(context.Background(), tc.old, tc.newer)
			if !tc.wantErr {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				return
			}
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(err.Error()).To(gomega.ContainSubstring("targetClusterRef is immutable"))
		})
	}
}

// The catalog check is re-run on update only when one of its inputs changed, so a
// CR whose extraConfig went stale-invalid against a regenerated catalog stays
// editable through every other field.
func TestNeutronMetadataAgentValidateUpdate_ExtraConfigCatalogGate(t *testing.T) {
	g := gomega.NewWithT(t)
	w := &NeutronMetadataAgentWebhook{}

	stale := validNeutronMetadataAgent()
	stale.Spec.ExtraConfig = map[string]map[string]string{
		"ovs": {"not_an_option": "x"},
	}

	relabelled := stale.DeepCopy()
	relabelled.Labels = map[string]string{"team": "network"}
	_, err := w.ValidateUpdate(context.Background(), stale, relabelled)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	edited := stale.DeepCopy()
	edited.Spec.ExtraConfig = map[string]map[string]string{
		"ovs": {"still_not_an_option": "x"},
	}
	_, err = w.ValidateUpdate(context.Background(), stale, edited)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("no such option in the neutron 2025.2 option catalog"))
}
