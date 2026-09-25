package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/olgm/proxy/internal/handoff"
	"github.com/olgm/proxy/internal/tunnel"
)

// A handoff moves this node into the process that replaces it, sessions and all.
//
// The old process stops taking connections, lets logins in progress reach their
// relay, and halts every relay at a byte boundary. It writes down what each was
// doing — the bytes it had read and not yet written, the tunnel's buffers and
// sequence numbers, the session — and hands that and every socket to systemd,
// then exits without closing anything a player can see. The new process takes the
// sockets back and carries on from the same bytes. Nothing is sent to a player or
// to the backend in between; the pause is the time it takes to change processes,
// and the tunnel's repair covers whatever arrived during it.

// handoffVersion is the snapshot format. A process handed a newer one than it
// knows starts clean rather than guess: the sessions in it end as in a restart,
// and nothing is misread.
const handoffVersion = 1

// handoffGrace is how long a handoff waits for connections still being admitted
// — a handshake, a whitelist check, a dial — to start relaying, before it cuts
// them off. A login that has not finished by then is a player who reconnects.
const handoffGrace = 2 * time.Second

// haltWait bounds the wait for a halted relay to stop. Halting one is a deadline
// and a flag, so this is only ever spent on a relay that is not coming back, and
// that session is lost rather than the handoff.
const haltWait = 5 * time.Second

// Handoff is how Run takes part in one: where a signal starts it, what the last
// process left, where the next one's goes, and who to tell once the node is up.
// proxyd wires it to systemd's fd store; see internal/handoff.
type Handoff struct {
	Signal <-chan struct{}
	From   *handoff.Inherited
	Keep   func(snapshot []byte, files []handoff.File) error
	Ready  func()
	// Drop takes entries out of the store at once, and returns once they are
	// gone: a socket the store still holds keeps its port however many times
	// this process closes its own copy.
	Drop func(names []string)
}

type snapshot struct {
	Version   int             `json:"version"`
	Listeners []listenerState `json:"listeners,omitempty"`
	// Control is the control link's listener, by the file it is stored under.
	Control     string       `json:"control,omitempty"`
	ControlBind string       `json:"control_bind,omitempty"`
	Relays      []relayState `json:"relays,omitempty"`
	// Feed is what the session feed had not posted yet.
	Feed []string `json:"feed,omitempty"`
}

// listenerState is one listener's sockets, by the names they are stored under,
// and its tunnel.
type listenerState struct {
	Net    string        `json:"net"`
	Bind   string        `json:"bind"`
	TCP    string        `json:"tcp,omitempty"`
	Bound  string        `json:"bound,omitempty"`
	Dial   string        `json:"dial,omitempty"`
	Tunnel *tunnel.State `json:"tunnel,omitempty"`
}

// relayState is one relay: a is the side that arrived — a player, or the previous
// hop — and b the side this node opened.
type relayState struct {
	Net     string        `json:"net"`
	Bind    string        `json:"bind"`
	A       endState      `json:"a"`
	B       endState      `json:"b"`
	AB      flowState     `json:"ab"`
	BA      flowState     `json:"ba"`
	Session *sessionState `json:"session,omitempty"`
	Start   time.Time     `json:"start,omitzero"`
}

// endState is a TCP connection by the file it is stored under, or a tunnel stream
// by its id.
type endState struct {
	FD     string `json:"fd,omitempty"`
	Stream uint64 `json:"stream,omitempty"`
}

type flowState struct {
	N       int64  `json:"n"`
	Pending []byte `json:"pending,omitempty"`
	Ended   bool   `json:"ended,omitempty"`
	Done    bool   `json:"done,omitempty"`
	Err     string `json:"err,omitempty"`
}

// sessionState is a login in progress: what its logout will need.
type sessionState struct {
	IP    string    `json:"ip"`
	Name  string    `json:"name"`
	UUID  string    `json:"uuid"`
	Proto int       `json:"proto"`
	Start time.Time `json:"start"`
	RTT   rttState  `json:"rtt"`
}

