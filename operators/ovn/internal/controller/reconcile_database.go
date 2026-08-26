// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/deployment"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// centralAppName is the app.kubernetes.io/name label value carried by every
// child an OVNCentral owns. The two databases, northd, the relay and the backup
// are one control plane and share it; they are told apart by the component
// label.
const centralAppName = "ovncentral"

// The condition types the two database steps set. They differ only in the
// database they report on, so both steps run the same code and pick their type
// off the raftDB they were handed.
const (
	conditionTypeNorthboundReady = "NorthboundReady"
	conditionTypeSouthboundReady = "SouthboundReady"
)

// The condition reasons of a database step. Every failed apply reports
// StatefulSetError, whichever of the four objects failed: the step is one unit
// of work, and splitting the reason per object would put the object that
// happened to fail first into a field consumers match on.
const (
	conditionReasonStatefulSetReady       = "StatefulSetReady"
	conditionReasonStatefulSetProgressing = "StatefulSetProgressing"
	conditionReasonStatefulSetError       = "StatefulSetError"
)

// The name suffixes and component-label values of the two databases. OVN spells
// them this way in its own option names (--db-nb-..., run_nb_ovsdb), so the
// children carry the same spelling as the flags that configure them.
const (
	suffixNorthbound = "nb"
	suffixSouthbound = "sb"
)

// The in-pod paths of the database container.
const (
	// ovnDataDir holds the database file and its Raft log, backed by the
	// per-member PersistentVolumeClaim.
	ovnDataDir = "/var/lib/ovn"
	// ovnRunDir holds the Unix control socket ovn-nbctl and ovn-sbctl talk to.
	ovnRunDir = "/var/run/ovn"
	// ovnLogDir holds ovsdb-server's log file.
	ovnLogDir = "/var/log/ovn"
	// ovnTLSDir holds the server keypair and the CA certificate the Raft peers
	// and every client are authenticated against.
	ovnTLSDir = "/etc/ovn/tls"
	// centralScriptDir holds the scripts ConfigMap.
	centralScriptDir = "/etc/ovn-central/bin"
)

// The names of the pod volumes.
const (
	dataVolumeName    = "db"
	runVolumeName     = "run"
	logVolumeName     = "log"
	tmpVolumeName     = "tmp"
	tlsVolumeName     = "tls"
	scriptsVolumeName = "scripts"
)

// The container port names shared by the Services and the container. A Raft
// member serves clients on one port and its peers on the other.
const (
	clientPortName = "client"
	raftPortName   = "raft"
)

// defaultStorageSize mirrors the +kubebuilder:default on OVNStorageSpec.Size. It
// is resolved for a spec that carries no size at all, which is only reachable
// when the CRD default was bypassed; the alternative is a quantity parse that
// panics on the empty string.
const defaultStorageSize = "1Gi"

// raftDB carries everything that differs between the two OVN Raft databases, so
// one set of builders and one reconcile step serve both. The two are the same
// program (ovsdb-server, started through ovn-ctl) over a different schema, on a
// different pair of ports, driven by a different set of ovn-ctl options.
type raftDB struct {
	// suffix names the database in every child object name and in the component
	// label: "nb" or "sb".
	suffix string
	// schema is the OVSDB schema the database serves.
	schema string
	// clientPort is the port clients connect to, raftPort the one the members
	// replicate over.
	clientPort int32
	raftPort   int32
	// clusterPrefix and sslPrefix are the two ovn-ctl option prefixes of this
	// database ("--db-nb-" and "--ovn-nb-db-ssl-"), assembled into the full
	// option names by the run script.
	clusterPrefix string
	sslPrefix     string
	// runCmd is the ovn-ctl subcommand that starts this database in the
	// foreground.
	runCmd string
	// ctlTool is the client the post-start hook opens the client port with.
	ctlTool string
	// base is the first node port of this database's range; member i is
	// published on base+i.
	base int32
	// spec is the block of the CR this database is configured from.
	spec *ovnv1alpha1.OVNDatabaseSpec
	// conditionType is the condition the step reports this database under.
	conditionType string
}

