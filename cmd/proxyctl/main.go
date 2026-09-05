// Command proxyctl deploys proxyd to a chain of servers described by one topology
// file. It runs on the operator's machine only; nothing that touches SSH
// credentials is ever installed on a node.
//
// It shells out to the system ssh and scp so that ~/.ssh/config, Tailscale SSH,
// ProxyJump, bastions, agents and hardware keys all work without us reimplementing
// any of it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/olgm/proxy/internal/proxy"
)

type Topology struct {
	// User is the unprivileged account proxyd runs as. Never root.
	User     string          `json:"user"`
	BasePort int             `json:"base_port"`
	Nodes    map[string]Node `json:"nodes"`
	Routes   []Route         `json:"routes"`

	keys *keyring
}

type Node struct {
	// SSH is the control plane: an ssh target or ~/.ssh/config alias.
	SSH string `json:"ssh"`
	// Addr is the data plane: the address the previous hop dials. Public IP today;
	// putting a mesh IP here is the whole change needed to move a hop onto a VPN.
	Addr string `json:"addr"`
	// BindAddr optionally restricts listeners to one local address. Empty binds all
	// interfaces. Set this to a mesh IP to give a relay no public surface at all.
	BindAddr string `json:"bind_addr,omitempty"`
}

type Route struct {
	Name  string `json:"name"`
	Entry string `json:"entry"`
	Port  int    `json:"port"`
	// Transport is how the hops after the entry talk to each other: "tcp" (default)
	// gives every leg its own congestion control, "udp" gives up-front loss repair,
	// duplication and racing. Players always arrive over TCP either way.
	Transport string `json:"transport,omitempty"`
	// Via is the ordered chain after the entry, ending at the exit. Shorthand for a
	// single path; set Paths and Exit instead to race several.
	Via []string `json:"via,omitempty"`
	// Exit is the node every path converges on, and the only one that talks to the
	// target. Required with Paths, implied by the end of Via.
	Exit  string `json:"exit,omitempty"`
	Paths []Path `json:"paths,omitempty"`
	// Duplicate is how many copies of each chunk every leg carries, for paths and
	// legs that do not set their own. UDP only; defaults to 2.
	Duplicate int `json:"duplicate,omitempty"`
	// Legs sets the count on individual legs, over whatever the path they belong
	// to says. A leg that is clean can carry one copy while the one after it
	// carries three: every node drops the copies it receives to one and sends on
	// with the count of the leg after.
	Legs []Leg `json:"legs,omitempty"`
	// Tunnel is optional per-route tuning, passed to every node on the route.
	Tunnel *proxy.Tunnel `json:"tunnel,omitempty"`
	Target Target        `json:"target"`
	// Whitelist is a local ign:uuid file, seeded onto the entry node the first time
	// it is deployed. The node owns it from then on — proxyd rewrites IGNs there
	// when players rename — so later deploys leave it alone.
	Whitelist string `json:"whitelist,omitempty"`
}

// Path is one way from the entry to the exit. Via lists only what is in between:
// every path starts at the entry and ends at the exit, because that is what makes
// the exit able to de-duplicate them.
type Path struct {
	Via       []string `json:"via"`
	Duplicate int      `json:"duplicate,omitempty"`
}

// Leg names one node-to-node edge of a route by its ends, in the order data
// travels toward the exit. The count applies to both directions of that leg.
type Leg struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Duplicate int    `json:"duplicate"`
}

func (l Leg) edge() [2]string { return [2]string{l.From, l.To} }

// defaultDuplicate is what an unqualified UDP route sends. Two copies of a
// Minecraft session is a few tens of KB/s: cheap against one lost packet costing a
// round trip, and useless against a leg that is dropping because it is full.
const defaultDuplicate = 2

