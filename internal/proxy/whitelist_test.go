package proxy

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/mojang"
)

const notchUUID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"

// Keep the tests off the network. Real Mojang behaviour is covered in
// internal/mojang; here the point is only that the gate consults it and obeys it.
type stubMojang struct{ owners map[string]string }

func (s stubMojang) NameFor(uuid string) (string, bool) { return "", false }

func (s stubMojang) UUIDFor(name, ip string) (string, bool) {
	u, ok := s.owners[strings.ToLower(name)]
	return u, ok
}

func (s stubMojang) LookupName(name string) (string, string, error) {
	u, ok := s.owners[strings.ToLower(name)]
	if !ok {
		return "", "", mojang.ErrNoSuchPlayer
	}
	return u, name, nil
}

func (s stubMojang) LookupUUID(uuid string) (string, error) {
	for n, u := range s.owners {
		if u == uuid {
			return n, nil
		}
	}
	return "", mojang.ErrNoSuchPlayer
}

// useMojang points the listener constructor at a stub for the duration of a test.
func useMojang(t *testing.T, owners map[string]string) {
	t.Helper()
	prev := newMojang
	newMojang = func() mojangAPI { return stubMojang{owners} }
	t.Cleanup(func() { newMojang = prev })
}

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
	if _, stubbed := newMojang().(stubMojang); !stubbed {
		useMojang(t, nil)
	}
	return startNode(t, Listener{
		Upstream:  upstream,
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565, Whitelist: list},
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
		if s.h.Address != "mc.example.com" {
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
// stops a spoofer from spending the egress IP's reputation with the backend.
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

// A status ping is answered here and never forwarded, whitelist or no whitelist.
// Every client refreshing its server list, and every scanner that finds 25565 open,
// would otherwise become a status request arriving at the backend from the egress
// address. See agents/operational-safety.md.
func TestStatusPingNeverReachesTheBackend(t *testing.T) {
	backend, got := fakeBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	c := dialIngress(t, ingress, 764, mc.IntentStatus, []byte{0x01, 0x00})
	doc := readStatusResponse(t, c)

	select {
	case <-got:
		t.Fatal("a status ping opened a connection to the backend")
	case <-time.After(500 * time.Millisecond):
	}
	if doc["description"] == nil {
		t.Fatalf("ingress answered without the branding: %v", doc)
	}
}

// A pre-1.19 client whose name is not in the file: the ingress asks who owns that
// name, and admits them only when the answer is a listed UUID. This is what lets a
// renamed player back in without letting a stranger who took their old name in.
func TestOldClientAdmittedByLookup(t *testing.T) {
	useMojang(t, map[string]string{"notchnew": notchUUID})
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"))

	dialIngress(t, ingress, 47, mc.IntentLogin, loginStart("NotchNew", nil))

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a renamed player on 1.8.9 was refused")
	}
}

// The same shape, but the name resolves to someone we never listed.
func TestOldClientRefusedWhenNameBelongsToAStranger(t *testing.T) {
	useMojang(t, map[string]string{"notch": "11111111222233334444555555555555"})
	backend, got := fakeLoginBackend(t)
	ingress := whitelistIngress(t, backend, whitelistFile(t, "Steve:"+notchUUID+"\n"))

	c := dialIngress(t, ingress, 47, mc.IntentLogin, loginStart("Notch", nil))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 256)); err != nil {
		t.Fatalf("no disconnect packet: %v", err)
	}
	select {
	case <-got:
		t.Fatal("a name owned by an unlisted uuid reached the backend")
	default:
	}
}

func TestRejectsUnreadableWhitelist(t *testing.T) {
	_, err := newServer(Listener{
		Bind: ":1", Upstream: "x:1",
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", Whitelist: "/nonexistent/whitelist.txt"},
	})
	if err == nil {
		t.Fatal("a listener with an unreadable whitelist started ungated")
	}
}
