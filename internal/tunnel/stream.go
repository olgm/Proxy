package tunnel

import (
	"errors"
	"io"
	"slices"
	"sync"
	"time"
)

var (
	// ErrUnrepairable means a chunk fell out of every buffer that could have
	// replaced it. An ordered stream cannot continue past that, so the session
	// ends and the player reconnects. Plain TCP has the same failure; it only
	// hides it for longer.
	ErrUnrepairable = errors.New("tunnel: stream is missing data no hop can still supply")
	ErrReset        = errors.New("tunnel: reset by peer")
	ErrClosed       = errors.New("tunnel: stream closed")
)

const (
	// ackEvery bounds how often a terminator reports its watermark; ackRepeat
	// re-announces it so a lost ACK cannot wedge the buffers behind it.
	ackEvery  = 20 * time.Millisecond
	ackRepeat = 250 * time.Millisecond
	// maxProbes bounds the originator's blind retransmits before it gives up on
	// the path entirely. Backed off, it works out near the repair deadline.
	maxProbes = 8
	// linger keeps a closed stream able to answer a late NACK, and keeps a
	// retransmitted chunk from being mistaken for a new stream and dialled again.
	linger = 60 * time.Second
	// maxHoles bounds how far ahead of itself a peer may claim to be.
	maxHoles = 1 << 16
)

type chunk struct {
	seq    uint64
	flags  byte
	data   []byte
	sentAt time.Time
	rtx    bool
}

// hole is a sequence number we have not seen but know exists, because something
// above it arrived.
type hole struct {
	first time.Time // when the gap appeared; the repair deadline runs from here
	last  time.Time // when we last asked for it
}

// dir is one direction of one stream as this node sees it.
//
// send is where the data leaves, back is where it arrived from. Whether those are
// empty is the node's entire role:
//
//	back empty -> this node originates the direction (the entry going out, the
//	              exit coming back), assigns sequence numbers and duplicates
//	send empty -> the direction terminates here and must be put back in order
//	neither    -> a relay: forward on arrival, keep a copy for the next hop to ask for
type dir struct {
	s    *Stream
	send []*Link
	back []*Link

	mu   sync.Mutex
	cond *sync.Cond

	// Outbound state: chunks we have put on send and may be asked for again.
	next    uint64
	buf     map[uint64]*chunk
	bufSize int
	acked   uint64 // every sequence below this is delivered at the far end
	probes  int
	moved   time.Time // last time acked advanced, or the first send
	srtt    time.Duration
	mdev    time.Duration

	// Inbound state.
	top     uint64 // one past the highest sequence seen
	want    map[uint64]*hole
	pending map[uint64]*chunk // terminators only: arrived, not yet in order
	deliver uint64            // terminators only: next sequence the reader gets
	head    []byte
	eof     bool
	lastAck time.Time
	sentAck uint64

	err error
}

