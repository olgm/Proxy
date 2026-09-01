package proxy

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
)

const notchUUID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"

var notchRaw = [16]byte{
	0x06, 0x9a, 0x79, 0xf4, 0x44, 0xe9, 0x47, 0x26,
	0xa5, 0xbe, 0xfc, 0xa9, 0x0e, 0x38, 0xaa, 0xf5,
}

func whitelistFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "whitelist.txt")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// loginStart builds a Login Start. Every length here fits in a one-byte VarInt.
func loginStart(name string, uuid []byte) []byte {
	body := []byte{0x00, byte(len(name))}
	body = append(body, name...)
	body = append(body, uuid...)
	return append([]byte{byte(len(body))}, body...)
}

type sawLogin struct {
	h     *mc.Handshake
	login []byte
}

// fakeLoginBackend parses the handshake and the Login Start behind it, so a test
// can assert the gate forwarded the login untouched.
func fakeLoginBackend(t *testing.T) (string, <-chan sawLogin) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan sawLogin, 1)
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
		_, raw, err := mc.ReadLoginStart(br, h.ProtocolVersion)
		if err != nil {
			return
		}
		ch <- sawLogin{h, raw}
	}()
	return ln.Addr().String(), ch
}

func whitelistIngress(t *testing.T, upstream, list string) string {
	t.Helper()
	return startNode(t, Listener{
		Upstream:  upstream,
		Minecraft: &Minecraft{RewriteHost: "mc.hypixel.net", RewritePort: 25565, Whitelist: list},
	})
}

func dialIngress(t *testing.T, addr string, proto int32, intent mc.Intent, login []byte) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	hs := (&mc.Handshake{
		ProtocolVersion: proto,
		Address:         "accel.example.com",
		Port:            25565,
		Intent:          intent,
	}).Encode()
	if _, err := c.Write(append(hs, login...)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWhitelistForwardsListedPlayer(t *testing.T) {
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	login := loginStart("Notch", notchRaw[:])
	dialIngress(t, ingress, 764, mc.IntentLogin, login)

	select {
	case s := <-got:
		if s.h.Address != "mc.hypixel.net" {
			t.Errorf("backend saw address %q", s.h.Address)
		}
		// The gate reads Login Start only to look at it. Changing a byte here
		// would desync the very login it is letting through.
		if !bytes.Equal(s.login, login) {
			t.Errorf("login start altered:\n got %x\nwant %x", s.login, login)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("whitelisted player never reached the backend")
	}
}

// A stranger has to be turned away before we dial. Never opening the chain is what
// stops a spoofer from spending the egress IP's reputation with Hypixel.
func TestWhitelistDeniesStrangerWithoutDialing(t *testing.T) {
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	stranger := [16]byte{0x11, 0x22, 0x33}
	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Herobrine", stranger[:]))

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no disconnect packet: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("not whitelisted")) {
		t.Errorf("disconnect reason missing: %q", buf[:n])
	}
	select {
	case <-got:
		t.Fatal("backend was dialed for a denied login")
	default:
	}
}

// Clients before 1.19 send no UUID, so the IGN is the only thing to match on.
func TestWhitelistIgnFallbackForOldClient(t *testing.T) {
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	// 47 is 1.8.9: Login Start is the name and nothing else.
	dialIngress(t, ingress, 47, mc.IntentLogin, loginStart("Notch", nil))

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a listed player on 1.8.9 was rejected")
	}
}

func TestWhitelistDeniesUnlistedOldClient(t *testing.T) {
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	c := dialIngress(t, ingress, 47, mc.IntentLogin, loginStart("Herobrine", nil))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 256)); err != nil {
		t.Fatalf("no disconnect packet: %v", err)
	}
	select {
	case <-got:
		t.Fatal("an unlisted 1.8.9 client reached the backend")
	default:
	}
}

// Status pings carry no identity to check. Gating them would only hide the MOTD
// from the people who are on the list.
func TestWhitelistLetsStatusPingThrough(t *testing.T) {
	backend, got := fakeBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	dialIngress(t, ingress, 764, mc.IntentStatus, []byte{0x01, 0x00, 0, 0, 0, 0, 0, 0})

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("status ping was blocked by the whitelist")
	}
}

func TestRejectsUnreadableWhitelist(t *testing.T) {
	_, err := newServer(Listener{
		Bind: ":1", Upstream: "x:1",
		Minecraft: &Minecraft{RewriteHost: "mc.hypixel.net", Whitelist: "/nonexistent/whitelist.txt"},
	})
	if err == nil {
		t.Fatal("a listener with an unreadable whitelist started ungated")
	}
}
