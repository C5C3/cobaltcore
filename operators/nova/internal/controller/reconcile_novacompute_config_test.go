// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/config"
	commonreconcile "github.com/c5c3/cobaltcore/internal/common/reconcile"
	novav1alpha1 "github.com/c5c3/cobaltcore/operators/nova/api/v1alpha1"
)

// runPoolConfig runs the PoolConfig step with the contract name resolved.
func runPoolConfig(t *testing.T, cr *novav1alpha1.NovaCompute, objs ...client.Object) (*NovaComputeReconciler, *novaComputePass) {
	t.Helper()
	r := newNovaComputeTestReconciler(nil, objs...)
	pass := &novaComputePass{secretName: testContract}
	_, err := r.reconcileNovaComputeConfig(context.Background(), r.Client, cr, pass)
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return r, pass
}

func TestNovaComputePoolDefaults(t *testing.T) {
	g := NewGomegaWithT(t)

	g.Expect(novaComputePoolDefaults(validNovaCompute())).To(Equal(map[string]map[string]string{
		"DEFAULT":          {"compute_driver": "libvirt.LibvirtDriver", "state_path": "/var/lib/nova"},
		"libvirt":          {"virt_type": "kvm", "connection_uri": "qemu:///system"},
		"os_vif_ovs":       {"ovsdb_connection": "unix:/run/openvswitch/db.sock"},
		"oslo_concurrency": {"lock_path": "/var/lib/nova/tmp"},
		"vnc":              {"server_listen": "$my_ip"},
	}))

	custom := validNovaCompute()
	custom.Spec.Libvirt = novav1alpha1.NovaComputeLibvirtSpec{
		VirtType: "qemu", CPUMode: "custom", CPUModels: []string{"Haswell-noTSX", "Skylake-Client"}, ImagesType: "qcow2",
	}
	g.Expect(novaComputePoolDefaults(custom)["libvirt"]).To(Equal(map[string]string{
		"virt_type": "qemu", "connection_uri": "qemu:///system",
		"cpu_mode": "custom", "cpu_models": "Haswell-noTSX,Skylake-Client", "images_type": "qcow2",
	}))

	hostModel := validNovaCompute()
	hostModel.Spec.Libvirt.CPUMode = "host-model"
	g.Expect(novaComputePoolDefaults(hostModel)["libvirt"]).NotTo(HaveKey("cpu_models"))

	// A CR that bypassed the schema default still renders kvm.
	bare := validNovaCompute()
	bare.Spec.Libvirt.VirtType = ""
	g.Expect(novaComputePoolDefaults(bare)["libvirt"]).To(HaveKeyWithValue("virt_type", "kvm"))
}

// TestNovaComputePoolDefaults_OnlyOwnedOrDefaultKeys pins the registry to the
// renderer: every rendered key is either owned or one of the two defaults the
// registry documents as not owned.
func TestNovaComputePoolDefaults_OnlyOwnedOrDefaultKeys(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.Libvirt = novav1alpha1.NovaComputeLibvirtSpec{
		VirtType: "kvm", CPUMode: "custom", CPUModels: []string{"Haswell"}, ImagesType: "raw",
	}

	owned := map[string]bool{"DEFAULT/compute_driver": true, "vnc/server_listen": true}
	for _, k := range novav1alpha1.NovaComputeOwnedConfigKeys {
		owned[k.Section+"/"+k.Key] = true
	}
	for section, keys := range novaComputePoolDefaults(cr) {
		for key := range keys {
			g.Expect(owned).To(HaveKey(section+"/"+key), "rendered %s/%s is neither owned nor a documented default", section, key)
		}
	}
}

func TestReconcileNovaComputeConfig_WaitsForTheContract(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	r := newNovaComputeTestReconciler(nil)

	result, err := r.reconcileNovaComputeConfig(context.Background(), r.Client, cr, &novaComputePass{secretName: testContract})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	cond := novaComputeCondition(cr, conditionTypeConfigReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(conditionReasonWaitingForComputeConfig))
	g.Expect(cond.Message).To(ContainSubstring("nova-compute-config not found in namespace openstack on the local cluster"))
	g.Expect(cond.Message).To(ContainSubstring("docs/guides/nova/connect-a-compute-cluster.md"))
}

