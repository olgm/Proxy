package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/olgm/proxy/internal/probe"
)

// Probe is the optional measurement service. Delete the block and nothing about
// the chain changes: probed is a separate binary, a separate unit and a separate
// user, and proxyd neither knows nor cares whether it is there.
//
// What it measures is not configurable, because it follows from the routes. Every
// leg a production path actually uses is probed twice — once at one copy and once
// at the count that leg really carries — and every route longer than a single leg
// is probed end to end the same way. A pair of nodes no route puts traffic between
// is never probed, so nothing here polls a path players do not use.
type Probe struct {
	// Hz is probes per second, per class. The wire cost is Hz × copies × 2
	// datagrams per leg per second, each under 100 bytes.
	Hz float64 `json:"hz,omitempty"`
	// Windows are the aggregation periods written to the log. Two is the useful
	// number: a short one to watch an incident happen, and a long one to put
	// enough samples behind a percentile out at p99 for it to mean anything.
	Windows []string `json:"windows,omitempty"`
	// TimeoutMS is how long an unanswered probe waits before it is called lost.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// MaxLogMB rotates the dataset at this size, keeping one previous file.
	MaxLogMB int `json:"max_log_mb,omitempty"`
}

const (
	probeUser    = "probed"
	probeDir     = "/var/lib/probed"
	probeLogPath = probeDir + "/probe.jsonl"
	probeCfgPath = "/etc/probed/config.json"
)

// probeEdge is one leg some production path really carries, and how many copies
// of every packet it carries. Legs are collected across every route, because a
// leg two routes share is one leg and has one count.
type probeEdge struct {
	from, to string
	dup      int
}

// probeChain is one route end to end, when that is more than a single leg. A
// route of exactly one leg is left out: it would be the same measurement as that
// leg's own class, sent twice.
type probeChain struct {
	name  string
	entry string
	exit  string
	// dup is the route's own count, and is what labels the production class in the
	// dataset. The per-leg counts are on the hops; this is the number a reader
	// recognises as "the setting this route runs at".
	dup int
	g   *graph
}

// probeTopology is what the routes say should be measured, before any of it is
// turned into per-node config.
type probeTopology struct {
	edges  []probeEdge
	chains []probeChain
	// port is the bound UDP port for every node something probes toward. A node
	// that only originates gets none, dials out, and needs no inbound rule — the
	// same shape the tunnel's entry has.
	port map[string]int
}

func probeTopo(t *Topology, next int) (*probeTopology, int, error) {
	pt := &probeTopology{port: map[string]int{}}
	dup := map[[2]string]int{}

	for _, r := range t.Routes {
		if r.Transport != "udp" {
			continue // a TCP route has no leg of ours to measure, and no duplication
		}
		g, err := buildGraph(t, r)
		if err != nil {
			return nil, next, err
		}
		n := 0
		for e, d := range g.dup {
			n++
			if old, ok := dup[e]; ok && old != d {
				return nil, next, fmt.Errorf("probe: routes disagree about %s>%s, one says %d copies and another %d; a leg carries one number",
					e[0], e[1], old, d)
			}
			dup[e] = d
		}
		if n > 1 {
			pt.chains = append(pt.chains, probeChain{name: r.Name, entry: r.Entry, exit: r.Exit,
				dup: r.Duplicate, g: g})
		}
	}
	if len(dup) == 0 {
		return nil, next, fmt.Errorf("probe: no route uses a udp leg, so there is nothing to measure")
	}

	for e := range dup {
		if _, ok := dup[[2]string{e[1], e[0]}]; ok {
			return nil, next, fmt.Errorf("probe: routes send traffic both ways across %s and %s; a probe leg has one direction",
				e[0], e[1])
		}
		pt.edges = append(pt.edges, probeEdge{from: e[0], to: e[1], dup: dup[e]})
	}
	sort.Slice(pt.edges, func(i, j int) bool {
		if pt.edges[i].from != pt.edges[j].from {
			return pt.edges[i].from < pt.edges[j].from
		}
		return pt.edges[i].to < pt.edges[j].to
	})
	sort.Slice(pt.chains, func(i, j int) bool { return pt.chains[i].name < pt.chains[j].name })

	// One port per node anything is probed toward, in a stable order so the map
	// `config` prints does not move between runs.
	for _, name := range sortedNodes(t) {
		for _, e := range pt.edges {
			if e.to == name {
				pt.port[name] = next
				next++
				break
			}
		}
	}
	return pt, next, nil
}

