// Package tunnel carries TCP byte streams between nodes over UDP.
//
// It exists so a chain can do three things plain TCP cannot: repair a lost packet
// from the previous hop instead of from the far end, send every packet more than
// once, and send it down several paths at once. Ordering is restored only where
// the stream leaves the tunnel; nodes in between forward each datagram the moment
// it arrives, so a hole never stalls the hops behind it.
//
// The unit of transfer is a chunk: a slice of the byte stream, numbered where it
// entered the tunnel. Every node keys everything off that one number — gap
// detection, retransmission, de-duplication and ordering alike. Duplicates carry
// the same number, which is what lets a receiver tell "another copy of 101" from
// "102 arrived and 101 did not".
package tunnel

import (
	"encoding/binary"
	"errors"
)

// msgType is the first byte of every plaintext datagram.
type msgType byte

const (
	msgData  msgType = 1
	msgNack  msgType = 2
	msgAck   msgType = 3
	msgPing  msgType = 4
	msgPong  msgType = 5
	msgReset msgType = 6
	// msgHead advertises the sender's horizon on one leg: one past the highest
	// sequence it has put on the wire. Gap detection cannot see a lost tail, so
	// a sender that has gone quiet with chunks outstanding says how far it got,
	// and the receiver turns everything it has not seen below that into holes.
	msgHead msgType = 7
)

// flagFin marks the last chunk of a direction. Its payload may be empty: the
// sequence number is what makes end-of-stream arrive in order like everything else.
//
// flagRtx marks a chunk sent again after its first send: an answer to a NACK, or
// the originator's blind probe of its highest chunk. It is what lets a relay tell
// a re-send from another copy. A copy of a number it holds is dropped; a re-send
// of one is passed on, because the node behind it may be the one missing it.
const (
	flagFin byte = 1
	flagRtx byte = 2
)

// dataHeader is type + stream + seq + flags, the plaintext a chunk carries.
const dataHeader = 1 + 8 + 8 + 1

// maxNackSeqs caps one NACK datagram. A receiver with more holes than this asks
// for the rest on its next tick rather than growing the packet.
const maxNackSeqs = 64

var (
	errShort   = errors.New("tunnel: truncated datagram")
	errMsgType = errors.New("tunnel: unknown message type")
)

// packet is a decoded datagram. Its slices alias the decode buffer and stay valid
// only until the next read on that socket.
type packet struct {
	typ     msgType
	stream  uint64
	seq     uint64
	flags   byte
	payload []byte
	seqs    []uint64
	through uint64
	nonce   uint64
}

func appendData(b []byte, stream, seq uint64, flags byte, payload []byte) []byte {
	b = append(b, byte(msgData))
	b = binary.BigEndian.AppendUint64(b, stream)
	b = binary.BigEndian.AppendUint64(b, seq)
	b = append(b, flags)
	return append(b, payload...)
}

func appendNack(b []byte, stream uint64, seqs []uint64) []byte {
	b = append(b, byte(msgNack))
	b = binary.BigEndian.AppendUint64(b, stream)
	b = binary.BigEndian.AppendUint16(b, uint16(len(seqs)))
	for _, s := range seqs {
		b = binary.BigEndian.AppendUint64(b, s)
	}
	return b
}

// appendAck carries a cumulative watermark: every sequence below through has been
// handed to the far end's reader, so no node between here and there needs to keep
// it for retransmission any more.
func appendAck(b []byte, stream, through uint64) []byte {
	b = append(b, byte(msgAck))
	b = binary.BigEndian.AppendUint64(b, stream)
	return binary.BigEndian.AppendUint64(b, through)
}

func appendHead(b []byte, stream, top uint64) []byte {
	b = append(b, byte(msgHead))
	b = binary.BigEndian.AppendUint64(b, stream)
	return binary.BigEndian.AppendUint64(b, top)
}

func appendReset(b []byte, stream uint64) []byte {
	b = append(b, byte(msgReset))
	return binary.BigEndian.AppendUint64(b, stream)
}

func appendEcho(b []byte, t msgType, nonce uint64) []byte {
	b = append(b, byte(t))
	return binary.BigEndian.AppendUint64(b, nonce)
}

func decode(b []byte) (packet, error) {
	if len(b) < 1 {
		return packet{}, errShort
	}
	p := packet{typ: msgType(b[0])}
	b = b[1:]
	switch p.typ {
	case msgData:
		if len(b) < dataHeader-1 {
			return packet{}, errShort
		}
		p.stream = binary.BigEndian.Uint64(b)
		p.seq = binary.BigEndian.Uint64(b[8:])
		p.flags = b[16]
		p.payload = b[17:]
	case msgNack:
		if len(b) < 10 {
			return packet{}, errShort
		}
		p.stream = binary.BigEndian.Uint64(b)
		n := int(binary.BigEndian.Uint16(b[8:]))
		if n > maxNackSeqs || len(b) < 10+n*8 {
			return packet{}, errShort
		}
		p.seqs = make([]uint64, n)
		for i := range p.seqs {
			p.seqs[i] = binary.BigEndian.Uint64(b[10+i*8:])
		}
	case msgAck, msgHead:
		if len(b) < 16 {
			return packet{}, errShort
		}
		p.stream = binary.BigEndian.Uint64(b)
		p.through = binary.BigEndian.Uint64(b[8:])
	case msgReset:
		if len(b) < 8 {
			return packet{}, errShort
		}
		p.stream = binary.BigEndian.Uint64(b)
	case msgPing, msgPong:
		if len(b) < 8 {
			return packet{}, errShort
		}
		p.nonce = binary.BigEndian.Uint64(b)
	default:
		return packet{}, errMsgType
	}
	return p, nil
}