func newDir(s *Stream, send, back []*Link) *dir {
	d := &dir{s: s, send: send, back: back,
		buf: map[uint64]*chunk{}, want: map[uint64]*hole{}}
	if len(send) == 0 {
		d.pending = map[uint64]*chunk{}
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *dir) originates() bool { return len(d.back) == 0 }
func (d *dir) terminates() bool { return len(d.send) == 0 }

// write splits the stream into chunks and sends each one. It blocks while the far
// end is behind by a window's worth of bytes, which is what pushes back on the
// player's socket when the exit cannot drain into the backend fast enough.
func (d *dir) write(p []byte) (int, error) {
	opt := &d.s.n.opt
	total := 0
	for len(p) > 0 {
		take := min(len(p), opt.maxChunk)
		d.mu.Lock()
		for d.bufSize >= opt.Window && d.err == nil {
			d.cond.Wait()
		}
		if d.err != nil {
			err := d.err
			d.mu.Unlock()
			return total, err
		}
		c := &chunk{seq: d.next, data: slices.Clone(p[:take]), sentAt: time.Now()}
		d.next++
		d.buf[c.seq] = c
		d.bufSize += take
		if d.moved.IsZero() {
			d.moved = c.sentAt
		}
		d.mu.Unlock()

		d.transmit(c, true, false)
		p = p[take:]
		total += take
	}
	return total, nil
}

// finish sends the end of the direction as an ordinary numbered chunk, so it
// arrives after everything before it and never ahead of a retransmission.
func (d *dir) finish() error {
	d.mu.Lock()
	if d.err != nil {
		err := d.err
		d.mu.Unlock()
		return err
	}
	c := &chunk{seq: d.next, flags: flagFin, sentAt: time.Now()}
	d.next++
	d.buf[c.seq] = c
	if d.moved.IsZero() {
		d.moved = c.sentAt
	}
	d.mu.Unlock()
	d.transmit(c, true, false)
	return nil
}

func (d *dir) transmit(c *chunk, originating, rtx bool) {
	plain := appendData(nil, d.s.id, c.seq, c.flags, c.data)
	for _, l := range d.send {
		n := 1
		if originating {
			n = l.dup
		}
		l.send(plain, n, rtx)
	}
}

func (d *dir) recv(p packet) {
	now := time.Now()
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	// Anything between the highest we had seen and this one is a hole. This is the
	// whole detector: 100 and 102 arrive, so 101 is missing and gets asked for.
	if p.seq >= d.top {
		// The window stops any correct sender getting this far ahead, so a jump
		// this large is a broken peer, and walking to it would hang the socket
		// loop rather than lose a stream.
		if p.seq-d.top > maxHoles {
			d.mu.Unlock()
			d.s.abort(ErrUnrepairable)
			return
		}
		for s := d.top; s < p.seq; s++ {
			d.want[s] = &hole{first: now}
		}
		d.top = p.seq + 1
	}
	delete(d.want, p.seq)

	if d.terminates() {
		_, dup := d.pending[p.seq]
		if p.seq < d.deliver || dup {
			d.mu.Unlock() // another copy of something we already have
			return
		}
		d.pending[p.seq] = &chunk{seq: p.seq, flags: p.flags, data: slices.Clone(p.payload)}
		d.cond.Broadcast()
		d.mu.Unlock()
		return
	}

	// A relay. Keep one copy so the next hop can ask us for it, and pass every
	// copy on: dropping the second would undo the duplication the entry paid for.
	if p.seq < d.acked {
		d.mu.Unlock()
		return
	}
	c, held := d.buf[p.seq]
	if !held {
		c = &chunk{seq: p.seq, flags: p.flags, data: slices.Clone(p.payload)}
		d.buf[p.seq] = c
		d.bufSize += len(c.data)
	}
	d.mu.Unlock()
	d.transmit(c, false, false)
}

func (d *dir) read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if len(d.head) > 0 {
			n := copy(p, d.head)
			d.head = d.head[n:]
			return n, nil
		}
		if c, ok := d.pending[d.deliver]; ok {
			delete(d.pending, d.deliver)
			d.deliver++
			if c.flags&flagFin != 0 {
				d.eof = true
			}
			d.head = c.data
			continue
		}
		if d.eof {
			return 0, io.EOF
		}
		if d.err != nil {
			return 0, d.err
		}
		d.cond.Wait()
	}
}

// onAck frees everything the far end has already read, and reports whether the
// watermark moved — a repeat carries no news and is not worth passing on.
func (d *dir) onAck(through uint64) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if through <= d.acked {
		return false
	}
	// A chunk that was only ever sent once gives an honest end-to-end round trip.
	if c, ok := d.buf[through-1]; ok && !c.rtx {
		d.sampleRTT(now.Sub(c.sentAt))
	}
	for s := d.acked; s < through; s++ {
		if c, ok := d.buf[s]; ok {
			d.bufSize -= len(c.data)
			delete(d.buf, s)
		}
	}
	d.acked = through
	d.probes = 0
	d.moved = now
	d.cond.Broadcast()
	return true
}

func (d *dir) onNack(l *Link, seqs []uint64) {
	now := time.Now()
	var resend []*chunk
	d.mu.Lock()
	for _, s := range seqs {
		if s < d.acked {
			continue
		}
		if c, ok := d.buf[s]; ok {
			c.rtx = true
			resend = append(resend, c)
			continue
		}
		if d.originates() {
			continue // out of our buffer; nothing can bring it back
		}
		// We never had it either. Want it ourselves, so the repair walks one leg
		// further back instead of the next hop asking us forever.
		if _, ok := d.want[s]; !ok {
			d.want[s] = &hole{first: now}
		}
		if s >= d.top {
			d.top = s + 1
		}
	}
	d.mu.Unlock()
	for _, c := range resend {
		l.send(appendData(nil, d.s.id, c.seq, c.flags, c.data), 1, true)
	}
}