// handoff hands the node to its successor through keep, and reports whether it
// did. Once it has halted anything it cannot take that back, so an error past
// that point is returned for the process to exit on, like any other failure: the
// successor starts from whatever keep did manage to store.
func (n *node) handoff(keep func([]byte, []handoff.File) error) error {
	g := n.gate
	g.shutDoors()
	for _, ln := range n.lns {
		ln.SetDeadline(time.Now())
	}
	if n.ctl != nil {
		n.ctl.SetDeadline(time.Now())
	}
	g.settle(handoffGrace)

	n.live.freeze()
	relays, cut := g.freeze()
	for _, c := range cut {
		c.SetDeadline(time.Now())
	}
	for r := range relays {
		r.halt()
	}

	fs := &fileSet{}
	defer fs.close()
	snap := snapshot{Version: handoffVersion}
	var sessions int64
	deadline := time.Now().Add(haltWait)
	for r, k := range relays {
		select {
		case <-r.done:
		case <-time.After(time.Until(deadline)):
			log.Printf("handoff: a relay on %s did not stop; its session ends here", k.bind)
			continue
		}
		if !r.carried() {
			continue // it finished on its own, and is writing itself down
		}
		rs, err := fs.relay(r, k)
		if err != nil {
			log.Printf("handoff: a relay on %s cannot be carried: %v", k.bind, err)
			continue
		}
		snap.Relays = append(snap.Relays, rs)
		if k.sess != nil {
			sessions++
		}
	}
	// A session that ended in the same moment is being written down by its own
	// goroutine, into the log and the feed this process is about to let go of.
	n.live.waitFor(sessions, time.Second)

	for _, s := range n.servers {
		ls := listenerState{Net: s.Network(), Bind: s.Bind}
		if s.tun != nil {
			st, socks := s.tun.Freeze()
			ls.Tunnel = &st
			ls.Bound, ls.Dial = fs.add(socks.Bound), fs.add(socks.Dial)
		}
		if ln := n.byBind[s.Bind]; ln != nil && ls.Net == "tcp" {
			ls.TCP = fs.add(ln)
		}
		snap.Listeners = append(snap.Listeners, ls)
	}
	if n.ctl != nil {
		snap.Control, snap.ControlBind = fs.add(n.ctl), n.ctlBind
	}
	if n.feed != nil {
		snap.Feed = n.feed.q.Detach(200 * time.Millisecond)
	}
	if fs.err != nil {
		log.Printf("handoff: %v", fs.err)
	}

	b, err := json.Marshal(snap)
	if err == nil {
		err = keep(b, fs.files)
	}
	n.release()
	if err != nil {
		return err
	}
	log.Printf("handoff: %d relay(s) and %d listener(s) handed on", len(snap.Relays), len(snap.Listeners))
	return nil
}

// release lets go of everything once it is safe elsewhere. It closes this
// process's copies, which ends nothing: the store holds the others.
func (n *node) release() {
	close(n.gate.released)
	for _, ln := range n.lns {
		ln.Close()
	}
	if n.ctl != nil {
		n.ctl.Close()
	}
	for _, s := range n.servers {
		if s.tun != nil {
			s.tun.Close()
		}
	}
	if n.live.log != nil {
		n.live.log.Close()
	}
}

// fileSet is every descriptor a handoff stores, each under a name of its own.
type fileSet struct {
	files []handoff.File
	err   error
}

type filer interface{ File() (*os.File, error) }

// add stores a copy of a socket's descriptor and returns its name, or "" for a
// socket there is none of.
func (fs *fileSet) add(c filer) string {
	switch v := c.(type) {
	case nil:
		return ""
	case *net.UDPConn:
		if v == nil {
			return ""
		}
	case *net.TCPListener:
		if v == nil {
			return ""
		}
	}
	f, err := c.File()
	if err != nil {
		fs.err = errors.Join(fs.err, err)
		return ""
	}
	name := "f" + strconv.Itoa(len(fs.files))
	fs.files = append(fs.files, handoff.File{Name: name, File: f})
	return name
}

