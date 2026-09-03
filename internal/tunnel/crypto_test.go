package tunnel

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	k := NewKey()
	a, _ := newSealer(k)
	b, _ := newSealer(k)
	msg := []byte("the quick brown fox")
	got, err := b.open(nil, a.seal(nil, msg))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q want %q", got, msg)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	a, _ := newSealer(NewKey())
	b, _ := newSealer(NewKey())
	if _, err := b.open(nil, a.seal(nil, []byte("x"))); err == nil {
		t.Fatal("opened a datagram sealed with another key")
	}
}

func TestTamperIsRejected(t *testing.T) {
	k := NewKey()
	a, _ := newSealer(k)
	b, _ := newSealer(k)
	wire := a.seal(nil, []byte("payload"))
	for i := range wire {
		bad := bytes.Clone(wire)
		bad[i] ^= 0x01
		if _, err := b.open(nil, bad); err == nil {
			t.Fatalf("opened a datagram with byte %d flipped", i)
		}
	}
}

// Two copies of the same chunk must not be the same bytes on the wire, or the
// receiver's replay window would eat the second and duplication would be a no-op.
func TestCopiesDifferOnTheWire(t *testing.T) {
	k := NewKey()
	a, _ := newSealer(k)
	b, _ := newSealer(k)
	msg := []byte("duplicated")
	one, two := a.seal(nil, msg), a.seal(nil, msg)
	if bytes.Equal(one, two) {
		t.Fatal("two seals of one payload produced identical datagrams")
	}
	for _, w := range [][]byte{one, two} {
		if got, err := b.open(nil, w); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("open: %v %q", err, got)
		}
	}
}

func TestReplayIsRejected(t *testing.T) {
	k := NewKey()
	a, _ := newSealer(k)
	b, _ := newSealer(k)
	wire := a.seal(nil, []byte("open me twice"))
	if _, err := b.open(nil, wire); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := b.open(nil, wire); err != errReplay {
		t.Fatalf("second open: got %v want %v", err, errReplay)
	}
}

// Datagrams reorder on any real path; the window has to tolerate that without
// treating an out-of-order arrival as a replay.
func TestReorderWithinWindowIsAccepted(t *testing.T) {
	k := NewKey()
	a, _ := newSealer(k)
	b, _ := newSealer(k)
	var wires [][]byte
	for i := 0; i < 200; i++ {
		wires = append(wires, a.seal(nil, []byte{byte(i)}))
	}
	for i := len(wires) - 1; i >= 0; i-- { // deliver backwards
		if _, err := b.open(nil, wires[i]); err != nil {
			t.Fatalf("datagram %d rejected: %v", i, err)
		}
	}
}

func TestKeyEncoding(t *testing.T) {
	k := NewKey()
	got, err := DecodeKey(EncodeKey(k))
	if err != nil || !bytes.Equal(got, k) {
		t.Fatalf("round trip: %v %x", err, got)
	}
	if _, err := DecodeKey("short"); err == nil {
		t.Fatal("accepted a key that is not 32 bytes")
	}
}