// tick drives everything that is a timer rather than a packet: asking again for a
// hole, reporting the watermark, and probing a path that has gone quiet.
func (d *dir) tick(now time.Time) {
	var (
		ask   []uint64
		ack   uint64
		probe *chunk
		dead  bool
	)
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	interval := backRTO(d.back)
	for s, h := range d.want {
		if now.Sub(h.first) > d.s.n.opt.Repair {
			if d.terminates() {
				dead = true
				break
			}
			delete(d.want, s) // a relay just stops asking; the far end still can
			continue
		}
		if !h.last.IsZero() && now.Sub(h.last) < interval {
			continue
		}
		if len(ask) < maxNackSeqs {
			h.last = now
			ask = append(ask, s)
		}
	}
	if d.terminates() && d.deliver > 0 && now.Sub(d.lastAck) >= ackEvery &&
		(d.deliver > d.sentAck || now.Sub(d.lastAck) >= ackRepeat) {
		ack, d.sentAck, d.lastAck = d.deliver, d.deliver, now
	}
	if d.originates() && d.acked < d.next && !d.moved.IsZero() {
		if now.Sub(d.moved) >= d.probeWait() {
			if d.probes >= maxProbes {
				dead = true
			} else {
				d.probes++
				d.moved = now
				// Re-send the *highest* chunk, not the lowest outstanding one.
				// Gap detection is blind past the last sequence that arrived, so a
				// receiver cannot ask for a tail it has never seen the far side of.
				// One copy of the last chunk moves its horizon to the end of what
				// we sent, and every hole under it becomes an ordinary NACK.
				if c, ok := d.buf[d.next-1]; ok {
					c.rtx = true
					probe = c
				}
			}
		}
	}
	d.mu.Unlock()

	if dead {
		d.s.abort(ErrUnrepairable)
		return
	}
	if len(ask) > 0 {
		slices.Sort(ask)
		plain := appendNack(nil, d.s.id, ask)
		for _, l := range d.back {
			l.send(plain, 1, false)
		}
	}
	if ack > 0 {
		plain := appendAck(nil, d.s.id, ack)
		for _, l := range d.back {
			l.send(plain, 1, false)
		}
	}
	if probe != nil {
		d.transmit(probe, false, true)
	}
}

func (d *dir) abort(err error) {
	d.mu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.cond.Broadcast()
	d.mu.Unlock()
}

// sampleRTT and rto track the whole path, not one leg: the sample is the time
// from sending a chunk to the far end reporting it delivered.
func (d *dir) sampleRTT(r time.Duration) {
	if d.srtt == 0 {
		d.srtt, d.mdev = r, r/2
		return
	}
	diff := d.srtt - r
	if diff < 0 {
		diff = -diff
	}
	d.mdev = (3*d.mdev + diff) / 4
	d.srtt = (7*d.srtt + r) / 8
}

func (d *dir) probeWait() time.Duration {
	base := 500 * time.Millisecond
	if d.srtt > 0 {
		base = d.srtt + 4*d.mdev
	}
	return clamp(base<<min(d.probes, 3), 100*time.Millisecond, time.Second)
}

func backRTO(links []*Link) time.Duration {
	out := maxRTO
	for _, l := range links {
		out = min(out, l.rto())
	}
	return out
}

// Stream is one client connection carried through the tunnel. At the entry and
// the exit it is a byte pipe that can half-close, so it substitutes for the TCP
// connection the plain-TCP path would have dialled. At a relay nobody reads or
// writes it; it only holds the buffers the two directions pass through.
type Stream struct {
	n  *Node
	id uint64
	// down carries data toward the exit, up carries it back toward the entry.
	down, up *dir
	// rx and tx are the terminal view of those two. Nil at a relay.
	rx, tx *dir

	mu       sync.Mutex
	lastSeen time.Time
	expires  time.Time // once closed, state lingers to answer late NACKs
}

func (s *Stream) Read(p []byte) (int, error) {
	if s.rx == nil {
		return 0, ErrClosed
	}
	return s.rx.read(p)
}

func (s *Stream) Write(p []byte) (int, error) {
	if s.tx == nil {
		return 0, ErrClosed
	}
	return s.tx.write(p)
}

// CloseWrite ends our half. The other direction keeps flowing, which is what lets
// a client stop talking without cutting off what the server is still sending.
func (s *Stream) CloseWrite() error {
	if s.tx == nil {
		return ErrClosed
	}
	return s.tx.finish()
}

// Close releases the stream. A stream that ended cleanly is not reset: both ends
// already saw the FIN in order, and a reset racing a retransmission would truncate
// it. State lingers either way so a late NACK still finds an answer.
func (s *Stream) Close() error {
	reset := false
	if s.rx != nil {
		s.rx.mu.Lock()
		reset = !s.rx.eof
		s.rx.mu.Unlock()
	}
	s.n.close(s, reset)
	return nil
}

func (s *Stream) abort(err error) {
	s.down.abort(err)
	s.up.abort(err)
	s.n.close(s, err != ErrReset)
}

func (s *Stream) touch(now time.Time) {
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
}
