package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/olgm/proxy/internal/trial"
	"github.com/olgm/proxy/internal/tunnel"
)

// TrialMesh is the bake-off, and the one file in this repo that describes legs no
// route uses. That is deliberate and is why it is separate from the topology:
// probed measures the production path and may not be pointed off it, so the
// question "which node should the path use" needs somewhere else to live.
//
// Delete this file, and cmd/triald with it, once the trial has answered.
type TrialMesh struct {
	// Port is the one UDP port every node in the mesh binds. The mesh is
	// symmetric — everybody dials everybody they have a leg to — so unlike probed
	// there is nothing to allocate per node.
	Port      int          `json:"port,omitempty"`
	Hz        float64      `json:"hz,omitempty"`
	Windows   []string     `json:"windows,omitempty"`
	TimeoutMS int          `json:"timeout_ms,omitempty"`
	MaxLogMB  int          `json:"max_log_mb,omitempty"`
	Trace     *trial.Trace `json:"trace,omitempty"`

	Nodes map[string]TrialNode `json:"nodes"`
	Legs  []TrialLeg           `json:"legs"`
	Runs  []TrialRun           `json:"runs"`
}

// TrialNode is one machine. Addr is the data plane and is a public IPv4 on
// purpose: production runs on public IPv4, and putting the measurement on the
// Tailscale overlay instead would hide the carrier diversity the trial is about.
type TrialNode struct {
	SSH  string `json:"ssh"`
	Addr string `json:"addr"`
	ID   uint8  `json:"id"`
}

// TrialLeg is one pair worth measuring, and what a round trip over it is expected
// to cost. ExpectMS drives the traceroute trigger and nothing else. Set it from a
// measurement, never from a hope: a value below what the leg really costs traces
// every window forever.
type TrialLeg struct {
	A        string  `json:"a"`
	B        string  `json:"b"`
	ExpectMS float64 `json:"expect_ms"`
}

// TrialRun is one directed flood over a subset of the legs, named for the node it
// starts at. ReverseOf takes another run's edges and turns them around, which is
// how the return direction is described without restating them and getting one
// wrong.
type TrialRun struct {
	From      string   `json:"from"`
	Edges     []string `json:"edges,omitempty"`
	ReverseOf string   `json:"reverse_of,omitempty"`
}

const (
	trialFileName = "trial.json"
	trialUser     = "triald"
	trialDir      = "/var/lib/triald"
	trialLogPath  = trialDir + "/trial.jsonl"
	trialCfgPath  = "/etc/triald/config.json"
	trialDefPort  = 9400
	trialDataDir  = "trial-data"
)

func loadTrial(path string) (*TrialMesh, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found; copy trial.example.json and edit it", path)
	}
	if err != nil {
		return nil, err
	}
	var m TrialMesh
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Port == 0 {
		m.Port = trialDefPort
	}
	if len(m.Nodes) == 0 {
		return nil, fmt.Errorf("%s: no nodes", path)
	}
	ids := map[uint8]string{}
	for name, n := range m.Nodes {
		switch {
		case n.SSH == "":
			return nil, fmt.Errorf("%s: node %s has no ssh target", path, name)
		case n.Addr == "":
			return nil, fmt.Errorf("%s: node %s has no addr", path, name)
		case n.ID == 0:
			return nil, fmt.Errorf("%s: node %s needs an id of 1-255; 0 is reserved", path, name)
		}
		if was, dup := ids[n.ID]; dup {
			return nil, fmt.Errorf("%s: id %d is both %s and %s", path, n.ID, was, name)
		}
		ids[n.ID] = name
	}
	for _, l := range m.Legs {
		if _, ok := m.Nodes[l.A]; !ok {
			return nil, fmt.Errorf("%s: leg %s>%s: no node %s", path, l.A, l.B, l.A)
		}
		if _, ok := m.Nodes[l.B]; !ok {
			return nil, fmt.Errorf("%s: leg %s>%s: no node %s", path, l.A, l.B, l.B)
		}
		if l.A == l.B {
			return nil, fmt.Errorf("%s: leg %s>%s goes nowhere", path, l.A, l.B)
		}
	}
	return &m, nil
}

