package tunnel

import (
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// pingEvery also keeps the entry's ephemeral socket alive through any NAT
	// between it and the next hop; the entry never binds a port of its own.
	pingEvery = time.Second
	minRTO    = 10 * time.Millisecond
	maxRTO    = time.Second
	// statsEvery governs the one line per link that says whether this leg is
	// carrying UDP at all, and at what loss. It is the only way to answer that
	// question from outside the node.
	statsEvery = 30 * time.Second
)

// Link is one node-to-node leg: the address of a peer, the key that leg is
// authenticated with, and a round-trip estimate measured on it. A link is
// bidirectional. Which way data flows across it is decided by which side of the
// node it hangs off, not by anything in the link itself.
type Link struct {
	sock *socket
	// ip is the address datagrams must arrive from. port is checked only for a
	// peer we dial: one that dials us uses an ephemeral source port.
	ip   netip.Addr
	port int
	// dup is how many copies of each chunk this node originates onto the link.
	// A chunk merely passed through is always sent once — see Node.
	dup int
	// down says which side of the node this link hangs off: true toward the exit,
	// false toward the entry. It is what tells a datagram's direction from the
	// socket it arrived on, so nothing on the wire has to carry one.
	down   bool
	seal   *sealer
	remote atomic.Pointer[net.UDPAddr]
	// wasUp is touched only by the node's timer goroutine, to log transitions.
	wasUp bool

	mu       sync.Mutex
	srtt     time.Duration
	mdev     time.Duration
	pending  uint64 // nonce of the ping awaiting a pong, 0 for none
	sentAt   time.Time
	lastPong time.Time
	st       linkStats
}

type linkStats struct{ sent, recv, rtx, pings, pongs, dropped uint64 }

func (l *Link) String() string {
	if r := l.remote.Load(); r != nil {
		return r.String()
	}
	return l.ip.String()
}

// rto is how long to wait before asking for a hole again. One round trip on this
// leg is the whole point: a repair costs the leg, not the chain.
func (l *Link) rto() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.srtt == 0 {
		return 100 * time.Millisecond
	}
	return clamp(l.srtt+4*l.mdev, minRTO, maxRTO)
}

func clamp(d, lo, hi time.Duration) time.Duration {
	return max(lo, min(hi, d))
}

func (l *Link) sample(r time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.srtt == 0 {
		l.srtt, l.mdev = r, r/2
		return
	}
	d := l.srtt - r
	if d < 0 {
		d = -d
	}
	l.mdev = (3*l.mdev + d) / 4
	l.srtt = (7*l.srtt + r) / 8
}

// send writes n independently sealed copies. Sealing each copy separately is not
// waste: identical bytes would look like a replay to the far end and the second
// copy would be discarded, which is exactly what duplication must not do.
func (l *Link) send(plain []byte, n int, rtx bool) {
	to := l.remote.Load()
	if to == nil {
		return // a peer that has never spoken has no address to answer at
	}
	for i := 0; i < n; i++ {
		buf := l.seal.seal(make([]byte, 0, nonceLen+len(plain)+16), plain)
		if _, err := l.sock.conn.WriteToUDP(buf, to); err != nil {
			l.count(func(s *linkStats) { s.dropped++ })
			return
		}
		l.count(func(s *linkStats) {
			s.sent++
			if rtx {
				s.rtx++
			}
		})
	}
}

func (l *Link) count(f func(*linkStats)) {
	l.mu.Lock()
	f(&l.st)
	l.mu.Unlock()
}

func (l *Link) ping() {
	l.mu.Lock()
	nonce := uint64(time.Now().UnixNano())
	l.pending, l.sentAt = nonce, time.Now()
	l.st.pings++
	l.mu.Unlock()
	l.send(appendEcho(nil, msgPing, nonce), 1, false)
}

func (l *Link) pong(nonce uint64) {
	l.mu.Lock()
	if l.pending != nonce {
		l.mu.Unlock()
		return
	}
	r := time.Since(l.sentAt)
	l.pending = 0
	l.lastPong = time.Now()
	l.st.pongs++
	l.mu.Unlock()
	l.sample(r)
}

// stats reports and resets the window. Loss is measured on the ping stream, which
// is the only traffic guaranteed to exist on an idle link.
func (l *Link) stats() (srtt, mdev time.Duration, s linkStats) {
	l.mu.Lock()
	defer l.mu.Unlock()
	srtt, mdev, s = l.srtt, l.mdev, l.st
	l.st = linkStats{}
	return
}

// up reports whether the far end answered recently. proxyctl reads this through
// the log line, because a filtered UDP port looks exactly like an open one until
// something replies from it.
func (l *Link) up() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.lastPong.IsZero() && time.Since(l.lastPong) < 5*pingEvery
}

// socket is one UDP socket and the links reachable through it. A node has at most
// two: one bound, for peers that dial us, and one ephemeral, for hops we dial.
type socket struct {
	conn  *net.UDPConn
	links []*Link
	node  *Node
}

// candidates are the links a datagram from this address could belong to. There
// can be more than one — two peers behind the same NAT, or two paths that happen
// to share an address — so the key decides, not the address. The address only
// narrows the search.
func (s *socket) candidates(from *net.UDPAddr) []*Link {
	ap := from.AddrPort()
	ip := ap.Addr().Unmap()
	var out []*Link
	for _, l := range s.links {
		if l.ip == ip && (l.port == 0 || l.port == int(ap.Port())) {
			out = append(out, l)
		}
	}
	return out
}

func (s *socket) read() {
	buf := make([]byte, 65535)
	plain := make([]byte, 0, 65535)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if s.node.stopped() {
				return
			}
			// A closed socket ends the loop; anything else is one bad datagram.
			if ne, ok := err.(net.Error); ok && !ne.Timeout() {
				log.Printf("%s: udp read: %v", s.node.opt.Name, err)
				return
			}
			continue
		}
		var (
			l   *Link
			out []byte
		)
		for _, c := range s.candidates(from) {
			if o, err := c.seal.open(plain[:0], buf[:n]); err == nil {
				l, out = c, o
				break
			}
			c.count(func(st *linkStats) { st.dropped++ })
		}
		if l == nil {
			continue
		}
		p, err := decode(out)
		if err != nil {
			l.count(func(st *linkStats) { st.dropped++ })
			continue
		}
		// Learn where to answer. A peer's source port is ephemeral, and may move
		// if it restarts or its NAT rebinds.
		if r := l.remote.Load(); r == nil || r.String() != from.String() {
			l.remote.Store(from)
		}
		l.count(func(st *linkStats) { st.recv++ })
		s.node.handle(l, p)
	}
}
