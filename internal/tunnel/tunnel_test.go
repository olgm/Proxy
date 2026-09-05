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
	delay time.Duration
	// tap sees every datagram that survived drop: which way it is going and how
	// big it is, which is all an observer of sealed traffic can know. It may
	// drop the datagram too.
	tap func(toRight bool, size int) (drop bool)
}

func newWire(t *testing.T, right *net.UDPAddr, drop func(int) bool) *net.UDPAddr {
	return newDelayedWire(t, right, drop, 0)
}

func newDelayedWire(t *testing.T, right *net.UDPAddr, drop func(int) bool, delay time.Duration) *net.UDPAddr {
	return startWire(t, &wire{right: right, drop: drop, delay: delay})
}

func newTappedWire(t *testing.T, right *net.UDPAddr, tap func(toRight bool, size int) bool) *net.UDPAddr {
	return startWire(t, &wire{right: right, tap: tap})
}

func startWire(t *testing.T, w *wire) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	w.conn = conn
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
		to := w.right
		if from.String() == w.right.String() {
			if to = w.left.Load(); to == nil {
				continue
			}
		} else {
			w.left.Store(from)
		}
		if w.tap != nil && w.tap(to == w.right, n) {
			continue
		}
		if w.delay == 0 {
			w.conn.WriteToUDP(buf[:n], to)
			continue
		}
		d := bytes.Clone(buf[:n])
		go func() {
			time.Sleep(w.delay)
			w.conn.WriteToUDP(d, to)
		}()
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

// chain builds entry -> relay -> exit with a lossy wire on each leg, and dup
// copies on every leg in both directions.
func chain(t *testing.T, dup int, repair time.Duration, dropA, dropB func(int) bool) (entry, exit *Node) {
	return delayedChain(t, dup, repair, 0, dropA, dropB)
}

func delayedChain(t *testing.T, dup int, repair, delay time.Duration, dropA, dropB func(int) bool) (entry, exit *Node) {
	t.Helper()
	k1, k2 := NewKey(), NewKey()
	exit = mustNode(t, Options{Name: "exit", Bind: local("0"), Repair: repair,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2, Dup: dup}}})
	w2 := newDelayedWire(t, exit.Addr(), dropB, delay)
	relay := mustNode(t, Options{Name: "relay", Bind: local("0"), Repair: repair,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1, Dup: dup}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2, Dup: dup}}})
	w1 := newDelayedWire(t, relay.Addr(), dropA, delay)
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

// A relay keeps the first copy of each number, drops the rest, and sends what it
// kept with its own count for the next leg. Counted on the wire rather than
// inferred: every full-size datagram leaving the relay is one copy of one
// chunk, so their number is the chunks times that leg's setting, whatever the
// leg before it carried.
func TestRelayDeduplicatesThenReduplicates(t *testing.T) {
	for _, c := range []struct {
		name    string
		in, out int
	}{
		{"two in, one out", 2, 1},
		{"one in, three out", 1, 3},
		{"two in, two out", 2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			var full atomic.Int64
			k1, k2 := NewKey(), NewKey()
			exit := mustNode(t, Options{Name: "exit", Bind: local("0"),
				Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2}}})
			w2 := newTappedWire(t, exit.Addr(), func(toRight bool, size int) bool {
				if toRight && size == defaultMaxDatagram {
					full.Add(1)
				}
				return false
			})
			relay := mustNode(t, Options{Name: "relay", Bind: local("0"),
				Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
				Hops:  []LinkConfig{{Addr: w2.String(), Key: k2, Dup: c.out}}})
			w1 := newWire(t, relay.Addr(), nil)
			entry := mustNode(t, Options{Name: "entry",
				Hops: []LinkConfig{{Addr: w1.String(), Key: k1, Dup: c.in}}})

			// Small enough that no socket buffer overflows on loopback: a drop
			// there would be repaired, and the repair would be counted too.
			body := payload(64 << 10)
			if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
				t.Fatalf("got %d bytes, want %d", len(got), len(body))
			}
			chunks := int64(len(body) / entry.opt.maxChunk) // full ones; the tail is short
			_, _, st := relay.down[0].stats()
			if got, want := full.Load(), chunks*int64(c.out); got != want {
				t.Fatalf("%d full datagrams left the relay, want %d chunks x %d (rtx=%d)",
					got, chunks, c.out, st.rtx)
			}
		})
	}
}

