package mc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

var testUUID = [16]byte{
	0x06, 0x9a, 0x79, 0xf4, 0x44, 0xe9, 0x47, 0x26,
	0xa5, 0xbe, 0xfc, 0xa9, 0x0e, 0x38, 0xaa, 0xf5,
}

// loginFrame builds a Login Start the way a client of that era would send it.
func loginFrame(name string, tail []byte) []byte {
	var body []byte
	body = appendVarInt(body, 0) // packet ID
	body = appendVarInt(body, int32(len(name)))
	body = append(body, name...)
	body = append(body, tail...)
	return append(appendVarInt(nil, int32(len(body))), body...)
}

func readLogin(t *testing.T, b []byte, proto int32) (*LoginStart, []byte, *bufio.Reader) {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(b))
	l, raw, err := ReadLoginStart(br, proto)
	if err != nil {
		t.Fatalf("ReadLoginStart(proto %d): %v", proto, err)
	}
	return l, raw, br
}

// The UUID's presence is a function of the protocol version, so each band has to
// parse on its own terms. See agents/hypixel-protocol.md §2.
func TestReadLoginStartByProtocol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   int32
		tail    []byte
		hasUUID bool
	}{
		{"1.8.9 name only", 47, nil, false},
		{"1.18.2 name only", 758, nil, false},
		{"1.19 no key no uuid", 759, []byte{0x00}, false},
		{"1.19.1 no key, uuid", 760, append([]byte{0x00, 0x01}, testUUID[:]...), true},
		{"1.19.2 key and uuid", 760, func() []byte {
			b := []byte{0x01}                              // has signature
			b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)          // expiry timestamp
			b = appendVarInt(b, 4)                         // public key
			b = append(b, 'k', 'e', 'y', '!')              //
			b = appendVarInt(b, 3)                         // signature
			b = append(b, 's', 'i', 'g')                   //
			return append(append(b, 0x01), testUUID[:]...) // has uuid
		}(), true},
		{"1.20.1 uuid", 763, append([]byte{0x01}, testUUID[:]...), true},
		{"1.20.1 opted out", 763, []byte{0x00}, false},
		{"1.20.2 uuid mandatory", 764, testUUID[:], true},
		{"1.21 uuid mandatory", 767, testUUID[:], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := loginFrame("Notch", tc.tail)
			l, raw, _ := readLogin(t, in, tc.proto)
			if l.Name != "Notch" {
				t.Errorf("name %q", l.Name)
			}
			if l.HasUUID != tc.hasUUID {
				t.Errorf("HasUUID = %v, want %v", l.HasUUID, tc.hasUUID)
			}
			if tc.hasUUID && l.UUID != testUUID {
				t.Errorf("uuid %x want %x", l.UUID, testUUID)
			}
			// The frame goes back on the wire untouched or the login desyncs.
			if !bytes.Equal(raw, in) {
				t.Errorf("raw replay differs:\n got %x\nwant %x", raw, in)
			}
		})
	}
}

func TestUUIDString(t *testing.T) {
	l := &LoginStart{UUID: testUUID, HasUUID: true}
	if got := l.UUIDString(); got != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Fatalf("got %q", got)
	}
	// No UUID must read as absent, not as the zero UUID: the zero value is a real
	// UUID and would match a list entry.
	if got := (&LoginStart{}).UUIDString(); got != "" {
		t.Fatalf("absent uuid rendered as %q", got)
	}
}

// Bytes pipelined behind Login Start must stay readable for the caller to forward.
func TestLoginStartLeavesTrailingBytes(t *testing.T) {
	trailing := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	in := append(loginFrame("Notch", testUUID[:]), trailing...)
	_, _, br := readLogin(t, in, 764)
	got, _ := br.Peek(br.Buffered())
	if !bytes.Equal(got, trailing) {
		t.Fatalf("buffered %x want %x", got, trailing)
	}
}

func TestLoginStartMalformed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    []byte
		proto int32
	}{
		{"zero length", []byte{0x00}, 764},
		{"wrong packet id", append(appendVarInt(nil, 3), 0x09, 0x01, 0x02), 764},
		{"truncated name", append(appendVarInt(nil, 4), 0x00, 0x7F, 0x61, 0x61), 764},
		{"uuid missing where mandatory", loginFrame("Notch", nil), 764},
		{"uuid truncated where mandatory", loginFrame("Notch", testUUID[:8]), 764},
		{"signature length past end", loginFrame("Notch", []byte{
			0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0x7F,
		}), 760},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tc.in))
			if _, _, err := ReadLoginStart(br, tc.proto); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestOversizedLoginStartRejected(t *testing.T) {
	var b []byte
	b = appendVarInt(b, MaxLoginStartLen+1)
	br := bufio.NewReader(bytes.NewReader(append(b, make([]byte, 64)...)))
	if _, _, err := ReadLoginStart(br, 764); err != ErrTooLarge {
		t.Fatalf("got %v want ErrTooLarge", err)
	}
}

func TestEncodeLoginDisconnect(t *testing.T) {
	// A client parses this in the login state, so it has to frame and decode as a
	// packet 0x00 carrying one JSON string.
	b := EncodeLoginDisconnect(`no "entry"`)
	br := bytes.NewReader(b)

	length, err := readVarInt(br)
	if err != nil || int(length) != br.Len() {
		t.Fatalf("length %d, %d bytes remain (err %v)", length, br.Len(), err)
	}
	id, err := readVarInt(br)
	if err != nil || id != 0 {
		t.Fatalf("packet id %d (err %v)", id, err)
	}
	s, err := readString(br)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(s), &chat); err != nil {
		t.Fatalf("reason is not valid JSON: %v (%q)", err, s)
	}
	if chat.Text != `no "entry"` {
		t.Fatalf("text %q", chat.Text)
	}
}
