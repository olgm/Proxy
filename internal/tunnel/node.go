package tunnel

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults. MaxDatagram is the whole datagram before IP and UDP headers: 1200
// bytes is the figure that survives every path anyone still runs, including a
// mesh VPN wrapped in another UDP header.
const (
	defaultMaxDatagram = 1200
	defaultWindow      = 1 << 20
	defaultRepair      = 5 * time.Second
	defaultIdle        = 120 * time.Second
	sweepEvery         = 5 * time.Second
	acceptBacklog      = 64
)

// Timers are every interval the tunnel runs on. Zero means the default. They
// are per node, so proxyctl sets the same ones on every node of a route; a
// receiver that re-asks faster than its sender expects is not wrong, only
// noisier. Minecraft traffic is light enough that all of these can be made a
// good deal more aggressive than the defaults without the extra packets
// mattering, and the defaults lean toward not wasting a leg's capacity.
type Timers struct {
	// Tick is the granularity of everything below. 5 ms.
	Tick time.Duration
	// Ping is sent per link; a link is called down after five unanswered. 1 s.
	Ping time.Duration
	// NackMin and NackMax clamp how often a hole is asked for again, and how
	// often a quiet sender repeats its horizon: srtt + 4·mdev measured on that
	// leg, held between these. Equal values fix the interval. 10 ms and 1 s.
	NackMin, NackMax time.Duration
	// HeadQuiet is how long a sender with chunks unacknowledged stays silent
	// before telling the next hop how far it got. 10 ms.
	HeadQuiet time.Duration
	// AckEvery bounds how often a terminator reports its watermark when it has
	// moved; AckRepeat re-announces it when it has not. 20 ms and 250 ms.
	AckEvery, AckRepeat time.Duration
	// ProbeMin and ProbeMax clamp the originator's blind re-send of its highest
	// chunk: end-to-end srtt + 4·mdev, doubling on each try. 100 ms and 1 s.
	ProbeMin, ProbeMax time.Duration
}

var defaultTimers = Timers{
	Tick:      5 * time.Millisecond,
	Ping:      time.Second,
	NackMin:   10 * time.Millisecond,
	NackMax:   time.Second,
	HeadQuiet: 10 * time.Millisecond,
	AckEvery:  20 * time.Millisecond,
	AckRepeat: 250 * time.Millisecond,
	ProbeMin:  100 * time.Millisecond,
	ProbeMax:  time.Second,
}

func (t *Timers) fill() error {
	for _, f := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&t.Tick, defaultTimers.Tick}, {&t.Ping, defaultTimers.Ping},
		{&t.NackMin, defaultTimers.NackMin}, {&t.NackMax, defaultTimers.NackMax},
		{&t.HeadQuiet, defaultTimers.HeadQuiet},
		{&t.AckEvery, defaultTimers.AckEvery}, {&t.AckRepeat, defaultTimers.AckRepeat},
		{&t.ProbeMin, defaultTimers.ProbeMin}, {&t.ProbeMax, defaultTimers.ProbeMax},
	} {
		if *f.v == 0 {
			*f.v = f.def
		}
		if *f.v < 0 {
			return errors.New("tunnel: a timer cannot be negative")
		}
	}
	if t.NackMin > t.NackMax {
		return fmt.Errorf("tunnel: nack_min %v is above nack_max %v", t.NackMin, t.NackMax)
	}
	if t.ProbeMin > t.ProbeMax {
		return fmt.Errorf("tunnel: probe_min %v is above probe_max %v", t.ProbeMin, t.ProbeMax)
	}
	return nil
}

// LinkConfig describes one leg. Addr is a plain IP for a peer that dials us,
// because its source port is ephemeral, and host:port for a hop we dial.
type LinkConfig struct {
	Addr string
	Key  []byte
	// Dup is how many copies of each chunk this node puts on this leg. It is
	// this node's setting for this leg only: the far end drops every copy but
	// the first, then sends on with its own count for the leg after.
	Dup int
}

type Options struct {
	Name string
	// Bind is the address peers reach us at. Empty is valid for a node that only
	// dials — the entry never binds, so it needs no inbound rule of its own.
	Bind  string
	Peers []LinkConfig
	Hops  []LinkConfig

	MaxDatagram int
	Window      int
	Repair      time.Duration
	Idle        time.Duration
	Timers      Timers

	// Resume carries on from a node frozen in another process instead of
	// starting clean. See Freeze.
	Resume *Resume

	maxChunk int
}