// Sizes on the wire of the two datagrams a tail test needs to pick out. With one
// copy per leg and nothing flowing back, each is the only datagram of its size
// headed toward the exit: a FIN has no payload, and an ACK, the other 16-byte
// body, travels the other way.
const (
	finSize  = nonceLen + 16 + dataHeader
	headSize = nonceLen + 16 + 1 + 16
)

// tailChain is entry -> relay -> exit, one copy per leg, with a tap on each leg
// toward the exit.
func tailChain(t *testing.T, tap1, tap2 func(size int) bool) (entry, exit *Node) {
	t.Helper()
	k1, k2 := NewKey(), NewKey()
	exit = mustNode(t, Options{Name: "exit", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2}}})
	w2 := newTappedWire(t, exit.Addr(), func(toRight bool, size int) bool { return toRight && tap2(size) })
	relay := mustNode(t, Options{Name: "relay", Bind: local("0"),
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2}}})
	w1 := newTappedWire(t, relay.Addr(), func(toRight bool, size int) bool { return toRight && tap1(size) })
	entry = mustNode(t, Options{Name: "entry",
		Hops: []LinkConfig{{Addr: w1.String(), Key: k1}}})
	return entry, exit
}

func keep(int) bool { return false }

// once drops the first datagram of the given size and nothing after it.
func once(size int) func(int) bool {
	var done atomic.Bool
	return func(n int) bool { return n == size && done.CompareAndSwap(false, true) }
}

// probes is how many blind re-sends the entry made on its one stream.
func probes(t *testing.T, entry *Node) int {
	t.Helper()
	live := entry.live()
	if len(live) != 1 {
		t.Fatalf("entry holds %d streams, want 1", len(live))
	}
	d := live[0].down
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probes
}

// Gap detection cannot see a lost tail. A sender that goes quiet with chunks
// unacknowledged advertises how far it got, so the hop after it can ask for the
// tail on that leg, within a few milliseconds and without the end-to-end probe.
// Whichever leg lost it: the entry advertises on the first, the relay on the
// second.
func TestLostTailIsFoundByTheHorizon(t *testing.T) {
	for _, c := range []struct {
		name       string
		tap1, tap2 func(int) bool
	}{
		{"on the first leg", once(finSize), keep},
		{"on the second leg", keep, once(finSize)},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry, exit := tailChain(t, c.tap1, c.tap2)
			body := payload(64 << 10)
			if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
				t.Fatalf("got %d bytes, want %d", len(got), len(body))
			}
			if n := probes(t, entry); n != 0 {
				t.Fatalf("the entry probed %d times; the horizon should have found the tail first", n)
			}
		})
	}
}

// With the horizon adverts lost too, the originator's blind re-send of its
// highest chunk is the last resort. When the leg that lost the tail is the one
// after a relay, that relay already holds the chunk, and the probe has to get
// through it anyway: dropped as one more copy, the exit's horizon would never
// reach the tail and the stream would hang until it was given up on.
func TestTailLossBehindARelayIsRepairedByTheProbe(t *testing.T) {
	dropFin := once(finSize)
	entry, exit := tailChain(t, keep, func(n int) bool { return n == headSize || dropFin(n) })
	body := payload(64 << 10)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if n := probes(t, entry); n == 0 {
		t.Fatal("nothing but the probe could have found the tail, and it never fired")
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

// Every timer turned down as far as anyone would: the chain still carries a
// stream through loss on both legs, and a lost tail is still found. This is
// what an operator tuning for latency over a light Minecraft stream will run.
func TestAggressiveTimers(t *testing.T) {
	fast := Timers{Tick: time.Millisecond, Ping: 50 * time.Millisecond,
		NackMin: 2 * time.Millisecond, NackMax: 5 * time.Millisecond,
		HeadQuiet: 2 * time.Millisecond,
		AckEvery:  2 * time.Millisecond, AckRepeat: 10 * time.Millisecond,
		ProbeMin: 20 * time.Millisecond, ProbeMax: 50 * time.Millisecond}
	k1, k2 := NewKey(), NewKey()
	dropFin := once(finSize)
	exit := mustNode(t, Options{Name: "exit", Bind: local("0"), Timers: fast,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2}}})
	w2 := newTappedWire(t, exit.Addr(), func(toRight bool, size int) bool {
		return toRight && dropFin(size)
	})
	relay := mustNode(t, Options{Name: "relay", Bind: local("0"), Timers: fast,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2}}})
	w1 := newWire(t, relay.Addr(), every(5))
	entry := mustNode(t, Options{Name: "entry", Timers: fast,
		Hops: []LinkConfig{{Addr: w1.String(), Key: k1}}})
	body := payload(200 << 10)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
}