// northboundDB describes the Northbound database, the one the CMS writes the
// logical network model into.
func northboundDB(cr *ovnv1alpha1.OVNCentral) raftDB {
	return raftDB{
		suffix:        suffixNorthbound,
		schema:        "OVN_Northbound",
		clientPort:    6641,
		raftPort:      6643,
		clusterPrefix: "--db-nb-",
		sslPrefix:     "--ovn-nb-db-ssl-",
		runCmd:        "run_nb_ovsdb",
		ctlTool:       "ovn-nbctl",
		base:          ptr.Deref(cr.Spec.Northbound.NodePortBase, ovnv1alpha1.DefaultNorthboundNodePortBase),
		spec:          &cr.Spec.Northbound,
		conditionType: conditionTypeNorthboundReady,
	}
}

// southboundDB describes the Southbound database, the one northd writes the
// translated flows into and every chassis reads from.
func southboundDB(cr *ovnv1alpha1.OVNCentral) raftDB {
	return raftDB{
		suffix:        suffixSouthbound,
		schema:        "OVN_Southbound",
		clientPort:    6642,
		raftPort:      6644,
		clusterPrefix: "--db-sb-",
		sslPrefix:     "--ovn-sb-db-ssl-",
		runCmd:        "run_sb_ovsdb",
		ctlTool:       "ovn-sbctl",
		base:          ptr.Deref(cr.Spec.Southbound.NodePortBase, ovnv1alpha1.DefaultSouthboundNodePortBase),
		spec:          &cr.Spec.Southbound,
		conditionType: conditionTypeSouthboundReady,
	}
}

// databaseStatus resolves the status block a database reports into.
func databaseStatus(cr *ovnv1alpha1.OVNCentral, db raftDB) *ovnv1alpha1.OVNDatabaseStatus {
	if db.suffix == suffixNorthbound {
		return &cr.Status.Northbound
	}
	return &cr.Status.Southbound
}

// reconcileNorthbound projects the Northbound Raft cluster.
func (r *OVNCentralReconciler) reconcileNorthbound(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	return r.reconcileRaftDatabase(ctx, children, cr, northboundDB(cr))
}

// reconcileSouthbound projects the Southbound Raft cluster.
func (r *OVNCentralReconciler) reconcileSouthbound(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral) (ctrl.Result, error) {
	return r.reconcileRaftDatabase(ctx, children, cr, southboundDB(cr))
}