// edges resolves one run to its directed edges, following ReverseOf.
func (m *TrialMesh) edges(r TrialRun) ([][2]string, error) {
	src := r.Edges
	if r.ReverseOf != "" {
		if len(src) > 0 {
			return nil, fmt.Errorf("run %s: reverse_of and edges are two answers to one question", r.From)
		}
		for _, o := range m.Runs {
			if o.From == r.ReverseOf {
				src = o.Edges
			}
		}
		if src == nil {
			return nil, fmt.Errorf("run %s: reverse_of names %s, which is not a run here", r.From, r.ReverseOf)
		}
	}
	out := make([][2]string, 0, len(src))
	for _, e := range src {
		a, b, ok := strings.Cut(e, ">")
		if !ok {
			return nil, fmt.Errorf("run %s: edge %q should read a>b", r.From, e)
		}
		if r.ReverseOf != "" {
			a, b = b, a
		}
		if m.leg(a, b) == nil {
			return nil, fmt.Errorf("run %s: edge %s>%s is not a declared leg", r.From, a, b)
		}
		out = append(out, [2]string{a, b})
	}
	return out, nil
}

func (m *TrialMesh) leg(a, b string) *TrialLeg {
	for i := range m.Legs {
		if (m.Legs[i].A == a && m.Legs[i].B == b) || (m.Legs[i].A == b && m.Legs[i].B == a) {
			return &m.Legs[i]
		}
	}
	return nil
}

func (m *TrialMesh) nodeNames() []string {
	out := make([]string, 0, len(m.Nodes))
	for n := range m.Nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// expandTrial turns the mesh into one config per node.
//
// Two things are worked out here rather than written down, so that they cannot
// disagree with the mesh. Which legs each node holds follows from the legs it
// appears in. And a leg that no run crosses in either direction is probed on its
// own — that is the only leg that needs its own traffic, because everywhere else
// a run's own packets are the leg's measurement.
func expandTrial(m *TrialMesh, keys *keyring) (map[string]*trial.Config, error) {
	names := map[string]uint8{}
	for name, n := range m.Nodes {
		names[name] = n.ID
	}

	// forward[run][node] is where that node passes an arrival of that run on to.
	forward := map[string]map[string][]string{}
	crossed := map[[2]string]bool{}
	for _, r := range m.Runs {
		if _, ok := m.Nodes[r.From]; !ok {
			return nil, fmt.Errorf("run %s: no node by that name", r.From)
		}
		es, err := m.edges(r)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			return nil, fmt.Errorf("run %s: no edges", r.From)
		}
		byNode := map[string][]string{}
		for _, e := range es {
			byNode[e[0]] = append(byNode[e[0]], e[1])
			l := m.leg(e[0], e[1])
			crossed[[2]string{l.A, l.B}] = true
		}
		forward[r.From] = byNode
	}

	cfgs := map[string]*trial.Config{}
	for _, name := range m.nodeNames() {
		node := m.Nodes[name]
		c := &trial.Config{
			Name: name, ID: node.ID,
			Bind:      fmt.Sprintf(":%d", m.Port),
			Names:     names,
			Hz:        m.Hz,
			Windows:   m.Windows,
			TimeoutMS: m.TimeoutMS,
			MaxLogMB:  m.MaxLogMB,
			Trace:     m.Trace,
			Log:       trialLogPath,
		}
		for _, l := range m.Legs {
			peer := ""
			switch name {
			case l.A:
				peer = l.B
			case l.B:
				peer = l.A
			default:
				continue
			}
			c.Legs = append(c.Legs, trial.Leg{
				Peer: peer, PeerID: m.Nodes[peer].ID,
				Addr:     net.JoinHostPort(m.Nodes[peer].Addr, strconv.Itoa(m.Port)),
				Key:      keys.trial(name, peer),
				ExpectMS: l.ExpectMS,
				Echo:     !crossed[[2]string{l.A, l.B}],
			})
		}
		sort.Slice(c.Legs, func(i, j int) bool { return c.Legs[i].Peer < c.Legs[j].Peer })

		for _, r := range m.Runs {
			if fwd, ok := forward[r.From][name]; ok {
				sort.Strings(fwd)
				c.Runs = append(c.Runs, trial.Run{From: r.From, FromID: m.Nodes[r.From].ID, Forward: fwd})
			}
		}
		sort.Slice(c.Runs, func(i, j int) bool { return c.Runs[i].From < c.Runs[j].From })

		if len(c.Legs) == 0 {
			return nil, fmt.Errorf("node %s has no legs; remove it or give it one", name)
		}
		// Checked here rather than discovered on the nodes. A mesh that expands to
		// something triald rejects otherwise deploys cleanly and then fails to
		// start everywhere at once.
		if err := trial.Validate(*c); err != nil {
			return nil, err
		}
		cfgs[name] = c
	}
	return cfgs, nil
}

