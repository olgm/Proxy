// Package trial measures a mesh of candidate legs against the legs we already
// run, so a node can be chosen on evidence rather than on a vendor's map.
//
// It is a sibling of internal/probe and deliberately not part of it. probed
// measures the production path and may never be pointed anywhere else; every leg
// here is one no route uses, which is the entire question. The two differ in
// three ways that matter, and each falls out of that:
//
// Nothing is de-duplicated. probed collapses the copies racing into an exit to
// one, because a player's packet only needs to arrive once. Here every copy is
// logged with the route it took, because which route won and by how much is the
// measurement.
//
// A route travels on the wire. Nowhere else in this repo does a node identify
// itself in a datagram; here a copy that cannot say where it has been answers
// nothing. The ids are data only — the key still decides which leg a datagram
// belongs to, so nothing acts on an unopened datagram.
//
// Every hop echoes what it forwards. That is what keeps a leg's latency honest:
// both timestamps are taken on the sender's own clock. Arrival times taken at two
// nodes would carry their clock offset, and on a leg whose one-way delay is about
// a millisecond that offset is larger than the thing being measured.
//
// This service is temporary. When the trial has answered its question, delete it.
package trial

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olgm/proxy/internal/jsonl"
	"github.com/olgm/proxy/internal/tunnel"
	"github.com/olgm/proxy/internal/window"
)

// Node is this machine's part of the mesh. What it does with an arriving probe
// follows from the runs it was given and nothing else: a run it starts, a run it
// passes on, or a run it holds — which makes it that run's terminus.
type Node struct {
	cfg      Config
	interval time.Duration
	timeout  time.Duration
	windows  []time.Duration
	shortest time.Duration

	legs   []*leg
	names  map[uint8]string
	byName map[string]*leg
	// runs maps the id of whoever started a run to the legs an arrival of that
	// run is passed on over. A run with no entry here stops at this node.
	runs map[uint8][]*leg
	// mine is where our own run begins. Empty on a node that starts none.
	mine []*leg
	// solo are the legs we probe on their own, for a leg no run crosses.
	solo []*leg

	conn *net.UDPConn
	w    *jsonl.Writer
	tr   *tracer

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// leg is one peer. Unlike probed's link it carries no duplication count: a copy
// here is a copy that took a different route, never the same route twice, so the
// only number of copies a leg ever carries is one.
type leg struct {
	n      *Node
	peer   string
	id     uint8
	ip     netip.Addr
	addr   *net.UDPAddr
	seal   *tunnel.Sealer
	expect float64
	echo   bool
	// name is the direction we send in and rname the direction we receive in.
	// Both exist because a leg is measured from both ends and the two are not the
	// same measurement.
	name, rname string

	seen  atomic.Int64
	wasUp bool

	mu     sync.Mutex
	seq    uint64
	out    map[uint64]time.Time
	recv   uint64
	series []*window.Series
}

func (l *leg) String() string { return l.addr.String() }

func (l *leg) up(interval time.Duration) bool {
	t := l.seen.Load()
	return t != 0 && time.Since(time.Unix(0, t)) < max(5*interval, 5*time.Second)
}

func (l *leg) send(plain []byte) {
	buf := l.seal.Seal(make([]byte, 0, tunnel.NonceLen+len(plain)+tunnel.GCMOverhead), plain)
	if _, err := l.n.conn.WriteToUDP(buf, l.addr); err != nil {
		return
	}
}

// sendProbe puts one probe on this leg and remembers when. Both halves of the
// round trip are timed here, on this machine's clock and no other, which is what
// makes the number a latency rather than a latency plus a clock offset.
func (l *leg) sendProbe(tick uint64, path []uint8) {
	l.mu.Lock()
	l.seq++
	seq := l.seq
	l.out[seq] = time.Now()
	l.mu.Unlock()
	l.send(appendProbe(nil, seq, tick, path))
}

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
		shortest: windows[0],
		names:    map[uint8]string{cfg.ID: cfg.Name},
		byName:   map[string]*leg{},
		runs:     map[uint8][]*leg{},
		stop:     make(chan struct{}),
	}
	for _, w := range windows {
		n.shortest = min(n.shortest, w)
	}
	for name, id := range cfg.Names {
		n.names[id] = name
	}

	// One bound socket. Every node in this mesh is dialled by every peer it has,
	// so unlike probed there is no second ephemeral socket and no leg that has to
	// wait to be spoken to before it can be measured.
	addr, err := net.ResolveUDPAddr("udp", cfg.Bind)
	if err != nil {
		return nil, fmt.Errorf("trial: bind %s: %w", cfg.Bind, err)
	}
	if n.conn, err = net.ListenUDP("udp", addr); err != nil {
		return nil, err
	}

	for _, lc := range cfg.Legs {
		host, port, err := DecodeAddr(lc.Addr)
		if err != nil {
			n.conn.Close()
			return nil, err
		}
		key, err := tunnel.DecodeKey(lc.Key)
		if err != nil {
			n.conn.Close()
			return nil, fmt.Errorf("trial: leg %s: %w", lc.Peer, err)
		}
		seal, err := tunnel.NewSealer(key)
		if err != nil {
			n.conn.Close()
			return nil, err
		}
		ip, err := resolve(host)
		if err != nil {
			n.conn.Close()
			return nil, err
		}
		l := &leg{
			n: n, peer: lc.Peer, id: lc.PeerID, ip: ip, seal: seal,
			addr:   &net.UDPAddr{IP: ip.AsSlice(), Port: port},
			expect: lc.ExpectMS, echo: lc.Echo,
			name:  cfg.Name + ">" + lc.Peer,
			rname: lc.Peer + ">" + cfg.Name,
			out:   map[uint64]time.Time{},
		}
		for _, d := range windows {
			l.series = append(l.series, window.NewSeries(d))
		}
		n.legs = append(n.legs, l)
		n.names[lc.PeerID] = lc.Peer
		n.byName[lc.Peer] = l
		if lc.Echo {
			n.solo = append(n.solo, l)
		}
	}

	for _, r := range cfg.Runs {
		fwd := make([]*leg, 0, len(r.Forward))
		for _, p := range r.Forward {
			fwd = append(fwd, n.byName[p])
		}
		n.runs[r.FromID] = fwd
		n.names[r.FromID] = r.From
		if r.From == cfg.Name {
			n.mine = fwd
		}
	}

	// Every node writes, which is the other place this differs from probed: there
	// only an originator has anything to say, here an arrival is a measurement in
	// its own right and every node sees some.
	if n.w, err = jsonl.NewWriter(cfg.Log, cfg.MaxLogMB); err != nil {
		n.conn.Close()
		return nil, err
	}
	if cfg.Trace != nil {
		n.tr = newTracer(*cfg.Trace)
	}
	return n, nil
}

