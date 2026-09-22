package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

// readStatusResponse reads the clientbound Status Response and returns its document.
func readStatusResponse(t *testing.T, c net.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	if _, err := mc.ReadVarInt(br); err != nil { // frame length
		t.Fatalf("status response length: %v", err)
	}
	if id, err := mc.ReadVarInt(br); err != nil || id != 0 {
		t.Fatalf("status response id %d: %v", id, err)
	}
	n, err := mc.ReadVarInt(br)
	if err != nil {
		t.Fatalf("status document length: %v", err)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatalf("status document: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("status document is not JSON: %v", err)
	}
	return doc
}

func statusIngress(t *testing.T, upstream string) string {
	t.Helper()
	return startNode(t, Listener{
		Upstream:  upstream,
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565},
	})
}

// The client puts a red incompatible badge on any listing whose protocol is not its
// own, so the answer has to follow the client rather than the file.
func TestStatusEchoesClientProtocol(t *testing.T) {
	ingress := statusIngress(t, "127.0.0.1:1") // never dialled
	for _, proto := range []int32{47, 340, 765} {
		c := dialIngress(t, ingress, proto, mc.IntentStatus, []byte{0x01, 0x00})
		doc := readStatusResponse(t, c)
		got := doc["version"].(map[string]any)["protocol"].(float64)
		if int32(got) != proto {
			t.Errorf("client on %d was told %v", proto, got)
		}
		c.Close()
	}
}

// An unreachable upstream must not stop the listing from answering: the whole point
// of answering here is that it does not depend on the backend being up.
func TestStatusAnswersWithUpstreamDown(t *testing.T) {
	c := dialIngress(t, statusIngress(t, "127.0.0.1:1"), 765, mc.IntentStatus, []byte{0x01, 0x00})
	if doc := readStatusResponse(t, c); doc["description"] == nil {
		t.Fatalf("no description in %v", doc)
	}
}

// holdingBackend accepts and keeps every connection, so a test can have sessions
// that are still open when it asks the ingress how many there are.
func holdingBackend(t *testing.T) string {
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
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

func TestStatusReportsLivePlayerCount(t *testing.T) {
	ingress := statusIngress(t, holdingBackend(t))

	c := dialIngress(t, ingress, 765, mc.IntentStatus, []byte{0x01, 0x00})
	if n := readStatusResponse(t, c)["players"].(map[string]any)["online"].(float64); n != 0 {
		t.Fatalf("idle ingress reported %v online", n)
	}
	c.Close()

	for i := 0; i < 2; i++ {
		dialIngress(t, ingress, 765, mc.IntentLogin, loginStart("Notch", nil))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dialIngress(t, ingress, 765, mc.IntentStatus, []byte{0x01, 0x00})
		n := readStatusResponse(t, c)["players"].(map[string]any)["online"].(float64)
		c.Close()
		if n == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("two sessions open, listing reported %v online", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// delayWire is a UDP relay that holds every datagram for a fixed time, so a test
// can give a chain a latency worth reporting.
func delayWire(t *testing.T, right string, delay time.Duration) string {
	t.Helper()
	to, err := net.ResolveUDPAddr("udp", right)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		var left *net.UDPAddr
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			dst := to
			if from.String() == to.String() {
				if left == nil {
					continue
				}
				dst = left
			} else {
				left = from
			}
			d := bytes.Clone(buf[:n])
			go func() {
				time.Sleep(delay)
				conn.WriteToUDP(d, dst)
			}()
		}
	}()
	return conn.LocalAddr().String()
}

// The number a player reads off the server list is the one they will feel in game,
// so it has to cover the chain and not just its first hop. Nothing in the status
// exchange carries a latency, so the only way to report one is to answer that late.
func TestPongIsHeldForTheChainLatency(t *testing.T) {
	const leg = 25 * time.Millisecond
	k := key()
	exit := startUDP(t, Listener{
		Upstream: holdingBackend(t),
		Peers:    []Link{{Addr: "127.0.0.1", Key: k}},
		Tunnel:   &Tunnel{PingMS: 50},
	})
	ingress := startNode(t, Listener{
		Hops:      []Link{{Addr: delayWire(t, exit, leg), Key: k}},
		Tunnel:    &Tunnel{PingMS: 50},
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565},
	})

	c := dialIngress(t, ingress, 765, mc.IntentStatus, []byte{0x01, 0x00})
	readStatusResponse(t, c)

	// Wait for the chain to have measured itself, then time the pong.
	payload := []byte{0x09, 0x01, 1, 2, 3, 4, 5, 6, 7, 8}
	deadline := time.Now().Add(10 * time.Second)
	for {
		start := time.Now()
		if _, err := c.Write(payload); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("no pong: %v", err)
		}
		held := time.Since(start)
		if !bytes.Equal(got, payload) {
			t.Fatalf("pong %x does not echo the ping %x", got, payload)
		}
		if held >= 2*leg-5*time.Millisecond {
			return // held for about the chain round trip, which is 2 legs
		}
		if time.Now().After(deadline) {
			t.Fatalf("pong came back in %v; the chain alone is about %v", held, 2*leg)
		}
		c.Close()
		c = dialIngress(t, ingress, 765, mc.IntentStatus, []byte{0x01, 0x00})
		readStatusResponse(t, c)
		time.Sleep(50 * time.Millisecond)
	}
}

// A plain TCP chain measures nothing, so there is nothing to wait for.
func TestPongIsImmediateWithoutATunnel(t *testing.T) {
	c := dialIngress(t, statusIngress(t, "127.0.0.1:1"), 765, mc.IntentStatus, []byte{0x01, 0x00})
	readStatusResponse(t, c)

	payload := []byte{0x09, 0x01, 8, 7, 6, 5, 4, 3, 2, 1}
	start := time.Now()
	c.Write(payload)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("no pong: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("pong %x does not echo %x", got, payload)
	}
	if held := time.Since(start); held > 500*time.Millisecond {
		t.Errorf("pong held for %v with no chain to wait for", held)
	}
}
