// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/apply"
	"github.com/c5c3/cobaltcore/internal/common/conditions"
	commonmulticluster "github.com/c5c3/cobaltcore/internal/common/multicluster"
	"github.com/c5c3/cobaltcore/internal/common/naming"
	ovnv1alpha1 "github.com/c5c3/cobaltcore/operators/ovn/api/v1alpha1"
)

// conditionTypeNodesReady is the condition the node step reports under. It
// covers the rendering of the per-node values, not the state of the pods that
// consume them: a node whose entry is rendered is one the DaemonSets can
// configure, and whether they have is what the two DaemonSet conditions say.
const conditionTypeNodesReady = "NodesReady"

// The condition reasons of the node step.
const (
	conditionReasonNodesRendered   = "NodesRendered"
	conditionReasonNoMatchingNodes = "NoMatchingNodes"
	conditionReasonNodeListError   = "NodeListError"
	conditionReasonNodesError      = "NodesError"
)

// The component-label values of the two ConfigMaps this step applies.
const (
	componentNodes          = "nodes"
	componentChassisScripts = "scripts"
)

// The keys of the scripts ConfigMap. The containers name a script by path, and
// both the path and the ConfigMap key are derived from the same constant, so
// the file a container executes cannot drift from the key that carries it.
const (
	hostPrepareScriptKey = "host-prepare.sh"
	applyNodeScriptKey   = "apply-node.sh"
	runOVSDBScriptKey    = "run-ovsdb.sh"
	runVswitchdScriptKey = "run-vswitchd.sh"
	evacuateScriptKey    = "evacuate.sh"
	chassisDelScriptKey  = "chassis-del.sh"
)

// configHashLength is how many hex characters of the SHA-256 digest a node
// entry's hash keeps. Four bytes are plenty to tell one rendering of a node's
// values from another, and a short hash is what makes the value readable in
// status.nodes and in a Job name derived from it.
const configHashLength = 8

// hostPrepareScript runs once per pod in a privileged init container. It loads
// the two kernel modules the datapath needs, creates the runtime directories
// the unprivileged containers write to, and initialises the local Open vSwitch
// database when the node carries none yet.
//
// The run directories are group-writable, and gid 42424 owns them. ovsdb-server
// runs as that user and the two datapath daemons run as uid 0 with CAP_DAC_
// OVERRIDE dropped, so uid 0 gets no relief from the ordinary permission check:
// the group is what lets ovs-vswitchd and ovn-controller write their pid files
// and control sockets into a directory the database user owns. The default 0755
// leaves them unable to create a single file there.
//
// The database is created here rather than by ovsdb-server itself because the
// file has to end up owned by the unprivileged user the ovsdb-server container
// runs as, and only this container can chown it.
//
// Its lock file is handed over with it. ovsdb-tool takes an fcntl lock on
// .conf.db.~lock~ beside the database and leaves the file behind owned by root
// with mode 0600, and ovsdb-server locks the same file before it opens the
// database: without the second chown the open fails with EACCES, which OVS
// reports as "failed to lock lockfile", and the container never serves the
// socket. The chown is guarded because ovsdb-tool only creates the lock file
// when it creates or converts the database, so a node whose database is already
// current has none.
//
// An existing database is converted to the schema of the image that is now
// running, which is what upstream's ovs-ctl does before it starts the server.
// ovsRunDir is a host path that outlives the pod, so a node that has not
// rebooted since an image bump carries the previous schema: ovsdb-server would
// serve it happily while the new ovs-vswitchd and ovn-controller asked for
// columns it does not have, and every feature gated on one of them would
// silently not apply.
const hostPrepareScript = `#!/bin/bash
set -eu
modprobe openvswitch && modprobe geneve
install -d -m 0775 -o 42424 -g 42424 /run/openvswitch /run/ovn
schema=/usr/share/openvswitch/vswitch.ovsschema
[ -f /run/openvswitch/conf.db ] || ovsdb-tool create /run/openvswitch/conf.db "$schema"
if [ "$(ovsdb-tool needs-conversion /run/openvswitch/conf.db "$schema")" = yes ]; then
  ovsdb-tool convert /run/openvswitch/conf.db "$schema"
fi
chown 42424:42424 /run/openvswitch/conf.db
if [ -e /run/openvswitch/.conf.db.~lock~ ]; then
  chown 42424:42424 /run/openvswitch/.conf.db.~lock~
fi
`