// reconcileRaftDatabase projects one Raft cluster: the scripts its members run,
// the headless Service they find each other through, one Service per member, and
// the StatefulSet itself. It reports the cluster under the database's own
// condition type and mirrors the live ready-member count into status.
//
// The scripts ConfigMap is applied by both database steps rather than by a step
// of its own. It holds the scripts of both databases, the apply is idempotent,
// and a separate step would make each database depend on a step that neither of
// them owns.
func (r *OVNCentralReconciler) reconcileRaftDatabase(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNCentral, db raftDB) (ctrl.Result, error) {
	if err := apply.EnsureObject(ctx, children, r.Scheme, cr, centralScriptsConfigMap(cr), apply.FieldManager); err != nil {
		return ctrl.Result{}, markDatabaseFailed(cr, db, fmt.Errorf("ensuring the central scripts ConfigMap: %w", err))
	}

	if err := apply.EnsureObject(ctx, children, r.Scheme, cr, raftHeadlessService(cr, db), apply.FieldManager); err != nil {
		return ctrl.Result{}, markDatabaseFailed(cr, db, fmt.Errorf("ensuring %s headless Service: %w", db.suffix, err))
	}

	// One Service per member, before the StatefulSet: a member that comes up
	// before its Service has no address to publish, and the endpoint step would
	// hold the whole CR unready until the next pass created it.
	for ordinal := int32(0); ordinal < db.spec.Replicas; ordinal++ {
		svc := raftPerPodService(cr, db, ordinal)
		if err := apply.EnsureObject(ctx, children, r.Scheme, cr, svc, apply.FieldManager); err != nil {
			return ctrl.Result{}, markDatabaseFailed(cr, db,
				fmt.Errorf("ensuring %s per-pod Service %s: %w", db.suffix, svc.Name, err))
		}
	}

	sts := raftStatefulSet(cr, db)
	if err := apply.EnsureObject(ctx, children, r.Scheme, cr, sts, apply.FieldManager); err != nil {
		return ctrl.Result{}, markDatabaseFailed(cr, db, fmt.Errorf("ensuring %s StatefulSet: %w", db.suffix, err))
	}

	// Readiness is read from a Get after the apply, not from the applied object:
	// the counters it is judged by live on the status subresource, which the
	// apply strips from its request body, and the StatefulSet controller writes
	// them rather than the operator.
	live := &appsv1.StatefulSet{}
	if err := children.Get(ctx, client.ObjectKeyFromObject(sts), live); err != nil {
		return ctrl.Result{}, markDatabaseFailed(cr, db, fmt.Errorf("reading %s StatefulSet: %w", db.suffix, err))
	}
	databaseStatus(cr, db).ReadyReplicas = live.Status.ReadyReplicas

	// The observedGeneration comparison is what tells a converged cluster from
	// one whose counters still describe the template before the last apply.
	if live.Status.ReadyReplicas != db.spec.Replicas || live.Status.ObservedGeneration != live.Generation {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               db.conditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonStatefulSetProgressing,
			Message: fmt.Sprintf("%d of %d %s Raft members are ready",
				live.Status.ReadyReplicas, db.spec.Replicas, db.suffix),
		})
		return ctrl.Result{RequeueAfter: RequeueRaftWait}, nil
	}

	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               db.conditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonStatefulSetReady,
		Message:            fmt.Sprintf("All %d %s Raft members are ready", db.spec.Replicas, db.suffix),
	})
	return ctrl.Result{}, nil
}

// markDatabaseFailed flips the database's condition to False/StatefulSetError so
// a failed apply cannot leave the aggregate Ready condition stale-True at the
// new observedGeneration, and returns err for the caller to hand back. A target
// cluster that grants the operator no statefulsets verb lands here.
func markDatabaseFailed(cr *ovnv1alpha1.OVNCentral, db raftDB, err error) error {
	centralSkeleton.MarkFailed(cr, db.conditionType, conditionReasonStatefulSetError, err)
	return err
}

// centralScriptsConfigMap builds the ConfigMap holding the scripts the database
// pods run. It carries both databases' scripts, so the two database steps apply
// the same object.
func centralScriptsConfigMap(cr *ovnv1alpha1.OVNCentral) *corev1.ConfigMap {
	nb, sb := northboundDB(cr), southboundDB(cr)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      centralScriptsName(cr),
			Namespace: cr.Namespace,
			Labels:    naming.CommonLabels(centralAppName, cr.Name),
		},
		Data: map[string]string{
			runScriptKey(nb):           runScript(cr, nb),
			runScriptKey(sb):           runScript(cr, sb),
			setConnectionScriptKey(nb): setConnectionScript(nb),
			setConnectionScriptKey(sb): setConnectionScript(sb),
		},
	}
}

