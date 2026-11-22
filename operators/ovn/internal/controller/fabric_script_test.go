// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// fabricScriptStubs stand in for ip, ovs-vsctl and sleep while the fabric node
// script runs. Shell functions shadow the PATH lookup, so the script reaches
// them with its arguments intact and nothing has to be laid down on a node.
//
// The stubs keep a small model of the node under FAKE: a file per link under
// links/ whose content is its kind ("nic", "bridge" or "vrf <table>"), the
// link's attributes and addresses under attr/<link>/, the OVS bridges under
// ovs/br/<bridge>/, each port as ovs/br/<bridge>/port-<name> whose content is
// "marked" once external_ids:c5c3-fabric is set on it, and the external_ids of
// the Open_vSwitch row under ovs/open/<key>. A link is created with ln, which
// fails when the name is taken, so two runs of the script racing for one device
// see the File exists answer the kernel gives. add-br creates the bridge's
// netdev with its accept_ra and disable_ipv6 files unless NO_BRIDGE_NETDEV is
// set, add-port refuses a port another bridge holds as ovs-vsctl does, and ip
// link add answers as a kernel without the vrf module does when NO_VRF_MODULE
// is set.
//
// Every call is recorded to CALL_LOG, and ip link set ... up also records the
// device's accept_ra and disable_ipv6 at that moment, which is what the
// ordering assertions read.
const fabricScriptStubs = `log() { printf '%s\n' "$*" >> "$CALL_LOG"; }
sleep() { log "sleep $*"; }
mklink() {
  local tmp="$FAKE/tmp.$$.$RANDOM"
  printf '%s\n' "$2" > "$tmp"
  if ln "$tmp" "$FAKE/links/$1" 2>/dev/null; then rm -f "$tmp"; mkdir -p "$FAKE/attr/$1"; return 0; fi
  rm -f "$tmp"; return 1
}
rmlink() { rm -f "$FAKE/links/$1"; rm -rf "$FAKE/attr/$1" "$FAKE/proc/net/ipv6/conf/$1"; }
setattr() {
  local tmp="$FAKE/tmp.$$.$RANDOM"
  mkdir -p "$FAKE/attr/$1"
  printf '%s\n' "$3" > "$tmp"; mv "$tmp" "$FAKE/attr/$1/$2"
}
setaddrs() {
  local tmp="$FAKE/tmp.$$.$RANDOM"
  mkdir -p "$FAKE/attr/$1"
  awk -v c="$2" '$1 != c' "$FAKE/attr/$1/addrs" > "$tmp" 2>/dev/null || true
  if [ -n "$3" ]; then printf '%s %s\n' "$2" "$3" >> "$tmp"; fi
  mv "$tmp" "$FAKE/attr/$1/addrs"
}
nodev() { echo "Cannot find device \"$1\"" >&2; return 1; }
ip() {
  log "ip $*"
  local detail=""
  while :; do case "$1" in -6|-o) shift ;; -d) detail=1; shift ;; *) break ;; esac; done
  local obj="$1" verb="$2"; shift 2
  case "$obj $verb" in
  "link show")
    if [ ! -f "$FAKE/links/$2" ]; then nodev "$2"; return 1; fi
    local kind; kind="$(cat "$FAKE/links/$2")"
    printf '7: %s: <BROADCAST,MULTICAST> mtu 1500\n' "$2"
    if [ -n "$detail" ] && [ "${kind%% *}" = vrf ]; then printf '    vrf table %s addrgenmode eui64\n' "${kind#vrf }"; fi ;;
  "link add")
    if [ -n "${NO_VRF_MODULE:-}" ]; then echo "Error: Unknown device type." >&2; return 2; fi
    if ! mklink "$1" "vrf $5"; then echo "RTNETLINK answers: File exists" >&2; return 2; fi ;;
  "link del")
    if [ ! -f "$FAKE/links/$1" ]; then nodev "$1"; return 1; fi
    rmlink "$1" ;;
  "link set")
    if [ ! -f "$FAKE/links/$1" ]; then nodev "$1"; return 1; fi
    case "$2" in
    up) log "state: $1 up with accept_ra=$(cat "$FAKE/proc/net/ipv6/conf/$1/accept_ra" 2>/dev/null || true)"
        log "state: $1 up with disable_ipv6=$(cat "$FAKE/proc/net/ipv6/conf/$1/disable_ipv6" 2>/dev/null || true)"
        setattr "$1" up true ;;
    *) setattr "$1" "$2" "$3" ;;
    esac ;;
  "addr show")
    if [ ! -f "$FAKE/links/$2" ]; then nodev "$2"; return 1; fi
    if [ -f "$FAKE/attr/$2/addrs" ]; then
      local cidr scope
      while read -r cidr scope; do
        if [ "$scope" = global ]; then printf '7: %s    inet %s scope global %s\n' "$2" "$cidr" "$2"; fi
      done < "$FAKE/attr/$2/addrs"
    fi ;;
  "addr replace")
    if [ ! -f "$FAKE/links/$3" ]; then nodev "$3"; return 1; fi
    setaddrs "$3" "$1" global ;;
  "addr del")
    if [ ! -f "$FAKE/links/$3" ]; then nodev "$3"; return 1; fi
    setaddrs "$3" "$1" "" ;;
  *) echo "stub: unknown ip call: $obj $verb $*" >&2; return 1 ;;
  esac
  return 0
}
ovs_cmd() {
  case "$1" in
  show) ;;
  find)
    local br port
    for br in "$FAKE"/ovs/br/*; do
      case "$2" in
      bridge) if [ -e "$br/marked" ]; then printf '%s\n\n' "${br##*/}"; fi ;;
      port)
        for port in "$br"/port-*; do
          if [ "$(cat "$port" 2>/dev/null)" = marked ]; then printf '%s\n\n' "${port##*/port-}"; fi
        done ;;
      esac
    done ;;
  br-exists) [ -d "$FAKE/ovs/br/$2" ] || return 2 ;;
  port-to-br)
    local br
    for br in "$FAKE"/ovs/br/*; do
      if [ -e "$br/port-$2" ]; then printf '%s\n' "${br##*/}"; return 0; fi
    done
    echo "ovs-vsctl: no port named $2" >&2; return 1 ;;
  del-br)
    if [ -d "$FAKE/ovs/br/$2" ]; then rm -rf "$FAKE/ovs/br/$2"; rmlink "$2"; fi ;;
  del-port) rm -f "$FAKE"/ovs/br/*/"port-${3:-$2}" ;;
  get)
    local key="${4#external_ids:}"
    if [ -f "$FAKE/ovs/open/$key" ]; then printf '"%s"\n' "$(cat "$FAKE/ovs/open/$key")"; fi ;;
  remove) rm -f "$FAKE/ovs/open/$5" ;;
  set)
    case "$2" in
    open)
      local kv="${4#external_ids:}" tmp="$FAKE/tmp.$$.$RANDOM"
      printf '%s\n' "${kv#*=}" > "$tmp"; mv "$tmp" "$FAKE/ovs/open/${kv%%=*}" ;;
    bridge) touch "$FAKE/ovs/br/$3/marked" ;;
    port) local port; for port in "$FAKE"/ovs/br/*/"port-$3"; do if [ -e "$port" ]; then echo marked > "$port"; fi; done ;;
    interface) printf '%s\n' "${4#mtu_request=}" > "$FAKE/ovs/br/$3/mtu_request" ;;
    esac ;;
  add-br)
    mkdir -p "$FAKE/ovs/br/$2"
    if [ -z "${NO_BRIDGE_NETDEV:-}" ]; then
      mklink "$2" bridge || true
      mkdir -p "$FAKE/proc/net/ipv6/conf/$2"
      if [ ! -f "$FAKE/proc/net/ipv6/conf/$2/accept_ra" ]; then echo 1 > "$FAKE/proc/net/ipv6/conf/$2/accept_ra"; fi
      if [ ! -f "$FAKE/proc/net/ipv6/conf/$2/disable_ipv6" ]; then echo 0 > "$FAKE/proc/net/ipv6/conf/$2/disable_ipv6"; fi
    fi ;;
  add-port)
    local br
    for br in "$FAKE"/ovs/br/*; do
      if [ "${br##*/}" != "$2" ] && [ -e "$br/port-$3" ]; then
        echo "ovs-vsctl: \"--may-exist add-port $2 $3\" but $3 is actually attached to bridge ${br##*/}" >&2; return 1
      fi
    done
    touch "$FAKE/ovs/br/$2/port-$3" ;;
  *) echo "stub: unknown ovs-vsctl command: $*" >&2; return 1 ;;
  esac
  return 0
}
ovs-vsctl() {
  log "ovs-vsctl $*"
  local a cmd=""
  for a in "$@" --; do
    if [ "$a" = "--" ]; then
      if [ -n "$cmd" ]; then ovs_cmd $cmd || return 1; fi
      cmd=""; continue
    fi
    case "$a" in --*) ;; *) cmd="$cmd $a" ;; esac
  done
  return 0
}
`