// applyNodeScript writes this node's values into the local Open vSwitch
// database, which is where ovn-controller reads its own configuration from.
//
// It waits for its own entry rather than failing on a missing one: the kubelet
// propagates ConfigMap volume updates on its sync period, so a node whose entry
// is rendered after its pod started sees the file appear within that period,
// and a pod that had exited would only be restarted at the backoff's pace.
//
// The bridges named by the mappings are created here too. ovn-controller
// attaches patch ports to them but never creates them, so a mapping pointing at
// a bridge that does not exist silently drops every packet on that physical
// network.
//
// spec.bridgeMappings is optional, and an empty one is removed rather than set:
// ovs-vsctl rejects "external_ids:ovn-bridge-mappings=" with "argument does not
// end in \"=\" followed by a value", which under set -eu fails the whole init
// container. A chassis with no mappings is the ordinary case, so the key gets
// the same treatment ovn-cms-options already gets below, and a mapping list
// emptied in the spec is cleared from the node rather than left behind.
const applyNodeScript = `#!/bin/bash
set -eu
f="/etc/ovn-chassis/nodes/${NODE_NAME}"
until [ -f "$f" ]; do sleep 5; done
# shellcheck source=/dev/null
. "$f"
until ovs-vsctl --timeout=5 --no-wait show >/dev/null 2>&1; do sleep 2; done
ovs-vsctl --timeout=15 set open . \
  external_ids:system-id="${SYSTEM_ID}" external_ids:hostname="${NODE_NAME}" \
  external_ids:ovn-encap-type="${ENCAP_TYPE}" external_ids:ovn-encap-ip="${NODE_IP}" \
  external_ids:ovn-remote="${OVN_REMOTE}" external_ids:ovn-remote-probe-interval="${OVN_REMOTE_PROBE_INTERVAL_MS}"
if [ -n "${BRIDGE_MAPPINGS}" ]; then ovs-vsctl --timeout=15 set open . external_ids:ovn-bridge-mappings="${BRIDGE_MAPPINGS}"
else ovs-vsctl --timeout=15 remove open . external_ids ovn-bridge-mappings; fi
if [ "${GATEWAY}" = "true" ]; then ovs-vsctl --timeout=15 set open . external_ids:ovn-cms-options=enable-chassis-as-gw
else ovs-vsctl --timeout=15 remove open . external_ids ovn-cms-options; fi
for m in ${BRIDGE_MAPPINGS//,/ }; do ovs-vsctl --timeout=15 --may-exist add-br "${m#*:}"; done
`

// runOVSDBScript is the entrypoint of the ovsdb-server container. The daemon is
// wrapped in a script for one line: the umask decides the mode of the socket it
// creates, and Open vSwitch offers no option for it.
//
// ovsdb-server creates /run/openvswitch/db.sock at 0770 before the umask is
// applied, and it runs as the unprivileged user, so the default 0022 would clear
// the group's write bit and leave the socket 0750. Connecting to a Unix socket
// needs write permission on it, so ovs-vswitchd and ovn-controller beside it
// would be refused: they run as uid 0 with every capability dropped but the two
// they are named for, and CAP_DAC_OVERRIDE is not among them. The symptom is
// silent, because the loop that waits for this socket discards its own output.
const runOVSDBScript = `#!/bin/bash
set -eu
umask 002
exec ovsdb-server /run/openvswitch/conf.db \
  --remote=punix:/run/openvswitch/db.sock \
  --remote=db:Open_vSwitch,Open_vSwitch,manager_options \
  --pidfile=/run/openvswitch/ovsdb-server.pid \
  --unixctl=/run/openvswitch/ovsdb-server.ctl
`

// runVswitchdScript is the entrypoint of the ovs-vswitchd container. The daemon
// talks to ovsdb-server over the shared socket, so it waits for that socket to
// answer before it execs: started earlier it exits immediately, and the restart
// backoff would then hold the datapath down far longer than the wait costs.
const runVswitchdScript = `#!/bin/bash
set -eu
until ovs-vsctl --timeout=5 --no-wait show >/dev/null 2>&1; do sleep 1; done
ovs-vsctl --no-wait init
exec ovs-vswitchd unix:/run/openvswitch/db.sock --pidfile=/run/openvswitch/ovs-vswitchd.pid --unixctl=/run/openvswitch/ovs-vswitchd.ctl
`

