// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// validNovaCompute returns a NovaCompute that passes every validation rule.
// Tests mutate single fields to exercise individual rules.
func validNovaCompute() *NovaCompute {
	return &NovaCompute{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "openstack"},
		Spec: NovaComputeSpec{
			NovaRef:      NovaRef{Name: "nova"},
			NodeSelector: map[string]string{"openstack.c5c3.io/nova-compute-pool": "a"},
			Libvirt:      NovaComputeLibvirtSpec{VirtType: "kvm"},
			UpdateStrategy: NovaComputeUpdateStrategy{
				Type: "RollingUpdate",
			},
		},
	}
}

// countingReader wraps a reader and counts its Get calls, so a test can assert
// that a path read nothing. err, when set, is returned by every Get.
type countingReader struct {
	client.Reader
	gets int
	err  error
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	if r.err != nil {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// novaComputeWebhookWith returns a webhook whose reader holds objs, and the
// reader itself so the caller can count its reads.
func novaComputeWebhookWith(objs ...client.Object) (*NovaComputeWebhook, *countingReader) {
	s := runtime.NewScheme()
	_ = AddToScheme(s)
	reader := &countingReader{Reader: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
	return &NovaComputeWebhook{Client: reader}, reader
}

// referencedNova is the Nova validNovaCompute names, in the same namespace.
func referencedNova() *Nova {
	nova := validNova()
	nova.Name = "nova"
	nova.Namespace = "openstack"
	return nova
}

// expectInvalid asserts err is an Invalid error carrying want.
func expectInvalid(t *testing.T, err error, want string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
	g.Expect(err.Error()).To(ContainSubstring(want))
}

func TestNovaComputeWebhook_DefaultIsANoOp(t *testing.T) {
	g := NewGomegaWithT(t)
	w, _ := novaComputeWebhookWith()
	nc := validNovaCompute()
	before := nc.DeepCopy()
	g.Expect(w.Default(context.Background(), nc)).To(Succeed())
	g.Expect(nc).To(Equal(before))
}

func TestNovaComputeWebhook_ValidateCreate_AdmitsAValidPool(t *testing.T) {
	g := NewGomegaWithT(t)
	w, _ := novaComputeWebhookWith(referencedNova())
	warnings, err := w.ValidateCreate(context.Background(), validNovaCompute())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(warnings).To(BeEmpty())
}

// TestNovaComputeWebhook_ValidateCreate_RejectsEachRule walks every rule
// validate() enforces, one mutation at a time, so each rule is known to fire on
// its own and to carry its own message.
func TestNovaComputeWebhook_ValidateCreate_RejectsEachRule(t *testing.T) {
	offboarding := "tolerates kvm.cloud.sap/offboarding:NoExecute; openstack-hypervisor-operator deletes " +
		"the compute service only after every agent pod on the node is gone, and a nova-compute that stays re-registers it"
	cpuModels := "cpuModels is required when cpuMode is custom and must be empty otherwise"

	for _, tc := range []struct {
		name string
		edit func(*NovaCompute)
		want string
	}{
		{
			name: "empty novaRef name",
			edit: func(nc *NovaCompute) { nc.Spec.NovaRef.Name = "" },
			want: "novaRef.name must be set (the Nova this node pool joins)",
		},
		{
			name: "empty nodeSelector",
			edit: func(nc *NovaCompute) { nc.Spec.NodeSelector = nil },
			want: "nodeSelector must carry at least one label",
		},
		{
			name: "nodeSelector key that is not a qualified name",
			edit: func(nc *NovaCompute) { nc.Spec.NodeSelector = map[string]string{"bad key": "a"} },
			want: "name part must consist of alphanumeric characters",
		},
		{
			name: "nodeSelector value that is not a label value",
			edit: func(nc *NovaCompute) { nc.Spec.NodeSelector = map[string]string{"pool": "a b"} },
			want: "a valid label must be an empty string or consist of alphanumeric characters",
		},
		{
			name: "offboarding toleration by key",
			edit: func(nc *NovaCompute) {
				nc.Spec.Tolerations = []corev1.Toleration{{
					Key: OffboardingTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute,
				}}
			},
			want: offboarding,
		},
		{
			name: "offboarding toleration with an empty effect",
			edit: func(nc *NovaCompute) {
				nc.Spec.Tolerations = []corev1.Toleration{{Key: OffboardingTaintKey, Operator: corev1.TolerationOpExists}}
			},
			want: offboarding,
		},
		{
			name: "wildcard toleration",
			edit: func(nc *NovaCompute) {
				nc.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
			},
			want: offboarding,
		},
		{
			name: "maxUnavailable with OnDelete",
			edit: func(nc *NovaCompute) {
				nc.Spec.UpdateStrategy = NovaComputeUpdateStrategy{Type: "OnDelete", MaxUnavailable: ptr.To(intstr.FromInt32(1))}
			},
			want: "maxUnavailable applies to RollingUpdate only",
		},
		{
			name: "maxUnavailable of 0%",
			edit: func(nc *NovaCompute) { nc.Spec.UpdateStrategy.MaxUnavailable = ptr.To(intstr.FromString("0%")) },
			want: "maxUnavailable must resolve to at least 1 for RollingUpdate",
		},
		{
			name: "malformed maxUnavailable",
			edit: func(nc *NovaCompute) { nc.Spec.UpdateStrategy.MaxUnavailable = ptr.To(intstr.FromString("half")) },
			want: "maxUnavailable must be an integer or a percentage",
		},
		{
			name: "cpuModels without custom",
			edit: func(nc *NovaCompute) { nc.Spec.Libvirt.CPUModels = []string{"Haswell"} },
			want: cpuModels,
		},
		{
			name: "custom without cpuModels",
			edit: func(nc *NovaCompute) { nc.Spec.Libvirt.CPUMode = "custom" },
			want: cpuModels,
		},
		{
			name: "rejected extraConfig key",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"host": "node-1"}}
			},
			want: "spec.extraConfig[DEFAULT][host]: Forbidden: host is managed via spec.nodeName (downward API) and must not be set in extraConfig",
		},
		{
			name: "rejected password key",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"placement": {"password": "secret"}}
			},
			want: "spec.extraConfig[placement][password]",
		},
		{
			name: "live_migration_scheme in extraConfig",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_scheme": "tcp"}}
			},
			want: "spec.extraConfig[libvirt][live_migration_scheme]: Forbidden: live_migration_scheme is managed via operator-computed and must not be set in extraConfig",
		},
		{
			name: "live_migration_with_native_tls in extraConfig",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_with_native_tls": "false"}}
			},
			want: "spec.extraConfig[libvirt][live_migration_with_native_tls]: Forbidden: live_migration_with_native_tls is managed via operator-computed and must not be set in extraConfig",
		},
		{
			name: "live_migration_uri in extraConfig",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_uri": "qemu+tcp://%s/system"}}
			},
			want: "spec.extraConfig[libvirt][live_migration_uri]: Forbidden: live_migration_uri is managed via operator-computed and must not be set in extraConfig",
		},
		{
			name: "live_migration_tunnelled in extraConfig",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_tunnelled": "true"}}
			},
			want: "spec.extraConfig[libvirt][live_migration_tunnelled]: Forbidden: live_migration_tunnelled is managed via operator-computed and must not be set in extraConfig",
		},
		{
			name: "live_migration_inbound_addr in extraConfig",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_inbound_addr": "192.0.2.10"}}
			},
			want: "spec.extraConfig[libvirt][live_migration_inbound_addr]: Forbidden: live_migration_inbound_addr is managed via status.hostIP (downward API) and must not be set in extraConfig",
		},
		{
			name: "newline in an extraConfig value",
			edit: func(nc *NovaCompute) {
				nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": "true\n[x]"}}
			},
			want: "must not contain a newline or carriage return",
		},
		{
			name: "image with tag and digest",
			edit: func(nc *NovaCompute) {
				nc.Spec.Image = &commonv1.ImageSpec{
					Repository: "ghcr.io/c5c3/nova-compute",
					Tag:        "2025.2",
					Digest:     "sha256:" + strings.Repeat("a", 64),
				}
			},
			want: "exactly one of image.tag or image.digest must be set",
		},
		{
			name: "targetClusterRef with an empty name",
			edit: func(nc *NovaCompute) { nc.Spec.TargetClusterRef = &commonv1.TargetClusterRefSpec{} },
			want: "spec.targetClusterRef.name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := novaComputeWebhookWith(referencedNova())
			nc := validNovaCompute()
			tc.edit(nc)
			_, err := w.ValidateCreate(context.Background(), nc)
			expectInvalid(t, err, tc.want)
		})
	}
}

