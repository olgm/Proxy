package probe

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olgm/proxy/internal/tunnel"
)

// Node is this machine's part of the measurement. What it does for each class
// follows from that class's links and nothing else: down but no up makes it the
// originator, up but no down the responder, both a relay.
type Node struct {
	cfg      Config
	interval time.Duration
	timeout  time.Duration
	windows  []time.Duration

	links   []*link
	classes map[uint8]*class
	socks   []*socket
	w       *writer

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// link is one leg. Unlike the tunnel's, it carries no duplication count of its
// own: two classes cross the same leg with different counts on purpose, so the
// count belongs to the class.
type link struct {
	sock   *socket
	ip     netip.Addr
	port   int
	seal   *tunnel.Sealer
	remote atomic.Pointer[net.UDPAddr]
	// seen is when this leg last carried anything of ours, and is the only honest
	// signal that it carries UDP at all: nothing about a UDP socket distinguishes
	// a filtered port from an open one until something answers through it. The log
	// line it drives has the same shape as proxyd's, so proxyctl reads both the
	// same way.
	seen  atomic.Int64
	wasUp bool
}

// up reports whether this leg has carried anything recently. The allowance is
// generous against the probe interval because one lost probe is not a dead leg.
func (l *link) up(interval time.Duration) bool {
	t := l.seen.Load()
	return t != 0 && time.Since(time.Unix(0, t)) < max(5*interval, 5*time.Second)
}

func (l *link) String() string {
	if l.port != 0 {
		return net.JoinHostPort(l.ip.String(), fmt.Sprint(l.port))
	}
	return l.ip.String()
}

// send writes n independently sealed copies, exactly as the tunnel does and for
// exactly the same reason: identical bytes would look like a replay to the far end
// and the second copy would be dropped, which is what duplication must not do.
func (l *link) send(plain []byte, n int) {
	to := l.remote.Load()
	if to == nil {
		return // a peer that has never spoken has no address to answer at
	}
	for i := 0; i < n; i++ {
		buf := l.seal.Seal(make([]byte, 0, tunnel.NonceLen+len(plain)+tunnel.GCMOverhead), plain)
		if _, err := l.sock.conn.WriteToUDP(buf, to); err != nil {
			return
		}
	}
}

type socket struct {
	conn  *net.UDPConn
	links []*link
	n     *Node
}

// class is one measurement's state. A node holds the originator's half, the
// responder's half, or both when it relays.
// hop pairs a link with the number of copies this class puts on it.
type hop struct {
	l   *link
	dup int
}

type class struct {
	Class
	n        *Node
	up, down []hop

	mu sync.Mutex
	// Responder and relay state. recv counts distinct probes accepted, which is
	// what the originator differences across a window to learn how many arrived;
	// counting copies instead would report a duplicated leg as carrying twice the
	// probes that were sent.
	recv, high        uint64
	seenReq, seenResp seqWindow
	// Originator state.
	seq    uint64
	out    map[uint64]time.Time
	series []*series
}

func (c *class) originates() bool { return len(c.up) == 0 && len(c.down) > 0 }
func (c *class) responds() bool   { return len(c.down) == 0 }

func New(cfg Config) (*Node, error) {
	if err := cfg.fill(); err != nil {
		return nil, err
	}
	windows, err := cfg.windows()
	if err != nil {
		return nil, err
	}

	n := &Node{
		cfg:      cfg,
		interval: time.Duration(float64(time.Second) / cfg.Hz),
		timeout:  time.Duration(cfg.TimeoutMS) * time.Millisecond,
		windows:  windows,
		classes:  map[uint8]*class{},
		stop:     make(chan struct{}),
	}

	// Two sockets at most, split the way the tunnel splits them: one bound, for
	// legs whose far end dials us, and one ephemeral, for legs we dial. A node
	// that only dials binds nothing and so needs no inbound firewall rule.
	var bound, dialed *socket
	n.links = make([]*link, len(cfg.Links))
	for i, lc := range cfg.Links {
		host, port, err := DecodeAddr(lc.Addr)
		if err != nil {
			return nil, err
		}
		key, err := tunnel.DecodeKey(lc.Key)
		if err != nil {
			return nil, fmt.Errorf("probe: link %s: %w", lc.Addr, err)
		}
		seal, err := tunnel.NewSealer(key)
		if err != nil {
			return nil, err
		}

		var sock *socket
		if port == 0 {
			if bound == nil {
				addr, err := net.ResolveUDPAddr("udp", cfg.Bind)
				if err != nil {
					return nil, fmt.Errorf("probe: bind %s: %w", cfg.Bind, err)
				}
				conn, err := net.ListenUDP("udp", addr)
				if err != nil {
					return nil, err
				}
				bound = &socket{conn: conn, n: n}
				n.socks = append(n.socks, bound)
			}
			sock = bound
		} else {
			if dialed == nil {
				conn, err := net.ListenUDP("udp", nil)
				if err != nil {
					return nil, err
				}
				dialed = &socket{conn: conn, n: n}
				n.socks = append(n.socks, dialed)
			}
			sock = dialed
		}

		ip, err := resolve(host)
		if err != nil {
			return nil, err
		}
		l := &link{sock: sock, ip: ip, port: port, seal: seal}
		if port != 0 {
			// We know where to reach it, so it can be probed before it has ever
			// spoken. A peer that dials us cannot: its source port is ephemeral.
			l.remote.Store(&net.UDPAddr{IP: ip.AsSlice(), Port: port})
		}
		sock.links = append(sock.links, l)
		n.links[i] = l
	}

	for _, cc := range cfg.Classes {
		c := &class{Class: cc, n: n, out: map[uint64]time.Time{}}
		for _, h := range cc.Up {
			c.up = append(c.up, hop{n.links[h.Link], cc.dup(h)})
		}
		for _, h := range cc.Down {
			c.down = append(c.down, hop{n.links[h.Link], cc.dup(h)})
		}
		if c.originates() {
			for _, d := range windows {
				c.series = append(c.series, newSeries(d))
			}
		}
		n.classes[cc.ID] = c
	}

	if n.anyOriginates() {
		if n.w, err = newWriter(cfg.Log, cfg.MaxLogMB); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *Node) anyOriginates() bool {
	for _, c := range n.classes {
		if c.originates() {
			return true
		}
	}
	return false
}

func resolve(host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("probe: cannot resolve %q", host)
	}
	ip, _ := netip.AddrFromSlice(ips[0])
	return ip.Unmap(), nil
}

func (n *Node) Start() {
	for _, s := range n.socks {
		n.wg.Add(1)
		go func(s *socket) { defer n.wg.Done(); s.read() }(s)
	}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.timers() }()
}