// evacuateScript moves the gateway duties off the chassis named by CHASSIS. It
// runs against the Northbound database, because a gateway assignment is part of
// the logical model rather than of the chassis's own registration: dropping the
// chassis from every router port's gateway list and from every HA group is what
// makes the remaining gateways take the traffic over.
//
// It is idempotent, so a rerun after a partial pass costs nothing: --if-exists
// turns an already-removed assignment into a no-op.
//
// Each loop collects its rows first and then submits every removal as one
// ovn-nbctl invocation. A process per row would be a TLS handshake and a full
// Northbound schema download per row, and a region with a few thousand logical
// router ports needs longer for that than the maintenanceActiveDeadlineSeconds
// the Job is given, which is terminal: backoffLimit is 0, so the drain would
// never finish and no other node of the CR would get its maintenance run
// either.
const evacuateScript = `#!/bin/bash
set -eu
T="--db=${NB_ADDR} -p ` + ovnTLSDir + `/tls.key -c ` + ovnTLSDir + `/tls.crt -C ` + ovnTLSDir + `/ca.crt --timeout=30"
args=()
mapfile -t lrps < <(ovn-nbctl $T --bare --columns=name find Logical_Router_Port)
for lrp in "${lrps[@]}"; do args+=(-- --if-exists lrp-del-gateway-chassis "$lrp" "$CHASSIS"); done
mapfile -t grps < <(ovn-nbctl $T --bare --columns=name find HA_Chassis_Group)
for grp in "${grps[@]}"; do args+=(-- --if-exists ha-chassis-group-remove-chassis "$grp" "$CHASSIS"); done
if [ ${#args[@]} -gt 0 ]; then ovn-nbctl $T "${args[@]}"; fi
`

// chassisDelScript removes the chassis named by CHASSIS from the Southbound
// database. Until it runs, the registration of a node that has left keeps
// claiming the ports of the workloads that moved off it, and the remaining
// chassis keep building tunnels to an endpoint that no longer answers.
//
// It addresses the database rather than the relay: a relay forwards writes, but
// a deregistration has to be durable and is worth aiming at the source.
const chassisDelScript = `#!/bin/bash
set -eu
ovn-sbctl --db="${SB_ADDR}" -p ` + ovnTLSDir + `/tls.key -c ` + ovnTLSDir + `/tls.crt -C ` + ovnTLSDir + `/ca.crt --timeout=30 --if-exists chassis-del "$CHASSIS"
`

// nodeEntry is everything one node needs to configure itself. The chassis pod
// has no API client of its own (the OVN image ships no HTTP client), so the
// rendered form of this struct is the whole channel between the operator and a
// node.
type nodeEntry struct {
	systemID       string
	gateway        bool
	bridgeMappings string
	encapType      string
	leaving        bool
}

// renderedNodes maps a node name to the entry rendered for it. It is keyed by
// node name because that is also the ConfigMap key and the file name the pod on
// that node reads, which is what lets one ConfigMap serve every node.
type renderedNodes map[string]nodeEntry

// render produces the file the node sources. It is a shell fragment of KEY=value
// lines rather than a structured format, because the consumer is a shell script
// in a container that has no parser to spare.
//
// LEAVING is written only when it is true, so an ordinary entry carries no key
// whose absence and whose "false" value would have to mean the same thing.
func (e nodeEntry) render() string {
	out := "SYSTEM_ID=" + e.systemID + "\n" +
		"GATEWAY=" + strconv.FormatBool(e.gateway) + "\n" +
		"BRIDGE_MAPPINGS=" + e.bridgeMappings + "\n" +
		"ENCAP_TYPE=" + e.encapType + "\n"
	if e.leaving {
		out += "LEAVING=true\n"
	}
	return out
}

// hash digests the rendered entry. status.nodes carries the hash of what a node
// last applied, so comparing it against this one is what tells a node that is
// running the current values from one that has still to pick up a change.
func (e nodeEntry) hash() string {
	sum := sha256.Sum256([]byte(e.render()))
	return hex.EncodeToString(sum[:])[:configHashLength]
}

