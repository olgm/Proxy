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
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/olgm/proxy/internal/botcfg"
	"github.com/olgm/proxy/internal/probe"
	"github.com/olgm/proxy/internal/proxy"
	"github.com/olgm/proxy/internal/version"
)

type Topology struct {
	// User is the unprivileged account proxyd runs as. Never root.
	User     string          `json:"user"`
	BasePort int             `json:"base_port"`
	Nodes    map[string]Node `json:"nodes"`
	Routes   []Route         `json:"routes"`
	// Discord, when set, deploys the bot to one node. Delete the block to run
	// without it.
	Discord *Discord `json:"discord,omitempty"`
	// Probe, when set, deploys probed beside proxyd on every node a production
	// path touches. Delete the block and the chain is unchanged: it is a separate
	// binary, unit and user, and proxyd does not know it exists.
	Probe *Probe `json:"probe,omitempty"`
	// Feeds, when set, post what the chain is doing to Discord webhooks. Every
	// feed is off until it is named here, and all of them are optional.
	Feeds *Feeds `json:"feeds,omitempty"`
	// IPInfo, when true, has every entry look up where its players' networks
	// are on ipinfo.io, by prefix, and record it with the session. Off unless
	// set: it is the one thing here that tells a third party about a player.
	IPInfo bool `json:"ipinfo,omitempty"`

	keys *keyring
	// nextPort is where automatic allocation got to, so probed's ports carry on
	// after the hops and the control links rather than colliding with them.
	nextPort int
}

// Discord describes the bot: which server, what each role grants, and where it
// runs. See cmd/proxybot and agents/control-plane.md.
type Discord struct {
	// Node runs the bot. Defaults to the primary: the first entry with a
	// whitelist, in route order. A Node that is itself such an entry becomes
	// the primary instead.
	Node  string                 `json:"node,omitempty"`
	Guild string                 `json:"guild"`
	Roles map[string]botcfg.Role `json:"roles"`
	// AuditChannel gets one line per change, when set.
	AuditChannel string `json:"audit_channel,omitempty"`
}

// Feeds are the Discord channels this chain talks to. Each one names an
// environment variable holding a webhook URL rather than the URL itself: a
// webhook URL is a bearer credential, and topology.json is the file people edit
// and paste at each other. Two feeds naming the same variable land in the same
// channel, which is the whole of "different, or the same".
//
// Who posts what is not a setting, because it follows from who can see it. A
// node knows its own sessions and no others; only probed has the measurements;
// only the bot can see the whole fleet, or see a node that has stopped
// answering at all.
type Feeds struct {
	// Sessions is posted by proxyd on every ingress: a line per login and per
	// logout, carrying that node's own online count. The fleet-wide roster is
	// Online, and is a separate feed for that reason.
	Sessions *Feed `json:"sessions,omitempty"`
	// Probe is posted by probed on every node that originates a class. A node
	// that only answers — Chicago today — has nothing to say and is not given
	// the URL.
	Probe *ProbeFeed `json:"probe,omitempty"`
	// Online is the roster: one message the bot keeps up to date in place.
	Online *OnlineFeed `json:"online,omitempty"`
	// Status is the card at the foot of the channel and the transitions above
	// it. The bot posts it because a node that is down cannot report that it is
	// down.
	Status *StatusFeed `json:"status,omitempty"`
}

// Feed is the one thing every feed needs: where to post.
type Feed struct {
	// WebhookEnv names the environment variable holding the webhook URL. It is
	// read from the operator's environment at deploy time and carried to the
	// node inside the install script over ssh stdin, the same path the bot token
	// takes, so it is never on a command line and never in /tmp.
	WebhookEnv string `json:"webhook_env"`
}

// ProbeFeed picks which windows reach the channel. probed measures and logs
// every window in the probe block whatever this says; this is only what is worth
// reading. Without it the four classes Hong Kong originates post eight messages
// a minute. Empty means the longest window configured, which is the one whose
// p99 has enough samples behind it to mean anything.
type ProbeFeed struct {
	Feed
	Windows []string `json:"windows,omitempty"`
}

// StatusFeed names the role a transition pings. Nothing else about the status
// feed is configurable: what is worth saying follows from what happened, and a
// threshold nobody set is one nobody has to keep right.
type StatusFeed struct {
	Feed
	// PingRole is a Discord role id, notified when a node changes state. Unset
	// pings nobody, which is the default: a channel that pings on every deploy
	// stops being read.
	PingRole string `json:"ping_role,omitempty"`
}

// OnlineFeed sets the order entry nodes appear in the roster. A node not named
// falls to the end in the order bot.json holds it, so a new entry shows up
// rather than disappearing.
type OnlineFeed struct {
	Feed
	Nodes []string `json:"nodes,omitempty"`
}