// trial returns the key sealing one leg of the bake-off mesh. Its own namespace,
// like probed's and for the same reason: holding these must not be a way into
// anything else, and this is the set most likely to end up on a machine we have
// had for a day and are not sure about.
func (k *keyring) trial(a, b string) string {
	if a > b {
		a, b = b, a
	}
	id := "trial|" + a + "|" + b
	if v, ok := k.keys[id]; ok {
		return v
	}
	v := tunnel.EncodeKey(tunnel.NewKey())
	k.keys[id] = v
	k.dirty = true
	return v
}

func printTrial(m *TrialMesh, cfgs map[string]*trial.Config) {
	fmt.Printf("trial: %d nodes, udp/%d, %.3g Hz, windows %v, log %s\n\n",
		len(cfgs), m.Port, cfgs[m.nodeNames()[0]].Hz, m.Windows, trialLogPath)
	for _, name := range m.nodeNames() {
		c := cfgs[name]
		fmt.Printf("%s (%s) id=%d\n", name, m.Nodes[name].Addr, c.ID)
		for _, l := range c.Legs {
			note := ""
			if l.Echo {
				note = "  (probed on its own: no run crosses it)"
			}
			fmt.Printf("    leg  %-12s %-22s expect %6.1fms%s\n", l.Peer, l.Addr, l.ExpectMS, note)
		}
		for _, r := range c.Runs {
			role := "relay"
			if r.From == name {
				role = "origin"
			}
			fmt.Printf("    run  %-12s %-6s -> %s\n", r.From, role, strings.Join(r.Forward, ", "))
		}
		if len(c.Runs) == 0 {
			fmt.Printf("    run  %-12s %s\n", "-", "terminus for every run")
		}
	}
	fmt.Println()
}

func trialCmd(file, repo string, args []string) error {
	verb := "config"
	if len(args) > 0 {
		verb = args[0]
	}
	if verb == "report" {
		// Reads only what pull already fetched, so it needs neither the mesh file
		// nor a network.
		return trialReport(args[1:])
	}

	m, err := loadTrial(file)
	if err != nil {
		return err
	}
	keys, err := loadKeys(file)
	if err != nil {
		return err
	}
	cfgs, err := expandTrial(m, keys)
	if err != nil {
		return err
	}

	switch verb {
	case "config":
		printTrial(m, cfgs)
		return nil
	case "deploy":
		printTrial(m, cfgs)
		if err := deployTrial(m, cfgs, repo); err != nil {
			return err
		}
		return keys.save()
	case "status":
		return trialStatus(m)
	case "pull":
		return trialPull(m)
	case "uninstall":
		return trialUninstall(m)
	}
	return fmt.Errorf("trial: unknown verb %q; want config, deploy, status, pull, report or uninstall", verb)
}