// systemIDPattern is the shape of a chassis identity: the UUID
// uuid.NewUUID() produces. Nothing else may reach the rendered entry, because
// applyNodeScript sources that file under bash, where a value is an assignment
// right-hand side and a "$(...)" in it runs as a command in the privileged
// init container of the node's ovn-controller pod.
var systemIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// parseNodeEntry reads a rendered entry back. It is what lets the live
// ConfigMap, rather than status alone, be the record of a node's system-id: a
// chassis identity has to survive a status the API server lost, because a new
// identity would leave the old registration behind in the Southbound database.
//
// Unknown keys and lines without a separator are ignored, so an entry a future
// operator version extended still parses here.
func parseNodeEntry(value string) nodeEntry {
	var entry nodeEntry
	for _, line := range strings.Split(value, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "SYSTEM_ID":
			entry.systemID = val
		case "GATEWAY":
			entry.gateway = val == "true"
		case "BRIDGE_MAPPINGS":
			entry.bridgeMappings = val
		case "ENCAP_TYPE":
			entry.encapType = val
		case "LEAVING":
			entry.leaving = val == "true"
		}
	}
	return entry
}

// reconcileNodes renders one entry per selected node into the nodes ConfigMap,
// applies the scripts the chassis containers run, and rebuilds status.nodes from
// what it rendered.
//
// A node that stops being selected, or that leaves the cluster, keeps its entry
// with LEAVING set instead of losing it. Its chassis registration outlives the
// node, and the record of the system-id to deregister has to outlive it too:
// the maintenance step drops both the key and the status entry once the
// chassis-deletion Job has succeeded.
func (r *OVNChassisReconciler) reconcileNodes(ctx context.Context, children client.Client, cr *ovnv1alpha1.OVNChassis) (renderedNodes, ctrl.Result, error) {
	// The nodes and the ConfigMap are read through the target cluster's own
	// uncached reader: the nodes are the ones the children land on, and a cache
	// that has not caught up would render a fresh system-id for a node whose
	// entry already carries one.
	reader := commonmulticluster.LiveReader(children)

	// The selector is applied by the API server rather than in renderNodeEntries:
	// nothing else reads the list, the webhook requires spec.nodeSelector to
	// carry at least one label, and an unfiltered LIST would serialize the whole
	// node inventory on every pass of every OVNChassis.
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels(cr.Spec.NodeSelector)); err != nil {
		err = fmt.Errorf("listing nodes: %w", err)
		chassisSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodeListError, err)
		return nil, ctrl.Result{}, err
	}

	live, err := readNodeEntries(ctx, reader, cr)
	if err != nil {
		chassisSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodesError, err)
		return nil, ctrl.Result{}, err
	}

	entries := renderNodeEntries(cr, nodes.Items, live)

	// Both ConfigMaps are applied even when nothing is selected. The empty nodes
	// ConfigMap is what the DaemonSet pods mount, and a volume whose ConfigMap
	// does not exist keeps a pod from starting at all.
	if err := r.ensureNodeConfigMaps(ctx, children, cr, entries); err != nil {
		chassisSkeleton.MarkFailed(cr, conditionTypeNodesReady, conditionReasonNodesError, err)
		return nil, ctrl.Result{}, err
	}

	cr.Status.Nodes = nodeStatuses(cr, entries)

	if len(entries) == 0 {
		conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
			Type:               conditionTypeNodesReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cr.Generation,
			Reason:             conditionReasonNoMatchingNodes,
			Message: fmt.Sprintf("No node matches spec.nodeSelector %s",
				labels.Set(cr.Spec.NodeSelector).String()),
		})
		return entries, ctrl.Result{}, nil
	}

	leaving := 0
	for _, entry := range entries {
		if entry.leaving {
			leaving++
		}
	}
	conditions.SetCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               conditionTypeNodesReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             conditionReasonNodesRendered,
		Message:            fmt.Sprintf("Rendered %d node entries (%d leaving)", len(entries), leaving),
	})
	return entries, ctrl.Result{}, nil
}

// readNodeEntries parses the entries the live nodes ConfigMap carries. A missing
// ConfigMap is the first pass of a chassis and yields no entries, which is not
// an error.
func readNodeEntries(ctx context.Context, reader client.Reader, cr *ovnv1alpha1.OVNChassis) (map[string]nodeEntry, error) {
	key := client.ObjectKey{Namespace: cr.Namespace, Name: chassisNodesName(cr)}

	var cm corev1.ConfigMap
	switch err := reader.Get(ctx, key, &cm); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the %s ConfigMap: %w", key.Name, err)
	}

	entries := make(map[string]nodeEntry, len(cm.Data))
	for name, value := range cm.Data {
		entries[name] = parseNodeEntry(value)
	}
	return entries, nil
}

