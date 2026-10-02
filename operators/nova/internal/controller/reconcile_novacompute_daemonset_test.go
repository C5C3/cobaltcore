// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/c5c3/cobaltcore/internal/common/testutil/simulators"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

var novaComputeDaemonSetKey = types.NamespacedName{Namespace: testNamespace, Name: testPoolName + "-nova-compute"}

// daemonSetPass is a pass whose earlier steps resolved the Nova and the config.
func daemonSetPass() *novaComputePass {
	return &novaComputePass{
		image:         commonv1.ImageSpec{Repository: novaComputeDefaultRepository, Tag: "2025.2"},
		secretName:    testContract,
		configMapName: testPoolName + "-config-abc",
		configHash:    "hash-1",
	}
}

// ownedDaemonSet is the pool's DaemonSet as a previous pass left it, with the
// given counters.
func ownedDaemonSet(t *testing.T, cr *novav1alpha1.NovaCompute, desired, ready int32) *appsv1.DaemonSet {
	t.Helper()
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: novaComputeDaemonSetKey.Name, Namespace: novaComputeDaemonSetKey.Namespace,
	}}
	if err := controllerutil.SetControllerReference(cr, ds, testScheme()); err != nil {
		t.Fatalf("setting the controller reference: %v", err)
	}
	ds.Status = appsv1.DaemonSetStatus{
		DesiredNumberScheduled: desired, CurrentNumberScheduled: desired,
		UpdatedNumberScheduled: ready, NumberReady: ready,
	}
	return ds
}

// reconcilePoolDaemonSet runs the PoolConfig and DaemonSet steps of one pass
// and returns the DaemonSet they leave behind.
func reconcilePoolDaemonSet(t *testing.T, r *NovaComputeReconciler, cr *novav1alpha1.NovaCompute) *appsv1.DaemonSet {
	t.Helper()
	g := NewGomegaWithT(t)
	ctx := context.Background()
	pass := daemonSetPass()
	_, err := r.reconcileNovaComputeConfig(ctx, r.Client, cr, pass)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, pass)
	g.Expect(err).NotTo(HaveOccurred())
	ds := &appsv1.DaemonSet{}
	g.Expect(r.Get(ctx, novaComputeDaemonSetKey, ds)).To(Succeed())
	return ds
}

// mountedPoolConfigMapName returns the ConfigMap a NovaCompute pod spec mounts
// its pool config from.
func mountedPoolConfigMapName(spec *corev1.PodSpec) string {
	for _, v := range spec.Volumes {
		if v.Name == poolConfigVolume && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

func deletingNovaCompute() *novav1alpha1.NovaCompute {
	cr := validNovaCompute()
	cr.DeletionTimestamp = ptr.To(metav1.Now())
	cr.Finalizers = []string{novaComputeDrainFinalizer}
	return cr
}

func TestNovaComputeAffinity(t *testing.T) {
	inTerm := func(node string) corev1.NodeSelectorTerm {
		return corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node},
		}}}
	}

	t.Run("the selector term carries every label and one NotIn per excluded node", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := validNovaCompute()
		cr.Spec.NodeSelector = map[string]string{"z": "1", testPoolLabel: "a"}

		affinity := novaComputeAffinity(cr, []string{"node-c", "node-a"}, nil)

		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		g.Expect(terms).To(Equal([]corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: testPoolLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}},
				{Key: "z", Operator: corev1.NodeSelectorOpIn, Values: []string{"1"}},
			},
			MatchFields: []corev1.NodeSelectorRequirement{
				{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-a"}},
				{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"node-c"}},
			},
		}}))
	})

	t.Run("each held node is a term of its own", func(t *testing.T) {
		g := NewGomegaWithT(t)
		affinity := novaComputeAffinity(validNovaCompute(), nil, []string{"node-b", "node-a"})

		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		g.Expect(terms).To(HaveLen(3))
		g.Expect(terms[1:]).To(Equal([]corev1.NodeSelectorTerm{inTerm("node-a"), inTerm("node-b")}))
	})

	t.Run("a deleting pool has no selector term", func(t *testing.T) {
		g := NewGomegaWithT(t)
		affinity := novaComputeAffinity(deletingNovaCompute(), []string{"node-x"}, []string{"node-a"})

		g.Expect(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).
			To(Equal([]corev1.NodeSelectorTerm{inTerm("node-a")}))
	})

	t.Run("no term is nil", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(novaComputeAffinity(deletingNovaCompute(), nil, nil)).To(BeNil())
	})
}

func TestReconcileNovaComputeDaemonSet_ProgressingMirrorsTheCounters(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 3, 2))
	pass := daemonSetPass()

	result, err := r.reconcileNovaComputeDaemonSet(context.Background(), r.Client, cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pass.daemonSetRendered).To(BeTrue(), "the VPA step targets the applied DaemonSet")
	g.Expect(result.IsZero()).To(BeTrue(), "a rollout is not a wait: the aggregates and the drain still run")
	cond := novaComputeCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonDaemonSetProgressing))
	g.Expect(cond.Message).To(ContainSubstring("2 of 3 nodes"))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeEquivalentTo(3))
	g.Expect(cr.Status.InstalledImage).To(BeEmpty())
}