func (n *Node) Close() {
	n.stopOnce.Do(func() {
		close(n.stop)
		for _, s := range n.socks {
			s.conn.Close()
		}
	})
	n.wg.Wait()
	if n.w != nil {
		n.w.Close()
	}
}

func (n *Node) stopped() bool {
	select {
	case <-n.stop:
		return true
	default:
		return false
	}
}

func (n *Node) timers() {
	probe := time.NewTicker(n.interval)
	sweep := time.NewTicker(max(n.timeout/4, 50*time.Millisecond))
	flush := time.NewTicker(n.flushEvery())
	live := time.NewTicker(max(n.interval, time.Second))
	defer func() {
		probe.Stop()
		sweep.Stop()
		flush.Stop()
		live.Stop()
	}()
	for {
		select {
		case <-n.stop:
			return
		case <-probe.C:
			for _, c := range n.classes {
				c.originate()
			}
		case now := <-sweep.C:
			for _, c := range n.classes {
				c.expire(now)
			}
		case now := <-flush.C:
			for _, c := range n.classes {
				c.flush(now)
			}
		case <-live.C:
			n.liveness()
		}
	}
}

// liveness logs a leg the moment it starts carrying our datagrams, and again if
// it stops. proxyctl reads exactly this to tell a blocked UDP port from an open
// one, so the wording matches proxyd's line and must keep matching it.
func (n *Node) liveness() {
	for _, l := range n.links {
		if u := l.up(n.interval); u != l.wasUp {
			l.wasUp = u
			word := "down"
			if u {
				word = "up"
			}
			log.Printf("%s: link %s %s", n.cfg.Name, l, word)
		}
	}
}

// flushEvery paces the check for closed windows. A window is written some time
// after it closes anyway, held back by the probe timeout, so checking often buys
// nothing — except when the windows themselves are short, which is what a test
// configures.
func (n *Node) flushEvery() time.Duration {
	d := time.Second
	for _, w := range n.windows {
		d = min(d, max(w/4, 10*time.Millisecond))
	}
	return d
}

// originate sends one probe down every path at once. Copies of the same sequence
// number arrive at the far end and are dropped to one there, so what comes back is
// the better of the paths per probe rather than on average — the same thing the
// exit does with a chunk.
func (c *class) originate() {
	if !c.originates() {
		return
	}
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.out[seq] = time.Now()
	c.mu.Unlock()

	plain := appendReq(nil, c.ID, seq)
	for _, h := range c.down {
		h.l.send(plain, h.dup)
	}
}

