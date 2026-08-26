// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// daemonSetParams is the minimal valid params set: an addressable object and
// the one container the pod cannot do without. Each test overrides only the
// field it exercises.
func daemonSetParams() DaemonSetParams {
	return DaemonSetParams{
		Namespace:      "ns",
		Name:           "test-chassis",
		Labels:         map[string]string{"app.kubernetes.io/name": "chassis", "app.kubernetes.io/instance": "test-chassis"},
		SelectorLabels: map[string]string{"app.kubernetes.io/name": "chassis"},
		Containers: []corev1.Container{{
			Name:  "ovn-controller",
			Image: "registry.example.com/ovn:2026.1",
		}},
	}
}

// pinDaemonSetParams populates every field the caller supplies, and leaves the
// two the builder defaults (UpdateStrategy, TerminationGracePeriodSeconds)
// unset, so the golden below captures what the builder decides on its own
// beside what it passes through.
func pinDaemonSetParams() DaemonSetParams {
	return DaemonSetParams{
		Namespace: "openstack",
		Name:      "test-chassis",
		Labels: map[string]string{
			"app.kubernetes.io/component":  "chassis",
			"app.kubernetes.io/instance":   "test-chassis",
			"app.kubernetes.io/managed-by": "ovn-operator",
			"app.kubernetes.io/name":       "ovn",
		},
		SelectorLabels: map[string]string{
			"app.kubernetes.io/instance": "test-chassis",
			"app.kubernetes.io/name":     "ovn",
		},
		PodAnnotations: map[string]string{"cobaltcore.sap/config-hash": "abc123"},
		NodeSelector:   map[string]string{"openstack.cobaltcore.sap/chassis": "true"},
		Tolerations: []corev1.Toleration{{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		HostNetwork:        true,
		PriorityClassName:  "system-node-critical",
		ServiceAccountName: "test-chassis",
		PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
		InitContainers: []corev1.Container{{
			Name:            "host-prepare",
			Image:           "registry.example.com/ovn:2026.1",
			Command:         []string{"/bin/sh", "-c", "modprobe openvswitch"},
			SecurityContext: PrivilegedSecurityContext(),
			VolumeMounts:    []corev1.VolumeMount{{Name: "lib-modules", MountPath: "/lib/modules", ReadOnly: true}},
		}},
		Containers: []corev1.Container{{
			Name:            "ovn-controller",
			Image:           "registry.example.com/ovn:2026.1",
			Command:         []string{"ovn-controller"},
			Env:             []corev1.EnvVar{{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}},
			SecurityContext: CapabilitySecurityContext("NET_ADMIN"),
			VolumeMounts:    []corev1.VolumeMount{{Name: "run-ovn", MountPath: "/run/ovn"}},
		}},
		Volumes: []corev1.Volume{
			{Name: "lib-modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"}}},
			{Name: "run-ovn", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}

// Byte-identity pin for the object BuildDaemonSet renders. The golden is
// FULL-OBJECT YAML captured from the builder as it stands, so a changed field
// order, a dropped default, or an extra nil-valued field surfaces here as a
// diff instead of as a silent pod-template churn that rolls every chassis pod
// in the fleet on an operator upgrade.
const pinDaemonSetGolden = `metadata:
  labels:
    app.kubernetes.io/component: chassis
    app.kubernetes.io/instance: test-chassis
    app.kubernetes.io/managed-by: ovn-operator
    app.kubernetes.io/name: ovn
  name: test-chassis
  namespace: openstack
spec:
  selector:
    matchLabels:
      app.kubernetes.io/instance: test-chassis
      app.kubernetes.io/name: ovn
  template:
    metadata:
      annotations:
        cobaltcore.sap/config-hash: abc123
      labels:
        app.kubernetes.io/component: chassis
        app.kubernetes.io/instance: test-chassis
        app.kubernetes.io/managed-by: ovn-operator
        app.kubernetes.io/name: ovn
    spec:
      containers:
      - command:
        - ovn-controller
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        image: registry.example.com/ovn:2026.1
        name: ovn-controller
        resources: {}
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            add:
            - NET_ADMIN
            drop:
            - ALL
          privileged: false
          readOnlyRootFilesystem: true
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /run/ovn
          name: run-ovn
      dnsPolicy: ClusterFirstWithHostNet
      hostNetwork: true
      initContainers:
      - command:
        - /bin/sh
        - -c
        - modprobe openvswitch
        image: registry.example.com/ovn:2026.1
        name: host-prepare
        resources: {}
        securityContext:
          allowPrivilegeEscalation: true
          privileged: true
          readOnlyRootFilesystem: true
        volumeMounts:
        - mountPath: /lib/modules
          name: lib-modules
          readOnly: true
      nodeSelector:
        openstack.cobaltcore.sap/chassis: "true"
      priorityClassName: system-node-critical
      securityContext:
        runAsUser: 0
      serviceAccountName: test-chassis
      terminationGracePeriodSeconds: 30
      tolerations:
      - effect: NoSchedule
        key: node-role.kubernetes.io/control-plane
        operator: Exists
      volumes:
      - hostPath:
          path: /lib/modules
        name: lib-modules
      - emptyDir: {}
        name: run-ovn
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 1
    type: RollingUpdate
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
`

func TestPinBuildDaemonSet(t *testing.T) {
	g := gomega.NewWithT(t)

	got, err := yaml.Marshal(BuildDaemonSet(pinDaemonSetParams()))

	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(got)).To(gomega.Equal(pinDaemonSetGolden),
		"the rendered DaemonSet must stay byte-identical")
}

// One node at a time is what makes a chassis rollout survivable: every pod of
// the DaemonSet is the data plane of the node it sits on, and maxSurge would
// put a second one there while the first still holds the host network.
func TestBuildDaemonSet_NilUpdateStrategyRendersRollingUpdateMaxUnavailableOne(t *testing.T) {
	g := gomega.NewWithT(t)

	ds := BuildDaemonSet(daemonSetParams())

	g.Expect(ds.Spec.UpdateStrategy.Type).To(gomega.Equal(appsv1.RollingUpdateDaemonSetStrategyType))
	g.Expect(ds.Spec.UpdateStrategy.RollingUpdate).NotTo(gomega.BeNil())
	g.Expect(ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable).To(gomega.HaveValue(gomega.Equal(intstr.FromInt32(1))))
	g.Expect(ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge).To(gomega.BeNil())
}

func TestBuildDaemonSet_CustomUpdateStrategyVerbatim(t *testing.T) {
	for name, strategy := range map[string]appsv1.DaemonSetUpdateStrategy{
		"on delete": {Type: appsv1.OnDeleteDaemonSetStrategyType},
		"rolling update with a surge": {
			Type: appsv1.RollingUpdateDaemonSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDaemonSet{
				MaxUnavailable: ptr.To(intstr.FromInt32(0)),
				MaxSurge:       ptr.To(intstr.FromString("10%")),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			p := daemonSetParams()
			p.UpdateStrategy = &strategy

			g.Expect(BuildDaemonSet(p).Spec.UpdateStrategy).To(gomega.Equal(strategy))
		})
	}
}

// A pod in the node's network namespace resolves against the node's
// resolv.conf unless the DNS policy says otherwise, and loses cluster DNS with
// it. The two belong together, so the builder derives one from the other.
func TestBuildDaemonSet_HostNetworkSetsClusterFirstWithHostNet(t *testing.T) {
	g := gomega.NewWithT(t)

	p := daemonSetParams()
	p.HostNetwork = true

	ds := BuildDaemonSet(p)

	g.Expect(ds.Spec.Template.Spec.HostNetwork).To(gomega.BeTrue())
	g.Expect(ds.Spec.Template.Spec.DNSPolicy).To(gomega.Equal(corev1.DNSClusterFirstWithHostNet))
}

// Without host networking the builder names no policy at all, so the API
// server's own default applies and the field never shows up in a diff.
func TestBuildDaemonSet_NoHostNetworkLeavesDNSPolicyEmpty(t *testing.T) {
	g := gomega.NewWithT(t)

	ds := BuildDaemonSet(daemonSetParams())

	g.Expect(ds.Spec.Template.Spec.HostNetwork).To(gomega.BeFalse())
	g.Expect(ds.Spec.Template.Spec.DNSPolicy).To(gomega.BeEmpty())
}

func TestBuildDaemonSet_NilGraceDefaultsToCommon(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(BuildDaemonSet(daemonSetParams()).Spec.Template.Spec.TerminationGracePeriodSeconds).
		To(gomega.HaveValue(gomega.Equal(commonv1.DefaultTerminationGracePeriodSeconds)))

	p := daemonSetParams()
	p.TerminationGracePeriodSeconds = ptr.To(int64(90))

	g.Expect(BuildDaemonSet(p).Spec.Template.Spec.TerminationGracePeriodSeconds).
		To(gomega.HaveValue(gomega.Equal(int64(90))))
}

// The FSGroup that BuildWorkload stamps would apply to every host path a
// node-level pod mounts, so this builder stamps nothing and renders what the
// caller hands it, nil included.
func TestBuildDaemonSet_PodSecurityContextVerbatimNoFSGroup(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(BuildDaemonSet(daemonSetParams()).Spec.Template.Spec.SecurityContext).To(gomega.BeNil())

	psc := &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0)), SupplementalGroups: []int64{101}}
	p := daemonSetParams()
	p.PodSecurityContext = psc

	got := BuildDaemonSet(p).Spec.Template.Spec.SecurityContext
	g.Expect(got).To(gomega.BeIdenticalTo(psc))
	g.Expect(got.FSGroup).To(gomega.BeNil())
}

