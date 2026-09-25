package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/handoff"
	"github.com/olgm/proxy/internal/mc"
)

// echoBackend reads a login and then echoes everything after it, and counts the
// connections it was given: a handoff that made the exit dial again would show
// up here as a second one.
func echoBackend(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				h, err := mc.ReadHandshake(br)
				if err != nil {
					return
				}
				if _, _, err := mc.ReadLoginStart(br, h.ProtocolVersion); err != nil {
					return
				}
				io.Copy(c, br)
				c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return ln.Addr().String(), &n
}

// handOver does what systemd and a new process would: takes n's snapshot and a
// copy of each of its descriptors, and builds the node that carries on from them.
func handOver(t *testing.T, n *node, cfg *Config) *node {
	t.Helper()
	var got *handoff.Inherited
	keep := func(snap []byte, files []handoff.File) error {
		var dup []handoff.File
		for _, f := range files {
			dup = append(dup, handoff.File{Name: f.Name, File: dupFile(t, f.File)})
		}
		got = handoff.NewInherited(snap, dup)
		return nil
	}
	if err := n.handoff(keep); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	next, resumes, err := build(cfg, readInherited(got))
	if err != nil {
		t.Fatalf("taking over: %v", err)
	}
	next.serve(resumes)
	return next
}

func dupFile(t *testing.T, f *os.File) *os.File {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var nfd int
	var derr error
	if err := rc.Control(func(fd uintptr) { nfd, derr = syscall.Dup(int(fd)) }); err != nil || derr != nil {
		t.Fatal(err, derr)
	}
	return os.NewFile(uintptr(nfd), f.Name())
}

// pattern is the byte a client sends at offset i, so a reader can check every
// byte it gets back without keeping what was sent.
func pattern(i int64) byte { return byte((i * 2654435761) >> 13) }

// player keeps a steady stream going through a login and checks every byte that
// comes back, in order.
type player struct {
	c        net.Conn
	sent     atomic.Int64
	got      atomic.Int64
	stop     chan struct{}
	wrote    chan error
	read     chan error
	stopOnce sync.Once
}

func play(t *testing.T, addr string) *player {
	t.Helper()
	p := &player{
		c:    dialIngress(t, addr, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:])),
		stop: make(chan struct{}), wrote: make(chan error, 1), read: make(chan error, 1),
	}
	go func() {
		buf := make([]byte, 4<<10)
		for {
			select {
			case <-p.stop:
				p.wrote <- p.c.(*net.TCPConn).CloseWrite()
				return
			default:
			}
			off := p.sent.Load()
			for i := range buf {
				buf[i] = pattern(off + int64(i))
			}
			if _, err := p.c.Write(buf); err != nil {
				p.wrote <- err
				return
			}
			p.sent.Add(int64(len(buf)))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go func() {
		buf := make([]byte, 16<<10)
		for {
			n, err := p.c.Read(buf)
			off := p.got.Load()
			for i := 0; i < n; i++ {
				if buf[i] != pattern(off+int64(i)) {
					p.read <- errors.New("a byte came back wrong")
					return
				}
			}
			p.got.Add(int64(n))
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				p.read <- err
				return
			}
		}
	}()
	return p
}