// expire calls an unanswered probe lost. It is filed under the time it was sent,
// so a window's loss is counted against the window that sent it.
func (c *class) expire(now time.Time) {
	if !c.originates() {
		return
	}
	var lost []time.Time
	c.mu.Lock()
	for seq, at := range c.out {
		if now.Sub(at) > c.n.timeout {
			delete(c.out, seq)
			lost = append(lost, at)
		}
	}
	for _, at := range lost {
		for _, s := range c.series {
			s.add(sample{at: at, lost: true})
		}
	}
	c.mu.Unlock()
}

func (c *class) flush(now time.Time) {
	if !c.originates() {
		return
	}
	c.mu.Lock()
	var out []*Report
	for _, s := range c.series {
		for _, i := range s.due(now, c.n.timeout) {
			if r := s.flush(i); r != nil {
				r.Class, r.Kind, r.Dup = c.Name, c.Kind, c.Duplicate
				out = append(out, r)
			}
		}
	}
	c.mu.Unlock()

	for _, r := range out {
		if err := c.n.w.write(r); err != nil {
			log.Printf("%s: probe log: %v", c.n.cfg.Name, err)
		}
		_, _, rt := r.Loss()
		log.Printf("%s: probe %s dup=%d w=%s rtt=%.1fms mdev=%.1fms loss=%.1f%% n=%d",
			c.n.cfg.Name, r.Class, r.Dup, dur(r.Window), r.P50, r.Mdev, rt, r.N)
	}
}

func (n *Node) handle(l *link, p packet) {
	c := n.classes[p.class]
	if c == nil {
		return
	}
	switch p.typ {
	case msgReq:
		c.request(p.seq)
	case msgResp:
		c.answer(p)
	}
}

// request is one probe arriving. Every node drops what it receives to one copy and
// then sends on with its own class's count, so the counts never compound along a
// chain: a relay fed two copies by a lossy leg puts the configured number on the
// clean one after it, not four.
func (c *class) request(seq uint64) {
	c.mu.Lock()
	if !c.seenReq.accept(seq) {
		c.mu.Unlock()
		return // another copy of a probe already forwarded or answered
	}
	c.recv++
	if seq > c.high {
		c.high = seq
	}
	recv, high := c.recv, c.high
	c.mu.Unlock()

	if !c.responds() {
		plain := appendReq(nil, c.ID, seq)
		for _, h := range c.down {
			h.l.send(plain, h.dup)
		}
		return
	}
	// The answer carries what this node has accepted so far, which is how the
	// originator learns how many of its probes arrived without any extra packet
	// being sent to tell it.
	plain := appendResp(nil, c.ID, seq, recv, high)
	for _, h := range c.up {
		h.l.send(plain, h.dup)
	}
}

func (c *class) answer(p packet) {
	if !c.originates() {
		c.mu.Lock()
		fresh := c.seenResp.accept(p.seq)
		c.mu.Unlock()
		if !fresh {
			return
		}
		plain := appendResp(nil, c.ID, p.seq, p.recv, p.high)
		for _, h := range c.up {
			h.l.send(plain, h.dup)
		}
		return
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.out[p.seq]
	if !ok {
		// A later copy of an answer already counted, or one for a probe that had
		// already timed out. Either way the first copy is the one that says how
		// fast the path is.
		return
	}
	delete(c.out, p.seq)
	for _, s := range c.series {
		s.add(sample{at: at, rtt: now.Sub(at), recv: p.recv})
	}
}

func (s *socket) candidates(from *net.UDPAddr) []*link {
	ap := from.AddrPort()
	ip := ap.Addr().Unmap()
	var out []*link
	for _, l := range s.links {
		if l.ip == ip && (l.port == 0 || l.port == int(ap.Port())) {
			out = append(out, l)
		}
	}
	return out
}

func (s *socket) read() {
	buf := make([]byte, 2048)
	plain := make([]byte, 0, 2048)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if s.n.stopped() {
				return
			}
			if ne, ok := err.(net.Error); ok && !ne.Timeout() {
				log.Printf("%s: probe read: %v", s.n.cfg.Name, err)
				return
			}
			continue
		}
		var (
			l   *link
			out []byte
		)
		// The key decides which leg a datagram belongs to, not the address: over
		// UDP anyone can write the far end's address on one.
		for _, cand := range s.candidates(from) {
			if o, err := cand.seal.Open(plain[:0], buf[:n]); err == nil {
				l, out = cand, o
				break
			}
		}
		if l == nil {
			continue
		}
		p, err := decode(out)
		if err != nil {
			continue
		}
		if r := l.remote.Load(); r == nil || r.String() != from.String() {
			l.remote.Store(from)
		}
		l.seen.Store(time.Now().UnixNano())
		s.n.handle(l, p)
	}
}