func (fs *fileSet) close() {
	for _, f := range fs.files {
		f.File.Close()
	}
}

func (fs *fileSet) relay(r *relayer, k *carry) (relayState, error) {
	rs := relayState{Net: k.net, Bind: k.bind, AB: flowOf(&r.ab), BA: flowOf(&r.ba), Start: k.start}
	var err error
	if rs.A, err = fs.end(r.a); err != nil {
		return rs, err
	}
	if rs.B, err = fs.end(r.b); err != nil {
		return rs, err
	}
	if k.sess != nil {
		s := k.sess
		rs.Session = &sessionState{IP: s.IP, Name: s.Name, UUID: s.UUID, Proto: s.Proto, Start: s.Start}
		if k.rtt != nil {
			rs.Session.RTT = k.rtt.halt()
		}
	}
	return rs, nil
}

func (fs *fileSet) end(h halfCloser) (endState, error) {
	switch c := h.(type) {
	case *tunnel.Stream:
		return endState{Stream: c.ID()}, nil
	case *net.TCPConn:
		f, err := c.File()
		if err != nil {
			return endState{}, err
		}
		name := "f" + strconv.Itoa(len(fs.files))
		fs.files = append(fs.files, handoff.File{Name: name, File: f})
		return endState{FD: name}, nil
	}
	return endState{}, fmt.Errorf("cannot carry a %T", h)
}

func flowOf(f *flow) flowState {
	st := flowState{N: f.n, Pending: f.pending, Ended: f.ended, Done: f.done}
	if f.err != nil {
		st.Err = f.err.Error()
	}
	return st
}

func (st flowState) flow() flow {
	f := flow{n: st.N, pending: st.Pending, ended: st.Ended, done: st.Done}
	if st.Err != "" {
		f.err = errors.New(st.Err)
	}
	return f
}

// inherited is what the previous process left, read for build. A snapshot that
// cannot be read, or is from a newer format, is ignored and every socket in it
// closed: the sessions end as in a restart, and nothing is misread.
type inherited struct {
	snap  snapshot
	from  *handoff.Inherited
	binds map[string]*listenerState
	drop  func(names []string)
}

func readInherited(from *handoff.Inherited) *inherited {
	if from == nil {
		return nil
	}
	in := &inherited{from: from, binds: map[string]*listenerState{}}
	if len(from.Snapshot) == 0 {
		log.Printf("handoff: descriptors but no snapshot; starting clean")
		return in
	}
	if err := json.Unmarshal(from.Snapshot, &in.snap); err != nil {
		log.Printf("handoff: snapshot: %v; starting clean", err)
		in.snap = snapshot{}
		return in
	}
	if in.snap.Version > handoffVersion {
		log.Printf("handoff: snapshot version %d is newer than this build's %d; starting clean",
			in.snap.Version, handoffVersion)
		in.snap = snapshot{}
		return in
	}
	for i := range in.snap.Listeners {
		ls := &in.snap.Listeners[i]
		in.binds[listenerKey(ls.Net, ls.Bind)] = ls
	}
	return in
}

// listenerKey is what a listener is known by across a handoff: its bind, and
// whether it is TCP or UDP, since one of each may share a bind.
func listenerKey(network, bind string) string { return network + " " + bind }