// links indexes one node's legs by the peer they reach. A leg we dial is
// host:port, one that dials us a bare IP — the same convention proxyd's config
// uses, and for the same reason: a node that dials has an ephemeral source port.
type probeLinks struct {
	cfg   []probe.LinkConfig
	index map[string]int
}

func (p *probeLinks) add(peer, addr, key string) int {
	if i, ok := p.index[peer]; ok {
		return i
	}
	i := len(p.cfg)
	p.cfg = append(p.cfg, probe.LinkConfig{Addr: addr, Key: key})
	p.index[peer] = i
	return i
}

// expandProbe turns the routes into one config per node, plus the reachability
// checks for the ports it needs opened.
func expandProbe(t *Topology, next int) (map[string]*probe.Config, []check, int, error) {
	pt, next, err := probeTopo(t, next)
	if err != nil {
		return nil, nil, next, err
	}

	links := map[string]*probeLinks{}
	classes := map[string][]probe.Class{}
	linksFor := func(node string) *probeLinks {
		if l, ok := links[node]; ok {
			return l
		}
		l := &probeLinks{index: map[string]int{}}
		links[node] = l
		return l
	}
	// One link per pair per node, shared by every class that crosses it, so a leg
	// carrying four classes still has one socket and one key.
	link := func(node, peer string) int {
		addr := t.Nodes[peer].Addr
		if port, ok := pt.port[peer]; ok && isDown(pt, node, peer) {
			addr = net.JoinHostPort(addr, strconv.Itoa(port))
		}
		return linksFor(node).add(peer, addr, t.keys.probe(node, peer))
	}

	id := 0
	nextID := func() (uint8, error) {
		id++
		if id > 255 {
			return 0, fmt.Errorf("probe: more than 255 classes; a class id is one byte")
		}
		return uint8(id), nil
	}

	// A leg is measured twice: at one copy, and at the count it really carries.
	// The difference between the two is the whole point — it is what duplication
	// buys on that leg, measured on that leg, at the same moment.
	for _, e := range pt.edges {
		name := e.from + ">" + e.to
		for _, d := range counts(e.dup) {
			cid, err := nextID()
			if err != nil {
				return nil, nil, next, err
			}
			out, in := link(e.from, e.to), link(e.to, e.from)
			classes[e.from] = append(classes[e.from], probe.Class{ID: cid, Name: name,
				Kind: probe.KindLeg, Duplicate: d, Down: []probe.Hop{{Link: out}}})
			classes[e.to] = append(classes[e.to], probe.Class{ID: cid, Name: name,
				Kind: probe.KindLeg, Duplicate: d, Up: []probe.Hop{{Link: in}}})
		}
	}

	// And the same pair end to end, which is the only class that sees what a
	// player's packet sees: every leg's copies, and every path raced into the exit.
	for _, ch := range pt.chains {
		name := ch.entry + ">" + ch.exit
		for _, baseline := range []bool{false, true} {
			cid, err := nextID()
			if err != nil {
				return nil, nil, next, err
			}
			dupOf := func(e [2]string) int {
				if baseline {
					return 1
				}
				return ch.g.dup[e]
			}
			// Every hop carries its own leg's count. The class default is only a
			// label, but it has to be an honest one: without it the baseline and
			// the production chain would be two rows of the dataset under the same
			// name and the same number, and nothing would tell them apart.
			label := ch.dup
			if baseline {
				label = 1
			}
			for node := range nodesOf(ch.g, ch.entry) {
				c := probe.Class{ID: cid, Name: name, Kind: probe.KindChain, Duplicate: max(label, 1)}
				for _, to := range ch.g.succ[node] {
					c.Down = append(c.Down, probe.Hop{Link: link(node, to), Duplicate: dupOf([2]string{node, to})})
				}
				for _, from := range ch.g.pred[node] {
					c.Up = append(c.Up, probe.Hop{Link: link(node, from), Duplicate: dupOf([2]string{from, node})})
				}
				classes[node] = append(classes[node], c)
			}
		}
	}

	p := t.Probe
	cfgs := map[string]*probe.Config{}
	for node, cl := range classes {
		c := &probe.Config{
			Name: node, Links: linksFor(node).cfg, Classes: cl,
			Hz: p.Hz, Windows: p.Windows, TimeoutMS: p.TimeoutMS,
			MaxLogMB: p.MaxLogMB, Log: probeLogPath,
			// Only what was asked for: probed applies the same default this
			// prints, so the two cannot say different things.
			FeedWindows: feedWindows(t),
		}
		if port, ok := pt.port[node]; ok {
			c.Bind = bindAddr(t.Nodes[node].BindAddr, port)
		}
		sort.Slice(c.Classes, func(i, j int) bool { return c.Classes[i].ID < c.Classes[j].ID })
		cfgs[node] = c
	}

	var checks []check
	for _, e := range pt.edges {
		port := pt.port[e.to]
		checks = append(checks, check{from: e.from, to: e.to, why: "probe", udp: true,
			unit: "probed", port: port,
			addr: net.JoinHostPort(t.Nodes[e.to].Addr, strconv.Itoa(port))})
	}

	// The health link is allocated only when the status feed asks for it. It
	// exists for exactly one caller, so without that caller it would be an open
	// port with nobody on the other end of it.
	if t.Feeds != nil && t.Feeds.Status != nil && t.Discord != nil {
		bot := t.botNode()
		for _, node := range probeNodes(cfgs) {
			cfgs[node].Health = &probe.HealthConfig{
				Bind:      bindAddr(t.Nodes[node].BindAddr, next),
				Key:       t.keys.probeHealth(node),
				AllowFrom: []string{t.Nodes[bot].Addr},
			}
			if bot != node {
				checks = append(checks, check{from: bot, to: node, why: "health",
					unit: "probed", port: next,
					addr: net.JoinHostPort(t.Nodes[node].Addr, strconv.Itoa(next))})
			}
			next++
		}
	}
	return cfgs, checks, next, nil
}

