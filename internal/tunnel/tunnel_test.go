package tunnel

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// wire is a UDP relay between whoever dials it and one fixed address, dropping
// datagrams on demand. It lets a test lose packets on a real socket, so the code
// under test is the code that ships rather than a seam cut for the test.
type wire struct {
	conn  *net.UDPConn
	right *net.UDPAddr
	left  atomic.Pointer[net.UDPAddr]
	n     atomic.Int64
	drop  func(int) bool
}

func newWire(t *testing.T, right *net.UDPAddr, drop func(int) bool) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{conn: conn, right: right, drop: drop}
	t.Cleanup(func() { conn.Close() })
	go w.run()
	return conn.LocalAddr().(*net.UDPAddr)
}

func (w *wire) run() {
	buf := make([]byte, 65535)
	for {
		n, from, err := w.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if w.drop != nil && w.drop(int(w.n.Add(1))) {
			continue
		}
		if from.String() == w.right.String() {
			if to := w.left.Load(); to != nil {
				w.conn.WriteToUDP(buf[:n], to)
			}
			continue
		}
		w.left.Store(from)
		w.conn.WriteToUDP(buf[:n], w.right)
	}
}

func mustNode(t *testing.T, opt Options) *Node {
	t.Helper()
	n, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func local(port string) string { return "127.0.0.1:" + port }

// chain builds entry -> relay -> exit with a lossy wire on each leg.
func chain(t *testing.T, dup int, repair time.Duration, dropA, dropB func(int) bool) (entry, exit *Node) {
	t.Helper()
	k1, k2 := NewKey(), NewKey()
	exit = mustNode(t, Options{Name: "exit", Bind: local("0"), Repair: repair,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2, Dup: dup}}})
	w2 := newWire(t, exit.Addr(), dropB)
	relay := mustNode(t, Options{Name: "relay", Bind: local("0"), Repair: repair,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2}}})
	w1 := newWire(t, relay.Addr(), dropA)
	entry = mustNode(t, Options{Name: "entry", Repair: repair,
		Hops: []LinkConfig{{Addr: w1.String(), Key: k1, Dup: dup}}})
	return entry, exit
}

func payload(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

// send writes body through the tunnel and returns what came out the other end.
func send(t *testing.T, entry, exit *Node, body []byte) []byte {
	t.Helper()
	s, err := entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		s.Write(body)
		s.CloseWrite()
	}()
	got := make(chan []byte, 1)
	fail := make(chan error, 1)
	go func() {
		es, err := exit.Accept()
		if err != nil {
			fail <- err
			return
		}
		b, err := io.ReadAll(es)
		if err != nil {
			fail <- err
			return
		}
		got <- b
	}()
	select {
	case b := <-got:
		return b
	case err := <-fail:
		t.Fatalf("exit: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out")
	}
	return nil
}

func TestChainCarriesAStream(t *testing.T) {
	entry, exit := chain(t, 1, 0, nil, nil)
	body := payload(200 << 10)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
}

func every(k int) func(int) bool { return func(n int) bool { return n%k == 0 } }

// The point of the NACK: a hole is refilled from the previous node, and the
// stream that comes out is byte-for-byte what went in.
func TestLossIsRepaired(t *testing.T) {
	for _, c := range []struct {
		name         string
		dropA, dropB func(int) bool
	}{
		{"first leg", every(4), nil},
		{"second leg", nil, every(4)},
		{"both legs", every(4), every(5)},
		{"both legs, heavy", every(3), every(3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry, exit := chain(t, 1, 0, c.dropA, c.dropB)
			body := payload(200 << 10)
			if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
				t.Fatalf("got %d bytes, want %d", len(got), len(body))
			}
		})
	}
}

// Duplication has to survive the far end de-duplicating: two copies in, one
// stream out, and half the datagrams on the floor.
func TestDuplicationSurvivesHalfLoss(t *testing.T) {
	entry, exit := chain(t, 2, 0, func(n int) bool { return n%2 == 0 }, nil)
	body := payload(200 << 10)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
}

// Racing: two paths into one exit. Whether the second one works, half works or
// is a black hole, the stream arrives in order and exactly once — the exit keeps
// the first copy of each chunk and drops the rest.
func TestRaceIntoOneExit(t *testing.T) {
	for _, c := range []struct {
		name  string
		dropB func(int) bool
	}{
		{"both paths live", nil},
		{"second path lossy", every(3)},
		{"second path is a black hole", func(int) bool { return true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry, exit := race(t, c.dropB)
			body := payload(200 << 10)
			if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
				t.Fatalf("got %d bytes, want %d", len(got), len(body))
			}
		})
	}
}

func race(t *testing.T, dropB func(int) bool) (entry, exit *Node) {
	t.Helper()
	kA, kB, kA2, kB2 := NewKey(), NewKey(), NewKey(), NewKey()
	exit = mustNode(t, Options{Name: "exit", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: kA2}, {Addr: "127.0.0.1", Key: kB2}}})
	wA2 := newWire(t, exit.Addr(), nil)
	wB2 := newWire(t, exit.Addr(), nil)
	relayA := mustNode(t, Options{Name: "relayA", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: kA}},
		Hops:  []LinkConfig{{Addr: wA2.String(), Key: kA2}}})
	relayB := mustNode(t, Options{Name: "relayB", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: kB}},
		Hops:  []LinkConfig{{Addr: wB2.String(), Key: kB2}}})
	wA1 := newWire(t, relayA.Addr(), nil)
	wB1 := newWire(t, relayB.Addr(), dropB)
	entry = mustNode(t, Options{Name: "entry",
		Hops: []LinkConfig{{Addr: wA1.String(), Key: kA}, {Addr: wB1.String(), Key: kB}}})
	return entry, exit
}

// Both directions, and a half-close that does not cut off what is still coming
// back — the shape every Minecraft session needs.
func TestHalfCloseLeavesTheOtherDirectionOpen(t *testing.T) {
	entry, exit := chain(t, 1, 0, nil, nil)
	s, err := entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		es, err := exit.Accept()
		if err != nil {
			done <- err
			return
		}
		b, err := io.ReadAll(es)
		if err != nil {
			done <- err
			return
		}
		if string(b) != "hello" {
			done <- errors.New("exit read " + string(b))
			return
		}
		_, err = es.Write([]byte("and back"))
		if err == nil {
			err = es.CloseWrite()
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "and back" {
		t.Fatalf("read %q", back)
	}
}

// A hole nothing can fill has to end the stream rather than hang: the reader
// gets an error, and the connection above can be torn down.
func TestUnrepairableStreamFails(t *testing.T) {
	// Let the stream establish, then black-hole the leg into the exit.
	entry, exit := chain(t, 1, 200*time.Millisecond, nil, func(n int) bool { return n > 8 })
	s, err := entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := s.Write(payload(4 << 10)); err != nil {
				return
			}
		}
	}()
	es, err := exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, es)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnrepairable) {
			t.Fatalf("got %v want %v", err, ErrUnrepairable)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("reader hung on an unrepairable hole")
	}
}