// prune lets go of every inherited listening socket the new config will not take,
// before anything is bound. A listener left in the store keeps its port, so a node
// that cannot read the snapshot, or whose config names a listener differently now,
// would otherwise fail to bind and crash again on every restart. Nothing else goes
// early: a connection the config has no use for is closed once build is done and
// leaves the store with everything else when the node is ready, so a start that
// fails before then leaves it, and its session, to whichever process comes next.
// The snapshot stays until then too. Without a snapshot there is no telling a
// listener from a connection, and all of it goes now.
func (in *inherited) prune(cfg *Config) {
	if in == nil {
		return
	}
	keep := map[string]bool{}
	for _, l := range cfg.Listeners {
		if ls := in.binds[listenerKey(l.Network(), l.Bind)]; ls != nil {
			keep[ls.TCP], keep[ls.Bound] = true, true
		}
	}
	if cfg.Control != nil && cfg.Control.Bind == in.snap.ControlBind {
		keep[in.snap.Control] = true
	}
	listening := map[string]bool{in.snap.Control: true}
	for _, ls := range in.snap.Listeners {
		listening[ls.TCP], listening[ls.Bound] = true, true
	}
	// Version is 0 only where readInherited found no snapshot it could use.
	blind := in.snap.Version == 0
	var gone []string
	for _, name := range in.from.Names() {
		if keep[name] || !blind && !listening[name] {
			continue
		}
		f := in.from.File(name)
		if f == nil {
			continue // the snapshot, which was read and closed already
		}
		f.Close()
		gone = append(gone, name)
	}
	if len(gone) > 0 {
		log.Printf("handoff: letting go of %d socket(s) the new config has no use for", len(gone))
		if in.drop != nil {
			in.drop(gone)
		}
	}
}

// tunnel is what a listener's tunnel resumes from, or nil to start clean. A
// listener the new config gives no tunnel takes nothing, and what its old one
// held is closed with everything else nobody claims.
func (in *inherited) tunnel(l Listener) *tunnel.Resume {
	if in == nil || len(l.Hops) == 0 && len(l.Peers) == 0 {
		return nil
	}
	key := listenerKey(l.Network(), l.Bind)
	ls := in.binds[key]
	if ls == nil || ls.Tunnel == nil {
		return nil
	}
	r := &tunnel.Resume{State: *ls.Tunnel, Attached: map[uint64]bool{}}
	r.Bound, r.Dial = in.udp(ls.Bound), in.udp(ls.Dial)
	for _, rs := range in.snap.Relays {
		if listenerKey(rs.Net, rs.Bind) != key {
			continue
		}
		for _, e := range []endState{rs.A, rs.B} {
			if e.FD == "" {
				r.Attached[e.Stream] = true
			}
		}
	}
	return r
}

func (in *inherited) udp(name string) *net.UDPConn {
	f := in.file(name)
	if f == nil {
		return nil
	}
	defer f.Close()
	c, err := net.FilePacketConn(f)
	if err != nil {
		log.Printf("handoff: %s: %v", name, err)
		return nil
	}
	u, ok := c.(*net.UDPConn)
	if !ok {
		c.Close()
		return nil
	}
	return u
}

func (in *inherited) tcp(name string) *net.TCPConn {
	f := in.file(name)
	if f == nil {
		return nil
	}
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		log.Printf("handoff: %s: %v", name, err)
		return nil
	}
	t, ok := c.(*net.TCPConn)
	if !ok {
		c.Close()
		return nil
	}
	return t
}

// listener is the TCP listener a bind was using, or nil to listen afresh.
func (in *inherited) listener(bind string) (*net.TCPListener, error) {
	if in == nil {
		return nil, nil
	}
	name := ""
	if ls := in.binds[listenerKey("tcp", bind)]; ls != nil {
		name = ls.TCP
	} else if bind == in.snap.ControlBind {
		name = in.snap.Control
	}
	f := in.file(name)
	if f == nil {
		return nil, nil
	}
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("handoff: listener %s: %w", bind, err)
	}
	return ln.(*net.TCPListener), nil
}

func (in *inherited) file(name string) *os.File {
	if in == nil || name == "" {
		return nil
	}
	return in.from.File(name)
}