// The selector is immutable after creation, so it takes the narrow label set
// alone while the object and the template carry the full one. Stamping the
// full set into the selector would pin the managed-by and component labels for
// the lifetime of the DaemonSet.
func TestBuildDaemonSet_LabelsAndSelectorLabelsStamped(t *testing.T) {
	g := gomega.NewWithT(t)

	p := daemonSetParams()
	ds := BuildDaemonSet(p)

	g.Expect(ds.Labels).To(gomega.Equal(p.Labels))
	g.Expect(ds.Spec.Template.Labels).To(gomega.Equal(p.Labels))
	g.Expect(ds.Spec.Selector).NotTo(gomega.BeNil())
	g.Expect(ds.Spec.Selector.MatchLabels).To(gomega.Equal(p.SelectorLabels))
	g.Expect(ds.Spec.Selector.MatchLabels).NotTo(gomega.HaveKey("app.kubernetes.io/instance"))
}

// --- EnsureDaemonSet ---

func TestEnsureDaemonSet_ReadyOnlyWhenGenerationObservedAndCountsMatch(t *testing.T) {
	for name, tc := range map[string]struct {
		status appsv1.DaemonSetStatus
		ready  bool
	}{
		"stale generation": {
			status: appsv1.DaemonSetStatus{
				ObservedGeneration:     0,
				DesiredNumberScheduled: 3,
				UpdatedNumberScheduled: 3,
				NumberReady:            3,
			},
		},
		"a pod short of ready": {
			status: appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 3,
				UpdatedNumberScheduled: 3,
				NumberReady:            2,
			},
		},
		"an old-template pod left": {
			status: appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 3,
				UpdatedNumberScheduled: 2,
				NumberReady:            2,
			},
		},
		// Nothing selected is nothing to wait for: a node selector that matches
		// no node must not hold the owning CR unready forever.
		"no node selected": {
			status: appsv1.DaemonSetStatus{ObservedGeneration: 1},
			ready:  true,
		},
		"every node ready": {
			status: appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 3,
				UpdatedNumberScheduled: 3,
				NumberReady:            3,
			},
			ready: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			s := newScheme()
			owner := testOwner()
			live := BuildDaemonSet(daemonSetParams())
			live.Namespace = "default"
			live.Generation = 1
			live.Status = tc.status

			c := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(owner, live).
				WithStatusSubresource(live).
				Build()

			desired := BuildDaemonSet(daemonSetParams())
			desired.Namespace = "default"

			_, ready, err := EnsureDaemonSet(context.Background(), c, s, owner, desired)

			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(ready).To(gomega.Equal(tc.ready))
		})
	}
}

