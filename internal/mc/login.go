package mc

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
)

// MaxLoginStartLen bounds the Login Start packet. A plain one is ~20 bytes; the
// headroom is for the 1.19 signed-profile block, which carries an RSA public key
// and a signature over it (~600 bytes observed).
const MaxLoginStartLen = 4096

// LoginStart is the serverbound Login Start packet (ID 0x00), the last packet a
// client sends in the clear that says anything about who it claims to be.
//
// Both fields are the client's unverified word. The real check happens between the
// client and the backend, inside encryption we cannot read; see
// agents/hypixel-protocol.md §4.
type LoginStart struct {
	Name string
	UUID [16]byte
	// HasUUID is false for every client before 1.19, and for 1.19–1.20.1 clients
	// that chose not to send one. Those logins can only be matched on Name.
	HasUUID bool
	// Trailing reports that the packet did not end where the protocol version said
	// it would. The version is the client's unverified word like everything else
	// here, so a mismatch means we read the packet by the wrong layout and whatever
	// we took for a UUID was some other field. It is dropped rather than trusted;
	// see ReadLoginStart.
	Trailing bool
}

// UUIDString renders the UUID in the canonical dashed form, or "" when the client
// sent none.
func (l *LoginStart) UUIDString() string {
	if !l.HasUUID {
		return ""
	}
	var b [36]byte
	hex.Encode(b[:8], l.UUID[0:4])
	hex.Encode(b[9:13], l.UUID[4:6])
	hex.Encode(b[14:18], l.UUID[6:8])
	hex.Encode(b[19:23], l.UUID[8:10])
	hex.Encode(b[24:], l.UUID[10:16])
	b[8], b[13], b[18], b[23] = '-', '-', '-', '-'
	return string(b[:])
}

// ReadLoginStart consumes one Login Start packet from r. It returns the parsed
// packet and the raw frame, which the caller must replay upstream byte for byte:
// we only read this packet to look at it, never to change it.
//
// The layout after the name depends on the protocol version — see
// agents/hypixel-protocol.md §2 — so the handshake's version has to be passed in.
//
// A correct branch consumes the body exactly. Bytes left over mean the version and
// the payload disagree, which a snapshot client does by construction: it reports
// 0x40000000|n, a number past every threshold here, so a snapshot of anything
// before 1.20.2 lands on the newest layout and reads a signature block as a UUID.
// Rather than hand that to the whitelist as an identity, drop it and let the name
// match. That is strictly safer — a client willing to lie about its version could
// have sent a bare name to begin with — and it keeps a version we have mismodelled
// from locking out the player behind it.
func ReadLoginStart(r *bufio.Reader, proto int32) (*LoginStart, []byte, error) {
	length, hdr, err := readVarIntRaw(r)
	if err != nil {
		return nil, nil, err
	}
	if length <= 0 {
		return nil, nil, ErrMalformed
	}
	if length > MaxLoginStartLen {
		return nil, nil, ErrTooLarge
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, nil, err
	}

	buf := bytes.NewReader(body)
	id, err := readVarInt(buf)
	if err != nil || id != 0 {
		return nil, nil, ErrMalformed
	}
	l := &LoginStart{}
	if l.Name, err = readString(buf); err != nil {
		return nil, nil, ErrMalformed
	}

	switch {
	case proto < 759:
		// Pre-1.19: the packet ends at the name. There is no UUID to read.
	case proto < 761:
		// 1.19–1.19.2: an optional signed public key, then an optional UUID.
		if err := skipSignature(buf); err != nil {
			return nil, nil, ErrMalformed
		}
		l.readOptionalUUID(buf)
	case proto < 764:
		// 1.19.3–1.20.1: optional UUID, the key block is gone.
		l.readOptionalUUID(buf)
	default:
		// 1.20.2+: always present.
		if buf.Len() < len(l.UUID) {
			return nil, nil, ErrMalformed
		}
		io.ReadFull(buf, l.UUID[:])
		l.HasUUID = true
	}
	if buf.Len() != 0 {
		l.Trailing = true
		l.UUID, l.HasUUID = [16]byte{}, false
	}
	return l, append(hdr, body...), nil
}

// readOptionalUUID consumes the trailing "has UUID" bool and the UUID behind it.
// Running out of bytes is not an error: the field is optional wherever it appears,
// and 1.19.0 clients stop before it entirely.
func (l *LoginStart) readOptionalUUID(r *bytes.Reader) {
	has, err := r.ReadByte()
	if err != nil || has == 0 || r.Len() < len(l.UUID) {
		return
	}
	io.ReadFull(r, l.UUID[:])
	l.HasUUID = true
}

// skipSignature steps over the 1.19 signed-profile block: an expiry timestamp, the
// player's public key, and Mojang's signature over it. We forward it untouched, so
// it only has to be measured, not understood.
func skipSignature(r *bytes.Reader) error {
	has, err := r.ReadByte()
	if err != nil {
		return err
	}
	if has == 0 {
		return nil
	}
	if r.Len() < 8 {
		return ErrMalformed
	}
	r.Seek(8, io.SeekCurrent)
	for i := 0; i < 2; i++ { // public key, then signature
		n, err := readVarInt(r)
		if err != nil || n < 0 || int(n) > r.Len() {
			return ErrMalformed
		}
		r.Seek(int64(n), io.SeekCurrent)
	}
	return nil
}

// EncodeLoginDisconnect builds a login-state Disconnect (0x00) carrying reason as a
// JSON chat component. The login state kept the JSON string form when 1.20.3 moved
// play and configuration to NBT, so this one encoding covers every version.
func EncodeLoginDisconnect(reason string) []byte {
	msg, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{reason})

	var body []byte
	body = appendVarInt(body, 0) // packet ID
	body = appendVarInt(body, int32(len(msg)))
	body = append(body, msg...)
	return append(appendVarInt(nil, int32(len(body))), body...)
}

// readVarIntRaw reads one VarInt and also returns the bytes it consumed, so a frame
// can be replayed upstream exactly as the client framed it.
func readVarIntRaw(r io.ByteReader) (int32, []byte, error) {
	var v uint32
	var raw []byte
	for i := 0; i < maxVarIntLen; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		raw = append(raw, b)
		v |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(v), raw, nil
		}
	}
	return 0, nil, ErrMalformed
}