// renderNodeEntries builds the entry of every node this CR is responsible for:
// the ones its selector matches, plus the ones it is still deregistering.
//
// live carries the entries the nodes ConfigMap holds now, which is where a
// node's system-id is kept.
func renderNodeEntries(cr *ovnv1alpha1.OVNChassis, nodes []corev1.Node, live map[string]nodeEntry) renderedNodes {
	selector := labels.SelectorFromSet(cr.Spec.NodeSelector)
	// A nil gateway selector matches nothing, which is what an OVNChassis
	// without spec.gateway describes. labels.Selector's own zero value matches
	// everything, so the nil check cannot be folded into the Matches call.
	var gateways labels.Selector
	if cr.Spec.Gateway != nil {
		gateways = labels.SelectorFromSet(cr.Spec.Gateway.NodeSelector)
	}
	mappings := renderBridgeMappings(cr.Spec.BridgeMappings)
	recorded := recordedNodes(cr)

	entries := make(renderedNodes, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		nodeLabels := labels.Set(node.Labels)
		if !selector.Matches(nodeLabels) {
			continue
		}
		entries[node.Name] = nodeEntry{
			systemID:       systemIDFor(node.Name, live, recorded),
			gateway:        gateways != nil && gateways.Matches(nodeLabels),
			bridgeMappings: mappings,
			encapType:      cr.Spec.EncapType,
		}
	}

	// A node that is no longer selected keeps its identity and its gateway role
	// on the way out, plus the LEAVING marker the maintenance step reads. The
	// mappings and the encapsulation are re-rendered from the spec rather than
	// carried over from the live entry, the way the status-recorded arm below
	// already does it: everything that ends up in the file the node sources has
	// to come from a source admission validated, and the live ConfigMap is not
	// one.
	for name, entry := range live {
		if _, selected := entries[name]; selected {
			continue
		}
		entries[name] = nodeEntry{
			systemID:       systemIDFor(name, live, recorded),
			gateway:        entry.gateway,
			bridgeMappings: mappings,
			encapType:      cr.Spec.EncapType,
			leaving:        true,
		}
	}

	// A node recorded in status but absent from the ConfigMap has either never
	// been rendered or has already been deregistered. The second case is what
	// status.nodes[].leaving distinguishes: the maintenance step clears the
	// status entry and the ConfigMap key together, so a leaving node missing
	// from the ConfigMap is one whose chassis-deletion Job succeeded.
	for _, node := range cr.Status.Nodes {
		if _, known := entries[node.Name]; known || node.Leaving {
			continue
		}
		entries[node.Name] = nodeEntry{
			systemID:       node.SystemID,
			gateway:        node.Gateway,
			bridgeMappings: mappings,
			encapType:      cr.Spec.EncapType,
			leaving:        true,
		}
	}
	return entries
}

// systemIDFor resolves the chassis identity of one node. The live ConfigMap wins
// over status, and status over a fresh identity: an identity that changes leaves
// the previous registration behind in the Southbound database, where it keeps
// claiming the ports of the workloads running on this very node.
//
// Both records are read back from objects a principal with update access to the
// namespace can edit, so a value that is not the UUID this function once wrote
// is discarded rather than rendered back out. Discarding it costs the node a
// new identity and one stale Southbound registration; keeping it would run
// whatever was substituted for it on the node.
func systemIDFor(name string, live map[string]nodeEntry, recorded map[string]ovnv1alpha1.OVNChassisNodeStatus) string {
	if id := live[name].systemID; systemIDPattern.MatchString(id) {
		return id
	}
	if id := recorded[name].SystemID; systemIDPattern.MatchString(id) {
		return id
	}
	return string(uuid.NewUUID())
}

// recordedNodes indexes status.nodes by node name.
func recordedNodes(cr *ovnv1alpha1.OVNChassis) map[string]ovnv1alpha1.OVNChassisNodeStatus {
	recorded := make(map[string]ovnv1alpha1.OVNChassisNodeStatus, len(cr.Status.Nodes))
	for _, node := range cr.Status.Nodes {
		recorded[node.Name] = node
	}
	return recorded
}