// counts is what a leg is measured at: one copy for the baseline, and the count
// it really carries. A leg that already carries one copy is measured once —
// probing it twice would be the same measurement under two names.
func counts(dup int) []int {
	if dup <= 1 {
		return []int{1}
	}
	return []int{1, dup}
}

// isDown reports whether node dials peer, which is what decides whether the link
// address carries a port.
func isDown(pt *probeTopology, node, peer string) bool {
	for _, e := range pt.edges {
		if e.from == node && e.to == peer {
			return true
		}
	}
	return false
}

// nodesOf walks a route's graph from its entry, so a chain class is placed on
// every node the route touches and on none that it does not.
func nodesOf(g *graph, entry string) map[string]bool {
	out := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if out[n] {
			return
		}
		out[n] = true
		for _, to := range g.succ[n] {
			walk(to)
		}
	}
	walk(entry)
	return out
}

// feedWindows is the explicit filter, or nothing. The default lives in
// internal/probe so that probed and this agree by construction.
func feedWindows(t *Topology) []string {
	if t.Feeds == nil || t.Feeds.Probe == nil {
		return nil
	}
	return t.Feeds.Probe.Windows
}

func printProbe(t *Topology, cfgs map[string]*probe.Config) {
	if t.Probe == nil {
		return
	}
	fmt.Printf("probe: %.3g Hz, windows %v, log %s\n", t.Probe.Hz, t.Probe.Windows, probeLogPath)
	for _, n := range probeNodes(cfgs) {
		c := cfgs[n]
		bind := c.Bind
		if bind == "" {
			// Like the tunnel's entry: it dials out and answers come back on the
			// same socket, so it needs no inbound rule of its own.
			bind = "(dials only)"
		}
		fmt.Printf("%s (%s)\n", n, t.Nodes[n].Addr)
		if c.Health != nil {
			fmt.Printf("    %-3s %-22s    %-14s %-6s     %s\n", "tcp", c.Health.Bind, "health", "", "bot")
		}
		for _, k := range c.Classes {
			role := "relay"
			switch {
			case len(k.Up) == 0:
				role = "origin"
			case len(k.Down) == 0:
				role = "answer"
			}
			fmt.Printf("    %-3s %-22s    %-14s %-6s x%-3d %s\n",
				"udp", bind, k.Name, k.Kind, k.Duplicate, role)
			bind = ""
		}
	}
	fmt.Println()
}

