package tunnel

import (
	"errors"
	"net"
	"time"
)

// A handoff moves a running node into a new process without the chain noticing.
// The sockets move as they are, so no port, address or key changes for the
// neighbours, and so does everything the node knows that a socket does not:
// every stream's buffers, sequence numbers and round trips, and what each leg has
// learned. Datagrams that arrive in between wait in the socket, and anything the
// old process never got to send is recovered by the repair it already runs on:
// the horizon advert and the NACK.
//
// What does not move is the seal. The new process starts a fresh epoch, exactly
// as a restart does, because carrying a counter across would put the uniqueness
// of every nonce on the handoff having stopped every sender first. A peer reads
// the new epoch as a restart and starts its replay window again, which it has
// always done.

// Sockets are a node's UDP sockets: the one it is bound on for peers, and the
// ephemeral one it dials hops from. Either is nil where the node has none.
type Sockets struct {
	Bound, Dial *net.UDPConn
}

// Resume is what a node carries on from: its predecessor's sockets and state.
type Resume struct {
	Sockets
	State State
	// Attached are the streams the layer above is carrying on with, and will
	// claim with Adopt. At the exit the others are offered to Accept again, so
	// a stream frozen before anyone dialled for it is dialled now. At the entry
	// they are reset: nothing is left to write them.
	Attached map[uint64]bool
}

// State is what a node knows that its sockets do not. It is written as JSON.
type State struct {
	Links    []LinkState   `json:"links,omitempty"`
	Streams  []StreamState `json:"streams,omitempty"`
	ChainRTT time.Duration `json:"chain_rtt,omitempty"`
}

// LinkState is one leg, named as the config names it.
type LinkState struct {
	Down   bool          `json:"down,omitempty"`
	Addr   string        `json:"addr"`
	Remote string        `json:"remote,omitempty"`
	SRTT   time.Duration `json:"srtt,omitempty"`
	MDev   time.Duration `json:"mdev,omitempty"`
	Up     bool          `json:"up,omitempty"`
	Replay ReplayState   `json:"replay"`
}

// ReplayState is the window over the peer's nonces. It moves with the leg so a
// datagram captured before the handoff cannot be played into the new process.
type ReplayState struct {
	Epoch uint32     `json:"epoch,omitempty"`
	Set   bool       `json:"set,omitempty"`
	High  uint64     `json:"high,omitempty"`
	Bits  [16]uint64 `json:"bits"`
}

// StreamState is one stream, open or lingering.
type StreamState struct {
	ID     uint64        `json:"id"`
	Sent   uint64        `json:"sent,omitempty"`
	Recv   uint64        `json:"recv,omitempty"`
	Idle   time.Duration `json:"idle,omitempty"`   // since the last datagram
	Linger time.Duration `json:"linger,omitempty"` // closed: how long it is still remembered
	Down   DirState      `json:"down"`
	Up     DirState      `json:"up"`
}

// DirState is one direction of a stream. Timestamps do not move: a new process
// starts every timer from the moment it resumes, which asks again for a hole a
// little later than the old one would have, and adverts a sender's horizon at
// once — which is what finds anything lost while nobody was reading.
type DirState struct {
	Next    uint64        `json:"next,omitempty"`
	Acked   uint64        `json:"acked,omitempty"`
	Probes  int           `json:"probes,omitempty"`
	SRTT    time.Duration `json:"srtt,omitempty"`
	MDev    time.Duration `json:"mdev,omitempty"`
	Buf     []Chunk       `json:"buf,omitempty"`
	Top     uint64        `json:"top,omitempty"`
	Want    []uint64      `json:"want,omitempty"`
	Pending []Chunk       `json:"pending,omitempty"`
	Deliver uint64        `json:"deliver,omitempty"`
	Head    []byte        `json:"head,omitempty"`
	EOF     bool          `json:"eof,omitempty"`
	SentAck uint64        `json:"sent_ack,omitempty"`
	Err     string        `json:"err,omitempty"`
}