// runScript is the entrypoint of a database container. It assembles the ovn-ctl
// options that place this pod in the Raft cluster and hands over to ovn-ctl,
// which execs ovsdb-server.
//
// Member 0 creates the cluster, every other member joins it through member 0's
// stable DNS name, which is what the remote-addr option carries. The election
// timer is only read when the cluster is created, which is why the field is
// CEL-immutable: a later change reaches the option but not the database.
//
// There is no --db-<db>-create-insecure-remote: the database has to be
// unreachable until the post-start hook has installed the TLS connection row.
func runScript(cr *ovnv1alpha1.OVNCentral, db raftDB) string {
	return fmt.Sprintf(`#!/bin/bash
set -eu
FQDN="$(hostname -f)"
ORD="${HOSTNAME##*-}"
ARGS="%[1]scluster-local-addr=${FQDN} %[1]scluster-local-proto=ssl %[2]skey=%[5]s/tls.key %[2]scert=%[5]s/tls.crt %[2]sca-cert=%[5]s/ca.crt %[1]selection-timer=${ELECTION_TIMER_MS}"
if [ "$ORD" != 0 ]; then
  ARGS="$ARGS %[1]scluster-remote-addr=%[3]s-0.%[3]s.${POD_NAMESPACE}.svc.cluster.local %[1]scluster-remote-proto=ssl"
fi
exec /usr/share/ovn/scripts/ovn-ctl $ARGS %[4]s
`, db.clusterPrefix, db.sslPrefix, raftName(cr, db), db.runCmd, ovnTLSDir)
}

// setConnectionScript is the post-start hook of a database container. ovn-ctl
// starts ovsdb-server with --remote=db:<schema>,<Global>,connections, so nothing
// listens on the client port until the connections row exists: this script is
// what creates it, on TLS and with the configured inactivity probe.
//
// It retries for two minutes because the socket it talks to appears only once
// ovsdb-server is up, and on a fresh cluster that waits for the first election.
// Failing after that leaves the pod running with a closed client port, which the
// readiness probe reports.
func setConnectionScript(db raftDB) string {
	return fmt.Sprintf(`#!/bin/bash
for i in $(seq 1 120); do
  if %[1]s --no-leader-only --timeout=5 --db=unix:%[2]s/ovn%[3]s_db.sock set-connection pssl:%[4]d:0.0.0.0 -- set connection . inactivity_probe=${INACTIVITY_PROBE_MS}; then
    exit 0
  fi
  sleep 1
done
exit 1
`, db.ctlTool, ovnRunDir, db.suffix, db.clientPort)
}

// runScriptKey and setConnectionScriptKey name a script in the ConfigMap. The
// container command and the ConfigMap data are derived from the same helper, so
// the file the container executes cannot drift from the key that carries it.
func runScriptKey(db raftDB) string {
	return "run-" + db.suffix + ".sh"
}

func setConnectionScriptKey(db raftDB) string {
	return "set-connection-" + db.suffix + ".sh"
}

// centralScriptsName names the scripts ConfigMap shared by both databases.
func centralScriptsName(cr *ovnv1alpha1.OVNCentral) string {
	return cr.Name + "-central-scripts"
}

// raftName names the StatefulSet of a database and the headless Service that
// gives its members their stable DNS names. They share one name because the
// StatefulSet derives the per-pod names from its serviceName.
func raftName(cr *ovnv1alpha1.OVNCentral, db raftDB) string {
	return cr.Name + "-" + db.suffix
}

// raftMemberName names one Raft member: the pod, and the Service publishing it.
func raftMemberName(cr *ovnv1alpha1.OVNCentral, db raftDB, ordinal int32) string {
	return fmt.Sprintf("%s-%d", raftName(cr, db), ordinal)
}

// raftServerSecretName names the Secret cert-manager writes this database's
// server keypair into.
func raftServerSecretName(cr *ovnv1alpha1.OVNCentral, db raftDB) string {
	return raftName(cr, db) + "-server"
}

// raftSelectorLabels is the pod selector of a database: the shared selector
// labels narrowed by the component, so the two databases of one OVNCentral
// select their own members and not each other's.
func raftSelectorLabels(cr *ovnv1alpha1.OVNCentral, db raftDB) map[string]string {
	labels := naming.SelectorLabels(centralAppName, cr.Name)
	labels[naming.LabelKeyComponent] = db.suffix
	return labels
}

