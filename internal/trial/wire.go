package trial

import (
	"encoding/binary"
	"errors"
)

type msgType byte

const (
	msgProbe msgType = 1 // one probe, travelling away from whoever started it
	msgEcho  msgType = 2 // answers one probe, back over the leg it arrived on
)

const (
	echoLen = 1 + 8 + 8
	// probeHead is everything before the path. A probe has no single length
	// because it carries one byte for every node it has passed through.
	probeHead = 1 + 8 + 8 + 1
)

var (
	errMsgType = errors.New("trial: unknown message type")
	errShort   = errors.New("trial: datagram too short")
	errPath    = errors.New("trial: bad path length")
)

// packet is one decoded datagram.
//
// A probe carries the whole route it has taken, origin first. That is the one
// place this differs from the tunnel and from probed, where nothing identifies a
// node on the wire: here the route *is* the measurement, and a copy arriving at
// the far end with no way to say which way it came is a copy that answers
// nothing. The ids are still only data — which leg a datagram belongs to is
// decided by the key that opens it, exactly as everywhere else, so nothing here
// acts on an unopened datagram or trusts an address.
type packet struct {
	typ    msgType
	legSeq uint64
	tick   uint64
	path   []uint8
	recv   uint64
}

func appendProbe(b []byte, legSeq, tick uint64, path []uint8) []byte {
	b = append(b, byte(msgProbe))
	b = binary.BigEndian.AppendUint64(b, legSeq)
	b = binary.BigEndian.AppendUint64(b, tick)
	b = append(b, byte(len(path)))
	return append(b, path...)
}

// appendEcho answers a probe with the leg sequence it arrived under and this
// node's count of probes that have arrived over this leg. Differencing that count
// across a window is how the sender learns how many of its probes got there,
// which is what splits a round-trip loss into the direction that caused it.
func appendEcho(b []byte, legSeq, recv uint64) []byte {
	b = append(b, byte(msgEcho))
	b = binary.BigEndian.AppendUint64(b, legSeq)
	return binary.BigEndian.AppendUint64(b, recv)
}

func decode(b []byte) (packet, error) {
	if len(b) == 0 {
		return packet{}, errShort
	}
	switch msgType(b[0]) {
	case msgEcho:
		if len(b) != echoLen {
			return packet{}, errShort
		}
		return packet{
			typ:    msgEcho,
			legSeq: binary.BigEndian.Uint64(b[1:9]),
			recv:   binary.BigEndian.Uint64(b[9:17]),
		}, nil

	case msgProbe:
		if len(b) < probeHead {
			return packet{}, errShort
		}
		n := int(b[probeHead-1])
		if n == 0 || n > maxPath || len(b) != probeHead+n {
			return packet{}, errPath
		}
		return packet{
			typ:    msgProbe,
			legSeq: binary.BigEndian.Uint64(b[1:9]),
			tick:   binary.BigEndian.Uint64(b[9:17]),
			// Copied, because the read buffer is reused for the next datagram and
			// the path outlives this call: it is logged and it is forwarded.
			path: append([]uint8(nil), b[probeHead:]...),
		}, nil
	}
	return packet{}, errMsgType
}
