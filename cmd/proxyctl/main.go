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
	Name   string   `json:"name"`
	Entry  string   `json:"entry"`
	Port   int      `json:"port"`
	Via    []string `json:"via"`
	Target Target   `json:"target"`
}

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
	check(err)
	cfgs, err := expand(t)
	check(err)

	switch cmd {
	case "config":
		printConfigs(t, cfgs)
	case "deploy":
		printConfigs(t, cfgs)
		check(deploy(t, cfgs, *repo))
	case "status":
		check(status(t))
	case "uninstall":
		check(uninstall(t))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: proxyctl <command> [-f topology.json] [-C repo]

  config     print the per-node configs this topology expands to, change nothing
  deploy     build, upload and start proxyd on every node
  status     report each node's service state and listening sockets
  uninstall  stop and remove the service, binary and config (leaves the account)
`)
}

func check(err error) {
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
	return &t, nil
}

// expand turns the operator-facing topology into one config per node. Hop ports are
// allocated here so they never have to be kept in sync by hand.
func expand(t *Topology) (map[string]*proxy.Config, error) {
	cfgs := map[string]*proxy.Config{}
	next := t.BasePort

	for _, r := range t.Routes {
		if r.Target.Addr == "" {
			return nil, fmt.Errorf("route %q: target.addr is required", r.Name)
		}
		if r.Port == 0 {
			return nil, fmt.Errorf("route %q: port is required", r.Name)
		}
		chain := append([]string{r.Entry}, r.Via...)
		seen := map[string]bool{}
		for _, n := range chain {
			if _, ok := t.Nodes[n]; !ok {
				return nil, fmt.Errorf("route %q: unknown node %q", r.Name, n)
			}
			if seen[n] {
				return nil, fmt.Errorf("route %q: node %q appears twice in the chain", r.Name, n)
			}
			seen[n] = true
		}

		// Hop 0 listens on the operator-chosen public port; the rest get allocated.
		ports := make([]int, len(chain))
		ports[0] = r.Port
		for i := 1; i < len(chain); i++ {
			ports[i] = next
			next++
		}

		for i, name := range chain {
			node := t.Nodes[name]
			l := proxy.Listener{Bind: bindAddr(node.BindAddr, ports[i])}
			if i == 0 {
				l.Minecraft = &proxy.Minecraft{
					RewriteHost: r.Target.RewriteHost,
					RewritePort: r.Target.RewritePort,
				}
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
			if cfgs[name] == nil {
				cfgs[name] = &proxy.Config{}
			}
			cfgs[name].Listeners = append(cfgs[name].Listeners, l)
		}
	}
	return cfgs, nil
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
			mode := "relay"
			if l.Minecraft != nil {
				mode = "minecraft->" + l.Minecraft.RewriteHost
			}
			allow := "any"
			if len(l.AllowFrom) > 0 {
				allow = strings.Join(l.AllowFrom, ",")
			}
			fmt.Printf("    %-22s -> %-28s %-28s allow=%s\n", l.Bind, l.Upstream, mode, allow)
		}
	}
	fmt.Println()
}

func marshal(c *proxy.Config) []byte {
	b, _ := json.MarshalIndent(c, "", "  ")
	return append(b, '\n')
}

func deploy(t *Topology, cfgs map[string]*proxy.Config, repo string) error {
	built := map[string]string{} // goarch -> local binary path
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
		out, err := ssh(node.SSH, installScript(t.User, root))
		if err != nil {
			return fmt.Errorf("%s: install: %w\n%s", name, err, out)
		}
		fmt.Printf("   install  %s", lastLines(out, 2))
	}
	return nil
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
func installScript(user string, root bool) string {
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

$SUDO install -m 0755 /tmp/proxyd.new /usr/local/bin/proxyd
$SUDO install -d -m 0755 /etc/proxyd
$SUDO install -m 0640 -o root -g "$USER" /tmp/proxyd.config.json /etc/proxyd/config.json
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
`, sudo, user, user)
}

func status(t *Topology) error {
	for _, name := range sortedNodes(t) {
		node := t.Nodes[name]
		out, _ := ssh(node.SSH, `systemctl is-active proxyd 2>&1 || true
ss -lntp 2>/dev/null | grep proxyd || echo "  (no listening sockets)"`)
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
