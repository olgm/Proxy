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
	tickEvery          = 5 * time.Millisecond
	sweepEvery         = 5 * time.Second
	acceptBacklog      = 64
)

// LinkConfig describes one leg. Addr is a plain IP for a peer that dials us,
// because its source port is ephemeral, and host:port for a hop we dial.
type LinkConfig struct {
	Addr string
	Key  []byte
	// Dup is how many copies of each chunk this node sends when it is the one
	// putting them on the wire. It is ignored on a relay: a chunk merely passing
	// through is forwarded once per copy received, so the count set where the
	// stream enters the tunnel is the count that reaches the far end.
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

	// lastUp is the last tick at which any link had answered a ping. Touched only
	// by the timer goroutine.
	lastUp time.Time
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
	opt.maxChunk = opt.MaxDatagram - nonceLen - 16 - dataHeader
	if opt.maxChunk < 64 {
		return nil, fmt.Errorf("tunnel: max_datagram %d leaves no room for payload", opt.MaxDatagram)
	}

	n := &Node{opt: opt, accept: make(chan *Stream, acceptBacklog),
		streams: map[uint64]*Stream{}, stop: make(chan struct{}), lastUp: time.Now()}

	if len(opt.Peers) > 0 {
		addr, err := net.ResolveUDPAddr("udp", opt.Bind)
		if err != nil {
			return nil, fmt.Errorf("tunnel: bind %s: %w", opt.Bind, err)
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return nil, err
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
		conn, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			return nil, err
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

	for _, s := range n.socks {
		go s.read()
	}
	go n.timers()
	return n, nil
}

func newLink(s *socket, ip netip.Addr, port int, c LinkConfig, down bool) (*Link, error) {
	seal, err := newSealer(c.Key)
	if err != nil {
		return nil, fmt.Errorf("tunnel: link %s: %w", c.Addr, err)
	}
	dup := c.Dup
	if dup < 1 {
		dup = 1
	}
	return &Link{sock: s, ip: ip, port: port, dup: dup, down: down, seal: seal}, nil
}

// Open starts a stream. Only a node with hops can: the entry is the one end that
// knows a new client has arrived.
func (n *Node) Open() (*Stream, error) {
	if len(n.down) == 0 {
		return nil, errors.New("tunnel: node has no hops to open a stream on")
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

func (n *Node) handle(l *Link, p packet) {
	switch p.typ {
	case msgPing:
		l.send(appendEcho(nil, msgPong, p.nonce), 1, false)
		return
	case msgPong:
		l.pong(p.nonce)
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
		n.mu.Unlock()
		if s.rx != nil {
			select {
			case n.accept <- s:
			default:
				n.mu.Lock()
				delete(n.streams, s.id)
				n.mu.Unlock()
				log.Printf("%s: accept backlog full, dropping stream", n.opt.Name)
				return
			}
		}
	} else {
		n.mu.Unlock()
	}

	now := time.Now()
	s.touch(now)

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
	case msgNack:
		ctrl.onNack(l, p.seqs)
	case msgAck:
		if ctrl.onAck(p.through) {
			// Pass the watermark on so every hop behind us can free its buffer too.
			plain := appendAck(nil, s.id, p.through)
			for _, k := range ctrl.back {
				k.send(plain, 1, false)
			}
		}
	case msgReset:
		plain := appendReset(nil, s.id)
		for _, k := range data.send {
			k.send(plain, 1, false)
		}
		s.abort(ErrReset)
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
			l.send(plain, 1, false)
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
	tick := time.NewTicker(tickEvery)
	ping := time.NewTicker(pingEvery)
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
		if s.pings > 0 && s.pings >= s.pongs {
			loss = 100 * float64(s.pings-s.pongs) / float64(s.pings)
		}
		log.Printf("%s: link %s %s rtt=%.1fms mdev=%.1fms loss=%.1f%% sent=%d recv=%d rtx=%d dropped=%d",
			n.opt.Name, l, upWord(l.up()), ms(srtt), ms(mdev), loss, s.sent, s.recv, s.rtx, s.dropped)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (s *Stream) gone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.expires.IsZero()
}