// Node is this machine's end of a tunnel. What it does follows from its links:
// hops but no peers makes it the entry, peers but no hops the exit, both a relay.
type Node struct {
	opt   Options
	up    []*Link // toward the entry
	down  []*Link // toward the exit
	socks []*socket

	accept chan *Stream

	mu      sync.Mutex
	streams map[uint64]*Stream

	stop     chan struct{}
	stopOnce sync.Once

	// frozen is set by Freeze, and from then on the node neither reads nor sends
	// anything, and nothing about a stream changes. thaw is closed with it, to
	// end the goroutines that do the reading and the timing; wg counts them.
	frozen atomic.Bool
	thaw   chan struct{}
	wg     sync.WaitGroup

	// lastUp is the last tick at which any link had answered a ping. Touched only
	// by the timer goroutine.
	lastUp time.Time

	// chain is what the entry knows about the round trip to the far end of the
	// tunnel: the echoes it has outstanding, and the smoothed time they took.
	chainMu  sync.Mutex
	chainRTT time.Duration
	echoSeq  uint64
	echoAt   map[uint64]time.Time
}

func New(opt Options) (*Node, error) {
	if len(opt.Peers) == 0 && len(opt.Hops) == 0 {
		return nil, errors.New("tunnel: a node needs peers, hops, or both")
	}
	if len(opt.Peers) > 0 && opt.Bind == "" {
		return nil, errors.New("tunnel: peers need a bind address to reach")
	}
	if opt.MaxDatagram == 0 {
		opt.MaxDatagram = defaultMaxDatagram
	}
	if opt.Window == 0 {
		opt.Window = defaultWindow
	}
	if opt.Repair == 0 {
		opt.Repair = defaultRepair
	}
	if opt.Idle == 0 {
		opt.Idle = defaultIdle
	}
	if err := opt.Timers.fill(); err != nil {
		return nil, err
	}
	opt.maxChunk = opt.MaxDatagram - NonceLen - 16 - dataHeader
	if opt.maxChunk < 64 {
		return nil, fmt.Errorf("tunnel: max_datagram %d leaves no room for payload", opt.MaxDatagram)
	}

	n := &Node{opt: opt, accept: make(chan *Stream, acceptBacklog),
		streams: map[uint64]*Stream{}, stop: make(chan struct{}), lastUp: time.Now(),
		echoAt: map[uint64]time.Time{}, thaw: make(chan struct{})}
	var from Sockets
	if opt.Resume != nil {
		from = opt.Resume.Sockets
	}

	if len(opt.Peers) > 0 {
		conn := from.Bound
		if conn == nil {
			addr, err := net.ResolveUDPAddr("udp", opt.Bind)
			if err != nil {
				return nil, fmt.Errorf("tunnel: bind %s: %w", opt.Bind, err)
			}
			if conn, err = net.ListenUDP("udp", addr); err != nil {
				return nil, err
			}
		}
		sock := &socket{conn: conn, node: n}
		for _, p := range opt.Peers {
			ip, err := netip.ParseAddr(p.Addr)
			if err != nil {
				return nil, fmt.Errorf("tunnel: peer %q must be a bare IP: %w", p.Addr, err)
			}
			l, err := newLink(sock, ip.Unmap(), 0, p, false)
			if err != nil {
				return nil, err
			}
			sock.links = append(sock.links, l)
			n.up = append(n.up, l)
		}
		n.socks = append(n.socks, sock)
	}
	if len(opt.Hops) > 0 {
		// The same socket a frozen node dialled from, where there is one: its port
		// is the address every hop has learned to answer, and a new one would
		// leave them answering nobody until the next ping.
		conn := from.Dial
		if conn == nil {
			var err error
			if conn, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
				return nil, err
			}
		}
		sock := &socket{conn: conn, node: n}
		for _, h := range opt.Hops {
			ua, err := net.ResolveUDPAddr("udp", h.Addr)
			if err != nil {
				return nil, fmt.Errorf("tunnel: hop %q: %w", h.Addr, err)
			}
			ip, ok := netip.AddrFromSlice(ua.IP)
			if !ok {
				return nil, fmt.Errorf("tunnel: hop %q has no address", h.Addr)
			}
			l, err := newLink(sock, ip.Unmap(), ua.Port, h, true)
			if err != nil {
				return nil, err
			}
			l.remote.Store(ua)
			sock.links = append(sock.links, l)
			n.down = append(n.down, l)
		}
		n.socks = append(n.socks, sock)
	}

	if opt.Resume != nil {
		n.resume(opt.Resume)
	}
	for _, s := range n.socks {
		n.wg.Add(1)
		go s.read()
	}
	n.wg.Add(1)
	go n.timers()
	if opt.Resume != nil {
		n.settle(opt.Resume.Attached)
	}
	return n, nil
}