// fabricScriptRun defines the stubs and sources the script the rig wrote to
// FABRIC_SCRIPT, which is fabricNodeScript with its two node paths pointed at
// the rig. The script's own exit ends the run.
const fabricScriptRun = fabricScriptStubs + `. "$FABRIC_SCRIPT"
`

// fabricTestNode is the NODE_NAME every fabric script run is handed.
const fabricTestNode = "node-a"

// fabricRig is one model node the fabric script runs against.
type fabricRig struct {
	dir    string
	script string // the path of the rewritten script
}

// The modes of the files and directories a rig lays down.
const (
	fabricRigDirMode  = 0o750
	fabricRigFileMode = 0o600
)

// newFabricRig lays down a node with the two uplinks fab1 and fab2 (link-local
// addresses only, accept_ra 1), the node network's eth0 with a global address,
// and an OVS bridge br-ex the script did not create and must leave alone.
func newFabricRig(t *testing.T) *fabricRig {
	t.Helper()
	g := NewGomegaWithT(t)

	if _, err := exec.LookPath(bashPath); err != nil {
		t.Skipf("%s is unavailable; the fabric node script cannot be exercised here: %v", bashPath, err)
	}
	probe := exec.CommandContext(t.Context(), bashPath, "-c", `[ "${BASH_VERSINFO[0]}" -ge 4 ]`)
	if err := probe.Run(); err != nil {
		t.Skipf("%s is older than bash 4; the fabric node script cannot be exercised here", bashPath)
	}

	rig := &fabricRig{dir: t.TempDir()}
	for _, d := range []string{"links", "attr", "ovs/br/br-ex", "ovs/open", "nodes", "proc/net/ipv6/conf"} {
		g.Expect(os.MkdirAll(filepath.Join(rig.dir, d), fabricRigDirMode)).To(Succeed())
	}
	rig.addNIC(t, "fab1", "fe80::1/64 link")
	rig.addNIC(t, "fab2", "fe80::2/64 link")
	rig.addNIC(t, "eth0", "10.0.0.5/24 global")

	// The script names its two node paths literally. They are pointed at the
	// rig, and the count guards against a rewrite that silently matched nothing.
	script := fabricNodeScript
	for from, to := range map[string]string{
		"/etc/ovn-chassis/nodes/":  filepath.Join(rig.dir, "nodes") + "/",
		"/proc/sys/net/ipv6/conf/": filepath.Join(rig.dir, "proc/net/ipv6/conf") + "/",
	} {
		g.Expect(strings.Count(script, from)).To(BeNumerically(">", 0), from)
		script = strings.ReplaceAll(script, from, to)
	}
	rig.script = filepath.Join(t.TempDir(), "fabric-node.sh")
	g.Expect(os.WriteFile(rig.script, []byte(script), fabricRigFileMode)).To(Succeed())
	return rig
}

