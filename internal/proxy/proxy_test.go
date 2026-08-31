package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

type saw struct {
	h    *mc.Handshake
	tail []byte
}

// fakeBackend stands in for Hypixel: it parses one handshake, reads what the client
// pipelined behind it, then writes back so the reverse direction is exercised too.
func fakeBackend(t *testing.T) (string, <-chan saw) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan saw, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		h, err := mc.ReadHandshake(br)
		if err != nil {
			return
		}
		tail := make([]byte, 8)
		if _, err := io.ReadFull(br, tail); err != nil {
			return
		}
		ch <- saw{h, tail}
		c.Write([]byte("SERVERHI"))
	}()
	return ln.Addr().String(), ch
}

// startNode runs one listener on an ephemeral port and returns its address.
func startNode(t *testing.T, l Listener) string {
	t.Helper()
	l.Bind = "127.0.0.1:0"
	s, err := newServer(l)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", l.Bind)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.accept(ln)
	return ln.Addr().String()
}

// The full HK -> TY -> CH -> Hypixel shape, in-process.
func TestThreeHopChainRewritesAndRelays(t *testing.T) {
	backend, got := fakeBackend(t)

	egress := startNode(t, Listener{Upstream: backend, AllowFrom: []string{"127.0.0.1"}})
	relay := startNode(t, Listener{Upstream: egress, AllowFrom: []string{"127.0.0.1"}})
	ingress := startNode(t, Listener{
		Upstream:  relay,
		Minecraft: &Minecraft{RewriteHost: "mc.hypixel.net", RewritePort: 25565},
	})

	c, err := net.Dial("tcp", ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	hs := (&mc.Handshake{
		ProtocolVersion: 765,
		Address:         "accel.example.com\x00203.0.113.9\x00uuid\x00[props]",
		Port:            25565,
		Intent:          mc.IntentLogin,
	}).Encode()
	loginStart := []byte{0x0a, 0x00, 0x08, 'n', 'o', 't', 'c', 'h'}
	if _, err := c.Write(append(hs, loginStart...)); err != nil {
		t.Fatal(err)
	}

	select {
	case s := <-got:
		if s.h.Address != "mc.hypixel.net" {
			t.Errorf("backend saw address %q, want mc.hypixel.net (rewrite or bungee-strip failed)", s.h.Address)
		}
		if s.h.Port != 25565 {
			t.Errorf("backend saw port %d, want 25565", s.h.Port)
		}
		if s.h.ProtocolVersion != 765 {
			t.Errorf("protocol version mutated: %d", s.h.ProtocolVersion)
		}
		if !bytes.Equal(s.tail, loginStart) {
			t.Errorf("pipelined login start corrupted: %x want %x", s.tail, loginStart)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never received the handshake")
	}

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	back := make([]byte, 8)
	if _, err := io.ReadFull(c, back); err != nil {
		t.Fatalf("reverse direction: %v", err)
	}
	if string(back) != "SERVERHI" {
		t.Fatalf("reverse direction got %q", back)
	}
}

func TestAllowlistRejectsStranger(t *testing.T) {
	backend, got := fakeBackend(t)
	// 192.0.2.0/24 is TEST-NET-1: never us.
	relay := startNode(t, Listener{Upstream: backend, AllowFrom: []string{"192.0.2.1"}})

	c, err := net.Dial("tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("anything"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	// The connection must die without reaching the backend. Whether that surfaces
	// as EOF or RST depends on whether unread data was pending, so accept either.
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("disallowed source got a live connection")
	}
	select {
	case <-got:
		t.Fatal("backend was reached despite the allowlist")
	default:
	}
}

func TestAllowlistAcceptsCIDR(t *testing.T) {
	backend, got := fakeBackend(t)
	egress := startNode(t, Listener{Upstream: backend, AllowFrom: []string{"127.0.0.0/8"}})
	ingress := startNode(t, Listener{
		Upstream:  egress,
		Minecraft: &Minecraft{RewriteHost: "mc.hypixel.net"},
	})
	c, err := net.Dial("tcp", ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hs := (&mc.Handshake{ProtocolVersion: 765, Address: "x\x00FML2\x00", Port: 1, Intent: mc.IntentLogin}).Encode()
	c.Write(append(hs, []byte{0x0a, 0x00, 0x08, 'n', 'o', 't', 'c', 'h'}...))
	select {
	case s := <-got:
		// RewritePort omitted, so the client's original port must survive.
		if s.h.Port != 1 {
			t.Errorf("port rewritten to %d despite rewrite_port being unset", s.h.Port)
		}
		if s.h.Address != "mc.hypixel.net\x00FML2\x00" {
			t.Errorf("forge marker lost: %q", s.h.Address)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CIDR allowlist blocked a permitted source")
	}
}

func TestRejectsBadConfig(t *testing.T) {
	if _, err := newServer(Listener{Bind: ":1", Upstream: ""}); err == nil {
		t.Error("missing upstream accepted")
	}
	if _, err := newServer(Listener{Bind: ":1", Upstream: "x:1", AllowFrom: []string{"not-an-ip"}}); err == nil {
		t.Error("unparseable allow_from accepted")
	}
}