// resume rebuilds every relay the snapshot carries onto the node's servers, and
// returns what starts each one again. A relay whose listener is gone, or one of
// whose ends did not come through, ends the way a restart would have ended it —
// once the node is ready, since a start that fails before then is retried from
// this same snapshot and would write its session down twice.
func (in *inherited) resume(servers []*server) []func() {
	if in == nil {
		return nil
	}
	byKey := map[string]*server{}
	for _, s := range servers {
		byKey[listenerKey(s.Network(), s.Bind)] = s
	}
	var out []func()
	for _, rs := range in.snap.Relays {
		s := byKey[listenerKey(rs.Net, rs.Bind)]
		var a, b halfCloser
		if s != nil {
			a, b = in.end(s, rs.A), in.end(s, rs.B)
		}
		if s == nil || a == nil || b == nil {
			rec := s
			if rec == nil {
				rec = servers[0] // any of them: they share the node's log and feed
			}
			out = append(out, func() {
				for _, e := range []halfCloser{a, b} {
					if e != nil {
						e.Close()
					}
				}
				if rs.Session != nil {
					rec.recordLost(rs)
				}
				log.Printf("handoff: a relay on %s did not survive the handoff", rs.Bind)
			})
			continue
		}
		r := newRelayer(a, b)
		r.ab, r.ba = rs.AB.flow(), rs.BA.flow()
		out = append(out, s.resume(rs, r))
	}
	return out
}

func (in *inherited) end(s *server, e endState) halfCloser {
	if e.FD != "" {
		if c := in.tcp(e.FD); c != nil {
			return c
		}
		return nil
	}
	if s.tun == nil {
		return nil
	}
	if st := s.tun.Adopt(e.Stream); st != nil {
		return st
	}
	return nil
}

// resume is what starts one carried relay again, of whichever kind it was.
func (s *server) resume(rs relayState, r *relayer) func() {
	k := &carry{bind: s.Bind, net: s.Network(), start: rs.Start}
	switch {
	case rs.Session != nil:
		c := r.a.(*net.TCPConn)
		st := rs.Session
		sess := Session{Node: s.node, IP: st.IP, Name: st.Name, UUID: st.UUID, Proto: st.Proto, Start: st.Start}
		rtt := resumeRTT(c, st.RTT)
		s.geo.Watch(sess.IP)
		return func() {
			defer r.a.Close()
			defer r.b.Close()
			s.gate.adopt(r, k)
			s.countMu.Lock()
			sess.Online = s.online.Add(1)
			id := s.live.add(sess, func() { r.a.Close(); r.b.Close() })
			s.countMu.Unlock()
			defer s.live.done()
			k.sess, k.rtt = &sess, rtt
			s.carryOn(sess, id, r.b, r, rtt)
		}
	case s.Net == "udp":
		st := r.a.(*tunnel.Stream)
		return func() {
			defer r.a.Close()
			defer r.b.Close()
			s.gate.adopt(r, k)
			s.finishStream(st, r, k.start)
		}
	default:
		return func() {
			defer r.a.Close()
			defer r.b.Close()
			s.gate.adopt(r, k)
			s.finishPlain(r)
		}
	}
}

// recordLost writes down a session the handoff could not carry on, so history has
// it: it ended at the handoff with what it had carried until then. The session
// need not have been this server's, only this node's.
func (s *server) recordLost(rs relayState) {
	st := rs.Session
	sess := Session{Node: s.node, IP: st.IP, Name: st.Name, UUID: st.UUID, Proto: st.Proto, Start: st.Start,
		End: time.Now(), Up: rs.AB.N, Down: rs.BA.N}
	s.live.record(sess)
	log.Printf("%s: logout %s name=%q uuid=%q for %s up=%s down=%s (lost in a handoff)",
		rs.Bind, sess.IP, sess.Name, sess.UUID, sess.For(), size(sess.Up), size(sess.Down))
	s.feed.logout(sess)
}