func resolve(host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("trial: cannot resolve %q", host)
	}
	ip, _ := netip.AddrFromSlice(ips[0])
	return ip.Unmap(), nil
}

// Addr is the address the mesh socket actually took, which a test needs when it
// asked for port zero.
func (n *Node) Addr() *net.UDPAddr { return n.conn.LocalAddr().(*net.UDPAddr) }

func (n *Node) Start() {
	n.wg.Add(2)
	go func() { defer n.wg.Done(); n.read() }()
	go func() { defer n.wg.Done(); n.timers() }()
}

func (n *Node) Close() {
	n.stopOnce.Do(func() {
		close(n.stop)
		n.conn.Close()
	})
	n.wg.Wait()
	n.w.Close()
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
			n.originate()
		case now := <-sweep.C:
			for _, l := range n.legs {
				l.expire(now, n.timeout)
			}
		case now := <-flush.C:
			for _, l := range n.legs {
				l.flush(now)
			}
		case <-live.C:
			n.liveness()
		}
	}
}

// flushEvery paces the check for closed windows, exactly as probed paces its own:
// a window is held back by the probe timeout anyway, so checking often buys
// nothing except when the windows are short, which is what a test configures.
func (n *Node) flushEvery() time.Duration {
	d := time.Second
	for _, w := range n.windows {
		d = min(d, max(w/4, 10*time.Millisecond))
	}
	return d
}

// originate starts one probe down every leg our run begins on, and one down every
// leg that has no run to carry it. The tick is the wall second, taken
// independently on every node: at one probe a second every node stamps the same
// tick for the same second without anyone coordinating, which is what makes six
// nodes' datasets joinable by nothing more than that column.
func (n *Node) originate() {
	tick := uint64(time.Now().Unix())
	path := []uint8{n.cfg.ID}
	for _, l := range n.mine {
		l.sendProbe(tick, path)
	}
	for _, l := range n.solo {
		l.sendProbe(tick, path)
	}
}