// addNIC lays down a NIC with one address, accept_ra 1 and IPv6 on.
func (r *fabricRig) addNIC(t *testing.T, name, addr string) {
	t.Helper()
	g := NewGomegaWithT(t)
	g.Expect(os.WriteFile(filepath.Join(r.dir, "links", name), []byte("nic\n"), fabricRigFileMode)).To(Succeed())
	g.Expect(os.MkdirAll(filepath.Join(r.dir, "attr", name), fabricRigDirMode)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(r.dir, "attr", name, "addrs"), []byte(addr+"\n"),
		fabricRigFileMode)).To(Succeed())
	conf := filepath.Join(r.dir, "proc/net/ipv6/conf", name)
	g.Expect(os.MkdirAll(conf, fabricRigDirMode)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(conf, "accept_ra"), []byte("1\n"), fabricRigFileMode)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(conf, "disable_ipv6"), []byte("0\n"), fabricRigFileMode)).To(Succeed())
}

// writeEntry writes the node's entry as the nodes ConfigMap would carry it.
func (r *fabricRig) writeEntry(t *testing.T, fabric *nodeFabric) {
	t.Helper()
	entry := nodeEntry{systemID: testFixedSystemID, encapType: "geneve", fabric: fabric}
	NewGomegaWithT(t).Expect(os.WriteFile(filepath.Join(r.dir, "nodes", fabricTestNode),
		[]byte(entry.render()), fabricRigFileMode)).To(Succeed())
}