func TestReconcileNovaComputeDaemonSet_ReadyRecordsTheImage(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 3, 2))
	_, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(simulators.MarkDaemonSetReady(ctx, r.Client, novaComputeDaemonSetKey)).To(Succeed())

	result, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(novaComputeCondition(cr, conditionTypeDaemonSetReady).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cr.Status.InstalledImage).To(Equal("ghcr.io/c5c3/nova-compute:2025.2"))
}

// TestReconcileNovaComputeDaemonSet_ZeroTermsDeletesTheDaemonSet pins the
// teardown path: a deleting pool that holds no draining node applies nothing,
// and deletes the DaemonSet it owns, which is what releases the pod of its last
// Releasing node.
func TestReconcileNovaComputeDaemonSet_ZeroTermsDeletesTheDaemonSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := deletingNovaCompute()
	r := newNovaComputeTestReconciler(nil, cr, ownedDaemonSet(t, cr, 1, 1))
	pass := daemonSetPass()

	result, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, pass)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pass.daemonSetRendered).To(BeFalse(), "the VPA step targets no deleted DaemonSet")
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(apierrors.IsNotFound(r.Get(ctx, novaComputeDaemonSetKey, &appsv1.DaemonSet{}))).To(BeTrue())
	cond := novaComputeCondition(cr, conditionTypeDaemonSetReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(ContainSubstring("removed"))
	g.Expect(cr.Status.DesiredNumberScheduled).To(BeZero())
}

func TestReconcileNovaComputeDaemonSet_ZeroTermsLeavesAForeignDaemonSet(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := deletingNovaCompute()
	foreign := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: novaComputeDaemonSetKey.Name, Namespace: novaComputeDaemonSetKey.Namespace,
	}}
	r := newNovaComputeTestReconciler(nil, cr, foreign)

	_, err := r.reconcileNovaComputeDaemonSet(ctx, r.Client, cr, daemonSetPass())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(r.Get(ctx, novaComputeDaemonSetKey, &appsv1.DaemonSet{})).To(Succeed())
}

// TestReconcileNovaComputeDaemonSet_RotatedContractRollsThePods runs the
// PoolConfig and DaemonSet steps against two passwords: the hash annotation
// on the pod template follows the contract.
func TestReconcileNovaComputeDaemonSet_RotatedContractRollsThePods(t *testing.T) {
	g := NewGomegaWithT(t)

	annotationFor := func(password string) string {
		cr := validNovaCompute()
		r := newNovaComputeTestReconciler(nil, cr, computeContractSecret(password))
		return reconcilePoolDaemonSet(t, r, cr).Spec.Template.Annotations[novaComputeConfigHashAnnotation]
	}

	first := annotationFor("pw-1")
	g.Expect(first).NotTo(BeEmpty())
	g.Expect(annotationFor("pw-1")).To(Equal(first))
	g.Expect(annotationFor("pw-2")).NotTo(Equal(first))
}

// TestReconcileNovaComputeDaemonSet_ChangedLibvirtSettingsRollThePods runs the
// PoolConfig and DaemonSet steps on one pool before and after a cpuMode change:
// the pod template moves to a new ConfigMap that carries the new keys, and the
// previous ConfigMap stays for the pods that still mount it under OnDelete.
func TestReconcileNovaComputeDaemonSet_ChangedLibvirtSettingsRollThePods(t *testing.T) {
	g := NewGomegaWithT(t)
	ctx := context.Background()
	cr := validNovaCompute()
	cr.Spec.Libvirt = novav1alpha1.NovaComputeLibvirtSpec{VirtType: "kvm", CPUMode: "host-passthrough"}
	r := newNovaComputeTestReconciler(nil, cr, computeContractSecret("pw-1"))

	first := mountedPoolConfigMapName(&reconcilePoolDaemonSet(t, r, cr).Spec.Template.Spec)
	g.Expect(first).NotTo(BeEmpty())
	g.Expect(mountedPoolConfigMapName(&reconcilePoolDaemonSet(t, r, cr).Spec.Template.Spec)).To(Equal(first))

	cr.Spec.Libvirt = novav1alpha1.NovaComputeLibvirtSpec{
		VirtType: "kvm", CPUMode: "custom", CPUModels: []string{"Skylake-Server-IBRS"},
	}
	second := mountedPoolConfigMapName(&reconcilePoolDaemonSet(t, r, cr).Spec.Template.Spec)
	g.Expect(second).NotTo(BeEmpty())
	g.Expect(second).NotTo(Equal(first))

	cm := &corev1.ConfigMap{}
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: second}, cm)).To(Succeed())
	g.Expect(cm.Data[poolConfigFile]).To(ContainSubstring("cpu_mode = custom"))
	g.Expect(cm.Data[poolConfigFile]).To(ContainSubstring("cpu_models = Skylake-Server-IBRS"))
	g.Expect(r.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: first}, &corev1.ConfigMap{})).
		To(Succeed(), "an OnDelete pod still mounts the previous ConfigMap")
}

