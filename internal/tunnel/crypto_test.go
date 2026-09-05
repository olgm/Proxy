package tunnel

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	k := NewKey()
	a, _ := NewSealer(k)
	b, _ := NewSealer(k)
	msg := []byte("the quick brown fox")
	got, err := b.Open(nil, a.Seal(nil, msg))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q want %q", got, msg)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	a, _ := NewSealer(NewKey())
	b, _ := NewSealer(NewKey())
	if _, err := b.Open(nil, a.Seal(nil, []byte("x"))); err == nil {
		t.Fatal("opened a datagram sealed with another key")
	}
}

func TestTamperIsRejected(t *testing.T) {
	k := NewKey()
	a, _ := NewSealer(k)
	b, _ := NewSealer(k)
	wire := a.Seal(nil, []byte("payload"))
	for i := range wire {
		bad := bytes.Clone(wire)
		bad[i] ^= 0x01
		if _, err := b.Open(nil, bad); err == nil {
			t.Fatalf("opened a datagram with byte %d flipped", i)
		}
	}
}

// Two copies of the same chunk must not be the same bytes on the wire, or the
// receiver's replay window would eat the second and duplication would be a no-op.
func TestCopiesDifferOnTheWire(t *testing.T) {
	k := NewKey()
	a, _ := NewSealer(k)
	b, _ := NewSealer(k)
	msg := []byte("duplicated")
	one, two := a.Seal(nil, msg), a.Seal(nil, msg)
	if bytes.Equal(one, two) {
		t.Fatal("two seals of one payload produced identical datagrams")
	}
	for _, w := range [][]byte{one, two} {
		if got, err := b.Open(nil, w); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("open: %v %q", err, got)
		}
	}
}

func TestReplayIsRejected(t *testing.T) {
	k := NewKey()
	a, _ := NewSealer(k)
	b, _ := NewSealer(k)
	wire := a.Seal(nil, []byte("open me twice"))
	if _, err := b.Open(nil, wire); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := b.Open(nil, wire); err != ErrReplay {
		t.Fatalf("second open: got %v want %v", err, ErrReplay)
	}
}

// Datagrams reorder on any real path; the window has to tolerate that without
// treating an out-of-order arrival as a replay.
func TestReorderWithinWindowIsAccepted(t *testing.T) {
	k := NewKey()
	a, _ := NewSealer(k)
	b, _ := NewSealer(k)
	var wires [][]byte
	for i := 0; i < 200; i++ {
		wires = append(wires, a.Seal(nil, []byte{byte(i)}))
	}
	for i := len(wires) - 1; i >= 0; i-- { // deliver backwards
		if _, err := b.Open(nil, wires[i]); err != nil {
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