func TestTimersThatCannotWork(t *testing.T) {
	for _, tm := range []Timers{
		{NackMin: 50 * time.Millisecond, NackMax: 10 * time.Millisecond},
		{ProbeMin: 2 * time.Second, ProbeMax: time.Second},
		{Tick: -time.Millisecond},
	} {
		_, err := New(Options{Name: "x", Timers: tm, Hops: []LinkConfig{{Addr: "127.0.0.1:1", Key: NewKey()}}})
		if err == nil {
			t.Errorf("accepted %+v", tm)
		}
	}
}

// Enough traffic to fill the window several times, over a path with enough latency
// for the window to be the thing that limits it. Full-size datagrams, in-order
// delivery across many of them, and buffers freed by acknowledgement rather than
// by age — none of which a short transfer on loopback exercises.
func TestBulkTransferOverALatentPath(t *testing.T) {
	entry, exit := delayedChain(t, 2, 0, 20*time.Millisecond, nil, every(16))
	body := payload(4 << 20)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
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

// A node that has just started has no link it can call up: a link is only up once
// a pong has come back, and the first ping is a second away. A stream that arrives
// in that window is being fed and must not be given up on.
func TestFreshStreamSurvivesLinksNotYetUp(t *testing.T) {
	entry, exit := chain(t, 1, 200*time.Millisecond, nil, nil)
	// Past the repair window, before the first ping.
	time.Sleep(500 * time.Millisecond)
	body := payload(200 << 10)
	if got := send(t, entry, exit, body); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
}

// echoChain is a three-node chain with a known one-way delay on every leg and a
// ping timer fast enough for a test to wait on.
func echoChain(t *testing.T, leg time.Duration) (entry, exit *Node) {
	t.Helper()
	timers := Timers{Ping: 50 * time.Millisecond}
	k1, k2 := NewKey(), NewKey()
	exit = mustNode(t, Options{Name: "exit", Bind: local("0"), Timers: timers,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k2}}})
	w2 := newDelayedWire(t, exit.Addr(), nil, leg)
	relay := mustNode(t, Options{Name: "relay", Bind: local("0"), Timers: timers,
		Peers: []LinkConfig{{Addr: "127.0.0.1", Key: k1}},
		Hops:  []LinkConfig{{Addr: w2.String(), Key: k2}}})
	w1 := newDelayedWire(t, relay.Addr(), nil, leg)
	entry = mustNode(t, Options{Name: "entry", Timers: timers,
		Hops: []LinkConfig{{Addr: w1.String(), Key: k1}}})
	return entry, exit
}

func waitChainRTT(t *testing.T, n *Node) time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d := n.ChainRTT(); d > 0 {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("chain rtt never measured")
	return 0
}

// The entry times the whole tunnel, not the one leg its own pings cover. Four
// delayed traversals separate it from the exit, so a measurement that came back as
// one leg's worth would be half of what it should be.
func TestChainEchoTimesTheWholeTunnel(t *testing.T) {
	const leg = 10 * time.Millisecond
	entry, exit := echoChain(t, leg)
	got := waitChainRTT(t, entry)

	if want := 4 * leg; got < want-5*time.Millisecond || got > want+80*time.Millisecond {
		t.Errorf("chain rtt %v, want about %v", got, want)
	}
	// Half of it is not enough: that would be the entry timing its own leg and
	// calling it the chain.
	if got < 2*leg+5*time.Millisecond {
		t.Errorf("chain rtt %v looks like a single leg, not the chain", got)
	}

	// The whole reason this is its own message: the exit dials the backend when a
	// stream opens, so a chain measurement that opened one would be a connection to
	// Hypixel every time. See agents/operational-safety.md.
	exit.mu.Lock()
	streams := len(exit.streams)
	exit.mu.Unlock()
	if streams != 0 {
		t.Errorf("measuring the chain opened %d stream(s) at the exit", streams)
	}
}

// A relay is not an end of the tunnel and must not answer for one, or the entry
// would be told the chain is as short as its first hop.
func TestOnlyTheExitAnswersAnEcho(t *testing.T) {
	entry, exit := echoChain(t, time.Millisecond)
	waitChainRTT(t, entry)
	if entry.originates() != true {
		t.Error("entry should originate echoes")
	}
	if exit.originates() {
		t.Error("the exit originated an echo; only the entry may")
	}
	if exit.ChainRTT() != 0 {
		t.Errorf("the exit measured a chain rtt of %v; it has no chain ahead of it", exit.ChainRTT())
	}
}
