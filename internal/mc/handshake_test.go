package mc

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// frame builds a handshake packet the way a client would send it.
func frame(proto int32, addr string, port uint16, intent Intent) []byte {
	h := &Handshake{ProtocolVersion: proto, Address: addr, Port: port, Intent: intent}
	return h.Encode()
}

func read(t *testing.T, b []byte) (*Handshake, *bufio.Reader) {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(b))
	h, err := ReadHandshake(br)
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}
	return h, br
}

func TestRoundTrip(t *testing.T) {
	in := frame(765, "mc.example.com", 25565, IntentLogin)
	h, _ := read(t, in)
	if h.ProtocolVersion != 765 || h.Address != "mc.example.com" || h.Port != 25565 || h.Intent != IntentLogin {
		t.Fatalf("got %+v", h)
	}
	if !bytes.Equal(h.Encode(), in) {
		t.Fatalf("re-encode differs:\n got %x\nwant %x", h.Encode(), in)
	}
}

func TestVarIntBoundary(t *testing.T) {
	// Protocol versions either side of a VarInt continuation boundary.
	for _, v := range []int32{0, 1, 127, 128, 16383, 16384, 2097151, 1 << 30} {
		h, _ := read(t, frame(v, "h", 1, IntentStatus))
		if h.ProtocolVersion != v {
			t.Fatalf("proto %d round-tripped as %d", v, h.ProtocolVersion)
		}
	}
}

func TestRewriteAddress(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "mc.example.com", "mc.hypixel.net"},
		{"fml", "mc.example.com\x00FML\x00", "mc.hypixel.net\x00FML\x00"},
		{"fml2", "mc.example.com\x00FML2\x00", "mc.hypixel.net\x00FML2\x00"},
		{"fml3", "mc.example.com\x00FML3\x00", "mc.hypixel.net\x00FML3\x00"},
		// BungeeCord forwarding: host\0clientIP\0uuid\0properties. Client-supplied
		// identity must never reach the backend.
		{"bungee", "mc.example.com\x001.2.3.4\x00abcd\x00[{\"name\":\"textures\"}]", "mc.hypixel.net"},
		{"already-correct", "mc.hypixel.net", "mc.hypixel.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handshake{Address: tc.in}
			h.RewriteAddress("mc.hypixel.net")
			if h.Address != tc.want {
				t.Fatalf("got %q want %q", h.Address, tc.want)
			}
		})
	}
}

// The client pipelines Login Start immediately after the handshake. bufio pulls it
// off the socket during the handshake read, so it must still be recoverable.
func TestPipelinedBytesSurvive(t *testing.T) {
	trailing := []byte{0x0a, 0x00, 0x08, 'n', 'o', 't', 'c', 'h'}
	in := append(frame(765, "mc.example.com", 25565, IntentLogin), trailing...)
	_, br := read(t, in)
	got, _ := br.Peek(br.Buffered())
	if !bytes.Equal(got, trailing) {
		t.Fatalf("buffered %x want %x", got, trailing)
	}
}

func TestLegacyPingRejected(t *testing.T) {
	// 0xFE would otherwise parse as a VarInt length of 126 and desync the stream.
	br := bufio.NewReader(bytes.NewReader([]byte{0xFE, 0x01}))
	if _, err := ReadHandshake(br); err != ErrLegacyPing {
		t.Fatalf("got %v want ErrLegacyPing", err)
	}
}

func TestOversizedRejected(t *testing.T) {
	// Length header claiming more than MaxHandshakeLen must be refused before we
	// allocate for it.
	var b []byte
	b = appendVarInt(b, MaxHandshakeLen+1)
	br := bufio.NewReader(bytes.NewReader(append(b, make([]byte, 64)...)))
	if _, err := ReadHandshake(br); err != ErrTooLarge {
		t.Fatalf("got %v want ErrTooLarge", err)
	}
}

func TestMalformedRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"zero length", []byte{0x00}},
		{"wrong packet id", append(appendVarInt(nil, 3), 0x09, 0x01, 0x02)},
		{"truncated body", append(appendVarInt(nil, 20), 0x00, 0x01)},
		{"varint never terminates", append(appendVarInt(nil, 8), 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)},
		{"string longer than packet", append(appendVarInt(nil, 5), 0x00, 0x00, 0x7F, 0x61, 0x61)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tc.in))
			if _, err := ReadHandshake(br); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestLongAddressWithinLimit(t *testing.T) {
	addr := "mc.example.com\x001.2.3.4\x00uuid\x00" + strings.Repeat("x", 3000)
	h, _ := read(t, frame(765, addr, 25565, IntentLogin))
	if h.Address != addr {
		t.Fatal("long address not preserved")
	}
	h.RewriteAddress("mc.hypixel.net")
	if h.Address != "mc.hypixel.net" {
		t.Fatalf("bungee payload not stripped: %q", h.Address)
	}
}