// nodeStatuses rebuilds status.nodes from what was rendered, sorted by name so
// a pass that changed nothing produces a byte-identical status and therefore no
// write.
//
// configHash is carried from the previous entry when it has one, because it
// records what a node has applied rather than what was rendered for it. A node
// seen for the first time is the exception: its first entry is applied by the
// init container before ovn-controller starts, so the hash counts as applied
// straight away. gatewayEvacuated is carried for the same reason, being the
// evacuation Job's finding rather than this step's.
func nodeStatuses(cr *ovnv1alpha1.OVNChassis, entries renderedNodes) []ovnv1alpha1.OVNChassisNodeStatus {
	recorded := recordedNodes(cr)

	statuses := make([]ovnv1alpha1.OVNChassisNodeStatus, 0, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		previous := recorded[name]
		configHash := previous.ConfigHash
		if configHash == "" {
			configHash = entry.hash()
		}
		statuses = append(statuses, ovnv1alpha1.OVNChassisNodeStatus{
			Name:             name,
			SystemID:         entry.systemID,
			Gateway:          entry.gateway,
			ConfigHash:       configHash,
			GatewayEvacuated: previous.GatewayEvacuated,
			Leaving:          entry.leaving,
		})
	}
	return statuses
}

// renderBridgeMappings renders spec.bridgeMappings into the comma-separated
// physnet:bridge list ovn-controller reads from ovn-bridge-mappings. Spec order
// is kept, because that is the order an operator reading the node's external_ids
// back expects to find.
func renderBridgeMappings(mappings []ovnv1alpha1.OVNBridgeMapping) string {
	rendered := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		rendered = append(rendered, mapping.PhysicalNetwork+":"+mapping.Bridge)
	}
	return strings.Join(rendered, ",")
}

// ensureNodeConfigMaps applies the two ConfigMaps the chassis pods mount.
func (r *OVNChassisReconciler) ensureNodeConfigMaps(ctx context.Context, children client.Client,
	cr *ovnv1alpha1.OVNChassis, entries renderedNodes,
) error {
	for _, cm := range []*corev1.ConfigMap{nodesConfigMap(cr, entries), chassisScriptsConfigMap(cr)} {
		if err := apply.EnsureObject(ctx, children, r.Scheme, cr, cm, apply.FieldManager); err != nil {
			return fmt.Errorf("ensuring %s ConfigMap: %w", cm.Name, err)
		}
	}
	return nil
}

// nodesConfigMap builds the ConfigMap the chassis pods mount, one key per node.
// One object serves every node of this CR: the pod on a node reads the file
// named after it and ignores the rest, and a ConfigMap per node would multiply
// the objects the operator applies by the size of the cluster.
func nodesConfigMap(cr *ovnv1alpha1.OVNChassis, entries renderedNodes) *corev1.ConfigMap {
	data := make(map[string]string, len(entries))
	for name, entry := range entries {
		data[name] = entry.render()
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chassisNodesName(cr),
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(chassisAppName, cr.Name, componentNodes),
		},
		Data: data,
	}
}

// chassisScriptsConfigMap builds the ConfigMap holding the scripts the chassis
// containers and the maintenance Jobs run. The Jobs share it with the pods
// rather than carrying their own copy, so the script that deregisters a chassis
// cannot drift from the one that registered it.
func chassisScriptsConfigMap(cr *ovnv1alpha1.OVNChassis) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chassisScriptsName(cr),
			Namespace: cr.Namespace,
			Labels:    naming.ComponentLabels(chassisAppName, cr.Name, componentChassisScripts),
		},
		Data: map[string]string{
			hostPrepareScriptKey: hostPrepareScript,
			applyNodeScriptKey:   applyNodeScript,
			runOVSDBScriptKey:    runOVSDBScript,
			runVswitchdScriptKey: runVswitchdScript,
			evacuateScriptKey:    evacuateScript,
			chassisDelScriptKey:  chassisDelScript,
		},
	}
}

// chassisNodesName names the ConfigMap carrying the per-node values.
func chassisNodesName(cr *ovnv1alpha1.OVNChassis) string {
	return cr.Name + "-" + componentNodes
}

// chassisScriptsName names the ConfigMap carrying the scripts. The suffix names
// the CR kind rather than only the component, so a chassis and a central of the
// same name keep separate scripts ConfigMaps in one namespace.
func chassisScriptsName(cr *ovnv1alpha1.OVNChassis) string {
	return cr.Name + "-chassis-" + componentChassisScripts
}