// TestNovaComputeWebhook_ValidateCreate_CollectsEveryViolation pins the single
// aggregated response: two independent violations come back together.
func TestNovaComputeWebhook_ValidateCreate_CollectsEveryViolation(t *testing.T) {
	g := NewGomegaWithT(t)
	w, _ := novaComputeWebhookWith(referencedNova())
	nc := validNovaCompute()
	nc.Spec.NovaRef.Name = ""
	nc.Spec.NodeSelector = nil
	_, err := w.ValidateCreate(context.Background(), nc)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("novaRef.name must be set"))
	g.Expect(err.Error()).To(ContainSubstring("nodeSelector must carry at least one label"))
}

// TestNovaComputeWebhook_ValidateCreate_AdmitsAnOffboardingTolerationWithSeconds
// pins the exemption: hvo counts a toleration with tolerationSeconds as
// evictable, so the pod leaves the node on its own.
func TestNovaComputeWebhook_ValidateCreate_AdmitsAnOffboardingTolerationWithSeconds(t *testing.T) {
	g := NewGomegaWithT(t)
	w, _ := novaComputeWebhookWith(referencedNova())
	nc := validNovaCompute()
	nc.Spec.Tolerations = []corev1.Toleration{
		{
			Key: OffboardingTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute,
			TolerationSeconds: ptr.To(int64(30)),
		},
		// A toleration of another taint, or of the offboarding key with another
		// effect, never matches.
		{Key: "node-role.kubernetes.io/compute", Operator: corev1.TolerationOpExists},
		{Key: OffboardingTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	_, err := w.ValidateCreate(context.Background(), nc)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestNovaComputeWebhook_ValidateCreate_RejectsAnOverlongName(t *testing.T) {
	w, _ := novaComputeWebhookWith(referencedNova())
	nc := validNovaCompute()
	nc.Name = strings.Repeat("p", MaxNovaComputeNameLength+1)
	_, err := w.ValidateCreate(context.Background(), nc)
	expectInvalid(t, err, "name must be at most 63 characters: it is the app.kubernetes.io/instance label value of every child")

	g := NewGomegaWithT(t)
	nc.Name = strings.Repeat("p", MaxNovaComputeNameLength)
	_, err = w.ValidateCreate(context.Background(), nc)
	g.Expect(err).NotTo(HaveOccurred(), "a name of exactly the bound is admitted")
}

// TestNovaComputeWebhook_CatalogCheck covers the three shapes of the
// extraConfig catalog check: against the referenced Nova's release, skipped
// with one warning without it, and not read at all without an overlay.
func TestNovaComputeWebhook_CatalogCheck(t *testing.T) {
	t.Run("an unknown option is rejected while the Nova exists", func(t *testing.T) {
		w, _ := novaComputeWebhookWith(referencedNova())
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		_, err := w.ValidateCreate(context.Background(), nc)
		expectInvalid(t, err, "no such option in the nova 2025.2 option catalog")
	})

	t.Run("a known option is admitted without a warning", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"compute_driver": "fake.FakeDriverWithoutFakeNodes"}}
		warnings, err := w.ValidateCreate(context.Background(), nc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})

	t.Run("an absent Nova skips the check with exactly one warning", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith()
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		warnings, err := w.ValidateCreate(context.Background(), nc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(Equal(admission.Warnings{"extraConfig catalog check skipped: Nova openstack/nova not found"}))
	})

	t.Run("a live-migration key is rejected without the Nova", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith()
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_scheme": "tcp"}}
		warnings, err := w.ValidateCreate(context.Background(), nc)
		expectInvalid(t, err, "spec.extraConfig[libvirt][live_migration_scheme]: Forbidden: "+
			"live_migration_scheme is managed via operator-computed and must not be set in extraConfig")
		g.Expect(warnings).To(Equal(admission.Warnings{"extraConfig catalog check skipped: Nova openstack/nova not found"}))
	})

	t.Run("an unowned live-migration option is admitted without a warning", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_permit_auto_converge": "true"}}
		warnings, err := w.ValidateCreate(context.Background(), nc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})

	t.Run("an empty libvirt section is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {}}
		warnings, err := w.ValidateCreate(context.Background(), nc)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeEmpty())
	})

	t.Run("an empty overlay reads nothing and warns nothing", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, reader := novaComputeWebhookWith()
		warnings, err := w.ValidateCreate(context.Background(), validNovaCompute())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(BeNil())
		g.Expect(reader.gets).To(BeZero())
	})

	t.Run("a failed read fails admission", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, reader := novaComputeWebhookWith()
		reader.err = errors.New("connection refused")
		nc := validNovaCompute()
		nc.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"compute_driver": "libvirt.LibvirtDriver"}}
		_, err := w.ValidateCreate(context.Background(), nc)
		g.Expect(err).To(HaveOccurred())
		g.Expect(apierrors.IsInternalError(err)).To(BeTrue(), "got %v", err)
	})
}