// raftHeadlessService gives every member a stable DNS name to reach its peers
// under. It publishes the addresses of members that are not ready yet: a member
// becomes ready by joining the cluster, and it can only join through peers it
// can resolve, so a Service that waited for readiness would never let the
// cluster form.
func raftHeadlessService(cr *ovnv1alpha1.OVNCentral, db raftDB) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      raftName(cr, db),
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(centralAppName, cr.Name, db.suffix),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 raftSelectorLabels(cr, db),
			Ports: []corev1.ServicePort{
				{Name: clientPortName, Port: db.clientPort, TargetPort: intstr.FromInt32(db.clientPort)},
				{Name: raftPortName, Port: db.raftPort, TargetPort: intstr.FromInt32(db.raftPort)},
			},
		},
	}
}

// raftPerPodService gives one member an address of its own. A Raft client
// addresses the members individually, because it has to reach the leader and any
// member can be it. A single load-balanced Service would send half the writes to
// a follower that cannot serve them.
//
// It is a ClusterIP unless spec.<db>.externallyReachable asks for a node port.
// The database holds the whole logical network model, so publishing it on every
// node IP of the cluster is a posture an operator opts into for the one client
// that needs it — an OVNChassis on a node without cluster networking, dialling
// the Southbound database — rather than the default for both databases.
func raftPerPodService(cr *ovnv1alpha1.OVNCentral, db raftDB, ordinal int32) *corev1.Service {
	port := corev1.ServicePort{
		Name:       clientPortName,
		Port:       db.clientPort,
		TargetPort: intstr.FromInt32(db.clientPort),
	}
	serviceType := corev1.ServiceTypeClusterIP
	if db.spec.ExternallyReachable {
		serviceType = corev1.ServiceTypeNodePort
		port.NodePort = db.base + ordinal
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      raftMemberName(cr, db, ordinal),
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(centralAppName, cr.Name, db.suffix),
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
			// The pod-name label is what pins the Service to one member; the
			// StatefulSet controller stamps it on every pod it creates.
			Selector: map[string]string{appsv1.StatefulSetPodNameLabel: raftMemberName(cr, db, ordinal)},
			Ports:    []corev1.ServicePort{port},
		},
	}
}

// raftStatefulSet builds the StatefulSet running one Raft cluster.
//
// The members come up in parallel: a rolling, ordinal-by-ordinal start would
// deadlock a fresh cluster, where member 0 waits for the quorum the later
// members are what forms. The claims survive a scale-down (whenScaled: Retain)
// so a member that comes back rejoins with its Raft log instead of resyncing the
// whole database, and are deleted with the CR (whenDeleted: Delete) so a
// deleted control plane leaves no volumes behind.
func raftStatefulSet(cr *ovnv1alpha1.OVNCentral, db raftDB) *appsv1.StatefulSet {
	name := raftName(cr, db)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(centralAppName, cr.Name, db.suffix),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         name,
			Replicas:            ptr.To(db.spec.Replicas),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: raftSelectorLabels(cr, db)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: naming.ComponentLabels(centralAppName, cr.Name, db.suffix),
				},
				Spec: corev1.PodSpec{
					// Five minutes of grace: a member that is shutting down should
					// hand over leadership and finish the snapshot it may be writing,
					// and a SIGKILL in the middle of that costs the next start a full
					// database resync from its peers.
					TerminationGracePeriodSeconds: ptr.To(int64(300)),
					// The pod-level context pins the user the database files are
					// written as and the group the volume is handed over with. ovn-ctl
					// logs one "chown: changing ownership of '/var/lib/ovn': Operation
					// not permitted" line at start, which is expected: fsGroup has
					// already put the volume in the right group, and the container may
					// not chown it again.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(deployment.OpenStackUID),
						RunAsGroup:   ptr.To(deployment.OpenStackUID),
						FSGroup:      ptr.To(deployment.OpenStackUID),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{ovsdbContainer(cr, db)},
					Volumes:    raftVolumes(cr, db),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{raftDataClaim(db)},
		},
	}
}

