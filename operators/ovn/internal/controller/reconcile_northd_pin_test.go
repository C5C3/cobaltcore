// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Byte-identity pin for the northd Deployment. The goldens below are
// FULL-OBJECT YAML captured from the builder as it stands today, so any refactor
// of the northd projection has to reproduce every rendered byte. The two
// database addresses reach the pods as command arguments, which means a change
// to how they are assembled restarts every northd pod in the fleet; the pin
// makes that visible in review instead of at rollout.
package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// pinCustomNorthdOVNCentral is the fixture behind the "custom" golden: every
// northd knob moved off its default at once, a digest-pinned image, and two
// three-member addresses, so a builder that reads the wrong field cannot hide
// behind a value that happens to match the default.
func pinCustomNorthdOVNCentral() *ovnv1alpha1.OVNCentral {
	cr := publishEndpoints(testOVNCentral())
	cr.Spec.Image = &commonv1.ImageSpec{
		Repository: "registry.example.com/ovn",
		Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	cr.Spec.Northd = ovnv1alpha1.OVNNorthdSpec{
		Threads: 4,
		Deployment: commonv1.DeploymentSpec{
			Replicas: 1,
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			},
		},
	}
	cr.Status.Northbound.InternalDbAddress = "ssl:10.96.0.11:6641,ssl:10.96.0.12:6641,ssl:10.96.0.13:6641"
	cr.Status.Southbound.InternalDbAddress = "ssl:10.96.0.21:6642,ssl:10.96.0.22:6642,ssl:10.96.0.23:6642"
	return cr
}

const pinNorthdDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: northd
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-northd
  namespace: openstack
spec:
  replicas: 3
  selector:
    matchLabels:
      app.kubernetes.io/component: northd
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: northd
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - ovn-northd
        - --ovnnb-db=ssl:10.96.0.11:6641
        - --ovnsb-db=ssl:10.96.0.21:6642
        - -p
        - /etc/ovn/tls/tls.key
        - -c
        - /etc/ovn/tls/tls.crt
        - -C
        - /etc/ovn/tls/ca.crt
        - --n-threads=1
        - --pidfile=/var/run/ovn/ovn-northd.pid
        - --unixctl=/var/run/ovn/ovn-northd.ctl
        image: ghcr.io/c5c3/ovn:26.03.2
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: northd
        readinessProbe:
          exec:
            command:
            - ovn-appctl
            - -t
            - /var/run/ovn/ovn-northd.ctl
            - status
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
          runAsGroup: 42424
          runAsNonRoot: true
          runAsUser: 42424
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: northd
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: northd
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-client
status: {}
`

const pinCustomNorthdDeploymentGolden = `metadata:
  labels:
    app.kubernetes.io/component: northd
    app.kubernetes.io/instance: ovn
    app.kubernetes.io/managed-by: ovncentral-operator
    app.kubernetes.io/name: ovncentral
  name: ovn-northd
  namespace: openstack
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: northd
      app.kubernetes.io/instance: ovn
      app.kubernetes.io/name: ovncentral
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/component: northd
        app.kubernetes.io/instance: ovn
        app.kubernetes.io/managed-by: ovncentral-operator
        app.kubernetes.io/name: ovncentral
    spec:
      containers:
      - command:
        - ovn-northd
        - --ovnnb-db=ssl:10.96.0.11:6641,ssl:10.96.0.12:6641,ssl:10.96.0.13:6641
        - --ovnsb-db=ssl:10.96.0.21:6642,ssl:10.96.0.22:6642,ssl:10.96.0.23:6642
        - -p
        - /etc/ovn/tls/tls.key
        - -c
        - /etc/ovn/tls/tls.crt
        - -C
        - /etc/ovn/tls/ca.crt
        - --n-threads=4
        - --pidfile=/var/run/ovn/ovn-northd.pid
        - --unixctl=/var/run/ovn/ovn-northd.ctl
        image: registry.example.com/ovn@sha256:1111111111111111111111111111111111111111111111111111111111111111
        lifecycle:
          preStop:
            exec:
              command:
              - /bin/sh
              - -c
              - sleep 5
        name: northd
        readinessProbe:
          exec:
            command:
            - ovn-appctl
            - -t
            - /var/run/ovn/ovn-northd.ctl
            - status
          initialDelaySeconds: 5
          periodSeconds: 5
          timeoutSeconds: 5
        resources:
          limits:
            memory: 2Gi
          requests:
            cpu: 200m
            memory: 512Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
          runAsGroup: 42424
          runAsNonRoot: true
          runAsUser: 42424
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /var/run/ovn
          name: run
        - mountPath: /var/log/ovn
          name: log
        - mountPath: /tmp
          name: tmp
        - mountPath: /etc/ovn/tls
          name: tls
          readOnly: true
      securityContext:
        fsGroup: 42424
      terminationGracePeriodSeconds: 30
      topologySpreadConstraints:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: northd
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: northd
            app.kubernetes.io/instance: ovn
            app.kubernetes.io/name: ovncentral
        maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
      volumes:
      - emptyDir: {}
        name: run
      - emptyDir: {}
        name: log
      - emptyDir: {}
        name: tmp
      - name: tls
        secret:
          secretName: ovn-client
status: {}
`

// TestPinNorthdDeployment pins the workload across a defaulted CR (three
// replicas, one compute thread, the operator's own image) and one that moves
// every knob at once.
func TestPinNorthdDeployment(t *testing.T) {
	cases := []struct {
		name   string
		cr     func() *ovnv1alpha1.OVNCentral
		golden string
	}{
		{
			name:   "default",
			cr:     func() *ovnv1alpha1.OVNCentral { return publishEndpoints(testOVNCentral()) },
			golden: pinNorthdDeploymentGolden,
		},
		{name: "custom", cr: pinCustomNorthdOVNCentral, golden: pinCustomNorthdDeploymentGolden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := yaml.Marshal(buildNorthdDeployment(tc.cr()))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(Equal(tc.golden),
				"the rendered northd Deployment must stay byte-identical")
		})
	}
}
