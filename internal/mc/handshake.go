// Package mc implements the minimum of the Minecraft wire protocol needed to read
// and rewrite a client's Handshake packet.
//
// The handshake is the only packet we ever parse. Everything after it is opaque:
// the client encrypts from the byte after it sends Encryption Response, and the
// compression threshold is negotiated inside that encrypted stream, so there is no
// point at which parsing could resume. See agents/hypixel-protocol.md.
package mc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

const (
	maxVarIntLen = 5

	// MaxHandshakeLen bounds the handshake packet. A plain handshake is well under
	// 300 bytes; the headroom is for BungeeCord-style forwarding, which appends skin
	// properties to the address field. We strip those, but must still read them.
	MaxHandshakeLen = 8192

	// legacyPingID marks a pre-1.7 server list ping. Its framing is unrelated to the
	// modern protocol, so a naive length parser reads 0xFE as "length 126" and
	// desyncs. We detect and reject it instead.
	legacyPingID = 0xFE
)

// Intent is the handshake's "next state" field.
type Intent int32

const (
	IntentStatus   Intent = 1
	IntentLogin    Intent = 2
	IntentTransfer Intent = 3
)

var (
	ErrLegacyPing = errors.New("mc: legacy (pre-1.7) ping not supported")
	ErrMalformed  = errors.New("mc: malformed handshake")
	ErrTooLarge   = errors.New("mc: handshake too large")
	ErrIntent     = errors.New("mc: handshake names no known next state")
)

// Handshake is the serverbound handshake packet (ID 0x00).
type Handshake struct {
	ProtocolVersion int32
	// Address is the hostname the client connected to, verbatim. It may carry
	// NUL-delimited extras: Forge markers, or BungeeCord forwarded identity.
	Address string
	Port    uint16
	Intent  Intent
}

// ReadHandshake consumes exactly one handshake packet from r. Bytes the client
// pipelined after it stay buffered in r for the caller to forward.
func ReadHandshake(r *bufio.Reader) (*Handshake, error) {
	first, err := r.Peek(1)
	if err != nil {
		return nil, err
	}
	if first[0] == legacyPingID {
		return nil, ErrLegacyPing
	}

	length, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, ErrMalformed
	}
	if length > MaxHandshakeLen {
		return nil, ErrTooLarge
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	buf := bytes.NewReader(body)

	id, err := readVarInt(buf)
	if err != nil || id != 0 {
		return nil, ErrMalformed
	}
	h := &Handshake{}
	if h.ProtocolVersion, err = readVarInt(buf); err != nil {
		return nil, ErrMalformed
	}
	if h.Address, err = readString(buf); err != nil {
		return nil, ErrMalformed
	}
	hi, err := buf.ReadByte()
	if err != nil {
		return nil, ErrMalformed
	}
	lo, err := buf.ReadByte()
	if err != nil {
		return nil, ErrMalformed
	}
	h.Port = uint16(hi)<<8 | uint16(lo)
	intent, err := readVarInt(buf)
	if err != nil {
		return nil, ErrMalformed
	}
	// Everything else in this packet we either overwrite (address, port) or pass
	// through as the client's own problem (protocol version). The intent we act on
	// and then re-emit, so an unknown one would be forwarded to the backend
	// verbatim: eight bytes from anyone at all, turned into a malformed handshake
	// arriving from our egress address. See agents/operational-safety.md.
	h.Intent = Intent(intent)
	if h.Intent < IntentStatus || h.Intent > IntentTransfer {
		return nil, ErrIntent
	}
	return h, nil
}

// RewriteAddress replaces the hostname while preserving Forge/FML markers and
// dropping BungeeCord-style forwarded identity fields.
//
// Hypixel authenticates the address the client claims to have connected to, so it
// must read as mc.hypixel.net. Forge clients append "\0FML\0" (or FML2/FML3) and
// break if it is lost. BungeeCord forwarding appends the client IP, UUID and signed
// profile properties; forwarding client-supplied identity upstream is never correct,
// so anything that is not an FML marker is discarded.
func (h *Handshake) RewriteAddress(host string) {
	parts := strings.Split(h.Address, "\x00")
	if len(parts) > 1 && strings.HasPrefix(parts[1], "FML") {
		h.Address = host + "\x00" + strings.Join(parts[1:], "\x00")
		return
	}
	h.Address = host
}

// Encode serialises the handshake back onto the wire, length-prefixed.
func (h *Handshake) Encode() []byte {
	var body []byte
	body = appendVarInt(body, 0) // packet ID
	body = appendVarInt(body, h.ProtocolVersion)
	body = appendVarInt(body, int32(len(h.Address)))
	body = append(body, h.Address...)
	body = append(body, byte(h.Port>>8), byte(h.Port))
	body = appendVarInt(body, int32(h.Intent))
	return append(appendVarInt(nil, int32(len(body))), body...)
}

// ReadVarInt reads one VarInt. Exported for tools/mcping, which parses a status
// response; proxyd itself never needs it outside this package.
func ReadVarInt(r io.ByteReader) (int32, error) { return readVarInt(r) }

func readVarInt(r io.ByteReader) (int32, error) {
	var v uint32
	for i := 0; i < maxVarIntLen; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(v), nil
		}
	}
	return 0, ErrMalformed
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || int(n) > r.Len() {
		return "", ErrMalformed
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func appendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^uint32(0x7F) == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7F|0x80))
		u >>= 7
	}
}