func TestNovaComputeUpdateStrategy(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	g.Expect(novaComputeUpdateStrategy(cr).RollingUpdate.MaxUnavailable.IntValue()).To(Equal(1))

	cr.Spec.UpdateStrategy.Type = "OnDelete"
	g.Expect(novaComputeUpdateStrategy(cr)).To(Equal(&appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}))
}

// TestBuildNovaComputeDaemonSet_CreateInstancesDir pins the init container that
// creates instances_path on the node: it runs before the chassis gate, as root
// without the privileged profile, on the state directory alone, with the pool's
// resources.
func TestBuildNovaComputeDaemonSet_CreateInstancesDir(t *testing.T) {
	render := func(cr *novav1alpha1.NovaCompute) corev1.PodSpec {
		return buildNovaComputeDaemonSet(cr, pinNovaComputeImage(), testContract, pinNovaComputeConfigMap,
			pinNovaComputeHash, novaComputeAffinity(validNovaCompute(), nil, nil)).Spec.Template.Spec
	}

	t.Run("it runs first, as root with DAC_OVERRIDE alone, on the state directory", func(t *testing.T) {
		g := NewGomegaWithT(t)
		pod := render(validNovaCompute())

		var names []string
		for _, c := range pod.InitContainers {
			names = append(names, c.Name)
		}
		g.Expect(names).To(Equal([]string{"create-instances-dir", "wait-for-chassis"}))

		c := pod.InitContainers[0]
		g.Expect(c.Command).To(Equal([]string{"mkdir", "-p", "-m", "0755", "/var/lib/nova/instances"}))
		g.Expect(c.Image).To(Equal(pod.Containers[0].Image))
		g.Expect(c.SecurityContext).To(Equal(&corev1.SecurityContext{
			Privileged:               ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsUser:                ptr.To(int64(0)),
			RunAsGroup:               ptr.To(int64(0)),
			RunAsNonRoot:             ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}))
		g.Expect(c.VolumeMounts).To(Equal([]corev1.VolumeMount{{Name: "var-lib-nova", MountPath: "/var/lib/nova"}}),
			"one mount, writable and without propagation")
	})

	t.Run("nil spec.resources renders none", func(t *testing.T) {
		g := NewGomegaWithT(t)
		g.Expect(render(validNovaCompute()).InitContainers[0].Resources).To(Equal(corev1.ResourceRequirements{}))
	})

	t.Run("spec.resources applies to it too", func(t *testing.T) {
		g := NewGomegaWithT(t)
		cr := pinOnDeleteNovaCompute()
		g.Expect(render(cr).InitContainers[0].Resources).To(Equal(*cr.Spec.Resources))
	})
}

// runCreateInstancesDir runs the rendered command for the directory at path and
// returns its error and what it wrote to stderr.
//
// The command travels through a script file, run from the parent of path, so
// the argument list of the test's own exec stays constant. Every word of the
// command is a plain shell word, so joining them with spaces gives the argument
// list the container runs. The script sets a restrictive umask before it execs
// the command: the umask applies to the child alone and the test process keeps
// its own.
func runCreateInstancesDir(t *testing.T, path string) (string, error) {
	t.Helper()
	for _, binary := range []string{shPath, "mkdir"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is unavailable; the rendered command cannot be exercised here: %v", binary, err)
		}
	}
	command := createInstancesDirCommand(filepath.Base(path))
	writeScriptFile(t, filepath.Dir(path), scriptFileName, "umask 0077\nexec "+strings.Join(command, " ")+"\n")

	cmd := exec.CommandContext(t.Context(), shPath, scriptFileName)
	cmd.Dir = filepath.Dir(path)
	// mkdir words its errors in the caller's locale; the container has none.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func TestCreateInstancesDirCommand(t *testing.T) {
	t.Run("creates a missing directory with mode 0755 whatever the umask", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		path := filepath.Join(t.TempDir(), "instances")

		stderr, err := runCreateInstancesDir(t, path)

		g.Expect(err).NotTo(HaveOccurred(), stderr)
		info, err := os.Stat(path)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(info.IsDir()).To(BeTrue())
		g.Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o755)))
	})

	t.Run("leaves the mode of an existing directory alone", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		path := filepath.Join(t.TempDir(), "instances")
		g.Expect(os.Mkdir(path, 0o700)).To(Succeed())

		stderr, err := runCreateInstancesDir(t, path)

		g.Expect(err).NotTo(HaveOccurred(), stderr)
		info, err := os.Stat(path)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	t.Run("fails on a regular file at the path", func(t *testing.T) {
		t.Parallel()
		g := NewGomegaWithT(t)
		path := filepath.Join(t.TempDir(), "instances")
		g.Expect(os.WriteFile(path, nil, 0o600)).To(Succeed())

		stderr, err := runCreateInstancesDir(t, path)

		var exitErr *exec.ExitError
		g.Expect(errors.As(err, &exitErr)).To(BeTrue(), "the command must fail by exiting, got %v", err)
		g.Expect(stderr).To(ContainSubstring("File exists"))
	})
}
