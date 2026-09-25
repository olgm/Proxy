package tunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// line is a three-node chain whose nodes a test can replace one at a time. Each
// node's options are kept, so the one that takes over is built from the same
// config as the one it replaces, the way a deploy with no config change is.
type line struct {
	entry, relay, exit          *Node
	entryOpt, relayOpt, exitOpt Options
}

func newLine(t *testing.T, drop func(int) bool) *line {
	t.Helper()
	k1, k2 := NewKey(), NewKey()
	l := &line{}
	l.exitOpt = Options{Name: "exit", Bind: local("0"), Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2}}}
	l.exit = mustNode(t, l.exitOpt)
	w2 := newWire(t, l.exit.Addr(), drop)
	l.relayOpt = Options{Name: "relay", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2}}}
	l.relay = mustNode(t, l.relayOpt)
	w1 := newWire(t, l.relay.Addr(), drop)
	l.entryOpt = Options{Name: "entry", Hops: []LinkConfig{{Addr: w1.String(), Key: k1}}}
	l.entry = mustNode(t, l.entryOpt)
	return l
}

// takeOver freezes old and starts its successor on the same sockets, the way a
// new process would get them: as duplicated descriptors, with the state through
// JSON, and the old node closed once the new one holds them.
func takeOver(t *testing.T, old *Node, opt Options, attached ...uint64) *Node {
	t.Helper()
	st, socks := old.Freeze()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var carried State
	if err := json.Unmarshal(b, &carried); err != nil {
		t.Fatal(err)
	}
	r := &Resume{State: carried, Attached: map[uint64]bool{}}
	for _, id := range attached {
		r.Attached[id] = true
	}
	r.Bound, r.Dial = dupUDP(t, socks.Bound), dupUDP(t, socks.Dial)
	old.Close()
	opt.Resume = r
	return mustNode(t, opt)
}

func dupUDP(t *testing.T, c *net.UDPConn) *net.UDPConn {
	t.Helper()
	if c == nil {
		return nil
	}
	f, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pc, err := net.FilePacketConn(f)
	if err != nil {
		t.Fatal(err)
	}
	return pc.(*net.UDPConn)
}

// writer feeds a stream a chunk at a time until told to stop, and reports what
// it managed to hand over: the part of the last chunk a halt refused comes back
// as the remainder, which is exactly what the layer above carries across.
type writer struct {
	body []byte
	sent int
	done chan error
}