// command builds one run of the script against the rig.
func (r *fabricRig) command(t *testing.T, callLog string, env ...string) *exec.Cmd {
	cmd := exec.CommandContext(t.Context(), bashPath, "-c", fabricScriptRun)
	cmd.Env = append(os.Environ(), "NODE_NAME="+fabricTestNode, "FAKE="+r.dir, "CALL_LOG="+callLog,
		"FABRIC_SCRIPT="+r.script)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// run runs the script once and returns its exit code, its stderr and the calls
// it made. A non-zero exit is an expected outcome here, not a test failure.
func (r *fabricRig) run(t *testing.T, env ...string) (int, string, string) {
	t.Helper()
	callLog := filepath.Join(t.TempDir(), "calls")
	cmd := r.command(t, callLog, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	return exitCode(t, cmd.Run()), stderr.String(), readCalls(t, callLog)
}

// exitCode turns the error of a finished script into its exit code.
func exitCode(t *testing.T, runErr error) int {
	t.Helper()
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	NewGomegaWithT(t).Expect(errors.As(runErr, &exitErr)).To(BeTrue(),
		"the script must fail by exiting, not by failing to start: %v", runErr)
	return exitErr.ExitCode()
}

// readCalls reads a call log, which a run that never called a stub leaves
// absent.
func readCalls(t *testing.T, callLog string) string {
	t.Helper()
	calls, err := os.ReadFile(filepath.Clean(callLog))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	NewGomegaWithT(t).Expect(err).NotTo(HaveOccurred())
	return string(calls)
}

// state reads the whole model node back, keyed by path below the rig. Two
// states compare equal when no run in between changed anything on the node.
func (r *fabricRig) state(t *testing.T) map[string]string {
	t.Helper()
	g := NewGomegaWithT(t)
	state := map[string]string{}
	node := os.DirFS(r.dir)
	err := fs.WalkDir(node, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := fs.ReadFile(node, path)
		state[path] = string(content)
		return err
	})
	g.Expect(err).NotTo(HaveOccurred())
	return state
}

// read returns one file of the model node, or "" when it is absent.
func (r *fabricRig) read(rel string) string {
	content, err := os.ReadFile(filepath.Clean(filepath.Join(r.dir, rel)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

// exists reports whether one path of the model node exists.
func (r *fabricRig) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.dir, rel))
	return err == nil
}

// writes returns the calls of a run that change the node: every ip and
// ovs-vsctl call that is not a show, find, get, br-exists or port-to-br.
func writes(calls string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
		switch {
		case !strings.HasPrefix(line, "ip ") && !strings.HasPrefix(line, "ovs-vsctl "):
		case strings.Contains(line, " show"), strings.Contains(line, " find "), strings.Contains(line, " get "),
			strings.Contains(line, " br-exists "), strings.Contains(line, " port-to-br "):
		default:
			out = append(out, line)
		}
	}
	return out
}

// testNodeFabric is the fabric assignment testFabric renders for testNodeA.
func testNodeFabric() *nodeFabric {
	return &nodeFabric{
		vrf: "fabric", table: 100, mtu: 9000, uplinks: "fab1:br-fab1,fab2:br-fab2",
		loopbackV4: "192.0.2.1", loopbackV6: "2001:db8::1",
	}
}