// flowing waits until another stretch of the stream has come back, which is what
// says the chain is carrying it now and not only before.
func (p *player) flowing(t *testing.T) {
	t.Helper()
	want := p.got.Load() + 64<<10
	deadline := time.Now().Add(15 * time.Second)
	for p.got.Load() < want {
		select {
		case err := <-p.read:
			t.Fatalf("the stream stopped: %v (%d bytes back)", err, p.got.Load())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream stalled at %d bytes back of %d sent", p.got.Load(), p.sent.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// finish ends the player's side cleanly and checks that every byte came back.
func (p *player) finish(t *testing.T) {
	t.Helper()
	close(p.stop)
	if err := <-p.wrote; err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-p.read:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the stream never ended: %d of %d bytes back", p.got.Load(), p.sent.Load())
	}
	if p.got.Load() != p.sent.Load() {
		t.Fatalf("%d bytes came back of %d sent", p.got.Load(), p.sent.Load())
	}
}

func udpAddr(n *node) string { return n.servers[0].tun.Addr().String() }

// Every node on a tunnelled chain is replaced, one after the other, under a login
// that is moving data the whole time. The player sees every byte in order, the
// backend sees one connection, and the session is written down once, when it
// ends, with every byte of it — the way it would have been with no deploy at all.
func TestHandoffCarriesALoginAcrossTheWholeChain(t *testing.T) {
	useMojang(t, nil)
	feed := newFeedCapture(t)
	t.Setenv(EnvSessionsWebhook, feed.url)
	backend, dials := echoBackend(t)
	k1, k2 := key(), key()

	exitCfg := &Config{Name: "ch", Listeners: []Listener{{Net: "udp", Bind: "127.0.0.1:0",
		Upstream: backend, Peers: []Link{{Addr: "127.0.0.1", Key: k2}}}}}
	exit := mustStart(t, exitCfg)
	relayCfg := &Config{Name: "ty2", Listeners: []Listener{{Net: "udp", Bind: "127.0.0.1:0",
		Peers: []Link{{Addr: "127.0.0.1", Key: k1}}, Hops: []Link{{Addr: udpAddr(exit), Key: k2}}}}}
	relay := mustStart(t, relayCfg)
	entryCfg := &Config{
		Name:       "hk",
		SessionLog: filepath.Join(t.TempDir(), "sessions.jsonl"),
		Listeners: []Listener{{Bind: "127.0.0.1:0", Hops: []Link{{Addr: udpAddr(relay), Key: k1}},
			Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565,
				Whitelist: whitelistFile(t, "Notch:"+notchUUID+"\n")}}},
	}
	entry := mustStart(t, entryCfg)
	// Every node binds 127.0.0.1:0 the first time; the one that takes over must
	// come up on the same ports, which it only can by inheriting the sockets.
	entryAddr := entry.lns[0].Addr().String()

	p := play(t, entryAddr)
	p.flowing(t)

	exit = handOver(t, exit, exitCfg)
	p.flowing(t)
	relay = handOver(t, relay, relayCfg)
	p.flowing(t)
	entry = handOver(t, entry, entryCfg)
	p.flowing(t)
	if got := entry.lns[0].Addr().String(); got != entryAddr {
		t.Fatalf("the entry came back on %s, not %s", got, entryAddr)
	}
	if n := entry.servers[0].online.Load(); n != 1 {
		t.Fatalf("the new entry counts %d online, want 1", n)
	}
	if live := entry.live.Live(); len(live) != 1 || live[0].Name != "Notch" {
		t.Fatalf("the new entry's roster is %+v", live)
	}

	p.finish(t)
	if n := dials.Load(); n != 1 {
		t.Fatalf("the backend was dialled %d times, want once", n)
	}
	waitPending(t, entry.live, 0)
	past, err := entry.live.History([]string{notchUUID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(past) != 1 {
		t.Fatalf("%d sessions written down, want 1", len(past))
	}
	// Up has never counted what bufio pulled in behind the Login Start, which
	// goes on ahead of the relay; everything after that it counts.
	sent := p.sent.Load()
	if past[0].Down != sent || past[0].Up > sent || past[0].Up < sent-4096 {
		t.Fatalf("session written down with up=%d down=%d, want %d", past[0].Up, past[0].Down, sent)
	}
	entry.close()
	exit.close()
	relay.close()
	all := feed.all()
	if strings.Count(all, "joined") != 1 || strings.Count(all, "left") != 1 {
		t.Fatalf("the feed should say joined once and left once: %q", all)
	}
}

// The egress is also an ingress: a direct route relays a player's TCP connection
// straight into the backend's, and both of those move to the next process as they
// are.
func TestHandoffCarriesADirectLogin(t *testing.T) {
	useMojang(t, nil)
	backend, dials := echoBackend(t)
	cfg := &Config{
		Name:       "ch",
		SessionLog: filepath.Join(t.TempDir(), "sessions.jsonl"),
		Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: backend,
			Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565,
				Whitelist: whitelistFile(t, "Notch:"+notchUUID+"\n")}}},
	}
	n := mustStart(t, cfg)
	p := play(t, n.lns[0].Addr().String())
	p.flowing(t)
	n = handOver(t, n, cfg)
	p.flowing(t)
	n = handOver(t, n, cfg)
	p.flowing(t)
	p.finish(t)
	if got := dials.Load(); got != 1 {
		t.Fatalf("the backend was dialled %d times, want once", got)
	}
	waitPending(t, n.live, 0)
	if past, _ := n.live.History(nil, 10); len(past) != 1 {
		t.Fatalf("%d sessions written down, want 1", len(past))
	}
	n.close()
}