func TestEnsureDaemonSet_SetsControllerReference(t *testing.T) {
	g := gomega.NewWithT(t)

	s := newScheme()
	owner := testOwner()

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(owner).
		Build()

	p := daemonSetParams()
	p.Namespace = "default"

	// Readiness is not asserted here: the fake client leaves Generation at 0 on
	// an apply-create, where an API server would set 1, so what comes back is
	// the fake's answer rather than a cluster's. The table above pins the rule
	// against explicit generations instead.
	_, _, err := EnsureDaemonSet(context.Background(), c, s, owner, BuildDaemonSet(p))

	g.Expect(err).NotTo(gomega.HaveOccurred())

	created := &appsv1.DaemonSet{}
	g.Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-chassis"}, created)).To(gomega.Succeed())
	g.Expect(created.OwnerReferences).To(gomega.HaveLen(1))
	g.Expect(created.OwnerReferences[0].Name).To(gomega.Equal("test-owner"))
	g.Expect(created.OwnerReferences[0].Controller).To(gomega.HaveValue(gomega.BeTrue()))
}

// A kind the scheme does not know cannot be applied, and the failure has to
// reach the caller rather than be reported as "not ready yet".
func TestEnsureDaemonSet_PropagatesApplyFailure(t *testing.T) {
	g := gomega.NewWithT(t)

	s := newScheme()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-owner", Namespace: "default", UID: "test-uid"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()

	p := daemonSetParams()
	p.Namespace = "other-namespace"

	_, ready, err := EnsureDaemonSet(context.Background(), c, s, owner, BuildDaemonSet(p))

	g.Expect(err).To(gomega.HaveOccurred(), "a cross-namespace owner reference must fail before the apply")
	g.Expect(ready).To(gomega.BeFalse())
}
