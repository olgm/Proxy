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
	// udp changes both how the link is tested and which rule opens it. A UDP port
	// cannot be probed by connecting to it — that always succeeds — so the test is
	// whether the near side has had an answer from the far side.
	udp bool
	// unit is the service whose journal proves that. Empty means proxyd; probed
	// logs the same line for its own legs, which are separate ports and separate
	// keys and so have to be proved separately.
	unit string
}

func (c check) service() string {
	if c.unit == "" {
		return "proxyd"
	}
	return c.unit
}

func (c check) proto() string {
	if c.udp {
		return "udp"
	}
	return "tcp"
}

func (c check) String() string {
	src := "here"
	if c.from != "" {
		src = c.from
	}
	return fmt.Sprintf("%s -> %s:%d/%s (%s)", src, c.to, c.port, c.proto(), c.why)
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

// linkUp asks the near node whether its tunnel to the far node has been answered.
// proxyd logs one line per link the moment a pong comes back, and again if one
// stops coming; that log is the only honest signal, since nothing about a UDP
// socket distinguishes a filtered port from an open one.
func linkUp(t *Topology, c check) bool {
	cmd := fmt.Sprintf(
		`journalctl -u %s -n 400 --no-pager -o cat 2>/dev/null | grep -F "link %s " | tail -1`, c.service(), c.addr)
	// The far node has to be running and the two have to have exchanged a ping,
	// which takes a second; give a blocked link long enough to prove it is blocked.
	for i := 0; i < 6; i++ {
		out, _ := ssh(t.Nodes[c.from].SSH, cmd)
		if strings.Contains(string(out), " up") {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

func reachable(t *Topology, c check) bool {
	if c.udp {
		return linkUp(t, c)
	}
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
		return fmt.Sprintf("ufw allow %s/%s comment 'proxyd entry'", port, c.proto())
	}
	return fmt.Sprintf("ufw allow from %s to any port %s proto %s comment '%s %s from %s'",
		t.Nodes[c.from].Addr, port, c.proto(), c.service(), c.why, c.from)
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