// Chunk is one numbered slice of a stream.
type Chunk struct {
	Seq   uint64 `json:"seq"`
	Flags byte   `json:"flags,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Rtx   bool   `json:"rtx,omitempty"`
}

// Freeze stops the node where it stands and reports its state and its sockets,
// for another process to resume. Nothing is read or sent after it returns, and no
// stream changes; the sockets stay open, and whatever arrives on them waits there
// for the process that takes them. Halt every stream the layer above is using
// first, so nothing is writing one while its state is read.
//
// A frozen node is finished in this process. Close it once the sockets are safe
// elsewhere: closing is all it does, and it sends nothing.
func (n *Node) Freeze() (State, Sockets) {
	if n.frozen.Swap(true) {
		return State{}, n.sockets()
	}
	close(n.thaw)
	for _, s := range n.socks {
		s.conn.SetReadDeadline(time.Now())
	}
	n.wg.Wait()

	st := State{ChainRTT: n.ChainRTT()}
	for _, l := range n.links() {
		st.Links = append(st.Links, l.state())
	}
	now := time.Now()
	for _, s := range n.live() {
		st.Streams = append(st.Streams, s.state(now))
	}
	return st, n.sockets()
}

func (n *Node) sockets() Sockets {
	var out Sockets
	for _, s := range n.socks {
		if len(s.links) > 0 && s.links[0].down {
			out.Dial = s.conn
		} else {
			out.Bound = s.conn
		}
	}
	return out
}

// Adopt hands the layer above a stream it was carrying before the handoff, or
// nil if the stream did not survive it.
func (n *Node) Adopt(id uint64) *Stream {
	n.mu.Lock()
	s := n.streams[id]
	n.mu.Unlock()
	if s == nil || s.gone() {
		return nil
	}
	s.offered.Store(true)
	return s
}

func (l *Link) state() LinkState {
	st := LinkState{Down: l.down, Addr: l.addr, Up: l.up()}
	if r := l.remote.Load(); r != nil {
		st.Remote = r.String()
	}
	l.mu.Lock()
	st.SRTT, st.MDev = l.srtt, l.mdev
	l.mu.Unlock()
	l.seal.mu.Lock()
	r := l.seal.seen
	l.seal.mu.Unlock()
	st.Replay = ReplayState{Epoch: r.epoch, Set: r.set, High: r.high, Bits: r.bits}
	return st
}

func (s *Stream) state(now time.Time) StreamState {
	st := StreamState{ID: s.id, Sent: s.sent.Load(), Recv: s.recv.Load()}
	s.mu.Lock()
	st.Idle = now.Sub(s.lastSeen)
	if !s.expires.IsZero() {
		st.Linger = max(s.expires.Sub(now), time.Millisecond)
	}
	s.mu.Unlock()
	st.Down, st.Up = s.down.state(), s.up.state()
	return st
}

func (d *dir) state() DirState {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := DirState{
		Next: d.next, Acked: d.acked, Probes: d.probes, SRTT: d.srtt, MDev: d.mdev,
		Top: d.top, Deliver: d.deliver, Head: d.head, EOF: d.eof, SentAck: d.sentAck,
	}
	for _, c := range d.buf {
		st.Buf = append(st.Buf, Chunk{Seq: c.seq, Flags: c.flags, Data: c.data, Rtx: c.rtx})
	}
	for _, c := range d.pending {
		st.Pending = append(st.Pending, Chunk{Seq: c.seq, Flags: c.flags, Data: c.data})
	}
	for seq := range d.want {
		st.Want = append(st.Want, seq)
	}
	if d.err != nil {
		st.Err = d.err.Error()
	}
	return st
}

// resume puts a frozen node's state into this one, before anything runs. A leg
// the new config no longer has is dropped with what was learned about it.
func (n *Node) resume(r *Resume) {
	now := time.Now()
	n.chainRTT = r.State.ChainRTT
	for _, ls := range r.State.Links {
		for _, l := range n.links() {
			if l.down == ls.Down && l.addr == ls.Addr {
				l.restore(ls, now)
			}
		}
	}
	for _, ss := range r.State.Streams {
		s := n.newStream(ss.ID)
		s.sent.Store(ss.Sent)
		s.recv.Store(ss.Recv)
		s.lastSeen = now.Add(-ss.Idle)
		if ss.Linger > 0 {
			s.expires = now.Add(ss.Linger)
		}
		s.down.restore(ss.Down, now)
		s.up.restore(ss.Up, now)
		s.offered.Store(r.Attached[ss.ID])
		n.streams[ss.ID] = s
	}
}

// settle deals with the streams nobody above claimed, once the node is running
// and can send.
func (n *Node) settle(attached map[uint64]bool) {
	for _, s := range n.live() {
		if attached[s.id] || s.gone() {
			continue
		}
		switch {
		case len(n.up) == 0:
			s.Close()
		case len(n.down) == 0 && s.rx.holds(0):
			n.offer(s)
		}
	}
}

func (l *Link) restore(st LinkState, now time.Time) {
	if l.port == 0 && st.Remote != "" {
		if a, err := net.ResolveUDPAddr("udp", st.Remote); err == nil {
			l.remote.Store(a)
		}
	}
	l.mu.Lock()
	l.srtt, l.mdev = st.SRTT, st.MDev
	if st.Up {
		// It answered a moment ago, as far as anyone here can tell. Starting it
		// down would log a flap that never happened and, on a node with every
		// link down, give up on streams that are merely waiting for this.
		l.lastPong = now
	}
	l.mu.Unlock()
	l.wasUp = st.Up
	l.seal.mu.Lock()
	l.seal.seen = replay{epoch: st.Replay.Epoch, set: st.Replay.Set, high: st.Replay.High, bits: st.Replay.Bits}
	l.seal.mu.Unlock()
}

func (d *dir) restore(st DirState, now time.Time) {
	d.next, d.acked, d.probes, d.srtt, d.mdev = st.Next, st.Acked, st.Probes, st.SRTT, st.MDev
	d.top, d.deliver, d.head, d.eof, d.sentAck = st.Top, st.Deliver, st.Head, st.EOF, st.SentAck
	for _, c := range st.Buf {
		d.buf[c.Seq] = &chunk{seq: c.Seq, flags: c.Flags, data: c.Data, sentAt: now, rtx: c.Rtx}
		d.bufSize += len(c.Data)
	}
	if d.pending != nil {
		for _, c := range st.Pending {
			d.pending[c.Seq] = &chunk{seq: c.Seq, flags: c.Flags, data: c.Data}
		}
	}
	for _, seq := range st.Want {
		d.want[seq] = &hole{first: now}
	}
	if d.acked < d.horizon() {
		d.moved = now
	}
	if st.Err != "" {
		d.err = errorNamed(st.Err)
	}
}

// errorNamed turns an ending back into the error it was, so a reader resumed on a
// stream that had already failed sees the same thing the old one would have.
func errorNamed(s string) error {
	for _, e := range []error{ErrClosed, ErrReset, ErrUnrepairable} {
		if e.Error() == s {
			return e
		}
	}
	return errors.New(s)
}