func TestReconcileNovaComputeConfig_IncompleteContract(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	contract := computeContractSecret("pw")
	delete(contract.Data, transportURLKey)
	contract.Data[passwordKey] = nil
	r := newNovaComputeTestReconciler(nil, contract)

	result, err := r.reconcileNovaComputeConfig(context.Background(), r.Client, cr, &novaComputePass{secretName: testContract})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(commonreconcile.RequeueSecretPolling))
	cond := novaComputeCondition(cr, conditionTypeConfigReady)
	g.Expect(cond.Reason).To(Equal(conditionReasonComputeConfigIncomplete))
	g.Expect(cond.Message).To(ContainSubstring("lacks transport_url, password"))
}

func TestReconcileNovaComputeConfig_RendersThePool(t *testing.T) {
	g := NewGomegaWithT(t)
	cr := validNovaCompute()
	cr.Spec.ExtraConfig = map[string]map[string]string{
		"DEFAULT": {"compute_driver": "fake.FakeDriverWithoutFakeNodes"},
		"libvirt": {"virt_type": "qemu"},
	}
	r, pass := runPoolConfig(t, cr, computeContractSecret("pw"))

	g.Expect(pass.configMapName).To(HavePrefix(testPoolName + "-config-"))
	g.Expect(pass.configHash).To(HaveLen(64))
	cond := novaComputeCondition(cr, conditionTypeConfigReady)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(conditionReasonConfigRendered))

	cm := &corev1.ConfigMap{}
	g.Expect(r.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: pass.configMapName}, cm)).To(Succeed())
	g.Expect(cm.Data[poolConfigFile]).To(ContainSubstring("compute_driver = fake.FakeDriverWithoutFakeNodes"))
	g.Expect(cm.Data[poolConfigFile]).To(ContainSubstring("virt_type = qemu"))
	g.Expect(cm.Data[poolConfigFile]).NotTo(ContainSubstring("pw"))

	// The override of an owned key is honored and reported.
	health := novaComputeCondition(cr, config.ConditionTypeExtraConfigHealthy)
	g.Expect(health).NotTo(BeNil())
	g.Expect(health.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(health.Message).To(ContainSubstring("virt_type"))
}

func TestReconcileNovaComputeConfig_HashFollowsTheContract(t *testing.T) {
	g := NewGomegaWithT(t)
	_, first := runPoolConfig(t, validNovaCompute(), computeContractSecret("pw-1"))
	_, again := runPoolConfig(t, validNovaCompute(), computeContractSecret("pw-1"))
	_, rotated := runPoolConfig(t, validNovaCompute(), computeContractSecret("pw-2"))

	g.Expect(again.configHash).To(Equal(first.configHash))
	g.Expect(rotated.configHash).NotTo(Equal(first.configHash))
}

// TestReconcileNovaComputeConfig_PrunesTheHistory pins the retention: the
// mounted ConfigMap plus the three newest before it.
func TestReconcileNovaComputeConfig_PrunesTheHistory(t *testing.T) {
	g := NewGomegaWithT(t)
	r := newNovaComputeTestReconciler(nil, computeContractSecret("pw"))
	ctx := context.Background()
	for i := range 5 {
		cr := validNovaCompute()
		cr.Spec.ExtraConfig = map[string]map[string]string{"DEFAULT": {"debug": fmt.Sprint(i)}}
		_, err := r.reconcileNovaComputeConfig(ctx, r.Client, cr, &novaComputePass{secretName: testContract})
		g.Expect(err).NotTo(HaveOccurred())
	}

	var list corev1.ConfigMapList
	g.Expect(r.List(ctx, &list, client.InNamespace(testNamespace))).To(Succeed())
	g.Expect(list.Items).To(HaveLen(defaultConfigMapRetainCount + 1))
}