// normalize resolves the shorthand and rejects combinations that cannot work.
func (r *Route) normalize() error {
	switch r.Transport {
	case "", "tcp":
		r.Transport = "tcp"
	case "udp":
	default:
		return fmt.Errorf("route %q: transport must be tcp or udp, not %q", r.Name, r.Transport)
	}
	if len(r.Via) > 0 && len(r.Paths) > 0 {
		return fmt.Errorf("route %q: set via or paths, not both", r.Name)
	}
	if len(r.Via) > 0 {
		r.Exit = r.Via[len(r.Via)-1]
		r.Paths = []Path{{Via: r.Via[:len(r.Via)-1], Duplicate: r.Duplicate}}
		r.Via = nil
	}
	if len(r.Paths) == 0 {
		return fmt.Errorf("route %q: needs via or paths", r.Name)
	}
	if r.Exit == "" {
		return fmt.Errorf("route %q: paths need an exit to converge on", r.Name)
	}
	if r.Transport == "tcp" {
		if len(r.Paths) > 1 {
			return fmt.Errorf("route %q: racing needs transport udp; tcp carries one path", r.Name)
		}
		if r.Duplicate > 1 || r.Paths[0].Duplicate > 1 || len(r.Legs) > 0 {
			return fmt.Errorf("route %q: duplicate needs transport udp", r.Name)
		}
		return nil
	}
	if r.Duplicate == 0 {
		r.Duplicate = defaultDuplicate
	}
	for i := range r.Paths {
		if r.Paths[i].Duplicate == 0 {
			r.Paths[i].Duplicate = r.Duplicate
		}
		if r.Paths[i].Duplicate < 1 {
			return fmt.Errorf("route %q: duplicate must be at least 1", r.Name)
		}
	}
	for _, l := range r.Legs {
		if l.Duplicate < 1 {
			return fmt.Errorf("route %q: leg %s>%s: duplicate must be at least 1", r.Name, l.From, l.To)
		}
	}
	return nil
}

// The list lives outside /etc/proxyd because proxyd writes to it, and /etc stays
// operator-owned and read-only to the service. StateDirectory= in the unit is what
// keeps this one path writable under ProtectSystem=strict.
const (
	whitelistDir  = "/var/lib/proxyd"
	whitelistPath = whitelistDir + "/whitelist.txt"
)

type Target struct {
	Addr        string `json:"addr"`
	RewriteHost string `json:"rewrite_host"`
	RewritePort uint16 `json:"rewrite_port"`
}