func deployTrial(m *TrialMesh, cfgs map[string]*trial.Config, repo string) error {
	tmp, err := os.MkdirTemp("", "proxyctl-trial")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	built := map[string]string{}
	for _, name := range m.nodeNames() {
		node := m.Nodes[name]
		fmt.Printf("== %s (%s) trial\n", name, node.SSH)

		arch, root, err := probeHost(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		bin, ok := built[arch]
		if !ok {
			bin = filepath.Join(tmp, "triald-"+arch)
			if err := build(repo, "./cmd/triald", arch, bin); err != nil {
				return fmt.Errorf("build triald %s: %w", arch, err)
			}
			built[arch] = bin
			fmt.Printf("   build    linux/%s\n", arch)
		}
		b, _ := json.MarshalIndent(cfgs[name], "", "  ")
		cfgPath := filepath.Join(tmp, name+".trial.json")
		if err := os.WriteFile(cfgPath, append(b, '\n'), 0o600); err != nil {
			return err
		}
		if err := scp(node.SSH, bin, "/tmp/triald.new"); err != nil {
			return fmt.Errorf("%s: upload triald: %w", name, err)
		}
		if err := scp(node.SSH, cfgPath, "/tmp/triald.config.json"); err != nil {
			return fmt.Errorf("%s: upload trial config: %w", name, err)
		}
		out, err := ssh(node.SSH, installTrialScript(root))
		if err != nil {
			return fmt.Errorf("%s: install triald: %w\n%s", name, err, out)
		}
		fmt.Printf("   install  %s", lastLines(out, 2))
	}
	fmt.Println()
	return trialStatus(m)
}

// installTrialScript is probed's install with two differences.
//
// AmbientCapabilities is the one that is not obvious: NoNewPrivileges disables
// file capabilities, so mtr-packet's cap_net_raw does nothing and every traceroute
// fails silently. Granting it ambiently is what makes the trigger work at all.
//
// And there is no feeds file: this service reports to a dataset and to nothing
// else. It is temporary and nobody should be wiring a channel to it.
func installTrialScript(root bool) string {
	sudo := "sudo -n"
	if root {
		sudo = ""
	}
	return fmt.Sprintf(`set -eu
SUDO="%s"
USER=%s
NOLOGIN=$(command -v nologin || echo /bin/false)

id -u "$USER" >/dev/null 2>&1 || \
  $SUDO useradd --system --no-create-home --shell "$NOLOGIN" "$USER"

$SUDO install -m 0755 /tmp/triald.new /usr/local/bin/triald
$SUDO install -d -m 0755 /etc/triald
$SUDO install -m 0640 -o root -g "$USER" /tmp/triald.config.json %s
$SUDO install -d -m 0750 -o "$USER" -g "$USER" %s
rm -f /tmp/triald.new /tmp/triald.config.json

$SUDO tee /etc/systemd/system/triald.service >/dev/null <<'UNIT'
[Unit]
Description=triald
After=network-online.target
Wants=network-online.target

[Service]
User=%s
ExecStart=/usr/local/bin/triald -c %s
Restart=always
RestartSec=2
StateDirectory=triald
NoNewPrivileges=true
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
LimitNOFILE=8192

[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable triald >/dev/null 2>&1
$SUDO systemctl restart triald
sleep 1
$SUDO systemctl is-active triald
`, sudo, trialUser, trialCfgPath, trialDir, trialUser, trialCfgPath)
}

// trialStatus reports which legs are actually carrying datagrams.
//
// A UDP port cannot be tested by connecting to it, so the only honest signal is
// that the far end has answered — which triald logs the moment a leg starts
// carrying, in the same words proxyd and probed use. A leg that stays down after a
// deploy is a firewall, and the rule that opens it is printed rather than applied.
func trialStatus(m *TrialMesh) error {
	fmt.Println("== trial")
	var blocked []string
	for _, name := range m.nodeNames() {
		node := m.Nodes[name]
		out, err := ssh(node.SSH, `systemctl is-active triald 2>/dev/null || true
journalctl -u triald -n 200 --no-pager 2>/dev/null | grep -F ": link " | tail -40 || true`)
		if err != nil {
			fmt.Printf("%-12s unreachable: %v\n", name, err)
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		state := strings.TrimSpace(lines[0])
		up := map[string]bool{}
		for _, l := range lines[1:] {
			i := strings.Index(l, ": link ")
			if i < 0 {
				continue
			}
			f := strings.Fields(l[i+len(": link "):])
			if len(f) == 2 {
				up[f[0]] = f[1] == "up"
			}
		}
		fmt.Printf("%-12s %s\n", name, state)
		for _, peer := range m.peersOf(name) {
			addr := net.JoinHostPort(m.Nodes[peer].Addr, strconv.Itoa(m.Port))
			word := "no traffic yet"
			switch u, seen := up[addr]; {
			case seen && u:
				word = "up"
			case seen:
				word = "down"
			}
			if word != "up" {
				blocked = append(blocked, fmt.Sprintf(
					"  %s: ufw allow from %s to any port %d proto udp comment 'triald from %s'",
					name, m.Nodes[peer].Addr, m.Port, peer))
			}
			fmt.Printf("    %-12s %-22s %s\n", peer, addr, word)
		}
	}
	if len(blocked) > 0 {
		fmt.Printf("\n%d legs are not carrying. If that is a firewall, these open them:\n", len(blocked))
		for _, b := range blocked {
			fmt.Println(b)
		}
		fmt.Println("\nNot applied: nothing here changes a firewall without being told to.")
	}
	return nil
}

func (m *TrialMesh) peersOf(name string) []string {
	var out []string
	for _, l := range m.Legs {
		switch name {
		case l.A:
			out = append(out, l.B)
		case l.B:
			out = append(out, l.A)
		}
	}
	sort.Strings(out)
	return out
}

// trialPull fetches every node's dataset, and probed's alongside it. Both are
// needed in one place: the first question the trial has to answer is how a
// candidate leg compares with the leg it would replace, and probed is the only
// thing that has been measuring the latter.
func trialPull(m *TrialMesh) error {
	if err := os.MkdirAll(trialDataDir, 0o750); err != nil {
		return err
	}
	for _, name := range m.nodeNames() {
		node := m.Nodes[name]
		for _, f := range []struct{ remote, suffix string }{
			{trialLogPath, ".trial.jsonl"},
			{"/var/lib/probed/probe.jsonl", ".probe.jsonl"},
		} {
			local := filepath.Join(trialDataDir, name+f.suffix)
			// Through a cat over ssh rather than scp: the files are root-or-service
			// owned and a plain scp of them fails on exactly the nodes that have
			// the most to say.
			out, err := ssh(node.SSH, "cat "+f.remote+" 2>/dev/null || true")
			if err != nil {
				fmt.Printf("%-12s %-14s %v\n", name, filepath.Base(f.remote), err)
				continue
			}
			if len(out) == 0 {
				fmt.Printf("%-12s %-14s (none)\n", name, filepath.Base(f.remote))
				continue
			}
			if err := os.WriteFile(local, out, 0o640); err != nil {
				return err
			}
			fmt.Printf("%-12s %-14s %d records -> %s\n",
				name, filepath.Base(f.remote), countLines(out), local)
		}
	}
	return nil
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func trialUninstall(m *TrialMesh) error {
	for _, name := range m.nodeNames() {
		node := m.Nodes[name]
		sudo := "sudo -n"
		if _, root, err := probeHost(node.SSH); err == nil && root {
			sudo = ""
		}
		// The dataset is left behind, as probed's is: it is measurement somebody
		// may still want, and nothing else writes there.
		out, err := ssh(node.SSH, fmt.Sprintf(`set -eu
SUDO="%s"
$SUDO systemctl disable --now triald >/dev/null 2>&1 || true
$SUDO rm -f /etc/systemd/system/triald.service /usr/local/bin/triald %s
$SUDO rmdir /etc/triald 2>/dev/null || true
$SUDO systemctl daemon-reload
echo removed`, sudo, trialCfgPath))
		if err != nil {
			fmt.Printf("%-12s %v\n%s\n", name, err, out)
			continue
		}
		fmt.Printf("%-12s %s", name, lastLines(out, 1))
	}
	fmt.Printf("\nDatasets left in %s on every node.\n", trialDir)
	return nil
}
