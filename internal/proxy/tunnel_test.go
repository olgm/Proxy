package proxy

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/tunnel"
)

// startUDP runs one tunnel hop on an ephemeral port and returns the address the
// previous hop should dial.
func startUDP(t *testing.T, l Listener) string {
	t.Helper()
	l.Net = "udp"
	l.Bind = "127.0.0.1:0"
	s, err := newServer(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.tun.Close() })
	go s.serveTunnel()
	return s.tun.Addr().String()
}

func key() string { return tunnel.EncodeKey(tunnel.NewKey()) }

// The same HK -> TY -> CHI -> backend shape as the TCP test, with both inter-node
// legs over UDP and every chunk sent twice. The ingress is unchanged: players
// still arrive over TCP, and the handshake is still the only packet parsed.
func TestUDPChainRewritesAndRelays(t *testing.T) {
	backend, got := fakeBackend(t)
	k1, k2 := key(), key()

	exit := startUDP(t, Listener{
		Upstream: backend,
		Peers:    []Link{{Addr: "127.0.0.1", Key: k2, Duplicate: 2}},
	})
	relay := startUDP(t, Listener{
		Peers: []Link{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []Link{{Addr: exit, Key: k2}},
	})
	ingress := startNode(t, Listener{
		Hops:      []Link{{Addr: relay, Key: k1, Duplicate: 2}},
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565},
	})

	c, err := net.Dial("tcp", ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	hs := (&mc.Handshake{
		ProtocolVersion: 765,
		Address:         "accel.example.com",
		Port:            25565,
		Intent:          mc.IntentLogin,
	}).Encode()
	loginStart := []byte{0x0a, 0x00, 0x08, 'n', 'o', 't', 'c', 'h'}
	if _, err := c.Write(append(hs, loginStart...)); err != nil {
		t.Fatal(err)
	}

	select {
	case s := <-got:
		if s.h.Address != "mc.example.com" || s.h.Port != 25565 {
			t.Errorf("backend saw %s:%d", s.h.Address, s.h.Port)
		}
		if s.h.ProtocolVersion != 765 {
			t.Errorf("protocol version mutated: %d", s.h.ProtocolVersion)
		}
		if !bytes.Equal(s.tail, loginStart) {
			t.Errorf("pipelined login start corrupted: %x want %x", s.tail, loginStart)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("backend never received the handshake through the tunnel")
	}

	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	back := make([]byte, 8)
	if _, err := io.ReadFull(c, back); err != nil {
		t.Fatalf("reverse direction: %v", err)
	}
	if string(back) != "SERVERHI" {
		t.Fatalf("reverse direction got %q", back)
	}
}

// A single UDP leg, entry straight to exit, with no relay in between.
func TestUDPWithoutARelay(t *testing.T) {
	backend, got := fakeBackend(t)
	k := key()
	exit := startUDP(t, Listener{Upstream: backend, Peers: []Link{{Addr: "127.0.0.1", Key: k}}})
	ingress := startNode(t, Listener{
		Hops:      []Link{{Addr: exit, Key: k}},
		Minecraft: &Minecraft{RewriteHost: "mc.example.com"},
	})
	c, err := net.Dial("tcp", ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hs := (&mc.Handshake{ProtocolVersion: 47, Address: "x", Port: 1, Intent: mc.IntentLogin}).Encode()
	c.Write(append(hs, []byte{0x0a, 0x00, 0x08, 'n', 'o', 't', 'c', 'h'}...))
	select {
	case s := <-got:
		if s.h.Address != "mc.example.com" {
			t.Errorf("backend saw address %q", s.h.Address)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("backend never received the handshake")
	}
}

func TestRejectsBadTunnelConfig(t *testing.T) {
	k := key()
	cases := []struct {
		name string
		l    Listener
	}{
		{"udp without peers", Listener{Net: "udp", Bind: ":1", Upstream: "x:1"}},
		{"peers on a tcp listener", Listener{Bind: ":1", Upstream: "x:1", Peers: []Link{{Addr: "127.0.0.1", Key: k}}}},
		{"upstream and hops together", Listener{Bind: ":1", Upstream: "x:1", Hops: []Link{{Addr: "127.0.0.1:1", Key: k}}}},
		{"neither upstream nor hops", Listener{Bind: ":1"}},
		{"unknown net", Listener{Net: "sctp", Bind: ":1", Upstream: "x:1"}},
		{"minecraft on a udp listener", Listener{Net: "udp", Bind: "127.0.0.1:0", Upstream: "x:1",
			Peers: []Link{{Addr: "127.0.0.1", Key: k}}, Minecraft: &Minecraft{RewriteHost: "h"}}},
		{"key that is not a key", Listener{Bind: ":1", Hops: []Link{{Addr: "127.0.0.1:1", Key: "nope"}}}},
		{"peer addressed with a port", Listener{Net: "udp", Bind: "127.0.0.1:0", Upstream: "x:1",
			Peers: []Link{{Addr: "127.0.0.1:9000", Key: k}}}},
		{"nack clamp upside down", Listener{Bind: ":1", Hops: []Link{{Addr: "127.0.0.1:1", Key: k}},
			Tunnel: &Tunnel{NackMinMS: 100, NackMaxMS: 10}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := newServer(c.l)
			if err == nil {
				if s.tun != nil {
					s.tun.Close()
				}
				t.Fatal("accepted a config that cannot work")
			}
		})
	}
}