func newLink(s *socket, ip netip.Addr, port int, c LinkConfig, down bool) (*Link, error) {
	seal, err := NewSealer(c.Key)
	if err != nil {
		return nil, fmt.Errorf("tunnel: link %s: %w", c.Addr, err)
	}
	dup := c.Dup
	if dup < 1 {
		dup = 1
	}
	return &Link{sock: s, ip: ip, port: port, dup: dup, down: down, seal: seal, addr: c.Addr}, nil
}

// Open starts a stream. Only a node with hops can: the entry is the one end that
// knows a new client has arrived.
func (n *Node) Open() (*Stream, error) {
	if len(n.down) == 0 {
		return nil, errors.New("tunnel: node has no hops to open a stream on")
	}
	if n.frozen.Load() {
		return nil, ErrClosed
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	s := n.newStream(binary.BigEndian.Uint64(b[:]))
	n.mu.Lock()
	n.streams[s.id] = s
	n.mu.Unlock()
	return s, nil
}

// Accept returns streams opened at the far end. Only the exit sees them: a relay
// passes chunks along without ever holding the bytes as a stream.
func (n *Node) Accept() (*Stream, error) {
	select {
	case s := <-n.accept:
		return s, nil
	case <-n.stop:
		return nil, ErrClosed
	case <-n.thaw:
		return nil, ErrClosed
	}
}

func (n *Node) Close() error {
	n.stopOnce.Do(func() {
		close(n.stop)
		for _, s := range n.socks {
			s.conn.Close()
		}
	})
	return nil
}

// Addr is the address peers reach this node at, or nil for a node that only
// dials. Resolved rather than configured, so a bind on port 0 reports the port
// the kernel picked.
func (n *Node) Addr() *net.UDPAddr {
	if len(n.opt.Peers) == 0 {
		return nil
	}
	return n.socks[0].conn.LocalAddr().(*net.UDPAddr)
}

func (n *Node) stopped() bool {
	select {
	case <-n.stop:
		return true
	default:
		return false
	}
}

func (n *Node) newStream(id uint64) *Stream {
	s := &Stream{n: n, id: id, lastSeen: time.Now()}
	s.down = newDir(s, n.down, n.up)
	s.up = newDir(s, n.up, n.down)
	switch {
	case len(n.up) == 0: // entry: writes toward the exit, reads what comes back
		s.rx, s.tx = s.up, s.down
	case len(n.down) == 0: // exit: mirror image
		s.rx, s.tx = s.down, s.up
	}
	return s
}

// handle dispatches one decoded datagram. size is what it weighed on the socket,
// sealed, which is what the leg was billed for and what a session's share of the
// bill is built from.
func (n *Node) handle(l *Link, p packet, size int) {
	switch p.typ {
	case msgPing:
		l.send(appendEcho(nil, msgPong, p.nonce), 1, false)
		return
	case msgPong:
		l.pong(p.nonce)
		return
	case msgEcho:
		// Pass it on while there is chain left, and turn it around where there is
		// not. A node with no hops is the exit, which is as far as anything of ours
		// goes: one hop further is the backend.
		if len(n.down) > 0 {
			n.flood(n.down, msgEcho, p.nonce)
		} else {
			l.send(appendEcho(nil, msgEchoReply, p.nonce), 1, false)
		}
		return
	case msgEchoReply:
		if len(n.up) > 0 {
			n.flood(n.up, msgEchoReply, p.nonce)
		} else {
			n.chainPong(p.nonce)
		}
		return
	}

	n.mu.Lock()
	s, ok := n.streams[p.stream]
	if !ok {
		// Only DATA opens a stream, and never at the entry, which is the end that
		// originates them: a stray NACK must not be able to make a node dial the
		// backend, and a late one must not resurrect a stream that has finished.
		if p.typ != msgData || len(n.up) == 0 {
			n.mu.Unlock()
			return
		}
		s = n.newStream(p.stream)
		n.streams[p.stream] = s
	}
	n.mu.Unlock()

	now := time.Now()
	s.touch(now)
	// Attributable from here down: everything above either belongs to the link
	// rather than to a session, or names a stream this node does not have.
	s.recv.Add(uint64(size))

	// A datagram arriving from the entry side carries data going down; anything
	// else on that link is a report about what we last sent up, and vice versa.
	data, ctrl := s.down, s.up
	if l.down {
		data, ctrl = s.up, s.down
	}

	switch p.typ {
	case msgData:
		if s.gone() {
			return // closed, and kept only so a late NACK still finds an answer
		}
		data.recv(p)
		if s.rx != nil && !s.offered.Load() && s.rx.holds(0) {
			n.offer(s)
		}
	case msgHead:
		if s.gone() {
			return
		}
		data.onHead(p.through)
	case msgNack:
		ctrl.onNack(l, p.seqs)
	case msgAck:
		if ctrl.onAck(p.through) {
			// Pass the watermark on so every hop behind us can free its buffer too.
			plain := appendAck(nil, s.id, p.through)
			for _, k := range ctrl.back {
				s.sent.Add(uint64(k.send(plain, 1, false)))
			}
		}
	case msgReset:
		plain := appendReset(nil, s.id)
		for _, k := range data.send {
			s.sent.Add(uint64(k.send(plain, 1, false)))
		}
		s.abort(ErrReset)
	}
}

// offer hands a stream to Accept once its first chunk has arrived, and not before.
// A chunk from the middle of a stream this node has no record of belongs to a
// stream it lost track of — it restarted, or finished the stream long enough ago
// to forget it — and dialling the backend for one would hand the backend the
// middle of somebody's session from our egress address. Such a stream is left to
// fail its repair deadline instead, and the reset that sends tells the rest of the
// chain it is gone.
func (n *Node) offer(s *Stream) {
	if !s.offered.CompareAndSwap(false, true) {
		return
	}
	select {
	case n.accept <- s:
	default:
		n.mu.Lock()
		delete(n.streams, s.id)
		n.mu.Unlock()
		log.Printf("%s: accept backlog full, dropping stream", n.opt.Name)
	}
}

// close retires a stream. Its state stays in the map for a while: it is what
// answers a NACK that arrives after the last byte, and what stops a retransmitted
// chunk from being read as a brand new stream and dialled to the backend again.
func (n *Node) close(s *Stream, reset bool) {
	s.mu.Lock()
	already := !s.expires.IsZero()
	s.expires = time.Now().Add(linger)
	s.mu.Unlock()
	if already {
		return
	}
	s.down.abort(ErrClosed)
	s.up.abort(ErrClosed)
	if reset {
		plain := appendReset(nil, s.id)
		for _, l := range n.links() {
			s.sent.Add(uint64(l.send(plain, 1, false)))
		}
	}
}

func (n *Node) links() []*Link {
	out := make([]*Link, 0, len(n.up)+len(n.down))
	return append(append(out, n.up...), n.down...)
}

func (n *Node) live() []*Stream {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*Stream, 0, len(n.streams))
	for _, s := range n.streams {
		out = append(out, s)
	}
	return out
}