// The first run on a bare node creates the VRF with its table and both
// loopbacks, one marked peer bridge per uplink with the NIC as its port and
// enslaved to the VRF, and the port mapping, whose port name is the bridge
// name. The node network's eth0 and the bridge the script did not create stay
// as they were.
func TestFabricScript_PreparesTheNode(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())

	code, stderr, calls := rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	g.Expect(rig.read("links/fabric")).To(Equal("vrf 100"))
	g.Expect(rig.read("attr/fabric/up")).To(Equal("true"))
	g.Expect(rig.read("attr/fabric/addrs")).To(Equal("192.0.2.1/32 global\n2001:db8::1/128 global"))
	g.Expect(rig.read("ovs/open/c5c3-fabric-vrf")).To(Equal("fabric"))
	g.Expect(rig.read("ovs/open/dynamic-routing-port-mapping")).To(Equal("br-fab1=br-fab1,br-fab2=br-fab2"))

	for nic, br := range map[string]string{"fab1": "br-fab1", "fab2": "br-fab2"} {
		g.Expect(rig.exists("ovs/br/"+br+"/marked")).To(BeTrue(), br)
		g.Expect(rig.exists("ovs/br/"+br+"/port-"+nic)).To(BeTrue(), br)
		g.Expect(rig.read("ovs/br/"+br+"/mtu_request")).To(Equal("9000"), br)
		g.Expect(rig.read("attr/"+br+"/master")).To(Equal("fabric"), br)
		g.Expect(rig.read("attr/"+nic+"/mtu")).To(Equal("9000"), nic)
		// accept_ra is 0 on both devices before either comes up, so no router
		// advertisement of the leaf reaches a device that is up.
		g.Expect(calls).To(ContainSubstring("state: "+nic+" up with accept_ra=0\n"), nic)
		g.Expect(calls).To(ContainSubstring("state: "+br+" up with accept_ra=0\n"), br)
		// IPv6 is off on the NIC before it comes up, so the kernel gives the
		// OVS port no link-local address and puts no fe80::/64 route over it
		// into the main table. The bridge keeps IPv6 for the session.
		g.Expect(calls).To(ContainSubstring("state: "+nic+" up with disable_ipv6=1\n"), nic)
		g.Expect(calls).To(ContainSubstring("state: "+br+" up with disable_ipv6=0\n"), br)
	}

	g.Expect(rig.read("attr/eth0/addrs")).To(Equal("10.0.0.5/24 global"))
	g.Expect(rig.exists("attr/eth0/master")).To(BeFalse())
	g.Expect(rig.exists("ovs/br/br-ex")).To(BeTrue(), "a bridge the script did not create is not its own")
}

// A second run over an unchanged entry changes nothing on the node.
func TestFabricScript_SecondRunChangesNothing(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())
	code, stderr, calls := rig.run(t)
	g.Expect(code).To(Equal(0), stderr+calls)
	before := rig.state(t)

	code, stderr, calls = rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	g.Expect(rig.state(t)).To(Equal(before))
	g.Expect(calls).NotTo(ContainSubstring("link del"), "an unchanged VRF is kept rather than re-created")
}

// An uplink that does not exist stops the run before it changes anything: the
// guards run ahead of the first write.
func TestFabricScript_MissingUplinkFailsBeforeAnyWrite(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	fabric := testNodeFabric()
	fabric.uplinks = "fab1:br-fab1,fab3:br-fab3"
	rig.writeEntry(t, fabric)
	before := rig.state(t)

	code, stderr, calls := rig.run(t)

	g.Expect(code).To(Equal(1))
	g.Expect(stderr).To(Equal("ERROR: fabric uplink fab3 does not exist on node-a\n"))
	g.Expect(writes(calls)).To(BeEmpty())
	g.Expect(rig.state(t)).To(Equal(before))
}

// An uplink that carries a global address is the node network's port rather
// than a fabric uplink, and the run stops before it changes anything.
func TestFabricScript_UplinkWithGlobalAddressFailsBeforeAnyWrite(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	fabric := testNodeFabric()
	fabric.uplinks = "fab1:br-fab1,eth0:br-fab2"
	rig.writeEntry(t, fabric)
	before := rig.state(t)

	code, stderr, calls := rig.run(t)

	g.Expect(code).To(Equal(1))
	g.Expect(stderr).To(Equal("ERROR: fabric uplink eth0 carries a global address on node-a; " +
		"a fabric uplink carries link-local addresses only\n"))
	g.Expect(writes(calls)).To(BeEmpty())
	g.Expect(rig.state(t)).To(Equal(before))
}