// ovsdbContainer builds the one container of a database pod.
func ovsdbContainer(cr *ovnv1alpha1.OVNCentral, db raftDB) corev1.Container {
	resources := corev1.ResourceRequirements{}
	if db.spec.Resources != nil {
		resources = *db.spec.Resources
	}

	return corev1.Container{
		Name:      "ovsdb",
		Image:     effectiveImage(cr.Spec.Image).Reference(),
		Command:   []string{"/bin/bash", "-c", "exec " + path.Join(centralScriptDir, runScriptKey(db))},
		Resources: resources,
		Env: []corev1.EnvVar{
			{Name: "OVN_DBDIR", Value: ovnDataDir},
			{Name: "ELECTION_TIMER_MS", Value: strconv.Itoa(int(db.spec.ElectionTimerMs))},
			{Name: "INACTIVITY_PROBE_MS", Value: strconv.Itoa(int(db.spec.InactivityProbeMs))},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			}},
		},
		Ports: []corev1.ContainerPort{
			{Name: clientPortName, ContainerPort: db.clientPort},
			{Name: raftPortName, ContainerPort: db.raftPort},
		},
		// The readiness probe opens the client port the way a client would, so a
		// member whose post-start hook has not installed the connection row yet
		// stays out of the headless Service's ready addresses.
		//
		// There is deliberately no liveness probe. A member that lost quorum still
		// answers on its client port and still holds the Raft log the cluster
		// needs back, so a liveness probe would either not notice it or restart
		// the one member whose data is the way out of the outage.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{
					"ovsdb-client",
					"-p", path.Join(ovnTLSDir, "tls.key"),
					"-c", path.Join(ovnTLSDir, "tls.crt"),
					"-C", path.Join(ovnTLSDir, "ca.crt"),
					"list-dbs",
					fmt.Sprintf("ssl:127.0.0.1:%d", db.clientPort),
				}},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
		},
		Lifecycle: &corev1.Lifecycle{
			PostStart: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{path.Join(centralScriptDir, setConnectionScriptKey(db))},
				},
			},
		},
		SecurityContext: deployment.RestrictedSecurityContext(),
		VolumeMounts:    raftVolumeMounts(),
	}
}

// raftVolumes builds the pod volumes. Everything the container writes outside
// its database volume goes to an emptyDir, because the root filesystem is
// read-only.
func raftVolumes(cr *ovnv1alpha1.OVNCentral, db raftDB) []corev1.Volume {
	return []corev1.Volume{
		{Name: runVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: logVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: tmpVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: raftServerSecretName(cr, db)},
		}},
		// 0555 rather than the default 0644: the container executes these files,
		// and a ConfigMap volume carries no executable bit unless it is asked for.
		{Name: scriptsVolumeName, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: centralScriptsName(cr)},
				DefaultMode:          ptr.To(int32(0o555)),
			},
		}},
	}
}

// raftVolumeMounts builds the container mounts. They are the same for both
// databases: what differs between them is inside the files, not where they sit.
func raftVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: dataVolumeName, MountPath: ovnDataDir},
		{Name: runVolumeName, MountPath: ovnRunDir},
		{Name: logVolumeName, MountPath: ovnLogDir},
		{Name: tmpVolumeName, MountPath: "/tmp"},
		{Name: tlsVolumeName, MountPath: ovnTLSDir, ReadOnly: true},
		{Name: scriptsVolumeName, MountPath: centralScriptDir},
	}
}

// raftDataClaim builds the per-member volume claim template holding the database
// file and its Raft log. The storage class is only set when the CR names one, so
// a CR that names none takes the cluster's default class rather than pinning the
// empty string.
func raftDataClaim(db raftDB) corev1.PersistentVolumeClaim {
	size := cmp.Or(db.spec.Storage.Size, defaultStorageSize)

	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if db.spec.Storage.StorageClassName != nil {
		claim.Spec.StorageClassName = db.spec.Storage.StorageClassName
	}
	return claim
}