// named is every configured feed with the name it is configured under, for
// error messages and for the config listing.
func (f *Feeds) named() []struct {
	name string
	feed *Feed
} {
	var out []struct {
		name string
		feed *Feed
	}
	add := func(name string, fd *Feed) {
		if fd != nil {
			out = append(out, struct {
				name string
				feed *Feed
			}{name, fd})
		}
	}
	if f == nil {
		return nil
	}
	add("sessions", f.Sessions)
	if f.Probe != nil {
		add("probe", &f.Probe.Feed)
	}
	if f.Online != nil {
		add("online", &f.Online.Feed)
	}
	if f.Status != nil {
		add("status", &f.Status.Feed)
	}
	return out
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
	// Motd is a local JSON file the entry answers server-list pings with. Unlike
	// the whitelist nothing on the node ever writes it, so every deploy replaces
	// it. Empty leaves the entry on proxyd's built-in listing.
	Motd string `json:"motd,omitempty"`
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

// chain is the nodes a single-path route visits, in the order data travels. A
// route whose exit is its entry visits one node: that ingress dials the target
// itself rather than handing the stream to a hop.
func (r Route) chain() []string {
	c := append([]string{r.Entry}, r.Paths[0].Via...)
	if r.Exit != r.Entry {
		c = append(c, r.Exit)
	}
	return c
}

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
		// A route that names itself as its own exit has no hops: the ingress
		// dials the target. There is no leg, so there is no transport to pick.
		if r.Exit == "" || r.Exit != r.Entry {
			return fmt.Errorf("route %q: needs via or paths", r.Name)
		}
		if r.Transport == "udp" {
			return fmt.Errorf("route %q: entry %q is its own exit, so there is no leg to tunnel; drop the transport", r.Name, r.Entry)
		}
		r.Paths = []Path{{Via: nil}}
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
	whitelistDir   = "/var/lib/proxyd"
	sessionLogPath = whitelistDir + "/sessions.jsonl"
	whitelistPath  = whitelistDir + "/whitelist.txt"
	// motdPath sits with the config rather than the whitelist: it is operator
	// owned and proxyd only ever reads it.
	motdPath = "/etc/proxyd/motd.json"
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
	mesh := fs.String("t", trialFileName, "trial mesh file, for the trial command")
	fs.Usage = usage

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs.Parse(os.Args[2:])

	// This needs no topology, and requiring a valid one to ask what version this
	// is would be backwards.
	if cmd == "version" {
		fmt.Println(version.String())
		return
	}

	// The trial mesh has its own file and does not need the topology loaded: its
	// nodes are the ones no route uses yet, which is the whole reason it exists.
	// Requiring a valid topology to ask about them would be backwards.
	if cmd == "trial" {
		fail(trialCmd(*mesh, *repo, fs.Args()))
		return
	}

	t, err := load(*file)
	fail(err)
	cfgs, checks, err := expand(t)
	fail(err)
	var pcfgs map[string]*probe.Config
	if t.Probe != nil {
		var pchecks []check
		pcfgs, pchecks, _, err = expandProbe(t, t.nextPort)
		fail(err)
		checks = append(checks, pchecks...)
	}

	switch cmd {
	case "config":
		printConfigs(t, cfgs, pcfgs)
		printProbe(t, pcfgs)
		printFeeds(t, cfgs, pcfgs)
	case "deploy":
		printConfigs(t, cfgs, pcfgs)
		printProbe(t, pcfgs)
		printFeeds(t, cfgs, pcfgs)
		fail(deploy(t, cfgs, pcfgs, checks, *repo))
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
  deploy     build, upload and start proxyd on every node; a running proxyd
             that says "handoff ready" hands its sessions to the new one
             instead of being restarted
  status     report each node's service state, whether it can hand off,
             listening sockets and tunnel links
  uninstall  stop and remove the service, binary and config (leaves the account
             and the whitelist)
  version    print the version every binary here ships under
  whitelist  list | add <name> [uuid] | remove <name|uuid>
             on every entry that holds a whitelist, over ssh
  trial      config | deploy [node...] | status | pull | report | uninstall
             the bake-off mesh from trial.json, for legs no route uses yet.
             Separate from everything above and temporary; see agents/trial.md

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
	if t.Probe != nil {
		if t.Probe.Hz == 0 {
			t.Probe.Hz = 1
		}
		if len(t.Probe.Windows) == 0 {
			t.Probe.Windows = []string{"1m", "10m"}
		}
	}
	if t.keys, err = loadKeys(path); err != nil {
		return nil, err
	}
	if err := t.checkDiscord(); err != nil {
		return nil, err
	}
	if err := t.checkFeeds(); err != nil {
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

// whitelistedEntries lists the entry nodes that hold a whitelist, in route
// order. The first is the primary unless the bot's node is one of them.
func (t *Topology) whitelistedEntries() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range t.Routes {
		if r.Whitelist != "" && !seen[r.Entry] {
			seen[r.Entry] = true
			out = append(out, r.Entry)
		}
	}
	return out
}

// botNode is where the bot runs, and primary the entry it writes to first.
func (t *Topology) botNode() string {
	if t.Discord != nil && t.Discord.Node != "" {
		return t.Discord.Node
	}
	return t.primary()
}

func (t *Topology) primary() string {
	entries := t.whitelistedEntries()
	if t.Discord != nil {
		for _, e := range entries {
			if e == t.Discord.Node {
				return e
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return entries[0]
}

func (t *Topology) checkDiscord() error {
	d := t.Discord
	if d == nil {
		return nil
	}
	if d.Guild == "" {
		return fmt.Errorf("discord: guild is required")
	}
	if len(d.Roles) == 0 {
		return fmt.Errorf("discord: no roles: nobody could use the bot")
	}
	if len(t.whitelistedEntries()) == 0 {
		return fmt.Errorf("discord: no route has a whitelist; the bot would have nothing to manage")
	}
	if _, ok := t.Nodes[t.botNode()]; !ok {
		return fmt.Errorf("discord: unknown node %q", d.Node)
	}
	return nil
}

// whitelistSeeds maps each entry node to the local list file seeded onto it.
// motdFiles is the listing document each entry answers with, by node. Like a
// whitelist it belongs to one entry, so two routes entering the same node may not
// disagree about it.
// checkFeeds validates the shape of the feeds block, and only the shape. Whether
// the environment actually holds the URLs is deploy's question: every other
// command has to keep working in a shell where .env was never sourced.
func (t *Topology) checkFeeds() error {
	if t.Feeds == nil {
		return nil
	}
	for _, f := range t.Feeds.named() {
		if f.feed.WebhookEnv == "" {
			return fmt.Errorf("feeds.%s: webhook_env is required; delete the block to turn the feed off", f.name)
		}
	}
	// A feed nobody can post is a typo, not a preference, so say so here rather
	// than deploying a chain that is quietly missing half of what was asked for.
	if t.Feeds.Probe != nil && t.Probe == nil {
		return fmt.Errorf("feeds.probe is posted by probed, and there is no probe block to deploy it")
	}
	if t.Feeds.Online != nil && t.Discord == nil {
		return fmt.Errorf("feeds.online is posted by the bot, and there is no discord block")
	}
	if t.Feeds.Status != nil && t.Discord == nil {
		return fmt.Errorf("feeds.status is posted by the bot, and there is no discord block")
	}
	if p := t.Feeds.Probe; p != nil {
		for _, w := range p.Windows {
			if !slices.Contains(t.Probe.Windows, w) {
				return fmt.Errorf("feeds.probe.windows: %q is not one of the probe windows %v", w, t.Probe.Windows)
			}
		}
	}
	if o := t.Feeds.Online; o != nil {
		// The roster is built from the entries the bot reaches over their
		// control links, which is exactly the whitelisted ones. A relay named
		// here would be a heading no player can ever appear under.
		entries := t.whitelistedEntries()
		for _, n := range o.Nodes {
			if !slices.Contains(entries, n) {
				if _, ok := t.Nodes[n]; !ok {
					return fmt.Errorf("feeds.online.nodes: no node %q", n)
				}
				return fmt.Errorf("feeds.online.nodes: %q is not an entry with a whitelist, so no player can be online there", n)
			}
		}
	}
	return nil
}

// Fixed names the services read. The variable in topology.json is the operator's
// own; this is what it is called once it reaches the node, so a feed is off
// exactly when its variable is unset and there is nothing else to check.
const (
	envSessionsWebhook = proxy.EnvSessionsWebhook
	envProbeWebhook    = probe.EnvProbeWebhook
	envOnlineWebhook   = botcfg.EnvOnlineWebhook
	envStatusWebhook   = botcfg.EnvStatusWebhook
)

// feedEnv renders the env file for one service: the variables it should hold,
// with the URLs read from the operator's environment. An empty return means the
// service has no feed and its file should be removed, which is what makes
// deleting a block from topology.json actually turn the feed off.
//
// A named variable that is not set is an error rather than a warning. The
// alternative is deploying a chain that looks configured and posts nothing.
func feedEnv(vars map[string]*Feed) (string, error) {
	var b strings.Builder
	for _, name := range sortedFeedVars(vars) {
		f := vars[name]
		url := os.Getenv(f.WebhookEnv)
		if url == "" {
			return "", fmt.Errorf("$%s is not set: put the webhook URL in .env and `set -a; . ./.env; set +a`", f.WebhookEnv)
		}
		if !strings.HasPrefix(url, "https://") {
			return "", fmt.Errorf("$%s does not look like a webhook URL", f.WebhookEnv)
		}
		fmt.Fprintf(&b, "%s=%s\n", name, url)
	}
	return b.String(), nil
}

func sortedFeedVars(m map[string]*Feed) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionFeedNodes are the nodes that post a session line: every ingress, which
// is every node holding a listener that parses a handshake.
func sessionFeedNodes(cfgs map[string]*proxy.Config) []string {
	var out []string
	for _, name := range names(cfgs) {
		for _, l := range cfgs[name].Listeners {
			if l.Minecraft != nil {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

func (t *Topology) motdFiles() (map[string]string, error) {
	out := map[string]string{}
	for _, r := range t.Routes {
		if r.Motd == "" {
			continue
		}
		if old, ok := out[r.Entry]; ok && old != r.Motd {
			return nil, fmt.Errorf("node %q is the entry for routes with different motds (%s, %s)", r.Entry, old, r.Motd)
		}
		out[r.Entry] = r.Motd
	}
	return out, nil
}

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
	// the hops. Only loopback may connect, which is enough for proxyctl over
	// ssh, until a discord block names the bot's node; then that node may too,
	// and its way in to every entry it does not live on is checked like a hop.
	// Every ingress writes down the sessions it relayed, which is what /watch
	// reads. It holds what the journal line already holds, and is bounded.
	for _, name := range sessionFeedNodes(cfgs) {
		cfgs[name].SessionLog = sessionLogPath
		cfgs[name].IPInfo = t.IPInfo
	}
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return nil, nil, err
	}
	bot := ""
	if t.Discord != nil {
		bot = t.botNode()
	}
	for _, name := range sortedKeys(seeds) {
		c := &proxy.Control{
			Bind: bindAddr(t.Nodes[name].BindAddr, next),
			Key:  t.keys.control(name),
		}
		if bot != "" {
			c.AllowFrom = []string{t.Nodes[bot].Addr}
			if bot != name {
				checks = append(checks, check{from: bot, to: name, why: "control", port: next,
					addr: net.JoinHostPort(t.Nodes[name].Addr, strconv.Itoa(next))})
			}
		}
		cfgs[name].Control = c
		next++
	}
	t.nextPort = next
	return cfgs, checks, nil
}

// botConfig is /etc/proxyd/bot.json for the bot's node: every entry's control
// address and key, and which one is the primary.
func botConfig(t *Topology, cfgs map[string]*proxy.Config, pcfgs map[string]*probe.Config) *botcfg.Config {
	bc := &botcfg.Config{
		Guild:        t.Discord.Guild,
		Roles:        t.Discord.Roles,
		AuditChannel: t.Discord.AuditChannel,
		Primary:      t.primary(),
		OnlineNodes:  t.onlineOrder(),
	}
	if t.Feeds != nil && t.Feeds.Status != nil {
		bc.StatusPing = t.Feeds.Status.PingRole
	}
	for _, name := range t.whitelistedEntries() {
		c := cfgs[name].Control
		_, port, _ := net.SplitHostPort(c.Bind)
		bc.Entries = append(bc.Entries, botcfg.Entry{
			Node: name,
			Addr: net.JoinHostPort(t.Nodes[name].Addr, port),
			Key:  c.Key,
		})
	}
	// The health links, when the status feed asked for them. A node running
	// probed without one simply is not listed, and the bot watches proxyd there
	// and says nothing about probed.
	for _, name := range probeNodes(pcfgs) {
		h := pcfgs[name].Health
		if h == nil {
			continue
		}
		_, port, _ := net.SplitHostPort(h.Bind)
		bc.Probes = append(bc.Probes, botcfg.Probed{
			Node: name,
			Addr: net.JoinHostPort(t.Nodes[name].Addr, port),
			Key:  h.Key,
		})
	}
	return bc
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
	chain := r.chain()
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
			// Only the entry needs this: it is the only node that sees a login,
			// and the only one that has to price the whole chain from the one leg
			// it can measure.
			l.ChainLegs = chainLegs(r)
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

// chainLegs is how many tunnel legs a session crosses between the entry and the
// exit. Via lists what is in between, so a path of n intermediate nodes is n+1
// legs. Where an entry races several paths this is the longest of them, which is
// what a node is told so that it prices a raced session by its busiest leg
// rather than its cheapest.
func chainLegs(r Route) int {
	n := 0
	for _, p := range r.Paths {
		n = max(n, len(p.Via)+1)
	}
	return n
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
	if r.Motd != "" {
		m.Motd = motdPath
	}
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
		// The node's own name, which it needs only so the session feed can say
		// which entry a player arrived at.
		cfgs[name] = &proxy.Config{Name: name}
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

func printConfigs(t *Topology, cfgs map[string]*proxy.Config, pcfgs map[string]*probe.Config) {
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
	if t.Discord != nil {
		bc := botConfig(t, cfgs, pcfgs)
		var entries []string
		for _, e := range bc.Entries {
			entries = append(entries, e.Node+"="+e.Addr)
		}
		fmt.Printf("bot on %s: guild %s, primary %s, entries %s\n",
			t.botNode(), bc.Guild, bc.Primary, strings.Join(entries, " "))
	}
	fmt.Println()
}

// printFeeds says which feeds are on and which nodes post them. It never prints
// a URL: this output goes in terminals and issue reports, and a webhook URL is a
// bearer credential. The variable name is safe and is the useful half anyway —
// it is what has to be in .env.
func printFeeds(t *Topology, cfgs map[string]*proxy.Config, pcfgs map[string]*probe.Config) {
	if t.Feeds == nil {
		return
	}
	line := func(name, posts, env string, nodes []string, note string) {
		if note != "" {
			note = "  " + note
		}
		fmt.Printf("    %-9s %-9s $%-26s %s%s\n", name, posts, env, strings.Join(nodes, " "), note)
	}
	fmt.Println("feeds:")
	if f := t.Feeds.Sessions; f != nil {
		line("sessions", "proxyd", f.WebhookEnv, sessionFeedNodes(cfgs), "")
	}
	if f := t.Feeds.Probe; f != nil {
		line("probe", "probed", f.WebhookEnv, probeOriginators(pcfgs), "windows "+strings.Join(t.probeFeedWindows(), " "))
	}
	if f := t.Feeds.Online; f != nil {
		line("online", "proxybot", f.WebhookEnv, []string{t.botNode()}, "order "+strings.Join(t.onlineOrder(), " "))
	}
	if f := t.Feeds.Status; f != nil {
		note := "pings nobody"
		if f.PingRole != "" {
			note = "pings role " + f.PingRole
		}
		line("status", "proxybot", f.WebhookEnv, []string{t.botNode()}, note)
	}
	fmt.Println()
}

// probeFeedWindows is which windows reach the channel: what was asked for, or
// the longest one configured. The longest is the default because it is the only
// one whose p99 has enough samples behind it to mean anything.
func (t *Topology) probeFeedWindows() []string {
	if t.Feeds == nil || t.Feeds.Probe == nil {
		return nil
	}
	return probe.FeedWindows(t.Probe.Windows, t.Feeds.Probe.Windows)
}

// onlineOrder is the order entry nodes appear in the roster: those named, then
// every other whitelisted entry in route order, so an entry added later shows up
// at the end rather than not at all.
func (t *Topology) onlineOrder() []string {
	if t.Feeds == nil || t.Feeds.Online == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range t.Feeds.Online.Nodes {
		if !seen[n] {
			out, seen[n] = append(out, n), true
		}
	}
	for _, n := range t.whitelistedEntries() {
		if !seen[n] {
			out, seen[n] = append(out, n), true
		}
	}
	return out
}

func marshal(c *proxy.Config) []byte {
	b, _ := json.MarshalIndent(c, "", "  ")
	return append(b, '\n')
}

func deploy(t *Topology, cfgs map[string]*proxy.Config, pcfgs map[string]*probe.Config, checks []check, repo string) error {
	built := map[string]string{} // goarch -> local binary path
	roots := map[string]bool{}   // node -> already root over ssh
	seeds, err := t.whitelistSeeds()
	if err != nil {
		return err
	}
	motds, err := t.motdFiles()
	if err != nil {
		return err
	}
	// Resolved before anything is uploaded: a missing variable should stop the
	// deploy, not leave half the chain posting and half of it silent.
	sessionEnv := ""
	if t.Feeds != nil && t.Feeds.Sessions != nil {
		if sessionEnv, err = feedEnv(map[string]*Feed{envSessionsWebhook: t.Feeds.Sessions}); err != nil {
			return fmt.Errorf("feeds.sessions: %w", err)
		}
	}
	ingress := map[string]bool{}
	for _, n := range sessionFeedNodes(cfgs) {
		ingress[n] = true
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

		arch, root, err := probeHost(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		roots[name] = root
		fmt.Printf("   probe    arch=%s root=%v\n", arch, root)

		bin, ok := built[arch]
		if !ok {
			bin = filepath.Join(tmp, "proxyd-"+arch)
			if err := build(repo, "./cmd/proxyd", arch, bin); err != nil {
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
		motd := motds[name]
		if motd != "" {
			if err := scp(node.SSH, motd, "/tmp/proxyd.motd.json"); err != nil {
				return fmt.Errorf("%s: upload motd: %w", name, err)
			}
		}
		env := ""
		if ingress[name] {
			env = sessionEnv
		}
		out, err := ssh(node.SSH, installScript(t.User, root, seed != "", motd != "", env))
		if err != nil {
			return fmt.Errorf("%s: install: %w\n%s", name, err, out)
		}
		fmt.Printf("   install  %s", lastLines(out, 2))
	}
	if t.Discord != nil {
		if err := deployBot(t, cfgs, pcfgs, tmp, repo, roots); err != nil {
			return err
		}
	}
	// probed last: a leg should be carrying real traffic before anything asks it
	// how well it does that.
	if len(pcfgs) > 0 {
		if err := deployProbe(t, pcfgs, tmp, repo, roots); err != nil {
			return err
		}
	}
	return verify(t, checks, roots)
}

// deployBot installs proxybot on its node, after every proxyd, so the control
// links it will dial are already answering.
func deployBot(t *Topology, cfgs map[string]*proxy.Config, pcfgs map[string]*probe.Config, tmp, repo string, roots map[string]bool) error {
	name := t.botNode()
	node := t.Nodes[name]
	fmt.Printf("== %s (%s) bot\n", name, node.SSH)

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if strings.ContainsAny(token, "\n\r'\\") {
		return fmt.Errorf("DISCORD_BOT_TOKEN contains characters a token never has")
	}
	arch, root, err := probeHost(node.SSH)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	roots[name] = root
	bin := filepath.Join(tmp, "proxybot-"+arch)
	if err := build(repo, "./cmd/proxybot", arch, bin); err != nil {
		return fmt.Errorf("build proxybot %s: %w", arch, err)
	}
	fmt.Printf("   build    linux/%s\n", arch)
	b, _ := json.MarshalIndent(botConfig(t, cfgs, pcfgs), "", "  ")
	cfgPath := filepath.Join(tmp, "bot.json")
	if err := os.WriteFile(cfgPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := scp(node.SSH, bin, "/tmp/proxybot.new"); err != nil {
		return fmt.Errorf("%s: upload proxybot: %w", name, err)
	}
	if err := scp(node.SSH, cfgPath, "/tmp/proxybot.config.json"); err != nil {
		return fmt.Errorf("%s: upload bot config: %w", name, err)
	}
	vars := map[string]*Feed{}
	if t.Feeds != nil {
		if t.Feeds.Online != nil {
			vars[envOnlineWebhook] = &t.Feeds.Online.Feed
		}
		if t.Feeds.Status != nil {
			vars[envStatusWebhook] = &t.Feeds.Status.Feed
		}
	}
	feeds, err := feedEnv(vars)
	if err != nil {
		return fmt.Errorf("feeds: %w", err)
	}
	out, err := ssh(node.SSH, installBotScript(root, token, feeds))
	if err != nil {
		return fmt.Errorf("%s: install proxybot: %w\n%s", name, err, out)
	}
	fmt.Printf("   install  %s", lastLines(out, 2))
	return nil
}

// probeHost reports the node's architecture and whether we are already root, so the
// install script can skip sudo on infrastructure that only permits root login.
func probeHost(target string) (arch string, root bool, err error) {
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

func build(repo, pkg, arch, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, pkg)
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
//
// It replaces a running proxyd without dropping anyone when it can. A proxyd that
// says "handoff ready" was started with an fd store and knows how to use it, so
// the new binary goes in and the old process gets SIGUSR2: it hands every socket
// and session to systemd and exits, and the new one carries on from them. If the
// new one is not up within three seconds the old binary and its config go back
// and take the same store, which they can, because the new one never let go of
// it. Anything older is restarted, which ends every session — the first deploy
// onto this model is the last one that does.
func installScript(user string, root, whitelist, motd bool, feeds string) string {
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
	// The listing, unlike the list, has no writer on the node, so this one is
	// replaced every time rather than seeded once.
	if motd {
		seed += `
$SUDO install -m 0640 -o root -g "$USER" /tmp/proxyd.motd.json ` + motdPath + `
rm -f /tmp/proxyd.motd.json`
	}
	return fmt.Sprintf(`set -eu
SUDO="%s"
USER=%s
NOLOGIN=$(command -v nologin || echo /bin/false)

id -u "$USER" >/dev/null 2>&1 || \
  $SUDO useradd --system --no-create-home --shell "$NOLOGIN" "$USER"

$SUDO install -m 0755 /tmp/proxyd.new /usr/local/bin/proxyd.next
$SUDO install -d -m 0755 /etc/proxyd
[ ! -f /etc/proxyd/config.json ] || $SUDO cp -p /etc/proxyd/config.json /etc/proxyd/config.json.prev
$SUDO install -m 0640 -o root -g "$USER" /tmp/proxyd.config.json /etc/proxyd/config.json
$SUDO install -d -m 0750 -o "$USER" -g "$USER" %s
%s
%s
rm -f /tmp/proxyd.new /tmp/proxyd.config.json

$SUDO tee /etc/systemd/system/proxyd.service >/dev/null <<'UNIT'
[Unit]
Description=proxyd
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=notify
NotifyAccess=main
User=%s
EnvironmentFile=-/etc/proxyd/feeds.env
ExecStart=/usr/local/bin/proxyd -c /etc/proxyd/config.json
Restart=always
RestartSec=100ms
FileDescriptorStoreMax=8192
TimeoutStopSec=15
StateDirectory=proxyd
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable proxyd >/dev/null 2>&1

%s$SUDO systemctl is-active proxyd
`, sudo, user, whitelistDir, seed, envFile("/etc/proxyd/feeds.env", `"$USER"`, feeds), user, swapScript("/usr/local/bin", "/etc/proxyd"))
}

// swapScript puts proxyd.next in bin in place of the running proxyd: by handoff
// where the running one says it can take part in one, by restart where it
// cannot. It exits 1 if a handoff fails, after putting the old binary and the old
// config in etc back.
//
// systemd starts the new process: the unit's RestartSec is short because a
// handoff is a pause every player on the node sits through, and its start limit
// is off so that a short RestartSec cannot turn a crash loop into a unit that
// stays failed. A start asked for by hand would not help: systemd holds a unit
// waiting out RestartSec as activating, and a start just waits with it.
//
// The unit restarts the ordinary way, through a failed state, and not with
// RestartMode=direct. Direct keeps a unit that fails before READY activating
// through every restart, so no start job for it ever finishes: a deploy's
// systemctl restart waited for ever on a binary that could not start, and
// proxybot, ordered after proxyd, never started beside a crash loop. The failed
// state in between is one systemd knows it will restart from, and the fd store
// is kept through it.
func swapScript(bin, etc string) string {
	return strings.NewReplacer("@BIN@", bin, "@ETC@", etc).Replace(`
# gone OLD: OLD is no longer the main process. up OLD: another one is, and has
# said it is ready, which under Type=notify is what "active" means.
gone() {
  for _ in $(seq 1 60); do
    [ "$(systemctl show -p MainPID --value proxyd)" != "$1" ] && return 0
    sleep 0.1
  done
  return 1
}
up() {
  for _ in $(seq 1 30); do
    PID=$(systemctl show -p MainPID --value proxyd)
    if [ "$PID" != "$1" ] && [ "$PID" != 0 ] && [ "$(systemctl is-active proxyd)" = active ]; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}
restore() {
  $SUDO mv -f @BIN@/proxyd.prev @BIN@/proxyd
  [ ! -f @ETC@/config.json.prev ] || $SUDO mv -f @ETC@/config.json.prev @ETC@/config.json
}

OLD=$(systemctl show -p MainPID --value proxyd 2>/dev/null || echo 0)
if [ "$(systemctl is-active proxyd 2>/dev/null)" = active ] &&
   [ "$(systemctl show -p StatusText --value proxyd 2>/dev/null)" = "handoff ready" ]; then
  $SUDO cp -p @BIN@/proxyd @BIN@/proxyd.prev
  $SUDO mv -f @BIN@/proxyd.next @BIN@/proxyd
  $SUDO kill -USR2 "$OLD"
  # The old process lets logins under way finish first, up to two seconds of
  # the six, and relays carry on meanwhile; only then does it hand off and go.
  if ! gone "$OLD"; then
    restore
    echo "handoff: the old proxyd never handed off, and is still running"
    exit 1
  fi
  if up "$OLD"; then
    echo "handoff: sessions carried from pid $OLD to $(systemctl show -p MainPID --value proxyd)"
  else
    NEW=$(systemctl show -p MainPID --value proxyd)
    if [ "$NEW" != 0 ] && [ "$(systemctl is-active proxyd)" = active ]; then
      # Up after all, only late, and holding the sessions: leave it be.
      echo "handoff: sessions carried from pid $OLD to $NEW, late"
    else
      # A new process lets go of the store only once it is ready, and systemd
      # keeps the store for as long as it means to restart the unit, so the old
      # binary can take it all back: put it and its config in place, stop
      # whatever is starting, start again. A process that turns ready between
      # the check above and the kill below is lost with its sessions; three
      # seconds late makes that rare.
      echo "handoff: the new proxyd did not come up; putting the old one back"
      restore
      [ "$NEW" = 0 ] || $SUDO kill -KILL "$NEW" 2>/dev/null || true
      $SUDO systemctl reset-failed proxyd 2>/dev/null || true
      $SUDO systemctl start --no-block proxyd
      if up "$NEW"; then
        echo "handoff: rolled back; the previous binary carried the sessions"
      else
        echo "handoff: the previous binary did not come up either"
      fi
      $SUDO systemctl is-active proxyd || true
      exit 1
    fi
  fi
else
  $SUDO mv -f @BIN@/proxyd.next @BIN@/proxyd
  $SUDO systemctl restart proxyd
  sleep 1
fi
`)
}

// envFile writes a service's feed variables, or removes the file when there are
// none. Removing it is what makes deleting a feed from topology.json actually
// turn the feed off rather than leaving the last URL behind. The body travels
// inside this script over ssh stdin, so a webhook URL is never on a command
// line and never lands in /tmp.
func envFile(path, group, body string) string {
	if body == "" {
		return "$SUDO rm -f " + path
	}
	// Removed before it is written, because install(1) is not uniform across the
	// fleet: ty runs uutils coreutils, whose install fails to overwrite an
	// existing file from /dev/stdin where GNU's replaces it. Creating is the one
	// path both agree on. Without this a node deploys once and then refuses every
	// deploy after it, which is worse than failing the first time.
	return "$SUDO rm -f " + path + "\n" +
		"$SUDO install -m 0640 -o root -g " + group + " /dev/stdin " + path + " <<'FEEDENV'\n" + body + "FEEDENV"
}

// installBotScript installs proxybot beside proxyd. The token travels inside
// this script, over ssh's stdin, into a root-only file the unit reads: never on
// a command line, never through /tmp. Without a token in the environment an
// existing file is kept, so a redeploy does not need it.
func installBotScript(root bool, token, feeds string) string {
	sudo := "sudo -n"
	if root {
		sudo = ""
	}
	env := `[ -f /etc/proxyd/bot.env ] || {
  echo "DISCORD_BOT_TOKEN is not set and this node has no /etc/proxyd/bot.env yet: put it in .env and source it" >&2
  exit 1
}`
	if token != "" {
		env = `$SUDO install -m 0600 -o root -g root /dev/stdin /etc/proxyd/bot.env <<'ENV'
DISCORD_BOT_TOKEN=` + token + `
ENV`
	}
	return fmt.Sprintf(`set -eu
SUDO="%s"
NOLOGIN=$(command -v nologin || echo /bin/false)

id -u proxybot >/dev/null 2>&1 || \
  $SUDO useradd --system --no-create-home --shell "$NOLOGIN" proxybot

$SUDO install -m 0755 /tmp/proxybot.new /usr/local/bin/proxybot
$SUDO install -d -m 0755 /etc/proxyd
$SUDO install -m 0640 -o root -g proxybot /tmp/proxybot.config.json /etc/proxyd/bot.json
rm -f /tmp/proxybot.new /tmp/proxybot.config.json
%s
%s

$SUDO tee /etc/systemd/system/proxybot.service >/dev/null <<'UNIT'
[Unit]
Description=proxybot
After=network-online.target proxyd.service
Wants=network-online.target

[Service]
User=proxybot
EnvironmentFile=/etc/proxyd/bot.env
EnvironmentFile=-/etc/proxyd/bot-feeds.env
ExecStart=/usr/local/bin/proxybot -c /etc/proxyd/bot.json
Restart=always
RestartSec=5
StateDirectory=proxybot
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable proxybot >/dev/null 2>&1
$SUDO systemctl restart proxybot
sleep 2
$SUDO systemctl is-active proxybot
`, sudo, env, envFile("/etc/proxyd/bot-feeds.env", "proxybot", feeds))
}

func status(t *Topology) error {
	// What each binary answers when asked its own version, keyed by where it runs.
	// Collected across every service because they all ship from one tree and one
	// deploy, so any disagreement is skew. See skew below.
	running := map[string]string{}

	for _, name := range sortedNodes(t) {
		node := t.Nodes[name]
		// The newest line per tunnel link, keyed on the address after "link" rather
		// than on a field number, because journald's own prefix would shift those.
		// A restart clears the set: proxyd logs its listeners first, so links that
		// only existed under an older config do not linger in the report.
		out, _ := ssh(node.SSH, versionLine("proxyd")+`
echo "$(systemctl is-active proxyd 2>&1) $(systemctl show -p StatusText --value proxyd 2>/dev/null)"
ss -lntup 2>/dev/null | grep proxyd || echo "  (no listening sockets)"
journalctl -u proxyd -n 400 --no-pager -o cat 2>/dev/null |
  awk '/ listen /{delete last}
       / link /{for(i=1;i<=NF;i++) if($i=="link"){last[$(i+1)]=$0}}
       END{for(k in last) print last[k]}' |
  sort || true`)
		ver, rest := splitVersion(out)
		running[name+" proxyd"] = ver
		fmt.Printf("== %s (%s) %s\n%s\n", name, node.Addr, ver, indent(rest))
	}
	if t.Probe != nil {
		for _, name := range sortedNodes(t) {
			// The newest line per class, keyed on name, kind, count and window
			// together: a leg measured at two counts reports both, and so do a
			// leg and a chain that share a name.
			out, _ := ssh(t.Nodes[name].SSH, versionLine("probed")+`
systemctl is-active probed 2>&1 | grep -q '^active' || exit 0
journalctl -u probed -n 400 --no-pager -o cat 2>/dev/null |
  awk '/ probe /{for(i=1;i<=NF;i++) if($i=="probe"){last[$(i+1)" "$(i+2)" "$(i+3)" "$(i+4)]=$0}}
       END{for(k in last) print last[k]}' |
  sort || true`)
			ver, rest := splitVersion(out)
			if len(strings.TrimSpace(rest)) == 0 {
				continue
			}
			running[name+" probed"] = ver
			fmt.Printf("== %s probe %s\n%s\n", name, ver, indent(rest))
		}
	}
	if t.Discord != nil {
		name := t.botNode()
		out, _ := ssh(t.Nodes[name].SSH, versionLine("proxybot")+`
systemctl is-active proxybot 2>&1 || true
journalctl -u proxybot -n 300 --no-pager -o cat 2>/dev/null |
  grep -E '^(discord|reconcile|audit|prune):|DISCORD_BOT_TOKEN' | tail -3 || true`)
		ver, rest := splitVersion(out)
		running[name+" proxybot"] = ver
		fmt.Printf("== %s bot %s\n%s\n", name, ver, indent(rest))
	}
	skew(running)
	return nil
}

// versionLine asks an installed binary what it is, as the first line of a status
// script. Both ways of not getting an answer are answers themselves and neither
// may fail the script, because the rest of the report is still worth having: a
// binary that is absent has never been deployed, and one that rejects -version
// predates it, which is every node until the first deploy after this.
func versionLine(bin string) string {
	p := "/usr/local/bin/" + bin
	return "if [ -x " + p + " ]; then " + p +
		` -version 2>/dev/null || echo "(pre-version build)"; else echo "(not installed)"; fi`
}

// splitVersion peels that first line back off, leaving the rest of the output to
// be printed as it always was.
func splitVersion(out []byte) (ver, rest string) {
	s := string(out)
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:i]), s[i+1:]
}

// skew reports binaries that disagree about what they are. Everything here is
// built from one tree by one deploy, so they should not: a second answer means a
// node was deployed by hand, or missed by the last deploy and still carrying the
// previous build. Silent when they agree — the versions are already printed above.
func skew(running map[string]string) {
	where := map[string][]string{}
	for w, v := range running {
		where[v] = append(where[v], w)
	}
	if len(where) < 2 {
		return
	}
	vers := make([]string, 0, len(where))
	for v := range where {
		vers = append(vers, v)
	}
	sort.Strings(vers)
	fmt.Println("== version skew")
	for _, v := range vers {
		sort.Strings(where[v])
		fmt.Printf("   %-28s %s\n", v, strings.Join(where[v], ", "))
	}
	fmt.Println()
}

func uninstall(t *Topology) error {
	for _, name := range sortedNodes(t) {
		node := t.Nodes[name]
		_, root, err := probeHost(node.SSH)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		sudo := "sudo -n"
		if root {
			sudo = ""
		}
		prb := ""
		if t.Probe != nil {
			prb = uninstallProbe()
		}
		bot := ""
		if t.Discord != nil && t.botNode() == name {
			bot = `$SUDO systemctl disable --now proxybot >/dev/null 2>&1 || true
$SUDO rm -f /etc/systemd/system/proxybot.service /usr/local/bin/proxybot /etc/proxyd/bot.json /etc/proxyd/bot.env`
		}
		out, err := ssh(node.SSH, fmt.Sprintf(`set -u
SUDO="%s"
$SUDO systemctl disable --now proxyd >/dev/null 2>&1 || true
$SUDO rm -f /etc/systemd/system/proxyd.service /usr/local/bin/proxyd /usr/local/bin/proxyd.prev /usr/local/bin/proxyd.next /etc/proxyd/config.json
%s
%s
$SUDO rmdir /etc/proxyd 2>/dev/null || true
$SUDO systemctl daemon-reload
echo removed
`, sudo, bot, prb))
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