func startWriter(s *Stream, body []byte, from int) *writer {
	w := &writer{body: body, sent: from, done: make(chan error, 1)}
	go func() {
		for w.sent < len(w.body) {
			end := min(w.sent+4<<10, len(w.body))
			n, err := s.Write(w.body[w.sent:end])
			w.sent += n
			if err != nil {
				w.done <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
		w.done <- s.CloseWrite()
	}()
	return w
}

// reader drains a stream into a buffer until it ends or is halted.
type reader struct {
	mu   sync.Mutex
	got  []byte
	done chan error
}

func startReader(s *Stream, into *reader) *reader {
	if into == nil {
		into = &reader{}
	}
	into.done = make(chan error, 1)
	go func() {
		buf := make([]byte, 8<<10)
		for {
			n, err := s.Read(buf)
			into.mu.Lock()
			into.got = append(into.got, buf[:n]...)
			into.mu.Unlock()
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				into.done <- err
				return
			}
		}
	}()
	return into
}

func (r *reader) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func wait(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return nil
}

// A relay can be replaced under a stream that is moving, over lossy legs, and the
// far end reads every byte in order without the stream noticing.
func TestRelayHandsOffMidStream(t *testing.T) {
	l := newLine(t, every(9))
	body := payload(2 << 20)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	w := startWriter(s, body, 0)
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(es, nil)
	waitFor(t, "the stream to be moving", func() bool { return r.len() > len(body)/4 })

	l.relay = takeOver(t, l.relay, l.relayOpt)

	if err := wait(t, w.done, "the writer"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := wait(t, r.done, "the reader"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(r.got, body) {
		t.Fatalf("read %d bytes, want %d intact", len(r.got), len(body))
	}
}

// The exit's reader is halted, the exit is replaced, and a reader on the new node
// picks up from the byte the old one stopped at.
func TestExitHandsOffMidStream(t *testing.T) {
	l := newLine(t, every(9))
	body := payload(2 << 20)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	w := startWriter(s, body, 0)
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(es, nil)
	waitFor(t, "the stream to be moving", func() bool { return r.len() > len(body)/4 })

	es.Halt()
	if err := wait(t, r.done, "the halted reader"); !errors.Is(err, ErrHalted) {
		t.Fatalf("a halted reader got %v, want %v", err, ErrHalted)
	}
	l.exit = takeOver(t, l.exit, l.exitOpt, es.ID())
	next := l.exit.Adopt(es.ID())
	if next == nil {
		t.Fatal("the stream did not survive the handoff")
	}
	startReader(next, r)

	if err := wait(t, w.done, "the writer"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := wait(t, r.done, "the reader"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(r.got, body) {
		t.Fatalf("read %d bytes, want %d intact", len(r.got), len(body))
	}
}

// The entry's writer is halted mid-write, the entry is replaced, and a writer on
// the new node carries on from exactly the byte the halt refused.
func TestEntryHandsOffMidStream(t *testing.T) {
	l := newLine(t, every(9))
	body := payload(2 << 20)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	w := startWriter(s, body, 0)
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(es, nil)
	waitFor(t, "the stream to be moving", func() bool { return r.len() > len(body)/4 })

	s.Halt()
	if err := wait(t, w.done, "the halted writer"); !errors.Is(err, ErrHalted) {
		t.Fatalf("a halted writer got %v, want %v", err, ErrHalted)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close on a halted stream: %v", err)
	}
	l.entry = takeOver(t, l.entry, l.entryOpt, s.ID())
	next := l.entry.Adopt(s.ID())
	if next == nil {
		t.Fatal("the stream did not survive the handoff")
	}
	w = startWriter(next, body, w.sent)

	if err := wait(t, w.done, "the writer"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := wait(t, r.done, "the reader"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(r.got, body) {
		t.Fatalf("read %d bytes, want %d intact", len(r.got), len(body))
	}
}

// A stream the exit had queued for Accept, but nobody had taken, is offered again
// by the node that takes over — from its first byte, since nothing read it.
func TestExitOffersAgainWhatNobodyTook(t *testing.T) {
	l := newLine(t, nil)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	s.CloseWrite()
	waitFor(t, "the exit to queue the stream", func() bool { return len(l.exit.accept) == 1 })

	l.exit = takeOver(t, l.exit, l.exitOpt)
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if es.ID() != s.ID() {
		t.Fatalf("offered stream %x, want %x", es.ID(), s.ID())
	}
	b, err := io.ReadAll(es)
	if err != nil || string(b) != "hello" {
		t.Fatalf("read %q, %v", b, err)
	}
}

// A stream the entry was carrying that nobody above claims has nobody left to
// write it, and the far end hears so rather than waiting out the repair window.
func TestEntryResetsWhatNobodyClaimed(t *testing.T) {
	l := newLine(t, nil)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(es, nil)
	waitFor(t, "the first bytes", func() bool { return r.len() == 5 })

	s.Halt()
	l.entry = takeOver(t, l.entry, l.entryOpt)
	if err := wait(t, r.done, "the reset"); !errors.Is(err, ErrReset) {
		t.Fatalf("the exit's reader got %v, want %v", err, ErrReset)
	}
}

// A frozen node sends nothing more, whatever its streams are asked to do: the
// state that would have sent it has already been written down for someone else.
func TestFrozenNodeSendsNothing(t *testing.T) {
	var after counter
	k := NewKey()
	exit := mustNode(t, Options{Name: "exit", Bind: local("0"), Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k}}})
	w := newTappedWire(t, exit.Addr(), func(toRight bool, size int) bool {
		if toRight {
			after.add()
		}
		return false
	})
	entry := mustNode(t, Options{Name: "entry", Hops: []LinkConfig{{Addr: w.String(), Key: k}}})
	s, err := entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	entry.Freeze()
	// Whatever left before the freeze may still be on its way through the wire.
	time.Sleep(200 * time.Millisecond)
	sent := after.load()
	s.Close()
	s.CloseWrite()
	s.Write([]byte("more"))
	time.Sleep(300 * time.Millisecond)
	if got := after.load(); got != sent {
		t.Fatalf("a frozen node sent %d more datagrams", got-sent)
	}
}

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A stream the exit was reading, which nobody above claims after the handoff, had
// a connection to the backend that is gone now. It must not be offered again —
// that would dial the backend for the rest of it — and a stream that had ended
// cleanly must not be reset either.
func TestExitDoesNotOfferAgainWhatItWasReading(t *testing.T) {
	l := newLine(t, nil)
	s, err := l.entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("hello"))
	s.CloseWrite()
	es, err := l.exit.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(es); err != nil || string(b) != "hello" {
		t.Fatalf("read %q, %v", b, err)
	}
	es.CloseWrite()
	es.Halt()
	es.Close() // does nothing: halted, as a relay finishing during a handoff is

	l.exit = takeOver(t, l.exit, l.exitOpt)
	offered := make(chan *Stream, 1)
	go func() {
		if st, err := l.exit.Accept(); err == nil {
			offered <- st
		}
	}()
	select {
	case <-offered:
		t.Fatal("a stream the exit had already read was offered again")
	case <-time.After(500 * time.Millisecond):
	}
	// The entry's read side sees the clean end, not a reset.
	if b, err := io.ReadAll(s); err != nil || len(b) != 0 {
		t.Fatalf("the entry read %q, %v; want a clean end", b, err)
	}
}