func main() {
	fs := flag.NewFlagSet("proxyctl", flag.ExitOnError)
	file := fs.String("f", "topology.json", "topology file")
	repo := fs.String("C", ".", "repository root to build proxyd from")
	fs.Usage = usage

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs.Parse(os.Args[2:])

	t, err := load(*file)
	fail(err)
	cfgs, checks, err := expand(t)
	fail(err)

	switch cmd {
	case "config":
		printConfigs(t, cfgs)
	case "deploy":
		printConfigs(t, cfgs)
		fail(deploy(t, cfgs, checks, *repo))
	case "status":
		fail(status(t))
	case "uninstall":
		fail(uninstall(t))
	case "whitelist":
		fail(whitelistCmd(t, fs.Args()))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: proxyctl <command> [-f topology.json] [-C repo]

  config     print the per-node configs this topology expands to, change nothing
  deploy     build, upload and start proxyd on every node
  status     report each node's service state, listening sockets and tunnel links
  uninstall  stop and remove the service, binary and config (leaves the account
             and the whitelist)
  whitelist  list | add <name> [uuid] | remove <name|uuid>
             on every entry that holds a whitelist, over ssh

A route with "transport": "udp" needs one key per leg, and every entry with a
whitelist one key for its control link. They are minted on the first deploy into
tunnel-keys.json beside the topology file, and reused after that, so redeploying
does not cut the chain. Keep that file: without it the next deploy mints new keys
and every node has to be redeployed together.
`)
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxyctl:", err)
		os.Exit(1)
	}
}

func load(path string) (*Topology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Topology
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if t.User == "" {
		t.User = "proxyd"
	}
	if t.User == "root" {
		return nil, fmt.Errorf("user must not be root")
	}
	if t.BasePort == 0 {
		t.BasePort = 9000
	}
	if len(t.Routes) == 0 {
		return nil, fmt.Errorf("topology defines no routes")
	}
	for name, n := range t.Nodes {
		if n.SSH == "" || n.Addr == "" {
			return nil, fmt.Errorf("node %q: ssh and addr are both required", name)
		}
	}
	for i := range t.Routes {
		if err := t.Routes[i].normalize(); err != nil {
			return nil, err
		}
	}
	if t.keys, err = loadKeys(path); err != nil {
		return nil, err
	}
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return nil, err
	}
	// Fail on a mistyped path here, not three ssh round trips into a deploy.
	for _, f := range seeds {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("whitelist: %w", err)
		}
	}
	return &t, nil
}

// whitelistSeeds maps each entry node to the local list file seeded onto it.
func (t *Topology) whitelistSeeds() (map[string]string, error) {
	seeds := map[string]string{}
	for _, r := range t.Routes {
		if r.Whitelist == "" {
			continue
		}
		if old, ok := seeds[r.Entry]; ok && old != r.Whitelist {
			return nil, fmt.Errorf("node %q is the entry for routes with different whitelists (%s, %s)", r.Entry, old, r.Whitelist)
		}
		seeds[r.Entry] = r.Whitelist
	}
	return seeds, nil
}

// expand turns the operator-facing topology into one config per node. Hop ports
// are allocated here so they never have to be kept in sync by hand. It also emits
// the reachability checks implied by the chain, so a deploy can tell the operator
// which links are blocked instead of leaving them to discover it with a client.
func expand(t *Topology) (map[string]*proxy.Config, []check, error) {
	cfgs := map[string]*proxy.Config{}
	var checks []check
	next := t.BasePort

	for _, r := range t.Routes {
		if r.Target.Addr == "" {
			return nil, nil, fmt.Errorf("route %q: target.addr is required", r.Name)
		}
		if r.Port == 0 {
			return nil, nil, fmt.Errorf("route %q: port is required", r.Name)
		}
		var (
			cks []check
			err error
		)
		if r.Transport == "udp" {
			next, cks, err = expandUDP(t, r, cfgs, next)
		} else {
			next, cks, err = expandTCP(t, r, cfgs, next)
		}
		if err != nil {
			return nil, nil, err
		}
		checks = append(checks, cks...)
	}

	// Every entry that holds a whitelist gets a control link, on the ports after
	// the hops. Only loopback may connect until a discord block names the bot's
	// node; that is enough for proxyctl, which arrives over ssh.
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return nil, nil, err
	}
	for _, name := range sortedKeys(seeds) {
		cfgs[name].Control = &proxy.Control{
			Bind: bindAddr(t.Nodes[name].BindAddr, next),
			Key:  t.keys.control(name),
		}
		next++
	}
	return cfgs, checks, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expandTCP is the original chain: one listener per hop, each dialling the next.
func expandTCP(t *Topology, r Route, cfgs map[string]*proxy.Config, next int) (int, []check, error) {
	chain := append(append([]string{r.Entry}, r.Paths[0].Via...), r.Exit)
	if err := distinct(t, r, chain); err != nil {
		return next, nil, err
	}

	// Hop 0 listens on the operator-chosen public port; the rest get allocated.
	ports := make([]int, len(chain))
	ports[0] = r.Port
	for i := 1; i < len(chain); i++ {
		ports[i] = next
		next++
	}

	var checks []check
	for i, name := range chain {
		node := t.Nodes[name]
		l := proxy.Listener{Bind: bindAddr(node.BindAddr, ports[i])}
		if i == 0 {
			l.Minecraft = minecraft(r)
		} else {
			// Only the previous hop may talk to this one, otherwise the relay is
			// an open proxy to the backend and the abuse lands on our egress IP.
			l.AllowFrom = []string{t.Nodes[chain[i-1]].Addr}
		}
		if i == len(chain)-1 {
			l.Upstream = r.Target.Addr
		} else {
			l.Upstream = net.JoinHostPort(t.Nodes[chain[i+1]].Addr, strconv.Itoa(ports[i+1]))
		}
		add(cfgs, name, l)

		if i == 0 {
			checks = append(checks, entryCheck(node, name, ports[i]))
		} else {
			checks = append(checks, check{from: chain[i-1], to: name, why: "hop", port: ports[i],
				addr: net.JoinHostPort(node.Addr, strconv.Itoa(ports[i]))})
		}
	}
	return next, checks, nil
}

// expandUDP lays out a tunnel. Every path starts at the entry and ends at the
// exit, so the shape is a graph rather than a line: a node forwards to all of its
// successors and answers all of its predecessors, and the exit is the one place
// the copies are merged back into a stream.
func expandUDP(t *Topology, r Route, cfgs map[string]*proxy.Config, next int) (int, []check, error) {
	g, err := buildGraph(t, r)
	if err != nil {
		return next, nil, err
	}

	// One UDP port per node that gets dialled. The entry is not one of them: it
	// dials out and replies come back to the same socket, so it needs no inbound
	// rule of its own.
	port := map[string]int{}
	for _, n := range g.order {
		port[n] = next
		next++
	}

	var checks []check
	for _, name := range append([]string{r.Entry}, g.order...) {
		node := t.Nodes[name]
		var l proxy.Listener
		l.Tunnel = r.Tunnel

		if name == r.Entry {
			l.Bind = bindAddr(node.BindAddr, r.Port)
			l.Minecraft = minecraft(r)
			checks = append(checks, entryCheck(node, name, r.Port))
		} else {
			l.Net = "udp"
			l.Bind = bindAddr(node.BindAddr, port[name])
			for _, p := range g.pred[name] {
				// A peer link carries the way back, with the same count as the
				// way out: one number per leg, set at both of its ends.
				l.Peers = append(l.Peers, proxy.Link{
					Addr:      t.Nodes[p].Addr,
					Key:       t.keys.get(r.Name, p, name),
					Duplicate: g.dup[[2]string{p, name}],
				})
			}
		}
		if name == r.Exit {
			l.Upstream = r.Target.Addr
		} else {
			for _, to := range g.succ[name] {
				l.Hops = append(l.Hops, proxy.Link{
					Addr:      net.JoinHostPort(t.Nodes[to].Addr, strconv.Itoa(port[to])),
					Key:       t.keys.get(r.Name, name, to),
					Duplicate: g.dup[[2]string{name, to}],
				})
				checks = append(checks, check{from: name, to: to, why: "hop", udp: true, port: port[to],
					addr: net.JoinHostPort(t.Nodes[to].Addr, strconv.Itoa(port[to]))})
			}
		}
		add(cfgs, name, l)
	}
	return next, checks, nil
}

// graph is one route's node-to-node edges, each with the number of copies it
// carries. Every leg has its own count, and both of its ends are told it.
type graph struct {
	succ  map[string][]string
	pred  map[string][]string
	order []string // nodes that need a bound port, in the order they first appear
	dup   map[[2]string]int
}

func buildGraph(t *Topology, r Route) (*graph, error) {
	g := &graph{succ: map[string][]string{}, pred: map[string][]string{}, dup: map[[2]string]int{}}
	if _, ok := t.Nodes[r.Entry]; !ok {
		return nil, fmt.Errorf("route %q: unknown node %q", r.Name, r.Entry)
	}
	seenNode := map[string]bool{r.Entry: true}
	seenEdge := map[[2]string]bool{}
	// A leg set by name wins over what its paths say, so it can also settle two
	// paths that disagree about a leg they share.
	named := map[[2]string]int{}
	for _, l := range r.Legs {
		if _, dup := named[l.edge()]; dup {
			return nil, fmt.Errorf("route %q: leg %s>%s is listed twice", r.Name, l.From, l.To)
		}
		named[l.edge()] = l.Duplicate
	}

	for _, p := range r.Paths {
		seq := append(append([]string{r.Entry}, p.Via...), r.Exit)
		if err := distinct(t, r, seq); err != nil {
			return nil, err
		}
		for _, n := range seq {
			if !seenNode[n] && n != r.Exit {
				seenNode[n] = true
				g.order = append(g.order, n)
			}
		}
		for i := 0; i+1 < len(seq); i++ {
			e := [2]string{seq[i], seq[i+1]}
			if !seenEdge[e] {
				seenEdge[e] = true
				g.succ[e[0]] = append(g.succ[e[0]], e[1])
				g.pred[e[1]] = append(g.pred[e[1]], e[0])
			}
			// Two paths that share a leg share its packets, and a leg cannot
			// carry two different numbers of copies.
			if n, ok := named[e]; ok {
				g.dup[e] = n
			} else if old, ok := g.dup[e]; ok && old != p.Duplicate {
				return nil, fmt.Errorf("route %q: paths ask for %d and %d copies of every packet on %s>%s; a leg carries one number, or set it under legs",
					r.Name, old, p.Duplicate, e[0], e[1])
			} else {
				g.dup[e] = p.Duplicate
			}
		}
	}
	for e := range named {
		if !seenEdge[e] {
			return nil, fmt.Errorf("route %q: leg %s>%s is not on any path", r.Name, e[0], e[1])
		}
	}
	// The exit binds last, so the port map reads in the order packets travel.
	g.order = append(g.order, r.Exit)

	if err := g.acyclic(r); err != nil {
		return nil, err
	}
	return g, nil
}

// acyclic rejects a set of paths whose edges loop. Each path is a line, but two
// of them can still disagree about which way a leg runs, and a loop in a tunnel
// that floods every successor is a packet storm.
func (g *graph) acyclic(r Route) error {
	const (
		open = 1
		done = 2
	)
	state := map[string]int{}
	var walk func(string) error
	walk = func(n string) error {
		switch state[n] {
		case done:
			return nil
		case open:
			return fmt.Errorf("route %q: paths form a loop through %q", r.Name, n)
		}
		state[n] = open
		for _, to := range g.succ[n] {
			if err := walk(to); err != nil {
				return err
			}
		}
		state[n] = done
		return nil
	}
	return walk(r.Entry)
}

// distinct rejects a path that visits a node twice, or names one that does not
// exist. A repeat would make a node its own next hop.
func distinct(t *Topology, r Route, seq []string) error {
	seen := map[string]bool{}
	for _, n := range seq {
		if _, ok := t.Nodes[n]; !ok {
			return fmt.Errorf("route %q: unknown node %q", r.Name, n)
		}
		if seen[n] {
			return fmt.Errorf("route %q: node %q appears twice in one path", r.Name, n)
		}
		seen[n] = true
	}
	return nil
}

func minecraft(r Route) *proxy.Minecraft {
	m := &proxy.Minecraft{RewriteHost: r.Target.RewriteHost, RewritePort: r.Target.RewritePort}
	if r.Whitelist != "" {
		m.Whitelist = whitelistPath
	}
	return m
}

func entryCheck(node Node, name string, port int) check {
	return check{to: name, why: "entry", port: port,
		addr: net.JoinHostPort(node.Addr, strconv.Itoa(port))}
}

func add(cfgs map[string]*proxy.Config, name string, l proxy.Listener) {
	if cfgs[name] == nil {
		cfgs[name] = &proxy.Config{}
	}
	cfgs[name].Listeners = append(cfgs[name].Listeners, l)
}

func bindAddr(addr string, port int) string {
	if addr == "" {
		return ":" + strconv.Itoa(port)
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

func names(cfgs map[string]*proxy.Config) []string {
	out := make([]string, 0, len(cfgs))
	for n := range cfgs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func printConfigs(t *Topology, cfgs map[string]*proxy.Config) {
	for _, n := range names(cfgs) {
		fmt.Printf("%s (%s)\n", n, t.Nodes[n].Addr)
		for _, l := range cfgs[n].Listeners {
			mode := l.Role()
			if l.Minecraft != nil && l.Minecraft.Whitelist != "" {
				mode += " +whitelist"
			}
			guard := "allow=any"
			if len(l.AllowFrom) > 0 {
				guard = "allow=" + strings.Join(l.AllowFrom, ",")
			}
			if l.Net == "udp" {
				guard = "peers=" + strings.Join(l.PeerAddrs(), ",")
			}
			fmt.Printf("    %-3s %-22s -> %-32s %-28s %s\n", l.Network(), l.Bind, l.Next(), mode, guard)
		}
		if c := cfgs[n].Control; c != nil {
			guard := "allow=loopback"
			if len(c.AllowFrom) > 0 {
				guard += "," + strings.Join(c.AllowFrom, ",")
			}
			fmt.Printf("    %-3s %-22s    %-32s %-28s %s\n", "tcp", c.Bind, "", "control", guard)
		}
	}
	fmt.Println()
}

func marshal(c *proxy.Config) []byte {
	b, _ := json.MarshalIndent(c, "", "  ")
	return append(b, '\n')
}

func deploy(t *Topology, cfgs map[string]*proxy.Config, checks []check, repo string) error {
	built := map[string]string{} // goarch -> local binary path
	roots := map[string]bool{}   // node -> already root over ssh
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return err
	}
	// Before anything is uploaded: a config carrying a key we then failed to
	// record would leave that node unable to talk to the next deploy.
	if err := t.keys.save(); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "proxyctl")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for _, name := range names(cfgs) {
		node := t.Nodes[name]
		fmt.Printf("== %s (%s)\n", name, node.SSH)

		arch, root, err := probe(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		roots[name] = root
		fmt.Printf("   probe    arch=%s root=%v\n", arch, root)

		bin, ok := built[arch]
		if !ok {
			bin = filepath.Join(tmp, "proxyd-"+arch)
			if err := build(repo, arch, bin); err != nil {
				return fmt.Errorf("build %s: %w", arch, err)
			}
			built[arch] = bin
			fmt.Printf("   build    linux/%s\n", arch)
		}

		cfgPath := filepath.Join(tmp, name+".json")
		if err := os.WriteFile(cfgPath, marshal(cfgs[name]), 0o600); err != nil {
			return err
		}
		if err := scp(node.SSH, bin, "/tmp/proxyd.new"); err != nil {
			return fmt.Errorf("%s: upload binary: %w", name, err)
		}
		if err := scp(node.SSH, cfgPath, "/tmp/proxyd.config.json"); err != nil {
			return fmt.Errorf("%s: upload config: %w", name, err)
		}
		seed := seeds[name]
		if seed != "" {
			if err := scp(node.SSH, seed, "/tmp/proxyd.whitelist.txt"); err != nil {
				return fmt.Errorf("%s: upload whitelist: %w", name, err)
			}
		}
		out, err := ssh(node.SSH, installScript(t.User, root, seed != ""))
		if err != nil {
			return fmt.Errorf("%s: install: %w\n%s", name, err, out)
		}
		fmt.Printf("   install  %s", lastLines(out, 2))
	}
	return verify(t, checks, roots)
}

// probe reports the node's architecture and whether we are already root, so the
// install script can skip sudo on infrastructure that only permits root login.
func probe(target string) (arch string, root bool, err error) {
	out, err := ssh(target, "uname -m; id -u")
	if err != nil {
		return "", false, fmt.Errorf("%w\n%s", err, out)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return "", false, fmt.Errorf("unexpected probe output: %q", out)
	}
	switch f[0] {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return "", false, fmt.Errorf("unsupported architecture %q", f[0])
	}
	return arch, f[1] == "0", nil
}

func build(repo, arch, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, "./cmd/proxyd")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, b)
	}
	return nil
}

func ssh(target, script string) ([]byte, error) {
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", target, "bash -s")
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}

func scp(target, local, remote string) error {
	cmd := exec.Command("scp", "-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", local, target+":"+remote)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, b)
	}
	return nil
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// installScript is intentionally idempotent: deploy is the only verb, and running
// it twice must be safe.
func installScript(user string, root, whitelist bool) string {
	sudo := "sudo -n"
	if root {
		sudo = ""
	}
	// Seed the list only when it is absent. proxyd rewrites IGNs into this file as
	// players rename, so the node holds the current copy; overwriting it on every
	// deploy would silently undo those.
	seed := ""
	if whitelist {
		seed = `[ -f ` + whitelistPath + ` ] || \
  $SUDO install -m 0640 -o "$USER" -g "$USER" /tmp/proxyd.whitelist.txt ` + whitelistPath + `
rm -f /tmp/proxyd.whitelist.txt`
	}
	return fmt.Sprintf(`set -eu
SUDO="%s"
USER=%s
NOLOGIN=$(command -v nologin || echo /bin/false)

id -u "$USER" >/dev/null 2>&1 || \
  $SUDO useradd --system --no-create-home --shell "$NOLOGIN" "$USER"

$SUDO install -m 0755 /tmp/proxyd.new /usr/local/bin/proxyd
$SUDO install -d -m 0755 /etc/proxyd
$SUDO install -m 0640 -o root -g "$USER" /tmp/proxyd.config.json /etc/proxyd/config.json
$SUDO install -d -m 0750 -o "$USER" -g "$USER" %s
%s
rm -f /tmp/proxyd.new /tmp/proxyd.config.json

$SUDO tee /etc/systemd/system/proxyd.service >/dev/null <<'UNIT'
[Unit]
Description=proxyd
After=network-online.target
Wants=network-online.target

[Service]
User=%s
ExecStart=/usr/local/bin/proxyd -c /etc/proxyd/config.json
Restart=always
RestartSec=2
StateDirectory=proxyd
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable proxyd >/dev/null 2>&1
$SUDO systemctl restart proxyd
sleep 1
$SUDO systemctl is-active proxyd
`, sudo, user, whitelistDir, seed, user)
}

func status(t *Topology) error {
	for _, name := range sortedNodes(t) {
		node := t.Nodes[name]
		// The newest line per tunnel link, keyed on the address after "link" rather
		// than on a field number, because journald's own prefix would shift those.
		// A restart clears the set: proxyd logs its listeners first, so links that
		// only existed under an older config do not linger in the report.
		out, _ := ssh(node.SSH, `systemctl is-active proxyd 2>&1 || true
ss -lntup 2>/dev/null | grep proxyd || echo "  (no listening sockets)"
journalctl -u proxyd -n 400 --no-pager -o cat 2>/dev/null |
  awk '/ listen /{delete last}
       / link /{for(i=1;i<=NF;i++) if($i=="link"){last[$(i+1)]=$0}}
       END{for(k in last) print last[k]}' |
  sort || true`)
		fmt.Printf("== %s (%s)\n%s\n", name, node.Addr, indent(string(out)))
	}
	return nil
}

func uninstall(t *Topology) error {
	for _, name := range sortedNodes(t) {
		node := t.Nodes[name]
		_, root, err := probe(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		sudo := "sudo -n"
		if root {
			sudo = ""
		}
		out, err := ssh(node.SSH, fmt.Sprintf(`set -u
SUDO="%s"
$SUDO systemctl disable --now proxyd >/dev/null 2>&1 || true
$SUDO rm -f /etc/systemd/system/proxyd.service /usr/local/bin/proxyd /etc/proxyd/config.json
$SUDO rmdir /etc/proxyd 2>/dev/null || true
$SUDO systemctl daemon-reload
echo removed
`, sudo))
		if err != nil {
			return fmt.Errorf("%s: %w\n%s", name, err, out)
		}
		fmt.Printf("== %s %s", name, lastLines(out, 1))
	}
	return nil
}

func sortedNodes(t *Topology) []string {
	out := make([]string, 0, len(t.Nodes))
	for n := range t.Nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "   " + strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}