// A device that already carries the VRF name and is not the VRF the script
// recorded is somebody else's: a NIC, or a VRF of the host on the fabric table
// or on another one. The run stops before it changes anything, rather than
// deleting the device or stripping its addresses.
func TestFabricScript_ForeignDeviceUnderTheVRFNameFailsBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct{ name, kind string }{
		{name: "a NIC", kind: "nic"},
		{name: "a VRF on the fabric table", kind: "vrf 100"},
		{name: "a VRF on another table", kind: "vrf 200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			rig := newFabricRig(t)
			rig.addNIC(t, "fabric", "10.0.0.6/24 global")
			g.Expect(os.WriteFile(filepath.Join(rig.dir, "links", "fabric"), []byte(tc.kind+"\n"),
				fabricRigFileMode)).To(Succeed())
			rig.writeEntry(t, testNodeFabric())
			before := rig.state(t)

			code, stderr, calls := rig.run(t)

			g.Expect(code).To(Equal(1), calls)
			g.Expect(stderr).To(Equal("ERROR: fabric exists on node-a and is not the VRF the fabric script created\n"))
			g.Expect(writes(calls)).To(BeEmpty())
			g.Expect(rig.state(t)).To(Equal(before))
		})
	}
}

// A peer bridge name that another device of the node carries, such as the
// node's bond, stops the run before it changes anything: OVS could not create
// the bridge's own interface, and the script would enslave that device to the
// VRF instead.
func TestFabricScript_ForeignDeviceUnderABridgeNameFailsBeforeAnyWrite(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.addNIC(t, "bond0", "10.0.0.7/24 global")
	fabric := testNodeFabric()
	fabric.uplinks = "fab1:br-fab1,fab2:bond0"
	rig.writeEntry(t, fabric)
	before := rig.state(t)

	code, stderr, calls := rig.run(t)

	g.Expect(code).To(Equal(1), calls)
	g.Expect(stderr).To(Equal("ERROR: bond0 exists on node-a and is not an OVS bridge; " +
		"a peer bridge takes a name no other device of the node carries\n"))
	g.Expect(writes(calls)).To(BeEmpty())
	g.Expect(rig.state(t)).To(Equal(before))
}

// An uplink that moves to another NIC or another kept bridge leaves the port
// it had: each bridge ends up with the one NIC the entry pairs it with, two
// bridges can swap their NICs, and a port the script did not add stays.
func TestFabricScript_MovedUplinkLeavesItsOldBridge(t *testing.T) {
	for _, tc := range []struct {
		name, uplinks string
		ports         map[string]string
	}{
		{
			name: "another NIC", uplinks: "fab3:br-fab1,fab2:br-fab2",
			ports: map[string]string{"br-fab1": "fab3", "br-fab2": "fab2"},
		},
		{
			name: "swapped NICs", uplinks: "fab2:br-fab1,fab1:br-fab2",
			ports: map[string]string{"br-fab1": "fab2", "br-fab2": "fab1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			rig := newFabricRig(t)
			rig.addNIC(t, "fab3", "fe80::3/64 link")
			rig.writeEntry(t, testNodeFabric())
			code, stderr, calls := rig.run(t)
			g.Expect(code).To(Equal(0), stderr+calls)
			patch := filepath.Join(rig.dir, "ovs/br/br-fab1/port-patch-br-fab1-to-br-int")
			g.Expect(os.WriteFile(patch, nil, fabricRigFileMode)).To(Succeed())

			moved := testNodeFabric()
			moved.uplinks = tc.uplinks
			rig.writeEntry(t, moved)
			code, stderr, calls = rig.run(t)

			g.Expect(code).To(Equal(0), stderr+calls)
			for br, nic := range tc.ports {
				entries, err := os.ReadDir(filepath.Join(rig.dir, "ovs/br", br))
				g.Expect(err).NotTo(HaveOccurred())
				var ports []string
				for _, e := range entries {
					if name, ok := strings.CutPrefix(e.Name(), "port-"); ok && !strings.HasPrefix(name, "patch-") {
						ports = append(ports, name)
					}
				}
				g.Expect(ports).To(ConsistOf(nic), br)
			}
			g.Expect(rig.exists("ovs/br/br-fab1/port-patch-br-fab1-to-br-int")).To(BeTrue(),
				"a port the script did not add is not its own")
		})
	}
}

// A kernel without the vrf module cannot create the VRF, and the run says so.
func TestFabricScript_VRFThatCannotBeCreatedFailsTheRun(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())

	code, stderr, _ := rig.run(t, "NO_VRF_MODULE=1")

	g.Expect(code).To(Equal(1))
	g.Expect(stderr).To(HaveSuffix(
		"ERROR: cannot create the VRF device fabric on node-a; the node kernel has no vrf module\n"))
	g.Expect(rig.exists("links/fabric")).To(BeFalse())
}

