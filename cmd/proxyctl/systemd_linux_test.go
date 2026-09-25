package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/proxy"
)

// systemdEnv turns on the one test here that installs proxyd for real: it writes
// a unit, adds a user and restarts a service, so it runs only where that is the
// point — CI's throwaway runner, which has systemd and passwordless sudo.
const systemdEnv = "PROXY_SYSTEMD_TEST"

// The install script, the unit and proxyd together, under a real systemd: the
// first deploy restarts, the second hands a playing connection to the new
// process, and a third, whose binary cannot start, puts the old one back — which
// takes the same sessions back out of the store. The connection sees every byte
// through all three.
func TestDeployUnderRealSystemd(t *testing.T) {
	if os.Getenv(systemdEnv) == "" {
		t.Skipf("installs a systemd unit; set %s=1 on a disposable host", systemdEnv)
	}
	backend := echoLogin(t)
	const entry = "127.0.0.1:30001"
	cfg := &proxy.Config{Name: "ci", Listeners: []proxy.Listener{{Bind: entry, Upstream: backend,
		Minecraft: &proxy.Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565}}}}
	b, _ := json.Marshal(cfg)
	bin := t.TempDir() + "/proxyd"
	if err := build("../..", "./cmd/proxyd", runtime.GOARCH, bin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("bash", "-c", `sudo -n systemctl disable --now proxyd; sudo -n rm -f /etc/systemd/system/proxyd.service; sudo -n systemctl daemon-reload`).Run()
	})
	install := func(binary string) string {
		t.Helper()
		if err := copyFile(binary, "/tmp/proxyd.new"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/tmp/proxyd.config.json", b, 0o644); err != nil {
			t.Fatal(err)
		}
		out, _ := exec.Command("bash", "-c", installScript("proxyd", false, false, false, "")).CombinedOutput()
		t.Logf("install:\n%s", out)
		return string(out)
	}

	// The first install is a restart: nothing was running that could hand off.
	install(bin)
	waitFor(t, "handoff ready", func() bool { return statusText() == "handoff ready" })

	c, err := net.Dial("tcp", entry)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hs := (&mc.Handshake{ProtocolVersion: 47, Address: "x", Port: 1, Intent: mc.IntentLogin}).Encode()
	c.Write(append(hs, 0x07, 0x00, 0x05, 'N', 'o', 't', 'c', 'h'))
	echoes(t, c, "first")

	// Echo one byte at a time through the handoff and keep the longest wait: that
	// is the pause a player sits through. The unit's RestartSec is two seconds,
	// so a pause under that says the script started the new process itself.
	stop, longest, broke := make(chan struct{}), make(chan time.Duration, 1), make(chan error, 1)
	go func() {
		var worst time.Duration
		for {
			select {
			case <-stop:
				longest <- worst
				return
			default:
			}
			start := time.Now()
			if err := echo(c, "x"); err != nil {
				broke <- err
				longest <- worst
				return
			}
			worst = max(worst, time.Since(start))
			time.Sleep(10 * time.Millisecond)
		}
	}()
	out := install(bin)
	close(stop)
	pause := <-longest
	select {
	case err := <-broke:
		t.Fatalf("the connection broke during the handoff: %v", err)
	default:
	}
	if !strings.Contains(out, "handoff: sessions carried") {
		t.Fatalf("the second deploy did not hand off")
	}
	t.Logf("longest pause through the handoff: %s", pause)
	if pause > 1500*time.Millisecond {
		t.Fatalf("players waited %s through a handoff", pause)
	}
	echoes(t, c, "second")

	broken := t.TempDir() + "/broken"
	os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if out := install(broken); !strings.Contains(out, "rolled back; the previous binary carried the sessions") {
		t.Fatalf("a binary that cannot start was not rolled back")
	}
	echoes(t, c, "third")
	if got := statusText(); got != "handoff ready" {
		t.Fatalf("after the rollback the status is %q", got)
	}
	journal, _ := exec.Command("journalctl", "-u", "proxyd", "--no-pager", "-o", "cat").CombinedOutput()
	t.Logf("journal:\n%s", journal)
}

func statusText() string {
	out, _ := exec.Command("systemctl", "show", "-p", "StatusText", "--value", "proxyd").Output()
	return strings.TrimSpace(string(out))
}

func echoes(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if err := echo(c, msg); err != nil {
		t.Fatal(err)
	}
}

func echo(c net.Conn, msg string) error {
	if _, err := c.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write %q: %w", msg, err)
	}
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
		return fmt.Errorf("sent %q, got %q: %v", msg, got, err)
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// echoLogin takes a login and echoes everything after it.
func echoLogin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				h, err := mc.ReadHandshake(br)
				if err != nil {
					return
				}
				if _, _, err := mc.ReadLoginStart(br, h.ProtocolVersion); err != nil {
					return
				}
				io.Copy(c, br)
			}()
		}
	}()
	return ln.Addr().String()
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	os.Remove(to)
	return os.WriteFile(to, b, 0o755)
}
