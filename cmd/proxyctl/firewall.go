package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// check is one link the chain needs: either the public entry port, or one hop
// dialling the next. Reachability is tested from the side that will really dial,
// because the far end's allowlist is scoped to that source.
type check struct {
	from string // node name; empty means the operator's machine
	to   string
	addr string
	port int
	why  string
}

func (c check) String() string {
	src := "here"
	if c.from != "" {
		src = c.from
	}
	return fmt.Sprintf("%s -> %s:%d (%s)", src, c.to, c.port, c.why)
}

// verify reports which links are blocked and, when a firewall is the likely cause,
// offers to open exactly the port that link needs. It never changes a firewall
// without being told to.
func verify(t *Topology, checks []check, roots map[string]bool) error {
	var blocked []check
	for _, c := range checks {
		if reachable(t, c) {
			fmt.Printf("   verify   ok      %s\n", c)
			continue
		}
		fmt.Printf("   verify   BLOCKED %s\n", c)
		blocked = append(blocked, c)
	}
	if len(blocked) == 0 {
		return nil
	}

	fmt.Println()
	for _, c := range blocked {
		node := t.Nodes[c.to]
		state := firewallState(node.SSH)
		rule := ufwRule(t, c)

		fmt.Printf("%s is unreachable.\n", c)
		if state != "" {
			fmt.Printf("  firewall on %s: %s\n", c.to, state)
		} else {
			fmt.Printf("  no active firewall detected on %s; the cause is something else\n", c.to)
			fmt.Printf("  (provider-level filtering, wrong addr, or proxyd not listening)\n\n")
			continue
		}
		fmt.Printf("  fix: %s\n", rule)

		if !confirm(fmt.Sprintf("  open port %d on %s now?", c.port, c.to)) {
			fmt.Printf("  skipped; run the command above on %s yourself\n\n", c.to)
			continue
		}
		sudo := "sudo -n "
		if roots[c.to] {
			sudo = ""
		}
		out, err := ssh(node.SSH, sudo+rule)
		if err != nil {
			fmt.Printf("  failed: %v\n%s\n", err, out)
			continue
		}
		if reachable(t, c) {
			fmt.Printf("  opened, now reachable\n\n")
		} else {
			fmt.Printf("  rule applied but still unreachable; suspect provider-level filtering\n\n")
		}
	}
	return nil
}

func reachable(t *Topology, c check) bool {
	if c.from == "" {
		conn, err := net.DialTimeout("tcp", c.addr, 8*time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	host, port, err := net.SplitHostPort(c.addr)
	if err != nil {
		return false
	}
	// Run the probe on the node that will really dial, so the far end sees the
	// source address its allowlist expects.
	out, _ := ssh(t.Nodes[c.from].SSH, fmt.Sprintf(
		"timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%s' >/dev/null 2>&1 && echo REACHABLE || echo BLOCKED", host, port))
	return strings.Contains(string(out), "REACHABLE")
}

// firewallState returns a one-line description, or "" if nothing obviously filters.
func firewallState(target string) string {
	out, _ := ssh(target, `ufw status 2>/dev/null | head -1
iptables -S 2>/dev/null | grep -E '^-P INPUT' || true`)
	var parts []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.EqualFold(l, "Status: active"):
			parts = append(parts, "ufw active")
		case l == "-P INPUT DROP", l == "-P INPUT REJECT":
			parts = append(parts, "default-deny INPUT")
		}
	}
	return strings.Join(parts, ", ")
}

func ufwRule(t *Topology, c check) string {
	port := strconv.Itoa(c.port)
	if c.from == "" {
		return fmt.Sprintf("ufw allow %s/tcp comment 'proxyd entry'", port)
	}
	return fmt.Sprintf("ufw allow from %s to any port %s proto tcp comment 'proxyd hop from %s'",
		t.Nodes[c.from].Addr, port, c.from)
}

// confirm returns false without asking when there is no terminal, so a scripted
// deploy reports what to run rather than silently editing a firewall.
func confirm(q string) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("  (not a terminal, not prompting)")
		return false
	}
	fmt.Printf("%s [y/N] ", q)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Println("(no input)")
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