func probeNodes(cfgs map[string]*probe.Config) []string {
	out := make([]string, 0, len(cfgs))
	for n := range cfgs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// probeOriginators are the nodes with something to report: a class with no up
// links is one this node starts, and only an originator writes a line. Chicago
// answers everything and originates nothing, so it is not given a webhook URL it
// would never use.
func probeOriginators(cfgs map[string]*probe.Config) []string {
	var out []string
	for _, n := range probeNodes(cfgs) {
		for _, k := range cfgs[n].Classes {
			if len(k.Up) == 0 {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// deployProbe installs probed on every node that measures anything, after every
// proxyd, so a leg is carrying real traffic before it is asked about.
func deployProbe(t *Topology, cfgs map[string]*probe.Config, tmp, repo string, roots map[string]bool) error {
	probeFeed := ""
	if t.Feeds != nil && t.Feeds.Probe != nil {
		var err error
		if probeFeed, err = feedEnv(map[string]*Feed{envProbeWebhook: &t.Feeds.Probe.Feed}); err != nil {
			return fmt.Errorf("feeds.probe: %w", err)
		}
	}
	originates := map[string]bool{}
	for _, n := range probeOriginators(cfgs) {
		originates[n] = true
	}
	built := map[string]string{}
	for _, name := range probeNodes(cfgs) {
		node := t.Nodes[name]
		fmt.Printf("== %s (%s) probe\n", name, node.SSH)

		arch, root, err := probeHost(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		roots[name] = root
		bin, ok := built[arch]
		if !ok {
			bin = filepath.Join(tmp, "probed-"+arch)
			if err := build(repo, "./cmd/probed", arch, bin); err != nil {
				return fmt.Errorf("build probed %s: %w", arch, err)
			}
			built[arch] = bin
			fmt.Printf("   build    linux/%s\n", arch)
		}
		b, _ := json.MarshalIndent(cfgs[name], "", "  ")
		cfgPath := filepath.Join(tmp, name+".probe.json")
		if err := os.WriteFile(cfgPath, append(b, '\n'), 0o600); err != nil {
			return err
		}
		if err := scp(node.SSH, bin, "/tmp/probed.new"); err != nil {
			return fmt.Errorf("%s: upload probed: %w", name, err)
		}
		if err := scp(node.SSH, cfgPath, "/tmp/probed.config.json"); err != nil {
			return fmt.Errorf("%s: upload probe config: %w", name, err)
		}
		env := ""
		if originates[name] {
			env = probeFeed
		}
		out, err := ssh(node.SSH, installProbeScript(root, env))
		if err != nil {
			return fmt.Errorf("%s: install probed: %w\n%s", name, err, out)
		}
		fmt.Printf("   install  %s", lastLines(out, 2))
	}
	return nil
}

// installProbeScript gives probed its own account and its own state directory.
// It holds the probe keys and nothing else: compromising it must not be a way
// into a live session, which is why it does not read proxyd's config.
func installProbeScript(root bool, feeds string) string {
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

$SUDO install -m 0755 /tmp/probed.new /usr/local/bin/probed
$SUDO install -d -m 0755 /etc/probed
$SUDO install -m 0640 -o root -g "$USER" /tmp/probed.config.json %s
$SUDO install -d -m 0750 -o "$USER" -g "$USER" %s
%s
rm -f /tmp/probed.new /tmp/probed.config.json

$SUDO tee /etc/systemd/system/probed.service >/dev/null <<'UNIT'
[Unit]
Description=probed
After=network-online.target
Wants=network-online.target

[Service]
User=%s
EnvironmentFile=-/etc/probed/feeds.env
ExecStart=/usr/local/bin/probed -c %s
Restart=always
RestartSec=2
StateDirectory=probed
NoNewPrivileges=true
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
$SUDO systemctl enable probed >/dev/null 2>&1
$SUDO systemctl restart probed
sleep 1
$SUDO systemctl is-active probed
`, sudo, probeUser, probeCfgPath, probeDir, envFile("/etc/probed/feeds.env", `"$USER"`, feeds), probeUser, probeCfgPath)
}

// uninstallProbe removes the service and its config. The dataset is left behind:
// it is measurement anyone may still want, and nothing else writes there.
func uninstallProbe() string {
	return `$SUDO systemctl disable --now probed >/dev/null 2>&1 || true
$SUDO rm -f /etc/systemd/system/probed.service /usr/local/bin/probed ` + probeCfgPath + `
$SUDO rmdir /etc/probed 2>/dev/null || true`
}