func (l *leg) expire(now time.Time, timeout time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for seq, at := range l.out {
		if now.Sub(at) > timeout {
			delete(l.out, seq)
			for _, s := range l.series {
				s.Add(window.Sample{At: at, Lost: true})
			}
		}
	}
}

func (l *leg) flush(now time.Time) {
	l.mu.Lock()
	var out []*window.Closed
	for _, s := range l.series {
		for _, i := range s.Due(now, l.n.timeout) {
			if c := s.Flush(i); c != nil {
				out = append(out, c)
			}
		}
	}
	l.mu.Unlock()

	for _, c := range out {
		if err := l.n.w.Write(record(l.name, l.expect, c)); err != nil {
			log.Printf("%s: trial log: %v", l.n.cfg.Name, err)
		}
		_, _, rt := c.Loss()
		log.Printf("%s: trial %s w=%s rtt=%.1fms mdev=%.1fms loss=%.1f%% n=%d",
			l.n.cfg.Name, l.name, dur(c.Window), c.P50, c.Mdev, rt, c.N)
		// Only the shortest window drives the trigger. A ten-minute window that
		// has finally closed is describing a path that may already have healed.
		if c.Window == l.n.shortest {
			l.n.consider(l, c)
		}
	}
}

func (n *Node) liveness() {
	for _, l := range n.legs {
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

func (n *Node) handle(l *leg, p packet, at time.Time) {
	switch p.typ {
	case msgProbe:
		n.arrived(l, p, at)
	case msgEcho:
		l.answered(p, at)
	}
}

// arrived is one probe landing. It is echoed straight back over the leg it came
// in on, which is what gives the sender its round trip, and then passed on to
// wherever this run goes next — one copy per onward leg, with this node appended
// to the route so the copy can still say where it has been.
//
// Nothing is dropped as a duplicate on the way. A second copy of the same tick
// over a different route is not noise to be collapsed; it is the comparison the
// whole mesh exists to make.
func (n *Node) arrived(l *leg, p packet, at time.Time) {
	l.mu.Lock()
	l.recv++
	recv := l.recv
	l.mu.Unlock()
	l.send(appendEcho(nil, p.legSeq, recv))

	path := append(append([]uint8(nil), p.path...), n.cfg.ID)
	fwd := 0
	if len(path) < maxPath {
		for _, t := range n.runs[p.path[0]] {
			// The loop guard, and the only reason the route is carried rather than
			// just the sender: a flood over a mesh with a cycle in it does not stop
			// on its own.
			if contains(path, t.id) {
				continue
			}
			t.sendProbe(p.tick, path)
			fwd++
		}
	}
	if err := n.w.Write(n.recordArrival(at, l.rname, p, path, fwd)); err != nil {
		log.Printf("%s: trial log: %v", n.cfg.Name, err)
	}
}

func (l *leg) answered(p packet, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.out[p.legSeq]
	if !ok {
		return // an echo for a probe already written off as lost
	}
	delete(l.out, p.legSeq)
	for _, s := range l.series {
		s.Add(window.Sample{At: at, RTT: now.Sub(at), Recv: p.recv})
	}
}

func contains(path []uint8, id uint8) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}

func (n *Node) candidates(from *net.UDPAddr) []*leg {
	ip := from.AddrPort().Addr().Unmap()
	var out []*leg
	for _, l := range n.legs {
		if l.ip == ip {
			out = append(out, l)
		}
	}
	return out
}

func (n *Node) read() {
	buf := make([]byte, 2048)
	plain := make([]byte, 0, 2048)
	for {
		nb, from, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			if n.stopped() {
				return
			}
			if ne, ok := err.(net.Error); ok && !ne.Timeout() {
				log.Printf("%s: trial read: %v", n.cfg.Name, err)
				return
			}
			continue
		}
		at := time.Now()
		var (
			l   *leg
			out []byte
		)
		// The key decides which leg a datagram belongs to, not the address: over
		// UDP anyone can write the far end's address on one.
		for _, cand := range n.candidates(from) {
			if o, err := cand.seal.Open(plain[:0], buf[:nb]); err == nil {
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
		l.seen.Store(at.UnixNano())
		n.handle(l, p, at)
	}
}