func (n *Node) timers() {
	defer n.wg.Done()
	tick := time.NewTicker(n.opt.Timers.Tick)
	ping := time.NewTicker(n.opt.Timers.Ping)
	sweep := time.NewTicker(sweepEvery)
	stat := time.NewTicker(statsEvery)
	defer func() {
		tick.Stop()
		ping.Stop()
		sweep.Stop()
		stat.Stop()
	}()
	for {
		select {
		case <-n.stop:
			return
		case <-n.thaw:
			return
		case now := <-tick.C:
			dead := n.silent(now)
			for _, s := range n.live() {
				if s.gone() {
					continue
				}
				// Both halves matter. No link answering is not enough on its own —
				// a link is only called up once a pong has come back, so a node
				// that has just started has none — and a stream still being fed is
				// not stalled whatever the ping stream says.
				if dead && now.Sub(s.seen()) > n.opt.Repair {
					s.abort(ErrUnrepairable)
					continue
				}
				s.down.tick(now)
				s.up.tick(now)
			}
		case <-ping.C:
			for _, l := range n.links() {
				l.ping()
				if u := l.up(); u != l.wasUp {
					l.wasUp = u
					log.Printf("%s: link %s %s", n.opt.Name, l, upWord(u))
				}
			}
			if n.originates() {
				n.chainEcho()
			}
		case now := <-sweep.C:
			n.sweep(now)
		case <-stat.C:
			n.logStats()
		}
	}
}

