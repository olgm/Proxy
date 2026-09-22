// Package probe measures the legs a route really runs on, continuously.
//
// It answers the one question proxyd's own link line cannot: what does a
// duplicated — and possibly raced — path cost against the same path carrying a
// single copy? So it sends two classes over every leg, one copy and the leg's
// production count, and the gap between them is what duplication buys. The same
// pair runs end to end over a multi-leg chain, where the copies are also raced
// across every path into the exit.
//
// The framing is the tunnel's, byte for byte, sealed by tunnel.Sealer under a key
// of the leg's own. That is deliberate and not incidental: a probe of a different
// size, a different protocol or a different shape is measuring a different path.
// What it is not is the tunnel's own socket — probed is a separate service, so it
// never sees contention on proxyd's socket. It measures the link, not the queue.
//
// Nothing here ever reaches the backend. A chain probe stops at the exit exactly
// as the tunnel's ECHO does, and for the same reason: one hop further is the backend.
// See agents/operational-safety.md.
package probe

import (
	"encoding/binary"
	"errors"
)

// msgType is the first byte of every plaintext datagram.
type msgType byte

const (
	// msgReq is one probe, originated by the near end of a leg or the entry of a
	// chain. A relay passes it on; the far end answers it.
	msgReq msgType = 1
	// msgResp answers one probe and carries the responder's own counters back, so
	// round-trip loss can be split into the direction that actually dropped it.
	// Without them a lost probe and a lost answer are the same number.
	msgResp msgType = 2
)

// Plaintext sizes, which are what the estimates in README.md are built from.
// A request is type + class + seq; an answer adds the responder's two counters.
const (
	reqLen  = 1 + 1 + 8
	respLen = 1 + 1 + 8 + 8 + 8
)

var (
	errShort   = errors.New("probe: truncated datagram")
	errMsgType = errors.New("probe: unknown message type")
)

// packet is a decoded datagram.
type packet struct {
	typ   msgType
	class uint8
	seq   uint64
	// recv is how many distinct sequence numbers of this class the responder has
	// accepted since it started, and high is the largest it has seen. Both are
	// counted after de-duplication, so a class carrying two copies of every probe
	// still advances recv once per probe. That is what makes the difference of two
	// of them across a window a count of probes that arrived, not of packets.
	recv, high uint64
}

func appendReq(b []byte, class uint8, seq uint64) []byte {
	b = append(b, byte(msgReq), class)
	return binary.BigEndian.AppendUint64(b, seq)
}

func appendResp(b []byte, class uint8, seq, recv, high uint64) []byte {
	b = append(b, byte(msgResp), class)
	b = binary.BigEndian.AppendUint64(b, seq)
	b = binary.BigEndian.AppendUint64(b, recv)
	return binary.BigEndian.AppendUint64(b, high)
}

func decode(b []byte) (packet, error) {
	if len(b) < 1 {
		return packet{}, errShort
	}
	p := packet{typ: msgType(b[0])}
	switch p.typ {
	case msgReq:
		if len(b) < reqLen {
			return packet{}, errShort
		}
		p.class = b[1]
		p.seq = binary.BigEndian.Uint64(b[2:])
	case msgResp:
		if len(b) < respLen {
			return packet{}, errShort
		}
		p.class = b[1]
		p.seq = binary.BigEndian.Uint64(b[2:])
		p.recv = binary.BigEndian.Uint64(b[10:])
		p.high = binary.BigEndian.Uint64(b[18:])
	default:
		return packet{}, errMsgType
	}
	return p, nil
}
