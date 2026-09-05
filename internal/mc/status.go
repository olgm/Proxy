package mc

import (
	"bufio"
	"bytes"
	"io"
)

// Serverbound packet IDs in the status state. Both are answered in-process: see
// agents/operational-safety.md for why a status ping must never reach the backend.
const (
	PacketStatusRequest int32 = 0x00
	PacketPing          int32 = 0x01
)

// MaxStatusPacketLen bounds a serverbound status packet. A Status Request is one
// byte on the wire and a Ping is nine; nothing legitimate comes near this.
const MaxStatusPacketLen = 256

// StatusPacket is one serverbound packet read in the status state. Body is what
// follows the ID, which is empty for a Status Request and the client's eight-byte
// payload for a Ping.
type StatusPacket struct {
	ID   int32
	Body []byte
}

// ReadStatusPacket consumes one framed packet from r.
func ReadStatusPacket(r *bufio.Reader) (*StatusPacket, error) {
	length, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, ErrMalformed
	}
	if length > MaxStatusPacketLen {
		return nil, ErrTooLarge
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	buf := bytes.NewReader(body)
	id, err := readVarInt(buf)
	if err != nil {
		return nil, ErrMalformed
	}
	rest := make([]byte, buf.Len())
	io.ReadFull(buf, rest)
	return &StatusPacket{ID: id, Body: rest}, nil
}

// EncodeStatusResponse wraps the MOTD document as clientbound Status Response.
func EncodeStatusResponse(doc string) []byte {
	var body []byte
	body = appendVarInt(body, PacketStatusRequest) // clientbound 0x00
	body = appendVarInt(body, int32(len(doc)))
	body = append(body, doc...)
	return append(appendVarInt(nil, int32(len(body))), body...)
}

// EncodePong echoes the client's ping payload back verbatim.
//
// The client times this itself — there is no field anywhere in the status exchange
// that reports a latency, so the only way to show one is to answer when it is true.
// See the ingress's chain delay in internal/proxy.
func EncodePong(payload []byte) []byte {
	var body []byte
	body = appendVarInt(body, PacketPing)
	body = append(body, payload...)
	return append(appendVarInt(nil, int32(len(body))), body...)
}
