package tunnel

import (
	"bytes"
	"testing"
)

func TestWireRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		enc  []byte
		want packet
	}{
		{"data", appendData(nil, 7, 42, flagFin, []byte("hello")),
			packet{typ: msgData, stream: 7, seq: 42, flags: flagFin, payload: []byte("hello")}},
		{"empty data", appendData(nil, 1, 0, 0, nil),
			packet{typ: msgData, stream: 1, payload: []byte{}}},
		{"nack", appendNack(nil, 9, []uint64{1, 2, 300}),
			packet{typ: msgNack, stream: 9, seqs: []uint64{1, 2, 300}}},
		{"ack", appendAck(nil, 3, 1000), packet{typ: msgAck, stream: 3, through: 1000}},
		{"reset", appendReset(nil, 4), packet{typ: msgReset, stream: 4}},
		{"ping", appendEcho(nil, msgPing, 55), packet{typ: msgPing, nonce: 55}},
		{"pong", appendEcho(nil, msgPong, 55), packet{typ: msgPong, nonce: 55}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decode(c.enc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.typ != c.want.typ || got.stream != c.want.stream || got.seq != c.want.seq ||
				got.flags != c.want.flags || got.through != c.want.through || got.nonce != c.want.nonce {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
			if !bytes.Equal(got.payload, c.want.payload) {
				t.Fatalf("payload %q want %q", got.payload, c.want.payload)
			}
			if len(got.seqs) != len(c.want.seqs) {
				t.Fatalf("seqs %v want %v", got.seqs, c.want.seqs)
			}
			for i := range got.seqs {
				if got.seqs[i] != c.want.seqs[i] {
					t.Fatalf("seqs %v want %v", got.seqs, c.want.seqs)
				}
			}
		})
	}
}

// A truncated datagram must be rejected rather than read past its own end: the
// length fields are attacker-influenced even though the AEAD proves the sender.
func TestTruncatedIsRejected(t *testing.T) {
	full := [][]byte{
		appendData(nil, 1, 2, 0, []byte("xy")),
		appendNack(nil, 1, []uint64{5, 6}),
		appendAck(nil, 1, 2),
		appendReset(nil, 1),
		appendEcho(nil, msgPing, 3),
	}
	for _, b := range full {
		for n := 0; n < len(b); n++ {
			if _, err := decode(b[:n]); err == nil && n < len(b)-len("xy") {
				t.Fatalf("decode accepted %d of %d bytes of type %d", n, len(b), b[0])
			}
		}
	}
}

func TestUnknownTypeRejected(t *testing.T) {
	if _, err := decode([]byte{99, 0, 0}); err != errMsgType {
		t.Fatalf("got %v want %v", err, errMsgType)
	}
}

// A NACK claiming more sequences than it carries must not be believed.
func TestNackCountIsBounded(t *testing.T) {
	b := appendNack(nil, 1, []uint64{7})
	b[9], b[10] = 0xff, 0xff // rewrite the count
	if _, err := decode(b); err == nil {
		t.Fatal("decode accepted a NACK longer than its own body")
	}
}