// A peer bridge whose netdev never appears fails the run after 30 one-second
// waits rather than enslaving a device that is not there.
func TestFabricScript_BridgeThatNeverAppearsFailsAfterThirtySeconds(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())

	code, stderr, calls := rig.run(t, "NO_BRIDGE_NETDEV=1")

	g.Expect(code).To(Equal(1))
	g.Expect(stderr).To(Equal("ERROR: peer bridge br-fab1 did not appear on node-a\n"))
	g.Expect(strings.Count(calls, "sleep 1\n")).To(Equal(30), calls)
	g.Expect(calls).NotTo(ContainSubstring("master"), "nothing is enslaved before the bridge exists")
}

// A changed table re-creates the VRF, changed loopbacks replace the old ones,
// and an uplink the entry no longer names loses its bridge.
func TestFabricScript_ChangedEntryIsApplied(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())
	code, stderr, calls := rig.run(t)
	g.Expect(code).To(Equal(0), stderr+calls)

	changed := testNodeFabric()
	changed.table = 200
	changed.loopbackV4 = "192.0.2.9"
	changed.loopbackV6 = "2001:db8::9"
	changed.uplinks = "fab1:br-fab1"
	rig.writeEntry(t, changed)

	code, stderr, calls = rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	g.Expect(rig.read("links/fabric")).To(Equal("vrf 200"))
	g.Expect(rig.read("attr/fabric/addrs")).To(Equal("192.0.2.9/32 global\n2001:db8::9/128 global"))
	g.Expect(rig.exists("ovs/br/br-fab2")).To(BeFalse())
	g.Expect(rig.exists("links/br-fab2")).To(BeFalse())
	g.Expect(rig.exists("links/fab2")).To(BeTrue(), "the NIC outlives its bridge")
	g.Expect(rig.read("ovs/open/dynamic-routing-port-mapping")).To(Equal("br-fab1=br-fab1"))
}

// A renamed VRF deletes the one the script recorded and records the new one.
func TestFabricScript_RenamedVRFReplacesTheRecordedOne(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())
	code, stderr, calls := rig.run(t)
	g.Expect(code).To(Equal(0), stderr+calls)

	renamed := testNodeFabric()
	renamed.vrf = "fabric2"
	rig.writeEntry(t, renamed)

	code, stderr, calls = rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	g.Expect(rig.exists("links/fabric")).To(BeFalse())
	g.Expect(rig.read("links/fabric2")).To(Equal("vrf 100"))
	g.Expect(rig.read("ovs/open/c5c3-fabric-vrf")).To(Equal("fabric2"))
	g.Expect(rig.read("attr/br-fab1/master")).To(Equal("fabric2"))
}

// An entry without FABRIC_ keys is the teardown: the marked bridges, the
// recorded VRF and the port mapping go, the NICs and every device the script
// did not create stay, and a second teardown finds nothing left to change.
func TestFabricScript_EntryWithoutFabricKeysTearsDown(t *testing.T) {
	g := NewGomegaWithT(t)
	rig := newFabricRig(t)
	rig.writeEntry(t, testNodeFabric())
	code, stderr, calls := rig.run(t)
	g.Expect(code).To(Equal(0), stderr+calls)

	rig.writeEntry(t, nil)
	code, stderr, calls = rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	for _, gone := range []string{
		"ovs/br/br-fab1", "ovs/br/br-fab2", "links/br-fab1", "links/br-fab2", "links/fabric",
		"ovs/open/c5c3-fabric-vrf", "ovs/open/dynamic-routing-port-mapping",
	} {
		g.Expect(rig.exists(gone)).To(BeFalse(), gone)
	}
	for _, kept := range []string{"links/fab1", "links/fab2", "links/eth0", "ovs/br/br-ex"} {
		g.Expect(rig.exists(kept)).To(BeTrue(), kept)
	}
	before := rig.state(t)

	code, stderr, calls = rig.run(t)

	g.Expect(code).To(Equal(0), stderr+calls)
	g.Expect(rig.state(t)).To(Equal(before))
}