// A player who connects while a handoff is under way is not refused: the
// listening socket is never closed, so the connection waits in its queue and the
// next process accepts it.
func TestHandoffKeepsTheDoorOpen(t *testing.T) {
	useMojang(t, nil)
	backend, _ := echoBackend(t)
	cfg := &Config{Name: "ch", Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: backend,
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565}}}}
	n := mustStart(t, cfg)
	addr := n.lns[0].Addr().String()

	var got *handoff.Inherited
	if err := n.handoff(func(snap []byte, files []handoff.File) error {
		var dup []handoff.File
		for _, f := range files {
			dup = append(dup, handoff.File{Name: f.Name, File: dupFile(t, f.File)})
		}
		got = handoff.NewInherited(snap, dup)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Between processes: nobody is accepting.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("refused between processes: %v", err)
	}
	defer c.Close()
	next, resumes, err := build(cfg, readInherited(got))
	if err != nil {
		t.Fatal(err)
	}
	next.serve(resumes)
	defer next.close()

	hs := (&mc.Handshake{ProtocolVersion: 764, Address: "x", Port: 1, Intent: mc.IntentLogin}).Encode()
	c.Write(append(hs, loginStart("Notch", notchRaw[:])...))
	c.Write([]byte("ping"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("the connection made between processes was not served: %q %v", b, err)
	}
}

func mustStart(t *testing.T, cfg *Config) *node {
	t.Helper()
	n, err := start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A login that has not reached its relay when the grace runs out is cut off rather
// than allowed to hold the handoff open: the player reconnects, and the handoff
// finishes on time.
func TestHandoffCutsOffALoginThatStalls(t *testing.T) {
	useMojang(t, nil)
	backend, _ := echoBackend(t)
	cfg := &Config{Name: "ch", Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: backend,
		Minecraft: &Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565}}}}
	n := mustStart(t, cfg)
	c, err := net.Dial("tcp", n.lns[0].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x10}) // the first byte of a handshake, and nothing more
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if err := n.handoff(func([]byte, []handoff.File) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > handoffGrace+time.Second {
		t.Fatalf("a stalled login held the handoff for %s", took)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the stalled login was left open")
	}
}

// A snapshot from a newer build is not guessed at: the node starts clean, and
// every descriptor that came with it is closed, which ends those sessions as a
// plain restart would have.
func TestNewerSnapshotStartsClean(t *testing.T) {
	a, b := tcpPair(t)
	f, err := b.File()
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	in := readInherited(handoff.NewInherited([]byte(`{"version":99,"relays":[{"bind":"x","a":{"fd":"f0"}}]}`),
		[]handoff.File{{Name: "f0", File: f}}))
	if len(in.snap.Relays) != 0 {
		t.Fatal("a newer snapshot was read")
	}
	backend, _ := echoBackend(t)
	n, _, err := build(&Config{Listeners: []Listener{{Bind: "127.0.0.1:0", Upstream: backend}}}, in)
	if err != nil {
		t.Fatal(err)
	}
	defer n.close()
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("a connection nobody could carry was left open: %v", err)
	}
}