func TestNovaComputeWebhook_ValidateUpdate(t *testing.T) {
	t.Run("a nodeSelector change is admitted", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		newObj := oldObj.DeepCopy()
		newObj.Spec.NodeSelector = map[string]string{"openstack.c5c3.io/nova-compute-pool": "b"}
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("a novaRef change is rejected", func(t *testing.T) {
		w, _ := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		newObj := oldObj.DeepCopy()
		newObj.Spec.NovaRef.Name = "other"
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		expectInvalid(t, err, "novaRef is immutable")
	})

	for _, tc := range []struct {
		name     string
		old, new *commonv1.TargetClusterRefSpec
	}{
		{name: "adding targetClusterRef", new: &commonv1.TargetClusterRefSpec{Name: "compute-a"}},
		{name: "removing targetClusterRef", old: &commonv1.TargetClusterRefSpec{Name: "compute-a"}},
	} {
		t.Run(tc.name+" is rejected", func(t *testing.T) {
			w, _ := novaComputeWebhookWith(referencedNova())
			oldObj := validNovaCompute()
			oldObj.Spec.TargetClusterRef = tc.old
			newObj := oldObj.DeepCopy()
			newObj.Spec.TargetClusterRef = tc.new
			_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
			expectInvalid(t, err, "targetClusterRef is immutable")
		})
	}

	t.Run("an unchanged overlay skips the catalog check", func(t *testing.T) {
		g := NewGomegaWithT(t)
		// The overlay names an option no catalog carries, the state a CR reaches
		// when a newer catalog dropped an option it was admitted with.
		w, reader := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		oldObj.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		newObj := oldObj.DeepCopy()
		newObj.Spec.NodeSelector = map[string]string{"openstack.c5c3.io/nova-compute-pool": "b"}
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reader.gets).To(BeZero())
	})

	t.Run("a changed overlay is checked again", func(t *testing.T) {
		w, _ := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		newObj := oldObj.DeepCopy()
		newObj.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"cpu_allocation_ration": "16.0"}}
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		expectInvalid(t, err, "no such option in the nova 2025.2 option catalog")
	})

	t.Run("adding a live-migration key is rejected", func(t *testing.T) {
		w, _ := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		newObj := oldObj.DeepCopy()
		newObj.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_with_native_tls": "false"}}
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		expectInvalid(t, err, "spec.extraConfig[libvirt][live_migration_with_native_tls]: Forbidden: "+
			"live_migration_with_native_tls is managed via operator-computed and must not be set in extraConfig")
	})

	// A pool admitted before the live-migration keys were rejected still
	// carries one. Its finalizer removal leaves the spec alone and must pass, or
	// the CR stays in Terminating; a spec edit on it is still validated.
	t.Run("a deleting pool that carries a now-rejected key releases its finalizer", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		stale := validNovaCompute()
		stale.Spec.ExtraConfig = map[string]map[string]string{"libvirt": {"live_migration_tunnelled": "false"}}
		stale.Finalizers = []string{"nova.openstack.c5c3.io/compute-drain"}
		stale.DeletionTimestamp = ptr.To(metav1.Now())

		released := stale.DeepCopy()
		released.Finalizers = nil
		_, err := w.ValidateUpdate(context.Background(), stale, released)
		g.Expect(err).NotTo(HaveOccurred())

		edited := released.DeepCopy()
		edited.Spec.NodeSelector = map[string]string{"openstack.c5c3.io/nova-compute-pool": "b"}
		_, err = w.ValidateUpdate(context.Background(), stale, edited)
		expectInvalid(t, err, "spec.extraConfig[libvirt][live_migration_tunnelled]: Forbidden")
	})

	t.Run("an overlong name is not re-checked on update", func(t *testing.T) {
		g := NewGomegaWithT(t)
		w, _ := novaComputeWebhookWith(referencedNova())
		oldObj := validNovaCompute()
		oldObj.Name = strings.Repeat("p", MaxNovaComputeNameLength+1)
		newObj := oldObj.DeepCopy()
		newObj.Finalizers = nil
		_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
		g.Expect(err).NotTo(HaveOccurred())
	})
}