// The init container and an apply Job can run the script on one node at the
// same moment. Both runs exit 0 and leave the node exactly as one run on its
// own does. The pair is started repeatedly, because whether the two race for
// the same device depends on the scheduler.
func TestFabricScript_ConcurrentRunsConverge(t *testing.T) {
	g := NewGomegaWithT(t)
	single := newFabricRig(t)
	single.writeEntry(t, testNodeFabric())
	code, stderr, calls := single.run(t)
	g.Expect(code).To(Equal(0), stderr+calls)
	want := single.state(t)

	for range 6 {
		rig := newFabricRig(t)
		rig.writeEntry(t, testNodeFabric())
		logs := t.TempDir()
		first := rig.command(t, filepath.Join(logs, "first"))
		second := rig.command(t, filepath.Join(logs, "second"))
		g.Expect(first.Start()).To(Succeed())
		g.Expect(second.Start()).To(Succeed())

		g.Expect(exitCode(t, first.Wait())).To(Equal(0), readCalls(t, filepath.Join(logs, "first")))
		g.Expect(exitCode(t, second.Wait())).To(Equal(0), readCalls(t, filepath.Join(logs, "second")))
		g.Expect(rig.state(t)).To(Equal(want))
	}
}

// routeProbeScriptStub stands in for ovn-sbctl: each list answers with the
// payload of its table, --bare printing one UUID per line, and fails when
// SBCTL_RC is set.
const routeProbeScriptStub = `ovn-sbctl() {
  if [ -n "${SBCTL_RC:-}" ]; then return "$SBCTL_RC"; fi
  case " $* " in
  *" list Advertised_Route "*) printf '%s' "${ADVERTISED:-}" ;;
  *" list Learned_Route "*) printf '%s' "${LEARNED:-}" ;;
  esac
}
. "$ROUTE_PROBE_SCRIPT"
`

// runRouteProbe runs route-probe.sh against the stub with its termination log
// pointed at a scratch file, and returns the exit code and what it reported.
func runRouteProbe(t *testing.T, env ...string) (int, string) {
	t.Helper()
	g := NewGomegaWithT(t)
	if _, err := exec.LookPath(bashPath); err != nil {
		t.Skipf("%s is unavailable; the route probe script cannot be exercised here: %v", bashPath, err)
	}
	dir := t.TempDir()
	report := filepath.Join(dir, "termination-log")
	g.Expect(strings.Count(routeProbeScript, "/dev/termination-log")).To(Equal(1))
	script := filepath.Join(dir, "route-probe.sh")
	g.Expect(os.WriteFile(script,
		[]byte(strings.ReplaceAll(routeProbeScript, "/dev/termination-log", report)), fabricRigFileMode)).To(Succeed())

	cmd := exec.CommandContext(t.Context(), bashPath, "-c", routeProbeScriptStub)
	cmd.Env = append(os.Environ(), "SB_ADDR=ssl:10.96.0.21:6642", "ROUTE_PROBE_SCRIPT="+script)
	cmd.Env = append(cmd.Env, env...)
	code := exitCode(t, cmd.Run())
	content, err := os.ReadFile(filepath.Clean(report))
	if errors.Is(err, os.ErrNotExist) {
		return code, ""
	}
	g.Expect(err).NotTo(HaveOccurred())
	return code, string(content)
}

// The probe reports the row counts of both tables in the form the Fabric step
// parses, an empty table as 0.
func TestRouteProbeScript_ReportsTheRowCounts(t *testing.T) {
	g := NewGomegaWithT(t)

	code, report := runRouteProbe(t, "ADVERTISED=a\nb\nc\n", "LEARNED=d\ne\nf\ng\n")
	g.Expect(code).To(Equal(0))
	g.Expect(report).To(Equal("advertised=3 learned=4\n"))
	g.Expect(routeProbeReport.MatchString(strings.TrimRight(report, "\n"))).To(BeTrue())

	code, report = runRouteProbe(t)
	g.Expect(code).To(Equal(0))
	g.Expect(report).To(Equal("advertised=0 learned=0\n"))
}

// A query that fails fails the script, rather than reading as a database
// without routes.
func TestRouteProbeScript_FailedQueryFailsTheRun(t *testing.T) {
	g := NewGomegaWithT(t)

	code, report := runRouteProbe(t, "SBCTL_RC=1")

	g.Expect(code).NotTo(Equal(0))
	g.Expect(report).To(BeEmpty(), "a failed probe writes no report")
}