// silent reports that nothing can get through. Gap detection cannot see a hole at
// the end of a stream — there is no higher sequence number to reveal it — so the
// only thing that distinguishes a quiet session from a dead path is whether the
// links themselves are still answering. Calling a link down already takes five
// seconds of unanswered pings; a repair window on top of that is the point at
// which a session is given up rather than left hanging.
func (n *Node) silent(now time.Time) bool {
	for _, l := range n.links() {
		if l.up() {
			n.lastUp = now
			return false
		}
	}
	return now.Sub(n.lastUp) > n.opt.Repair
}

func upWord(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func (n *Node) sweep(now time.Time) {
	for _, s := range n.live() {
		s.mu.Lock()
		expired := !s.expires.IsZero() && now.After(s.expires)
		idle := now.Sub(s.lastSeen) > n.opt.Idle
		s.mu.Unlock()
		if expired {
			n.mu.Lock()
			delete(n.streams, s.id)
			n.mu.Unlock()
			continue
		}
		if idle {
			s.abort(ErrClosed)
		}
	}
}

// logStats is the only view from outside a node of whether a leg is carrying UDP
// at all. A filtered UDP port is indistinguishable from an open one until
// something answers, so proxyctl reads these lines rather than probing.
func (n *Node) logStats() {
	for _, l := range n.links() {
		srtt, mdev, s := l.stats()
		if s.pings == 0 && s.sent == 0 && s.recv == 0 {
			continue
		}
		loss := 0.0
		if s.pings > 0 {
			loss = 100 * float64(s.lost) / float64(s.pings)
		}
		log.Printf("%s: link %s %s rtt=%.1fms mdev=%.1fms loss=%.1f%% sent=%d recv=%d rtx=%d dropped=%d",
			n.opt.Name, l, upWord(l.up()), ms(srtt), ms(mdev), loss, s.sent, s.recv, s.rtx, s.dropped)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// ChainRTT is the round trip from here to the far end of the tunnel and back, or
// zero at a node that does not originate echoes or has not had one answered yet.
//
// The ingress reports this as the latency of a status ping, so a player sees the
// distance to the last node that is ours instead of the distance to the first.
// See internal/proxy.
func (n *Node) ChainRTT() time.Duration {
	n.chainMu.Lock()
	defer n.chainMu.Unlock()
	return n.chainRTT
}

// originates reports whether this node is the end that starts echoes. Only the
// entry does: it is the one with somewhere to send them and nobody behind it.
func (n *Node) originates() bool { return len(n.up) == 0 && len(n.down) > 0 }

// chainEcho starts one measurement. With more than one hop it goes down every path
// and the first answer back wins, which is the same thing the data plane does.
func (n *Node) chainEcho() {
	now := time.Now()
	n.chainMu.Lock()
	// An echo that was never answered is a lost datagram, not a slow one. Drop it
	// rather than let it age into a wrong sample or sit in the map for ever.
	stale := 5 * n.opt.Timers.Ping
	for id, at := range n.echoAt {
		if now.Sub(at) > stale {
			delete(n.echoAt, id)
		}
	}
	n.echoSeq++
	id := n.echoSeq
	n.echoAt[id] = now
	n.chainMu.Unlock()

	n.flood(n.down, msgEcho, id)
}

// chainPong folds one answered echo into the smoothed round trip. Copies of the
// same echo arrive when the chain races several paths; the first is the one that
// says how fast the chain is, so the rest are dropped by the id already being gone.
func (n *Node) chainPong(id uint64) {
	n.chainMu.Lock()
	defer n.chainMu.Unlock()
	at, ok := n.echoAt[id]
	if !ok {
		return
	}
	delete(n.echoAt, id)
	r := time.Since(at)
	if n.chainRTT == 0 {
		n.chainRTT = r
		return
	}
	n.chainRTT = (7*n.chainRTT + r) / 8
}

// flood sends one small control datagram on every link in a direction. Echoes carry
// no sequence number, so there is nothing for a relay to de-duplicate: a copy that
// arrives second is dropped by the originator instead.
func (n *Node) flood(links []*Link, t msgType, nonce uint64) {
	for _, l := range links {
		l.send(appendEcho(nil, t, nonce), 1, false)
	}
}

func (s *Stream) gone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.expires.IsZero()
}
